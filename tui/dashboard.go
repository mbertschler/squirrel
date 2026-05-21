package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/mbertschler/squirrel/store"
)

// dashboardModel will surface a one-screen summary of squirrel's live state:
// in-flight runs, recent terminal runs, per-volume health. The first commit
// only stubs the structure; the dashboard logic lands in a follow-up
// commit in this PR.
type dashboardModel struct {
	store *store.Store

	width, height int
}

func newDashboardModel(s *store.Store) *dashboardModel {
	return &dashboardModel{store: s}
}

func (m *dashboardModel) Init() tea.Cmd { return nil }

func (m *dashboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if sz, ok := msg.(tea.WindowSizeMsg); ok {
		m.width, m.height = sz.Width, sz.Height
	}
	return m, nil
}

func (m *dashboardModel) View() string {
	return styleMuted.Render("dashboard — coming in the next commit")
}
