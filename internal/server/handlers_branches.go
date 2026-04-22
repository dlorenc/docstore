package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/dlorenc/docstore/internal/db"
	evtypes "github.com/dlorenc/docstore/internal/events/types"
	mergeutil "github.com/dlorenc/docstore/internal/merge"
	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/policy"
	"github.com/dlorenc/docstore/internal/store"
)

// ---------------------------------------------------------------------------
// Branch handlers
// ---------------------------------------------------------------------------

// handleBranches implements GET /repos/:name/branches?status=active[&include_draft=true][&draft=true]
func (s *server) handleBranches(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}
	statusFilter := r.URL.Query().Get("status")
	includeDraft := r.URL.Query().Get("include_draft") == "true"
	onlyDraft := r.URL.Query().Get("draft") == "true"

	branches, err := s.readStore.ListBranches(r.Context(), repo, statusFilter, includeDraft, onlyDraft)
	if err != nil {
		slog.Error("internal error", "op", "list_branches", "repo", repo, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		return
	}
	if branches == nil {
		branches = []store.BranchInfo{}
	}
	writeJSON(w, http.StatusOK, branches)
}

// handleCreateBranch implements POST /repos/:name/branch
func (s *server) handleCreateBranch(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")

	var req model.CreateBranchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Repo = repo

	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Name == "main" {
		writeError(w, http.StatusBadRequest, "cannot create branch named 'main'")
		return
	}

	resp, err := s.commitStore.CreateBranch(r.Context(), req)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrBranchExists):
			writeAPIError(w, ErrCodeBranchExists, http.StatusConflict, "branch already exists")
		case errors.Is(err, db.ErrRepoNotFound):
			writeAPIError(w, ErrCodeRepoNotFound, http.StatusNotFound, "repo not found")
		default:
			slog.Error("internal error", "op", "create_branch", "repo", repo, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	identity := IdentityFromContext(r.Context())
	s.emit(r.Context(), evtypes.BranchCreated{
		Repo:         repo,
		Branch:       req.Name,
		BaseSequence: resp.BaseSequence,
		CreatedBy:    identity,
	})
	slog.Info("branch created", "repo", repo, "branch", req.Name, "by", identity)
	writeJSON(w, http.StatusCreated, resp)
}

