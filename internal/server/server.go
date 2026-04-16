package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/dlorenc/docstore/internal/blob"
	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/policy"
	"github.com/dlorenc/docstore/internal/store"
)

// readStore is the interface for all read operations used by handlers.
// *store.Store satisfies this interface.
type readStore interface {
	MaterializeTree(ctx context.Context, repo, branch string, atSeq *int64, limit int, afterPath string) ([]store.TreeEntry, error)
	GetFile(ctx context.Context, repo, branch, path string, atSeq *int64) (*store.FileContent, error)
	GetFileHistory(ctx context.Context, repo, branch, path string, limit int, afterSeq *int64) ([]store.FileHistoryEntry, error)
	GetBranch(ctx context.Context, repo, branch string) (*store.BranchInfo, error)
	ListBranches(ctx context.Context, repo, statusFilter string, includeDraft, onlyDraft bool) ([]store.BranchInfo, error)
	GetDiff(ctx context.Context, repo, branch string) (*store.DiffResult, error)
	GetCommit(ctx context.Context, repo string, seq int64) (*store.CommitDetail, error)
	GetChain(ctx context.Context, repo string, from, to int64) ([]store.ChainEntry, error)
}

// policyCache is the interface for loading and invalidating OPA policy engines.
// *policy.Cache satisfies this interface.
type policyCache interface {
	Load(ctx context.Context, repo string, st policy.ReadStore) (*policy.Engine, map[string][]string, error)
	Invalidate(repo string)
}

// WriteStore abstracts the database write, repo management, and role operations.
type WriteStore interface {
	// Org management
	CreateOrg(ctx context.Context, name, createdBy string) (*model.Org, error)
	GetOrg(ctx context.Context, name string) (*model.Org, error)
	ListOrgs(ctx context.Context) ([]model.Org, error)
	DeleteOrg(ctx context.Context, name string) error
	ListOrgRepos(ctx context.Context, owner string) ([]model.Repo, error)

	// Org membership
	GetOrgMember(ctx context.Context, org, identity string) (*model.OrgMember, error)
	AddOrgMember(ctx context.Context, org, identity string, role model.OrgRole, invitedBy string) error
	RemoveOrgMember(ctx context.Context, org, identity string) error
	ListOrgMembers(ctx context.Context, org string) ([]model.OrgMember, error)

	// Org invitations
	CreateInvite(ctx context.Context, org, email string, role model.OrgRole, invitedBy, token string, expiresAt time.Time) (*model.OrgInvite, error)
	ListInvites(ctx context.Context, org string) ([]model.OrgInvite, error)
	AcceptInvite(ctx context.Context, org, token, identity string) error
	RevokeInvite(ctx context.Context, org, inviteID string) error

	// Repo management
	CreateRepo(ctx context.Context, req model.CreateRepoRequest) (*model.Repo, error)
	DeleteRepo(ctx context.Context, name string) error
	ListRepos(ctx context.Context) ([]model.Repo, error)
	GetRepo(ctx context.Context, name string) (*model.Repo, error)

	// Branch and commit operations (all repo-scoped via req.Repo / explicit repo param)
	Commit(ctx context.Context, req model.CommitRequest) (*model.CommitResponse, error)
	CreateBranch(ctx context.Context, req model.CreateBranchRequest) (*model.CreateBranchResponse, error)
	UpdateBranchDraft(ctx context.Context, repo, name string, draft bool) error
	Merge(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error)
	DeleteBranch(ctx context.Context, repo, name string) error
	Rebase(ctx context.Context, req model.RebaseRequest) (*model.RebaseResponse, []db.MergeConflict, error)

	// Review and check-run operations
	CreateReview(ctx context.Context, repo, branch, reviewer string, status model.ReviewStatus, body string) (*model.Review, error)
	ListReviews(ctx context.Context, repo, branch string, atSeq *int64) ([]model.Review, error)
	CreateCheckRun(ctx context.Context, repo, branch, checkName string, status model.CheckRunStatus, reporter string, logURL *string) (*model.CheckRun, error)
	ListCheckRuns(ctx context.Context, repo, branch string, atSeq *int64) ([]model.CheckRun, error)

	// Role management
	GetRole(ctx context.Context, repo, identity string) (*model.Role, error)
	SetRole(ctx context.Context, repo, identity string, role model.RoleType) error
	DeleteRole(ctx context.Context, repo, identity string) error
	ListRoles(ctx context.Context, repo string) ([]model.Role, error)
	HasAdmin(ctx context.Context, repo string) (bool, error)

	// Purge
	Purge(ctx context.Context, req db.PurgeRequest) (*db.PurgeResult, error)

	// Release management
	CreateRelease(ctx context.Context, repo, name string, sequence int64, body, createdBy string) (*model.Release, error)
	GetRelease(ctx context.Context, repo, name string) (*model.Release, error)
	ListReleases(ctx context.Context, repo string, limit int, afterID string) ([]model.Release, error)
	DeleteRelease(ctx context.Context, repo, name string) error

	// CommitSequenceExists reports whether the given sequence number exists in
	// the commits table for repo. Used to validate release sequence references.
	CommitSequenceExists(ctx context.Context, repo string, sequence int64) (bool, error)
}

