package main

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/sync"
)

// TestVerifyRunStatus maps VerifyRemote outcomes to the status the scheduler
// logs, including the empty-string "nothing recorded" signal.
func TestVerifyRunStatus(t *testing.T) {
	cases := []struct {
		name string
		rep  sync.RemoteVerifyReport
		err  error
		want string
	}{
		{"error", sync.RemoteVerifyReport{RunID: 1}, errors.New("boom"), store.RunStatusFailed},
		{"no-op", sync.RemoteVerifyReport{RunID: 0}, nil, ""},
		{"clean", sync.RemoteVerifyReport{RunID: 2, Objects: 3, Verified: 3}, nil, store.RunStatusSuccess},
		{"mismatch", sync.RemoteVerifyReport{RunID: 3, Objects: 3, Missing: []string{"abcd"}}, nil, store.RunStatusPartial},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := verifyRunStatus(c.rep, c.err); got != c.want {
				t.Fatalf("verifyRunStatus = %q, want %q", got, c.want)
			}
		})
	}
}

// TestDurabilityPullStatus: a clean pull is success, a refused rewind is
// partial (surfaced, never applied), a transport error is failed.
func TestDurabilityPullStatus(t *testing.T) {
	if got, _ := durabilityPullStatus(sync.DurabilityPullReport{Applied: 2}, nil); got != store.RunStatusSuccess {
		t.Fatalf("clean pull status = %q, want success", got)
	}
	if got, msg := durabilityPullStatus(sync.DurabilityPullReport{Rewinds: []sync.DurabilityRewind{{}}}, nil); got != store.RunStatusPartial || msg == "" {
		t.Fatalf("rewind pull = (%q,%q), want partial with a message", got, msg)
	}
	if got, msg := durabilityPullStatus(sync.DurabilityPullReport{}, errors.New("timeout")); got != store.RunStatusFailed || msg != "timeout" {
		t.Fatalf("errored pull = (%q,%q), want (failed,timeout)", got, msg)
	}
}

// TestAnyDestinationNeedsScheduledVerify covers the wiring predicate: a
// per-destination cadence or an [agent] default on a verifiable layout
// trips it; a mirror layout never does.
func TestAnyDestinationNeedsScheduledVerify(t *testing.T) {
	base := func() *config.Config {
		return &config.Config{
			Agent:        &config.Agent{},
			Destinations: map[string]*config.Destination{},
		}
	}

	cfg := base()
	cfg.Destinations["m"] = &config.Destination{Layout: config.LayoutMirror, VerifyEvery: time.Hour}
	if anyDestinationNeedsScheduledVerify(cfg) {
		t.Fatalf("mirror layout must never need scheduled verify")
	}

	cfg = base()
	cfg.Destinations["p"] = &config.Destination{Layout: config.LayoutPacked, VerifyEvery: time.Hour}
	if !anyDestinationNeedsScheduledVerify(cfg) {
		t.Fatalf("packed destination with own cadence should need verify")
	}

	cfg = base()
	cfg.Agent.VerifyEvery = time.Hour
	cfg.Destinations["c"] = &config.Destination{Layout: config.LayoutContentAddressed}
	if !anyDestinationNeedsScheduledVerify(cfg) {
		t.Fatalf("agent default should cover a verifiable destination with no own cadence")
	}
}

// TestAnyNodeNeedsScheduledPull: only a node with pull_durability_every trips.
func TestAnyNodeNeedsScheduledPull(t *testing.T) {
	cfg := &config.Config{Nodes: map[string]*config.Node{
		"a": {},
	}}
	if anyNodeNeedsScheduledPull(cfg) {
		t.Fatalf("no node has a pull cadence")
	}
	cfg.Nodes["b"] = &config.Node{PullDurabilityEvery: time.Hour}
	if !anyNodeNeedsScheduledPull(cfg) {
		t.Fatalf("node b has a pull cadence")
	}
}

// TestSchedulerToolsRebuildKeepsRclone: a reload to a config that needs no
// rclone must not take the wrapper away from a sync the scheduler decided to
// kick a moment earlier, under the config that did. Losing the race would
// turn a cadence the operator merely removed into a failed run.
func TestSchedulerToolsRebuildKeepsRclone(t *testing.T) {
	tools := &schedulerTools{out: io.Discard}
	located := &sync.Rclone{}
	tools.rcl.Store(located)

	if err := tools.rebuild(context.Background(), &config.Config{}); err != nil {
		t.Fatalf("rebuild against an rclone-less config: %v", err)
	}
	if got := tools.rclone(); got != located {
		t.Fatalf("rclone() = %p after a reload that needs none, want the located wrapper %p", got, located)
	}
}
