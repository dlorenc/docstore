package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

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

// diffHunkLine is a single line in a computed line-level diff.
type diffHunkLine struct {
	kind byte   // '+', '-', or ' '
	text string
}

// fileDiffData holds pre-computed diff lines for a single changed file.
type fileDiffData struct {
	hunks []diffHunkLine
	err   string // non-empty if fetch or diff computation failed
}

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
	fileContents  map[string]fileDiffData // pre-fetched diffs keyed by path

	// Proposal state for the current branch.
	proposal *model.Proposal // nil = not yet loaded or no open proposal

	activePanel   panel
	diffCursor    int
	expandedFiles map[int]bool

	mainHeadSeq int64 // current head sequence of main branch

	// Merge confirmation state.
	merging      bool
	mergeConfirm bool // waiting for y/N
	mergeMessage string

	// Refresh feedback state.
	refreshing    bool
	lastRefreshed time.Time

	// Review overlay.
	showReviewOverlay bool
	reviewOverlay     reviewOverlayModel

	// Proposal overlay.
	showProposalOverlay bool
	proposalOverlay     proposalOverlayModel

	// Auto-refresh: true when any HEAD check is pending.
	autoRefreshPending bool

	goBack bool
	quit   bool
}

// checkRefreshTick is sent by the auto-refresh ticker.
type checkRefreshTick struct{}

func scheduleCheckRefresh() tea.Cmd {
	return tea.Tick(5*time.Second, func(time.Time) tea.Msg {
		return checkRefreshTick{}
	})
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
	// If proposal overlay is active, delegate to it.
	if m.showProposalOverlay {
		switch msg := msg.(type) {
		case tea.KeyMsg:
			if msg.String() == "esc" {
				m.showProposalOverlay = false
				return m, nil
			}
			if msg.String() == "ctrl+c" {
				m.quit = true
				return m, tea.Quit
			}
		case proposalOpenedMsg:
			if msg.err == nil {
				m.showProposalOverlay = false
				m.loading = true
				return m, loadBranchDetail(m.client, m.branchName)
			}
		}
		var cmd tea.Cmd
		m.proposalOverlay, cmd = m.proposalOverlay.Update(msg)
		if m.proposalOverlay.submitted {
			m.showProposalOverlay = false
			m.loading = true
			return m, loadBranchDetail(m.client, m.branchName)
		}
		return m, cmd
	}

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
	case checkRefreshTick:
		if m.autoRefreshPending {
			m.loading = true
			return m, loadBranchDetail(m.client, m.branchName)
		}

	case branchDetailLoadedMsg:
		m.loading = false
		m.refreshing = false
		m.lastRefreshed = time.Now()
		m.err = msg.err
		if msg.err == nil {
			m.diff = msg.diff
			m.reviews = msg.reviews
			m.checks = msg.checks
			m.headSeq = msg.headSeq
			m.baseSeq = msg.baseSeq
			m.mainHeadSeq = msg.mainHeadSeq
			m.baseTreePaths = msg.baseTreePaths
			m.fileContents = msg.fileContents
			m.proposal = msg.proposal
		}
		// Auto-refresh: schedule a tick if any HEAD check is pending.
		m.autoRefreshPending = false
		for _, c := range m.checks {
			if c.Sequence == m.headSeq && c.Status == model.CheckRunPending {
				m.autoRefreshPending = true
				break
			}
		}
		if m.autoRefreshPending {
			return m, scheduleCheckRefresh()
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
			m.autoRefreshPending = false
			m.goBack = true

		case "Q", "ctrl+c":
			m.quit = true

		case "tab":
			m.activePanel = (m.activePanel + 1) % 3

		case "p":
			// Only open proposal overlay if no open proposal exists.
			if m.proposal == nil {
				m.showProposalOverlay = true
				m.proposalOverlay = newProposalOverlay(m.client, m.branchName)
				return m, m.proposalOverlay.Init()
			}

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
			m.refreshing = true
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

	// Title with proposal indicator.
	proposalIndicator := "[no proposal]"
	if m.loading {
		proposalIndicator = ""
	} else if m.proposal != nil {
		switch m.proposal.State {
		case model.ProposalOpen:
			proposalIndicator = styleApproved.Render("[proposal open]")
		case model.ProposalClosed:
			proposalIndicator = styleStale.Render("[proposal closed]")
		case model.ProposalMerged:
			proposalIndicator = stylePassed.Render("[proposal merged]")
		}
	}
	title := fmt.Sprintf("%s  [head:%d  base:%d]", m.branchName, m.headSeq, m.baseSeq)
	if proposalIndicator != "" {
		title += "  " + proposalIndicator
	}
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
		if m.mainHeadSeq > 0 && m.baseSeq < m.mainHeadSeq {
			sb.WriteString(styleError.Render(fmt.Sprintf(
				"  ⚠ base seq %d is behind main head seq %d — conflicts possible",
				m.baseSeq, m.mainHeadSeq)) + "\n")
		}
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
	} else if m.showProposalOverlay {
		sb.WriteString("\n")
		sb.WriteString(styleBorder.Render(m.proposalOverlay.View()))
		sb.WriteString("\n")
	} else {
		sb.WriteString("\n")
		helpText := "  tab panels · enter expand/collapse · r review · m merge · R refresh · q back"
		if m.proposal == nil && !m.loading {
			helpText = "  tab panels · enter expand/collapse · p proposal · r review · m merge · R refresh · q back"
		}
		sb.WriteString(styleHelp.Render(helpText))
		if m.refreshing {
			sb.WriteString("\n" + styleHelp.Render("  ↻ refreshing…"))
		} else if !m.lastRefreshed.IsZero() {
			sb.WriteString("\n" + styleHelp.Render("  last refreshed at "+m.lastRefreshed.Format("15:04:05")))
		}
	}

	return sb.String()
}

