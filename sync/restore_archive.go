package sync

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/klauspost/compress/zstd"
	"github.com/zeebo/blake3"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// archiveRestore restores a content-addressed or packed destination back
// to the local filesystem, read-only against both the index and the
// destination. It resolves each requested path to a content hash from the
// local index, locates the bytes (a per-hash object under objects/, or a
// member inside a tar.zst pack under packs/), fetches through the same
// rclone (crypt) read path sync uses, and re-hashes every extracted
// content to BLAKE3 before writing — the misplacement/corruption check the
// offset/length pack slicing needs. Packs are fetched once per pack and
// every requested member is extracted from that single stream.
type archiveRestore struct {
	store    *store.Store
	rcl      *Rclone
	vol      *config.Volume
	dest     *config.Destination
	volID    int64
	runID    int64
	target   string // resolved local directory (vol.Path or --to)
	preserve bool   // move overwritten files to .squirrel-restore-history/
	dryRun   bool
}

// restoreArchive orchestrates one content-addressed or packed restore: it
// wires the restorer, runs it, and writes the kind='restore' runs row's
// terminal state. A fatal error (nothing counted as an individual failure)
// is turned into a synthesised failed-file so the runs row carries the
// reason. Read-only against the index and destination throughout.
func restoreArchive(ctx context.Context, s *store.Store, rcl *Rclone, vol *config.Volume, dest *config.Destination, volID, runID int64, targetInPlace bool, opts RestoreOptions, rep *Report) error {
	rep.RunID = runID
	target := vol.Path
	if opts.ToPath != "" {
		target = opts.ToPath
	}
	ar := newArchiveRestore(s, rcl, vol, dest, volID, runID, target, targetInPlace && opts.InPlace, opts.DryRun)
	runErr := ar.run(ctx, rep, opts.IncludeFromFile)
	if runErr != nil && rep.RcloneResult.Errors == 0 {
		rep.RcloneResult.FatalError = true
		rep.RcloneResult.FailedFiles = append(rep.RcloneResult.FailedFiles, FailedFile{Message: runErr.Error()})
	}
	finishRun(ctx, s, opts.DryRun, runID, rep)
	return runErr
}

// newArchiveRestore builds the restorer. preserve is set only for an
// in-place restore that must keep any overwritten bytes, mirroring the
// mirror layout's --backup-dir contract.
func newArchiveRestore(s *store.Store, rcl *Rclone, vol *config.Volume, dest *config.Destination, volID, runID int64, target string, preserve, dryRun bool) *archiveRestore {
	return &archiveRestore{
		store: s, rcl: rcl, vol: vol, dest: dest,
		volID: volID, runID: runID, target: target, preserve: preserve, dryRun: dryRun,
	}
}

// restoreContent is one content to restore and the volume-relative paths it
// must land at. A content can appear at several paths (dedup), so one fetch
// serves them all.
type restoreContent struct {
	contentID int64
	blake3    []byte
	size      int64
	paths     []string
}

func (c restoreContent) firstPath() string { return c.paths[0] }

// packGroup batches every requested member of one pack so the pack is
// fetched once. members are sorted by byte offset before extraction.
type packGroup struct {
	key     []byte // pack_key (BLAKE3 of the compressed pack bytes)
	members []packMemberRestore
}

// packMemberRestore locates one content inside its pack's uncompressed tar.
type packMemberRestore struct {
	content restoreContent
	offset  int64
	length  int64
}

// run resolves the requested contents, splits them into standalone objects
// and pack members, and restores each — recording per-content outcomes on
// rep. It returns an error for a fatal problem (enumeration failure) or a
// summary when individual contents failed; the caller derives the run's
// terminal status from rep.RcloneResult.
func (ar *archiveRestore) run(ctx context.Context, rep *Report, includeFile string) error {
	contents, err := ar.resolveContents(ctx, includeFile)
	if err != nil {
		return err
	}
	objects, packs, err := ar.classify(ctx, contents)
	if err != nil {
		return err
	}
	for _, c := range objects {
		ar.restoreObject(ctx, rep, c)
	}
	for _, g := range packs {
		ar.restorePack(ctx, rep, g)
	}
	if rep.RcloneResult.Errors > 0 {
		return fmt.Errorf("restore of %q from %q: %d content item(s) failed", ar.vol.Name, ar.dest.Name, rep.RcloneResult.Errors)
	}
	return nil
}

