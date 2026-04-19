package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// proposalOverlayModel is the modal for creating a new proposal.
type proposalOverlayModel struct {
	client      *tuiClient
	branch      string
	titleInput  textinput.Model
	descInput   textinput.Model
	focusDesc   bool // true when description input is focused
	submitting  bool
	err         error
	submitted   bool
}

func newProposalOverlay(client *tuiClient, branch string) proposalOverlayModel {
	ti := textinput.New()
	ti.Placeholder = "Proposal title (required)"
	ti.Focus()
	ti.Width = 48

	di := textinput.New()
	di.Placeholder = "Description (optional)"
	di.Width = 48

	return proposalOverlayModel{
		client:     client,
		branch:     branch,
		titleInput: ti,
		descInput:  di,
	}
}

func (m proposalOverlayModel) Init() tea.Cmd {
	return textinput.Blink
}

func (m proposalOverlayModel) Update(msg tea.Msg) (proposalOverlayModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			return m, nil

		case "tab":
			m.focusDesc = !m.focusDesc
			if m.focusDesc {
				m.titleInput.Blur()
				m.descInput.Focus()
			} else {
				m.descInput.Blur()
				m.titleInput.Focus()
			}
			return m, textinput.Blink

		case "enter":
			if !m.submitting && m.titleInput.Value() != "" {
				m.submitting = true
				return m, openProposal(m.client, m.branch, m.titleInput.Value(), m.descInput.Value())
			}
		}

	case proposalOpenedMsg:
		m.submitting = false
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.submitted = true
		}
		return m, nil
	}

	var cmd tea.Cmd
	if m.focusDesc {
		m.descInput, cmd = m.descInput.Update(msg)
	} else {
		m.titleInput, cmd = m.titleInput.Update(msg)
	}
	return m, cmd
}

func (m proposalOverlayModel) View() string {
	var sb strings.Builder

	sb.WriteString(styleTitle.Render("Open Proposal") + "\n\n")
	sb.WriteString("  Branch: " + m.branch + "\n\n")

	titleLabel := "  Title:       "
	descLabel := "  Description: "
	if !m.focusDesc {
		titleLabel = styleApproved.Render(titleLabel)
	}
	if m.focusDesc {
		descLabel = styleApproved.Render(descLabel)
	}

	sb.WriteString(titleLabel + m.titleInput.View() + "\n")
	sb.WriteString(descLabel + m.descInput.View() + "\n\n")

	if m.err != nil {
		sb.WriteString(styleError.Render("  Error: "+m.err.Error()) + "\n\n")
	}
	if m.submitting {
		sb.WriteString("  Opening proposal...\n")
	}

	sb.WriteString(styleHelp.Render("  tab toggle · enter submit · esc cancel"))
	return sb.String()
}
