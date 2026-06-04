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

// hooksModel lists recent external-tool hook runs (#84): the generic
// outcome squirrel records each time it nudged a per-volume command on a
// change or interval trigger. It is a read-only table mirroring the
// Volumes screen's shape — squirrel records pass/fail and an exit code
// only, never what the command did, so there is nothing to drill into.
type hooksModel struct {
	store *store.Store

	width, height int

	table table.Model

	rows        []store.HookRun
	volumesByID map[int64]store.Volume
	loaded      bool
	loadErr     error
}

type hooksDataMsg struct {
	hooks   []store.HookRun
	volumes []store.Volume
	err     error
}

func newHooksModel(s *store.Store) *hooksModel {
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
	return &hooksModel{store: s, table: t}
}

func (m *hooksModel) Init() tea.Cmd { return m.fetch() }

func (m *hooksModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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
	case hooksDataMsg:
		m.loaded = true
		m.loadErr = msg.err
		if msg.err == nil {
			m.rows = msg.hooks
			m.volumesByID = make(map[int64]store.Volume, len(msg.volumes))
			for _, v := range msg.volumes {
				m.volumesByID[v.ID] = v
			}
			m.refreshRows()
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m *hooksModel) View() string {
	if !m.loaded && m.loadErr == nil {
		return styleMuted.Render("loading hooks…")
	}
	if m.loadErr != nil {
		return styleErr.Render(fmt.Sprintf("hooks error: %v", m.loadErr))
	}
	if len(m.rows) == 0 {
		return styleHeader.Render("Hooks") + "\n" +
			styleMuted.Render("no hook runs yet — configure [volumes.<name>.hook] and run the agent")
	}
	header := styleHeader.Render(fmt.Sprintf("Hooks (%d)", len(m.rows)))
	return header + "\n" + m.table.View()
}

func (m *hooksModel) refreshRows() {
	now := time.Now()
	rows := make([]table.Row, 0, len(m.rows))
	for _, r := range m.rows {
		rows = append(rows, table.Row{
			m.volumeName(r.VolumeID),
			r.Trigger,
			whenAgo(r.EndedAtNs, now),
			hookRunDuration(r),
			lipgloss.NewStyle().Foreground(statusColour(r.Status)).Render(glyphForStatus(r.Status) + " " + r.Status),
			hookExitCode(r),
			hookChanged(r),
		})
	}
	m.table.SetRows(rows)
	m.resizeColumns()
}

func (m *hooksModel) resizeColumns() {
	cols := []table.Column{
		{Title: "VOLUME", Width: 16},
		{Title: "TRIGGER", Width: 10},
		{Title: "WHEN", Width: 12},
		{Title: "DURATION", Width: 10},
		{Title: "STATUS", Width: 11},
		{Title: "EXIT", Width: 6},
		{Title: "CHANGED", Width: 8},
	}
	if m.width > 0 {
		fixed := 0
		for _, c := range cols {
			fixed += c.Width
		}
		fixed += 2 * (len(cols) - 1)
		surplus := m.width - fixed - 4
		if surplus > 0 {
			cols[0].Width += surplus
		}
	}
	m.table.SetColumns(cols)
}

func (m *hooksModel) volumeName(id int64) string {
	if v, ok := m.volumesByID[id]; ok {
		return v.Name
	}
	return fmt.Sprintf("vol#%d", id)
}

func (m *hooksModel) fetch() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		hooks, err := m.store.ListHookRuns(ctx, store.HookRunListOpts{Limit: 500, Descending: true})
		if err != nil {
			return hooksDataMsg{err: err}
		}
		vols, err := m.store.ListVolumes(ctx)
		if err != nil {
			return hooksDataMsg{err: err}
		}
		return hooksDataMsg{hooks: hooks, volumes: vols}
	}
}

// hookRunDuration renders a hook run's elapsed time, "—" while still
// running.
func hookRunDuration(r store.HookRun) string {
	if !r.EndedAtNs.Valid {
		return styleMuted.Render("—")
	}
	return humanDuration(time.Duration(r.EndedAtNs.Int64 - r.StartedAtNs))
}

// hookExitCode renders the recorded exit code, "—" when none was produced
// (a timeout or spawn failure leaves it NULL).
func hookExitCode(r store.HookRun) string {
	if !r.ExitCode.Valid {
		return styleMuted.Render("—")
	}
	return fmt.Sprintf("%d", r.ExitCode.Int64)
}

// hookChanged renders the SQUIRREL_CHANGED value the hook was passed, so a
// "fired but nothing moved" no-op is distinguishable at a glance.
func hookChanged(r store.HookRun) string {
	if r.Changed {
		return "yes"
	}
	return styleMuted.Render("no")
}
