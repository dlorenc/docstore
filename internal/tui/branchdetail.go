package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/dlorenc/docstore/internal/model"
)

// panel identifies which panel is active in the branch detail view.
type panel int

const (
	panelDiff panel = iota
	panelReviews
	panelChecks
)

// branchDetailModel shows diff, reviews, and checks for a specific branch.
type branchDetailModel struct {
	client     *tuiClient
	branchName string

	loading bool
	err     error

	diff          *model.DiffResponse
	reviews       []model.Review
	checks        []model.CheckRun
	headSeq       int64
	baseSeq       int64
	baseTreePaths map[string]bool

	activePanel    panel
	diffCursor     int
	expandedFiles  map[int]bool
	diffFileHunks  []string // raw diff content per file (simplified)

	// Merge confirmation state.
	merging        bool
	mergeConfirm   bool // waiting for y/N
	mergeMessage   string

	// Review overlay.
	showReviewOverlay bool
	reviewOverlay     reviewOverlayModel

	goBack bool
	quit   bool
}

func newBranchDetailModel(client *tuiClient, branchName string) branchDetailModel {
	return branchDetailModel{
		client:        client,
		branchName:    branchName,
		loading:       true,
		expandedFiles: make(map[int]bool),
	}
}

func (m branchDetailModel) Init() tea.Cmd {
	return loadBranchDetail(m.client, m.branchName)
}

func (m branchDetailModel) Update(msg tea.Msg) (branchDetailModel, tea.Cmd) {
	// If review overlay is active, delegate to it.
	if m.showReviewOverlay {
		switch msg := msg.(type) {
		case tea.KeyMsg:
			if msg.String() == "esc" {
				m.showReviewOverlay = false
				return m, nil
			}
			if msg.String() == "ctrl+c" {
				m.quit = true
				return m, tea.Quit
			}
		case reviewSubmittedMsg:
			if msg.err == nil {
				m.showReviewOverlay = false
				// Refresh reviews panel.
				m.loading = true
				return m, loadBranchDetail(m.client, m.branchName)
			}
		}
		var cmd tea.Cmd
		m.reviewOverlay, cmd = m.reviewOverlay.Update(msg)
		if m.reviewOverlay.submitted {
			m.showReviewOverlay = false
			m.loading = true
			return m, loadBranchDetail(m.client, m.branchName)
		}
		return m, cmd
	}

	// Merge confirmation state.
	if m.mergeConfirm {
		switch msg := msg.(type) {
		case tea.KeyMsg:
			switch msg.String() {
			case "y", "Y":
				m.mergeConfirm = false
				m.merging = true
				m.mergeMessage = ""
				return m, mergeBranch(m.client, m.branchName)
			default:
				m.mergeConfirm = false
				m.mergeMessage = "Merge cancelled."
				return m, nil
			}
		case mergeResultMsg:
			m.merging = false
			if msg.err != nil {
				m.mergeMessage = styleError.Render("Merge failed: " + msg.err.Error())
			} else if len(msg.conflicts) > 0 {
				paths := make([]string, len(msg.conflicts))
				for i, c := range msg.conflicts {
					paths[i] = c.Path
				}
				m.mergeMessage = styleError.Render("Conflicts: " + strings.Join(paths, ", "))
			} else {
				m.mergeMessage = styleApproved.Render(fmt.Sprintf("Merged into main at sequence %d.", msg.sequence))
			}
			return m, nil
		}
		return m, nil
	}

	switch msg := msg.(type) {
	case branchDetailLoadedMsg:
		m.loading = false
		m.err = msg.err
		if msg.err == nil {
			m.diff = msg.diff
			m.reviews = msg.reviews
			m.checks = msg.checks
			m.headSeq = msg.headSeq
			m.baseSeq = msg.baseSeq
			m.baseTreePaths = msg.baseTreePaths
		}

	case mergeResultMsg:
		m.merging = false
		if msg.err != nil {
			m.mergeMessage = styleError.Render("Merge failed: " + msg.err.Error())
		} else if len(msg.conflicts) > 0 {
			paths := make([]string, len(msg.conflicts))
			for i, c := range msg.conflicts {
				paths[i] = c.Path
			}
			m.mergeMessage = styleError.Render("Conflicts: " + strings.Join(paths, ", "))
		} else {
			m.mergeMessage = styleApproved.Render(fmt.Sprintf("Merged into main at sequence %d.", msg.sequence))
		}

	case tea.KeyMsg:
		switch msg.String() {
		case "q":
			m.goBack = true

		case "Q", "ctrl+c":
			m.quit = true

		case "tab":
			m.activePanel = (m.activePanel + 1) % 3

		case "r":
			m.showReviewOverlay = true
			m.reviewOverlay = newReviewOverlay(m.client, m.branchName)
			return m, m.reviewOverlay.Init()

		case "m":
			if !m.merging {
				m.mergeConfirm = true
				m.mergeMessage = ""
			}

		case "R":
			m.loading = true
			m.err = nil
			m.mergeMessage = ""
			return m, loadBranchDetail(m.client, m.branchName)

		case "enter":
			if m.activePanel == panelDiff && m.diff != nil {
				files := diffFiles(m.diff, m.baseTreePaths)
				if m.diffCursor >= 0 && m.diffCursor < len(files) {
					m.expandedFiles[m.diffCursor] = !m.expandedFiles[m.diffCursor]
				}
			}

		case "j", "down":
			if m.activePanel == panelDiff && m.diff != nil {
				files := diffFiles(m.diff, m.baseTreePaths)
				if m.diffCursor < len(files)-1 {
					m.diffCursor++
				}
			}

		case "k", "up":
			if m.activePanel == panelDiff {
				if m.diffCursor > 0 {
					m.diffCursor--
				}
			}
		}
	}
	return m, nil
}

