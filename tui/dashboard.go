package tui

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/status"
	"github.com/mbertschler/squirrel/store"
)

// dashboardModel surfaces squirrel's live state in one screen:
//
//   - the local agent's health (one-line probe of /v1/health)
//   - standing per-destination alarms
//   - runs currently in-flight (kind, volume, destination, elapsed)
//   - the per-(volume × target) coverage + durability grid (the "am I
//     safe?" panel, from the shared status query layer)
//   - the most recent terminal runs
//
// Data is pulled on each tickMsg via a single SQL pass plus one HTTP probe;
// both run as Bubble Tea commands so the UI never blocks on I/O.
type dashboardModel struct {
	store  *store.Store
	cfg    *config.Config
	client *agentClient

	width, height int

	data        dashboardData
	loaded      bool
	loadErr     error
	agentStatus agentStatus

	// readinessByVol caches the last computed offload-readiness tally per
	// volume name. The coverage/durability grid rebuilds every one-second
	// tick with readiness skipped (its dominant cost — a whole-index gate
	// pass); the tally is refreshed on the slower readinessRefreshTicks
	// cadence and overlaid onto the grid at render time, so "N offloadable
	// now" stays visible without paying for it every tick.
	readinessByVol      map[string]status.OffloadReadiness
	ticksSinceReadiness int
}

// readinessRefreshTicks is how many one-second ticks pass between
// offload-readiness recomputations. Coverage and durability still refresh
// every tick; only the expensive gate-pass tally is throttled.
const readinessRefreshTicks = 15

// dashboardData is the snapshot rendered by the dashboard. Built fresh on
// each tick — there is no incremental update path, since the queries are
// trivial and the resulting struct is small.
type dashboardData struct {
	now        time.Time
	volumes    []store.Volume
	activeRuns []store.Run
	recentRuns []store.Run
	// coverage is the per-(volume × target) sync-coverage and durability
	// grid from the shared status query layer — the same facts and
	// severities `squirrel status` prints. Empty when no config is loaded
	// (the grid needs sync_to / offload_requires / cadences to render).
	coverage status.Report
	// alarms are the standing per-destination alarms (#157, F30). A verify
	// mismatch latches one until cleared; the dashboard shows them so the
	// trust surface answers "am I safe?" with a red panel, not silence.
	alarms []store.DestinationAlarm
	// contested are the standing contested-path freezes (#158, F27). A
	// peer-sync conflict latches one until an operator resolves it; the
	// dashboard shows them as a badge on *this* machine — the losing edge,
	// not only the hub — so a silently diverging file surfaces.
	contested []store.ContestedPath
}

type dashboardDataMsg struct {
	data dashboardData
	err  error
	// readinessFresh is true when this fetch recomputed the offload tally
	// (a slow-cadence tick), so the receiver should refresh its cache from
	// data.coverage; a fast tick leaves it false and the cache stands.
	readinessFresh bool
}

type agentStatus struct {
	configured bool
	reachable  bool
	lastErr    string
}

type agentStatusMsg agentStatus

func newDashboardModel(s *store.Store, cfg *config.Config) *dashboardModel {
	return &dashboardModel{store: s, cfg: cfg}
}

// attachClient is called by the root model after construction so the
// dashboard can probe the agent. Done separately from newDashboardModel so
// the constructor stays narrow.
func (m *dashboardModel) attachClient(c *agentClient) { m.client = c }

func (m *dashboardModel) Init() tea.Cmd {
	// Recompute readiness on (re)activation so the tally is current the
	// moment the user opens the dashboard, then throttle on the tick path.
	m.ticksSinceReadiness = 0
	return tea.Batch(m.fetchData(true), m.probeAgent())
}

func (m *dashboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tickMsg:
		m.ticksSinceReadiness++
		refresh := m.ticksSinceReadiness >= readinessRefreshTicks
		if refresh {
			m.ticksSinceReadiness = 0
		}
		return m, tea.Batch(m.fetchData(refresh), m.probeAgent())
	case dashboardDataMsg:
		m.loaded = true
		m.loadErr = msg.err
		if msg.err == nil {
			m.data = msg.data
			if msg.readinessFresh {
				m.cacheReadiness(msg.data.coverage)
			}
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
	}
	if alarms := m.renderAlarms(); alarms != "" {
		sections = append(sections, alarms)
	}
	if contested := m.renderContested(); contested != "" {
		sections = append(sections, contested)
	}
	sections = append(sections,
		m.renderActiveRuns(),
		m.renderCoverage(),
		m.renderRecentRuns(),
	)
	return strings.Join(sections, "\n\n")
}

