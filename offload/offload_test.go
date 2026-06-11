package offload

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeebo/blake3"

	"github.com/mbertschler/squirrel/index"
	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/volmark"
)

func setupStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// volName is the single volume name every test in this package indexes
// and offloads under.
const volName = "vol"

func indexVolume(t *testing.T, s *store.Store, root string) index.Report {
	t.Helper()
	rep, err := index.Index(context.Background(), s, root, index.Options{Name: volName, Workers: 2})
	if err != nil {
		t.Fatalf("Index %s: %v", root, err)
	}
	if rep.Errors > 0 {
		t.Fatalf("index errors: %v", rep.ErrorList)
	}
	return rep
}

func testVolume(t *testing.T, s *store.Store) store.Volume {
	t.Helper()
	v, err := s.GetVolumeByName(context.Background(), volName)
	if err != nil {
		t.Fatalf("GetVolumeByName(%q): %v", volName, err)
	}
	return v
}

func selfNode(t *testing.T, s *store.Store) store.Node {
	t.Helper()
	n, err := s.GetSelfNode(context.Background())
	if err != nil {
		t.Fatalf("GetSelfNode: %v", err)
	}
	return n
}

// seedVector records durability evidence the same way every real
// advancement path (destination handlers, peer-sync, the durability
// pull) does: one monotonic UpsertDestinationRunID per component.
func seedVector(t *testing.T, s *store.Store, volumeID int64, target string, nodeID, run int64) {
	t.Helper()
	if err := s.UpsertDestinationRunID(context.Background(), volumeID, target, nodeID, run, false); err != nil {
		t.Fatalf("UpsertDestinationRunID(%s): %v", target, err)
	}
}

func rowAt(t *testing.T, s *store.Store, volumeID int64, relPath string) store.FileRow {
	t.Helper()
	r, err := s.GetByPath(context.Background(), volumeID, relPath)
	if err != nil {
		t.Fatalf("GetByPath(%s): %v", relPath, err)
	}
	return r
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("expected %s on disk: %v", path, err)
	}
}

func mustBeGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err == nil {
		t.Fatalf("expected %s to be deleted", path)
	}
}

func countRuns(t *testing.T, s *store.Store) int {
	t.Helper()
	runs, err := s.ListRuns(context.Background(), store.ListRunsOpts{})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	return len(runs)
}

// oneResult asserts the report carries exactly one result for relPath
// with the given outcome and returns it.
func oneResult(t *testing.T, rep Report, relPath string, outcome Outcome) FileResult {
	t.Helper()
	for _, r := range rep.Results {
		if r.Path == relPath {
			if r.Outcome != outcome {
				t.Fatalf("%s outcome = %d (%v), want %d", relPath, r.Outcome, r.Reasons, outcome)
			}
			return r
		}
	}
	t.Fatalf("no result for %s in %+v", relPath, rep.Results)
	return FileResult{}
}

