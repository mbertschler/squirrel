package sync

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/volmark"
)

// mirrorRereadShare is the share of a local mirror's stored bytes one
// verify pass re-reads, least recently verified first: a tenth, so every
// byte is read again within about ten passes.
const mirrorRereadShare = 10

// verifyMirror checks every copy a native mirror holds by squirrel's
// records, the live and the displaced ones: each gets one Lstat against its
// recorded size and mtime, and on a local disk a slice of them is re-read
// through BLAKE3. A copy that is gone or changed is marked lost, which
// latches the destination's alarm, demotes its volume's evidence to
// presence+size, and makes the next push write the path again if the index
// still holds that content there.
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
	pass := &mirrorPass{s: s, dest: dest, rep: rep, lostIn: map[int64]bool{}}
	verifyErr := pass.run(ctx, rows)
	if err := pass.demoteLostVolumes(ctx); err != nil && verifyErr == nil {
		verifyErr = err
	}
	if err := recordVerifyOutcome(ctx, s, rep, verifyErr); err != nil {
		return err
	}
	if verifyErr == nil && rep.Clean() {
		return upgradeFingerprintVectors(ctx, s, dest.Name)
	}
	return verifyErr
}

// mirrorPass is one verify pass over a native mirror: where it reports, and
// the volumes it found a copy lost in.
type mirrorPass struct {
	s      *store.Store
	dest   *config.Destination
	rep    *RemoteVerifyReport
	lostIn map[int64]bool
}

// mirrorCopy is one recorded copy as a verify pass addresses it.
type mirrorCopy struct {
	store.StoredRemotePath
	name string // where the copy is, relative to the destination root
}

// run checks every volume's marker before any of its copies, so a disk
// that is not mounted, or a share emptied from under its root, fails the
// pass instead of reading as every copy gone.
func (p *mirrorPass) run(ctx context.Context, rows []store.StoredRemotePath) error {
	tr, err := openReadOnly(ctx, p.dest)
	if err != nil {
		return err
	}
	defer func() { _ = tr.Close() }()
	if err := requireVolumeMarkers(ctx, tr, p.dest, storedVolumes(rows)); err != nil {
		return err
	}
	var intact []mirrorCopy
	for _, r := range rows {
		c := mirrorCopy{StoredRemotePath: r, name: storedCopyName(r)}
		ok, err := p.statCopy(ctx, tr, c)
		if err != nil {
			return err
		}
		if ok {
			intact = append(intact, c)
		}
	}
	if !readsBack(p.dest) {
		return nil
	}
	return p.rereadCopies(ctx, tr, rereadSlice(intact))
}

func storedVolumes(rows []store.StoredRemotePath) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range rows {
		if !seen[r.Volume] {
			seen[r.Volume] = true
			out = append(out, r.Volume)
		}
	}
	return out
}

// requireVolumeMarkers fails unless each volume's directory on the root
// holds the .squirrel-volume marker naming it.
func requireVolumeMarkers(ctx context.Context, tr transport, dest *config.Destination, volumes []string) error {
	for _, v := range volumes {
		name := path.Join(v, volmark.MarkerName)
		data, err := readSmallFile(ctx, tr, name, maxMarkerBytes)
		if err != nil {
			return fmt.Errorf("destination %q: read %s: %w — the disk may not be mounted, or the root is wrong; nothing was checked", dest.Name, name, err)
		}
		m, err := volmark.Parse(data)
		if err == nil && m.Volume != v {
			err = fmt.Errorf("it names volume %q", m.Volume)
		}
		if err != nil {
			return fmt.Errorf("destination %q: %s: %w — nothing was checked", dest.Name, name, err)
		}
	}
	return nil
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
func (p *mirrorPass) statCopy(ctx context.Context, tr transport, c mirrorCopy) (bool, error) {
	e, err := tr.Stat(ctx, c.name)
	switch {
	case errors.Is(err, fs.ErrNotExist) || errors.Is(err, errParentNotDir):
		return false, p.loseCopy(ctx, c, &p.rep.PathsMissing)
	case err != nil:
		return false, fmt.Errorf("stat %s: %w", c.name, err)
	case !matchesRecord(e, c.RemotePath):
		return false, p.loseCopy(ctx, c, &p.rep.PathsChanged)
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

// rereadCopies hashes each copy as the disk returns it and compares it with
// the fingerprint its row records, or with its content's BLAKE3 while it
// records none: a match re-stamps the fingerprint, other bytes are a
// finding. A copy a push moved since the pass read its row is left to the
// next pass.
func (p *mirrorPass) rereadCopies(ctx context.Context, tr transport, copies []mirrorCopy) error {
	for _, c := range copies {
		sum, err := readBack(ctx, tr, c.name)
		if errors.Is(err, fs.ErrNotExist) {
			if err := p.loseCopy(ctx, c, &p.rep.PathsMissing); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("re-read %s: %w", c.name, err)
		}
		want := hex.EncodeToString(c.Blake3)
		if c.Checksum.Valid {
			want = c.Checksum.String
		}
		if got := hex.EncodeToString(sum); got != want {
			if err := p.loseCopy(ctx, c, &p.rep.PathsChanged); err != nil {
				return err
			}
			continue
		}
		stamped, err := p.s.RecordRemotePathVerified(ctx, c.ID, want, store.NowNs())
		if err != nil {
			return err
		}
		if stamped {
			p.rep.PathsReread++
		}
	}
	return nil
}

// loseCopy marks a copy lost and lists its name under the finding, unless a
// push moved its row since the pass read it.
func (p *mirrorPass) loseCopy(ctx context.Context, c mirrorCopy, finding *[]string) error {
	moved, err := p.s.LoseStoredRemotePath(ctx, c.ID, c.State)
	if err != nil || !moved {
		return err
	}
	*finding = append(*finding, c.name)
	p.lostIn[c.VolumeID] = true
	return nil
}

// demoteLostVolumes demotes the evidence of every volume the pass found a
// copy lost in, so the offload gate stops taking the destination's word
// for that volume as a whole until a push writes the copies again.
func (p *mirrorPass) demoteLostVolumes(ctx context.Context) error {
	for volumeID := range p.lostIn {
		if err := p.s.DemoteFingerprintVerifiedVector(ctx, volumeID, p.dest.Name); err != nil {
			return err
		}
	}
	return nil
}