// resolveContents lists the volume's present paths from the local index and
// folds them into one restoreContent per hash (dedup). When includeFile is
// set (a `--from <node>` origin filter the CLI materialised), only its paths
// are restored.
func (ar *archiveRestore) resolveContents(ctx context.Context, includeFile string) ([]restoreContent, error) {
	rows, err := ar.store.ListPresentContent(ctx, ar.volID)
	if err != nil {
		return nil, fmt.Errorf("list present content for %q: %w", ar.vol.Name, err)
	}
	var include map[string]bool
	if includeFile != "" {
		if include, err = loadIncludeSet(includeFile); err != nil {
			return nil, err
		}
	}
	byContent := map[int64]int{} // content id -> index into out
	var out []restoreContent
	for _, r := range rows {
		if include != nil && !include[r.Path] {
			continue
		}
		if idx, ok := byContent[r.ContentID]; ok {
			out[idx].paths = append(out[idx].paths, r.Path)
			continue
		}
		byContent[r.ContentID] = len(out)
		out = append(out, restoreContent{contentID: r.ContentID, blake3: r.Blake3, size: r.SizeBytes, paths: []string{r.Path}})
	}
	return out, nil
}

// classify splits the resolved contents into standalone objects and
// per-pack member groups. A content with a pack_members row is a pack
// member; everything else is a per-hash object (the content-addressed
// layout has no packs, so every content lands there). Pack groups are
// returned sorted by key and their members sorted by offset so extraction
// streams the pack forward.
func (ar *archiveRestore) classify(ctx context.Context, contents []restoreContent) ([]restoreContent, []*packGroup, error) {
	var objects []restoreContent
	byPack := map[int64]*packGroup{}
	for _, c := range contents {
		m, err := ar.store.GetPackMember(ctx, c.contentID)
		if store.IsNotFound(err) {
			objects = append(objects, c)
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("locate content %s: %w", hex.EncodeToString(c.blake3), err)
		}
		g := byPack[m.PackID]
		if g == nil {
			pack, err := ar.store.GetPack(ctx, m.PackID)
			if err != nil {
				return nil, nil, fmt.Errorf("resolve pack %d for %s: %w", m.PackID, hex.EncodeToString(c.blake3), err)
			}
			g = &packGroup{key: pack.PackKey}
			byPack[m.PackID] = g
		}
		g.members = append(g.members, packMemberRestore{content: c, offset: m.ByteOffset, length: m.ByteLength})
	}
	return objects, sortedPackGroups(byPack), nil
}

// sortedPackGroups returns the groups keyed by pack id in a deterministic
// order (by pack key), each group's members sorted by ascending byte
// offset so a single forward pass over the decompressed pack reaches them
// in order.
func sortedPackGroups(byPack map[int64]*packGroup) []*packGroup {
	groups := make([]*packGroup, 0, len(byPack))
	for _, g := range byPack {
		slices.SortFunc(g.members, func(a, b packMemberRestore) int { return cmp.Compare(a.offset, b.offset) })
		groups = append(groups, g)
	}
	slices.SortFunc(groups, func(a, b *packGroup) int { return bytes.Compare(a.key, b.key) })
	return groups
}

// restoreObject fetches one per-hash object, verifies its BLAKE3, and
// writes it to every path that references it. A fetch or verification
// failure is recorded per content and does not abort the restore.
func (ar *archiveRestore) restoreObject(ctx context.Context, rep *Report, c restoreContent) {
	if ar.dryRun {
		ar.countContent(rep, c)
		return
	}
	tmp, err := ar.fetch(ctx, path.Join(ObjectsDirName, hex.EncodeToString(c.blake3)))
	if err != nil {
		ar.recordFailure(rep, c.firstPath(), err)
		return
	}
	defer func() { _ = os.Remove(tmp) }()
	digest, err := hashLocalFile(tmp)
	if err != nil {
		ar.recordFailure(rep, c.firstPath(), fmt.Errorf("re-hash object: %w", err))
		return
	}
	if !bytes.Equal(digest, c.blake3) {
		ar.recordFailure(rep, c.firstPath(), fmt.Errorf("object hashes to %s, want %s", hex.EncodeToString(digest), hex.EncodeToString(c.blake3)))
		return
	}
	ar.place(rep, tmp, c)
}

