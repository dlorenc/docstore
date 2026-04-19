// Package tui implements the terminal UI for the ds command.
package tui

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/dlorenc/docstore/internal/model"
)

// view identifies which screen is currently shown.
type view int

const (
	viewBranchList view = iota
	viewBranchDetail
)

// tuiClient wraps HTTP calls for the TUI.
type tuiClient struct {
	httpClient *http.Client
	remote     string
	repo       string
	author     string
	debug      *log.Logger // nil when DS_TUI_DEBUG is unset
}

func (c *tuiClient) repoBase() string {
	return c.remote + "/repos/" + c.repo + "/-"
}

func (c *tuiClient) debugf(format string, args ...interface{}) {
	if c.debug != nil {
		c.debug.Printf(format, args...)
	}
}

func (c *tuiClient) get(path string) (*http.Response, error) {
	fullURL := c.repoBase() + path
	req, err := http.NewRequest("GET", fullURL, nil)
	if err != nil {
		c.debugf("GET %s: build request: %v", fullURL, err)
		return nil, err
	}
	req.Header.Set("X-DocStore-Identity", c.author)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.debugf("GET %s: %v", fullURL, err)
		return nil, err
	}
	c.debugf("GET %s -> %d", fullURL, resp.StatusCode)
	return resp, nil
}

func (c *tuiClient) postJSON(path string, body interface{}) (*http.Response, error) {
	fullURL := c.repoBase() + path
	data, err := json.Marshal(body)
	if err != nil {
		c.debugf("POST %s: marshal body: %v", fullURL, err)
		return nil, err
	}
	req, err := http.NewRequest("POST", fullURL, jsonBody(data))
	if err != nil {
		c.debugf("POST %s: build request: %v", fullURL, err)
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-DocStore-Identity", c.author)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.debugf("POST %s: %v", fullURL, err)
		return nil, err
	}
	c.debugf("POST %s -> %d", fullURL, resp.StatusCode)
	return resp, nil
}

// decodeJSON decodes a successful (2xx) response body into target. On non-2xx
// it reads the body, tries to parse it as model.ErrorResponse, and returns a
// formatted error that includes the status code and server message. This
// prevents callers from mis-decoding an error envelope as their expected type
// (e.g. decoding `{"error":"..."}` as `[]model.CheckRun`).
//
// The caller retains responsibility for closing resp.Body.
func (c *tuiClient) decodeJSON(resp *http.Response, target interface{}) error {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		var errResp model.ErrorResponse
		msg := ""
		if jsonErr := json.Unmarshal(body, &errResp); jsonErr == nil && errResp.Error != "" {
			msg = errResp.Error
		} else if len(body) > 0 {
			msg = string(body)
		}
		c.debugf("decodeJSON: HTTP %d: %s", resp.StatusCode, msg)
		if msg == "" {
			return fmt.Errorf("server returned HTTP %d", resp.StatusCode)
		}
		return fmt.Errorf("server returned HTTP %d: %s", resp.StatusCode, msg)
	}
	if target == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		c.debugf("decodeJSON: decode %T: %v", target, err)
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

// topModel is the root Bubble Tea model; it routes to sub-models based on the active view.
type topModel struct {
	client     *tuiClient
	activeView view

	branchList   branchListModel
	branchDetail branchDetailModel
}

// newTopModel creates the initial top-level model.
func newTopModel(client *tuiClient) topModel {
	return topModel{
		client:     client,
		activeView: viewBranchList,
		branchList: newBranchListModel(client),
	}
}

func (m topModel) Init() tea.Cmd {
	return m.branchList.Init()
}

func (m topModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m.activeView {
	case viewBranchList:
		newList, cmd := m.branchList.Update(msg)
		m.branchList = newList

		if m.branchList.openBranch != "" {
			branch := m.branchList.openBranch
			m.branchList.openBranch = ""
			m.branchDetail = newBranchDetailModel(m.client, branch)
			m.activeView = viewBranchDetail
			return m, m.branchDetail.Init()
		}
		if m.branchList.quit {
			return m, tea.Quit
		}
		return m, cmd

	case viewBranchDetail:
		newDetail, cmd := m.branchDetail.Update(msg)
		m.branchDetail = newDetail

		if m.branchDetail.goBack {
			m.branchDetail.goBack = false
			m.activeView = viewBranchList
			// Refresh branch list when going back.
			return m, m.branchList.Init()
		}
		if m.branchDetail.quit {
			return m, tea.Quit
		}
		return m, cmd
	}
	return m, nil
}

func (m topModel) View() string {
	switch m.activeView {
	case viewBranchList:
		return m.branchList.View()
	case viewBranchDetail:
		return m.branchDetail.View()
	}
	return ""
}

// --- Helper types for async data loading ---

type branchesLoadedMsg struct {
	branches []branchSummary
	err      error
}

