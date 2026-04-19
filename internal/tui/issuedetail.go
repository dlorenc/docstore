package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/dlorenc/docstore/internal/model"
)

// issueDetailModel shows the full detail of a single issue.
type issueDetailModel struct {
	client      *tuiClient
	issue       *model.Issue
	comments    []model.IssueComment
	refs        []model.IssueRef
	loading     bool
	err         error
	goBack      bool
	quit        bool
	statusMsg   string

	showCommentOverlay bool
	commentOverlay     issueOverlayModel
	showCloseOverlay   bool
	closeOverlay       issueOverlayModel
}

func newIssueDetailModel(client *tuiClient, issue *model.Issue) issueDetailModel {
	return issueDetailModel{
		client:  client,
		issue:   issue,
		loading: true,
	}
}

func (m issueDetailModel) Init() tea.Cmd {
	return loadIssueDetail(m.client, m.issue.Number)
}

func (m issueDetailModel) Update(msg tea.Msg) (issueDetailModel, tea.Cmd) {
	if m.showCommentOverlay {
		switch msg := msg.(type) {
		case tea.KeyMsg:
			if msg.String() == "esc" {
				m.showCommentOverlay = false
				return m, nil
			}
			if msg.String() == "ctrl+c" {
				m.quit = true
				return m, tea.Quit
			}
		case issueCommentCreatedMsg:
			if msg.err == nil {
				m.showCommentOverlay = false
				m.loading = true
				return m, loadIssueDetail(m.client, m.issue.Number)
			}
		}
		var cmd tea.Cmd
		m.commentOverlay, cmd = m.commentOverlay.Update(msg)
		if m.commentOverlay.submitted {
			m.showCommentOverlay = false
			m.loading = true
			return m, loadIssueDetail(m.client, m.issue.Number)
		}
		return m, cmd
	}

	if m.showCloseOverlay {
		switch msg := msg.(type) {
		case tea.KeyMsg:
			if msg.String() == "esc" {
				m.showCloseOverlay = false
				return m, nil
			}
			if msg.String() == "ctrl+c" {
				m.quit = true
				return m, tea.Quit
			}
		case issueClosedMsg:
			if msg.err == nil {
				m.showCloseOverlay = false
				m.loading = true
				return m, loadIssueDetail(m.client, m.issue.Number)
			}
		}
		var cmd tea.Cmd
		m.closeOverlay, cmd = m.closeOverlay.Update(msg)
		if m.closeOverlay.submitted {
			m.showCloseOverlay = false
			m.loading = true
			return m, loadIssueDetail(m.client, m.issue.Number)
		}
		return m, cmd
	}

	switch msg := msg.(type) {
	case issueDetailLoadedMsg:
		m.loading = false
		m.err = msg.err
		if msg.err == nil {
			m.issue = msg.issue
			m.comments = msg.comments
			m.refs = msg.refs
		}

	case issueClosedMsg:
		if msg.err != nil {
			m.statusMsg = styleError.Render("Close failed: " + msg.err.Error())
		} else {
			m.loading = true
			return m, loadIssueDetail(m.client, m.issue.Number)
		}

	case issueReopenedMsg:
		if msg.err != nil {
			m.statusMsg = styleError.Render("Reopen failed: " + msg.err.Error())
		} else {
			m.loading = true
			return m, loadIssueDetail(m.client, m.issue.Number)
		}

	case tea.KeyMsg:
		switch msg.String() {
		case "esc", "b":
			m.goBack = true

		case "q", "ctrl+c":
			m.quit = true

		case "c":
			if m.issue != nil {
				m.showCommentOverlay = true
				m.commentOverlay = newIssueCommentOverlay(m.client, m.issue.Number)
				return m, m.commentOverlay.Init()
			}

		case "x":
			if m.issue != nil && m.issue.State == model.IssueStateOpen {
				m.showCloseOverlay = true
				m.closeOverlay = newIssueCloseOverlay(m.client, m.issue.Number)
				return m, m.closeOverlay.Init()
			}

		case "r":
			if m.issue != nil && m.issue.State == model.IssueStateClosed {
				return m, reopenIssueCmd(m.client, m.issue.Number)
			}
		}
	}
	return m, nil
}

