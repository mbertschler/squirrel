package sync

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	gosync "sync"
	"sync/atomic"
	"time"

	"github.com/zeebo/blake3"

	"github.com/mbertschler/squirrel/store"
)

// transportArtifacts is an artifact store squirrel writes itself: a
// content-addressed or packed destination on a local disk, or on an sftp
// server without crypt. Each artifact is staged in the volume's
// .squirrel-staging/run-<id>/ while BLAKE3 hashes the bytes sent, flushed,
// read back — through BLAKE3 on a local disk, through the server's hash
// command on sftp — and renamed onto its name, so it lands whole or not at
// all. Artifacts land a batch at a time, several in flight.
type transportArtifacts struct {
	*destinationRoot

	mu gosync.Mutex
	// noServerHash is why the sftp server fingerprinted nothing this push,
	// once a landing found out.
	noServerHash error
}

// root is the destination root as the push may hold it: guarded, so only
// runID's own commits pass (none when runID is 0), and bounded by progress.
func (a *transportArtifacts) root(ctx context.Context, runID int64) (transport, error) {
	return a.guarded(ctx, nameGuard{volumeDir: a.volumeDir, runID: runID, content: true, finished: a.runFinished})
}

// markers gates a push that writes on the volume marker, as a native
// mirror does: written under --init, which also creates a missing local
// root, and refused otherwise.
func (a *transportArtifacts) markers(ctx context.Context, opts Options) error {
	if opts.DryRun {
		return nil
	}
	return a.ensureMarker(ctx, opts.Init)
}

func (a *transportArtifacts) exists(ctx context.Context, name string) (bool, error) {
	tr, err := a.root(ctx, 0)
	if err != nil {
		return false, err
	}
	return present(ctx, tr, name)
}

func (a *transportArtifacts) rootEmpty(ctx context.Context) (bool, error) {
	tr, err := a.root(ctx, 0)
	if err != nil {
		return false, err
	}
	return treeEmpty(ctx, tr, ".")
}

// reconcile removes what finished runs left staged: a partial artifact
// from a crash, a drifted source's copy, or one whose name another run's
// landing already held.
func (a *transportArtifacts) reconcile(ctx context.Context, rep *Report, runID int64) error {
	tr, err := a.root(ctx, runID)
	if err != nil {
		return err
	}
	return a.clearFinishedStaging(ctx, rep, tr)
}

// artifactLanding is one artifact on its way through putAll.
type artifactLanding struct {
	artifactPut
	staged string
	// sent is the server-side hash of the bytes sent, "" without one.
	sent string
	res  *artifactResult
}

// putAll takes the artifacts through the steps of a landing, each for all
// of them before the next: stage, flush, confirm, rename, flush. An
// artifact whose step fails drops out.
func (a *transportArtifacts) putAll(ctx context.Context, runID int64, items []artifactPut) []artifactResult {
	out := make([]artifactResult, len(items))
	tr, err := a.root(ctx, runID)
	if err != nil {
		for i := range out {
			out[i].err = err
		}
		return out
	}
	ls := make([]*artifactLanding, len(items))
	for i, it := range items {
		ls[i] = &artifactLanding{artifactPut: it, staged: stagingName(a.volumeDir, runID, stagingKey(it.name)), res: &out[i]}
	}
	b := artifactBatch{a: a, tr: tr, n: pushConcurrency(a.dest)}
	b.each(ctx, ls, b.stage)
	b.flush(ctx, ls, "flush the staged artifacts")
	b.each(ctx, ls, b.confirmStaged)
	b.each(ctx, ls, b.rename)
	b.flush(ctx, ls, "flush the landed artifacts")
	return out
}

// artifactBatch runs the steps of putAll over one batch.
type artifactBatch struct {
	a       *transportArtifacts
	tr      transport
	n       int
	stalled atomic.Bool
}

// each runs step for every artifact still on its way, n in flight; an
// artifact whose step fails drops out, and a stall starts no more.
func (b *artifactBatch) each(ctx context.Context, ls []*artifactLanding, step func(context.Context, *artifactLanding) error) {
	inFlight(ctx, b.n, pendingArtifacts(ls), b.stalled.Load, func(l *artifactLanding) {
		if err := step(ctx, l); err != nil {
			l.res.err = err
			if errors.Is(err, errTransportStalled) {
				b.stalled.Store(true)
			}
		}
	})
}

// flush makes the batch's last step durable; a flush that fails fails
// every artifact still on its way.
func (b *artifactBatch) flush(ctx context.Context, ls []*artifactLanding, what string) {
	pending := pendingArtifacts(ls)
	if len(pending) == 0 || b.stalled.Load() {
		return
	}
	if err := b.tr.Flush(ctx); err != nil {
		for _, l := range pending {
			l.res.err = fmt.Errorf("%s: %w", what, err)
		}
	}
}

func pendingArtifacts(ls []*artifactLanding) []*artifactLanding {
	var out []*artifactLanding
	for _, l := range ls {
		if l.res.err == nil {
			out = append(out, l)
		}
	}
	return out
}

