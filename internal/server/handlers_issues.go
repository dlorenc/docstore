package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"

	"github.com/dlorenc/docstore/internal/db"
	evtypes "github.com/dlorenc/docstore/internal/events/types"
	"github.com/dlorenc/docstore/internal/model"
)

// ---------------------------------------------------------------------------
// Issue handlers
// ---------------------------------------------------------------------------

var (
	reMentionProposal = regexp.MustCompile(`\bproposal:([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\b`)
	reMentionCommit   = regexp.MustCompile(`\bcommit:(\d+)\b`)
	reMentionIssue    = regexp.MustCompile(`(?:^|[^&\w])#(\d+)`)
)

// parseMentions extracts cross-reference mentions from a text body.
// Returns proposal UUIDs, commit sequence numbers, and issue numbers.
func parseMentions(body string) (proposalIDs []string, commitSeqs []int64, issueNums []int64) {
	for _, m := range reMentionProposal.FindAllStringSubmatch(body, -1) {
		proposalIDs = append(proposalIDs, m[1])
	}
	for _, m := range reMentionCommit.FindAllStringSubmatch(body, -1) {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			commitSeqs = append(commitSeqs, n)
		}
	}
	for _, m := range reMentionIssue.FindAllStringSubmatch(body, -1) {
		// Skip 6-digit sequences that look like CSS hex colors (#rrggbb).
		if len(m[1]) == 6 {
			continue
		}
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			issueNums = append(issueNums, n)
		}
	}
	return
}

// upsertIssueBodyRefs creates issue_refs on issueNum for proposal and commit
// mentions found in body, ignoring duplicate and not-found errors.
func (s *server) upsertIssueBodyRefs(ctx context.Context, repo string, issueNum int64, body string) {
	proposalIDs, commitSeqs, _ := parseMentions(body)
	for _, pid := range proposalIDs {
		_, err := s.commitStore.CreateIssueRef(ctx, repo, issueNum, model.IssueRefTypeProposal, pid)
		if err != nil && !errors.Is(err, db.ErrIssueRefExists) && !errors.Is(err, db.ErrIssueNotFound) {
			slog.Warn("upsert issue ref", "repo", repo, "issue", issueNum, "ref_type", "proposal", "ref_id", pid, "error", err)
		}
	}
	for _, seq := range commitSeqs {
		refID := strconv.FormatInt(seq, 10)
		_, err := s.commitStore.CreateIssueRef(ctx, repo, issueNum, model.IssueRefTypeCommit, refID)
		if err != nil && !errors.Is(err, db.ErrIssueRefExists) && !errors.Is(err, db.ErrIssueNotFound) {
			slog.Warn("upsert issue ref", "repo", repo, "issue", issueNum, "ref_type", "commit", "ref_id", refID, "error", err)
		}
	}
}

// parseIssueNumber parses the "number" path value into an int64.
// Writes 400 and returns false on failure.
func parseIssueNumber(w http.ResponseWriter, r *http.Request) (int64, bool) {
	n, err := strconv.ParseInt(r.PathValue("number"), 10, 64)
	if err != nil || n <= 0 {
		writeError(w, http.StatusBadRequest, "invalid issue number")
		return 0, false
	}
	return n, true
}