// renderAlarms shows the standing per-destination alarms (#157, F30) high
// on the dashboard, right below agent health, because a latched verify
// mismatch is exactly the "am I safe?" answer the trust surface must not
// bury. Returns "" when nothing is in alarm so the section is absent on a
// healthy install rather than showing an empty green box.
func (m *dashboardModel) renderAlarms() string {
	if len(m.data.alarms) == 0 {
		return ""
	}
	header := styleErr.Render(fmt.Sprintf("Alarms (%d)", len(m.data.alarms)))
	rows := [][]string{{"DESTINATION", "KIND", "SINCE", "RUN", "DETAIL"}}
	for _, a := range m.data.alarms {
		rows = append(rows, []string{
			a.Destination,
			a.Kind,
			whenAgo(sql.NullInt64{Int64: a.RaisedAtNs, Valid: true}, m.data.now),
			fmt.Sprintf("#%d", a.RaisedRunID),
			a.Detail,
		})
	}
	colours := []lipgloss.Color{colourFailure, "", "", "", ""}
	return header + "\n" + renderTable(rows, colours)
}

// renderContested shows standing contested-path freezes (#158, F27) as a
// badge just below the alarms, because a divergent edit frozen on this
// machine is part of the "am I safe?" answer the trust surface must not
// bury. Returns "" when nothing is frozen so the section is absent on a
// healthy install. The count in the header is the badge the losing edge
// needs — the hub is no longer the only place the conflict is visible.
func (m *dashboardModel) renderContested() string {
	if len(m.data.contested) == 0 {
		return ""
	}
	header := styleErr.Render(fmt.Sprintf("Contested paths (%d)", len(m.data.contested)))
	rows := [][]string{{"VOLUME", "PATH", "PRESERVED AT", "SINCE"}}
	for _, c := range m.data.contested {
		rows = append(rows, []string{
			m.volumeName(sql.NullInt64{Int64: c.VolumeID, Valid: true}),
			c.Path,
			contestedPreservedCell(c.PreservedAtPath),
			whenAgo(sql.NullInt64{Int64: c.RaisedAtNs, Valid: true}, m.data.now),
		})
	}
	colours := []lipgloss.Color{colourFailure, "", "", ""}
	hint := styleMuted.Render("resolve with `squirrel conflicts resolve <volume> <path>`")
	return header + "\n" + renderTable(rows, colours) + "\n" + hint
}

