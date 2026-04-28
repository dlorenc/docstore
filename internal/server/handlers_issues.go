package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/service"
)

// ---------------------------------------------------------------------------
// Issue handlers
// ---------------------------------------------------------------------------

// parseMentions extracts cross-reference mentions from a text body.
// Returns proposal UUIDs, commit sequence numbers, and issue numbers.
func parseMentions(body string) ([]string, []int64, []int64) {
	return service.ParseMentions(body)
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

	identity := IdentityFromContext(r.Context())
	iss, err := s.svc.CreateIssue(r.Context(), identity, repo, req.Title, req.Body, req.Labels)
	if err != nil {
		slog.Error("internal error", "op", "create_issue", "repo", repo, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		return
	}

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

	var req model.UpdateIssueRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	var labelsPtr *[]string
	if req.Labels != nil {
		labelsPtr = &req.Labels
	}
	iss, err := s.svc.UpdateIssue(r.Context(), identity, role, repo, number, req.Title, req.Body, labelsPtr)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrForbidden):
			writeAPIError(w, ErrCodeForbidden, http.StatusForbidden, err.Error())
		case errors.Is(err, db.ErrIssueNotFound):
			writeAPIError(w, ErrCodeIssueNotFound, http.StatusNotFound, "issue not found")
		default:
			slog.Error("internal error", "op", "update_issue", "repo", repo, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

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

	iss, err := s.svc.CloseIssue(r.Context(), identity, role, repo, number, reason)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrForbidden):
			writeAPIError(w, ErrCodeForbidden, http.StatusForbidden, err.Error())
		case errors.Is(err, service.ErrConflict):
			writeAPIError(w, ErrCodeConflict, http.StatusConflict, err.Error())
		case errors.Is(err, db.ErrIssueNotFound):
			writeAPIError(w, ErrCodeIssueNotFound, http.StatusNotFound, "issue not found")
		default:
			slog.Error("internal error", "op", "close_issue", "repo", repo, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

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

	iss, err := s.svc.ReopenIssue(r.Context(), identity, role, repo, number)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrForbidden):
			writeAPIError(w, ErrCodeForbidden, http.StatusForbidden, err.Error())
		case errors.Is(err, service.ErrConflict):
			writeAPIError(w, ErrCodeConflict, http.StatusConflict, err.Error())
		case errors.Is(err, db.ErrIssueNotFound):
			writeAPIError(w, ErrCodeIssueNotFound, http.StatusNotFound, "issue not found")
		default:
			slog.Error("internal error", "op", "reopen_issue", "repo", repo, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

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

	identity := IdentityFromContext(r.Context())
	c, err := s.svc.CreateIssueComment(r.Context(), identity, repo, number, req.Body)
	if err != nil {
		if errors.Is(err, db.ErrIssueNotFound) {
			writeAPIError(w, ErrCodeIssueNotFound, http.StatusNotFound, "issue not found")
		} else {
			slog.Error("internal error", "op", "create_issue_comment", "repo", repo, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

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

	var req model.UpdateIssueCommentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Body == "" {
		writeError(w, http.StatusBadRequest, "body is required")
		return
	}

	c, err := s.svc.UpdateIssueComment(r.Context(), identity, role, repo, number, commentID, req.Body)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrForbidden):
			writeAPIError(w, ErrCodeForbidden, http.StatusForbidden, err.Error())
		case errors.Is(err, db.ErrIssueCommentNotFound):
			writeAPIError(w, ErrCodeCommentNotFound, http.StatusNotFound, "comment not found")
		default:
			slog.Error("internal error", "op", "update_issue_comment", "repo", repo, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	writeJSON(w, http.StatusOK, c)
}

// handleDeleteIssueComment implements DELETE /repos/:name/issues/:number/comments/:commentID
func (s *server) handleDeleteIssueComment(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	_, ok := parseIssueNumber(w, r)
	if !ok {
		return
	}

	commentID := r.PathValue("commentID")
	identity := IdentityFromContext(r.Context())
	role := RoleFromContext(r.Context())

	if err := s.svc.DeleteIssueComment(r.Context(), identity, role, repo, commentID); err != nil {
		switch {
		case errors.Is(err, service.ErrForbidden):
			writeAPIError(w, ErrCodeForbidden, http.StatusForbidden, err.Error())
		case errors.Is(err, db.ErrIssueCommentNotFound):
			writeAPIError(w, ErrCodeCommentNotFound, http.StatusNotFound, "comment not found")
		default:
			slog.Error("internal error", "op", "delete_issue_comment", "repo", repo, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
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
