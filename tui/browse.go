package tui

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/dustin/go-humanize"

	"github.com/mbertschler/squirrel/store"
)

// browseModel is the ncdu-style folder traversal for a single volume. The
// volume is set by the root model on browseEnterVolumeMsg; the model then
// loads the volume root, lists subfolders + present files, and lets the
// user descend with enter / ascend with backspace.
//
// Two columns ("files", "size") are reserved for the future cumulative
// rollup work that lands with the folders-table migration in a separate
// PR. Today they render "—" for folder rows; file rows fill the size
// column with the file's own SizeBytes (which we already have) and the
// files column with "1" since a single file is one item.
type browseModel struct {
	store *store.Store

	width, height int

	volumeID   int64
	volumeName string

	current    store.Folder // the folder currently displayed
	atRoot     bool
	entries    []browseEntry
	loaded     bool
	loadErr    error
	fileDetail *store.FileRow // when non-nil, the file-detail panel is open

	table table.Model
}

// browseEntry is one row in the listing: either a subfolder or a present
// file at the current folder. fields are mutually exclusive — folder rows
// have folder != nil, file rows have file != nil.
type browseEntry struct {
	folder *store.Folder
	file   *store.FileRow
}

type browseDataMsg struct {
	folder  store.Folder
	atRoot  bool
	entries []browseEntry
	err     error
}

func newBrowseModel(s *store.Store) *browseModel {
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
	return &browseModel{store: s, table: t}
}

// setVolume seeds the screen with a freshly selected volume. Called by the
// root model when it receives browseEnterVolumeMsg. We reset all per-volume
// state so re-entering a different volume doesn't show stale rows from
// the previous one.
func (m *browseModel) setVolume(id int64, name string) {
	m.volumeID = id
	m.volumeName = name
	m.current = store.Folder{}
	m.entries = nil
	m.loaded = false
	m.loadErr = nil
	m.fileDetail = nil
}

func (m *browseModel) Init() tea.Cmd {
	if m.volumeID == 0 {
		return nil
	}
	return m.loadPath("")
}

