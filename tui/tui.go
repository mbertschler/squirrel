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
	screenHooks
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

// rootModel is the top-level Bubble Tea model. It owns the per-screen
// models, the terminal size, and the status bar; the SQLite store, config,
// and agent client are passed down to whichever screens need them at
// construction time rather than parked on the root.
type rootModel struct {
	width, height int
	active        screen

	// Status bar text. Cleared on the next tick after it was set, so transient
	// errors stay visible long enough to read without sticking forever.
	status        string
	statusSetTick int

	dashboard *dashboardModel
	runs      *runsModel
	volumes   *volumesModel
	hooks     *hooksModel
	browse    *browseModel
}

func newRootModel(s *store.Store, cfg *config.Config) *rootModel {
	client := newAgentClient(cfg)
	dash := newDashboardModel(s, cfg)
	dash.attachClient(client)
	return &rootModel{
		active:    screenDashboard,
		dashboard: dash,
		runs:      newRunsModel(s),
		volumes:   newVolumesModel(s),
		hooks:     newHooksModel(s),
		browse:    newBrowseModel(s),
	}
}

func (m *rootModel) Init() tea.Cmd {
	// Only the starting screen needs to fetch — the others load lazily on
	// first activation via switchTo. Matches the per-tick activation model
	// so we don't pay for off-screen screens here only to ignore them
	// later.
	return tea.Batch(tick(), m.activeScreen().Init())
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
		cmds = append(cmds, forwardSize(m.hooks, msg))
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
		// Only the active screen gets a tick. Forwarding to every screen
		// each second triples DB load needlessly (the store pins
		// MaxOpenConns(1), so off-screen tab queries serialize behind the
		// visible one). Background screens stay stale until reactivated;
		// the activate-on-switch path below kicks them with a fresh
		// fetch the moment the user does switch.
		return m, tea.Batch(tick(), forwardTick(m.activeScreen(), msg))

	case errMsg:
		m.status = msg.err.Error()
		m.statusSetTick = 5
		return m, nil

	case browseEnterVolumeMsg:
		m.browse.setVolume(msg.volumeID, msg.volumeName)
		m.active = screenBrowse
		return m, m.browse.Init()

	case tea.KeyMsg:
		// Global keys first. Modal screens get a chance to consume them via
		// modalConsumesKey so e.g. "esc" closes a Browse file-detail panel
		// before it would back out to the Volumes screen.
		if m.modalConsumesKey(msg.String()) {
			break // fall through to the per-screen delegation below
		}
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "q":
			if m.active == screenBrowse {
				// In Browse, q backs out to the Volumes list instead of quitting.
				return m, m.switchTo(screenVolumes)
			}
			return m, tea.Quit
		case "tab":
			return m, m.switchTo(m.nextTab(+1))
		case "shift+tab":
			return m, m.switchTo(m.nextTab(-1))
		case "1":
			return m, m.switchTo(screenDashboard)
		case "2":
			return m, m.switchTo(screenRuns)
		case "3":
			return m, m.switchTo(screenVolumes)
		case "4":
			return m, m.switchTo(screenHooks)
		case "esc":
			if m.active == screenBrowse {
				return m, m.switchTo(screenVolumes)
			}
		}
	}

	_, cmd := m.activeScreen().Update(msg)
	return m, cmd
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

	body := m.activeScreen().View()
	body = lipgloss.NewStyle().
		Width(m.width).
		Height(bodyHeight).
		Padding(0, 1).
		Render(body)

	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

// modalConsumesKey reports whether the active screen has a modal context
// (file-detail panel, run-detail panel) for which the given key should be
// delivered to the screen rather than handled globally. Without this,
// pressing esc inside a Browse file-detail panel would back the user out
// to the Volumes list instead of closing the panel.
func (m *rootModel) modalConsumesKey(key string) bool {
	switch key {
	case "esc", "q", "enter", "backspace":
	default:
		return false
	}
	switch m.active {
	case screenBrowse:
		return m.browse.fileDetail != nil
	case screenRuns:
		return m.runs.selectedDetail != nil
	}
	return false
}

// nextTab returns the screen `delta` steps away in the tab cycle.
// Browse is excluded because it is reached from Volumes, not from the
// tab bar.
func (m *rootModel) nextTab(delta int) screen {
	tabs := []screen{screenDashboard, screenRuns, screenVolumes, screenHooks}
	idx := 0
	for i, t := range tabs {
		if t == m.active {
			idx = i
			break
		}
	}
	return tabs[(idx+delta+len(tabs))%len(tabs)]
}

// switchTo activates the given screen and returns a Cmd that triggers a
// fresh data fetch on it. The immediate fetch on switch is necessary
// because off-screen tabs no longer receive periodic tickMsgs — without
// it the user would see stale data for up to one polling interval after
// switching.
func (m *rootModel) switchTo(s screen) tea.Cmd {
	if s == m.active {
		return nil
	}
	m.active = s
	return m.activeScreen().Init()
}

// activeScreen returns the tea.Model for whichever screen is currently
// focused. Used by tickMsg dispatch and switchTo so the routing logic
// stays in one place.
func (m *rootModel) activeScreen() tea.Model {
	switch m.active {
	case screenDashboard:
		return m.dashboard
	case screenRuns:
		return m.runs
	case screenVolumes:
		return m.volumes
	case screenHooks:
		return m.hooks
	case screenBrowse:
		return m.browse
	}
	return m.dashboard
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
		{screenHooks, "Hooks", "4"},
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
	left := "q quit · tab / shift-tab switch · 1-4 jump"
	if m.active == screenBrowse {
		left = "↑↓ navigate · enter descend · backspace ascend · esc / q back"
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
