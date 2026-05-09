package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
	"github.com/dlorenc/docstore/internal/model"
)

// ---------------------------------------------------------------------------
// Issue management
// ---------------------------------------------------------------------------

// IssueList lists issues for the repo.
// state defaults to "open" if empty.
func (a *App) IssueList(state, author string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if state == "" {
		state = "open"
	}
	q := url.Values{}
	q.Set("state", state)
	if author != "" {
		q.Set("author", author)
	}
	resp, err := a.httpGet(cfg, repoBase(cfg)+"/issues?"+q.Encode())
	if err != nil {
		return fmt.Errorf("fetching issues: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	var r model.ListIssuesResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	if len(r.Issues) == 0 {
		fmt.Fprintf(a.Out, "No %s issues\n", state)
		return nil
	}
	fmt.Fprintf(a.Out, "%-6s  %-40s  %-30s  %-8s  %s\n", "NUMBER", "TITLE", "AUTHOR", "STATE", "CREATED")
	for _, iss := range r.Issues {
		title := iss.Title
		if len(title) > 38 {
			title = title[:37] + "…"
		}
		fmt.Fprintf(a.Out, "%-6d  %-40s  %-30s  %-8s  %s\n",
			iss.Number, title, iss.Author, string(iss.State),
			iss.CreatedAt.Format(time.RFC3339))
	}
	return nil
}

// IssueCreate creates a new issue.
func (a *App) IssueCreate(title, body string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if title == "" {
		return fmt.Errorf("--title is required")
	}
	req := model.CreateIssueRequest{
		Title: title,
		Body:  body,
	}
	resp, err := a.postJSON(cfg, repoBase(cfg)+"/issues", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	var r model.CreateIssueResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	fmt.Fprintf(a.Out, "Created issue #%d\n", r.Number)
	return nil
}

// IssueShow shows details for a single issue, including comments and refs.
func (a *App) IssueShow(number int64) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	base := repoBase(cfg) + "/issues/" + strconv.FormatInt(number, 10)

	resp, err := a.httpGet(cfg, base)
	if err != nil {
		return fmt.Errorf("fetching issue: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	var iss model.Issue
	if err := json.NewDecoder(resp.Body).Decode(&iss); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}

	fmt.Fprintf(a.Out, "Issue #%d: %s\n", iss.Number, iss.Title)
	fmt.Fprintf(a.Out, "State:   %s\n", string(iss.State))
	fmt.Fprintf(a.Out, "Author:  %s\n", iss.Author)
	fmt.Fprintf(a.Out, "Created: %s\n", iss.CreatedAt.Format(time.RFC3339))
	if iss.CloseReason != nil {
		fmt.Fprintf(a.Out, "Closed:  %s\n", string(*iss.CloseReason))
	}
	if iss.Body != "" {
		fmt.Fprintf(a.Out, "\n%s\n", iss.Body)
	}

	resp2, err := a.httpGet(cfg, base+"/comments")
	if err != nil {
		return fmt.Errorf("fetching comments: %w", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode == http.StatusOK {
		var cr model.ListIssueCommentsResponse
		if err := json.NewDecoder(resp2.Body).Decode(&cr); err == nil && len(cr.Comments) > 0 {
			fmt.Fprintf(a.Out, "\nComments (%d):\n", len(cr.Comments))
			for _, c := range cr.Comments {
				fmt.Fprintf(a.Out, "  [%s] %s: %s\n", c.CreatedAt.Format("2006-01-02"), c.Author, c.Body)
			}
		}
	}

	resp3, err := a.httpGet(cfg, base+"/refs")
	if err != nil {
		return fmt.Errorf("fetching refs: %w", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode == http.StatusOK {
		var rr model.ListIssueRefsResponse
		if err := json.NewDecoder(resp3.Body).Decode(&rr); err == nil && len(rr.Refs) > 0 {
			fmt.Fprintf(a.Out, "\nRefs (%d):\n", len(rr.Refs))
			for _, ref := range rr.Refs {
				fmt.Fprintf(a.Out, "  %s: %s\n", string(ref.RefType), ref.RefID)
			}
		}
	}

	return nil
}

// IssueClose closes an issue.
func (a *App) IssueClose(number int64, reason string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if reason == "" {
		reason = string(model.IssueCloseReasonCompleted)
	}
	req := model.CloseIssueRequest{Reason: model.IssueCloseReason(reason)}
	resp, err := a.postJSON(cfg, repoBase(cfg)+"/issues/"+strconv.FormatInt(number, 10)+"/close", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	fmt.Fprintf(a.Out, "Closed issue #%d\n", number)
	return nil
}

// IssueReopen reopens a closed issue.
func (a *App) IssueReopen(number int64) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	resp, err := a.postJSON(cfg, repoBase(cfg)+"/issues/"+strconv.FormatInt(number, 10)+"/reopen", model.ReopenIssueRequest{})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	fmt.Fprintf(a.Out, "Reopened issue #%d\n", number)
	return nil
}

// IssueCommentAdd adds a comment to an issue.
func (a *App) IssueCommentAdd(number int64, body string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if body == "" {
		return fmt.Errorf("--body is required")
	}
	req := model.CreateIssueCommentRequest{Body: body}
	resp, err := a.postJSON(cfg, repoBase(cfg)+"/issues/"+strconv.FormatInt(number, 10)+"/comments", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	var r model.CreateIssueCommentResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	fmt.Fprintf(a.Out, "Added comment %s\n", r.ID)
	return nil
}

// IssueCommentEdit edits an existing issue comment.
// issueNumber is required to construct the URL path.
func (a *App) IssueCommentEdit(issueNumber int64, commentID, body string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if body == "" {
		return fmt.Errorf("--body is required")
	}
	req := model.UpdateIssueCommentRequest{Body: body}
	urlPath := repoBase(cfg) + "/issues/" + strconv.FormatInt(issueNumber, 10) + "/comments/" + url.PathEscape(commentID)
	resp, err := a.patchJSON(cfg, urlPath, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	fmt.Fprintf(a.Out, "Updated comment %s\n", commentID)
	return nil
}

// IssueRefs lists cross-references for an issue.
func (a *App) IssueRefs(number int64) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	resp, err := a.httpGet(cfg, repoBase(cfg)+"/issues/"+strconv.FormatInt(number, 10)+"/refs")
	if err != nil {
		return fmt.Errorf("fetching refs: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	var r model.ListIssueRefsResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	if len(r.Refs) == 0 {
		fmt.Fprintf(a.Out, "No refs for issue #%d\n", number)
		return nil
	}
	fmt.Fprintf(a.Out, "%-10s  %-36s  %s\n", "TYPE", "REF_ID", "CREATED")
	for _, ref := range r.Refs {
		fmt.Fprintf(a.Out, "%-10s  %-36s  %s\n",
			string(ref.RefType), ref.RefID, ref.CreatedAt.Format(time.RFC3339))
	}
	return nil
}

// IssueTie ties a proposal or commit ref to an issue.
func (a *App) IssueTie(number int64, refType, refID string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	req := model.AddIssueRefRequest{
		RefType: model.IssueRefType(refType),
		RefID:   refID,
	}
	resp, err := a.postJSON(cfg, repoBase(cfg)+"/issues/"+strconv.FormatInt(number, 10)+"/refs", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	fmt.Fprintf(a.Out, "Tied %s %s to issue #%d\n", refType, refID, number)
	return nil
}