// TestOffloadHappyPath: with every required target's vector covering
// the volume's content, all selected files are unlinked and their rows
// flipped present → offloaded with last_seen_run_id stamped to the one
// kind='offload' run that wraps the invocation.
func TestOffloadHappyPath(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	writeFile(t, filepath.Join(root, "sub", "b.txt"), "bravo")
	s := setupStore(t)
	ctx := context.Background()
	idx := indexVolume(t, s, root)
	v := testVolume(t, s)
	self := selfNode(t, s)
	seedVector(t, s, v.ID, "t1", self.ID, idx.RunID)
	seedVector(t, s, v.ID, "t2", self.ID, idx.RunID)

	rep, err := Offload(ctx, s, root, Options{
		Name: volName, Paths: []string{"."}, Require: []string{"t1", "t2"},
	})
	if err != nil {
		t.Fatalf("Offload: %v", err)
	}
	if rep.Offloaded != 2 || rep.NotDurable != 0 || rep.Drift != 0 || rep.Errors != 0 {
		t.Fatalf("report = %+v", rep)
	}
	oneResult(t, rep, "a.txt", OutcomeOffloaded)
	oneResult(t, rep, "sub/b.txt", OutcomeOffloaded)
	mustBeGone(t, filepath.Join(root, "a.txt"))
	mustBeGone(t, filepath.Join(root, "sub", "b.txt"))

	for _, p := range []string{"a.txt", "sub/b.txt"} {
		row := rowAt(t, s, v.ID, p)
		if row.Status != store.StatusOffloaded {
			t.Fatalf("%s status = %q, want offloaded", p, row.Status)
		}
		if row.LastSeenRunID != rep.RunID {
			t.Fatalf("%s last_seen_run_id = %d, want offload run %d", p, row.LastSeenRunID, rep.RunID)
		}
		if row.FirstSeenRunID != idx.RunID {
			t.Fatalf("%s first_seen_run_id = %d, want index run %d", p, row.FirstSeenRunID, idx.RunID)
		}
	}

	run, err := s.GetRun(ctx, rep.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Kind != store.RunKindOffload || run.Destination.Valid ||
		run.Status != store.RunStatusSuccess || run.FileCount != 2 {
		t.Fatalf("offload run = %+v, want kind=offload destination=NULL status=success file_count=2", run)
	}
}

// TestOffloadGateMissingComponent: a required target with no vector
// component for the content's origin keeps the file on disk, with the
// failure reported per target.
func TestOffloadGateMissingComponent(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	s := setupStore(t)
	idx := indexVolume(t, s, root)
	v := testVolume(t, s)
	self := selfNode(t, s)
	seedVector(t, s, v.ID, "t1", self.ID, idx.RunID)

	rep, err := Offload(context.Background(), s, root, Options{
		Name: volName, Paths: []string{"."}, Require: []string{"t1", "t2"},
	})
	if err != nil {
		t.Fatalf("Offload: %v", err)
	}
	if rep.Offloaded != 0 || rep.NotDurable != 1 || rep.Errors != 0 {
		t.Fatalf("report = %+v", rep)
	}
	res := oneResult(t, rep, "a.txt", OutcomeNotDurable)
	if len(res.Reasons) != 1 || !strings.Contains(res.Reasons[0], "t2: missing component for origin "+self.Name) {
		t.Fatalf("reasons = %v, want one t2 missing-component failure", res.Reasons)
	}
	mustExist(t, filepath.Join(root, "a.txt"))
	if row := rowAt(t, s, v.ID, "a.txt"); row.Status != store.StatusPresent {
		t.Fatalf("status = %q, want present", row.Status)
	}

	run, err := s.GetRun(context.Background(), rep.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != store.RunStatusSuccess || run.FileCount != 0 {
		t.Fatalf("skip-only run = %+v, want status=success file_count=0", run)
	}
}

// TestOffloadGateStaleComponent: content introduced after the target's
// recorded watermark is refused with the have/need pair, while content
// the watermark covers offloads in the same invocation.
func TestOffloadGateStaleComponent(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	s := setupStore(t)
	first := indexVolume(t, s, root)
	v := testVolume(t, s)
	self := selfNode(t, s)
	seedVector(t, s, v.ID, "t1", self.ID, first.RunID)

	writeFile(t, filepath.Join(root, "b.txt"), "bravo")
	second := indexVolume(t, s, root)

	rep, err := Offload(context.Background(), s, root, Options{
		Name: volName, Paths: []string{"."}, Require: []string{"t1"},
	})
	if err != nil {
		t.Fatalf("Offload: %v", err)
	}
	oneResult(t, rep, "a.txt", OutcomeOffloaded)
	res := oneResult(t, rep, "b.txt", OutcomeNotDurable)
	want := fmt.Sprintf("t1: stale: have %d need %d", first.RunID, second.RunID)
	if len(res.Reasons) != 1 || !strings.Contains(res.Reasons[0], want) {
		t.Fatalf("reasons = %v, want %q", res.Reasons, want)
	}
	mustBeGone(t, filepath.Join(root, "a.txt"))
	mustExist(t, filepath.Join(root, "b.txt"))
}

// TestOffloadPeerOriginContent: content carrying a recorded origin
// (node, run) is gated against that origin's vector component — the
// self component is irrelevant for it.
func TestOffloadPeerOriginContent(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "seed.txt"), "seed")
	s := setupStore(t)
	ctx := context.Background()
	idx := indexVolume(t, s, root)
	v := testVolume(t, s)
	peer, err := s.CreateNode(ctx, "peer1", "http://peer1.internal")
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	for _, f := range []struct {
		name      string
		content   string
		originRun int64
	}{
		{"covered.txt", "from peer covered", 7},
		{"ahead.txt", "from peer ahead", 9},
	} {
		p := filepath.Join(root, f.name)
		writeFile(t, p, f.content)
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		sum := blake3.Sum256([]byte(f.content))
		err = s.Upsert(ctx, store.FileRow{
			VolumeID: v.ID, Path: f.name, Blake3: sum[:], SizeBytes: fi.Size(),
			MtimeNs: fi.ModTime().UnixNano(), Status: store.StatusPresent,
			FirstSeenRunID: idx.RunID, LastSeenRunID: idx.RunID, IndexedAtNs: store.NowNs(),
		}, &store.Provenance{NodeID: peer.ID, RunID: f.originRun})
		if err != nil {
			t.Fatalf("Upsert %s: %v", f.name, err)
		}
	}
	seedVector(t, s, v.ID, "t1", peer.ID, 7)

	rep, err := Offload(ctx, s, root, Options{
		Name: volName, Paths: []string{"covered.txt", "ahead.txt"}, Require: []string{"t1"},
	})
	if err != nil {
		t.Fatalf("Offload: %v", err)
	}
	oneResult(t, rep, "covered.txt", OutcomeOffloaded)
	res := oneResult(t, rep, "ahead.txt", OutcomeNotDurable)
	if !strings.Contains(res.Reasons[0], "t1: stale: have 7 need 9 (origin peer1)") {
		t.Fatalf("reasons = %v, want stale failure naming origin peer1", res.Reasons)
	}
	mustBeGone(t, filepath.Join(root, "covered.txt"))
	mustExist(t, filepath.Join(root, "ahead.txt"))
}

