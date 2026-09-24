package sync

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zeebo/blake3"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/index"
	"github.com/mbertschler/squirrel/runevents"
	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/volmark"
)

// mirrorFixture is one volume, "pics", mirrored to a native destination,
// "usb", with the destination's volume marker in place. The destination
// is a local directory unless the fixture is set up on another backend.
type mirrorFixture struct {
	store *store.Store
	pair  Pair
	src   string // the volume's source directory
	dst   string // the destination root
}

// mirrorBackend is where a mirror fixture's destination lives: its name,
// and the settings of a destination rooted at dst on it.
type mirrorBackend struct {
	name     string
	settings func(t *testing.T, dst string) string
}

var (
	localBackend = mirrorBackend{"local", func(_ *testing.T, dst string) string {
		return fmt.Sprintf("type = \"local\"\nroot = %q\n", dst)
	}}
	// sftpBackend serves the destination root from an in-process sftp
	// server, so the fixture reads what landed straight off the disk.
	sftpBackend = mirrorBackend{"sftp", func(t *testing.T, dst string) string {
		srv := startSFTPServer(t)
		host, port, _ := net.SplitHostPort(srv.addr)
		return fmt.Sprintf("type = \"sftp\"\nroot = %q\nhost = %q\nport = %q\nuser = \"u\"\npassword = \"p\"\nknown_hosts_file = %q\n",
			dst, host, port, srv.knownHosts(t, srv.hostKeys[0].PublicKey()))
	}}
	mirrorBackends = []mirrorBackend{localBackend, sftpBackend}
)

func setupMirrorFixture(t *testing.T) *mirrorFixture {
	t.Helper()
	return setupMirrorFixtureOn(t, localBackend)
}

