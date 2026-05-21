// Package tui implements squirrel's terminal UI. It is a read-mostly view
// over the local SQLite index (via the store package) with a thin HTTP
// client to the local agent for write actions like "kick an index run". The
// agent is required for actions; browsing the index works fine when the
// agent is down.
//
// The TUI follows the Bubble Tea Elm pattern: one root [model] holds the
// shared state (store handle, config, agent client, terminal size, current
// screen) and each screen is its own tea.Model embedded into the root and
// dispatched to on Update/View. A single tick message every pollInterval
// drives data refresh; screens declare what they want to fetch in response
// to the tick and surface the result via screen-specific messages.
package tui

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// pollInterval is how often the TUI refreshes background state (volumes,
// running runs, recent runs). It is short enough that the UI feels live but
// long enough that SQLite poll cost is negligible.
const pollInterval = time.Second

// screen identifies which top-level view is currently focused. Browse is
// not in the tab bar — it is entered by selecting a volume on the Volumes
// screen and exited with backspace/escape — because it is contextual to a
// volume rather than a peer of the other screens.
type screen int

const (
	screenDashboard screen = iota
	screenRuns
	screenVolumes
	screenBrowse
)

// tickMsg is the periodic refresh trigger. Screens watch for it and emit
// their own data-fetch commands in response.
type tickMsg time.Time

// errMsg surfaces a non-fatal error to the status bar. Fatal errors come
// out of Run as the returned error instead.
type errMsg struct{ err error }

// Run launches the TUI and blocks until the user quits or the context is
// cancelled. The caller owns the store and config (the TUI does not close
// either). cfg may be nil — Browse and most read paths still work; the
// agent client falls back to a no-agent stub that surfaces a "configure an
// agent to kick runs from the TUI" error to the user when they try an
// action.
func Run(ctx context.Context, s *store.Store, cfg *config.Config) error {
	root := newRootModel(s, cfg)
	prog := tea.NewProgram(
		root,
		tea.WithContext(ctx),
		tea.WithAltScreen(),
	)
	_, err := prog.Run()
	return err
}

// rootModel is the top-level Bubble Tea model. It owns the shared state and
// delegates Update/View to the active screen model.
type rootModel struct {
	store  *store.Store
	cfg    *config.Config
	client *agentClient

	width, height int
	active        screen

	// Status bar text. Cleared on the next tick after it was set, so transient
	// errors stay visible long enough to read without sticking forever.
	status        string
	statusSetTick int

	dashboard *dashboardModel
	runs      *runsModel
	volumes   *volumesModel
	browse    *browseModel
}

func newRootModel(s *store.Store, cfg *config.Config) *rootModel {
	client := newAgentClient(cfg)
	return &rootModel{
		store:     s,
		cfg:       cfg,
		client:    client,
		active:    screenDashboard,
		dashboard: newDashboardModel(s),
		runs:      newRunsModel(s),
		volumes:   newVolumesModel(s),
		browse:    newBrowseModel(s),
	}
}

func (m *rootModel) Init() tea.Cmd {
	return tea.Batch(
		tick(),
		m.dashboard.Init(),
		m.runs.Init(),
		m.volumes.Init(),
	)
}