// handleListIssues implements GET /repos/:name/issues
func (s *server) handleListIssues(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	state := r.URL.Query().Get("state")
	if state == "" {
		state = "open"
	}
	switch state {
	case "open", "closed", "all":
	default:
		writeError(w, http.StatusBadRequest, "invalid state; must be open, closed, or all")
		return
	}
	stateFilter := state
	if state == "all" {
		stateFilter = ""
	}
	authorFilter := r.URL.Query().Get("author")
	labelFilter := r.URL.Query().Get("label")

	issues, err := s.commitStore.ListIssues(r.Context(), repo, stateFilter, authorFilter, labelFilter)
	if err != nil {
		slog.Error("internal error", "op", "list_issues", "repo", repo, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, model.ListIssuesResponse{Issues: issues})
}

// handleCreateIssue implements POST /repos/:name/issues
func (s *server) handleCreateIssue(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	var req model.CreateIssueRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Title == "" {
		writeError(w, http.StatusBadRequest, "title is required")
		return
	}

	author := IdentityFromContext(r.Context())
	iss, err := s.commitStore.CreateIssue(r.Context(), repo, req.Title, req.Body, author, req.Labels)
	if err != nil {
		slog.Error("internal error", "op", "create_issue", "repo", repo, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		return
	}

	s.upsertIssueBodyRefs(r.Context(), repo, iss.Number, req.Body)
	s.emit(r.Context(), evtypes.IssueOpened{Repo: repo, IssueID: iss.ID, Number: iss.Number, Author: author})
	slog.Info("issue opened", "repo", repo, "number", iss.Number, "author", author)
	writeJSON(w, http.StatusCreated, model.CreateIssueResponse{ID: iss.ID, Number: iss.Number})
}

// handleGetIssue implements GET /repos/:name/issues/:number
func (s *server) handleGetIssue(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	number, ok := parseIssueNumber(w, r)
	if !ok {
		return
	}

	iss, err := s.commitStore.GetIssue(r.Context(), repo, number)
	if err != nil {
		if errors.Is(err, db.ErrIssueNotFound) {
			writeAPIError(w, ErrCodeIssueNotFound, http.StatusNotFound, "issue not found")
		} else {
			slog.Error("internal error", "op", "get_issue", "repo", repo, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}
	writeJSON(w, http.StatusOK, iss)
}

// handleUpdateIssue implements PATCH /repos/:name/issues/:number
func (s *server) handleUpdateIssue(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	number, ok := parseIssueNumber(w, r)
	if !ok {
		return
	}

	identity := IdentityFromContext(r.Context())
	role := RoleFromContext(r.Context())

	existing, err := s.commitStore.GetIssue(r.Context(), repo, number)
	if err != nil {
		if errors.Is(err, db.ErrIssueNotFound) {
			writeError(w, http.StatusNotFound, "issue not found")
		} else {
			slog.Error("internal error", "op", "update_issue", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	if existing.Author != identity && role != model.RoleMaintainer && role != model.RoleAdmin {
		writeError(w, http.StatusForbidden, "forbidden: must be issue author or maintainer")
		return
	}

	var req model.UpdateIssueRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	var labelsPtr *[]string
	if req.Labels != nil {
		labelsPtr = &req.Labels
	}
	iss, err := s.commitStore.UpdateIssue(r.Context(), repo, number, req.Title, req.Body, labelsPtr)
	if err != nil {
		if errors.Is(err, db.ErrIssueNotFound) {
			writeError(w, http.StatusNotFound, "issue not found")
		} else {
			slog.Error("internal error", "op", "update_issue", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	if req.Body != nil {
		s.upsertIssueBodyRefs(r.Context(), repo, number, *req.Body)
	}
	s.emit(r.Context(), evtypes.IssueUpdated{Repo: repo, IssueID: iss.ID, Number: iss.Number})
	writeJSON(w, http.StatusOK, iss)
}

// handleCloseIssue implements POST /repos/:name/issues/:number/close
func (s *server) handleCloseIssue(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	number, ok := parseIssueNumber(w, r)
	if !ok {
		return
	}

	identity := IdentityFromContext(r.Context())
	role := RoleFromContext(r.Context())

	existing, err := s.commitStore.GetIssue(r.Context(), repo, number)
	if err != nil {
		if errors.Is(err, db.ErrIssueNotFound) {
			writeError(w, http.StatusNotFound, "issue not found")
		} else {
			slog.Error("internal error", "op", "close_issue", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	if existing.Author != identity && role != model.RoleMaintainer && role != model.RoleAdmin {
		writeError(w, http.StatusForbidden, "forbidden: must be issue author or maintainer")
		return
	}

	if existing.State == model.IssueStateClosed {
		writeError(w, http.StatusConflict, "issue is already closed")
		return
	}

	var req model.CloseIssueRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	reason := req.Reason
	if reason == "" {
		reason = model.IssueCloseReasonCompleted
	}
	switch reason {
	case model.IssueCloseReasonCompleted, model.IssueCloseReasonNotPlanned, model.IssueCloseReasonDuplicate:
	default:
		writeError(w, http.StatusBadRequest, "invalid close reason")
		return
	}

	iss, err := s.commitStore.CloseIssue(r.Context(), repo, number, reason, identity)
	if err != nil {
		if errors.Is(err, db.ErrIssueNotFound) {
			writeError(w, http.StatusNotFound, "issue not found")
		} else {
			slog.Error("internal error", "op", "close_issue", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	s.emit(r.Context(), evtypes.IssueClosed{Repo: repo, IssueID: iss.ID, Number: iss.Number, Reason: string(reason), ClosedBy: identity})
	slog.Info("issue closed", "repo", repo, "number", number, "by", identity)
	writeJSON(w, http.StatusOK, iss)
}

// handleReopenIssue implements POST /repos/:name/issues/:number/reopen
func (s *server) handleReopenIssue(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	number, ok := parseIssueNumber(w, r)
	if !ok {
		return
	}

	identity := IdentityFromContext(r.Context())
	role := RoleFromContext(r.Context())

	existing, err := s.commitStore.GetIssue(r.Context(), repo, number)
	if err != nil {
		if errors.Is(err, db.ErrIssueNotFound) {
			writeError(w, http.StatusNotFound, "issue not found")
		} else {
			slog.Error("internal error", "op", "reopen_issue", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	if existing.Author != identity && role != model.RoleMaintainer && role != model.RoleAdmin {
		writeError(w, http.StatusForbidden, "forbidden: must be issue author or maintainer")
		return
	}

	if existing.State == model.IssueStateOpen {
		writeError(w, http.StatusConflict, "issue is already open")
		return
	}

	iss, err := s.commitStore.ReopenIssue(r.Context(), repo, number)
	if err != nil {
		if errors.Is(err, db.ErrIssueNotFound) {
			writeError(w, http.StatusNotFound, "issue not found")
		} else {
			slog.Error("internal error", "op", "reopen_issue", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	s.emit(r.Context(), evtypes.IssueReopened{Repo: repo, IssueID: iss.ID, Number: iss.Number})
	slog.Info("issue reopened", "repo", repo, "number", number)
	writeJSON(w, http.StatusOK, iss)
}

// handleListIssueComments implements GET /repos/:name/issues/:number/comments
func (s *server) handleListIssueComments(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	number, ok := parseIssueNumber(w, r)
	if !ok {
		return
	}

	comments, err := s.commitStore.ListIssueComments(r.Context(), repo, number)
	if err != nil {
		slog.Error("internal error", "op", "list_issue_comments", "repo", repo, "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, model.ListIssueCommentsResponse{Comments: comments})
}

// handleCreateIssueComment implements POST /repos/:name/issues/:number/comments
func (s *server) handleCreateIssueComment(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	number, ok := parseIssueNumber(w, r)
	if !ok {
		return
	}

	var req model.CreateIssueCommentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Body == "" {
		writeError(w, http.StatusBadRequest, "body is required")
		return
	}

	author := IdentityFromContext(r.Context())
	c, err := s.commitStore.CreateIssueComment(r.Context(), repo, number, req.Body, author)
	if err != nil {
		if errors.Is(err, db.ErrIssueNotFound) {
			writeError(w, http.StatusNotFound, "issue not found")
		} else {
			slog.Error("internal error", "op", "create_issue_comment", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	s.upsertIssueBodyRefs(r.Context(), repo, number, req.Body)
	s.emit(r.Context(), evtypes.IssueCommentCreated{Repo: repo, IssueID: c.IssueID, Number: number, CommentID: c.ID, Author: author})
	slog.Info("issue comment created", "repo", repo, "issue", number, "comment", c.ID, "author", author)
	writeJSON(w, http.StatusCreated, model.CreateIssueCommentResponse{ID: c.ID})
}

// handleUpdateIssueComment implements PATCH /repos/:name/issues/:number/comments/:commentID
func (s *server) handleUpdateIssueComment(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	number, ok := parseIssueNumber(w, r)
	if !ok {
		return
	}

	commentID := r.PathValue("commentID")
	identity := IdentityFromContext(r.Context())
	role := RoleFromContext(r.Context())

	existing, err := s.commitStore.GetIssueComment(r.Context(), repo, commentID)
	if err != nil {
		if errors.Is(err, db.ErrIssueCommentNotFound) {
			writeError(w, http.StatusNotFound, "comment not found")
		} else {
			slog.Error("internal error", "op", "update_issue_comment", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	issue, err := s.commitStore.GetIssue(r.Context(), repo, number)
	if err != nil {
		if errors.Is(err, db.ErrIssueNotFound) {
			writeError(w, http.StatusNotFound, "comment not found")
		} else {
			slog.Error("internal error", "op", "update_issue_comment", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}
	if existing.IssueID != issue.ID {
		writeError(w, http.StatusNotFound, "comment not found")
		return
	}

	if existing.Author != identity && role != model.RoleMaintainer && role != model.RoleAdmin {
		writeError(w, http.StatusForbidden, "forbidden: must be comment author or maintainer")
		return
	}

	var req model.UpdateIssueCommentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Body == "" {
		writeError(w, http.StatusBadRequest, "body is required")
		return
	}

	c, err := s.commitStore.UpdateIssueComment(r.Context(), repo, commentID, req.Body)
	if err != nil {
		if errors.Is(err, db.ErrIssueCommentNotFound) {
			writeError(w, http.StatusNotFound, "comment not found")
		} else {
			slog.Error("internal error", "op", "update_issue_comment", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	s.upsertIssueBodyRefs(r.Context(), repo, number, req.Body)
	s.emit(r.Context(), evtypes.IssueCommentUpdated{Repo: repo, IssueID: c.IssueID, Number: number, CommentID: c.ID})
	writeJSON(w, http.StatusOK, c)
}

// handleDeleteIssueComment implements DELETE /repos/:name/issues/:number/comments/:commentID
func (s *server) handleDeleteIssueComment(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	number, ok := parseIssueNumber(w, r)
	if !ok {
		return
	}

	commentID := r.PathValue("commentID")
	identity := IdentityFromContext(r.Context())
	role := RoleFromContext(r.Context())

	existing, err := s.commitStore.GetIssueComment(r.Context(), repo, commentID)
	if err != nil {
		if errors.Is(err, db.ErrIssueCommentNotFound) {
			writeError(w, http.StatusNotFound, "comment not found")
		} else {
			slog.Error("internal error", "op", "delete_issue_comment", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	issue, err := s.commitStore.GetIssue(r.Context(), repo, number)
	if err != nil {
		if errors.Is(err, db.ErrIssueNotFound) {
			writeError(w, http.StatusNotFound, "comment not found")
		} else {
			slog.Error("internal error", "op", "delete_issue_comment", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}
	if existing.IssueID != issue.ID {
		writeError(w, http.StatusNotFound, "comment not found")
		return
	}

	if existing.Author != identity && role != model.RoleMaintainer && role != model.RoleAdmin {
		writeError(w, http.StatusForbidden, "forbidden: must be comment author or maintainer")
		return
	}

	if err := s.commitStore.DeleteIssueComment(r.Context(), repo, commentID); err != nil {
		if errors.Is(err, db.ErrIssueCommentNotFound) {
			writeError(w, http.StatusNotFound, "comment not found")
		} else {
			slog.Error("internal error", "op", "delete_issue_comment", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListIssueRefs implements GET /repos/:name/issues/:number/refs
func (s *server) handleListIssueRefs(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	number, ok := parseIssueNumber(w, r)
	if !ok {
		return
	}

	refs, err := s.commitStore.ListIssueRefs(r.Context(), repo, number)
	if err != nil {
		slog.Error("internal error", "op", "list_issue_refs", "repo", repo, "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, model.ListIssueRefsResponse{Refs: refs})
}

// handleAddIssueRef implements POST /repos/:name/issues/:number/refs
func (s *server) handleAddIssueRef(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	number, ok := parseIssueNumber(w, r)
	if !ok {
		return
	}

	var req model.AddIssueRefRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	switch req.RefType {
	case model.IssueRefTypeProposal, model.IssueRefTypeCommit:
	default:
		writeError(w, http.StatusBadRequest, "invalid ref_type; must be proposal or commit")
		return
	}
	if req.RefID == "" {
		writeError(w, http.StatusBadRequest, "ref_id is required")
		return
	}

	ref, err := s.commitStore.CreateIssueRef(r.Context(), repo, number, req.RefType, req.RefID)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrIssueNotFound):
			writeError(w, http.StatusNotFound, "issue not found")
		case errors.Is(err, db.ErrIssueRefExists):
			writeError(w, http.StatusConflict, "ref already exists")
		default:
			slog.Error("internal error", "op", "add_issue_ref", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}
	writeJSON(w, http.StatusCreated, model.AddIssueRefResponse{ID: ref.ID})
}

// handleProposalIssues implements GET /repos/:name/proposals/:proposalID/issues
func (s *server) handleProposalIssues(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	proposalID := r.PathValue("proposalID")
	issues, err := s.commitStore.ListIssuesByRef(r.Context(), repo, model.IssueRefTypeProposal, proposalID)
	if err != nil {
		slog.Error("internal error", "op", "proposal_issues", "repo", repo, "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, model.ListIssuesResponse{Issues: issues})
}

// handleCommitIssues implements GET /repos/:name/commit/:sequence/issues
func (s *server) handleCommitIssues(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	seqStr := r.PathValue("sequence")
	if _, err := strconv.ParseInt(seqStr, 10, 64); err != nil {
		writeError(w, http.StatusBadRequest, "invalid sequence number")
		return
	}
	issues, err := s.commitStore.ListIssuesByRef(r.Context(), repo, model.IssueRefTypeCommit, seqStr)
	if err != nil {
		slog.Error("internal error", "op", "commit_issues", "repo", repo, "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, model.ListIssuesResponse{Issues: issues})
}
