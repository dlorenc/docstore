package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/dlorenc/docstore/internal/db"
	evtypes "github.com/dlorenc/docstore/internal/events/types"
	mergeutil "github.com/dlorenc/docstore/internal/merge"
	"github.com/dlorenc/docstore/internal/model"
)

// ---------------------------------------------------------------------------
// Review and check handlers
// ---------------------------------------------------------------------------

// handleReview implements POST /repos/:name/review
func (s *server) handleReview(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	reviewer := IdentityFromContext(r.Context())

	var req model.CreateReviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Branch == "" {
		writeError(w, http.StatusBadRequest, "branch is required")
		return
	}
	if req.Status == "" {
		writeError(w, http.StatusBadRequest, "status is required")
		return
	}

	review, err := s.commitStore.CreateReview(r.Context(), repo, req.Branch, reviewer, req.Status, req.Body)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrBranchNotFound):
			writeAPIError(w, ErrCodeBranchNotFound, http.StatusNotFound, "branch not found")
		case errors.Is(err, db.ErrSelfApproval):
			writeAPIError(w, ErrCodeForbidden, http.StatusForbidden, "reviewer cannot approve their own commits")
		default:
			slog.Error("internal error", "op", "review", "repo", repo, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	bs, bsErr := s.computeBranchStatus(r.Context(), repo, req.Branch, reviewer)
	if bsErr != nil {
		slog.Warn("branch status computation failed", "op", "review", "repo", repo, "branch", req.Branch, "error", bsErr)
	}
	s.emit(r.Context(), evtypes.ReviewSubmitted{
		Repo:         repo,
		Branch:       req.Branch,
		Sequence:     review.Sequence,
		Reviewer:     reviewer,
		Status:       string(req.Status),
		BranchStatus: bs,
	})
	slog.Info("review submitted", "repo", repo, "branch", req.Branch, "reviewer", reviewer, "status", req.Status)
	writeJSON(w, http.StatusCreated, model.CreateReviewResponse{
		ID:       review.ID,
		Sequence: review.Sequence,
	})
}

// handleCheck implements POST /repos/:name/check
func (s *server) handleCheck(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	reporter := IdentityFromContext(r.Context())

	var req model.CreateCheckRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Branch == "" {
		writeError(w, http.StatusBadRequest, "branch is required")
		return
	}
	if req.CheckName == "" {
		writeError(w, http.StatusBadRequest, "check_name is required")
		return
	}
	if req.Status == "" {
		writeError(w, http.StatusBadRequest, "status is required")
		return
	}

	attempt := int16(1)
	if req.Attempt != nil {
		attempt = *req.Attempt
	}
	cr, err := s.commitStore.CreateCheckRun(r.Context(), repo, req.Branch, req.CheckName, req.Status, reporter, req.LogURL, req.Sequence, attempt, req.Metadata)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrBranchNotFound):
			writeAPIError(w, ErrCodeBranchNotFound, http.StatusNotFound, "branch not found")
		default:
			slog.Error("internal error", "op", "check", "repo", repo, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	bs, bsErr := s.computeBranchStatus(r.Context(), repo, req.Branch, reporter)
	if bsErr != nil {
		slog.Warn("branch status computation failed", "op", "check", "repo", repo, "branch", req.Branch, "error", bsErr)
	}
	s.emit(r.Context(), evtypes.CheckReported{
		Repo:         repo,
		Branch:       req.Branch,
		Sequence:     cr.Sequence,
		CheckName:    req.CheckName,
		Status:       string(req.Status),
		Reporter:     reporter,
		BranchStatus: bs,
	})
	writeJSON(w, http.StatusCreated, model.CreateCheckRunResponse{
		ID:       cr.ID,
		Sequence: cr.Sequence,
		LogURL:   req.LogURL,
	})
}

// handleGetReviews serves GET /repos/:name/branch/:branch/reviews
func (s *server) handleGetReviews(w http.ResponseWriter, r *http.Request, repo, branch string) {
	var atSeq *int64
	if v := r.URL.Query().Get("at"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid 'at' parameter")
			return
		}
		atSeq = &n
	}

	reviews, err := s.commitStore.ListReviews(r.Context(), repo, branch, atSeq)
	if err != nil {
		slog.Error("internal error", "op", "list_reviews", "repo", repo, "branch", branch, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		return
	}
	if reviews == nil {
		reviews = []model.Review{}
	}
	writeJSON(w, http.StatusOK, reviews)
}

