package sync

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/volmark"
)

// mirrorRestore restores a volume from a native mirror through a read-only
// transport. With an index that holds present paths for the volume, it
// fetches each of them by path and checks its bytes against the indexed
// BLAKE3 as they stream. Without one — a fresh machine — it walks the
// mirrored tree and checks each file against the mirror's receipts.
type mirrorRestore struct {
	store  *store.Store
	vol    *config.Volume
	dest   *config.Destination
	volID  int64
	tr     transport
	placer restorePlacer
	dryRun bool
}

// restoreMirror runs one native mirror restore and writes the
// kind='restore' runs row's terminal state.
func restoreMirror(ctx context.Context, s *store.Store, vol *config.Volume, dest *config.Destination, volID, runID int64, targetInPlace bool, opts RestoreOptions, rep *Report) error {
	rep.RunID = runID
	tr, err := openReadOnly(ctx, dest)
	if err != nil {
		finishRestore(ctx, s, opts.DryRun, runID, rep, err)
		return err
	}
	defer func() { _ = tr.Close() }()
	mr := &mirrorRestore{
		store: s, vol: vol, dest: dest, volID: volID, tr: tr,
		placer: newRestorePlacer(vol, runID, targetInPlace, opts), dryRun: opts.DryRun,
	}
	runErr := mr.run(ctx, rep, opts.IncludeFromFile)
	finishRestore(ctx, s, opts.DryRun, runID, rep, runErr)
	return runErr
}

func (mr *mirrorRestore) run(ctx context.Context, rep *Report, includeFile string) error {
	rows, err := mr.store.ListPresentContent(ctx, mr.volID)
	if err != nil {
		return fmt.Errorf("list present content for %q: %w", mr.vol.Name, err)
	}
	switch {
	case includeFile != "":
		include, err := loadIncludeSet(includeFile)
		if err != nil {
			return err
		}
		mr.restoreIndexed(ctx, rep, slices.DeleteFunc(rows, func(r store.PathDelta) bool { return !include[r.Path] }))
	case len(rows) > 0:
		mr.restoreIndexed(ctx, rep, rows)
	default:
		if err := mr.restoreWalked(ctx, rep); err != nil {
			return err
		}
	}
	if rep.RcloneResult.Errors > 0 {
		return fmt.Errorf("restore of %q from %q: %d file(s) failed", mr.vol.Name, mr.dest.Name, rep.RcloneResult.Errors)
	}
	return nil
}

// restoreIndexed restores each present path the index names, checked
// against its indexed hash and stamped with its indexed mtime.
func (mr *mirrorRestore) restoreIndexed(ctx context.Context, rep *Report, rows []store.PathDelta) {
	for _, r := range rows {
		mr.restoreOne(ctx, rep, r.Path, r.SizeBytes, r.Blake3, time.Unix(0, r.MtimeNs))
	}
}

// restoreWalked restores every file of the mirrored tree. A file a receipt
// names is checked against the content the newest such receipt recorded;
// the rest are restored unchecked, and counted in a warning.
func (mr *mirrorRestore) restoreWalked(ctx context.Context, rep *Report) error {
	receipts, err := mr.readReceipts(ctx, rep)
	if err != nil {
		return err
	}
	unchecked := 0
	err = mr.walk(ctx, rep, "", func(rel string, e entry) {
		want, ok := receipts[rel]
		if !ok {
			unchecked++
		}
		mr.restoreOne(ctx, rep, rel, e.size, want, e.mtime)
	})
	if unchecked > 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("%d file(s) on %q are named in no receipt, so their bytes could not be checked; they are restored unchecked", unchecked, mr.dest.Name))
	}
	return err
}

