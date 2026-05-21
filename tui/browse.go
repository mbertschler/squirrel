package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/mbertschler/squirrel/store"
)

// browseModel is the ncdu-style folder traversal for a single volume. The
// first commit only stubs the structure.
type browseModel struct {
	store         *store.Store
	width, height int

	volumeID   int64
	volumeName string
}

func newBrowseModel(s *store.Store) *browseModel {
	return &browseModel{store: s}
}

func (m *browseModel) setVolume(id int64, name string) {
	m.volumeID = id
	m.volumeName = name
}

func (m *browseModel) Init() tea.Cmd { return nil }

func (m *browseModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if sz, ok := msg.(tea.WindowSizeMsg); ok {
		m.width, m.height = sz.Width, sz.Height
	}
	return m, nil
}

func (m *browseModel) View() string {
	return styleMuted.Render("browse — coming in a later commit")
}
