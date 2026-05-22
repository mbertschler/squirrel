package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/mbertschler/squirrel/config"
)

// TestNewRootModelInitProducesCmd exercises the construction path end to
// end and checks Init produces a non-nil command batch. It's a smoke test
// — no terminal is attached so we can't drive Update interactively — but
// it catches "Run nil-dereferences on boot" regressions cheaply.
func TestNewRootModelInitProducesCmd(t *testing.T) {
	s := openTestStore(t)
	root := newRootModel(s, nil)
	if root == nil {
		t.Fatal("newRootModel returned nil")
	}
	if cmd := root.Init(); cmd == nil {
		t.Fatal("rootModel.Init returned nil cmd — at minimum the tick should be armed")
	}
}

func TestNewRootModelWithAgentConfig(t *testing.T) {
	// Sanity check that an [agent] block doesn't cause the constructor to
	// fall over — exercises newAgentClient's TLS-detection branch and the
	// dashboard.attachClient call site.
	s := openTestStore(t)
	cfg := &config.Config{
		Agent: &config.Agent{Listen: "127.0.0.1:12345", Token: "secret"},
	}
	root := newRootModel(s, cfg)
	if !root.dashboard.client.configured() {
		t.Errorf("agent client should report configured when an [agent] block is present")
	}
}

// Compile-time guard that the screen models implement tea.Model. If a
// future refactor changes the signature on one of them, the build fails
// here instead of at runtime via a panic from the root delegate.
var (
	_ tea.Model = (*rootModel)(nil)
	_ tea.Model = (*dashboardModel)(nil)
	_ tea.Model = (*runsModel)(nil)
	_ tea.Model = (*volumesModel)(nil)
	_ tea.Model = (*browseModel)(nil)
)
