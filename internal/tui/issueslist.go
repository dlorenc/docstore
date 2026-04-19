package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/dlorenc/docstore/internal/model"
)

type issueFilter int

const (
	filterOpen issueFilter = iota
	filterClosed
	filterAll
)

func (f issueFilter) stateParam() string {
	switch f {
	case filterClosed:
		return "closed"
	case filterAll:
		return ""
	default:
		return "open"
	}
}

// issueListModel is the issue list screen.
type issueListModel struct {
	client    *tuiClient
	issues    []model.Issue
	filter    issueFilter
	cursor    int
	loading   bool
	err       error
	openIssue *model.Issue
	goBack    bool
	quit      bool

	showCreateOverlay bool
	createOverlay     issueOverlayModel
}

func newIssueListModel(client *tuiClient) issueListModel {
	return issueListModel{
		client:  client,
		filter:  filterOpen,
		loading: true,
	}
}

func (m issueListModel) Init() tea.Cmd {
	return loadIssues(m.client, m.filter.stateParam())
}

func (m issueListModel) Update(msg tea.Msg) (issueListModel, tea.Cmd) {
	if m.showCreateOverlay {
		switch msg := msg.(type) {
		case tea.KeyMsg:
			if msg.String() == "esc" {
				m.showCreateOverlay = false
				return m, nil
			}
			if msg.String() == "ctrl+c" {
				m.quit = true
				return m, tea.Quit
			}
		case issueCreatedMsg:
			if msg.err == nil {
				m.showCreateOverlay = false
				m.loading = true
				return m, loadIssues(m.client, m.filter.stateParam())
			}
		}
		var cmd tea.Cmd
		m.createOverlay, cmd = m.createOverlay.Update(msg)
		if m.createOverlay.submitted {
			m.showCreateOverlay = false
			m.loading = true
			return m, loadIssues(m.client, m.filter.stateParam())
		}
		return m, cmd
	}

	switch msg := msg.(type) {
	case issuesLoadedMsg:
		m.loading = false
		m.err = msg.err
		m.issues = msg.issues
		if m.cursor >= len(m.issues) {
			m.cursor = 0
		}

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.quit = true

		case "i", "esc":
			m.goBack = true

		case "j", "down":
			if m.cursor < len(m.issues)-1 {
				m.cursor++
			}

		case "k", "up":
			if m.cursor > 0 {
				m.cursor--
			}

		case "enter":
			if len(m.issues) > 0 {
				issue := m.issues[m.cursor]
				m.openIssue = &issue
			}

		case "tab":
			m.filter = (m.filter + 1) % 3
			m.loading = true
			m.cursor = 0
			return m, loadIssues(m.client, m.filter.stateParam())

		case "n":
			m.showCreateOverlay = true
			m.createOverlay = newIssueCreateOverlay(m.client)
			return m, m.createOverlay.Init()
		}
	}
	return m, nil
}

func (m issueListModel) View() string {
	var sb strings.Builder

	sb.WriteString(styleTitle.Render("Issues — "+m.client.repo) + "\n\n")

	// Filter tabs.
	filterLabels := []string{"Open", "Closed", "All"}
	parts := make([]string, len(filterLabels))
	for i, label := range filterLabels {
		if issueFilter(i) == m.filter {
			parts[i] = styleTabActive.Render(label)
		} else {
			parts[i] = styleTabInactive.Render(label)
		}
	}
	sb.WriteString(" " + strings.Join(parts, "  ") + "\n")
	sb.WriteString(strings.Repeat("─", 60) + "\n")

	if m.loading {
		sb.WriteString("  Loading...\n")
	} else if m.err != nil {
		sb.WriteString(styleError.Render("  Error: "+m.err.Error()) + "\n")
	} else if len(m.issues) == 0 {
		sb.WriteString("  No " + filterLabels[m.filter] + " issues.\n")
	} else {
		header := fmt.Sprintf("  %-5s %-40s %-15s", "#", "TITLE", "AUTHOR")
		sb.WriteString(styleHeader.Render(header) + "\n")

		for i, iss := range m.issues {
			stateTag := ""
			if m.filter == filterAll {
				if iss.State == model.IssueStateOpen {
					stateTag = styleApproved.Render("[open]  ")
				} else {
					stateTag = styleStale.Render("[closed]")
				}
			}
			line := fmt.Sprintf("%-5d %-40s %-15s",
				iss.Number,
				truncate(iss.Title, 40),
				truncate(iss.Author, 15),
			)
			prefix := "  "
			if m.filter == filterAll {
				line = stateTag + " " + line
			}
			if i == m.cursor {
				sb.WriteString(styleSelected.Render(prefix+line) + "\n")
			} else {
				sb.WriteString(prefix + line + "\n")
			}
		}
	}

	sb.WriteString("\n")
	if m.showCreateOverlay {
		sb.WriteString(styleBorder.Render(m.createOverlay.View()))
		sb.WriteString("\n")
	} else {
		sb.WriteString(styleHelp.Render("  j/k navigate · enter open · tab filter · n new · i/esc back · q quit"))
	}

	return sb.String()
}