type branchDetailLoadedMsg struct {
	diff          *model.DiffResponse
	reviews       []model.Review
	checks        []model.CheckRun
	headSeq       int64
	baseSeq       int64
	baseTreePaths map[string]bool
	proposal      *model.Proposal // nil if no open proposal
	err           error
}

type mergeResultMsg struct {
	sequence int64
	conflicts []model.ConflictEntry
	err      error
}

type reviewSubmittedMsg struct {
	id  string
	err error
}

type proposalOpenedMsg struct {
	id  string
	err error
}

type proposalClosedMsg struct {
	err error
}

// branchSummary holds a branch plus its review/check summary.
type branchSummary struct {
	branch   model.Branch
	author   string
	approved int
	rejected int
	passed   int
	failed   int
	pending  int
}

// --- Commands that fetch data ---

func loadBranches(c *tuiClient) tea.Cmd {
	return func() tea.Msg {
		resp, err := c.get("/branches?status=active")
		if err != nil {
			return branchesLoadedMsg{err: err}
		}
		defer resp.Body.Close()

		var branches []model.Branch
		if err := c.decodeJSON(resp, &branches); err != nil {
			return branchesLoadedMsg{err: fmt.Errorf("loading branches: %w", err)}
		}

		summaries := make([]branchSummary, 0, len(branches))
		for _, b := range branches {
			s := branchSummary{branch: b}

			// Fetch head commit to get branch author.
			if b.HeadSequence > 0 {
				commitPath := fmt.Sprintf("/commit/%d", b.HeadSequence)
				cmResp, cmErr := c.get(commitPath)
				if cmErr == nil {
					var cm model.GetCommitResponse
					if jsonErr := c.decodeJSON(cmResp, &cm); jsonErr == nil {
						s.author = cm.Author
					}
					cmResp.Body.Close()
				}
			}

			// Fetch reviews (explicit close per iteration; deduplicate per reviewer).
			rResp, rErr := c.get("/branch/" + url.PathEscape(b.Name) + "/reviews")
			if rErr == nil {
				var reviews []model.Review
				jsonErr := c.decodeJSON(rResp, &reviews)
				rResp.Body.Close()
				if jsonErr == nil {
					latest := make(map[string]model.Review)
					for _, r := range reviews {
						prev, ok := latest[r.Reviewer]
						if !ok || r.CreatedAt.After(prev.CreatedAt) {
							latest[r.Reviewer] = r
						}
					}
					for _, r := range latest {
						if r.Sequence >= b.HeadSequence {
							if r.Status == model.ReviewApproved {
								s.approved++
							} else if r.Status == model.ReviewRejected {
								s.rejected++
							}
						}
					}
				}
			}

			// Fetch checks (explicit close per iteration).
			cResp, cErr := c.get("/branch/" + url.PathEscape(b.Name) + "/checks")
			if cErr == nil {
				var checks []model.CheckRun
				jsonErr := c.decodeJSON(cResp, &checks)
				cResp.Body.Close()
				if jsonErr == nil {
					for _, ch := range checks {
						if ch.Sequence == b.HeadSequence {
							switch ch.Status {
							case model.CheckRunPassed:
								s.passed++
							case model.CheckRunFailed:
								s.failed++
							case model.CheckRunPending:
								s.pending++
							}
						}
					}
				}
			}

			summaries = append(summaries, s)
		}

		return branchesLoadedMsg{branches: summaries}
	}
}