// handleGetChecks serves GET /repos/:name/branch/:branch/checks
func (s *server) handleGetChecks(w http.ResponseWriter, r *http.Request, repo, branch string) {
	var atSeq *int64
	if v := r.URL.Query().Get("at"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid 'at' parameter")
			return
		}
		atSeq = &n
	}
	history := r.URL.Query().Get("history") == "true"

	checkRuns, err := s.commitStore.ListCheckRuns(r.Context(), repo, branch, atSeq, history)
	if err != nil {
		slog.Error("internal error", "op", "list_checks", "repo", repo, "branch", branch, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		return
	}
	if checkRuns == nil {
		checkRuns = []model.CheckRun{}
	}
	writeJSON(w, http.StatusOK, checkRuns)
}

// handleRetryChecks implements POST /repos/:name/-/checks/retry
func (s *server) handleRetryChecks(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	var req model.RetryChecksRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Branch == "" {
		writeError(w, http.StatusBadRequest, "branch is required")
		return
	}

	attempt, err := s.commitStore.RetryChecks(r.Context(), repo, req.Branch, req.Sequence, req.Checks)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrBranchNotFound):
			writeAPIError(w, ErrCodeBranchNotFound, http.StatusNotFound, "branch not found")
		default:
			slog.Error("internal error", "op", "retry_checks", "repo", repo, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	s.emit(r.Context(), evtypes.CheckRunRetry{
		Repo:     repo,
		Branch:   req.Branch,
		Sequence: req.Sequence,
		Checks:   req.Checks,
		Attempt:  attempt,
	})
	writeJSON(w, http.StatusAccepted, model.RetryChecksResponse{Attempt: attempt})
}

// ---------------------------------------------------------------------------
// Review comment handlers
// ---------------------------------------------------------------------------

// handleCreateReviewComment implements POST /repos/:name/comment
func (s *server) handleCreateReviewComment(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}
	author := IdentityFromContext(r.Context())

	var req model.CreateReviewCommentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Branch == "" {
		writeError(w, http.StatusBadRequest, "branch is required")
		return
	}
	if req.Path == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	if req.VersionID == "" {
		writeError(w, http.StatusBadRequest, "version_id is required")
		return
	}
	if req.Body == "" {
		writeError(w, http.StatusBadRequest, "body is required")
		return
	}

	comment, err := s.commitStore.CreateReviewComment(r.Context(), repo, req.Branch, req.Path, req.VersionID, req.Body, author, req.ReviewID)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrBranchNotFound):
			writeAPIError(w, ErrCodeBranchNotFound, http.StatusNotFound, "branch not found")
		default:
			slog.Error("internal error", "op", "create_review_comment", "repo", repo, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	slog.Info("review comment created", "repo", repo, "branch", req.Branch, "path", req.Path, "author", author)
	writeJSON(w, http.StatusCreated, model.CreateReviewCommentResponse{
		ID:       comment.ID,
		Sequence: comment.Sequence,
	})
}

