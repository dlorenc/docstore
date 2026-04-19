package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// branchListModel is the home screen showing all active branches.
type branchListModel struct {
	client     *tuiClient
	branches   []branchSummary
	cursor     int
	loading    bool
	err        error
	openBranch string
	quit       bool

	refreshing    bool
	lastRefreshed time.Time
}

func newBranchListModel(client *tuiClient) branchListModel {
	return branchListModel{
		client:  client,
		loading: true,
	}
}

func (m branchListModel) Init() tea.Cmd {
	m.loading = true
	return loadBranches(m.client)
}

func (m branchListModel) Update(msg tea.Msg) (branchListModel, tea.Cmd) {
	switch msg := msg.(type) {
	case branchesLoadedMsg:
		m.loading = false
		m.refreshing = false
		m.lastRefreshed = time.Now()
		m.err = msg.err
		m.branches = msg.branches
		if m.cursor >= len(m.branches) {
			m.cursor = 0
		}

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.quit = true

		case "j", "down":
			if m.cursor < len(m.branches)-1 {
				m.cursor++
			}

		case "k", "up":
			if m.cursor > 0 {
				m.cursor--
			}

		case "enter":
			if len(m.branches) > 0 {
				m.openBranch = m.branches[m.cursor].branch.Name
			}

		case "R":
			m.loading = true
			m.refreshing = true
			m.err = nil
			return m, loadBranches(m.client)
		}
	}
	return m, nil
}

func (m branchListModel) View() string {
	var sb strings.Builder

	repoLabel := styleTitle.Render("DocStore — " + m.client.repo)
	sb.WriteString(repoLabel + "\n\n")

	if m.loading {
		sb.WriteString("  Loading...\n")
	} else if m.err != nil {
		sb.WriteString(styleError.Render("  Error: "+m.err.Error()) + "\n")
	} else if len(m.branches) == 0 {
		sb.WriteString("  No active branches.\n")
	} else {
		// Header.
		header := fmt.Sprintf("  %-30s %-5s %-5s %-20s %-5s %-5s",
			"BRANCH", "HEAD", "BASE", "AUTHOR", "REV", "CI")
		sb.WriteString(styleHeader.Render(header) + "\n")

		for i, s := range m.branches {
			b := s.branch
			rev := formatRevSummary(s.approved, s.rejected)
			ci := formatCISummary(s.passed, s.failed, s.pending)

			line := fmt.Sprintf("%-30s %-5d %-5d %-20s %-5s %-5s",
				truncate(b.Name, 30), b.HeadSequence, b.BaseSequence, "", rev, ci)

			prefix := "  "
			if i == m.cursor {
				sb.WriteString(styleSelected.Render(prefix+line) + "\n")
			} else {
				sb.WriteString(prefix + line + "\n")
			}
		}
	}

	sb.WriteString("\n")
	if m.refreshing {
		sb.WriteString(styleHelp.Render("  j/k navigate · enter open · R refresh · q quit  ↻ refreshing…"))
	} else if !m.lastRefreshed.IsZero() {
		sb.WriteString(styleHelp.Render("  j/k navigate · enter open · R refresh · q quit  · last refreshed at " + m.lastRefreshed.Format("15:04:05")))
	} else {
		sb.WriteString(styleHelp.Render("  j/k navigate · enter open · R refresh · q quit"))
	}

	return sb.String()
}

func formatRevSummary(approved, rejected int) string {
	if approved == 0 && rejected == 0 {
		return "—"
	}
	if rejected > 0 {
		return styleRejected.Render(fmt.Sprintf("✗%d", rejected))
	}
	return styleApproved.Render(fmt.Sprintf("✓%d", approved))
}

func formatCISummary(passed, failed, pending int) string {
	if passed == 0 && failed == 0 && pending == 0 {
		return "—"
	}
	if failed > 0 {
		return styleFailed.Render(fmt.Sprintf("✗%d", failed))
	}
	if pending > 0 {
		return stylePending.Render(fmt.Sprintf("○%d", pending))
	}
	return stylePassed.Render(fmt.Sprintf("✓%d", passed))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