func (m branchDetailModel) View() string {
	var sb strings.Builder

	// Title.
	title := fmt.Sprintf("%s  [head:%d  base:%d]", m.branchName, m.headSeq, m.baseSeq)
	sb.WriteString(styleTitle.Render(title) + "\n")

	// Tab bar.
	sb.WriteString(m.renderTabs() + "\n")
	sb.WriteString(strings.Repeat("─", 60) + "\n")

	if m.loading {
		sb.WriteString("  Loading...\n")
	} else if m.err != nil {
		sb.WriteString(styleError.Render("  Error: "+m.err.Error()) + "\n")
	} else {
		switch m.activePanel {
		case panelDiff:
			sb.WriteString(m.renderDiff())
		case panelReviews:
			sb.WriteString(m.renderReviews())
		case panelChecks:
			sb.WriteString(m.renderChecks())
		}
	}

	// Merge confirmation or message.
	if m.mergeConfirm {
		sb.WriteString("\n")
		sb.WriteString(styleConfirm.Render(fmt.Sprintf("  Merge %s into main? [y/N] ", m.branchName)))
	} else if m.merging {
		sb.WriteString("\n  Merging...\n")
	} else if m.mergeMessage != "" {
		sb.WriteString("\n  " + m.mergeMessage + "\n")
	}

	// Review overlay.
	if m.showReviewOverlay {
		sb.WriteString("\n")
		sb.WriteString(styleBorder.Render(m.reviewOverlay.View()))
		sb.WriteString("\n")
	} else {
		sb.WriteString("\n")
		sb.WriteString(styleHelp.Render("  tab panels · enter expand/collapse · r review · m merge · R refresh · q back"))
	}

	return sb.String()
}

func (m branchDetailModel) renderTabs() string {
	tabs := []struct {
		label string
		p     panel
	}{
		{"[Diff]", panelDiff},
		{"Reviews", panelReviews},
		{"Checks", panelChecks},
	}
	parts := make([]string, len(tabs))
	for i, t := range tabs {
		if t.p == m.activePanel {
			parts[i] = styleTabActive.Render(t.label)
		} else {
			parts[i] = styleTabInactive.Render(t.label)
		}
	}
	return " " + strings.Join(parts, "  ")
}

// diffFile groups a path and its change type.
type diffFile struct {
	path       string
	changeType string // "+", "-", "~"
	binary     bool
}

func diffFiles(d *model.DiffResponse, baseTreePaths map[string]bool) []diffFile {
	if d == nil {
		return nil
	}
	var files []diffFile
	for _, e := range d.BranchChanges {
		var changeType string
		if e.VersionID == nil {
			changeType = "-"
		} else if len(baseTreePaths) > 0 && !baseTreePaths[e.Path] {
			changeType = "+"
		} else {
			changeType = "~"
		}
		files = append(files, diffFile{path: e.Path, changeType: changeType, binary: e.Binary})
	}
	return files
}

