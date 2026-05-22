package tui

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/mbertschler/squirrel/store"
)

// dashboardModel surfaces squirrel's live state in one screen:
//
//   - the local agent's health (one-line probe of /v1/health)
//   - runs currently in-flight (kind, volume, destination, elapsed)
//   - per-volume health (name, path, last-index, last-sync)
//   - the most recent terminal runs
//
// Data is pulled on each tickMsg via a single SQL pass plus one HTTP probe;
// both run as Bubble Tea commands so the UI never blocks on I/O.
type dashboardModel struct {
	store  *store.Store
	client *agentClient

	width, height int

	data        dashboardData
	loaded      bool
	loadErr     error
	agentStatus agentStatus
}

// dashboardData is the snapshot rendered by the dashboard. Built fresh on
// each tick — there is no incremental update path, since the queries are
// trivial and the resulting struct is small.
type dashboardData struct {
	now        time.Time
	volumes    []store.Volume
	activeRuns []store.Run
	recentRuns []store.Run
	// latestByVol[volID][kind] is the most recent terminal-status run for
	// that (volume, kind) pair. Used to fill the "last index" / "last sync"
	// columns of the volumes table.
	latestByVol map[int64]map[string]store.Run
}

type dashboardDataMsg struct {
	data dashboardData
	err  error
}

type agentStatus struct {
	configured bool
	reachable  bool
	lastErr    string
}

type agentStatusMsg agentStatus

func newDashboardModel(s *store.Store) *dashboardModel {
	return &dashboardModel{store: s}
}

// attachClient is called by the root model after construction so the
// dashboard can probe the agent. Done separately from newDashboardModel so
// the constructor stays narrow.
func (m *dashboardModel) attachClient(c *agentClient) { m.client = c }

func (m *dashboardModel) Init() tea.Cmd {
	return tea.Batch(m.fetchData(), m.probeAgent())
}

func (m *dashboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tickMsg:
		return m, tea.Batch(m.fetchData(), m.probeAgent())
	case dashboardDataMsg:
		m.loaded = true
		m.loadErr = msg.err
		if msg.err == nil {
			m.data = msg.data
		}
		return m, nil
	case agentStatusMsg:
		m.agentStatus = agentStatus(msg)
		return m, nil
	}
	return m, nil
}

func (m *dashboardModel) View() string {
	if !m.loaded && m.loadErr == nil {
		return styleMuted.Render("loading…")
	}
	if m.loadErr != nil {
		return styleErr.Render(fmt.Sprintf("dashboard error: %v", m.loadErr))
	}
	sections := []string{
		m.renderAgentBlock(),
		m.renderActiveRuns(),
		m.renderVolumes(),
		m.renderRecentRuns(),
	}
	return strings.Join(sections, "\n\n")
}

func (m *dashboardModel) renderAgentBlock() string {
	header := styleHeader.Render("Agent")
	var body string
	switch {
	case !m.agentStatus.configured:
		body = styleMuted.Render("no [agent] in config — Browse and history still work")
	case m.agentStatus.reachable:
		dot := lipgloss.NewStyle().Foreground(colourSuccess).Render("●")
		body = fmt.Sprintf("%s up   %s", dot, styleMuted.Render(m.client.baseURL))
	default:
		dot := lipgloss.NewStyle().Foreground(colourFailure).Render("●")
		errText := m.agentStatus.lastErr
		if errText == "" {
			errText = "unreachable"
		}
		body = fmt.Sprintf("%s down %s   %s", dot, styleMuted.Render(m.client.baseURL), styleErr.Render(errText))
	}
	return header + "\n" + body
}

func (m *dashboardModel) renderActiveRuns() string {
	header := styleHeader.Render(fmt.Sprintf("Active runs (%d)", len(m.data.activeRuns)))
	if len(m.data.activeRuns) == 0 {
		return header + "\n" + styleMuted.Render("nothing running")
	}
	rows := make([][]string, 0, len(m.data.activeRuns))
	rows = append(rows, []string{"ID", "KIND", "VOLUME", "DESTINATION", "ELAPSED"})
	for _, r := range m.data.activeRuns {
		rows = append(rows, []string{
			fmt.Sprintf("#%d", r.ID),
			r.Kind,
			m.volumeName(r.VolumeID),
			nullStr(r.Destination),
			elapsedSince(r.StartedAtNs, m.data.now),
		})
	}
	return header + "\n" + renderTable(rows, []lipgloss.Color{"", "", "", "", colourRunning})
}