// TestOffloadPeerPulledEvidenceTarget: the policy may require a target
// no local config declares — its evidence arrives via the peer
// durability pull, which lands in destination_run_ids under the peer's
// target name. The gate consumes those rows like any other.
func TestOffloadPeerPulledEvidenceTarget(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	s := setupStore(t)
	idx := indexVolume(t, s, root)
	v := testVolume(t, s)
	seedVector(t, s, v.ID, "remote-archive", selfNode(t, s).ID, idx.RunID)

	rep, err := Offload(context.Background(), s, root, Options{
		Name: volName, Paths: []string{"a.txt"}, Require: []string{"remote-archive"},
	})
	if err != nil {
		t.Fatalf("Offload: %v", err)
	}
	oneResult(t, rep, "a.txt", OutcomeOffloaded)
	mustBeGone(t, filepath.Join(root, "a.txt"))
}

// TestOffloadEmptyPolicyRefused: without an explicit offload policy the
// command refuses before touching the index, the disk, or the runs
// table.
func TestOffloadEmptyPolicyRefused(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	s := setupStore(t)
	indexVolume(t, s, root)
	runsBefore := countRuns(t, s)

	_, err := Offload(context.Background(), s, root, Options{
		Name: volName, Paths: []string{"."},
	})
	if err == nil || !strings.Contains(err.Error(), "no offload policy") {
		t.Fatalf("err = %v, want empty-policy refusal", err)
	}
	mustExist(t, filepath.Join(root, "a.txt"))
	if got := countRuns(t, s); got != runsBefore {
		t.Fatalf("runs count = %d, want %d (refusal must record nothing)", got, runsBefore)
	}
}

// TestOffloadRequiresSelector: a bare invocation with neither paths nor
// an age bound is refused — selecting the whole volume takes an
// explicit ".".
func TestOffloadRequiresSelector(t *testing.T) {
	s := setupStore(t)
	_, err := Offload(context.Background(), s, t.TempDir(), Options{
		Name: volName, Require: []string{"t1"},
	})
	if err == nil || !strings.Contains(err.Error(), "needs a selector") {
		t.Fatalf("err = %v, want selector refusal", err)
	}
}

// TestOffloadSelectorSemantics: an exact file path selects one file, a
// directory prefix selects its subtree on path-segment boundaries, and
// unmatched siblings stay untouched.
func TestOffloadSelectorSemantics(t *testing.T) {
	root := t.TempDir()
	files := []string{"Photos/2019/a.jpg", "Photos/2019-extra/b.jpg", "Photos/2020/c.jpg", "docs/d.txt"}
	for i, f := range files {
		writeFile(t, filepath.Join(root, filepath.FromSlash(f)), fmt.Sprintf("content-%d", i))
	}
	s := setupStore(t)
	idx := indexVolume(t, s, root)
	v := testVolume(t, s)
	seedVector(t, s, v.ID, "t1", selfNode(t, s).ID, idx.RunID)
	opts := Options{Name: volName, Require: []string{"t1"}}

	opts.Paths = []string{"Photos/2019", "docs/d.txt"}
	rep, err := Offload(context.Background(), s, root, opts)
	if err != nil {
		t.Fatalf("Offload: %v", err)
	}
	if rep.Offloaded != 2 || len(rep.Results) != 2 {
		t.Fatalf("report = %+v, want exactly Photos/2019/a.jpg and docs/d.txt", rep)
	}
	oneResult(t, rep, "Photos/2019/a.jpg", OutcomeOffloaded)
	oneResult(t, rep, "docs/d.txt", OutcomeOffloaded)
	mustExist(t, filepath.Join(root, "Photos", "2019-extra", "b.jpg"))
	mustExist(t, filepath.Join(root, "Photos", "2020", "c.jpg"))
}

