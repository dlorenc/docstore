package cli

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"github.com/dlorenc/docstore/internal/model"
)


// Reviews lists reviews for a branch (defaults to current branch if empty).
// A review is marked [stale] if its sequence < the branch's head_sequence.
func (a *App) Reviews(branch string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if branch == "" {
		branch = cfg.Branch
	}

	headSeq, err := a.branchHeadSequence(cfg, branch)
	if err != nil {
		return err
	}

	resp, err := a.httpGet(cfg, repoBase(cfg)+"/branch/"+url.PathEscape(branch)+"/reviews")
	if err != nil {
		return fmt.Errorf("fetching reviews: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}

	var reviews []model.Review
	if err := json.NewDecoder(resp.Body).Decode(&reviews); err != nil {
		return fmt.Errorf("decoding reviews: %w", err)
	}

	if len(reviews) == 0 {
		fmt.Fprintf(a.Out, "No reviews for branch '%s'\n", branch)
		return nil
	}

	fmt.Fprintf(a.Out, "%-36s  %-30s  %-4s  %-10s  %s\n", "ID", "REVIEWER", "SEQ", "STATUS", "BODY")
	for _, r := range reviews {
		stale := ""
		if r.Sequence < headSeq {
			stale = "  [stale]"
		}
		fmt.Fprintf(a.Out, "%-36s  %-30s  %-4d  %-10s  %s%s\n",
			r.ID, r.Reviewer, r.Sequence, string(r.Status), r.Body, stale)
	}
	return nil
}

// Review submits a review for a branch.
func (a *App) Review(branch, status, body string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if branch == "" {
		branch = cfg.Branch
	}

	reviewStatus := model.ReviewStatus(status)
	if reviewStatus != model.ReviewApproved && reviewStatus != model.ReviewRejected {
		return fmt.Errorf("status must be 'approved' or 'rejected'")
	}

	req := model.CreateReviewRequest{
		Branch: branch,
		Status: reviewStatus,
		Body:   body,
	}
	resp, err := a.postJSON(cfg, repoBase(cfg)+"/review", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}

	var reviewResp model.CreateReviewResponse
	if err := json.NewDecoder(resp.Body).Decode(&reviewResp); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}

	fmt.Fprintf(a.Out, "Review submitted: %s (id: %s, sequence: %d)\n", status, reviewResp.ID, reviewResp.Sequence)
	return nil
}

// Comment creates an inline file annotation on a branch.
// The version_id is resolved automatically by fetching the file metadata from the server.
func (a *App) Comment(branch, path, body string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if branch == "" {
		branch = cfg.Branch
	}

	// Resolve version_id for the file on this branch.
	q := url.Values{}
	q.Set("branch", branch)
	fileResp, err := a.httpGet(cfg, repoBase(cfg)+"/file/"+url.PathEscape(path)+"?"+q.Encode())
	if err != nil {
		return fmt.Errorf("fetching file info: %w", err)
	}
	defer fileResp.Body.Close()
	if fileResp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("file %q not found on branch %q", path, branch)
	}
	if fileResp.StatusCode != http.StatusOK {
		return a.readError(fileResp)
	}
	var fileMeta struct {
		VersionID string `json:"version_id"`
	}
	if err := json.NewDecoder(fileResp.Body).Decode(&fileMeta); err != nil {
		return fmt.Errorf("decoding file info: %w", err)
	}
	if fileMeta.VersionID == "" {
		return fmt.Errorf("file %q was deleted on branch %q", path, branch)
	}

	req := model.CreateReviewCommentRequest{
		Branch:    branch,
		Path:      path,
		VersionID: fileMeta.VersionID,
		Body:      body,
	}
	resp, err := a.postJSON(cfg, repoBase(cfg)+"/comment", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return a.readError(resp)
	}

	var createResp model.CreateReviewCommentResponse
	if err := json.NewDecoder(resp.Body).Decode(&createResp); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}

	fmt.Fprintf(a.Out, "Comment created: id=%s, sequence=%d\n", createResp.ID, createResp.Sequence)
	return nil
}