// restoreOne fetches rel from the mirror and places it. A path that
// already holds the wanted bytes is left as it is and counted as already
// correct; a failure is recorded against rel and the restore goes on.
func (mr *mirrorRestore) restoreOne(ctx context.Context, rep *Report, rel string, size int64, want []byte, mtime time.Time) {
	if want != nil && mr.placer.holds(rel, size, want) {
		rep.RcloneResult.Checked++
		return
	}
	if mr.dryRun {
		rep.RcloneResult.Transferred++
		rep.RcloneResult.Bytes += size
		return
	}
	rc, err := mr.tr.Get(ctx, path.Join(mr.vol.Name, rel))
	if err != nil {
		recordRestoreFailure(rep, rel, fmt.Errorf("fetch from %q: %w", mr.dest.Name, err))
		return
	}
	defer func() { _ = rc.Close() }()
	n, err := mr.placer.place(rel, ctxReader{ctx: ctx, r: rc}, want, mtime)
	if err != nil {
		recordRestoreFailure(rep, rel, err)
		return
	}
	rep.RcloneResult.Transferred++
	rep.RcloneResult.Bytes += n
}

// walk visits every file of the mirrored tree under dir, leaving out
// squirrel's reserved entries at the volume root. An entry that is neither
// a file nor a directory is reported and not restored.
func (mr *mirrorRestore) walk(ctx context.Context, rep *Report, dir string, visit func(rel string, e entry)) error {
	name := path.Join(mr.vol.Name, dir)
	entries, err := mr.tr.List(ctx, name)
	if err != nil {
		return fmt.Errorf("list %s on %q: %w", name, mr.dest.Name, err)
	}
	for _, e := range entries {
		rel := path.Join(dir, e.name)
		switch {
		case dir == "" && (isReservedFolderPath(e.name) || e.name == volmark.MarkerName):
		case e.kind == kindDir:
			if err := mr.walk(ctx, rep, rel, visit); err != nil {
				return err
			}
		case e.kind == kindFile:
			visit(rel, e)
		default:
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s on %q is neither a file nor a directory; it is not restored", rel, mr.dest.Name))
		}
	}
	return nil
}

// readReceipts folds the mirror's receipts, oldest run first, into the
// BLAKE3 each path last held: a present entry overrides every earlier run.
// A receipt that cannot be read is reported and left out, so the paths it
// covered are checked against older receipts.
func (mr *mirrorRestore) readReceipts(ctx context.Context, rep *Report) (map[string][]byte, error) {
	dir := path.Join(mr.vol.Name, IndexDirName)
	entries, err := mr.tr.List(ctx, dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list %s on %q: %w", dir, mr.dest.Name, err)
	}
	var runs []int64
	for _, e := range entries {
		if id, ok := receiptRunID(e); ok {
			runs = append(runs, id)
		}
	}
	slices.Sort(runs)
	out := map[string][]byte{}
	for _, id := range runs {
		name := path.Join(dir, "run-"+strconv.FormatInt(id, 10))
		if err := foldReceipt(ctx, mr.tr, name, out); err != nil {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("receipt %s on %q could not be read (%v); the files it names are checked against older receipts", name, mr.dest.Name, err))
		}
	}
	return out, nil
}

// receiptRunID reads the run id of a receipt, a file named run-<id>.
func receiptRunID(e entry) (int64, bool) {
	idText, ok := strings.CutPrefix(e.name, "run-")
	if !ok || e.kind != kindFile {
		return 0, false
	}
	id, err := strconv.ParseInt(idText, 10, 64)
	return id, err == nil && id > 0 && strconv.FormatInt(id, 10) == idText
}

// foldReceipt records into the content of every present path the receipt
// at name lists.
func foldReceipt(ctx context.Context, tr transport, name string, into map[string][]byte) error {
	rc, err := tr.Get(ctx, name)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	sc := bufio.NewScanner(ctxReader{ctx: ctx, r: rc})
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var e ManifestEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return err
		}
		if e.Status != store.StatusPresent {
			continue
		}
		sum, err := hex.DecodeString(e.Blake3)
		if err != nil || len(sum) != 32 {
			return fmt.Errorf("%s: blake3 %q is not a 32-byte hex hash", e.Path, e.Blake3)
		}
		into[e.Path] = sum
	}
	return sc.Err()
}