// restorePack fetches one pack once and extracts every requested member
// from the single decompressed stream.
func (ar *archiveRestore) restorePack(ctx context.Context, rep *Report, g *packGroup) {
	if ar.dryRun {
		for _, m := range g.members {
			ar.countContent(rep, m.content)
		}
		return
	}
	tmp, err := ar.fetch(ctx, path.Join(PacksDirName, hex.EncodeToString(g.key)))
	if err != nil {
		ar.failMembers(rep, g.members, err)
		return
	}
	defer func() { _ = os.Remove(tmp) }()
	ar.extractMembers(rep, tmp, g.members)
}

// extractMembers streams the decompressed pack once, seeking to each
// member's data offset and reading its length, re-hashing before writing.
// A stream-level failure fails the remaining members; a per-member hash
// mismatch fails only that content and continues.
func (ar *archiveRestore) extractMembers(rep *Report, packTmp string, members []packMemberRestore) {
	f, err := os.Open(packTmp)
	if err != nil {
		ar.failMembers(rep, members, err)
		return
	}
	defer func() { _ = f.Close() }()
	zr, err := zstd.NewReader(f)
	if err != nil {
		ar.failMembers(rep, members, fmt.Errorf("open pack stream: %w", err))
		return
	}
	defer zr.Close()
	var pos int64
	for i, m := range members {
		if m.offset < pos {
			ar.failMembers(rep, members[i:], fmt.Errorf("pack members overlap at offset %d", m.offset))
			return
		}
		if _, err := io.CopyN(io.Discard, zr, m.offset-pos); err != nil {
			ar.failMembers(rep, members[i:], fmt.Errorf("seek to pack member: %w", err))
			return
		}
		memberTmp, digest, err := ar.readMember(zr, m.length)
		pos = m.offset + m.length
		if err != nil {
			if memberTmp != "" {
				_ = os.Remove(memberTmp)
			}
			ar.failMembers(rep, members[i:], err)
			return
		}
		if !bytes.Equal(digest, m.content.blake3) {
			_ = os.Remove(memberTmp)
			ar.recordFailure(rep, m.content.firstPath(), fmt.Errorf("pack member hashes to %s, want %s", hex.EncodeToString(digest), hex.EncodeToString(m.content.blake3)))
			continue
		}
		ar.place(rep, memberTmp, m.content)
		_ = os.Remove(memberTmp)
	}
}

// readMember copies length bytes off the decompressed pack stream into a
// temp file, returning its path and the BLAKE3 of the bytes. The temp path
// is returned even on a short read so the caller can remove it.
func (ar *archiveRestore) readMember(r io.Reader, length int64) (string, []byte, error) {
	tmp, err := os.CreateTemp("", "squirrel-restore-member-*")
	if err != nil {
		return "", nil, fmt.Errorf("stage pack member: %w", err)
	}
	h := blake3.New()
	n, err := io.CopyN(io.MultiWriter(tmp, h), r, length)
	closeErr := tmp.Close()
	if err != nil {
		return tmp.Name(), nil, fmt.Errorf("read pack member: %w", err)
	}
	if closeErr != nil {
		return tmp.Name(), nil, fmt.Errorf("close pack member: %w", closeErr)
	}
	if n != length {
		return tmp.Name(), nil, fmt.Errorf("pack member short read: got %d, want %d", n, length)
	}
	return tmp.Name(), h.Sum(nil), nil
}

