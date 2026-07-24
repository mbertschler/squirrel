package tui

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/mbertschler/squirrel/store"
)

// runsModel is a paged table over the runs table with a press-enter detail
// view. Kind filtering is exposed as keystrokes (i = index only, s = sync,
// a = audit, r = restore, A = all) so the screen stays usable on a small
// terminal without a separate filter widget.
type runsModel struct {
	store *store.Store

	width, height int

	table table.Model

	rows           []store.Run
	volumesByID    map[int64]store.Volume
	filterKind     string // empty = no filter
	fold           bool   // collapse consecutive no-op rows (F19)
	foldedCount    int    // no-op rows hidden by the current fold
	loaded         bool
	loadErr        error
	selectedDetail *store.Run
	// displayRuns aligns 1:1 with the table's rows: the run behind each
	// row, or nil for a fold-marker row (which has no detail view).
	displayRuns []*store.Run
}

type runsDataMsg struct {
	runs    []store.Run
	volumes []store.Volume
	err     error
}

func newRunsModel(s *store.Store) *runsModel {
	t := table.New(
		table.WithFocused(true),
		table.WithHeight(15),
	)
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
	return &runsModel{store: s, table: t, fold: true}
}

func (m *runsModel) Init() tea.Cmd { return m.fetch() }

func (m *runsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		// Leave room for header, filter line, footer.
		h := msg.Height - 8
		if h < 5 {
			h = 5
		}
		m.table.SetHeight(h)
		m.resizeColumns()
		return m, nil
	case tickMsg:
		// Only refetch when the detail view is closed — keep the detail
		// snapshot stable while the user is reading it.
		if m.selectedDetail == nil {
			return m, m.fetch()
		}
		return m, nil
	case runsDataMsg:
		m.loaded = true
		m.loadErr = msg.err
		if msg.err == nil {
			m.rows = msg.runs
			m.volumesByID = make(map[int64]store.Volume, len(msg.volumes))
			for _, v := range msg.volumes {
				m.volumesByID[v.ID] = v
			}
			m.applyFilter()
		}
		return m, nil
	case tea.KeyMsg:
		if m.selectedDetail != nil {
			switch msg.String() {
			case "esc", "backspace", "enter":
				m.selectedDetail = nil
				return m, nil
			}
			return m, nil
		}
		switch msg.String() {
		case "enter":
			// A fold-marker row maps to nil in displayRuns and has no
			// detail view; ignore enter on it.
			idx := m.table.Cursor()
			if idx >= 0 && idx < len(m.displayRuns) && m.displayRuns[idx] != nil {
				r := *m.displayRuns[idx]
				m.selectedDetail = &r
			}
			return m, nil
		case "i":
			m.setFilter(store.RunKindIndex)
			return m, nil
		case "s":
			m.setFilter(store.RunKindSync)
			return m, nil
		case "a":
			m.setFilter(store.RunKindAudit)
			return m, nil
		case "r":
			m.setFilter(store.RunKindRestore)
			return m, nil
		case "A":
			m.setFilter("")
			return m, nil
		case "f":
			m.fold = !m.fold
			m.applyFilter()
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m *runsModel) View() string {
	if !m.loaded && m.loadErr == nil {
		return styleMuted.Render("loading runs…")
	}
	if m.loadErr != nil {
		return styleErr.Render(fmt.Sprintf("runs error: %v", m.loadErr))
	}
	if m.selectedDetail != nil {
		return m.renderDetail(*m.selectedDetail)
	}
	header := styleHeader.Render("Runs") + "  " + m.renderFilterChips() + m.renderFoldChip()
	return header + "\n" + m.table.View() + "\n" +
		styleMuted.Render("i index · s sync · a audit · r restore · A all · f fold · enter detail")
}

// renderFoldChip notes how many no-op rows the fold is hiding, so a folded
// view never looks like runs simply went missing.
func (m *runsModel) renderFoldChip() string {
	if !m.fold || m.foldedCount == 0 {
		return ""
	}
	return "  " + styleMuted.Render(fmt.Sprintf("(%d no-op rows folded — f to expand)", m.foldedCount))
}

// renderFilterChips renders the active-filter pill, or muted text when no
// filter is set. Kept inline so the runs screen stays self-contained.
func (m *runsModel) renderFilterChips() string {
	if m.filterKind == "" {
		return styleMuted.Render("(all kinds)")
	}
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color("231")).
		Background(colourAccent).
		Padding(0, 1).
		Render(m.filterKind)
}

