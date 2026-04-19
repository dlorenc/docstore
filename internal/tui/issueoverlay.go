package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/dlorenc/docstore/internal/model"
)

type issueOverlayMode int

const (
	overlayCreateIssue issueOverlayMode = iota
	overlayCloseIssue
	overlayAddComment
)

// closeReasons lists the valid close reasons in display order.
var closeReasons = []model.IssueCloseReason{
	model.IssueCloseReasonCompleted,
	model.IssueCloseReasonNotPlanned,
	model.IssueCloseReasonDuplicate,
}

type issueFocus int

const (
	issueFocusTitle issueFocus = iota
	issueFocusBody
)

// issueOverlayModel handles create issue, close issue, and add comment overlays.
type issueOverlayModel struct {
	client      *tuiClient
	mode        issueOverlayMode
	issueNumber int64

	// create mode fields
	titleInput textinput.Model
	bodyArea   textarea.Model
	focus      issueFocus

	// close mode fields
	reasonIdx int

	submitting bool
	err        error
	submitted  bool
}

func newIssueCreateOverlay(client *tuiClient) issueOverlayModel {
	ti := textinput.New()
	ti.Placeholder = "Issue title (required)"
	ti.Focus()
	ti.Width = 48

	ta := textarea.New()
	ta.Placeholder = "Description (optional)"
	ta.SetWidth(48)
	ta.SetHeight(4)

	return issueOverlayModel{
		client:     client,
		mode:       overlayCreateIssue,
		titleInput: ti,
		bodyArea:   ta,
		focus:      issueFocusTitle,
	}
}

func newIssueCloseOverlay(client *tuiClient, issueNumber int64) issueOverlayModel {
	return issueOverlayModel{
		client:      client,
		mode:        overlayCloseIssue,
		issueNumber: issueNumber,
	}
}

func newIssueCommentOverlay(client *tuiClient, issueNumber int64) issueOverlayModel {
	ta := textarea.New()
	ta.Placeholder = "Comment body (required)"
	ta.SetWidth(48)
	ta.SetHeight(4)
	ta.Focus()

	return issueOverlayModel{
		client:      client,
		mode:        overlayAddComment,
		issueNumber: issueNumber,
		bodyArea:    ta,
	}
}

func (m issueOverlayModel) Init() tea.Cmd {
	switch m.mode {
	case overlayCreateIssue:
		return textinput.Blink
	case overlayAddComment:
		return textarea.Blink
	}
	return nil
}

func (m issueOverlayModel) Update(msg tea.Msg) (issueOverlayModel, tea.Cmd) {
	switch m.mode {
	case overlayCreateIssue:
		return m.updateCreate(msg)
	case overlayCloseIssue:
		return m.updateClose(msg)
	case overlayAddComment:
		return m.updateComment(msg)
	}
	return m, nil
}

func (m issueOverlayModel) updateCreate(msg tea.Msg) (issueOverlayModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			return m, nil

		case "tab":
			m.focus = (m.focus + 1) % 2
			m.titleInput.Blur()
			m.bodyArea.Blur()
			switch m.focus {
			case issueFocusTitle:
				m.titleInput.Focus()
				return m, textinput.Blink
			case issueFocusBody:
				m.bodyArea.Focus()
				return m, textarea.Blink
			}
			return m, nil

		case "enter":
			if m.focus == issueFocusBody {
				// Pass enter through to textarea so it inserts a newline.
				break
			}
			if !m.submitting && m.titleInput.Value() != "" {
				m.submitting = true
				return m, createIssueCmd(m.client, m.titleInput.Value(), m.bodyArea.Value())
			}
			return m, nil

		case "ctrl+enter":
			if m.focus == issueFocusBody && !m.submitting && m.titleInput.Value() != "" {
				m.submitting = true
				return m, createIssueCmd(m.client, m.titleInput.Value(), m.bodyArea.Value())
			}
			return m, nil
		}

	case issueCreatedMsg:
		m.submitting = false
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.submitted = true
		}
		return m, nil
	}

	var cmd tea.Cmd
	switch m.focus {
	case issueFocusTitle:
		m.titleInput, cmd = m.titleInput.Update(msg)
	case issueFocusBody:
		m.bodyArea, cmd = m.bodyArea.Update(msg)
	}
	return m, cmd
}