func (m *dashboardModel) renderVolumes() string {
	header := styleHeader.Render(fmt.Sprintf("Volumes (%d)", len(m.data.volumes)))
	if len(m.data.volumes) == 0 {
		return header + "\n" + styleMuted.Render("no volumes configured")
	}
	rows := [][]string{{"NAME", "PATH", "LAST INDEX", "LAST SYNC"}}
	for _, v := range m.data.volumes {
		rows = append(rows, []string{
			v.Name,
			v.Path,
			m.formatLast(v.ID, store.RunKindIndex),
			m.formatLast(v.ID, store.RunKindSync),
		})
	}
	return header + "\n" + renderTable(rows, nil)
}

func (m *dashboardModel) renderRecentRuns() string {
	header := styleHeader.Render(fmt.Sprintf("Recent runs (%d)", len(m.data.recentRuns)))
	if len(m.data.recentRuns) == 0 {
		return header + "\n" + styleMuted.Render("no runs yet")
	}
	rows := [][]string{{"ID", "KIND", "VOLUME", "STATUS", "WHEN", "DURATION", "FILES"}}
	colours := []lipgloss.Color{"", "", "", "", "", "", ""}
	for _, r := range m.data.recentRuns {
		rows = append(rows, []string{
			fmt.Sprintf("#%d", r.ID),
			r.Kind,
			m.volumeName(r.VolumeID),
			r.Status,
			whenAgo(r.EndedAtNs, m.data.now),
			runDuration(r),
			fmt.Sprintf("%d", r.FileCount),
		})
	}
	t := renderTableColoured(rows, colours, func(rowIdx, colIdx int) lipgloss.Color {
		if rowIdx == 0 || colIdx != 3 {
			return ""
		}
		return statusColour(rows[rowIdx][3])
	})
	return header + "\n" + t
}

func (m *dashboardModel) volumeName(id sql.NullInt64) string {
	if !id.Valid {
		return "—"
	}
	for _, v := range m.data.volumes {
		if v.ID == id.Int64 {
			return v.Name
		}
	}
	return fmt.Sprintf("vol#%d", id.Int64)
}

func (m *dashboardModel) formatLast(volID int64, kind string) string {
	byKind := m.data.latestByVol[volID]
	if byKind == nil {
		return styleMuted.Render("—")
	}
	r, ok := byKind[kind]
	if !ok {
		return styleMuted.Render("—")
	}
	ago := whenAgo(r.EndedAtNs, m.data.now)
	statusGlyph := lipgloss.NewStyle().Foreground(statusColour(r.Status)).Render(glyphForStatus(r.Status))
	return fmt.Sprintf("%s %s", ago, statusGlyph)
}

func (m *dashboardModel) fetchData() tea.Cmd {
	return func() tea.Msg {
		// Use a tight per-fetch deadline so a stuck DB doesn't freeze the UI.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		data, err := loadDashboardData(ctx, m.store)
		return dashboardDataMsg{data: data, err: err}
	}
}

func (m *dashboardModel) probeAgent() tea.Cmd {
	return func() tea.Msg {
		if m.client == nil || !m.client.configured() {
			return agentStatusMsg{configured: false}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		err := m.client.health(ctx)
		st := agentStatusMsg{configured: true, reachable: err == nil}
		if err != nil {
			st.lastErr = err.Error()
		}
		return st
	}
}

// loadDashboardData runs the SQL queries that back the dashboard. The
// recent-runs bucket comes from a bounded ListRuns scan (200 is plenty
// for "what happened today"); the per-(volume,kind) "last successful"
// table comes from its own helper that scans every run, so volumes
// whose last index sits beyond the recent window still surface
// correctly.
func loadDashboardData(ctx context.Context, s *store.Store) (dashboardData, error) {
	now := time.Now()
	vols, err := s.ListVolumes(ctx)
	if err != nil {
		return dashboardData{}, fmt.Errorf("list volumes: %w", err)
	}
	runs, err := s.ListRuns(ctx, store.ListRunsOpts{Limit: 200, Descending: true})
	if err != nil {
		return dashboardData{}, fmt.Errorf("list runs: %w", err)
	}
	latestByVol, err := s.LatestSuccessfulRunsByVolumeAndKind(ctx)
	if err != nil {
		return dashboardData{}, fmt.Errorf("latest by volume: %w", err)
	}
	var active, recent []store.Run
	for _, r := range runs {
		if r.Status == store.RunStatusRunning {
			active = append(active, r)
			continue
		}
		if len(recent) < 10 {
			recent = append(recent, r)
		}
	}
	return dashboardData{
		now:         now,
		volumes:     vols,
		activeRuns:  active,
		recentRuns:  recent,
		latestByVol: latestByVol,
	}, nil
}
