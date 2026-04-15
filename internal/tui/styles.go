package tui

import "github.com/charmbracelet/lipgloss"

var (
	styleTitle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("15"))

	styleBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("63"))

	styleSelected = lipgloss.NewStyle().
			Background(lipgloss.Color("63")).
			Foreground(lipgloss.Color("15"))

	styleHeader = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("11"))

	styleHelp = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8"))

	styleAdded = lipgloss.NewStyle().
			Foreground(lipgloss.Color("10")) // green

	styleRemoved = lipgloss.NewStyle().
			Foreground(lipgloss.Color("9")) // red

	styleModified = lipgloss.NewStyle().
			Foreground(lipgloss.Color("11")) // yellow

	styleStale = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8")) // gray

	styleApproved = lipgloss.NewStyle().
			Foreground(lipgloss.Color("10")) // green

	styleRejected = lipgloss.NewStyle().
			Foreground(lipgloss.Color("9")) // red

	stylePassed = lipgloss.NewStyle().
			Foreground(lipgloss.Color("10"))

	styleFailed = lipgloss.NewStyle().
			Foreground(lipgloss.Color("9"))

	stylePending = lipgloss.NewStyle().
			Foreground(lipgloss.Color("11"))

	styleTabActive = lipgloss.NewStyle().
			Bold(true).
			Underline(true).
			Foreground(lipgloss.Color("63"))

	styleTabInactive = lipgloss.NewStyle().
				Foreground(lipgloss.Color("8"))

	styleError = lipgloss.NewStyle().
			Foreground(lipgloss.Color("9"))

	styleConfirm = lipgloss.NewStyle().
			Foreground(lipgloss.Color("11")).
			Bold(true)
)
