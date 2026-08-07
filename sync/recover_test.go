package sync

import (
	"testing"
	"time"
)

// TestParseSnapshotName reads the identity back out of the filenames
// ensureLocalSnapshot writes. The name is the only metadata every rclone
// backend agrees on, so this parse is what a recovery's "how old is this
// catalog" answer rests on.
func TestParseSnapshotName(t *testing.T) {
	want := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	got := parseSnapshotName("photos", "index-20260807T120000.000Z-run-42.db")
	if !got.TakenAt.Equal(want) {
		t.Errorf("TakenAt = %v, want %v", got.TakenAt, want)
	}
	if got.RunID != 42 {
		t.Errorf("RunID = %d, want 42", got.RunID)
	}
	if got.Volume != "photos" || got.Name != "index-20260807T120000.000Z-run-42.db" {
		t.Errorf("identity = %+v, want the volume and name carried through", got)
	}
}

// TestParseSnapshotNameSurvivesOddNames: a name that does not follow the
// convention still lists, with a zero time and run. Refusing to show an
// operator a recovery candidate over an unparsed filename would be the
// wrong trade in the one flow where they have least to work with — but the
// zero must be visible as "unknown", never rendered as fresh.
func TestParseSnapshotNameSurvivesOddNames(t *testing.T) {
	for _, name := range []string{
		"index-hand-copied.db",
		"index-.db",
		"index-notatimestamp-run-7.db",
		"index-20260807T120000.000Z-run-notanumber.db",
	} {
		got := parseSnapshotName("photos", name)
		if got.Name != name {
			t.Errorf("name %q was not carried through: %+v", name, got)
		}
		if _, ok := got.Age(time.Now()); ok && got.TakenAt.IsZero() {
			t.Errorf("%q reports a knowable age from a zero time", name)
		}
	}
}

// TestParseSnapshotNamePartialParse: a good timestamp with a bad run id
// keeps the timestamp. The age is the field a recovery decision turns on,
// so it must not be lost to an unrelated malformed half.
func TestParseSnapshotNamePartialParse(t *testing.T) {
	got := parseSnapshotName("photos", "index-20260807T120000.000Z-run-xx.db")
	if got.TakenAt.IsZero() {
		t.Fatal("a parseable timestamp was discarded because the run id was not")
	}
	if got.RunID != 0 {
		t.Errorf("RunID = %d, want 0 for an unparseable id", got.RunID)
	}
}

// TestSortSnapshotsNewestFirst pins the order the whole flow depends on:
// chooseSnapshot takes the head as "the newest", so a wrong order here
// silently installs the wrong catalog.
func TestSortSnapshotsNewestFirst(t *testing.T) {
	mk := func(vol, name string, day int) IndexSnapshot {
		var ts time.Time
		if day > 0 {
			ts = time.Date(2026, 8, day, 0, 0, 0, 0, time.UTC)
		}
		return IndexSnapshot{Volume: vol, Name: name, TakenAt: ts}
	}
	snaps := []IndexSnapshot{
		mk("photos", "old.db", 1),
		mk("photos", "unparsed.db", 0),
		mk("docs", "new.db", 9),
		mk("photos", "mid.db", 5),
	}
	sortSnapshots(snaps)

	gotOrder := make([]string, len(snaps))
	for i, s := range snaps {
		gotOrder[i] = s.Name
	}
	want := []string{"new.db", "mid.db", "old.db", "unparsed.db"}
	for i := range want {
		if gotOrder[i] != want[i] {
			t.Fatalf("order = %v, want %v (newest first, unparsed last)", gotOrder, want)
		}
	}
}

// TestSortSnapshotsIsTotal: two snapshots sharing a timestamp must still
// order deterministically, or a listing reshuffles between runs of an
// unchanged destination.
func TestSortSnapshotsIsTotal(t *testing.T) {
	ts := time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)
	first := []IndexSnapshot{
		{Volume: "photos", Name: "b.db", TakenAt: ts},
		{Volume: "docs", Name: "a.db", TakenAt: ts},
		{Volume: "photos", Name: "a.db", TakenAt: ts},
	}
	second := []IndexSnapshot{first[2], first[0], first[1]}
	sortSnapshots(first)
	sortSnapshots(second)
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("order depends on input order: %+v vs %+v", first, second)
		}
	}
	if first[0].Volume != "docs" {
		t.Errorf("tie broken by %q, want volume order to decide", first[0].Volume)
	}
}

// TestIndexSnapshotAge reports an age only when there is one to report.
func TestIndexSnapshotAge(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	snap := IndexSnapshot{TakenAt: now.Add(-3 * time.Hour)}
	age, ok := snap.Age(now)
	if !ok || age != 3*time.Hour {
		t.Errorf("Age = %v, %v; want 3h, true", age, ok)
	}
	if _, ok := (IndexSnapshot{}).Age(now); ok {
		t.Error("a zero TakenAt reported a knowable age")
	}
}