func (b *artifactBatch) stage(ctx context.Context, l *artifactLanding) error {
	sent, err := b.a.stage(ctx, b.tr, l.staged, l.src, l.size, l.sum)
	l.sent = sent
	return err
}

func (b *artifactBatch) confirmStaged(ctx context.Context, l *artifactLanding) error {
	fingerprint, err := b.a.confirm(ctx, b.tr, l.staged, l.sum, l.sent, true)
	l.res.fingerprint = fingerprint
	return err
}

// rename moves the staged artifact onto its name. A name that already
// holds a file is confirmed instead, and never replaced.
func (b *artifactBatch) rename(ctx context.Context, l *artifactLanding) error {
	err := b.tr.Rename(ctx, l.staged, l.name)
	if !errors.Is(err, fs.ErrExist) {
		return err
	}
	fingerprint, err := b.a.confirm(ctx, b.tr, l.name, l.sum, l.sent, false)
	l.res.fingerprint = fingerprint
	return err
}

// stage streams src into staged, hashing the bytes sent with BLAKE3 and,
// on sftp, with the hash the server's command computes. It returns that
// server-side hash's value for what was sent, or "" when there is none.
func (a *transportArtifacts) stage(ctx context.Context, tr transport, staged, src string, size int64, sum []byte) (string, error) {
	f, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", src, err)
	}
	defer f.Close()
	b3 := blake3.New()
	counted := &countWriter{w: b3}
	sink := io.Writer(counted)
	server := newArtifactHash(a.dest.HashAlgo)
	if server != nil {
		sink = io.MultiWriter(counted, server)
	}
	if err := tr.Put(ctx, staged, io.TeeReader(f, sink), time.Now()); err != nil {
		return "", fmt.Errorf("stage %s: %w", staged, err)
	}
	if counted.n != size || !bytes.Equal(b3.Sum(nil), sum) {
		return "", fmt.Errorf("%w: %s sent %d bytes hashing to %s, want %d bytes %s", errContentDrift, src, counted.n, hex.EncodeToString(b3.Sum(nil)), size, hex.EncodeToString(sum))
	}
	e, err := tr.Stat(ctx, staged)
	if err != nil {
		return "", fmt.Errorf("confirm %s: %w", staged, err)
	}
	if e.size != size {
		return "", fmt.Errorf("%s landed with size %d, want %d", staged, e.size, size)
	}
	return hexSum(server), nil
}

// confirm checks that name holds the bytes sent — read back through
// BLAKE3 against sum on a local disk, hashed by the server's command
// against sent on sftp — and returns the fingerprint that earns. An sftp
// server without the command confirms nothing and fingerprints nothing:
// a staged artifact is committed with its fingerprint pending; one already
// at its name — a crash between its landing and its record, or an earlier
// failed run's orphan — is downloaded and hashed instead, because squirrel
// never replaces it.
func (a *transportArtifacts) confirm(ctx context.Context, tr transport, name string, sum []byte, sent string, staged bool) (*remoteChecksum, error) {
	if !readsBack(a.dest) {
		cs, err := tr.ServerHash(ctx, name)
		switch {
		case err == nil && cs.Value == sent:
			return &cs, nil
		case err == nil:
			return nil, fmt.Errorf("%w: %s hashes to %s %s on the server, sent %s", errReadBackMismatch, name, cs.Algo, cs.Value, sent)
		case !errors.Is(err, errNoServerHash):
			return nil, fmt.Errorf("hash %s on the server: %w", name, err)
		}
		a.mu.Lock()
		a.noServerHash = err
		a.mu.Unlock()
		if staged {
			return nil, nil
		}
	}
	got, err := readBack(ctx, tr, name)
	if err != nil {
		return nil, fmt.Errorf("read back %s: %w", name, err)
	}
	if !bytes.Equal(got, sum) {
		return nil, fmt.Errorf("%w: %s hashes to %s, sent %s — squirrel never replaces it; inspect it", errReadBackMismatch, name, hex.EncodeToString(got), hex.EncodeToString(sum))
	}
	if !readsBack(a.dest) {
		return nil, nil
	}
	return &remoteChecksum{Algo: store.ChecksumAlgoBlake3, Value: hex.EncodeToString(got)}, nil
}

// capture warns about the artifacts that landed without a fingerprint: an
// sftp server that runs no hash command squirrel trusts.
func (a *transportArtifacts) capture(_ context.Context, rep *Report, dir string, targets []captureTarget) {
	if len(targets) == 0 {
		return
	}
	rep.Warnings = append(rep.Warnings, fmt.Sprintf("destination %q: %d artifact(s) under %s/ landed without a fingerprint (%v); they stay pending, so the destination cannot gate offload for them", a.dest.Name, len(targets), dir, a.noServerHash))
}

// shelf is the volume's .squirrel-index/, reached through runID's guarded
// transport.
func (a *transportArtifacts) shelf(runID int64) snapshotShelf {
	return transportShelf{
		open:   func(ctx context.Context) (transport, error) { return a.root(ctx, runID) },
		volume: a.volumeDir,
		runID:  runID,
	}
}

func hexSum(h hash.Hash) string {
	if h == nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}
