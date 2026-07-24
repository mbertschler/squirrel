package status

import (
	"database/sql"
	"testing"
	"time"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

func TestCadenceLevel(t *testing.T) {
	h := time.Hour
	cases := []struct {
		age     time.Duration
		cadence time.Duration
		want    Level
	}{
		{30 * time.Minute, h, LevelOK},                 // within cadence
		{h, h, LevelOK},                                // exactly at cadence
		{2 * h, h, LevelWarn},                          // late, within lateFactor
		{lateFactor * h, h, LevelWarn},                 // at the boundary
		{lateFactor*h + time.Minute, h, LevelCritical}, // beyond lateFactor
		{1000 * h, 0, LevelOK},                         // no cadence: any age is fine
	}
	for _, c := range cases {
		if got := cadenceLevel(c.age, c.cadence); got != c.want {
			t.Errorf("cadenceLevel(%s, %s) = %v, want %v", c.age, c.cadence, got, c.want)
		}
	}
}

func refusedRun(errMsg string) *store.Run {
	return &store.Run{Status: store.RunStatusRefused, Error: sql.NullString{String: errMsg, Valid: errMsg != ""}}
}

func TestClassifyStanding(t *testing.T) {
	bootstrapErr := "destination has no marker — re-run with --init to bootstrap"
	cases := []struct {
		name       string
		lastTerm   *store.Run
		hadSuccess bool
		want       Standing
	}{
		{"no runs", nil, false, StandingNone},
		{"fresh bootstrap refusal", refusedRun(bootstrapErr), false, StandingNeedsBootstrap},
		{"refusal after prior success", refusedRun(bootstrapErr), true, StandingRefused},
		{"non-bootstrap refusal", refusedRun("layout guard: history is not packed"), false, StandingRefused},
		{"success", &store.Run{Status: store.RunStatusSuccess}, true, StandingNone},
	}
	for _, c := range cases {
		if got := classifyStanding(c.lastTerm, c.hadSuccess); got != c.want {
			t.Errorf("%s: classifyStanding = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDurabilityLevel(t *testing.T) {
	dur := func(local, covered, stale bool) *Durability {
		return &Durability{LocalContent: local, Covered: covered, Stale: stale}
	}
	cases := []struct {
		name     string
		d        *Durability
		required bool
		want     Level
	}{
		{"no local content", dur(false, false, false), true, LevelNeutral},
		{"required covered fresh", dur(true, true, false), true, LevelOK},
		{"required uncovered", dur(true, false, false), true, LevelWarn},
		{"required covered stale", dur(true, true, true), true, LevelWarn},
		{"non-required covered", dur(true, true, false), false, LevelOK},
		{"non-required uncovered", dur(true, false, false), false, LevelNeutral},
	}
	for _, c := range cases {
		if got := durabilityLevel(c.d, c.required); got != c.want {
			t.Errorf("%s: durabilityLevel = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestEvidenceStale(t *testing.T) {
	age := func(d time.Duration) *time.Duration { return &d }
	cases := []struct {
		name string
		d    *Durability
		want bool
	}{
		{"no policy", &Durability{MaxEvidenceAge: 0, EvidenceAge: age(time.Hour)}, false},
		{"unknown age fail-closed", &Durability{MaxEvidenceAge: time.Hour, EvidenceAge: nil}, true},
		{"fresh", &Durability{MaxEvidenceAge: time.Hour, EvidenceAge: age(30 * time.Minute)}, false},
		{"aged out", &Durability{MaxEvidenceAge: time.Hour, EvidenceAge: age(2 * time.Hour)}, true},
	}
	for _, c := range cases {
		if got := evidenceStale(c.d); got != c.want {
			t.Errorf("%s: evidenceStale = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestLevelExitCode(t *testing.T) {
	cases := map[Level]int{LevelNeutral: 0, LevelOK: 0, LevelWarn: 1, LevelCritical: 2}
	for l, want := range cases {
		if got := l.ExitCode(); got != want {
			t.Errorf("%v.ExitCode() = %d, want %d", l, got, want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:                      "0 B",
		512:                    "512 B",
		1024:                   "1.0 KiB",
		1536:                   "1.5 KiB",
		1024 * 1024:            "1.0 MiB",
		3 * 1024 * 1024 * 1024: "3.0 GiB",
	}
	for n, want := range cases {
		if got := HumanBytes(n); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestOrderedTargets(t *testing.T) {
	// sync_to first in declared order, then offload_requires entries not
	// already listed, deduplicated ("nas" appears in both).
	vol := &config.Volume{SyncTo: []string{"nas"}, OffloadRequires: []string{"nas", "s3archive"}}
	got := orderedTargets(vol)
	want := []string{"nas", "s3archive"}
	if len(got) != len(want) {
		t.Fatalf("orderedTargets = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("orderedTargets = %v, want %v", got, want)
		}
	}
}

func TestStateLabel(t *testing.T) {
	age := func(d time.Duration) *time.Duration { return &d }
	cases := []struct {
		name string
		t    TargetStatus
		want string
	}{
		{"alarm", TargetStatus{Standing: StandingAlarm}, "alarm"},
		{"needs-init", TargetStatus{Standing: StandingNeedsBootstrap}, "needs-init"},
		{"failed", TargetStatus{LastOutcome: "failed", SyncTarget: true}, "failed"},
		{"relayed", TargetStatus{SyncTarget: false, Required: true}, "—"},
		{"never", TargetStatus{SyncTarget: true}, "never-synced"},
		{"ok", TargetStatus{SyncTarget: true, LastSyncAgo: age(time.Minute), SyncLevel: LevelOK}, "ok"},
		{"late", TargetStatus{SyncTarget: true, LastSyncAgo: age(time.Hour), SyncLevel: LevelWarn}, "late"},
	}
	for _, c := range cases {
		if got := StateLabel(c.t); got != c.want {
			t.Errorf("%s: StateLabel = %q, want %q", c.name, got, c.want)
		}
	}
}