func (m *browseModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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
		// Re-issue a load for the current path so live indexing changes
		// surface without the user manually re-navigating. Skipped while
		// the file-detail panel is open so the values the user is reading
		// don't move under them. Also skipped before the first load
		// completed — Init() handles that one.
		if m.fileDetail != nil || !m.loaded {
			return m, nil
		}
		return m, m.loadPath(m.current.Path)
	case browseDataMsg:
		m.loaded = true
		m.loadErr = msg.err
		if msg.err == nil {
			// Preserve cursor position across refreshes by remembering the
			// old selection's name and restoring it if it's still present.
			selectedName := m.currentSelectionName()
			m.current = msg.folder
			m.atRoot = msg.atRoot
			m.entries = msg.entries
			m.refreshRows()
			m.restoreSelection(selectedName)
		}
		return m, nil
	case tea.KeyMsg:
		if m.fileDetail != nil {
			switch msg.String() {
			case "esc", "backspace", "enter":
				m.fileDetail = nil
				return m, nil
			}
			return m, nil
		}
		switch msg.String() {
		case "enter", "right", "l":
			return m, m.activateSelection()
		case "backspace", "left", "h":
			if m.atRoot {
				return m, nil
			}
			parentPath := parentOf(m.current.Path)
			return m, m.loadPath(parentPath)
		case "g":
			m.table.GotoTop()
			return m, nil
		case "G":
			m.table.GotoBottom()
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m *browseModel) View() string {
	if m.volumeID == 0 {
		return styleMuted.Render("no volume selected — pick one on the Volumes tab")
	}
	if !m.loaded && m.loadErr == nil {
		return styleMuted.Render("loading…")
	}
	if m.loadErr != nil {
		return styleErr.Render(fmt.Sprintf("browse error: %v", m.loadErr))
	}
	if m.fileDetail != nil {
		return m.renderFileDetail(*m.fileDetail)
	}
	title := lipgloss.JoinHorizontal(lipgloss.Top,
		styleHeader.Render(m.volumeName),
		styleMuted.Render("  "+m.displayPath()),
	)
	footer := styleMuted.Render("↑↓ navigate · enter descend · backspace ascend · g/G top/bottom · esc back to volumes")
	return title + "\n" + m.table.View() + "\n" + footer
}

func (m *browseModel) displayPath() string {
	if m.current.Path == "" {
		return "/"
	}
	return "/" + m.current.Path
}

func (m *browseModel) refreshRows() {
	rows := make([]table.Row, 0, len(m.entries)+1)
	if !m.atRoot {
		rows = append(rows, table.Row{
			"📁",
			"..",
			styleMuted.Render("—"),
			styleMuted.Render("—"),
		})
	}
	for _, e := range m.entries {
		if e.folder != nil {
			rows = append(rows, table.Row{
				"📁",
				folderDisplayName(e.folder.Path) + "/",
				// Both columns are placeholders until the cumulative-rollup
				// migration lands. Reserved here so the layout is stable.
				styleMuted.Render("—"),
				styleMuted.Render("—"),
			})
			continue
		}
		rows = append(rows, table.Row{
			"  ",
			path.Base(e.file.Path),
			"1",
			humanize.IBytes(uint64(e.file.SizeBytes)),
		})
	}
	m.table.SetRows(rows)
	m.resizeColumns()
}

func (m *browseModel) renderFileDetail(f store.FileRow) string {
	now := time.Now()
	lines := [][2]string{
		{"Path", "/" + f.Path},
		{"Size", fmt.Sprintf("%s  (%d bytes)", humanize.IBytes(uint64(f.SizeBytes)), f.SizeBytes)},
		{"Blake3", hexShort(f.Blake3, 32)},
		{"Status", lipgloss.NewStyle().Foreground(statusColour(f.Status)).Render(f.Status)},
		{"Mtime", time.Unix(0, f.MtimeNs).Format(time.RFC3339)},
		{"Indexed", fmt.Sprintf("%s  (%s ago)",
			time.Unix(0, f.IndexedAtNs).Format(time.RFC3339),
			humanDuration(now.Sub(time.Unix(0, f.IndexedAtNs))),
		)},
		{"First seen", fmt.Sprintf("run #%d", f.FirstSeenRunID)},
		{"Last seen", fmt.Sprintf("run #%d", f.LastSeenRunID)},
	}
	if f.OriginRunID.Valid {
		lines = append(lines, [2]string{"Origin run", fmt.Sprintf("#%d", f.OriginRunID.Int64)})
	}
	if f.OriginNodeID.Valid {
		lines = append(lines, [2]string{"Origin node", fmt.Sprintf("#%d", f.OriginNodeID.Int64)})
	}
	var b strings.Builder
	b.WriteString(styleHeader.Render(path.Base(f.Path)))
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

func (m *browseModel) activateSelection() tea.Cmd {
	idx := m.table.Cursor()
	if idx < 0 {
		return nil
	}
	if !m.atRoot {
		// Account for the synthesized ".." entry.
		if idx == 0 {
			return m.loadPath(parentOf(m.current.Path))
		}
		idx--
	}
	if idx >= len(m.entries) {
		return nil
	}
	e := m.entries[idx]
	if e.folder != nil {
		return m.loadPath(e.folder.Path)
	}
	// File rows open the detail panel instead of descending. Avoids the
	// "enter does nothing on a file" UX papercut.
	file := *e.file
	m.fileDetail = &file
	return nil
}

// currentSelectionName returns the display name of the currently highlighted
// row (subfolder or file), or "" when nothing is selected. Used to anchor
// the cursor across tick-driven refreshes.
func (m *browseModel) currentSelectionName() string {
	idx := m.table.Cursor()
	if idx < 0 {
		return ""
	}
	if !m.atRoot {
		if idx == 0 {
			return ".."
		}
		idx--
	}
	if idx >= len(m.entries) {
		return ""
	}
	e := m.entries[idx]
	if e.folder != nil {
		return folderDisplayName(e.folder.Path)
	}
	return path.Base(e.file.Path)
}

// restoreSelection moves the table cursor to the row whose display name
// matches name. No-op when name is empty or absent — the table already
// defaults to row 0 in that case.
func (m *browseModel) restoreSelection(name string) {
	if name == "" {
		return
	}
	rows := m.table.Rows()
	for i, r := range rows {
		if len(r) >= 2 && (r[1] == name || r[1] == name+"/") {
			m.table.SetCursor(i)
			return
		}
	}
}

func (m *browseModel) resizeColumns() {
	cols := []table.Column{
		{Title: "", Width: 2},
		{Title: "NAME", Width: 32},
		{Title: "FILES", Width: 8},
		{Title: "SIZE", Width: 12},
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

// loadPath issues the SQL queries that back this folder view. The path is
// volume-relative without a leading slash; the volume root is "".
func (m *browseModel) loadPath(folderPath string) tea.Cmd {
	volID := m.volumeID
	s := m.store
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		folder, err := s.GetFolderByPath(ctx, volID, folderPath)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return browseDataMsg{err: fmt.Errorf("folder %q has not been indexed yet", folderPath)}
			}
			return browseDataMsg{err: err}
		}
		children, err := s.ListChildFolders(ctx, folder.ID)
		if err != nil {
			return browseDataMsg{err: err}
		}
		files, err := s.ListPresentFilesInFolder(ctx, folder.ID)
		if err != nil {
			return browseDataMsg{err: err}
		}
		entries := make([]browseEntry, 0, len(children)+len(files))
		for i := range children {
			c := children[i]
			entries = append(entries, browseEntry{folder: &c})
		}
		for i := range files {
			f := files[i]
			entries = append(entries, browseEntry{file: &f})
		}
		return browseDataMsg{
			folder:  folder,
			atRoot:  folderPath == "",
			entries: entries,
		}
	}
}

// folderDisplayName extracts the last path component for display. Mirrors
// store.Folder.Name() but doesn't need a Folder value.
func folderDisplayName(p string) string {
	if p == "" {
		return ""
	}
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// parentOf returns the parent path of p in the volume's path space. The
// root is the empty string; the parent of any top-level entry is the root.
func parentOf(p string) string {
	if p == "" {
		return ""
	}
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}

// hexShort renders the first n hex digits of b plus an ellipsis. Used in
// the file-detail panel to keep the 64-character blake3 from dominating
// the layout on a narrow terminal.
func hexShort(b []byte, n int) string {
	const hex = "0123456789abcdef"
	if len(b)*2 <= n {
		out := make([]byte, len(b)*2)
		for i, x := range b {
			out[2*i] = hex[x>>4]
			out[2*i+1] = hex[x&0x0f]
		}
		return string(out)
	}
	bytesShown := (n + 1) / 2
	out := make([]byte, bytesShown*2)
	for i := 0; i < bytesShown; i++ {
		out[2*i] = hex[b[i]>>4]
		out[2*i+1] = hex[b[i]&0x0f]
	}
	return string(out) + "…"
}
