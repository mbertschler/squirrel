package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/mbertschler/squirrel/store"
)

// volumesModel is the multi-volume root: it lists every configured volume
// and lets the user descend into one to open the Browse screen for it. The
// first commit only stubs the structure.
type volumesModel struct {
	store         *store.Store
	width, height int
}

// browseEnterVolumeMsg is emitted by the Volumes screen when the user
// selects a volume to browse. The root model handles it by switching the
// active screen and seeding the Browse model.
type browseEnterVolumeMsg struct {
	volumeID   int64
	volumeName string
}

func newVolumesModel(s *store.Store) *volumesModel {
	return &volumesModel{store: s}
}

func (m *volumesModel) Init() tea.Cmd { return nil }

func (m *volumesModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if sz, ok := msg.(tea.WindowSizeMsg); ok {
		m.width, m.height = sz.Width, sz.Height
	}
	return m, nil
}

func (m *volumesModel) View() string {
	return styleMuted.Render("volumes — coming in a later commit")
}
