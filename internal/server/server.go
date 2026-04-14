package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/store"
)

// WriteStore abstracts the database write and repo management operations.
type WriteStore interface {
	// Repo management
	CreateRepo(ctx context.Context, req model.CreateRepoRequest) (*model.Repo, error)
	DeleteRepo(ctx context.Context, name string) error
	ListRepos(ctx context.Context) ([]model.Repo, error)
	GetRepo(ctx context.Context, name string) (*model.Repo, error)

	// Branch and commit operations (all repo-scoped via req.Repo / explicit repo param)
	Commit(ctx context.Context, req model.CommitRequest) (*model.CommitResponse, error)
	CreateBranch(ctx context.Context, req model.CreateBranchRequest) (*model.CreateBranchResponse, error)
	Merge(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error)
	DeleteBranch(ctx context.Context, repo, name string) error
	Rebase(ctx context.Context, req model.RebaseRequest) (*model.RebaseResponse, []db.MergeConflict, error)

	// Review and check-run operations
	CreateReview(ctx context.Context, repo, branch, reviewer string, status model.ReviewStatus, body string) (*model.Review, error)
	ListReviews(ctx context.Context, repo, branch string, atSeq *int64) ([]model.Review, error)
	CreateCheckRun(ctx context.Context, repo, branch, checkName string, status model.CheckRunStatus, reporter string) (*model.CheckRun, error)
	ListCheckRuns(ctx context.Context, repo, branch string, atSeq *int64) ([]model.CheckRun, error)
}

// CommitStore is an alias for backward compatibility with tests.
type CommitStore = WriteStore

// New returns an http.Handler with all routes registered.
// devIdentity, if non-empty, bypasses IAP JWT validation (for local dev/testing).
// writeStore provides write operations; pass nil if only read/health endpoints are needed.
// database provides read operations; pass nil if only write/health endpoints are needed.
func New(writeStore WriteStore, database *sql.DB, devIdentity string) http.Handler {
	s := &server{commitStore: writeStore}
	if database != nil {
		s.readStore = store.New(database)
	}

	// Health check is exempt from auth — load balancers and probes call it.
	outer := http.NewServeMux()
	outer.HandleFunc("GET /healthz", handleHealth)

	// All other routes require IAP authentication.
	inner := http.NewServeMux()

	// Repo management endpoints
	inner.HandleFunc("POST /repos", s.handleCreateRepo)
	inner.HandleFunc("GET /repos", s.handleListRepos)
	inner.HandleFunc("GET /repos/{name}", s.handleGetRepo)
	inner.HandleFunc("DELETE /repos/{name}", s.handleDeleteRepo)

	// Repo-scoped read endpoints
	inner.HandleFunc("GET /repos/{name}/tree", s.handleTree)
	inner.HandleFunc("GET /repos/{name}/file/{path...}", s.handleFile)
	inner.HandleFunc("GET /repos/{name}/commit/{sequence}", s.handleGetCommit)
	inner.HandleFunc("GET /repos/{name}/diff", s.handleDiff)
	inner.HandleFunc("GET /repos/{name}/branches", s.handleBranches)
	inner.HandleFunc("GET /repos/{name}/branch/{bname}/status", notImplemented)
	// Branch sub-resource reads; {branch...} handles names that contain slashes.
	// More-specific {bname}/status route above wins for /status paths.
	inner.HandleFunc("GET /repos/{name}/branch/{branch...}", s.handleBranchGet)

	// Repo-scoped write endpoints
	inner.HandleFunc("POST /repos/{name}/commit", s.handleCommit)
	inner.HandleFunc("POST /repos/{name}/branch", s.handleCreateBranch)
	inner.HandleFunc("POST /repos/{name}/merge", s.handleMerge)
	inner.HandleFunc("POST /repos/{name}/rebase", s.handleRebase)
	inner.HandleFunc("POST /repos/{name}/review", s.handleReview)
	inner.HandleFunc("POST /repos/{name}/check", s.handleCheck)
	inner.HandleFunc("DELETE /repos/{name}/branch/{bname...}", s.handleDeleteBranch)

	outer.Handle("/", IAPMiddleware(devIdentity)(inner))
	return outer
}

type server struct {
	commitStore CommitStore
	readStore   *store.Store
}

func (s *server) handleCommit(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	var req model.CommitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	req.Repo = repo

	if req.Branch == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "branch is required"})
		return
	}
	if len(req.Files) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "at least one file is required"})
		return
	}
	if req.Message == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message is required"})
		return
	}
	for _, f := range req.Files {
		if f.Path == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file path is required"})
			return
		}
	}

	// Author always comes from the authenticated identity; clients cannot override it.
	req.Author = IdentityFromContext(r.Context())

	resp, err := s.commitStore.Commit(r.Context(), req)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrBranchNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "branch not found"})
		case errors.Is(err, db.ErrBranchNotActive):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "branch is not active"})
		default:
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal server error"})
		}
		return
	}

	writeJSON(w, http.StatusCreated, resp)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func notImplemented(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "not implemented"})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