// TestOffloadOlderThanSelector: the age bound filters on the indexed
// mtime and ANDs with path selectors.
func TestOffloadOlderThanSelector(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-48 * time.Hour)
	writeFile(t, filepath.Join(root, "sub", "old.txt"), "old in sub")
	writeFile(t, filepath.Join(root, "sub", "new.txt"), "new in sub")
	writeFile(t, filepath.Join(root, "top-old.txt"), "old at top")
	for _, p := range []string{filepath.Join(root, "sub", "old.txt"), filepath.Join(root, "top-old.txt")} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}
	s := setupStore(t)
	idx := indexVolume(t, s, root)
	v := testVolume(t, s)
	seedVector(t, s, v.ID, "t1", selfNode(t, s).ID, idx.RunID)

	rep, err := Offload(context.Background(), s, root, Options{
		Name: volName, Paths: []string{"sub"}, OlderThan: 24 * time.Hour, Require: []string{"t1"},
	})
	if err != nil {
		t.Fatalf("Offload: %v", err)
	}
	if rep.Offloaded != 1 || len(rep.Results) != 1 {
		t.Fatalf("report = %+v, want exactly sub/old.txt", rep)
	}
	oneResult(t, rep, "sub/old.txt", OutcomeOffloaded)
	mustExist(t, filepath.Join(root, "sub", "new.txt"))
	mustExist(t, filepath.Join(root, "top-old.txt"))

	rep, err = Offload(context.Background(), s, root, Options{
		Name: volName, OlderThan: 24 * time.Hour, Require: []string{"t1"},
	})
	if err != nil {
		t.Fatalf("Offload age-only: %v", err)
	}
	if rep.Offloaded != 1 {
		t.Fatalf("age-only report = %+v, want exactly top-old.txt", rep)
	}
	oneResult(t, rep, "top-old.txt", OutcomeOffloaded)
	mustExist(t, filepath.Join(root, "sub", "new.txt"))

	if _, err := Offload(context.Background(), s, root, Options{
		Name: volName, Paths: []string{"sub"}, OlderThan: -time.Hour, Require: []string{"t1"},
	}); err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("negative --older-than err = %v, want refusal naming the negative duration", err)
	}
}

// TestOffloadSelectorMissReported: a selector matching nothing is
// surfaced instead of silently producing an empty run.
func TestOffloadSelectorMissReported(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	s := setupStore(t)
	indexVolume(t, s, root)

	rep, err := Offload(context.Background(), s, root, Options{
		Name: volName, Paths: []string{"nope"}, Require: []string{"t1"},
	})
	if err != nil {
		t.Fatalf("Offload: %v", err)
	}
	if len(rep.Results) != 0 {
		t.Fatalf("results = %+v, want none", rep.Results)
	}
	if len(rep.SelectorMisses) != 1 || rep.SelectorMisses[0] != "nope" {
		t.Fatalf("SelectorMisses = %v, want [nope]", rep.SelectorMisses)
	}
}

// TestOffloadDryRunTouchesNothing: dry-run reports the gate decision
// per file and leaves the disk, the rows, and the runs table exactly as
// they were.
func TestOffloadDryRunTouchesNothing(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	s := setupStore(t)
	first := indexVolume(t, s, root)
	v := testVolume(t, s)
	seedVector(t, s, v.ID, "t1", selfNode(t, s).ID, first.RunID)
	writeFile(t, filepath.Join(root, "b.txt"), "bravo")
	indexVolume(t, s, root)
	runsBefore := countRuns(t, s)

	rep, err := Offload(context.Background(), s, root, Options{
		Name: volName, Paths: []string{"."}, Require: []string{"t1"}, DryRun: true,
	})
	if err != nil {
		t.Fatalf("Offload dry-run: %v", err)
	}
	if rep.RunID != 0 {
		t.Fatalf("dry-run RunID = %d, want 0", rep.RunID)
	}
	oneResult(t, rep, "a.txt", OutcomeOffloaded)
	oneResult(t, rep, "b.txt", OutcomeNotDurable)
	mustExist(t, filepath.Join(root, "a.txt"))
	mustExist(t, filepath.Join(root, "b.txt"))
	for _, p := range []string{"a.txt", "b.txt"} {
		if row := rowAt(t, s, v.ID, p); row.Status != store.StatusPresent {
			t.Fatalf("%s status = %q after dry-run, want present", p, row.Status)
		}
	}
	if got := countRuns(t, s); got != runsBefore {
		t.Fatalf("runs count = %d, want %d (dry-run must record nothing)", got, runsBefore)
	}
}