func (m *runsModel) renderDetail(r store.Run) string {
	now := time.Now()
	lines := [][2]string{
		{"ID", fmt.Sprintf("#%d", r.ID)},
		{"Kind", r.Kind},
		{"Volume", m.volumeName(r.VolumeID)},
		{"Destination", nullStr(r.Destination)},
		{"Status", lipgloss.NewStyle().Foreground(statusColour(r.Status)).Render(glyphForStatus(r.Status) + " " + r.Status)},
		{"Started", fmt.Sprintf("%s  (%s)", time.Unix(0, r.StartedAtNs).Format(time.RFC3339), humanDuration(now.Sub(time.Unix(0, r.StartedAtNs)))+" ago")},
	}
	if r.EndedAtNs.Valid {
		end := time.Unix(0, r.EndedAtNs.Int64)
		lines = append(lines,
			[2]string{"Ended", fmt.Sprintf("%s  (%s)", end.Format(time.RFC3339), humanDuration(now.Sub(end))+" ago")},
			[2]string{"Duration", humanDuration(end.Sub(time.Unix(0, r.StartedAtNs)))},
		)
	} else {
		lines = append(lines,
			[2]string{"Ended", styleMuted.Render("—  (still running)")},
			[2]string{"Elapsed", humanDuration(now.Sub(time.Unix(0, r.StartedAtNs)))},
		)
	}
	lines = append(lines, [2]string{"Files", fmt.Sprintf("%d", r.FileCount)})
	lines = append(lines, [2]string{"Mode", shallowLabel(r.Shallow)})
	if r.PeerNodeID.Valid {
		lines = append(lines, [2]string{"Peer node", fmt.Sprintf("#%d", r.PeerNodeID.Int64)})
	}
	if r.CorrelatedRunID.Valid {
		lines = append(lines, [2]string{"Correlated run", fmt.Sprintf("#%d", r.CorrelatedRunID.Int64)})
	}
	if r.Error.Valid && r.Error.String != "" {
		lines = append(lines, [2]string{"Error", styleErr.Render(r.Error.String)})
	}

	var b strings.Builder
	b.WriteString(styleHeader.Render(fmt.Sprintf("Run #%d", r.ID)))
	b.WriteString("\n\n")
	labelWidth := 0
	for _, kv := range lines {
		if w := lipgloss.Width(kv[0]); w > labelWidth {
			labelWidth = w
		}
	}
	for _, kv := range lines {
		b.WriteString(styleMuted.Render(padRight(kv[0]+":", labelWidth+1)))
		b.WriteString("  ")
		b.WriteString(kv[1])
		b.WriteByte('\n')
	}
	b.WriteString("\n")
	b.WriteString(styleMuted.Render("esc / backspace / enter — back"))
	return b.String()
}

func (m *runsModel) setFilter(kind string) {
	m.filterKind = kind
	m.applyFilter()
}

func (m *runsModel) filteredRows() []store.Run {
	if m.filterKind == "" {
		return m.rows
	}
	out := make([]store.Run, 0, len(m.rows))
	for _, r := range m.rows {
		if r.Kind == m.filterKind {
			out = append(out, r)
		}
	}
	return out
}

func (m *runsModel) applyFilter() {
	filtered := m.filteredRows()
	now := time.Now()
	rows := make([]table.Row, 0, len(filtered))
	display := make([]*store.Run, 0, len(filtered))
	m.foldedCount = 0
	for i := 0; i < len(filtered); {
		// Collapse a maximal run of consecutive no-op rows into one marker,
		// but only when there is more than one — a lone no-op isn't worth
		// a fold line (F19).
		if m.fold && runIsNoOp(filtered[i]) {
			j := i
			for j < len(filtered) && runIsNoOp(filtered[j]) {
				j++
			}
			if n := j - i; n > 1 {
				rows = append(rows, foldMarkerRow(n))
				display = append(display, nil)
				m.foldedCount += n
				i = j
				continue
			}
		}
		r := filtered[i]
		rows = append(rows, m.runTableRow(r, now))
		display = append(display, &r)
		i++
	}
	m.displayRuns = display
	m.table.SetRows(rows)
	m.resizeColumns()
}

// runTableRow renders one run as a table row.
func (m *runsModel) runTableRow(r store.Run, now time.Time) table.Row {
	return table.Row{
		fmt.Sprintf("#%d", r.ID),
		r.Kind,
		m.volumeName(r.VolumeID),
		destinationCell(r),
		r.Status,
		whenAgo(r.EndedAtNs, now),
		runDuration(r),
		fmt.Sprintf("%d", r.FileCount),
		shallowGlyph(r.Shallow),
	}
}

