package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
)

// reviewOverlayModel is the overlay for submitting a review.
type reviewOverlayModel struct {
	client     *tuiClient
	branch     string
	approved   bool // true = approve, false = reject
	textarea   textarea.Model
	submitting bool
	err        error
	submitted  bool
}

func newReviewOverlay(client *tuiClient, branch string) reviewOverlayModel {
	ta := textarea.New()
	ta.Placeholder = "Optional comment..."
	ta.SetWidth(48)
	ta.SetHeight(3)
	ta.Focus()
	return reviewOverlayModel{
		client:   client,
		branch:   branch,
		approved: true,
		textarea: ta,
	}
}

func (m reviewOverlayModel) Init() tea.Cmd {
	return textarea.Blink
}

func (m reviewOverlayModel) Update(msg tea.Msg) (reviewOverlayModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			m.submitted = false
			// Signal cancel — caller checks submitted
			return m, nil

		case "tab":
			m.approved = !m.approved
			return m, nil

		case "enter":
			if !m.submitting {
				m.submitting = true
				status := "rejected"
				if m.approved {
					status = "approved"
				}
				return m, submitReview(m.client, m.branch, status, m.textarea.Value())
			}

		default:
			var cmd tea.Cmd
			m.textarea, cmd = m.textarea.Update(msg)
			return m, cmd
		}

	case reviewSubmittedMsg:
		m.submitting = false
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.submitted = true
		}
	}
	return m, nil
}

func (m reviewOverlayModel) View() string {
	var sb strings.Builder

	sb.WriteString(styleTitle.Render("Submit Review") + "\n\n")

	approveLabel := "( ) Approve"
	rejectLabel := "( ) Reject"
	if m.approved {
		approveLabel = "(•) Approve"
	} else {
		rejectLabel = "(•) Reject"
	}

	sb.WriteString("  Status:  " + styleApproved.Render(approveLabel) + "  " + styleRejected.Render(rejectLabel) + "\n\n")
	sb.WriteString("  Comment (optional):\n")
	sb.WriteString("  " + m.textarea.View() + "\n\n")

	if m.err != nil {
		sb.WriteString(styleError.Render("  Error: "+m.err.Error()) + "\n\n")
	}
	if m.submitting {
		sb.WriteString("  Submitting...\n")
	}

	sb.WriteString(styleHelp.Render("  tab toggle · enter submit · esc cancel"))
	return sb.String()
}