// handleListReviewComments serves GET /repos/:name/branch/:branch/comments
func (s *server) handleListReviewComments(w http.ResponseWriter, r *http.Request, repo, branch string) {
	var path *string
	if v := r.URL.Query().Get("path"); v != "" {
		path = &v
	}

	comments, err := s.commitStore.ListReviewComments(r.Context(), repo, branch, path)
	if err != nil {
		slog.Error("internal error", "op", "list_review_comments", "repo", repo, "branch", branch, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		return
	}
	if comments == nil {
		comments = []model.ReviewComment{}
	}
	writeJSON(w, http.StatusOK, comments)
}

// handleDeleteReviewComment implements DELETE /repos/:name/comment/:commentID
func (s *server) handleDeleteReviewComment(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}
	commentID := r.PathValue("commentID")
	identity := IdentityFromContext(r.Context())
	role := RoleFromContext(r.Context())

	comment, err := s.commitStore.GetReviewComment(r.Context(), repo, commentID)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrCommentNotFound):
			writeAPIError(w, ErrCodeCommentNotFound, http.StatusNotFound, "comment not found")
		default:
			slog.Error("internal error", "op", "get_review_comment", "repo", repo, "comment", commentID, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	// Only the comment author or a maintainer+ may delete a comment.
	if comment.Author != identity && role != model.RoleMaintainer && role != model.RoleAdmin {
		writeAPIError(w, ErrCodeForbidden, http.StatusForbidden, "forbidden: must be comment author or maintainer")
		return
	}

	if err := s.commitStore.DeleteReviewComment(r.Context(), repo, commentID); err != nil {
		slog.Error("internal error", "op", "delete_review_comment", "repo", repo, "comment", commentID, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		return
	}

	slog.Info("review comment deleted", "repo", repo, "comment", commentID, "by", identity)
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Proposal handlers
// ---------------------------------------------------------------------------

// handleCreateProposal implements POST /repos/:name/proposals
func (s *server) handleCreateProposal(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	author := IdentityFromContext(r.Context())

	var req model.CreateProposalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Branch == "" {
		writeError(w, http.StatusBadRequest, "branch is required")
		return
	}
	if req.Title == "" {
		writeError(w, http.StatusBadRequest, "title is required")
		return
	}
	baseBranch := req.BaseBranch
	if baseBranch == "" {
		baseBranch = "main"
	}

	p, err := s.commitStore.CreateProposal(r.Context(), repo, req.Branch, baseBranch, req.Title, req.Description, author)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrProposalExists):
			writeAPIError(w, ErrCodeProposalExists, http.StatusConflict, "branch already has an open proposal")
		case errors.Is(err, db.ErrBranchNotFound):
			writeAPIError(w, ErrCodeBranchNotFound, http.StatusNotFound, "branch not found")
		default:
			slog.Error("internal error", "op", "create_proposal", "repo", repo, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	var headSeq int64
	if bi, err := s.readStore.GetBranch(r.Context(), repo, req.Branch); err == nil {
		headSeq = bi.HeadSequence
	} else {
		slog.Warn("could not fetch branch head for proposal event", "repo", repo, "branch", req.Branch, "error", err)
	}
	s.emit(r.Context(), evtypes.ProposalOpened{
		Repo:       repo,
		Branch:     req.Branch,
		BaseBranch: baseBranch,
		ProposalID: p.ID,
		Author:     author,
		Sequence:   headSeq,
	})
	s.upsertProposalMentionRefs(r.Context(), repo, p.ID, req.Description)
	slog.Info("proposal opened", "repo", repo, "branch", req.Branch, "proposal_id", p.ID, "author", author)
	writeJSON(w, http.StatusCreated, model.CreateProposalResponse{ID: p.ID})
}

// handleListProposals implements GET /repos/:name/proposals
func (s *server) handleListProposals(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	var state *model.ProposalState
	if sv := r.URL.Query().Get("state"); sv != "" {
		ps := model.ProposalState(sv)
		switch ps {
		case model.ProposalOpen, model.ProposalClosed, model.ProposalMerged:
			state = &ps
		default:
			writeError(w, http.StatusBadRequest, "invalid state; must be open, closed, or merged")
			return
		}
	}

	var branch *string
	if bv := r.URL.Query().Get("branch"); bv != "" {
		branch = &bv
	}

	proposals, err := s.commitStore.ListProposals(r.Context(), repo, state, branch)
	if err != nil {
		slog.Error("internal error", "op", "list_proposals", "repo", repo, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		return
	}
	if proposals == nil {
		proposals = []*model.Proposal{}
	}
	writeJSON(w, http.StatusOK, proposals)
}

// handleGetProposal implements GET /repos/:name/proposals/:proposalID
func (s *server) handleGetProposal(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	proposalID := r.PathValue("proposalID")
	p, err := s.commitStore.GetProposal(r.Context(), repo, proposalID)
	if err != nil {
		if errors.Is(err, db.ErrProposalNotFound) {
			writeAPIError(w, ErrCodeProposalNotFound, http.StatusNotFound, "proposal not found")
		} else {
			slog.Error("internal error", "op", "get_proposal", "repo", repo, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// handleUpdateProposal implements PATCH /repos/:name/proposals/:proposalID
func (s *server) handleUpdateProposal(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	proposalID := r.PathValue("proposalID")
	identity := IdentityFromContext(r.Context())
	role := RoleFromContext(r.Context())

	// Fetch existing proposal to check authz.
	existing, err := s.commitStore.GetProposal(r.Context(), repo, proposalID)
	if err != nil {
		if errors.Is(err, db.ErrProposalNotFound) {
			writeAPIError(w, ErrCodeProposalNotFound, http.StatusNotFound, "proposal not found")
		} else {
			slog.Error("internal error", "op", "update_proposal", "repo", repo, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	if existing.Author != identity && role != model.RoleMaintainer && role != model.RoleAdmin {
		writeAPIError(w, ErrCodeForbidden, http.StatusForbidden, "forbidden: must be proposal author or maintainer")
		return
	}

	var req model.UpdateProposalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	p, err := s.commitStore.UpdateProposal(r.Context(), repo, proposalID, req.Title, req.Description)
	if err != nil {
		if errors.Is(err, db.ErrProposalNotFound) {
			writeAPIError(w, ErrCodeProposalNotFound, http.StatusNotFound, "proposal not found")
		} else {
			slog.Error("internal error", "op", "update_proposal", "repo", repo, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}
	if req.Description != nil {
		s.upsertProposalMentionRefs(r.Context(), repo, proposalID, *req.Description)
	}
	writeJSON(w, http.StatusOK, p)
}

// handleCloseProposal implements POST /repos/:name/proposals/:proposalID/close
func (s *server) handleCloseProposal(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	proposalID := r.PathValue("proposalID")
	identity := IdentityFromContext(r.Context())
	role := RoleFromContext(r.Context())

	// Fetch existing proposal to check authz.
	existing, err := s.commitStore.GetProposal(r.Context(), repo, proposalID)
	if err != nil {
		if errors.Is(err, db.ErrProposalNotFound) {
			writeAPIError(w, ErrCodeProposalNotFound, http.StatusNotFound, "proposal not found")
		} else {
			slog.Error("internal error", "op", "close_proposal", "repo", repo, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	if existing.Author != identity && role != model.RoleMaintainer && role != model.RoleAdmin {
		writeAPIError(w, ErrCodeForbidden, http.StatusForbidden, "forbidden: must be proposal author or maintainer")
		return
	}

	if err := s.commitStore.CloseProposal(r.Context(), repo, proposalID); err != nil {
		if errors.Is(err, db.ErrProposalNotFound) {
			writeAPIError(w, ErrCodeProposalNotFound, http.StatusNotFound, "proposal not found")
		} else {
			slog.Error("internal error", "op", "close_proposal", "repo", repo, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	s.emit(r.Context(), evtypes.ProposalClosed{
		Repo:       repo,
		Branch:     existing.Branch,
		ProposalID: proposalID,
	})
	slog.Info("proposal closed", "repo", repo, "proposal_id", proposalID, "by", identity)
	w.WriteHeader(http.StatusNoContent)
}

// handleMerge implements POST /repos/:name/merge
func (s *server) handleMerge(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	var req model.MergeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Repo = repo

	if req.Branch == "" {
		writeError(w, http.StatusBadRequest, "branch is required")
		return
	}
	if req.Branch == "main" {
		writeError(w, http.StatusBadRequest, "cannot merge main into itself")
		return
	}

	// Author always comes from the authenticated identity; body value is ignored.
	req.Author = IdentityFromContext(r.Context())

	// Policy evaluation: runs before any database transaction.
	if denied, err := s.evaluateMergePolicy(r.Context(), repo, req.Branch, req.Author); err != nil {
		slog.Error("policy evaluation error", "op", "merge", "repo", repo, "branch", req.Branch, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "policy evaluation error")
		return
	} else if denied != nil {
		slog.Warn("merge denied by policy", "repo", repo, "branch", req.Branch, "actor", req.Author)
		// Collect policy names for event.
		policyNames := make([]string, len(denied))
		for i, p := range denied {
			policyNames[i] = p.Name
		}
		s.emit(r.Context(), evtypes.MergeBlocked{
			Repo:     repo,
			Branch:   req.Branch,
			Actor:    req.Author,
			Policies: policyNames,
		})
		writeJSON(w, http.StatusForbidden, model.MergePolicyError{Policies: denied})
		return
	}

	resp, conflicts, err := s.commitStore.Merge(r.Context(), req)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrMergeConflict):
			slog.Warn("merge conflict", "repo", repo, "branch", req.Branch, "conflicts", len(conflicts))
			// Convert conflicts to API response.
			apiConflicts := make([]model.ConflictEntry, len(conflicts))
			for i, c := range conflicts {
				apiConflicts[i] = model.ConflictEntry{
					Path:            c.Path,
					MainVersionID:   c.MainVersionID,
					BranchVersionID: c.BranchVersionID,
				}
			}
			writeJSON(w, http.StatusConflict, model.MergeConflictError{Conflicts: apiConflicts})
		case errors.Is(err, db.ErrBranchNotFound):
			writeAPIError(w, ErrCodeBranchNotFound, http.StatusNotFound, "branch not found")
		case errors.Is(err, db.ErrBranchNotActive):
			writeAPIError(w, ErrCodeBranchNotActive, http.StatusConflict, "branch is not active")
		case errors.Is(err, db.ErrBranchDraft):
			writeAPIError(w, ErrCodeBranchDraft, http.StatusConflict, "branch is in draft state")
		default:
			slog.Error("internal error", "op", "merge", "repo", repo, "branch", req.Branch, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	// Dry-run: skip all side effects and return the computed sequence.
	if req.DryRun {
		slog.Info("merge dry-run", "repo", repo, "branch", req.Branch, "by", req.Author, "sequence", resp.Sequence)
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// Invalidate the policy cache: the merge may have updated policies on main.
	if s.policyCache != nil {
		s.policyCache.Invalidate(repo)
	}

	// Best-effort: transition any open proposal for this branch to merged.
	if mp, mergeProposalErr := s.commitStore.MergeProposal(r.Context(), repo, req.Branch); mergeProposalErr != nil {
		slog.Warn("failed to merge proposal", "repo", repo, "branch", req.Branch, "error", mergeProposalErr)
	} else if mp != nil {
		s.emit(r.Context(), evtypes.ProposalMerged{
			Repo:       repo,
			Branch:     req.Branch,
			BaseBranch: mp.BaseBranch,
			ProposalID: mp.ID,
		})
	}

	bsMerge, bsMergeErr := s.computeBranchStatus(r.Context(), repo, req.Branch, req.Author)
	if bsMergeErr != nil {
		slog.Warn("branch status computation failed", "op", "merge", "repo", repo, "branch", req.Branch, "error", bsMergeErr)
	}
	s.emit(r.Context(), evtypes.BranchMerged{
		Repo:         repo,
		Branch:       req.Branch,
		Sequence:     resp.Sequence,
		MergedBy:     req.Author,
		BranchStatus: bsMerge,
	})
	slog.Info("branch merged", "repo", repo, "branch", req.Branch, "by", req.Author, "sequence", resp.Sequence)
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// Policy helpers
// ---------------------------------------------------------------------------

// evaluateMergePolicy evaluates all OPA policies for a pending merge.
// Returns the failing PolicyResults (non-nil means denied), or nil if allowed.
// Returns an error only for unexpected infrastructure failures.
// When readStore or policyCache are nil, or no policies exist, the merge is allowed.
func (s *server) evaluateMergePolicy(ctx context.Context, repo, branch, actor string) ([]model.PolicyResult, error) {
	return mergeutil.EvaluatePolicy(ctx, s.policyCache, s.readStore, s.commitStore, repo, branch, actor)
}

// ---------------------------------------------------------------------------
// Cross-reference helpers
// ---------------------------------------------------------------------------

// upsertProposalMentionRefs creates issue_refs for issues mentioned in a proposal body.
// When a proposal body contains "#N", an issue_ref of type "proposal" pointing at proposalID
// is upserted on issue N.
func (s *server) upsertProposalMentionRefs(ctx context.Context, repo, proposalID, body string) {
	_, _, issueNums := parseMentions(body)
	for _, num := range issueNums {
		_, err := s.commitStore.CreateIssueRef(ctx, repo, num, model.IssueRefTypeProposal, proposalID)
		if err != nil && !errors.Is(err, db.ErrIssueRefExists) && !errors.Is(err, db.ErrIssueNotFound) {
			slog.Warn("upsert proposal mention ref", "repo", repo, "issue", num, "proposal", proposalID, "error", err)
		}
	}
}