// foldMarkerRow is the placeholder shown in place of a collapsed block of
// no-op rows. It carries the same column count as a real row so the table
// layout stays aligned.
func foldMarkerRow(n int) table.Row {
	return table.Row{"⋯", "", fmt.Sprintf("%d no-op runs folded", n), "", "", "", "", "", ""}
}

// runIsNoOp reports whether a run is routine no-op noise — a clean success
// that touched no files. Mirrors the CLI's `runs --changes` fold rule; a
// peer-sync no-op has file_count 0, so those collapse, while bucket/index
// no-ops (file_count counts files considered, not moved) stay visible.
func runIsNoOp(r store.Run) bool {
	return r.Status == store.RunStatusSuccess && r.FileCount == 0
}

// destinationCell renders the DESTINATION column, prefixing a peer-sync row
// with its initiation-direction arrow so inbound and outbound are
// distinguishable (F18): "→" outbound (we pushed), "←" inbound (peer
// pushed). Only the initiator records runs.shallow, so a set flag marks the
// pushing side (see store.Run.Shallow).
func destinationCell(r store.Run) string {
	if !r.Destination.Valid {
		return nullStr(r.Destination)
	}
	if !r.PeerNodeID.Valid {
		return r.Destination.String
	}
	arrow := "→"
	if !r.Shallow.Valid {
		arrow = "←"
	}
	return arrow + " " + r.Destination.String
}

func (m *runsModel) resizeColumns() {
	cols := []table.Column{
		{Title: "ID", Width: 6},
		{Title: "KIND", Width: 8},
		{Title: "VOLUME", Width: 16},
		{Title: "DESTINATION", Width: 16},
		{Title: "STATUS", Width: 9},
		{Title: "WHEN", Width: 12},
		{Title: "DURATION", Width: 10},
		{Title: "FILES", Width: 8},
		{Title: "MODE", Width: 8},
	}
	// If we know the terminal width, give the surplus to the variable-width
	// columns (volume, destination) — those are the ones that benefit most
	// from extra room on a wide terminal.
	if m.width > 0 {
		fixed := 0
		for _, c := range cols {
			fixed += c.Width
		}
		// Account for the 2-space gutters that the table library uses
		// between columns; cheaper to under-pad than over-pad.
		fixed += 2 * (len(cols) - 1)
		surplus := m.width - fixed - 4 // -4 for padding the screen leaves
		if surplus > 0 {
			cols[2].Width += surplus / 2
			cols[3].Width += surplus - surplus/2
		}
	}
	m.table.SetColumns(cols)
}

// shallowLabel renders the runs.shallow flag for the detail view as a
// full word. NULL stays "—" (with context) because pre-v10 history and
// sync/restore runs have nothing honest to say about the hashing mode.
func shallowLabel(s sql.NullBool) string {
	if !s.Valid {
		return styleMuted.Render("— (not recorded)")
	}
	if s.Bool {
		return "shallow (skipped rehash when size/mtime matched)"
	}
	return "full (rehashed every file)"
}

// shallowGlyph renders runs.shallow compactly for the table column.
// NULL stays "—" because the flag wasn't recorded (pre-v10 history) or
// doesn't apply (sync/restore runs).
func shallowGlyph(s sql.NullBool) string {
	if !s.Valid {
		return styleMuted.Render("—")
	}
	if s.Bool {
		return "shallow"
	}
	return "full"
}

func (m *runsModel) volumeName(id sql.NullInt64) string {
	if !id.Valid {
		return styleMuted.Render("—")
	}
	if v, ok := m.volumesByID[id.Int64]; ok {
		return v.Name
	}
	return fmt.Sprintf("vol#%d", id.Int64)
}

func (m *runsModel) fetch() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		// 500 is a generous cap that still loads instantly even on a busy
		// install; older runs can be inspected via `squirrel runs` from the
		// CLI if needed.
		runs, err := m.store.ListRuns(ctx, store.ListRunsOpts{Limit: 500, Descending: true})
		if err != nil {
			return runsDataMsg{err: err}
		}
		vols, err := m.store.ListVolumes(ctx)
		if err != nil {
			return runsDataMsg{err: err}
		}
		return runsDataMsg{runs: runs, volumes: vols}
	}
}
