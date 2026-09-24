package sync

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// mirrorRereadShare is the share of a local mirror's stored bytes one
// verify pass re-reads, least recently verified first: a tenth, so every
// byte is read again within about ten passes.
const mirrorRereadShare = 10

// verifyMirror checks every copy a native mirror holds by squirrel's
// records, the live and the displaced ones: each gets one Lstat against its
// recorded size and mtime, and on a local disk a slice of them is re-read
// through BLAKE3. A copy that is gone or changed is marked lost, which
// latches the destination's alarm and makes the next push write the path
// again if the index still holds that content there.
func verifyMirror(ctx context.Context, s *store.Store, dest *config.Destination, rep *RemoteVerifyReport) error {
	rows, err := s.ListStoredRemotePaths(ctx, dest.Name)
	if err != nil {
		return fmt.Errorf("list recorded mirror copies for %q: %w", dest.Name, err)
	}
	rep.Paths = len(rows)
	if len(rows) == 0 {
		return nil
	}
	runID, err := s.BeginRemoteVerifyRun(ctx)
	if err != nil {
		return fmt.Errorf("record verify run: %w", err)
	}
	rep.RunID = runID
	verifyErr := checkMirrorCopies(ctx, s, dest, rows, rep)
	if err := recordVerifyOutcome(ctx, s, rep, verifyErr); err != nil {
		return err
	}
	if verifyErr == nil && rep.Clean() {
		return upgradeFingerprintVectors(ctx, s, dest.Name)
	}
	return verifyErr
}

// mirrorCopy is one recorded copy as a verify pass addresses it.
type mirrorCopy struct {
	store.StoredRemotePath
	name string // where the copy is, relative to the destination root
}

func checkMirrorCopies(ctx context.Context, s *store.Store, dest *config.Destination, rows []store.StoredRemotePath, rep *RemoteVerifyReport) error {
	tr, err := openReadOnly(ctx, dest)
	if err != nil {
		return err
	}
	defer func() { _ = tr.Close() }()
	var intact []mirrorCopy
	for _, r := range rows {
		c := mirrorCopy{StoredRemotePath: r, name: storedCopyName(r)}
		ok, err := statCopy(ctx, s, tr, c, rep)
		if err != nil {
			return err
		}
		if ok {
			intact = append(intact, c)
		}
	}
	if !readsBack(dest) {
		return nil
	}
	return rereadCopies(ctx, s, tr, rereadSlice(intact), rep)
}

// storedCopyName is where a live copy sits, <volume>/<path>, or a displaced
// one, <volume>/.squirrel-history/run-<displaced run>/<path>.
func storedCopyName(r store.StoredRemotePath) string {
	if r.State == store.RemotePathDisplaced {
		return historyName(r.Volume, r.DisplacedRunID.Int64, r.Path)
	}
	return path.Join(r.Volume, r.Path)
}

// statCopy checks one copy's size and mtime and reports whether it is
// intact. A copy that is gone or changed is recorded as a finding.
func statCopy(ctx context.Context, s *store.Store, tr transport, c mirrorCopy, rep *RemoteVerifyReport) (bool, error) {
	e, err := tr.Stat(ctx, c.name)
	switch {
	case errors.Is(err, fs.ErrNotExist) || errors.Is(err, errParentNotDir):
		return false, loseCopy(ctx, s, c, &rep.PathsMissing)
	case err != nil:
		return false, fmt.Errorf("stat %s: %w", c.name, err)
	case !matchesRecord(e, c.RemotePath):
		return false, loseCopy(ctx, s, c, &rep.PathsChanged)
	}
	return true, nil
}

// rereadSlice picks the copies one pass re-reads: least recently verified
// first, until they cover mirrorRereadShare of the bytes the destination
// holds, and always at least one.
func rereadSlice(copies []mirrorCopy) []mirrorCopy {
	var total int64
	for _, c := range copies {
		total += c.SizeBytes
	}
	var read int64
	for i, c := range copies {
		if i > 0 && read*mirrorRereadShare >= total {
			return copies[:i]
		}
		read += c.SizeBytes
	}
	return copies
}

// rereadCopies hashes each copy as the disk returns it: a match re-stamps
// its fingerprint, other bytes are a finding.
func rereadCopies(ctx context.Context, s *store.Store, tr transport, copies []mirrorCopy, rep *RemoteVerifyReport) error {
	for _, c := range copies {
		sum, err := readBack(ctx, tr, c.name)
		if errors.Is(err, fs.ErrNotExist) {
			if err := loseCopy(ctx, s, c, &rep.PathsMissing); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("re-read %s: %w", c.name, err)
		}
		if !bytes.Equal(sum, c.Blake3) {
			if err := loseCopy(ctx, s, c, &rep.PathsChanged); err != nil {
				return err
			}
			continue
		}
		if err := s.RecordRemotePathVerified(ctx, c.ID, hex.EncodeToString(sum), store.NowNs()); err != nil {
			return err
		}
		rep.PathsReread++
	}
	return nil
}

// loseCopy marks a copy lost and lists its name under the finding, unless a
// push moved its row since the pass read it.
func loseCopy(ctx context.Context, s *store.Store, c mirrorCopy, finding *[]string) error {
	moved, err := s.LoseStoredRemotePath(ctx, c.ID, c.State)
	if err != nil || !moved {
		return err
	}
	*finding = append(*finding, c.name)
	return nil
}