func (m branchDetailModel) renderTabs() string {
	tabs := []struct {
		label string
		p     panel
	}{
		{"Diff", panelDiff},
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

		// If expanded, show diff content or binary notice.
		if m.expandedFiles[i] {
			if f.binary {
				sb.WriteString(styleModified.Render("    binary file, no preview") + "\n")
			} else if data, ok := m.fileContents[f.path]; ok {
				sb.WriteString(renderFileDiff(data))
			} else {
				sb.WriteString(styleModified.Render("    (diff not available)") + "\n")
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

	// Deduplicate by CheckName, keeping the entry with the highest Sequence.
	latest := make(map[string]model.CheckRun)
	for _, c := range m.checks {
		prev, ok := latest[c.CheckName]
		if !ok || c.Sequence > prev.Sequence {
			latest[c.CheckName] = c
		}
	}
	deduped := make([]model.CheckRun, 0, len(latest))
	for _, c := range latest {
		deduped = append(deduped, c)
	}
	sort.Slice(deduped, func(i, j int) bool {
		return deduped[i].CheckName < deduped[j].CheckName
	})

	var sb strings.Builder
	for _, c := range deduped {
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
		if c.LogURL != nil {
			sb.WriteString("       log: " + *c.LogURL + "\n")
		}
	}
	return sb.String()
}

// renderFileDiff renders pre-computed diff hunks as coloured lines.
func renderFileDiff(data fileDiffData) string {
	var sb strings.Builder
	if data.err != "" {
		sb.WriteString(styleModified.Render("    "+data.err) + "\n")
		return sb.String()
	}
	const maxLines = 300
	hunks := data.hunks
	truncated := false
	if len(hunks) > maxLines {
		hunks = hunks[:maxLines]
		truncated = true
	}
	for _, h := range hunks {
		line := "    " + string([]byte{h.kind}) + " " + h.text
		switch h.kind {
		case '+':
			sb.WriteString(styleAdded.Render(line) + "\n")
		case '-':
			sb.WriteString(styleRemoved.Render(line) + "\n")
		default:
			sb.WriteString(styleStale.Render(line) + "\n")
		}
	}
	if truncated {
		sb.WriteString(styleModified.Render("    ... (output truncated)") + "\n")
	}
	return sb.String()
}

// computeFileDiff builds a fileDiffData from raw base and head file content.
// changeType is "+", "-", or "~".
func computeFileDiff(baseContent, headContent []byte, changeType string) fileDiffData {
	splitLines := func(b []byte) []string {
		lines := strings.Split(string(b), "\n")
		// Trim trailing empty line produced by a trailing newline.
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		return lines
	}

	switch changeType {
	case "+":
		lines := splitLines(headContent)
		hunks := make([]diffHunkLine, len(lines))
		for i, l := range lines {
			hunks[i] = diffHunkLine{'+', l}
		}
		return fileDiffData{hunks: hunks}
	case "-":
		lines := splitLines(baseContent)
		hunks := make([]diffHunkLine, len(lines))
		for i, l := range lines {
			hunks[i] = diffHunkLine{'-', l}
		}
		return fileDiffData{hunks: hunks}
	default: // "~"
		base := splitLines(baseContent)
		head := splitLines(headContent)
		if len(base)+len(head) > 4000 {
			return fileDiffData{err: "(file too large to diff inline)"}
		}
		return fileDiffData{hunks: lcsLineDiff(base, head)}
	}
}

// lcsLineDiff computes a line-level diff of base → head using LCS.
func lcsLineDiff(base, head []string) []diffHunkLine {
	m, n := len(base), len(head)

	// Build DP table.
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if base[i-1] == head[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] >= dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}

	// Traceback (builds result in reverse).
	result := make([]diffHunkLine, 0, m+n)
	i, j := m, n
	for i > 0 || j > 0 {
		switch {
		case i > 0 && j > 0 && base[i-1] == head[j-1]:
			result = append(result, diffHunkLine{' ', base[i-1]})
			i--
			j--
		case j > 0 && (i == 0 || dp[i][j-1] > dp[i-1][j]):
			result = append(result, diffHunkLine{'+', head[j-1]})
			j--
		default:
			result = append(result, diffHunkLine{'-', base[i-1]})
			i--
		}
	}

	// Reverse to get forward order.
	for l, r := 0, len(result)-1; l < r; l, r = l+1, r-1 {
		result[l], result[r] = result[r], result[l]
	}
	return result
}