func (m issueOverlayModel) updateClose(msg tea.Msg) (issueOverlayModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			return m, nil

		case "j", "down":
			if m.reasonIdx < len(closeReasons)-1 {
				m.reasonIdx++
			}
			return m, nil

		case "k", "up":
			if m.reasonIdx > 0 {
				m.reasonIdx--
			}
			return m, nil

		case "enter":
			if !m.submitting {
				m.submitting = true
				return m, closeIssueCmd(m.client, m.issueNumber, closeReasons[m.reasonIdx])
			}
			return m, nil
		}

	case issueClosedMsg:
		m.submitting = false
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.submitted = true
		}
		return m, nil
	}
	return m, nil
}

func (m issueOverlayModel) updateComment(msg tea.Msg) (issueOverlayModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			return m, nil

		case "enter":
			if !m.submitting && m.bodyArea.Value() != "" {
				m.submitting = true
				return m, createIssueCommentCmd(m.client, m.issueNumber, m.bodyArea.Value())
			}
			return m, nil
		}

	case issueCommentCreatedMsg:
		m.submitting = false
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.submitted = true
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.bodyArea, cmd = m.bodyArea.Update(msg)
	return m, cmd
}

func (m issueOverlayModel) View() string {
	switch m.mode {
	case overlayCreateIssue:
		return m.viewCreate()
	case overlayCloseIssue:
		return m.viewClose()
	case overlayAddComment:
		return m.viewComment()
	}
	return ""
}

func (m issueOverlayModel) viewCreate() string {
	var sb strings.Builder

	sb.WriteString(styleTitle.Render("New Issue") + "\n\n")

	titleLabel := "  Title:  "
	if m.focus == issueFocusTitle {
		titleLabel = styleApproved.Render(titleLabel)
	}
	sb.WriteString(titleLabel + m.titleInput.View() + "\n\n")

	bodyLabel := "  Body:\n"
	if m.focus == issueFocusBody {
		bodyLabel = styleApproved.Render("  Body:\n")
	}
	sb.WriteString(bodyLabel)
	sb.WriteString("  " + m.bodyArea.View() + "\n\n")

	if m.err != nil {
		sb.WriteString(styleError.Render("  Error: "+m.err.Error()) + "\n\n")
	}
	if m.submitting {
		sb.WriteString("  Creating...\n")
	}

	if m.focus == issueFocusBody {
		sb.WriteString(styleHelp.Render("  tab toggle · ctrl+enter submit · esc cancel"))
	} else {
		sb.WriteString(styleHelp.Render("  tab toggle · enter submit · esc cancel"))
	}
	return sb.String()
}

func (m issueOverlayModel) viewClose() string {
	var sb strings.Builder

	sb.WriteString(styleTitle.Render("Close Issue") + "\n\n")
	sb.WriteString("  Select reason:\n\n")

	for i, r := range closeReasons {
		radio := "( ) "
		if i == m.reasonIdx {
			radio = "(•) "
		}
		label := "  " + radio + string(r)
		if i == m.reasonIdx {
			sb.WriteString(styleSelected.Render(label) + "\n")
		} else {
			sb.WriteString(label + "\n")
		}
	}

	sb.WriteString("\n")
	if m.err != nil {
		sb.WriteString(styleError.Render("  Error: "+m.err.Error()) + "\n\n")
	}
	if m.submitting {
		sb.WriteString("  Closing...\n")
	}

	sb.WriteString(styleHelp.Render("  j/k navigate · enter confirm · esc cancel"))
	return sb.String()
}

func (m issueOverlayModel) viewComment() string {
	var sb strings.Builder

	sb.WriteString(styleTitle.Render("Add Comment") + "\n\n")
	sb.WriteString("  " + m.bodyArea.View() + "\n\n")

	if m.err != nil {
		sb.WriteString(styleError.Render("  Error: "+m.err.Error()) + "\n\n")
	}
	if m.submitting {
		sb.WriteString("  Submitting...\n")
	}

	sb.WriteString(styleHelp.Render("  enter submit · esc cancel"))
	return sb.String()
}