// TestOffloadIndexerIntegration: a follow-up index run treats the
// offloaded row's on-disk absence as expected, and re-acquiring the
// bytes flips it back to present with first_seen preserved.
func TestOffloadIndexerIntegration(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	writeFile(t, filepath.Join(root, "keep.txt"), "kept")
	s := setupStore(t)
	ctx := context.Background()
	idx := indexVolume(t, s, root)
	v := testVolume(t, s)
	seedVector(t, s, v.ID, "t1", selfNode(t, s).ID, idx.RunID)

	rep, err := Offload(ctx, s, root, Options{
		Name: volName, Paths: []string{"a.txt"}, Require: []string{"t1"},
	})
	if err != nil || rep.Offloaded != 1 {
		t.Fatalf("Offload: %v, report %+v", err, rep)
	}

	after := indexVolume(t, s, root)
	if after.Missing != 0 {
		t.Fatalf("re-index Missing = %d, want 0 (offloaded absence is expected)", after.Missing)
	}
	if row := rowAt(t, s, v.ID, "a.txt"); row.Status != store.StatusOffloaded {
		t.Fatalf("status after re-index = %q, want offloaded", row.Status)
	}

	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	indexVolume(t, s, root)
	row := rowAt(t, s, v.ID, "a.txt")
	if row.Status != store.StatusPresent {
		t.Fatalf("status after re-acquire = %q, want present", row.Status)
	}
	if row.FirstSeenRunID != idx.RunID {
		t.Fatalf("first_seen_run_id = %d, want %d (re-acquire must not rewrite it)", row.FirstSeenRunID, idx.RunID)
	}
}

// TestOffloadMissingRowsNeverConsidered: a 'missing' row is no longer
// on disk to delete and must stay out of the candidate set entirely.
func TestOffloadMissingRowsNeverConsidered(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "gone.txt"), "gone")
	writeFile(t, filepath.Join(root, "here.txt"), "here")
	s := setupStore(t)
	indexVolume(t, s, root)
	if err := os.Remove(filepath.Join(root, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	second := indexVolume(t, s, root)
	v := testVolume(t, s)
	seedVector(t, s, v.ID, "t1", selfNode(t, s).ID, second.RunID)

	rep, err := Offload(context.Background(), s, root, Options{
		Name: volName, Paths: []string{"."}, Require: []string{"t1"},
	})
	if err != nil {
		t.Fatalf("Offload: %v", err)
	}
	if len(rep.Results) != 1 || rep.Offloaded != 1 {
		t.Fatalf("report = %+v, want exactly here.txt offloaded", rep)
	}
	oneResult(t, rep, "here.txt", OutcomeOffloaded)
	if row := rowAt(t, s, v.ID, "gone.txt"); row.Status != store.StatusMissing {
		t.Fatalf("gone.txt status = %q, want missing (untouched)", row.Status)
	}
}

// TestOffloadSupersededContentNeverGates: when a path's content
// changed since the watermark, the live row is gated on the *new*
// content's origin — the superseded predecessor's covered coordinate
// must not let the new bytes be deleted.
func TestOffloadSupersededContentNeverGates(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a.txt")
	writeFile(t, p, "version one")
	s := setupStore(t)
	first := indexVolume(t, s, root)
	v := testVolume(t, s)
	seedVector(t, s, v.ID, "t1", selfNode(t, s).ID, first.RunID)

	writeFile(t, p, "version two")
	indexVolume(t, s, root)

	rep, err := Offload(context.Background(), s, root, Options{
		Name: volName, Paths: []string{"a.txt"}, Require: []string{"t1"},
	})
	if err != nil {
		t.Fatalf("Offload: %v", err)
	}
	oneResult(t, rep, "a.txt", OutcomeNotDurable)
	mustExist(t, p)

	history, err := s.ListHistoryByPath(context.Background(), v.ID, "a.txt")
	if err != nil {
		t.Fatalf("ListHistoryByPath: %v", err)
	}
	if len(history) != 2 || history[0].Status != store.StatusSuperseded {
		t.Fatalf("history = %+v, want superseded v1 + present v2", history)
	}
}

// TestOffloadReservedSubtreesExcluded: squirrel-owned preservation
// directories are never offload candidates even when a selector covers
// them and the gate would pass.
func TestOffloadReservedSubtreesExcluded(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".squirrel-history", "run-1", "x.bin"), "preserved")
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	s := setupStore(t)
	idx := indexVolume(t, s, root)
	v := testVolume(t, s)
	seedVector(t, s, v.ID, "t1", selfNode(t, s).ID, idx.RunID)

	rep, err := Offload(context.Background(), s, root, Options{
		Name: volName, Paths: []string{"."}, Require: []string{"t1"},
	})
	if err != nil {
		t.Fatalf("Offload: %v", err)
	}
	if len(rep.Results) != 1 || rep.Offloaded != 1 {
		t.Fatalf("report = %+v, want exactly a.txt", rep)
	}
	oneResult(t, rep, "a.txt", OutcomeOffloaded)
	mustExist(t, filepath.Join(root, ".squirrel-history", "run-1", "x.bin"))
}

