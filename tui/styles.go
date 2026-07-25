package tui

import "github.com/charmbracelet/lipgloss"

// Styles are the shared lip gloss styles used across screens. Keeping them
// in one place lets the colour palette be tuned without hunting through the
// individual screen files. The TUI assumes a 256-colour terminal; on a 16-
// colour terminal lipgloss degrades gracefully.
var (
	colourAccent  = lipgloss.Color("39")  // cyan-ish
	colourMuted   = lipgloss.Color("244") // grey
	colourSuccess = lipgloss.Color("42")  // green
	colourWarning = lipgloss.Color("214") // amber
	colourFailure = lipgloss.Color("196") // red
	colourRunning = lipgloss.Color("39")  // cyan-ish, same as accent

	styleTitle = lipgloss.NewStyle().Bold(true).Foreground(colourAccent)
	styleMuted = lipgloss.NewStyle().Foreground(colourMuted)
	styleErr   = lipgloss.NewStyle().Foreground(colourFailure)

	styleTab = lipgloss.NewStyle().
			Padding(0, 2).
			Foreground(colourMuted)
	styleTabActive = styleTab.
			Foreground(lipgloss.Color("231")).
			Background(colourAccent).
			Bold(true)

	styleStatusBar = lipgloss.NewStyle().
			Foreground(colourMuted).
			Padding(0, 1)

	styleHeader = lipgloss.NewStyle().Bold(true).Underline(true)
)

// statusColour returns the foreground colour for a run status.
func statusColour(status string) lipgloss.Color {
	switch status {
	case "success":
		return colourSuccess
	case "failed":
		return colourFailure
	case "partial":
		return colourWarning
	case "running":
		return colourRunning
	case "refused":
		// A refusal is a fail-closed safety gate; it must read as red so a
		// month-dead backup disk produces visible red, not a muted dot.
		return colourFailure
	case "aborted":
		// A reaped run never completed but did not fail; amber keeps it out
		// of the green "you may close the laptop" set without crying failure.
		return colourWarning
	default:
		return colourMuted
	}
}
