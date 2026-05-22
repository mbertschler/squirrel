package tui

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/mbertschler/squirrel/store"
)

// whenAgo formats a nullable nanosecond timestamp as "5m ago", "2h ago",
// "3d ago", or "—" if not set. Used by the dashboard and runs screens. We
// don't pull in a calendar library — these are status-bar values, not
// audit-grade timestamps.
func whenAgo(ns sql.NullInt64, now time.Time) string {
	if !ns.Valid {
		return styleMuted.Render("—")
	}
	d := now.Sub(time.Unix(0, ns.Int64))
	return humanDuration(d) + " ago"
}

// elapsedSince formats how long a run has been going for, given its
// started_at_ns. Used by the active-runs panel which doesn't want " ago".
func elapsedSince(startedNs int64, now time.Time) string {
	if startedNs == 0 {
		return styleMuted.Render("—")
	}
	d := now.Sub(time.Unix(0, startedNs))
	return humanDuration(d)
}

// humanDuration renders a time.Duration in the most useful unit. The
// thresholds match ncdu / git log defaults: seconds < 60, minutes < 60,
// hours < 24, days otherwise. Negative durations (clock skew) are clamped
// to "0s".
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		mins := int(d / time.Minute)
		secs := int(d/time.Second) - mins*60
		if secs == 0 {
			return fmt.Sprintf("%dm", mins)
		}
		return fmt.Sprintf("%dm %ds", mins, secs)
	case d < 24*time.Hour:
		hrs := int(d / time.Hour)
		mins := int(d/time.Minute) - hrs*60
		if mins == 0 {
			return fmt.Sprintf("%dh", hrs)
		}
		return fmt.Sprintf("%dh %dm", hrs, mins)
	default:
		days := int(d / (24 * time.Hour))
		hrs := int(d/time.Hour) - days*24
		if hrs == 0 {
			return fmt.Sprintf("%dd", days)
		}
		return fmt.Sprintf("%dd %dh", days, hrs)
	}
}

// runDuration returns a human-readable elapsed-time for a Run, "—" if it
// hasn't ended (the active-runs panel uses elapsedSince instead).
func runDuration(r store.Run) string {
	if !r.EndedAtNs.Valid {
		return styleMuted.Render("—")
	}
	return humanDuration(time.Duration(r.EndedAtNs.Int64 - r.StartedAtNs))
}

// nullStr unwraps a sql.NullString to a printable value, returning a muted
// em-dash for NULL. Saves repetitive .Valid checks at every render site.
func nullStr(s sql.NullString) string {
	if !s.Valid || s.String == "" {
		return styleMuted.Render("—")
	}
	return s.String
}

// glyphForStatus maps run status to a one-character indicator. Kept
// minimal because the colour does most of the work — the glyph is the
// fallback for users on terminals without colour.
func glyphForStatus(status string) string {
	switch status {
	case "success":
		return "✓"
	case "failed":
		return "✗"
	case "partial":
		return "~"
	case "running":
		return "●"
	default:
		return "·"
	}
}

// renderTable renders a 2D string grid with column alignment. The first row
// is treated as a header; columns are padded to the max width in the
// column. perColColour, if non-nil, paints the given colour onto every
// non-header cell of that column.
func renderTable(rows [][]string, perColColour []lipgloss.Color) string {
	return renderTableColoured(rows, perColColour, nil)
}

// renderTableColoured is the workhorse — perCell, if non-nil, can override
// the per-column colour on a cell-by-cell basis. Returns an empty string
// when rows is empty.
func renderTableColoured(
	rows [][]string,
	perColColour []lipgloss.Color,
	perCell func(rowIdx, colIdx int) lipgloss.Color,
) string {
	if len(rows) == 0 {
		return ""
	}
	cols := len(rows[0])
	widths := make([]int, cols)
	for _, r := range rows {
		for i, cell := range r {
			if i >= cols {
				break
			}
			w := lipgloss.Width(cell)
			if w > widths[i] {
				widths[i] = w
			}
		}
	}
	var b strings.Builder
	for ri, r := range rows {
		for i, cell := range r {
			if i >= cols {
				break
			}
			padded := padRight(cell, widths[i])
			if ri == 0 {
				padded = styleHeader.Render(padded)
			} else {
				var c lipgloss.Color
				if perCell != nil {
					c = perCell(ri, i)
				}
				if c == "" && i < len(perColColour) {
					c = perColColour[i]
				}
				if c != "" {
					padded = lipgloss.NewStyle().Foreground(c).Render(padded)
				}
			}
			b.WriteString(padded)
			if i < cols-1 {
				b.WriteString("  ")
			}
		}
		if ri < len(rows)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// padRight pads s with spaces on the right to reach width measured in
// display cells. Strings already at or above width are returned unchanged.
func padRight(s string, width int) string {
	gap := width - lipgloss.Width(s)
	if gap <= 0 {
		return s
	}
	return s + strings.Repeat(" ", gap)
}
