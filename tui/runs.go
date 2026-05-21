package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/mbertschler/squirrel/store"
)

// runsModel will list the runs table with drill-in detail. The first commit
// only stubs the structure.
type runsModel struct {
	store         *store.Store
	width, height int
}

func newRunsModel(s *store.Store) *runsModel {
	return &runsModel{store: s}
}

func (m *runsModel) Init() tea.Cmd { return nil }

func (m *runsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if sz, ok := msg.(tea.WindowSizeMsg); ok {
		m.width, m.height = sz.Width, sz.Height
	}
	return m, nil
}

func (m *runsModel) View() string {
	return styleMuted.Render("runs — coming in the next commit")
}