// handleUpdateBranch implements PATCH /repos/:name/branch/:bname
func (s *server) handleUpdateBranch(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	bname := r.PathValue("bname")

	if !s.validateRepo(w, r, repo) {
		return
	}
	if bname == "main" {
		writeError(w, http.StatusBadRequest, "cannot modify branch 'main'")
		return
	}

	if !s.checkBranchIfMatch(w, r, repo, bname) {
		return
	}

	var req model.UpdateBranchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := s.commitStore.UpdateBranchDraft(r.Context(), repo, bname, req.Draft); err != nil {
		switch {
		case errors.Is(err, db.ErrBranchNotFound):
			writeAPIError(w, ErrCodeBranchNotFound, http.StatusNotFound, "branch not found")
		default:
			slog.Error("internal error", "op", "update_branch", "repo", repo, "branch", bname, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	slog.Info("branch updated", "repo", repo, "branch", bname, "draft", req.Draft, "by", IdentityFromContext(r.Context()))
	writeJSON(w, http.StatusOK, model.UpdateBranchResponse{Name: bname, Draft: req.Draft})
}

// handleEnableAutoMerge implements POST /repos/:name/-/branch/:bname/auto-merge (writer+)
// Enables auto-merge on the branch. When all policies are satisfied and CI checks
// pass, the branch will be automatically merged.
func (s *server) handleEnableAutoMerge(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	bname := r.PathValue("bname")

	if !s.validateRepo(w, r, repo) {
		return
	}
	if bname == "main" {
		writeError(w, http.StatusBadRequest, "cannot enable auto-merge on branch 'main'")
		return
	}

	if !s.checkBranchIfMatch(w, r, repo, bname) {
		return
	}

	if err := s.commitStore.SetBranchAutoMerge(r.Context(), repo, bname, true); err != nil {
		switch {
		case errors.Is(err, db.ErrBranchNotFound):
			writeAPIError(w, ErrCodeBranchNotFound, http.StatusNotFound, "branch not found")
		default:
			slog.Error("internal error", "op", "enable_auto_merge", "repo", repo, "branch", bname, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	identity := IdentityFromContext(r.Context())
	s.emit(r.Context(), evtypes.BranchAutoMergeEnabled{Repo: repo, Branch: bname, EnabledBy: identity})
	slog.Info("auto-merge enabled", "repo", repo, "branch", bname, "by", identity)
	w.WriteHeader(http.StatusNoContent)
}

// handleDisableAutoMerge implements DELETE /repos/:name/-/branch/:bname/auto-merge (writer+)
// Disables auto-merge on the branch.
func (s *server) handleDisableAutoMerge(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	bname := r.PathValue("bname")

	if !s.validateRepo(w, r, repo) {
		return
	}
	if bname == "main" {
		writeError(w, http.StatusBadRequest, "cannot disable auto-merge on branch 'main'")
		return
	}

	if !s.checkBranchIfMatch(w, r, repo, bname) {
		return
	}

	if err := s.commitStore.SetBranchAutoMerge(r.Context(), repo, bname, false); err != nil {
		switch {
		case errors.Is(err, db.ErrBranchNotFound):
			writeAPIError(w, ErrCodeBranchNotFound, http.StatusNotFound, "branch not found")
		default:
			slog.Error("internal error", "op", "disable_auto_merge", "repo", repo, "branch", bname, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	identity := IdentityFromContext(r.Context())
	s.emit(r.Context(), evtypes.BranchAutoMergeDisabled{Repo: repo, Branch: bname, DisabledBy: identity})
	slog.Info("auto-merge disabled", "repo", repo, "branch", bname, "by", identity)
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteBranch implements DELETE /repos/:name/branch/{bname}
func (s *server) handleDeleteBranch(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	bname := r.PathValue("bname")

	if !s.validateRepo(w, r, repo) {
		return
	}
	if bname == "main" {
		writeError(w, http.StatusBadRequest, "cannot delete branch 'main'")
		return
	}

	if !s.checkBranchIfMatch(w, r, repo, bname) {
		return
	}

	err := s.commitStore.DeleteBranch(r.Context(), repo, bname)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrBranchNotFound):
			writeAPIError(w, ErrCodeBranchNotFound, http.StatusNotFound, "branch not found")
		case errors.Is(err, db.ErrBranchNotActive):
			writeAPIError(w, ErrCodeBranchNotActive, http.StatusConflict, "branch is already merged or abandoned")
		default:
			slog.Error("internal error", "op", "delete_branch", "repo", repo, "branch", bname, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	identity := IdentityFromContext(r.Context())
	s.emit(r.Context(), evtypes.BranchAbandoned{Repo: repo, Branch: bname, AbandonedBy: identity})
	slog.Info("branch deleted", "repo", repo, "branch", bname, "by", identity)
	w.WriteHeader(http.StatusNoContent)
}

// handleBranchGet dispatches GET /repos/:name/branch/{branch...} to the
// appropriate sub-resource handler based on the path suffix.
// Branch names may contain slashes (e.g. "feature/x"), so we use a trailing
// wildcard and strip the sub-resource suffix manually — the same technique
// used by handleFile for the "/history" suffix.
func (s *server) handleBranchGet(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	branchPath := r.PathValue("branch")

	switch {
	case strings.HasSuffix(branchPath, "/reviews"):
		branch := strings.TrimSuffix(branchPath, "/reviews")
		s.handleGetReviews(w, r, repo, branch)
	case strings.HasSuffix(branchPath, "/checks"):
		branch := strings.TrimSuffix(branchPath, "/checks")
		s.handleGetChecks(w, r, repo, branch)
	case strings.HasSuffix(branchPath, "/comments"):
		branch := strings.TrimSuffix(branchPath, "/comments")
		s.handleListReviewComments(w, r, repo, branch)
	default:
		s.handleGetBranch(w, r, repo, branchPath)
	}
}

// handleGetBranch implements GET /repos/:name/branch/:branch (bare branch fetch).
// It returns the branch metadata and sets an ETag header derived from the branch's
// head_sequence so clients can use conditional writes (If-Match).
func (s *server) handleGetBranch(w http.ResponseWriter, r *http.Request, repo, branch string) {
	if s.readStore == nil {
		writeAPIError(w, ErrCodeServiceUnavailable, http.StatusServiceUnavailable, "read store not available")
		return
	}
	if !s.validateRepo(w, r, repo) {
		return
	}
	bi, err := s.readStore.GetBranch(r.Context(), repo, branch)
	if err != nil {
		slog.Error("internal error", "op", "get_branch", "repo", repo, "branch", branch, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		return
	}
	if bi == nil {
		writeAPIError(w, ErrCodeBranchNotFound, http.StatusNotFound, "branch not found")
		return
	}
	etag := computeETag(repo, branch, fmt.Sprintf("%d", bi.HeadSequence))
	w.Header().Set("ETag", etag)
	writeJSON(w, http.StatusOK, bi)
}

// checkBranchIfMatch validates the If-Match header for a branch conditional write.
// Returns true if the check passes (header absent or ETag matches); writes the
// appropriate error and returns false otherwise.
func (s *server) checkBranchIfMatch(w http.ResponseWriter, r *http.Request, repo, branch string) bool {
	ifMatch := r.Header.Get("If-Match")
	if ifMatch == "" {
		return true
	}
	if s.readStore == nil {
		writeAPIError(w, ErrCodeServiceUnavailable, http.StatusServiceUnavailable, "read store not available")
		return false
	}
	bi, err := s.readStore.GetBranch(r.Context(), repo, branch)
	if err != nil {
		slog.Error("internal error", "op", "if_match_branch", "repo", repo, "branch", branch, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		return false
	}
	if bi == nil {
		writeAPIError(w, ErrCodeBranchNotFound, http.StatusNotFound, "branch not found")
		return false
	}
	if ifMatch == "*" {
		// RFC 7232 §3.2: "If-Match: *" means proceed if the resource exists.
		return true
	}
	etag := computeETag(repo, branch, fmt.Sprintf("%d", bi.HeadSequence))
	if etag != ifMatch {
		writeAPIError(w, ErrCodePreconditionFailed, http.StatusPreconditionFailed, "ETag mismatch")
		return false
	}
	return true
}

// handleRebase implements POST /repos/:name/rebase
func (s *server) handleRebase(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	var req model.RebaseRequest
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
		writeError(w, http.StatusBadRequest, "cannot rebase main")
		return
	}

	// Author always comes from the authenticated identity; body value is ignored.
	req.Author = IdentityFromContext(r.Context())

	resp, conflicts, err := s.commitStore.Rebase(r.Context(), req)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrRebaseConflict):
			slog.Warn("rebase conflict", "repo", repo, "branch", req.Branch, "conflicts", len(conflicts))
			apiConflicts := make([]model.ConflictEntry, len(conflicts))
			for i, c := range conflicts {
				apiConflicts[i] = model.ConflictEntry{
					Path:            c.Path,
					MainVersionID:   c.MainVersionID,
					BranchVersionID: c.BranchVersionID,
				}
			}
			writeJSON(w, http.StatusConflict, model.RebaseConflictError{Conflicts: apiConflicts})
		case errors.Is(err, db.ErrBranchNotFound):
			writeAPIError(w, ErrCodeBranchNotFound, http.StatusNotFound, "branch not found")
		case errors.Is(err, db.ErrBranchNotActive):
			writeAPIError(w, ErrCodeBranchNotActive, http.StatusConflict, "branch is not active")
		default:
			slog.Error("internal error", "op", "rebase", "repo", repo, "branch", req.Branch, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	bs, bsErr := s.computeBranchStatus(r.Context(), repo, req.Branch, req.Author)
	if bsErr != nil {
		slog.Warn("branch status computation failed", "op", "rebase", "repo", repo, "branch", req.Branch, "error", bsErr)
	}
	s.emit(r.Context(), evtypes.BranchRebased{
		Repo:            repo,
		Branch:          req.Branch,
		NewBaseSequence: resp.NewBaseSequence,
		NewHeadSequence: resp.NewHeadSequence,
		CommitsReplayed: resp.CommitsReplayed,
		RebasedBy:       req.Author,
		BranchStatus:    bs,
	})
	slog.Info("branch rebased", "repo", repo, "branch", req.Branch, "by", req.Author, "commits_replayed", resp.CommitsReplayed)
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// Policy helpers
// ---------------------------------------------------------------------------

// assembleMergeInput gathers all data needed for OPA policy evaluation and
// returns a populated Input. Returns nil, nil if the branch does not exist
// (callers should let the merge handler surface the 404). owners is the raw
// directory→owners map from the policy cache; it is resolved per changed path
// before being placed on the Input.
func (s *server) assembleMergeInput(ctx context.Context, repo, branch, actor string, owners map[string][]string) (*policy.Input, error) {
	return mergeutil.AssembleInput(ctx, s.readStore, s.commitStore, repo, branch, actor, owners)
}

// ---------------------------------------------------------------------------
// Branch status handler
// ---------------------------------------------------------------------------

// computeBranchStatus evaluates merge-eligibility for the given branch and
// returns the result as a BranchStatusResponse. It is best-effort: if the
// read store is unavailable or the branch cannot be found, it returns
// (nil, nil) so callers can omit the field without blocking the primary
// operation. Infrastructure errors are returned for the caller to log.
func (s *server) computeBranchStatus(ctx context.Context, repo, branch, actor string) (*model.BranchStatusResponse, error) {
	if s.readStore == nil {
		return nil, nil
	}

	// Load policies (nil engine → bootstrap mode → mergeable=true).
	var engine *policy.Engine
	var owners map[string][]string
	if s.policyCache != nil {
		var err error
		engine, owners, err = s.policyCache.Load(ctx, repo, s.readStore)
		if err != nil {
			return nil, err
		}
	}

	// Fetch branch info to populate auto_merge in the response.
	branchInfo, err := s.readStore.GetBranch(ctx, repo, branch)
	if err != nil {
		return nil, err
	}
	if branchInfo == nil {
		return nil, nil
	}

	if engine == nil {
		// Bootstrap mode: no policies defined.
		return &model.BranchStatusResponse{
			Mergeable: true,
			Policies:  []model.PolicyResult{},
			AutoMerge: branchInfo.AutoMerge,
		}, nil
	}

	input, err := s.assembleMergeInput(ctx, repo, branch, actor, owners)
	if err != nil {
		return nil, err
	}
	if input == nil {
		return nil, nil
	}

	results, err := engine.Evaluate(ctx, *input)
	if err != nil {
		return nil, err
	}
	if results == nil {
		results = []model.PolicyResult{}
	}

	mergeable := true
	for _, res := range results {
		if !res.Pass {
			mergeable = false
			break
		}
	}

	return &model.BranchStatusResponse{
		Mergeable: mergeable,
		Policies:  results,
		AutoMerge: branchInfo.AutoMerge,
	}, nil
}

// handleBranchStatus implements GET /repos/:name/branch/:branch/status.
// It evaluates merge policies without performing any write operations.
func (s *server) handleBranchStatus(w http.ResponseWriter, r *http.Request, repo, branch string) {
	if s.readStore == nil {
		writeAPIError(w, ErrCodeServiceUnavailable, http.StatusServiceUnavailable, "read store not available")
		return
	}

	actor := IdentityFromContext(r.Context())

	// Determine if the repo even exists.
	if !s.validateRepo(w, r, repo) {
		return
	}

	resp, err := s.computeBranchStatus(r.Context(), repo, branch, actor)
	if err != nil {
		slog.Error("branch status error", "op", "branch_status", "repo", repo, "branch", branch, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "policy evaluation error")
		return
	}
	if resp == nil {
		writeAPIError(w, ErrCodeBranchNotFound, http.StatusNotFound, "branch not found")
		return
	}

	writeJSON(w, http.StatusOK, *resp)
}

// ---------------------------------------------------------------------------
// Agent context handler
// ---------------------------------------------------------------------------

// ErrAgentContextBranchNotFound is returned by assembleAgentContext when the
// named branch does not exist on the repo. Callers translate this to 404.
var ErrAgentContextBranchNotFound = errors.New("branch not found")

// ErrAgentContextReadStoreUnavailable is returned when the server was built
// without a read store. Callers translate this to 503.
var ErrAgentContextReadStoreUnavailable = errors.New("read store not available")

// AssembleAgentContext builds the full branch-context snapshot (diff, reviews,
// checks, proposals, linked issues, policies, recent commits) in one pass.
//
// Exported so in-process consumers (e.g. the UI package) can render the same
// view humans and agents see without going back over HTTP.
func (s *server) AssembleAgentContext(ctx context.Context, repo, branch, actor string) (*model.AgentContextResponse, error) {
	if s.readStore == nil {
		return nil, ErrAgentContextReadStoreUnavailable
	}

	branchInfo, err := s.readStore.GetBranch(ctx, repo, branch)
	if err != nil {
		return nil, fmt.Errorf("get branch: %w", err)
	}
	if branchInfo == nil {
		return nil, ErrAgentContextBranchNotFound
	}

	diff, err := s.readStore.GetDiff(ctx, repo, branch)
	if err != nil {
		return nil, fmt.Errorf("get diff: %w", err)
	}
	if diff == nil {
		diff = &store.DiffResult{}
	}

	reviews, err := s.commitStore.ListReviews(ctx, repo, branch, &branchInfo.HeadSequence)
	if err != nil {
		return nil, fmt.Errorf("list reviews: %w", err)
	}
	if reviews == nil {
		reviews = []model.Review{}
	}

	checkRuns, err := s.commitStore.ListCheckRuns(ctx, repo, branch, &branchInfo.HeadSequence, false)
	if err != nil {
		return nil, fmt.Errorf("list check runs: %w", err)
	}
	if checkRuns == nil {
		checkRuns = []model.CheckRun{}
	}

	openState := model.ProposalOpen
	proposalPtrs, err := s.commitStore.ListProposals(ctx, repo, &openState, &branch)
	if err != nil {
		return nil, fmt.Errorf("list proposals: %w", err)
	}
	proposals := make([]model.Proposal, len(proposalPtrs))
	for i, p := range proposalPtrs {
		proposals[i] = *p
	}

	linkedIssues := []model.Issue{}
	if len(proposalPtrs) > 0 {
		linkedIssues, err = s.commitStore.ListIssuesByRef(ctx, repo, model.IssueRefTypeProposal, proposalPtrs[0].ID)
		if err != nil {
			return nil, fmt.Errorf("list issues by ref: %w", err)
		}
		if linkedIssues == nil {
			linkedIssues = []model.Issue{}
		}
	}

	// Cap chain queries to the last 50 sequences on the branch.
	const maxCommitRange = int64(50)
	recentCommits := []store.ChainEntry{}
	if branchInfo.HeadSequence > branchInfo.BaseSequence {
		from := branchInfo.BaseSequence + 1
		if branchInfo.HeadSequence-from+1 > maxCommitRange {
			from = branchInfo.HeadSequence - maxCommitRange + 1
		}
		chainEntries, err := s.readStore.GetChain(ctx, repo, from, branchInfo.HeadSequence)
		if err != nil {
			return nil, fmt.Errorf("get chain: %w", err)
		}
		for _, e := range chainEntries {
			if e.Branch == branch {
				recentCommits = append(recentCommits, e)
			}
		}
	}

	var changedPaths []string
	for _, e := range diff.BranchChanges {
		changedPaths = append(changedPaths, e.Path)
	}

	fileOwnership := make(map[string][]string)
	policyResults := []model.PolicyResult{}
	mergeable := true

	if s.policyCache != nil {
		engine, owners, err := s.policyCache.Load(ctx, repo, s.readStore)
		if err != nil {
			return nil, fmt.Errorf("policy cache load: %w", err)
		}
		if engine != nil {
			for _, p := range changedPaths {
				fileOwnership[p] = policy.ResolveOwners(owners, p)
			}

			actorRoles := []string{}
			if role, err := s.commitStore.GetRole(ctx, repo, actor); err == nil && role != nil {
				actorRoles = []string{string(role.Role)}
			}

			var proposalInput *policy.ProposalInput
			if len(proposalPtrs) > 0 {
				p := proposalPtrs[0]
				proposalInput = &policy.ProposalInput{
					ID:         p.ID,
					BaseBranch: p.BaseBranch,
					Title:      p.Title,
					State:      string(p.State),
				}
			}

			reviewInputs := make([]policy.ReviewInput, len(reviews))
			for i, rev := range reviews {
				reviewInputs[i] = policy.ReviewInput{
					Reviewer: rev.Reviewer,
					Status:   string(rev.Status),
					Sequence: rev.Sequence,
				}
			}
			checkInputs := make([]policy.CheckRunInput, len(checkRuns))
			for i, cr := range checkRuns {
				checkInputs[i] = policy.CheckRunInput{
					CheckName: cr.CheckName,
					Status:    string(cr.Status),
					Sequence:  cr.Sequence,
				}
			}
			resolvedOwners := make(map[string][]string)
			for _, p := range changedPaths {
				resolvedOwners[p] = policy.ResolveOwners(owners, p)
			}

			input := policy.Input{
				Actor:        actor,
				ActorRoles:   actorRoles,
				Action:       "merge",
				Repo:         repo,
				Branch:       branch,
				Draft:        branchInfo.Draft,
				ChangedPaths: changedPaths,
				Reviews:      reviewInputs,
				CheckRuns:    checkInputs,
				Owners:       resolvedOwners,
				HeadSeq:      branchInfo.HeadSequence,
				BaseSeq:      branchInfo.BaseSequence,
				Proposal:     proposalInput,
			}

			results, err := engine.Evaluate(ctx, input)
			if err != nil {
				return nil, fmt.Errorf("policy evaluate: %w", err)
			}
			if results != nil {
				policyResults = results
			}
			for _, res := range policyResults {
				if !res.Pass {
					mergeable = false
					break
				}
			}
		}
	}

	diffResp := model.DiffResponse{
		BranchChanges: make([]model.DiffEntry, len(diff.BranchChanges)),
		MainChanges:   make([]model.DiffEntry, len(diff.MainChanges)),
	}
	for i, e := range diff.BranchChanges {
		diffResp.BranchChanges[i] = model.DiffEntry{Path: e.Path, VersionID: e.VersionID, Binary: e.Binary}
	}
	for i, e := range diff.MainChanges {
		diffResp.MainChanges[i] = model.DiffEntry{Path: e.Path, VersionID: e.VersionID, Binary: e.Binary}
	}
	for _, c := range diff.Conflicts {
		diffResp.Conflicts = append(diffResp.Conflicts, model.ConflictEntry{
			Path:            c.Path,
			MainVersionID:   c.MainVersionID,
			BranchVersionID: c.BranchVersionID,
		})
	}

	return &model.AgentContextResponse{
		Branch: model.Branch{
			Repo:         repo,
			Name:         branchInfo.Name,
			HeadSequence: branchInfo.HeadSequence,
			BaseSequence: branchInfo.BaseSequence,
			Status:       model.BranchStatus(branchInfo.Status),
			Draft:        branchInfo.Draft,
			AutoMerge:    branchInfo.AutoMerge,
		},
		Diff:          diffResp,
		Reviews:       reviews,
		CheckRuns:     checkRuns,
		Proposals:     proposals,
		LinkedIssues:  linkedIssues,
		FileOwnership: fileOwnership,
		RecentCommits: recentCommits,
		Policies:      policyResults,
		Mergeable:     mergeable,
	}, nil
}

// handleAgentContext implements GET /repos/:name/-/branch/:branch/agent-context.
// It is a thin HTTP wrapper over AssembleAgentContext.
func (s *server) handleAgentContext(w http.ResponseWriter, r *http.Request, repo, branch string) {
	if s.readStore == nil {
		writeAPIError(w, ErrCodeServiceUnavailable, http.StatusServiceUnavailable, "read store not available")
		return
	}
	if !s.validateRepo(w, r, repo) {
		return
	}

	resp, err := s.AssembleAgentContext(r.Context(), repo, branch, IdentityFromContext(r.Context()))
	if err != nil {
		switch {
		case errors.Is(err, ErrAgentContextBranchNotFound):
			writeAPIError(w, ErrCodeBranchNotFound, http.StatusNotFound, "branch not found")
		case errors.Is(err, ErrAgentContextReadStoreUnavailable):
			writeAPIError(w, ErrCodeServiceUnavailable, http.StatusServiceUnavailable, "read store not available")
		default:
			slog.Error("internal error", "op", "agent_context", "repo", repo, "branch", branch, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