// CommitStore is an alias for backward compatibility with tests.
type CommitStore = WriteStore

// New returns an http.Handler with all routes registered.
// devIdentity, if non-empty, bypasses IAP JWT validation (for local dev/testing).
// bootstrapAdmin, if non-empty, has admin access to any repo with no existing admin.
// writeStore provides write operations; pass nil if only read/health endpoints are needed.
// database provides read operations; pass nil if only write/health endpoints are needed.
func New(writeStore WriteStore, database *sql.DB, devIdentity, bootstrapAdmin string) http.Handler {
	return newServer(writeStore, database, nil, devIdentity, bootstrapAdmin)
}

// NewWithBlobStore is like New but also wires a BlobStore into the read store
// so that files stored externally can be fetched by the file endpoint.
func NewWithBlobStore(writeStore WriteStore, database *sql.DB, bs blob.BlobStore, devIdentity, bootstrapAdmin string) http.Handler {
	return newServer(writeStore, database, bs, devIdentity, bootstrapAdmin)
}

func newServer(writeStore WriteStore, database *sql.DB, bs blob.BlobStore, devIdentity, bootstrapAdmin string) http.Handler {
	s := &server{commitStore: writeStore, policyCache: policy.NewCache()}
	if database != nil {
		rs := store.New(database)
		if bs != nil {
			rs.SetBlobStore(bs)
		}
		s.readStore = rs
	}
	return s.buildHandler(devIdentity, bootstrapAdmin, writeStore)
}

// buildHandler wires up all routes and middleware for the given server.
// Extracted so tests can construct a server with injected dependencies and still
// get the full middleware stack.
func (s *server) buildHandler(devIdentity, bootstrapAdmin string, writeStore WriteStore) http.Handler {
	// Health check is exempt from auth — load balancers and probes call it.
	outer := http.NewServeMux()
	outer.HandleFunc("GET /healthz", handleHealth)

	// All other routes require IAP authentication.
	inner := http.NewServeMux()

	// Org management endpoints
	inner.HandleFunc("POST /orgs", s.handleCreateOrg)
	inner.HandleFunc("GET /orgs", s.handleListOrgs)
	inner.HandleFunc("GET /orgs/{org}", s.handleGetOrg)
	inner.HandleFunc("DELETE /orgs/{org}", s.handleDeleteOrg)
	inner.HandleFunc("GET /orgs/{org}/repos", s.handleListOrgRepos)

	// Org membership endpoints
	inner.HandleFunc("GET /orgs/{org}/members", s.handleListOrgMembers)
	inner.HandleFunc("POST /orgs/{org}/members/{identity}", s.handleAddOrgMember)
	inner.HandleFunc("DELETE /orgs/{org}/members/{identity}", s.handleRemoveOrgMember)

	// Org invite endpoints
	inner.HandleFunc("GET /orgs/{org}/invites", s.handleListInvites)
	inner.HandleFunc("POST /orgs/{org}/invites", s.handleCreateInvite)
	inner.HandleFunc("POST /orgs/{org}/invites/{token}/accept", s.handleAcceptInvite)
	inner.HandleFunc("DELETE /orgs/{org}/invites/{id}", s.handleRevokeInvite)

	// Repo list and create (no trailing slash — exact matches)
	inner.HandleFunc("POST /repos", s.handleCreateRepo)
	inner.HandleFunc("GET /repos", s.handleListRepos)

	// All repo-scoped operations use a prefix handler that dispatches via the
	// "/-/" separator. Examples:
	//   GET  /repos/acme/myrepo/-/tree
	//   POST /repos/acme/myrepo/-/commit
	//   GET  /repos/acme/myrepo          (bare: GET/DELETE a repo)
	inner.Handle("/repos/", http.HandlerFunc(s.handleReposPrefix))

	// Chain: IAPMiddleware → RBACMiddleware (when store present) → routes.
	// IAP must run first to set identity in context before RBAC reads it.
	var routed http.Handler = inner
	if writeStore != nil {
		routed = RBACMiddleware(writeStore, bootstrapAdmin)(inner)
	}
	outer.Handle("/", IAPMiddleware(devIdentity)(routed))
	return RequestLogger(outer)
}

type server struct {
	commitStore CommitStore
	readStore   readStore
	policyCache policyCache
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
			slog.Error("internal error", "op", "commit", "repo", repo, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal server error"})
		}
		return
	}

	// Invalidate the policy cache when committing directly to main so the
	// next merge picks up any updated .rego or OWNERS files.
	if req.Branch == "main" && s.policyCache != nil {
		s.policyCache.Invalidate(repo)
	}

	slog.Info("commit created", "repo", repo, "branch", req.Branch, "sequence", resp.Sequence, "files", len(req.Files), "author", req.Author)
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