func (m branchDetailModel) renderDiff() string {
	if m.diff == nil {
		return "  No diff data.\n"
	}
	files := diffFiles(m.diff, m.baseTreePaths)
	if len(files) == 0 {
		return "  No changes on this branch.\n"
	}

	var sb strings.Builder
	for i, f := range files {
		prefix := "  "
		icon := f.changeType
		var iconStyled string
		switch icon {
		case "+":
			iconStyled = styleAdded.Render("+")
		case "-":
			iconStyled = styleRemoved.Render("-")
		default:
			iconStyled = styleModified.Render("~")
		}

		pathLabel := f.path
		if f.binary {
			pathLabel = f.path + " [binary]"
		}

		if i == m.diffCursor {
			line := styleSelected.Render(prefix + iconStyled + " " + pathLabel)
			sb.WriteString(line + "\n")
		} else {
			sb.WriteString(prefix + iconStyled + " " + pathLabel + "\n")
		}

		// If expanded, show placeholder or binary notice.
		if m.expandedFiles[i] {
			if f.binary {
				sb.WriteString(styleModified.Render("    binary file, no preview") + "\n")
			} else {
				sb.WriteString(styleModified.Render("    (file contents diff not available in current view)") + "\n")
			}
		}
	}
	return sb.String()
}

func (m branchDetailModel) renderReviews() string {
	if len(m.reviews) == 0 {
		return "  No reviews.\n"
	}

	// Collapse to one per reviewer (latest).
	latest := make(map[string]model.Review)
	for _, r := range m.reviews {
		prev, ok := latest[r.Reviewer]
		if !ok || r.CreatedAt.After(prev.CreatedAt) {
			latest[r.Reviewer] = r
		}
	}

	approved, rejected, staleCount := 0, 0, 0
	for _, r := range latest {
		isStale := r.Sequence < m.headSeq
		if isStale {
			staleCount++
		} else if r.Status == model.ReviewApproved {
			approved++
		} else if r.Status == model.ReviewRejected {
			rejected++
		}
	}

	var sb strings.Builder
	summary := fmt.Sprintf("  %d approved · %d rejected", approved, rejected)
	if staleCount > 0 {
		summary += fmt.Sprintf(" (%d stale)", staleCount)
	}
	sb.WriteString(styleHeader.Render(summary) + "\n\n")

	for _, r := range latest {
		isStale := r.Sequence < m.headSeq
		icon := "?"
		var iconStyled string
		switch r.Status {
		case model.ReviewApproved:
			icon = "✓"
			iconStyled = styleApproved.Render(icon)
		case model.ReviewRejected:
			icon = "✗"
			iconStyled = styleRejected.Render(icon)
		default:
			iconStyled = icon
		}

		body := r.Body
		if len(body) > 40 {
			body = body[:37] + "..."
		}

		line := fmt.Sprintf("  %s %-30s %-10s seq %-4d  %q",
			iconStyled, r.Reviewer, string(r.Status), r.Sequence, body)

		if isStale {
			line += "  [stale]"
			sb.WriteString(styleStale.Render(line) + "\n")
		} else {
			sb.WriteString(line + "\n")
		}
	}
	return sb.String()
}

func (m branchDetailModel) renderChecks() string {
	if len(m.checks) == 0 {
		return "  No check runs.\n"
	}

	var sb strings.Builder
	for _, c := range m.checks {
		isStale := c.Sequence < m.headSeq
		icon := "?"
		var iconStyled string
		switch c.Status {
		case model.CheckRunPassed:
			icon = "✓"
			iconStyled = stylePassed.Render(icon)
		case model.CheckRunFailed:
			icon = "✗"
			iconStyled = styleFailed.Render(icon)
		case model.CheckRunPending:
			icon = "○"
			iconStyled = stylePending.Render(icon)
		default:
			iconStyled = icon
		}

		line := fmt.Sprintf("  %s %-20s %-10s seq %-4d  %s",
			iconStyled, c.CheckName, string(c.Status), c.Sequence, c.Reporter)

		if isStale {
			line += "  [stale]"
			sb.WriteString(styleStale.Render(line) + "\n")
		} else {
			sb.WriteString(line + "\n")
		}
	}
	return sb.String()
}