// Comments lists inline file comments for a branch. If path is non-empty, only
// comments on that path are shown.
func (a *App) Comments(branch, path string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if branch == "" {
		branch = cfg.Branch
	}

	q := url.Values{}
	if path != "" {
		q.Set("path", path)
	}
	urlStr := repoBase(cfg) + "/branch/" + url.PathEscape(branch) + "/comments"
	if len(q) > 0 {
		urlStr += "?" + q.Encode()
	}

	resp, err := a.httpGet(cfg, urlStr)
	if err != nil {
		return fmt.Errorf("fetching comments: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}

	var comments []model.ReviewComment
	if err := json.NewDecoder(resp.Body).Decode(&comments); err != nil {
		return fmt.Errorf("decoding comments: %w", err)
	}

	if len(comments) == 0 {
		fmt.Fprintf(a.Out, "No comments for branch '%s'\n", branch)
		return nil
	}

	fmt.Fprintf(a.Out, "%-36s  %-40s  %-4s  %s\n", "ID", "PATH", "SEQ", "BODY")
	for _, c := range comments {
		fmt.Fprintf(a.Out, "%-36s  %-40s  %-4d  %s\n", c.ID, c.Path, c.Sequence, c.Body)
	}
	return nil
}

// Checks lists check runs for a branch (defaults to current branch if empty).
// If showAll is false, only the latest result per check_name is shown.
func (a *App) Checks(branch string, showAll bool) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if branch == "" {
		branch = cfg.Branch
	}

	resp, err := a.httpGet(cfg, repoBase(cfg)+"/branch/"+url.PathEscape(branch)+"/checks")
	if err != nil {
		return fmt.Errorf("fetching checks: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}

	var checkRuns []model.CheckRun
	if err := json.NewDecoder(resp.Body).Decode(&checkRuns); err != nil {
		return fmt.Errorf("decoding checks: %w", err)
	}

	if len(checkRuns) == 0 {
		fmt.Fprintf(a.Out, "No check runs for branch '%s'\n", branch)
		return nil
	}

	headSeq, err := a.branchHeadSequence(cfg, branch)
	if err != nil {
		return err
	}

	// Deduplicate by check_name keeping highest sequence unless --all is set.
	if !showAll {
		latest := make(map[string]model.CheckRun)
		for _, c := range checkRuns {
			prev, ok := latest[c.CheckName]
			if !ok || c.Sequence > prev.Sequence {
				latest[c.CheckName] = c
			}
		}
		checkRuns = slices.Collect(maps.Values(latest))
		slices.SortFunc(checkRuns, func(a, b model.CheckRun) int {
			return strings.Compare(a.CheckName, b.CheckName)
		})
	}

	fmt.Fprintf(a.Out, "%-8s  %-20s  %-4s  %-10s  %s\n", "ID", "CHECK NAME", "SEQ", "STATUS", "REPORTER")
	for _, c := range checkRuns {
		stale := ""
		if c.Sequence < headSeq {
			stale = "  [stale]"
		}
		id := c.ID
		if len(id) > 8 {
			id = id[:8]
		}
		fmt.Fprintf(a.Out, "%-8s  %-20s  %-4d  %-10s  %s%s\n",
			id, c.CheckName, c.Sequence, string(c.Status), c.Reporter, stale)
		if c.LogURL != nil {
			fmt.Fprintf(a.Out, "         log: %s\n", *c.LogURL)
		}
	}
	return nil
}

// Check reports a CI check result for a branch.
// logURL and sequence are optional; pass nil to omit them from the request.
func (a *App) Check(branch, name, status string, logURL *string, sequence *int64) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if branch == "" {
		branch = cfg.Branch
	}

	checkStatus := model.CheckRunStatus(status)
	if checkStatus != model.CheckRunPassed && checkStatus != model.CheckRunFailed && checkStatus != model.CheckRunPending {
		return fmt.Errorf("status must be 'passed', 'failed', or 'pending'")
	}

	req := model.CreateCheckRunRequest{
		Branch:    branch,
		CheckName: name,
		Status:    checkStatus,
		LogURL:    logURL,
		Sequence:  sequence,
	}
	resp, err := a.postJSON(cfg, repoBase(cfg)+"/check", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}

	var checkResp model.CreateCheckRunResponse
	if err := json.NewDecoder(resp.Body).Decode(&checkResp); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}

	fmt.Fprintf(a.Out, "Check run submitted: %s=%s (id: %s, sequence: %d)\n", name, status, checkResp.ID, checkResp.Sequence)
	return nil
}

// branchHeadSequence returns the head sequence of a named branch.
func (a *App) branchHeadSequence(cfg *Config, branch string) (int64, error) {
	resp, err := a.httpGet(cfg, repoBase(cfg)+"/branches")
	if err != nil {
		return 0, fmt.Errorf("fetching branches: %w", err)
	}
	defer resp.Body.Close()

	var branches []model.Branch
	if err := json.NewDecoder(resp.Body).Decode(&branches); err != nil {
		return 0, fmt.Errorf("decoding branches: %w", err)
	}

	for _, b := range branches {
		if b.Name == branch {
			return b.HeadSequence, nil
		}
	}
	return 0, fmt.Errorf("branch %q not found", branch)
}

// RetryChecks requests a retry of CI checks for a branch.
// If checks is empty, all failed checks at the branch's current head sequence are retried.
func (a *App) RetryChecks(branch string, checks []string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if branch == "" {
		branch = cfg.Branch
	}

	seq, err := a.branchHeadSequence(cfg, branch)
	if err != nil {
		return err
	}

	req := model.RetryChecksRequest{
		Branch:   branch,
		Sequence: seq,
		Checks:   checks,
	}
	resp, err := a.postJSON(cfg, repoBase(cfg)+"/checks/retry", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		return a.readError(resp)
	}

	var retryResp model.RetryChecksResponse
	if err := json.NewDecoder(resp.Body).Decode(&retryResp); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}

	if len(checks) == 0 {
		fmt.Fprintf(a.Out, "Retrying all failed checks on branch '%s' at sequence %d (attempt %d)\n", branch, seq, retryResp.Attempt)
	} else {
		fmt.Fprintf(a.Out, "Retrying checks %v on branch '%s' at sequence %d (attempt %d)\n", checks, branch, seq, retryResp.Attempt)
	}
	return nil
}