// contestedPreservedCell renders the preserved-loser location, or a dash
// when the freeze record carries none (an initiator that learned of the
// freeze without a preserved path).
func contestedPreservedCell(p string) string {
	if p == "" {
		return "—"
	}
	return p
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

// renderCoverage is the "am I safe?" panel: per volume, the per-target
// sync-coverage and durability grid from the shared status layer, replacing
// the old single LAST SYNC cell that hid a week-behind target behind a
// fresh ✓ (friction-log F16/F17/F23). Each volume gets a header line
// (name, path, index freshness, offloadable total) coloured by its worst
// level, then a target sub-table with the STATE and DURABLE cells coloured
// per target.
func (m *dashboardModel) renderCoverage() string {
	vols := m.data.coverage.Volumes
	header := styleHeader.Render(fmt.Sprintf("Coverage (%d)", len(vols)))
	if len(vols) == 0 {
		hint := "no volumes configured"
		if m.cfg == nil {
			hint = "no config loaded — coverage needs sync_to / offload_requires to render"
		}
		return header + "\n" + styleMuted.Render(hint)
	}
	blocks := []string{header}
	for _, v := range vols {
		blocks = append(blocks, m.renderVolumeCoverage(v))
	}
	return strings.Join(blocks, "\n\n")
}

// renderVolumeCoverage renders one volume's coverage block. The
// offload-readiness figure comes from the throttled cache (the grid itself
// rebuilds every tick with readiness skipped), falling back to the volume's
// own tally when the cache has no entry yet.
func (m *dashboardModel) renderVolumeCoverage(v status.VolumeStatus) string {
	dot := lipgloss.NewStyle().Foreground(levelColour(v.Level())).Render("●")
	title := fmt.Sprintf("%s %s  %s", dot, v.Name, styleMuted.Render(v.Path))
	offload := v.Offload
	if cached, ok := m.readinessByVol[v.Name]; ok {
		offload = cached
	}
	meta := styleMuted.Render(fmt.Sprintf("index %s · %s",
		status.IndexLabel(v), status.OffloadLabel(offload)))
	if len(v.Targets) == 0 {
		return title + "\n" + meta + "\n" + styleMuted.Render("  no targets configured")
	}
	rows := [][]string{{"TARGET", "ROLE", "LAST SYNC", "STATE", "DURABLE", "METHOD", "EVIDENCE"}}
	for _, t := range v.Targets {
		rows = append(rows, []string{
			t.Name, status.RoleLabel(t), status.LastSyncLabel(t), status.StateLabel(t),
			status.DurableLabel(t), status.MethodLabel(t), status.EvidenceLabel(t),
		})
	}
	tbl := renderTableColoured(rows, nil, coverageCellColour(v.Targets))
	return title + "\n" + meta + "\n" + tbl
}

// coverageCellColour paints the STATE column by each target's coverage
// level and the DURABLE column by its durability level, so the two "am I
// safe?" dimensions read at a glance without decoding the words.
func coverageCellColour(targets []status.TargetStatus) func(rowIdx, colIdx int) lipgloss.Color {
	const stateCol, durableCol = 3, 4
	return func(rowIdx, colIdx int) lipgloss.Color {
		if rowIdx == 0 || rowIdx > len(targets) {
			return ""
		}
		t := targets[rowIdx-1]
		switch colIdx {
		case stateCol:
			return levelColour(t.SyncLevel)
		case durableCol:
			if t.Durability != nil {
				return levelColour(t.Durability.Level)
			}
		}
		return ""
	}
}

// levelColour maps a status level onto the dashboard palette. Neutral gets
// no colour (the default foreground) so informational cells don't shout.
func levelColour(l status.Level) lipgloss.Color {
	switch l {
	case status.LevelOK:
		return colourSuccess
	case status.LevelWarn:
		return colourWarning
	case status.LevelCritical:
		return colourFailure
	default:
		return ""
	}
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

// fetchData loads the dashboard snapshot. withReadiness selects whether the
// coverage build pays for the offload-readiness tally: false on the
// one-second tick (the grid still refreshes; the cached tally is overlaid
// at render), true on the slow cadence and on activation.
func (m *dashboardModel) fetchData(withReadiness bool) tea.Cmd {
	return func() tea.Msg {
		// Use a tight per-fetch deadline so a stuck DB doesn't freeze the UI.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		data, err := loadDashboardData(ctx, m.store, m.cfg, !withReadiness)
		return dashboardDataMsg{data: data, err: err, readinessFresh: withReadiness && err == nil}
	}
}

// cacheReadiness refreshes the per-volume offload tally from a report that
// was built with readiness computed. Fast-tick reports carry only the
// policy flag (zero counts) and must not overwrite this cache.
func (m *dashboardModel) cacheReadiness(rep status.Report) {
	cache := make(map[string]status.OffloadReadiness, len(rep.Volumes))
	for _, v := range rep.Volumes {
		cache[v.Name] = v.Offload
	}
	m.readinessByVol = cache
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

// loadDashboardData runs the queries that back the dashboard. The
// recent-runs bucket comes from a bounded ListRuns scan (200 is plenty
// for "what happened today"); the coverage grid comes from the shared
// status query layer, which scans per (volume × target) so a target beyond
// the recent-runs window still surfaces correctly. The coverage build is
// skipped when no config is loaded — it needs sync_to / offload_requires /
// cadences — leaving the grid to render its own "no config" hint.
// skipReadiness omits the expensive offload-readiness tally (see fetchData
// and readinessRefreshTicks).
func loadDashboardData(ctx context.Context, s *store.Store, cfg *config.Config, skipReadiness bool) (dashboardData, error) {
	now := time.Now()
	vols, err := s.ListVolumes(ctx)
	if err != nil {
		return dashboardData{}, fmt.Errorf("list volumes: %w", err)
	}
	runs, err := s.ListRuns(ctx, store.ListRunsOpts{Limit: 200, Descending: true})
	if err != nil {
		return dashboardData{}, fmt.Errorf("list runs: %w", err)
	}
	alarms, err := s.ListDestinationAlarms(ctx)
	if err != nil {
		return dashboardData{}, fmt.Errorf("list alarms: %w", err)
	}
	contested, err := s.ListContestedPaths(ctx)
	if err != nil {
		return dashboardData{}, fmt.Errorf("list contested paths: %w", err)
	}
	var coverage status.Report
	if cfg != nil {
		if coverage, err = status.BuildWithOptions(ctx, s, cfg, status.Options{SkipOffloadReadiness: skipReadiness}); err != nil {
			return dashboardData{}, fmt.Errorf("build coverage: %w", err)
		}
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
		now:        now,
		volumes:    vols,
		activeRuns: active,
		recentRuns: recent,
		coverage:   coverage,
		alarms:     alarms,
		contested:  contested,
	}, nil
}