func (m *rootModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		// Forward to all screens so they can layout against the new size even
		// while invisible — switching tabs should not require a resize event
		// to refresh the layout.
		var cmds []tea.Cmd
		cmds = append(cmds, forwardSize(m.dashboard, msg))
		cmds = append(cmds, forwardSize(m.runs, msg))
		cmds = append(cmds, forwardSize(m.volumes, msg))
		cmds = append(cmds, forwardSize(m.browse, msg))
		return m, tea.Batch(cmds...)

	case tickMsg:
		// Clear stale status text. One tick of stickiness is enough to notice
		// an error without it lingering across screens.
		if m.statusSetTick > 0 {
			m.statusSetTick--
			if m.statusSetTick == 0 {
				m.status = ""
			}
		}
		// Fan the tick out to every screen — they each decide what to refresh.
		// Re-arm the timer.
		return m, tea.Batch(
			tick(),
			forwardTick(m.dashboard, msg),
			forwardTick(m.runs, msg),
			forwardTick(m.volumes, msg),
			forwardTick(m.browse, msg),
		)

	case errMsg:
		m.status = msg.err.Error()
		m.statusSetTick = 5
		return m, nil

	case browseEnterVolumeMsg:
		m.browse.setVolume(msg.volumeID, msg.volumeName)
		m.active = screenBrowse
		return m, m.browse.Init()

	case tea.KeyMsg:
		// Global keys first. Screens never see q/?/Tab/1-3.
		switch msg.String() {
		case "ctrl+c", "q":
			if m.active == screenBrowse {
				// In Browse, q backs out to the Volumes list instead of quitting.
				m.active = screenVolumes
				return m, nil
			}
			return m, tea.Quit
		case "tab":
			m.cycleTab(+1)
			return m, nil
		case "shift+tab":
			m.cycleTab(-1)
			return m, nil
		case "1":
			m.active = screenDashboard
			return m, nil
		case "2":
			m.active = screenRuns
			return m, nil
		case "3":
			m.active = screenVolumes
			return m, nil
		case "esc":
			if m.active == screenBrowse {
				m.active = screenVolumes
				return m, nil
			}
		}
	}

	// Delegate to the active screen.
	switch m.active {
	case screenDashboard:
		_, cmd := m.dashboard.Update(msg)
		return m, cmd
	case screenRuns:
		_, cmd := m.runs.Update(msg)
		return m, cmd
	case screenVolumes:
		_, cmd := m.volumes.Update(msg)
		return m, cmd
	case screenBrowse:
		_, cmd := m.browse.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *rootModel) View() string {
	if m.width == 0 {
		return "loading…"
	}
	header := m.renderTabs()
	footer := m.renderStatusBar()

	bodyHeight := m.height - lipgloss.Height(header) - lipgloss.Height(footer)
	if bodyHeight < 1 {
		bodyHeight = 1
	}

	var body string
	switch m.active {
	case screenDashboard:
		body = m.dashboard.View()
	case screenRuns:
		body = m.runs.View()
	case screenVolumes:
		body = m.volumes.View()
	case screenBrowse:
		body = m.browse.View()
	}

	body = lipgloss.NewStyle().
		Width(m.width).
		Height(bodyHeight).
		Padding(0, 1).
		Render(body)

	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

func (m *rootModel) cycleTab(delta int) {
	// Tabs cycle through Dashboard / Runs / Volumes only — Browse is reached
	// from Volumes, not from the tab bar.
	tabs := []screen{screenDashboard, screenRuns, screenVolumes}
	var idx int
	for i, t := range tabs {
		if t == m.active {
			idx = i
			break
		}
	}
	idx = (idx + delta + len(tabs)) % len(tabs)
	m.active = tabs[idx]
}

func (m *rootModel) renderTabs() string {
	labels := []struct {
		s  screen
		t  string
		hk string
	}{
		{screenDashboard, "Dashboard", "1"},
		{screenRuns, "Runs", "2"},
		{screenVolumes, "Volumes", "3"},
	}
	var rendered []string
	for _, l := range labels {
		text := fmt.Sprintf("%s %s", l.hk, l.t)
		if l.s == m.active || (m.active == screenBrowse && l.s == screenVolumes) {
			rendered = append(rendered, styleTabActive.Render(text))
		} else {
			rendered = append(rendered, styleTab.Render(text))
		}
	}
	title := styleTitle.Padding(0, 1).Render("squirrel")
	bar := lipgloss.JoinHorizontal(lipgloss.Top, append([]string{title}, rendered...)...)
	return lipgloss.NewStyle().Width(m.width).Render(bar)
}

func (m *rootModel) renderStatusBar() string {
	left := "q quit · tab switch · ? help"
	if m.active == screenBrowse {
		left = "↑↓ navigate · enter descend · backspace ascend · esc back · q back"
	}
	var right string
	if m.status != "" {
		right = styleErr.Render(m.status)
	}
	bar := lipgloss.NewStyle().Width(m.width).Render(
		lipgloss.JoinHorizontal(lipgloss.Top,
			styleStatusBar.Render(left),
			lipgloss.NewStyle().
				Width(m.width-lipgloss.Width(left)-2).
				Align(lipgloss.Right).
				Render(right),
		),
	)
	return bar
}

func tick() tea.Cmd {
	return tea.Tick(pollInterval, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// forwardTick / forwardSize call Update on a child screen and return its
// command, discarding the returned model — screens mutate in place because
// they are pointer receivers. The helpers keep the root Update readable.
func forwardTick(m tea.Model, msg tickMsg) tea.Cmd {
	_, cmd := m.Update(msg)
	return cmd
}

func forwardSize(m tea.Model, msg tea.WindowSizeMsg) tea.Cmd {
	_, cmd := m.Update(msg)
	return cmd
}