func loadBranchDetail(c *tuiClient, branchName string) tea.Cmd {
	return func() tea.Msg {
		// BUG-7: Fetch all branches (not filtered by status) so merged/closed branches are found.
		bResp, err := c.get("/branches")
		if err != nil {
			return branchDetailLoadedMsg{err: err}
		}
		defer bResp.Body.Close()
		var allBranches []model.Branch
		if err := c.decodeJSON(bResp, &allBranches); err != nil {
			return branchDetailLoadedMsg{err: err}
		}
		var headSeq, baseSeq int64
		for _, b := range allBranches {
			if b.Name == branchName {
				headSeq = b.HeadSequence
				baseSeq = b.BaseSequence
				break
			}
		}

		// Diff.
		dResp, err := c.get("/diff?branch=" + url.QueryEscape(branchName))
		if err != nil {
			return branchDetailLoadedMsg{err: err}
		}
		defer dResp.Body.Close()
		var diff model.DiffResponse
		if err := c.decodeJSON(dResp, &diff); err != nil {
			return branchDetailLoadedMsg{err: err}
		}

		// BUG-5 fix: Fetch the base tree with pagination to handle repos with >100 files.
		baseTreePaths := make(map[string]bool)
		const treePageSize = 500
		var afterCursor string
		for {
			treeURL := fmt.Sprintf("/tree?branch=%s&at=%d&limit=%d",
				url.QueryEscape(branchName), baseSeq, treePageSize)
			if afterCursor != "" {
				treeURL += "&after=" + url.QueryEscape(afterCursor)
			}
			tResp, tErr := c.get(treeURL)
			if tErr != nil {
				break
			}
			var treeEntries []model.TreeEntry
			jsonErr := c.decodeJSON(tResp, &treeEntries)
			tResp.Body.Close()
			if jsonErr != nil {
				break
			}
			for _, e := range treeEntries {
				baseTreePaths[e.Path] = true
			}
			if len(treeEntries) < treePageSize {
				break // last page
			}
			afterCursor = treeEntries[len(treeEntries)-1].Path
		}

		// Reviews.
		rResp, err := c.get("/branch/" + url.PathEscape(branchName) + "/reviews")
		if err != nil {
			return branchDetailLoadedMsg{err: err}
		}
		defer rResp.Body.Close()
		var reviews []model.Review
		if err := c.decodeJSON(rResp, &reviews); err != nil {
			return branchDetailLoadedMsg{err: fmt.Errorf("loading reviews: %w", err)}
		}

		// Checks.
		cResp, err := c.get("/branch/" + url.PathEscape(branchName) + "/checks")
		if err != nil {
			return branchDetailLoadedMsg{err: err}
		}
		defer cResp.Body.Close()
		var checks []model.CheckRun
		if err := c.decodeJSON(cResp, &checks); err != nil {
			return branchDetailLoadedMsg{err: fmt.Errorf("loading checks: %w", err)}
		}

		// Proposal: fetch open proposals filtered by branch.
		var openProposal *model.Proposal
		pResp, pErr := c.get("/proposals?state=open&branch=" + url.QueryEscape(branchName))
		if pErr == nil {
			var proposals []model.Proposal
			if decErr := c.decodeJSON(pResp, &proposals); decErr == nil && len(proposals) > 0 {
				openProposal = &proposals[0]
			}
			pResp.Body.Close()
		}

		return branchDetailLoadedMsg{
			diff:          &diff,
			reviews:       reviews,
			checks:        checks,
			headSeq:       headSeq,
			baseSeq:       baseSeq,
			baseTreePaths: baseTreePaths,
			proposal:      openProposal,
		}
	}
}

func mergeBranch(c *tuiClient, branchName string) tea.Cmd {
	return func() tea.Msg {
		req := model.MergeRequest{Branch: branchName}
		resp, err := c.postJSON("/merge", req)
		if err != nil {
			return mergeResultMsg{err: err}
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusConflict {
			var conflictErr model.MergeConflictError
			json.NewDecoder(resp.Body).Decode(&conflictErr)
			return mergeResultMsg{conflicts: conflictErr.Conflicts}
		}

		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
			var errResp model.ErrorResponse
			json.NewDecoder(resp.Body).Decode(&errResp)
			return mergeResultMsg{err: fmt.Errorf("server error: %s", errResp.Error)}
		}

		var mergeResp model.MergeResponse
		json.NewDecoder(resp.Body).Decode(&mergeResp)
		return mergeResultMsg{sequence: mergeResp.Sequence}
	}
}

func openProposal(c *tuiClient, branchName, title, description, baseBranch string) tea.Cmd {
	return func() tea.Msg {
		req := model.CreateProposalRequest{
			Branch:      branchName,
			BaseBranch:  baseBranch,
			Title:       title,
			Description: description,
		}
		resp, err := c.postJSON("/proposals", req)
		if err != nil {
			return proposalOpenedMsg{err: err}
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			var errResp model.ErrorResponse
			json.NewDecoder(resp.Body).Decode(&errResp)
			return proposalOpenedMsg{err: fmt.Errorf("server error: %s", errResp.Error)}
		}

		var r model.CreateProposalResponse
		json.NewDecoder(resp.Body).Decode(&r)
		return proposalOpenedMsg{id: r.ID}
	}
}

func closeProposal(c *tuiClient, proposalID string) tea.Cmd {
	return func() tea.Msg {
		resp, err := c.postJSON("/proposals/"+proposalID+"/close", nil)
		if err != nil {
			return proposalClosedMsg{err: err}
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
			var errResp model.ErrorResponse
			json.NewDecoder(resp.Body).Decode(&errResp)
			return proposalClosedMsg{err: fmt.Errorf("server error: %s", errResp.Error)}
		}
		return proposalClosedMsg{}
	}
}

func submitReview(c *tuiClient, branchName, status, body string) tea.Cmd {
	return func() tea.Msg {
		req := model.CreateReviewRequest{
			Branch: branchName,
			Status: model.ReviewStatus(status),
			Body:   body,
		}
		resp, err := c.postJSON("/review", req)
		if err != nil {
			return reviewSubmittedMsg{err: err}
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			var errResp model.ErrorResponse
			json.NewDecoder(resp.Body).Decode(&errResp)
			return reviewSubmittedMsg{err: fmt.Errorf("server error: %s", errResp.Error)}
		}

		var r model.CreateReviewResponse
		json.NewDecoder(resp.Body).Decode(&r)
		return reviewSubmittedMsg{id: r.ID}
	}
}
