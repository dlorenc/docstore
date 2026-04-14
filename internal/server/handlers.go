package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/store"
)

// parseRepoPath parses a /repos/... URL path into the full repo name and the
// endpoint string that follows the "/-/" separator.
//
//	/repos/acme/myrepo/-/tree        → ("acme/myrepo", "tree", true)
//	/repos/acme/team/sub/-/commit    → ("acme/team/sub", "commit", true)
//	/repos/acme/myrepo               → ("acme/myrepo", "", true)  // bare repo
//	/something/else                  → ("", "", false)
//
// NOTE: repoAndSubPath in middleware.go parses the same "/-/" URL format for
// RBAC purposes. Both functions must be kept in sync if the URL structure changes.
func parseRepoPath(path string) (repoName, endpoint string, ok bool) {
	const prefix = "/repos/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	rest := path[len(prefix):]
	if rest == "" {
		return "", "", false
	}
	idx := strings.Index(rest, "/-/")
	if idx == -1 {
		// Bare /repos/:repopath — no endpoint
		return rest, "", true
	}
	return rest[:idx], rest[idx+3:], true
}

// handleReposPrefix is the catch-all handler for all /repos/... paths (except
// the bare GET /repos and POST /repos which are registered as exact matches).
// It parses the repo name and endpoint from the URL using the "/-/" separator,
// sets the "name" path value so existing sub-handlers work unchanged, and
// dispatches to the appropriate handler.
func (s *server) handleReposPrefix(w http.ResponseWriter, r *http.Request) {
	repoName, endpoint, ok := parseRepoPath(r.URL.Path)
	if !ok || repoName == "" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	r.SetPathValue("name", repoName)

	if endpoint == "" {
		// Bare /repos/:reponame
		switch r.Method {
		case http.MethodGet:
			s.handleGetRepo(w, r)
		case http.MethodDelete:
			s.handleDeleteRepo(w, r)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}

	switch {
	case endpoint == "tree":
		if r.Method == http.MethodGet {
			s.handleTree(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "branches":
		if r.Method == http.MethodGet {
			s.handleBranches(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "diff":
		if r.Method == http.MethodGet {
			s.handleDiff(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "commit":
		if r.Method == http.MethodPost {
			s.handleCommit(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "branch":
		if r.Method == http.MethodPost {
			s.handleCreateBranch(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "merge":
		if r.Method == http.MethodPost {
			s.handleMerge(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "rebase":
		if r.Method == http.MethodPost {
			s.handleRebase(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "review":
		if r.Method == http.MethodPost {
			s.handleReview(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "check":
		if r.Method == http.MethodPost {
			s.handleCheck(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "purge":
		if r.Method == http.MethodPost {
			s.handlePurge(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "roles":
		if r.Method == http.MethodGet {
			s.handleListRoles(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case strings.HasPrefix(endpoint, "commit/"):
		if r.Method == http.MethodGet {
			r.SetPathValue("sequence", strings.TrimPrefix(endpoint, "commit/"))
			s.handleGetCommit(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case strings.HasPrefix(endpoint, "file/"):
		if r.Method == http.MethodGet {
			r.SetPathValue("path", strings.TrimPrefix(endpoint, "file/"))
			s.handleFile(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case strings.HasPrefix(endpoint, "roles/"):
		identity := strings.TrimPrefix(endpoint, "roles/")
		r.SetPathValue("identity", identity)
		switch r.Method {
		case http.MethodPut:
			s.handleSetRole(w, r)
		case http.MethodDelete:
			s.handleDeleteRole(w, r)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case strings.HasPrefix(endpoint, "branch/"):
		bpath := strings.TrimPrefix(endpoint, "branch/")
		switch r.Method {
		case http.MethodDelete:
			r.SetPathValue("bname", bpath)
			s.handleDeleteBranch(w, r)
		case http.MethodGet:
			if strings.HasSuffix(bpath, "/status") {
				notImplemented(w, r)
			} else {
				r.SetPathValue("branch", bpath)
				s.handleBranchGet(w, r)
			}
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

// ---------------------------------------------------------------------------
// Org handlers
// ---------------------------------------------------------------------------

// handleCreateOrg implements POST /orgs
func (s *server) handleCreateOrg(w http.ResponseWriter, r *http.Request) {
	var req model.CreateOrgRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if strings.ContainsRune(req.Name, '/') {
		writeError(w, http.StatusBadRequest, "org name may not contain '/'")
		return
	}

	identity := IdentityFromContext(r.Context())
	org, err := s.commitStore.CreateOrg(r.Context(), req.Name, identity)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrOrgExists):
			writeError(w, http.StatusConflict, "org already exists")
		default:
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}
	writeJSON(w, http.StatusCreated, org)
}

// handleListOrgs implements GET /orgs
func (s *server) handleListOrgs(w http.ResponseWriter, r *http.Request) {
	orgs, err := s.commitStore.ListOrgs(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if orgs == nil {
		orgs = []model.Org{}
	}
	writeJSON(w, http.StatusOK, model.ListOrgsResponse{Orgs: orgs})
}

// handleGetOrg implements GET /orgs/{org}
func (s *server) handleGetOrg(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("org")
	org, err := s.commitStore.GetOrg(r.Context(), name)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrOrgNotFound):
			writeError(w, http.StatusNotFound, "org not found")
		default:
			writeError(w, http.StatusInternalServerError, "query failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, org)
}

// handleDeleteOrg implements DELETE /orgs/{org}
func (s *server) handleDeleteOrg(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("org")
	err := s.commitStore.DeleteOrg(r.Context(), name)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrOrgNotFound):
			writeError(w, http.StatusNotFound, "org not found")
		case errors.Is(err, db.ErrOrgHasRepos):
			writeError(w, http.StatusConflict, "org has repos; delete them first")
		default:
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListOrgRepos implements GET /orgs/{org}/repos
func (s *server) handleListOrgRepos(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("org")
	if _, err := s.commitStore.GetOrg(r.Context(), owner); err != nil {
		if errors.Is(err, db.ErrOrgNotFound) {
			writeError(w, http.StatusNotFound, "org not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	repos, err := s.commitStore.ListOrgRepos(r.Context(), owner)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if repos == nil {
		repos = []model.Repo{}
	}
	writeJSON(w, http.StatusOK, model.ReposResponse{Repos: repos})
}

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
			writeError(w, http.StatusNotFound, "branch not found")
		case errors.Is(err, db.ErrSelfApproval):
			writeError(w, http.StatusForbidden, "reviewer cannot approve their own commits")
		default:
			slog.Error("internal error", "op", "review", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

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

	cr, err := s.commitStore.CreateCheckRun(r.Context(), repo, req.Branch, req.CheckName, req.Status, reporter)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrBranchNotFound):
			writeError(w, http.StatusNotFound, "branch not found")
		default:
			slog.Error("internal error", "op", "check", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	writeJSON(w, http.StatusCreated, model.CreateCheckRunResponse{
		ID:       cr.ID,
		Sequence: cr.Sequence,
	})
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
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
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
		writeError(w, http.StatusInternalServerError, "query failed")
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

	checkRuns, err := s.commitStore.ListCheckRuns(r.Context(), repo, branch, atSeq)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if checkRuns == nil {
		checkRuns = []model.CheckRun{}
	}
	writeJSON(w, http.StatusOK, checkRuns)
}

// parseDayDuration parses a duration string of the form "Nd" (e.g. "7d", "30d").
// N must be a positive integer.
func parseDayDuration(s string) (time.Duration, error) {
	if len(s) < 2 || s[len(s)-1] != 'd' {
		return 0, fmt.Errorf("invalid duration %q: must be of the form \"Nd\" (e.g. \"7d\")", s)
	}
	n, err := strconv.ParseInt(s[:len(s)-1], 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid duration %q: must be a positive integer followed by 'd'", s)
	}
	return time.Duration(n) * 24 * time.Hour, nil
}

// handlePurge implements POST /repos/:name/purge
func (s *server) handlePurge(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	identity := IdentityFromContext(r.Context())

	var req model.PurgeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.OlderThan == "" {
		writeError(w, http.StatusBadRequest, "older_than is required")
		return
	}

	dur, err := parseDayDuration(req.OlderThan)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	result, err := s.commitStore.Purge(r.Context(), db.PurgeRequest{
		Repo:      repo,
		OlderThan: dur,
		DryRun:    req.DryRun,
	})
	if err != nil {
		switch {
		case errors.Is(err, db.ErrRepoNotFound):
			writeError(w, http.StatusNotFound, "repo not found")
		default:
			slog.Error("internal error", "op", "purge", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	slog.Info("purge complete", "repo", repo, "dry_run", req.DryRun, "branches_purged", result.BranchesPurged, "docs_deleted", result.DocumentsDeleted, "by", identity)
	writeJSON(w, http.StatusOK, model.PurgeResponse{
		BranchesPurged:     result.BranchesPurged,
		FileCommitsDeleted: result.FileCommitsDeleted,
		CommitsDeleted:     result.CommitsDeleted,
		DocumentsDeleted:   result.DocumentsDeleted,
		ReviewsDeleted:     result.ReviewsDeleted,
		CheckRunsDeleted:   result.CheckRunsDeleted,
	})
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// validateRepo checks that the named repo exists. It writes a 404 and returns
// false when the repo is not found, so callers can do:
//
//	if !s.validateRepo(w, r, repo) { return }
func (s *server) validateRepo(w http.ResponseWriter, r *http.Request, repo string) bool {
	_, err := s.commitStore.GetRepo(r.Context(), repo)
	if err != nil {
		if errors.Is(err, db.ErrRepoNotFound) {
			writeError(w, http.StatusNotFound, "repo not found")
		} else {
			writeError(w, http.StatusInternalServerError, "query failed")
		}
		return false
	}
	return true
}

// handleCreateRepo implements POST /repos
func (s *server) handleCreateRepo(w http.ResponseWriter, r *http.Request) {
	var req model.CreateRepoRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Owner == "" {
		writeError(w, http.StatusBadRequest, "owner is required")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	fullName := req.FullName()
	if strings.SplitN(fullName, "/", 2)[0] != req.Owner {
		writeError(w, http.StatusBadRequest, "owner must match first segment of repo name")
		return
	}
	if req.CreatedBy == "" {
		req.CreatedBy = IdentityFromContext(r.Context())
	}

	repo, err := s.commitStore.CreateRepo(r.Context(), req)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrRepoExists):
			writeError(w, http.StatusConflict, "repo already exists")
		case errors.Is(err, db.ErrOrgNotFound):
			writeError(w, http.StatusNotFound, "org not found")
		default:
			slog.Error("internal error", "op", "create_repo", "repo", req.Name, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	slog.Info("repo created", "repo", repo.Name, "by", IdentityFromContext(r.Context()))
	writeJSON(w, http.StatusCreated, repo)
}

// handleListRepos implements GET /repos
func (s *server) handleListRepos(w http.ResponseWriter, r *http.Request) {
	repos, err := s.commitStore.ListRepos(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if repos == nil {
		repos = []model.Repo{}
	}
	writeJSON(w, http.StatusOK, model.ReposResponse{Repos: repos})
}

// handleGetRepo implements GET /repos/:name
func (s *server) handleGetRepo(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	repo, err := s.commitStore.GetRepo(r.Context(), name)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrRepoNotFound):
			writeError(w, http.StatusNotFound, "repo not found")
		default:
			writeError(w, http.StatusInternalServerError, "query failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, repo)
}

// handleDeleteRepo implements DELETE /repos/:name (hard delete)
func (s *server) handleDeleteRepo(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	err := s.commitStore.DeleteRepo(r.Context(), name)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrRepoNotFound):
			writeError(w, http.StatusNotFound, "repo not found")
		default:
			slog.Error("internal error", "op", "delete_repo", "repo", name, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	slog.Info("repo deleted", "repo", name, "by", IdentityFromContext(r.Context()))
	w.WriteHeader(http.StatusNoContent)
}

// handleTree implements GET /repos/:name/tree?branch=main&at=N&limit=N&after=cursor
func (s *server) handleTree(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}
	branch := r.URL.Query().Get("branch")
	if branch == "" {
		branch = "main"
	}

	var atSequence *int64
	if v := r.URL.Query().Get("at"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid 'at' parameter")
			return
		}
		atSequence = &n
	}

	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "invalid 'limit' parameter")
			return
		}
		limit = n
	}

	afterPath := r.URL.Query().Get("after")

	entries, err := s.readStore.MaterializeTree(r.Context(), repo, branch, atSequence, limit, afterPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if entries == nil {
		entries = []store.TreeEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}

// handleFile implements:
//   - GET /repos/:name/file/{path...}          → file content
//   - GET /repos/:name/file/{path...}/history  → file change history
//
// Query params: branch (default "main"), at (sequence), limit, after (cursor).
func (s *server) handleFile(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}
	fullPath := r.PathValue("path")

	// Check for /history suffix.
	if strings.HasSuffix(fullPath, "/history") {
		filePath := strings.TrimSuffix(fullPath, "/history")
		s.handleFileHistory(w, r, repo, filePath)
		return
	}

	s.handleFileContent(w, r, repo, fullPath)
}

func (s *server) handleFileContent(w http.ResponseWriter, r *http.Request, repo, path string) {
	branch := r.URL.Query().Get("branch")
	if branch == "" {
		branch = "main"
	}

	var atSequence *int64
	if v := r.URL.Query().Get("at"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid 'at' parameter")
			return
		}
		atSequence = &n
	}

	fc, err := s.readStore.GetFile(r.Context(), repo, branch, path, atSequence)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if fc == nil {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}

	writeJSON(w, http.StatusOK, fc)
}

func (s *server) handleFileHistory(w http.ResponseWriter, r *http.Request, repo, path string) {
	branch := r.URL.Query().Get("branch")
	if branch == "" {
		branch = "main"
	}

	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "invalid 'limit' parameter")
			return
		}
		limit = n
	}

	var afterSeq *int64
	if v := r.URL.Query().Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid 'after' parameter")
			return
		}
		afterSeq = &n
	}

	entries, err := s.readStore.GetFileHistory(r.Context(), repo, branch, path, limit, afterSeq)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if entries == nil {
		entries = []store.FileHistoryEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}

// handleGetCommit implements GET /repos/:name/commit/{sequence}
func (s *server) handleGetCommit(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	seqStr := r.PathValue("sequence")
	seq, err := strconv.ParseInt(seqStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid sequence number")
		return
	}

	detail, err := s.readStore.GetCommit(r.Context(), repo, seq)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if detail == nil {
		writeError(w, http.StatusNotFound, "commit not found")
		return
	}

	writeJSON(w, http.StatusOK, detail)
}

// handleBranches implements GET /repos/:name/branches?status=active
func (s *server) handleBranches(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}
	statusFilter := r.URL.Query().Get("status")

	branches, err := s.readStore.ListBranches(r.Context(), repo, statusFilter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if branches == nil {
		branches = []store.BranchInfo{}
	}
	writeJSON(w, http.StatusOK, branches)
}

// handleDiff implements GET /repos/:name/diff?branch=X
func (s *server) handleDiff(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}
	branch := r.URL.Query().Get("branch")
	if branch == "" {
		writeError(w, http.StatusBadRequest, "branch parameter is required")
		return
	}

	result, err := s.readStore.GetDiff(r.Context(), repo, branch)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if result == nil {
		writeError(w, http.StatusNotFound, "branch not found")
		return
	}

	// Convert to API response type.
	resp := model.DiffResponse{
		BranchChanges: make([]model.DiffEntry, len(result.BranchChanges)),
		MainChanges:   make([]model.DiffEntry, len(result.MainChanges)),
	}
	for i, e := range result.BranchChanges {
		resp.BranchChanges[i] = model.DiffEntry{Path: e.Path, VersionID: e.VersionID}
	}
	for i, e := range result.MainChanges {
		resp.MainChanges[i] = model.DiffEntry{Path: e.Path, VersionID: e.VersionID}
	}
	for _, c := range result.Conflicts {
		resp.Conflicts = append(resp.Conflicts, model.ConflictEntry{
			Path:            c.Path,
			MainVersionID:   c.MainVersionID,
			BranchVersionID: c.BranchVersionID,
		})
	}

	writeJSON(w, http.StatusOK, resp)
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
			writeError(w, http.StatusConflict, "branch already exists")
		case errors.Is(err, db.ErrRepoNotFound):
			writeError(w, http.StatusNotFound, "repo not found")
		default:
			slog.Error("internal error", "op", "create_branch", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	slog.Info("branch created", "repo", repo, "branch", req.Name, "by", IdentityFromContext(r.Context()))
	writeJSON(w, http.StatusCreated, resp)
}

// handleDeleteBranch implements DELETE /repos/:name/branch/{bname}
func (s *server) handleDeleteBranch(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	bname := r.PathValue("bname")
	if bname == "main" {
		writeError(w, http.StatusBadRequest, "cannot delete branch 'main'")
		return
	}

	err := s.commitStore.DeleteBranch(r.Context(), repo, bname)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrBranchNotFound):
			writeError(w, http.StatusNotFound, "branch not found")
		case errors.Is(err, db.ErrBranchNotActive):
			writeError(w, http.StatusConflict, "branch is already merged or abandoned")
		default:
			slog.Error("internal error", "op", "delete_branch", "repo", repo, "branch", bname, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	slog.Info("branch deleted", "repo", repo, "branch", bname, "by", IdentityFromContext(r.Context()))
	w.WriteHeader(http.StatusNoContent)
}

// handleListRoles implements GET /repos/:name/roles (admin only — enforced by RBAC middleware)
func (s *server) handleListRoles(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	roles, err := s.commitStore.ListRoles(r.Context(), repo)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if roles == nil {
		roles = []model.Role{}
	}
	writeJSON(w, http.StatusOK, model.RolesResponse{Roles: roles})
}

// handleSetRole implements PUT /repos/:name/roles/:identity (admin only — enforced by RBAC middleware)
func (s *server) handleSetRole(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	identity := r.PathValue("identity")

	if identity == "" {
		writeError(w, http.StatusBadRequest, "identity is required")
		return
	}

	var req model.SetRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	switch req.Role {
	case model.RoleReader, model.RoleWriter, model.RoleMaintainer, model.RoleAdmin:
		// valid
	default:
		writeError(w, http.StatusBadRequest, "invalid role; must be reader, writer, maintainer, or admin")
		return
	}

	if err := s.commitStore.SetRole(r.Context(), repo, identity, req.Role); err != nil {
		slog.Error("internal error", "op", "set_role", "repo", repo, "identity", identity, "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	slog.Info("role assigned", "repo", repo, "identity", identity, "role", req.Role, "by", IdentityFromContext(r.Context()))
	writeJSON(w, http.StatusOK, model.Role{Identity: identity, Role: req.Role})
}

// handleDeleteRole implements DELETE /repos/:name/roles/:identity (admin only — enforced by RBAC middleware)
func (s *server) handleDeleteRole(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	identity := r.PathValue("identity")

	if identity == "" {
		writeError(w, http.StatusBadRequest, "identity is required")
		return
	}

	if err := s.commitStore.DeleteRole(r.Context(), repo, identity); err != nil {
		switch {
		case errors.Is(err, db.ErrRoleNotFound):
			writeError(w, http.StatusNotFound, "role not found")
		default:
			slog.Error("internal error", "op", "delete_role", "repo", repo, "identity", identity, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	slog.Info("role removed", "repo", repo, "identity", identity, "by", IdentityFromContext(r.Context()))
	w.WriteHeader(http.StatusNoContent)
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
			writeError(w, http.StatusNotFound, "branch not found")
		case errors.Is(err, db.ErrBranchNotActive):
			writeError(w, http.StatusBadRequest, "branch is not active")
		default:
			slog.Error("internal error", "op", "rebase", "repo", repo, "branch", req.Branch, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	slog.Info("branch rebased", "repo", repo, "branch", req.Branch, "by", req.Author, "commits_replayed", resp.CommitsReplayed)
	writeJSON(w, http.StatusOK, resp)
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
			writeError(w, http.StatusNotFound, "branch not found")
		case errors.Is(err, db.ErrBranchNotActive):
			writeError(w, http.StatusConflict, "branch is not active")
		default:
			slog.Error("internal error", "op", "merge", "repo", repo, "branch", req.Branch, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	slog.Info("branch merged", "repo", repo, "branch", req.Branch, "by", req.Author, "sequence", resp.Sequence)
	writeJSON(w, http.StatusOK, resp)
}
