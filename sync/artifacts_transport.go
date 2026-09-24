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
	"time"

	"github.com/zeebo/blake3"

	"github.com/mbertschler/squirrel/store"
)

// transportArtifacts is an artifact store squirrel writes itself: a
// content-addressed or packed destination on a local disk, or on an sftp
// server without crypt. Each artifact is staged in the volume's
// .squirrel-staging/run-<id>/ while BLAKE3 hashes the bytes sent, read
// back — through BLAKE3 on a local disk, through the server's hash command
// on sftp — and renamed onto its name, so it lands whole or not at all.
type transportArtifacts struct {
	*destinationRoot
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

func (a *transportArtifacts) put(ctx context.Context, runID int64, name, src string, size int64, sum []byte) (*remoteChecksum, error) {
	tr, err := a.root(ctx, runID)
	if err != nil {
		return nil, err
	}
	staged := stagingName(a.volumeDir, runID, stagingKey(name))
	sent, err := a.stage(ctx, tr, staged, src, size, sum)
	if err != nil {
		return nil, err
	}
	fingerprint, err := a.confirm(ctx, tr, staged, sum, sent, true)
	if err != nil {
		return nil, err
	}
	err = tr.Rename(ctx, staged, name)
	if errors.Is(err, fs.ErrExist) {
		return a.confirm(ctx, tr, name, sum, sent, false)
	}
	return fingerprint, err
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
		a.noServerHash = err
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