// TestOffloadRefusedWhileRunInFlight: offload defers to any running run
// on the volume.
func TestOffloadRefusedWhileRunInFlight(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	s := setupStore(t)
	ctx := context.Background()
	idx := indexVolume(t, s, root)
	v := testVolume(t, s)
	seedVector(t, s, v.ID, "t1", selfNode(t, s).ID, idx.RunID)
	if _, err := s.BeginIndexRun(ctx, store.RunKindIndex, v.ID, false); err != nil {
		t.Fatalf("BeginIndexRun: %v", err)
	}

	_, err := Offload(ctx, s, root, Options{
		Name: volName, Paths: []string{"."}, Require: []string{"t1"},
	})
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("err = %v, want already-running refusal", err)
	}
	mustExist(t, filepath.Join(root, "a.txt"))
}

// TestOffloadIdentityMismatchRefused: a DB/config path disagreement or
// a volume marker naming another volume aborts before any deletion.
func TestOffloadIdentityMismatchRefused(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	s := setupStore(t)
	indexVolume(t, s, root)

	_, err := Offload(context.Background(), s, t.TempDir(), Options{
		Name: volName, Paths: []string{"."}, Require: []string{"t1"},
	})
	if err == nil || !strings.Contains(err.Error(), "resolve the conflict") {
		t.Fatalf("err = %v, want DB/config path mismatch refusal", err)
	}

	if err := volmark.Write(root, volmark.Marker{Volume: "other"}); err != nil {
		t.Fatalf("tamper marker: %v", err)
	}
	_, err = Offload(context.Background(), s, root, Options{
		Name: volName, Paths: []string{"."}, Require: []string{"t1"},
	})
	if err == nil || !strings.Contains(err.Error(), "volume marker") {
		t.Fatalf("err = %v, want marker refusal", err)
	}
	mustExist(t, filepath.Join(root, "a.txt"))
}

// TestOffloadUnindexedVolumeRefused: a volume with no index rows has no
// evidence to gate on.
func TestOffloadUnindexedVolumeRefused(t *testing.T) {
	s := setupStore(t)
	_, err := Offload(context.Background(), s, t.TempDir(), Options{
		Name: volName, Paths: []string{"."}, Require: []string{"t1"},
	})
	if err == nil || !strings.Contains(err.Error(), "no index rows") {
		t.Fatalf("err = %v, want unindexed refusal", err)
	}
}

func TestCleanSelectors(t *testing.T) {
	got, err := cleanSelectors([]string{"a/b/", "./c", ".", "./"})
	if err != nil {
		t.Fatalf("cleanSelectors: %v", err)
	}
	if len(got) != 4 || got[0] != "a/b" || got[1] != "c" || got[2] != "." || got[3] != "." {
		t.Fatalf("cleaned = %v, want [a/b c . .]", got)
	}
	for _, bad := range []string{"", "/abs/path", "../escape", "a/../../b", "a/..", "b/c/../.."} {
		if _, err := cleanSelectors([]string{bad}); err == nil {
			t.Fatalf("cleanSelectors(%q) succeeded, want refusal", bad)
		}
	}
}
