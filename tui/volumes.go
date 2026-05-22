package tui

import (
	"context"
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/mbertschler/squirrel/store"
)

// volumesModel is the multi-volume root: it lists every configured volume
// with a per-volume summary (path, last-index, last-sync, files at last
// index) and lets the user descend into one with enter to open the Browse
// screen. The same SQL fetch as the dashboard backs it — one ListVolumes
// + one ListRuns pass — because the relevant data is identical.
type volumesModel struct {
	store *store.Store

	width, height int

	table table.Model

	volumes     []store.Volume
	latestByVol map[int64]map[string]store.Run
	loaded      bool
	loadErr     error
}

// browseEnterVolumeMsg is emitted by the Volumes screen when the user
// selects a volume to browse. The root model handles it by switching the
// active screen and seeding the Browse model.
type browseEnterVolumeMsg struct {
	volumeID   int64
	volumeName string
}

type volumesDataMsg struct {
	volumes     []store.Volume
	latestByVol map[int64]map[string]store.Run
	err         error
}

func newVolumesModel(s *store.Store) *volumesModel {
	t := table.New(table.WithFocused(true), table.WithHeight(15))
	style := table.DefaultStyles()
	style.Header = style.Header.
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(colourMuted).
		BorderBottom(true).
		Bold(true)
	style.Selected = style.Selected.
		Foreground(lipgloss.Color("231")).
		Background(colourAccent).
		Bold(false)
	t.SetStyles(style)
	return &volumesModel{store: s, table: t}
}

func (m *volumesModel) Init() tea.Cmd { return m.fetch() }

func (m *volumesModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		h := msg.Height - 7
		if h < 5 {
			h = 5
		}
		m.table.SetHeight(h)
		m.resizeColumns()
		return m, nil
	case tickMsg:
		return m, m.fetch()
	case volumesDataMsg:
		m.loaded = true
		m.loadErr = msg.err
		if msg.err == nil {
			m.volumes = msg.volumes
			m.latestByVol = msg.latestByVol
			m.refreshRows()
		}
		return m, nil
	case tea.KeyMsg:
		if msg.String() == "enter" && len(m.volumes) > 0 {
			idx := m.table.Cursor()
			if idx >= 0 && idx < len(m.volumes) {
				v := m.volumes[idx]
				return m, func() tea.Msg {
					return browseEnterVolumeMsg{volumeID: v.ID, volumeName: v.Name}
				}
			}
		}
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m *volumesModel) View() string {
	if !m.loaded && m.loadErr == nil {
		return styleMuted.Render("loading volumes…")
	}
	if m.loadErr != nil {
		return styleErr.Render(fmt.Sprintf("volumes error: %v", m.loadErr))
	}
	if len(m.volumes) == 0 {
		return styleHeader.Render("Volumes") + "\n" +
			styleMuted.Render("no volumes configured — add one in your config.toml")
	}
	header := styleHeader.Render(fmt.Sprintf("Volumes (%d)", len(m.volumes)))
	footer := styleMuted.Render("enter — browse selected volume")
	return header + "\n" + m.table.View() + "\n" + footer
}

func (m *volumesModel) refreshRows() {
	now := time.Now()
	rows := make([]table.Row, 0, len(m.volumes))
	for _, v := range m.volumes {
		rows = append(rows, table.Row{
			v.Name,
			v.Path,
			m.formatLast(v.ID, store.RunKindIndex, now),
			m.formatLast(v.ID, store.RunKindSync, now),
			m.filesAtLastIndex(v.ID),
		})
	}
	m.table.SetRows(rows)
	m.resizeColumns()
}

func (m *volumesModel) formatLast(volID int64, kind string, now time.Time) string {
	byKind := m.latestByVol[volID]
	if byKind == nil {
		return styleMuted.Render("—")
	}
	r, ok := byKind[kind]
	if !ok {
		return styleMuted.Render("—")
	}
	return whenAgo(r.EndedAtNs, now) + " " +
		lipgloss.NewStyle().Foreground(statusColour(r.Status)).Render(glyphForStatus(r.Status))
}

func (m *volumesModel) filesAtLastIndex(volID int64) string {
	byKind := m.latestByVol[volID]
	if byKind == nil {
		return styleMuted.Render("—")
	}
	r, ok := byKind[store.RunKindIndex]
	if !ok {
		return styleMuted.Render("—")
	}
	return fmt.Sprintf("%d", r.FileCount)
}

func (m *volumesModel) resizeColumns() {
	cols := []table.Column{
		{Title: "NAME", Width: 16},
		{Title: "PATH", Width: 32},
		{Title: "LAST INDEX", Width: 16},
		{Title: "LAST SYNC", Width: 16},
		{Title: "FILES", Width: 10},
	}
	if m.width > 0 {
		fixed := 0
		for _, c := range cols {
			fixed += c.Width
		}
		fixed += 2 * (len(cols) - 1)
		surplus := m.width - fixed - 4
		if surplus > 0 {
			cols[1].Width += surplus
		}
	}
	m.table.SetColumns(cols)
}

func (m *volumesModel) fetch() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		vols, err := m.store.ListVolumes(ctx)
		if err != nil {
			return volumesDataMsg{err: err}
		}
		// Direct per-(volume, kind) query rather than a bounded ListRuns
		// scan — see store.LatestSuccessfulRunsByVolumeAndKind for the
		// reasoning. Volumes whose last index sits outside the recent
		// window still get an accurate "last index" cell.
		latestByVol, err := m.store.LatestSuccessfulRunsByVolumeAndKind(ctx)
		if err != nil {
			return volumesDataMsg{err: err}
		}
		return volumesDataMsg{volumes: vols, latestByVol: latestByVol}
	}
}