// fetch pulls one destination-root subpath (objects/<hash> or
// packs/<key>) to a local temp file through the same rclone read path —
// crypt overlay included — the push uses, and returns the temp path. The
// caller removes it.
func (ar *archiveRestore) fetch(ctx context.Context, subpath string) (string, error) {
	tmp, err := os.CreateTemp("", "squirrel-restore-*")
	if err != nil {
		return "", fmt.Errorf("stage download: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("stage download: %w", err)
	}
	uri := remoteSubpathURI(ar.dest, subpath)
	if err := ar.rcl.copyTo(ctx, uri, tmp.Name(), checkersArgs(ar.dest)...); err != nil {
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("fetch %s: %w", subpath, err)
	}
	return tmp.Name(), nil
}

// place writes the verified content at srcTmp to each of its paths under
// the target directory, counting the files landed. A per-path write
// failure is recorded and does not stop the others.
func (ar *archiveRestore) place(rep *Report, srcTmp string, c restoreContent) {
	for _, rel := range c.paths {
		if err := ar.writeOne(rel, srcTmp); err != nil {
			ar.recordFailure(rep, rel, err)
			continue
		}
		rep.RcloneResult.Transferred++
		rep.RcloneResult.Bytes += c.size
	}
}

// writeOne copies the verified bytes to <target>/<rel>, creating parent
// directories and — for an in-place restore — first preserving any file it
// would overwrite under .squirrel-restore-history/run-<id>/.
func (ar *archiveRestore) writeOne(rel, srcTmp string) error {
	dst := filepath.Join(ar.target, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create target dir: %w", err)
	}
	if ar.preserve {
		if err := ar.backupExisting(rel, dst); err != nil {
			return err
		}
	}
	return copyFileContents(srcTmp, dst)
}

// backupExisting moves an existing regular file at dst under the per-run
// restore-history subtree so an in-place overwrite never destroys prior
// bytes — the local-side counterpart of sync's --backup-dir.
func (ar *archiveRestore) backupExisting(rel, dst string) error {
	info, err := os.Lstat(dst)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat existing %s: %w", rel, err)
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	backup := filepath.Join(ar.target, RestoreHistoryDirName, "run-"+strconv.FormatInt(ar.runID, 10), filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(backup), 0o755); err != nil {
		return fmt.Errorf("create restore-history dir: %w", err)
	}
	if err := os.Rename(dst, backup); err != nil {
		return fmt.Errorf("preserve overwritten %s: %w", rel, err)
	}
	return nil
}

// countContent tallies the files (and bytes) a content would restore,
// used by the dry-run preview.
func (ar *archiveRestore) countContent(rep *Report, c restoreContent) {
	rep.RcloneResult.Transferred += int64(len(c.paths))
	rep.RcloneResult.Bytes += c.size * int64(len(c.paths))
}

// recordFailure counts one failed content and captures its message (capped).
func (ar *archiveRestore) recordFailure(rep *Report, object string, err error) {
	rep.RcloneResult.Errors++
	if int64(len(rep.RcloneResult.FailedFiles)) < maxFailedFiles {
		rep.RcloneResult.FailedFiles = append(rep.RcloneResult.FailedFiles, FailedFile{Object: object, Message: err.Error()})
	}
}

// failMembers records the same failure against every member's first path —
// used when a whole pack could not be fetched or its stream broke.
func (ar *archiveRestore) failMembers(rep *Report, members []packMemberRestore, err error) {
	for _, m := range members {
		ar.recordFailure(rep, m.content.firstPath(), err)
	}
}

// loadIncludeSet reads the newline-delimited path listing the CLI wrote for
// a `--from <node>` restore into a set. Paths are passed verbatim (raw),
// matching how writeRestorePathFilter emitted them.
func loadIncludeSet(pathname string) (map[string]bool, error) {
	f, err := os.Open(pathname)
	if err != nil {
		return nil, fmt.Errorf("open include list: %w", err)
	}
	defer func() { _ = f.Close() }()
	set := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			set[line] = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read include list: %w", err)
	}
	return set, nil
}

// copyFileContents writes src's bytes to dst, truncating an existing file.
// The bytes are already BLAKE3-verified by the caller, so a plain copy is
// safe.
func copyFileContents(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open restored bytes: %w", err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("write %s: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dst, err)
	}
	return nil
}