func (m issueDetailModel) View() string {
	var sb strings.Builder

	if m.issue == nil {
		sb.WriteString("  Loading...\n")
		return sb.String()
	}

	stateStyled := ""
	if m.issue.State == model.IssueStateOpen {
		stateStyled = styleApproved.Render("[open]")
	} else {
		stateStyled = styleStale.Render("[closed]")
	}
	title := fmt.Sprintf("#%d  %s  %s", m.issue.Number, m.issue.Title, stateStyled)
	sb.WriteString(styleTitle.Render(title) + "\n")
	sb.WriteString(strings.Repeat("─", 60) + "\n")

	if m.loading {
		sb.WriteString("  Loading...\n")
	} else if m.err != nil {
		sb.WriteString(styleError.Render("  Error: "+m.err.Error()) + "\n")
	} else {
		sb.WriteString(fmt.Sprintf("  Author:  %s\n", m.issue.Author))
		sb.WriteString(fmt.Sprintf("  Created: %s\n", m.issue.CreatedAt.Format("2006-01-02 15:04:05")))
		if m.issue.CloseReason != nil {
			sb.WriteString(fmt.Sprintf("  Reason:  %s\n", string(*m.issue.CloseReason)))
		}
		if m.issue.ClosedBy != nil {
			sb.WriteString(fmt.Sprintf("  Closed by: %s\n", *m.issue.ClosedBy))
		}
		if len(m.issue.Labels) > 0 {
			sb.WriteString(fmt.Sprintf("  Labels:  %s\n", strings.Join(m.issue.Labels, ", ")))
		}

		if m.issue.Body != "" {
			sb.WriteString("\n")
			sb.WriteString(styleHeader.Render("  Description") + "\n")
			for _, line := range strings.Split(m.issue.Body, "\n") {
				sb.WriteString("  " + line + "\n")
			}
		}

		if len(m.comments) > 0 {
			sb.WriteString("\n")
			sb.WriteString(styleHeader.Render(fmt.Sprintf("  Comments (%d)", len(m.comments))) + "\n")
			sb.WriteString(strings.Repeat("─", 40) + "\n")
			for _, c := range m.comments {
				sb.WriteString(fmt.Sprintf("  %s  %s\n",
					styleModified.Render(c.Author),
					c.CreatedAt.Format("2006-01-02 15:04"),
				))
				for _, line := range strings.Split(c.Body, "\n") {
					sb.WriteString("    " + line + "\n")
				}
				sb.WriteString("\n")
			}
		}

		if len(m.refs) > 0 {
			sb.WriteString(styleHeader.Render(fmt.Sprintf("  References (%d)", len(m.refs))) + "\n")
			for _, ref := range m.refs {
				sb.WriteString(fmt.Sprintf("  %s  %s\n", string(ref.RefType), ref.RefID))
			}
		}
	}

	if m.statusMsg != "" {
		sb.WriteString("\n  " + m.statusMsg + "\n")
	}

	sb.WriteString("\n")
	if m.showCommentOverlay {
		sb.WriteString(styleBorder.Render(m.commentOverlay.View()))
		sb.WriteString("\n")
	} else if m.showCloseOverlay {
		sb.WriteString(styleBorder.Render(m.closeOverlay.View()))
		sb.WriteString("\n")
	} else {
		helpText := "  c comment · esc/b back"
		if m.issue != nil && !m.loading {
			if m.issue.State == model.IssueStateOpen {
				helpText = "  c comment · x close · esc/b back"
			} else {
				helpText = "  c comment · r reopen · esc/b back"
			}
		}
		sb.WriteString(styleHelp.Render(helpText))
	}

	return sb.String()
}