func setupMirrorFixtureOn(t *testing.T, b mirrorBackend) *mirrorFixture {
	t.Helper()
	root := t.TempDir()
	f := &mirrorFixture{src: filepath.Join(root, "src"), dst: filepath.Join(root, "usb")}
	for _, d := range []string{f.src, filepath.Join(f.dst, "pics")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := volmark.Write(filepath.Join(f.dst, "pics"), volmark.Marker{Volume: "pics"}); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
	s, err := store.Open(filepath.Join(root, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	f.store = s
	cfgPath := filepath.Join(root, "config.toml")
	body := "[destinations.usb]\n" + b.settings(t, f.dst) + "\n" +
		"[volumes.pics]\npath = \"" + f.src + "\"\nsync_to = [\"usb\"]\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	f.pair = Pair{Volume: cfg.Volumes["pics"], Destination: cfg.Destinations["usb"]}
	return f
}

func (f *mirrorFixture) write(t *testing.T, rel, body string) {
	t.Helper()
	p := filepath.Join(f.src, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *mirrorFixture) index(t *testing.T) {
	t.Helper()
	if _, err := index.Index(context.Background(), f.store, f.src, index.Options{Name: "pics"}); err != nil {
		t.Fatalf("index: %v", err)
	}
}

// push runs one push with no rclone wrapper at all.
func (f *mirrorFixture) push(t *testing.T, opts Options) (Report, error) {
	t.Helper()
	return RunPair(context.Background(), f.store, Tools{}, f.pair, opts)
}

func (f *mirrorFixture) mustPush(t *testing.T) Report {
	t.Helper()
	rep, err := f.push(t, Options{})
	if err != nil || rep.Status != store.RunStatusSuccess {
		t.Fatalf("push: status=%q err=%v warnings=%v", rep.Status, err, rep.Warnings)
	}
	return rep
}

// pushCrashing runs one push whose transport crashes at the first call
// crashAt selects.
func (f *mirrorFixture) pushCrashing(t *testing.T, crashAt func(transportCall) bool, mode crashMode) (Report, error) {
	t.Helper()
	h := f.handler(t)
	h.openTransport = func(ctx context.Context, d *config.Destination) (transport, error) {
		raw, err := openDestinationTransport(ctx, d)
		if err != nil {
			return nil, err
		}
		return &faultTransport{transport: raw, crashAt: crashAt, mode: mode}, nil
	}
	return h.Push(context.Background(), Options{})
}

func (f *mirrorFixture) handler(t *testing.T) *mirrorHandler {
	t.Helper()
	h, err := HandlerFor(f.store, Tools{}, f.pair)
	if err != nil {
		t.Fatalf("HandlerFor: %v", err)
	}
	return h.(*mirrorHandler)
}

func (f *mirrorFixture) volumeID(t *testing.T) int64 {
	t.Helper()
	v, err := f.store.GetVolumeByName(context.Background(), "pics")
	if err != nil {
		t.Fatalf("GetVolumeByName: %v", err)
	}
	return v.ID
}

// dest is the path of rel under the destination's volume directory.
func (f *mirrorFixture) dest(rel string) string {
	return filepath.Join(f.dst, "pics", filepath.FromSlash(rel))
}

func (f *mirrorFixture) readDest(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(f.dest(rel))
	if err != nil {
		t.Fatalf("read destination %s: %v", rel, err)
	}
	return string(b)
}

func (f *mirrorFixture) rows(t *testing.T) []store.RemotePath {
	t.Helper()
	rows, err := f.store.ListRemotePaths(context.Background(), "usb", f.volumeID(t))
	if err != nil {
		t.Fatalf("ListRemotePaths: %v", err)
	}
	return rows
}

// rowsAt returns the states of rel's rows, oldest first.
func (f *mirrorFixture) rowsAt(t *testing.T, rel string) []string {
	t.Helper()
	var states []string
	for _, r := range f.rows(t) {
		if r.Path == rel {
			states = append(states, r.State)
		}
	}
	return states
}

// contentHashes collects the BLAKE3 of every file under the destination
// root outside staging: what invariant 2 says only grows.
func (f *mirrorFixture) contentHashes(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	err := filepath.WalkDir(f.dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == StagingDirName {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() {
			out[fileHash(t, p)] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk destination: %v", err)
	}
	return out
}

func fileHash(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	sum := blake3.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// checkInvariants asserts the design's three invariants after a clean
// push, given the content the destination held before it.
func (f *mirrorFixture) checkInvariants(t *testing.T, before map[string]bool) {
	t.Helper()
	ctx := context.Background()
	volID := f.volumeID(t)
	liveContent := map[string]int64{}
	for _, r := range f.rows(t) {
		want := f.contentHashOf(t, volID, r)
		switch r.State {
		case store.RemotePathLive:
			liveContent[r.Path] = r.ContentID
			if got := fileHash(t, f.dest(r.Path)); got != want {
				t.Errorf("live %s holds %s, want %s", r.Path, got, want)
			}
		case store.RemotePathDisplaced:
			p := filepath.Join(f.dst, filepath.FromSlash(historyName("pics", r.DisplacedRunID.Int64, r.Path)))
			if got := fileHash(t, p); got != want {
				t.Errorf("displaced %s holds %s at %s, want %s", r.Path, got, p, want)
			}
		case store.RemotePathCommitting, store.RemotePathDisplacing:
			t.Errorf("%s row %d is still %s after a clean push", r.Path, r.ID, r.State)
		}
	}
	after := f.contentHashes(t)
	for h := range before {
		if !after[h] {
			t.Errorf("content %s left the destination", h)
		}
	}
	present, err := f.store.ListPresentContent(ctx, volID)
	if err != nil {
		t.Fatalf("ListPresentContent: %v", err)
	}
	for _, d := range present {
		if liveContent[d.Path] != d.ContentID {
			t.Errorf("present %s has no live row holding content %d", d.Path, d.ContentID)
		}
	}
}

func (f *mirrorFixture) contentHashOf(t *testing.T, volID int64, r store.RemotePath) string {
	t.Helper()
	hist, err := f.store.ListHistoryByPath(context.Background(), volID, r.Path)
	if err != nil {
		t.Fatalf("ListHistoryByPath %s: %v", r.Path, err)
	}
	for _, h := range hist {
		if h.ContentID == r.ContentID {
			return hex.EncodeToString(h.Blake3)
		}
	}
	t.Fatalf("no files row holds content %d at %s", r.ContentID, r.Path)
	return ""
}

// TestMirrorPushWithoutRclone: a local mirror pushes through the native
// handler with no rclone wrapper, lands every present path with its bytes
// and mtime, records each one live with the BLAKE3 its read-back
// confirmed, writes the run's receipt, and advances the vector as
// fingerprint-verified. An unchanged second push writes nothing and still
// leaves a receipt.
func TestMirrorPushWithoutRclone(t *testing.T) {
	f := setupMirrorFixture(t)
	f.write(t, "a.txt", "alpha")
	f.write(t, "2024/cat.jpg", "meow")
	f.index(t)

	rep := f.mustPush(t)
	if rep.RcloneResult.Transferred != 2 || rep.RcloneResult.Bytes != int64(len("alpha")+len("meow")) {
		t.Fatalf("counters = %+v, want 2 paths, 9 bytes", rep.RcloneResult)
	}
	if f.readDest(t, "a.txt") != "alpha" || f.readDest(t, "2024/cat.jpg") != "meow" {
		t.Fatal("destination bytes differ from the source")
	}
	srcInfo, _ := os.Stat(filepath.Join(f.src, "a.txt"))
	dstInfo, _ := os.Stat(f.dest("a.txt"))
	if !dstInfo.ModTime().Equal(srcInfo.ModTime()) {
		t.Fatalf("mtime = %v, want the source's %v", dstInfo.ModTime(), srcInfo.ModTime())
	}
	for _, rel := range []string{"a.txt", "2024/cat.jpg"} {
		if got := f.rowsAt(t, rel); len(got) != 1 || got[0] != store.RemotePathLive {
			t.Fatalf("%s rows = %v, want one live", rel, got)
		}
	}
	for _, r := range f.rows(t) {
		if r.Checksum.String != hex.EncodeToString(r.Blake3) || !r.VerifiedAtNs.Valid {
			t.Fatalf("%s row = %+v, want the read-back BLAKE3 recorded", r.Path, r)
		}
	}
	if rep.Fingerprints != 2 {
		t.Fatalf("fingerprints = %d, want one read-back per path", rep.Fingerprints)
	}
	delta, err := f.store.ListPathDeltaSince(context.Background(), f.volumeID(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := encodeManifestSegment(delta)
	if got := f.readDest(t, ".squirrel-index/run-"+strconv.FormatInt(rep.RunID, 10)); got != string(want) {
		t.Fatalf("receipt = %q, want the delta's manifest segment %q", got, want)
	}
	comps := volumeComponents(t, f.store, "pics", "usb")
	if len(comps) != 1 || comps[0].VerifyMethod != store.VerifyMethodFingerprint {
		t.Fatalf("vector = %+v, want one fingerprint-verified component", comps)
	}
	run, err := f.store.GetRun(context.Background(), rep.RunID)
	if err != nil || !run.Shallow.Valid || !run.Shallow.Bool {
		t.Fatalf("run = %+v, %v; want recorded shallow", run, err)
	}

	if rep.AlreadyCorrect != 0 {
		t.Fatalf("first push already_correct = %d, want 0", rep.AlreadyCorrect)
	}
	again := f.mustPush(t)
	if again.RcloneResult.Transferred != 0 || !again.Changed.Valid || again.Changed.Int64 != 0 || again.AlreadyCorrect != 2 {
		t.Fatalf("unchanged push: counters=%+v changed=%+v already_correct=%d, want nothing written and both paths correct", again.RcloneResult, again.Changed, again.AlreadyCorrect)
	}
	if got := f.readDest(t, ".squirrel-index/run-"+strconv.FormatInt(again.RunID, 10)); got != "" {
		t.Fatalf("unchanged push's receipt = %q, want empty", got)
	}
	f.checkInvariants(t, nil)
}

// TestMirrorTranslationRules is the design's translation table, decided
// from squirrel's records alone. After one push, a changed path, an added
// one, a missing one and an offloaded one are re-planned against the full
// delta: only present paths produce an operation, and a path whose live
// record already holds its content is only confirmed.
func TestMirrorTranslationRules(t *testing.T) {
	f := setupMirrorFixture(t)
	for _, rel := range []string{"same.txt", "changed.txt", "gone.txt", "offloaded.txt"} {
		f.write(t, rel, "v1 "+rel)
	}
	f.index(t)
	f.mustPush(t)
	f.write(t, "changed.txt", "v2 changed")
	f.write(t, "new.txt", "new")
	if err := os.Remove(filepath.Join(f.src, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	f.index(t)
	ctx := context.Background()
	volID := f.volumeID(t)
	off, err := f.store.GetByPath(ctx, volID, "offloaded.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.MarkOffloaded(ctx, volID, "offloaded.txt", off.ContentID, off.LastSeenRunID); err != nil {
		t.Fatalf("MarkOffloaded: %v", err)
	}
	delta, err := f.store.ListPathDeltaSince(ctx, volID, 0)
	if err != nil {
		t.Fatal(err)
	}

	ops, err := f.handler(t).translate(ctx, pushPlan{volumeID: volID, delta: delta})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	got := map[string]string{}
	for _, p := range ops.paths {
		got[p.delta.Path] = map[bool]string{true: "confirm", false: "write"}[p.current]
	}
	want := map[string]string{
		"same.txt":    "confirm", // present, live record holds C
		"changed.txt": "write",   // present, live record holds other content
		"new.txt":     "write",   // present, no live record
		// gone.txt (missing), offloaded.txt (offloaded) and changed.txt's
		// superseded row produce nothing.
	}
	if len(got) != len(want) {
		t.Fatalf("operations = %v, want %v", got, want)
	}
	for rel, op := range want {
		if got[rel] != op {
			t.Errorf("%s: %s, want %s", rel, got[rel], op)
		}
	}
}

// TestMirrorWriteDisplacesWhatThePathHolds carries the translation table's
// destructive decisions to the disk: a recorded version moves into the
// run's history as its record, an entry squirrel never wrote moves there as
// unrecorded bytes with a warning and an audit note, and a path deleted at
// the source stays on the destination.
func TestMirrorWriteDisplacesWhatThePathHolds(t *testing.T) {
	f := setupMirrorFixture(t)
	f.write(t, "changed.txt", "v1")
	f.write(t, "gone.txt", "kept")
	f.index(t)
	f.mustPush(t)
	f.write(t, "changed.txt", "v2")
	f.write(t, "new.txt", "ours")
	if err := os.Remove(filepath.Join(f.src, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.dest("new.txt"), []byte("foreign"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.index(t)
	before := f.contentHashes(t)

	rep := f.mustPush(t)
	run := strconv.FormatInt(rep.RunID, 10)
	if f.readDest(t, "changed.txt") != "v2" || f.readDest(t, ".squirrel-history/run-"+run+"/changed.txt") != "v1" {
		t.Fatal("the recorded version did not move into this run's history")
	}
	if got := f.rowsAt(t, "changed.txt"); len(got) != 2 || got[0] != store.RemotePathDisplaced || got[1] != store.RemotePathLive {
		t.Fatalf("changed.txt rows = %v, want displaced then live", got)
	}
	if f.readDest(t, "new.txt") != "ours" || f.readDest(t, ".squirrel-history/run-"+run+"/new.txt") != "foreign" {
		t.Fatal("the foreign entry was not preserved in history")
	}
	if !warned(rep, "new.txt held bytes squirrel did not write") {
		t.Fatalf("warnings = %v, want one about the foreign new.txt", rep.Warnings)
	}
	audit, err := f.store.ListRunAudit(context.Background(), rep.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if !auditHas(audit, store.TransitionDisplaceUnrecorded, "new.txt") {
		t.Fatalf("audit = %+v, want a displace-unrecorded note for new.txt", audit)
	}
	if f.readDest(t, "gone.txt") != "kept" {
		t.Fatal("a path deleted at the source left the destination")
	}
	f.checkInvariants(t, before)
}

// TestMirrorRecordChangedBehindSquirrelsBack: an already-correct path
// whose bytes changed on the destination loses its record, is displaced as
// unrecorded bytes, and is written again. The first push crashes before its
// receipt, so the next one re-plans every path.
func TestMirrorRecordChangedBehindSquirrelsBack(t *testing.T) {
	f := setupMirrorFixture(t)
	f.write(t, "a.txt", "ours")
	f.index(t)
	if rep, err := f.pushCrashing(t, isReceiptPut, crashBefore); !crashedOn(rep, err) {
		t.Fatalf("crashing push = %v, want the injected crash", err)
	}
	if err := os.WriteFile(f.dest("a.txt"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := f.contentHashes(t)

	rep := f.mustPush(t)
	if f.readDest(t, "a.txt") != "ours" {
		t.Fatal("the tampered path was not written again")
	}
	if got := f.rowsAt(t, "a.txt"); len(got) != 2 || got[0] != store.RemotePathLost || got[1] != store.RemotePathLive {
		t.Fatalf("a.txt rows = %v, want lost then live — never displaced", got)
	}
	if !warned(rep, "behind squirrel's back") {
		t.Fatalf("warnings = %v, want one about the changed path", rep.Warnings)
	}
	f.checkInvariants(t, before)
}

func warned(rep Report, substr string) bool {
	for _, w := range rep.Warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

func auditHas(audit []store.RunAudit, transition, substr string) bool {
	for _, a := range audit {
		if a.Transition == transition && strings.Contains(a.Note.String, substr) {
			return true
		}
	}
	return false
}

func isReceiptPut(c transportCall) bool {
	return c.op == "put" && strings.Contains(c.name, IndexDirName+"/run-")
}

// mirrorCrashCase is one row of the crash table: where the push dies, and
// what it leaves recorded.
type mirrorCrashCase struct {
	name    string
	crashAt func(transportCall) bool
	mode    crashMode
	// crashed is the changed path's rows after the crash, oldest first.
	crashed []string
	check   func(t *testing.T, f *mirrorFixture, clean Report)
}

// TestMirrorCrashTable is the design's crash table: a push that dies at
// each point of writing one changed path, then a clean push, on both
// transports. Each row pins what the crash leaves recorded and how
// reconcile settles it; afterwards the three invariants hold.
func TestMirrorCrashTable(t *testing.T) {
	staged := func(c transportCall) bool { return strings.Contains(c.name, StagingDirName+"/") }
	intoHistory := func(c transportCall) bool { return c.op == "rename" && strings.Contains(c.to, HistoryDirName+"/") }
	commitRename := func(c transportCall) bool { return c.op == "rename" && staged(c) }
	cases := []mirrorCrashCase{
		{"staging write", func(c transportCall) bool { return c.op == "put" && staged(c) }, crashMidway,
			[]string{store.RemotePathLive}, nil},
		{"displacing recorded", intoHistory, crashBefore,
			[]string{store.RemotePathDisplacing}, nil},
		{"displace rename", intoHistory, crashAfter,
			[]string{store.RemotePathDisplacing}, nil},
		{"committing recorded", commitRename, crashBefore,
			[]string{store.RemotePathDisplaced, store.RemotePathCommitting}, nil},
		{"commit rename", commitRename, crashAfter,
			[]string{store.RemotePathDisplaced, store.RemotePathCommitting}, nil},
		{"every write, before the seal", isReceiptPut, crashBefore,
			[]string{store.RemotePathDisplaced, store.RemotePathLive},
			func(t *testing.T, _ *mirrorFixture, clean Report) {
				if clean.RcloneResult.Transferred != 0 || clean.RcloneResult.Checked != 1 || clean.AlreadyCorrect != 2 {
					t.Fatalf("clean push counters = %+v already_correct=%d, want the planned path confirmed and both paths correct", clean.RcloneResult, clean.AlreadyCorrect)
				}
			}},
	}
	for _, b := range mirrorBackends {
		for _, c := range cases {
			t.Run(b.name+"/"+c.name, func(t *testing.T) {
				runCrashCase(t, setupMirrorFixtureOn(t, b), c)
			})
		}
	}
}

func runCrashCase(t *testing.T, f *mirrorFixture, c mirrorCrashCase) {
	f.write(t, "keep.txt", "untouched")
	f.write(t, "a.txt", "v1")
	f.index(t)
	f.mustPush(t)
	f.write(t, "a.txt", "v2 is longer")
	f.index(t)
	before := f.contentHashes(t)

	if rep, err := f.pushCrashing(t, c.crashAt, c.mode); !crashedOn(rep, err) {
		t.Fatalf("crashing push = %v (failures %+v), want the injected crash", err, rep.RcloneResult.FailedFiles)
	}
	if got := f.rowsAt(t, "a.txt"); !slicesEqual(got, c.crashed) {
		t.Fatalf("rows after the crash = %v, want %v", got, c.crashed)
	}
	clean := f.mustPush(t)
	if f.readDest(t, "a.txt") != "v2 is longer" {
		t.Fatal("the clean push did not land the new version")
	}
	if c.check != nil {
		c.check(t, f, clean)
	}
	f.checkInvariants(t, before)
	f.checkStagingEmpty(t, clean.RunID)
}

// crashedOn reports whether a push failed on the injected crash, directly
// or on one of its paths.
func crashedOn(rep Report, err error) bool {
	if errors.Is(err, errInjectedCrash) {
		return true
	}
	for _, ff := range rep.RcloneResult.FailedFiles {
		if strings.Contains(ff.Message, errInjectedCrash.Error()) {
			return err != nil
		}
	}
	return false
}

func slicesEqual(a, b []string) bool {
	return strings.Join(a, ",") == strings.Join(b, ",")
}

// checkStagingEmpty asserts that only the latest run's staging directory
// is left, and that it holds nothing: every earlier run's staging was
// removed by reconcile.
func (f *mirrorFixture) checkStagingEmpty(t *testing.T, latest int64) {
	t.Helper()
	dir := f.dest(StagingDirName)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "run-"+strconv.FormatInt(latest, 10) {
			t.Errorf("staging still holds %s", e.Name())
			continue
		}
		if left, _ := os.ReadDir(filepath.Join(dir, e.Name())); len(left) != 0 {
			t.Errorf("the latest run's staging holds %d entries", len(left))
		}
	}
}

// TestMirrorRefusesShallow: --shallow has nothing to switch off on a
// native mirror, so it is refused before any run starts.
func TestMirrorRefusesShallow(t *testing.T) {
	f := setupMirrorFixture(t)
	f.write(t, "a.txt", "alpha")
	f.index(t)
	_, err := f.push(t, Options{Shallow: true})
	if err == nil || !strings.Contains(err.Error(), "--shallow") {
		t.Fatalf("shallow push = %v, want a refusal naming --shallow", err)
	}
	if runs := mustListSyncRuns(t, f.store); len(runs) != 0 {
		t.Fatalf("a refused shallow push wrote runs: %+v", runs)
	}
}

func mustListSyncRuns(t *testing.T, s *store.Store) []store.Run {
	t.Helper()
	runs, err := s.ListRuns(context.Background(), store.ListRunsOpts{})
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Run
	for _, r := range runs {
		if r.Kind == store.RunKindSync {
			out = append(out, r)
		}
	}
	return out
}

// TestMirrorDryRunWritesNothing: a dry run previews what a push would
// write and changes nothing — no runs row, no files, no records.
func TestMirrorDryRunWritesNothing(t *testing.T) {
	f := setupMirrorFixture(t)
	f.write(t, "a.txt", "alpha")
	f.index(t)
	f.mustPush(t)
	f.write(t, "a.txt", "alpha two")
	f.write(t, "b.txt", "beta")
	f.index(t)
	before := len(f.rows(t))

	rep, err := f.push(t, Options{DryRun: true})
	if err != nil || rep.RunID != 0 {
		t.Fatalf("dry run: run=%d err=%v", rep.RunID, err)
	}
	if rep.RcloneResult.Transferred != 2 || rep.RcloneResult.Bytes != int64(len("alpha two")+len("beta")) {
		t.Fatalf("preview = %+v, want 2 paths", rep.RcloneResult)
	}
	if _, err := os.Stat(f.dest("b.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the dry run wrote b.txt: %v", err)
	}
	if len(f.rows(t)) != before || len(mustListSyncRuns(t, f.store)) != 1 {
		t.Fatal("the dry run recorded something")
	}
}

// TestMirrorMarkerRefusalIsRecorded: without the volume marker — an
// unplugged disk, a mistyped root — the push refuses and records the
// refusal as its own run.
func TestMirrorMarkerRefusalIsRecorded(t *testing.T) {
	f := setupMirrorFixture(t)
	if err := os.Remove(filepath.Join(f.dst, "pics", volmark.MarkerName)); err != nil {
		t.Fatal(err)
	}
	f.write(t, "a.txt", "alpha")
	f.index(t)
	rep, err := f.push(t, Options{})
	if !errors.Is(err, ErrRefused) || rep.Status != store.RunStatusRefused {
		t.Fatalf("push without a marker: status=%q err=%v, want a recorded refusal", rep.Status, err)
	}
	if runs := mustListSyncRuns(t, f.store); len(runs) != 1 || runs[0].Status != store.RunStatusRefused {
		t.Fatalf("sync runs = %+v, want one refused", runs)
	}
}

// TestMirrorWatermarkRule: a native mirror adopts no existing tree. An
// rclone-era success with its tree still there is refused; once the root
// is emptied it is a fresh start; and a root emptied behind squirrel's back
// while its records remain is refused again.
func TestMirrorWatermarkRule(t *testing.T) {
	f := setupMirrorFixture(t)
	f.write(t, "a.txt", "alpha")
	f.index(t)
	ctx := context.Background()
	rcloneRun, err := f.store.BeginRun(ctx, store.RunKindSync, f.volumeID(t), "usb", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.FinishRun(ctx, rcloneRun, store.RunStatusSuccess, "", 1); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.dest("a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = f.push(t, Options{})
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "does not adopt") {
		t.Fatalf("push over an rclone-era tree = %v, want a refusal", err)
	}

	if err := os.Remove(f.dest("a.txt")); err != nil {
		t.Fatal(err)
	}
	f.mustPush(t)
	if f.readDest(t, "a.txt") != "alpha" {
		t.Fatal("the fresh start did not write the tree")
	}

	if err := os.RemoveAll(filepath.Join(f.dst, "pics")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(f.dst, "pics"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := volmark.Write(filepath.Join(f.dst, "pics"), volmark.Marker{Volume: "pics"}); err != nil {
		t.Fatal(err)
	}
	_, err = f.push(t, Options{})
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "destination reset") {
		t.Fatalf("push to a wiped root with records = %v, want a refusal", err)
	}
}

// TestMirrorForeignStagingIsLeftAlone: something in staging squirrel did
// not name is reported and never removed.
func TestMirrorForeignStagingIsLeftAlone(t *testing.T) {
	f := setupMirrorFixture(t)
	f.write(t, "a.txt", "alpha")
	f.index(t)
	first := f.mustPush(t)
	foreign := f.dest(filepath.Join(StagingDirName, "run-"+strconv.FormatInt(first.RunID, 10), "notes.txt"))
	if err := os.WriteFile(foreign, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep := f.mustPush(t)
	if !warned(rep, "notes.txt is in squirrel's staging") {
		t.Fatalf("warnings = %v, want the foreign file reported", rep.Warnings)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("the foreign file was removed: %v", err)
	}
}

// TestMirrorFileDirectorySwap: a file that became a directory, and a
// directory that became a file, each move the old entry into history —
// the directory with every version recorded in it — before the new one
// lands.
func TestMirrorFileDirectorySwap(t *testing.T) {
	f := setupMirrorFixture(t)
	f.write(t, "a", "file a")
	f.write(t, "d/x", "inside d")
	f.index(t)
	f.mustPush(t)
	for _, rel := range []string{"a", "d"} {
		if err := os.RemoveAll(filepath.Join(f.src, rel)); err != nil {
			t.Fatal(err)
		}
	}
	f.write(t, "a/b", "under a")
	f.write(t, "d", "file d")
	f.index(t)
	before := f.contentHashes(t)

	rep := f.mustPush(t)
	run := strconv.FormatInt(rep.RunID, 10)
	if f.readDest(t, "a/b") != "under a" || f.readDest(t, "d") != "file d" {
		t.Fatal("the swapped paths did not land")
	}
	if f.readDest(t, ".squirrel-history/run-"+run+"/a") != "file a" || f.readDest(t, ".squirrel-history/run-"+run+"/d/x") != "inside d" {
		t.Fatal("the replaced entries are not in history")
	}
	if got := f.rowsAt(t, "d/x"); len(got) != 1 || got[0] != store.RemotePathDisplaced {
		t.Fatalf("d/x rows = %v, want displaced with its directory", got)
	}
	f.checkInvariants(t, before)
}

// TestMirrorProgressEvents: a push reports one progress event per planned
// path.
func TestMirrorProgressEvents(t *testing.T) {
	f := setupMirrorFixture(t)
	f.write(t, "a.txt", "alpha")
	f.write(t, "b.txt", "beta")
	f.index(t)
	var events []runevents.Progress
	_, err := f.push(t, Options{Progress: func(p runevents.Progress) { events = append(events, p) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Done != 2 || events[1].Total != 2 || events[1].BytesDone != 9 {
		t.Fatalf("events = %+v, want two ending at 2/2 paths and 9 bytes", events)
	}
}

// blockingTransport holds every Stat until release is closed.
type blockingTransport struct {
	transport
	release chan struct{}
}

func (b blockingTransport) Stat(ctx context.Context, name string) (entry, error) {
	<-b.release
	return b.transport.Stat(ctx, name)
}

// TestMirrorStallFailsThePush: a transport call that makes no progress
// fails the push after the stall timeout, and a later push to the same
// destination refuses while that call is still inside the system.
func TestMirrorStallFailsThePush(t *testing.T) {
	f := setupMirrorFixture(t)
	f.write(t, "a.txt", "alpha")
	f.index(t)
	f.mustPush(t)
	f.write(t, "a.txt", "alpha two")
	f.index(t)

	release := make(chan struct{})
	h := f.handler(t)
	h.stallTimeout = 50 * time.Millisecond
	h.openTransport = func(ctx context.Context, d *config.Destination) (transport, error) {
		raw, err := openDestinationTransport(ctx, d)
		return blockingTransport{transport: raw, release: release}, err
	}
	if _, err := h.Push(context.Background(), Options{}); !errors.Is(err, errTransportStalled) {
		t.Fatalf("stalled push = %v, want errTransportStalled", err)
	}
	if _, err := f.push(t, Options{}); err == nil || !strings.Contains(err.Error(), "have not returned") {
		t.Fatalf("push while a call is stuck = %v, want a refusal", err)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for stalledCounter("usb").Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	f.mustPush(t)
	if f.readDest(t, "a.txt") != "alpha two" {
		t.Fatal("the push after the stall cleared did not land")
	}
	f.checkInvariants(t, nil)
}

// corruptingTransport returns every staged file it reads with its first
// byte flipped, as a disk that kept other bytes than it was given.
type corruptingTransport struct{ transport }

func (c corruptingTransport) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	rc, err := c.transport.Get(ctx, name)
	if err != nil || !strings.Contains(name, "/"+StagingDirName+"/") {
		return rc, err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	if len(b) > 0 {
		b[0] ^= 0xff
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// TestMirrorReadBackRefusesBytesTheDiskChanged: a staged copy that reads
// back as other bytes than were sent is never committed. The path fails,
// the run fails before its receipt, and the staged copy stays for the next
// push's reconcile.
func TestMirrorReadBackRefusesBytesTheDiskChanged(t *testing.T) {
	f := setupMirrorFixture(t)
	f.write(t, "a.txt", "alpha")
	f.index(t)
	h := f.handler(t)
	h.openTransport = func(ctx context.Context, d *config.Destination) (transport, error) {
		raw, err := openDestinationTransport(ctx, d)
		return corruptingTransport{raw}, err
	}
	rep, err := h.Push(context.Background(), Options{})
	if err == nil || rep.Status != store.RunStatusFailed {
		t.Fatalf("push: status=%q err=%v, want a failed run", rep.Status, err)
	}
	if len(rep.RcloneResult.FailedFiles) != 1 || !strings.Contains(rep.RcloneResult.FailedFiles[0].Message, errReadBackMismatch.Error()) {
		t.Fatalf("failed files = %+v, want a read-back mismatch", rep.RcloneResult.FailedFiles)
	}
	if _, err := os.Stat(f.dest("a.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a.txt was committed: %v", err)
	}
	if rows := f.rows(t); len(rows) != 0 {
		t.Fatalf("rows = %+v, want none", rows)
	}
	staged := filepath.Join(f.dst, filepath.FromSlash(stagingName("pics", rep.RunID, stagingKey("a.txt"))))
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("staged copy: %v", err)
	}
	f.mustPush(t)
	if _, err := os.Stat(staged); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the next push left the failed run's staging: %v", err)
	}
	f.checkInvariants(t, nil)
}
