package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// focus tracks which input field is active in the proposal overlay.
type proposalFocus int

const (
	focusTitle proposalFocus = iota
	focusDesc
	focusBase
)

// proposalOverlayModel is the modal for creating a new proposal.
type proposalOverlayModel struct {
	client          *tuiClient
	branch          string
	titleInput      textinput.Model
	descInput       textinput.Model
	baseBranchInput textinput.Model
	focus           proposalFocus
	submitting      bool
	err             error
	submitted       bool
}

func newProposalOverlay(client *tuiClient, branch string) proposalOverlayModel {
	ti := textinput.New()
	ti.Placeholder = "Proposal title (required)"
	ti.Focus()
	ti.Width = 48

	di := textinput.New()
	di.Placeholder = "Description (optional)"
	di.Width = 48

	bi := textinput.New()
	bi.Placeholder = "Base branch"
	bi.SetValue("main")
	bi.Width = 48

	return proposalOverlayModel{
		client:          client,
		branch:          branch,
		titleInput:      ti,
		descInput:       di,
		baseBranchInput: bi,
		focus:           focusTitle,
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
			m.focus = (m.focus + 1) % 3
			m.titleInput.Blur()
			m.descInput.Blur()
			m.baseBranchInput.Blur()
			switch m.focus {
			case focusTitle:
				m.titleInput.Focus()
			case focusDesc:
				m.descInput.Focus()
			case focusBase:
				m.baseBranchInput.Focus()
			}
			return m, textinput.Blink

		case "enter":
			if !m.submitting && m.titleInput.Value() != "" {
				baseBranch := m.baseBranchInput.Value()
				if baseBranch == "" {
					baseBranch = "main"
				}
				m.submitting = true
				return m, openProposal(m.client, m.branch, m.titleInput.Value(), m.descInput.Value(), baseBranch)
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
	switch m.focus {
	case focusTitle:
		m.titleInput, cmd = m.titleInput.Update(msg)
	case focusDesc:
		m.descInput, cmd = m.descInput.Update(msg)
	case focusBase:
		m.baseBranchInput, cmd = m.baseBranchInput.Update(msg)
	}
	return m, cmd
}

func (m proposalOverlayModel) View() string {
	var sb strings.Builder

	sb.WriteString(styleTitle.Render("Open Proposal") + "\n\n")
	sb.WriteString("  Branch: " + m.branch + "\n\n")

	titleLabel := "  Title:        "
	descLabel := "  Description:  "
	baseLabel := "  Base branch:  "
	if m.focus == focusTitle {
		titleLabel = styleApproved.Render(titleLabel)
	}
	if m.focus == focusDesc {
		descLabel = styleApproved.Render(descLabel)
	}
	if m.focus == focusBase {
		baseLabel = styleApproved.Render(baseLabel)
	}

	sb.WriteString(titleLabel + m.titleInput.View() + "\n")
	sb.WriteString(descLabel + m.descInput.View() + "\n")
	sb.WriteString(baseLabel + m.baseBranchInput.View() + "\n\n")

	if m.err != nil {
		sb.WriteString(styleError.Render("  Error: "+m.err.Error()) + "\n\n")
	}
	if m.submitting {
		sb.WriteString("  Opening proposal...\n")
	}

	sb.WriteString(styleHelp.Render("  tab toggle · enter submit · esc cancel"))
	return sb.String()
}
