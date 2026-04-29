package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/dlorenc/docstore/internal/blob"
	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/events"
	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/policy"
	"github.com/dlorenc/docstore/internal/service"
	"github.com/dlorenc/docstore/internal/store"
	"github.com/dlorenc/docstore/internal/ui"
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
	ListOrgMemberships(ctx context.Context, identity string) ([]model.OrgMember, error)

	// Org invitations
	CreateInvite(ctx context.Context, org, email string, role model.OrgRole, invitedBy, token string, expiresAt time.Time) (*model.OrgInvite, error)
	ListInvites(ctx context.Context, org string) ([]model.OrgInvite, error)
	GetInviteByToken(ctx context.Context, org, token string) (*model.OrgInvite, error)
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
	SetBranchAutoMerge(ctx context.Context, repo, name string, autoMerge bool) error
	Merge(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error)
	DeleteBranch(ctx context.Context, repo, name string) error
	Rebase(ctx context.Context, req model.RebaseRequest) (*model.RebaseResponse, []db.MergeConflict, error)

	// Review and check-run operations
	CreateReview(ctx context.Context, repo, branch, reviewer string, status model.ReviewStatus, body string) (*model.Review, error)
	ListReviews(ctx context.Context, repo, branch string, atSeq *int64) ([]model.Review, error)
	CreateCheckRun(ctx context.Context, repo, branch, checkName string, status model.CheckRunStatus, reporter string, logURL *string, atSequence *int64, attempt int16, metadata json.RawMessage) (*model.CheckRun, error)
	ListCheckRuns(ctx context.Context, repo, branch string, atSeq *int64, history bool) ([]model.CheckRun, error)
	RetryChecks(ctx context.Context, repo, branch string, seq int64, checks []string) (int16, error)

	// Review comment operations
	CreateReviewComment(ctx context.Context, repo, branch, path, versionID, body, author string, reviewID *string) (*model.ReviewComment, error)
	ListReviewComments(ctx context.Context, repo, branch string, path *string) ([]model.ReviewComment, error)
	GetReviewComment(ctx context.Context, repo, id string) (*model.ReviewComment, error)
	DeleteReviewComment(ctx context.Context, repo, id string) error

	// Role management
	GetRole(ctx context.Context, repo, identity string) (*model.Role, error)
	SetRole(ctx context.Context, repo, identity string, role model.RoleType) error
	DeleteRole(ctx context.Context, repo, identity string) error
	ListRoles(ctx context.Context, repo string) ([]model.Role, error)
	ListRolesByIdentity(ctx context.Context, identity string) ([]model.RepoRole, error)
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

	// Event subscription management
	CreateSubscription(ctx context.Context, req model.CreateSubscriptionRequest) (*model.EventSubscription, error)
	GetSubscription(ctx context.Context, id string) (*model.EventSubscription, error)
	ListSubscriptions(ctx context.Context) ([]model.EventSubscription, error)
	ListSubscriptionsByCreator(ctx context.Context, createdBy string) ([]model.EventSubscription, error)
	DeleteSubscription(ctx context.Context, id string) error
	ResumeSubscription(ctx context.Context, id string) error

	// Proposal management
	CreateProposal(ctx context.Context, repo, branch, baseBranch, title, description, author string) (*model.Proposal, error)
	GetProposal(ctx context.Context, repo, proposalID string) (*model.Proposal, error)
	ListProposals(ctx context.Context, repo string, state *model.ProposalState, branch *string) ([]*model.Proposal, error)
	UpdateProposal(ctx context.Context, repo, proposalID string, title, description *string) (*model.Proposal, error)
	CloseProposal(ctx context.Context, repo, proposalID string) error
	MergeProposal(ctx context.Context, repo, branch string) (*model.Proposal, error)

	// Issue management
	CreateIssue(ctx context.Context, repo, title, body, author string, labels []string) (*model.Issue, error)
	GetIssue(ctx context.Context, repo string, number int64) (*model.Issue, error)
	ListIssues(ctx context.Context, repo, stateFilter, authorFilter, labelFilter string) ([]model.Issue, error)
	UpdateIssue(ctx context.Context, repo string, number int64, title, body *string, labels *[]string) (*model.Issue, error)
	CloseIssue(ctx context.Context, repo string, number int64, reason model.IssueCloseReason, closedBy string) (*model.Issue, error)
	ReopenIssue(ctx context.Context, repo string, number int64) (*model.Issue, error)

	// Issue comment management
	CreateIssueComment(ctx context.Context, repo string, number int64, body, author string) (*model.IssueComment, error)
	GetIssueComment(ctx context.Context, repo, id string) (*model.IssueComment, error)
	ListIssueComments(ctx context.Context, repo string, number int64) ([]model.IssueComment, error)
	UpdateIssueComment(ctx context.Context, repo, id, body string) (*model.IssueComment, error)
	DeleteIssueComment(ctx context.Context, repo, id string) error

	// Issue ref management
	CreateIssueRef(ctx context.Context, repo string, number int64, refType model.IssueRefType, refID string) (*model.IssueRef, error)
	ListIssueRefs(ctx context.Context, repo string, number int64) ([]model.IssueRef, error)
	ListIssuesByRef(ctx context.Context, repo string, refType model.IssueRefType, refID string) ([]model.Issue, error)

	// CI job queries (read-only)
	GetCIJob(ctx context.Context, id string) (*model.CIJob, error)
	ListCIJobs(ctx context.Context, repo string, branch, status *string, limit int) ([]model.CIJob, error)
}

// CommitStore is an alias for backward compatibility with tests.
type CommitStore = WriteStore

// ReadStore is the read-only data access interface used by the server's GET handlers.
// It is exported so external test packages can inject test doubles for contract tests.
type ReadStore = readStore

// NewWithReadStore constructs a handler with explicitly injected read and write stores.
// Intended for contract tests outside this package that need to verify the server's
// wire format against clients that consume the API (e.g. cmd/ci-scheduler).
func NewWithReadStore(ws WriteStore, rs ReadStore, devIdentity, bootstrapAdmin string) http.Handler {
	pc := policy.NewCache()
	s := &server{
		commitStore: ws,
		readStore:   rs,
		policyCache: pc,
	}
	if ws != nil {
		s.svc = service.New(ws, nil, pc)
	}
	return s.buildHandler(devIdentity, bootstrapAdmin, ws)
}


// New returns an http.Handler with all routes registered.
// devIdentity, if non-empty, bypasses IAP JWT validation (for local dev/testing).
// bootstrapAdmin, if non-empty, has admin access to any repo with no existing admin.
// writeStore provides write operations; pass nil if only read/health endpoints are needed.
// database provides read operations; pass nil if only write/health endpoints are needed.
func New(writeStore WriteStore, database *sql.DB, devIdentity, bootstrapAdmin string) http.Handler {
	return newServer(writeStore, database, nil, nil, devIdentity, bootstrapAdmin, "", "")
}

// NewWithBlobStore is like New but also wires a BlobStore into the read store
// so that files stored externally can be fetched by the file endpoint.
func NewWithBlobStore(writeStore WriteStore, database *sql.DB, bs blob.BlobStore, devIdentity, bootstrapAdmin string) http.Handler {
	return newServer(writeStore, database, bs, nil, devIdentity, bootstrapAdmin, "", "")
}

// NewWithBroker is like NewWithBlobStore but also wires an event Broker.
// Use this in production so mutation handlers can emit events.
// iapClientID and iapClientSecret are optional; when set they are advertised
// via the /.well-known/ds-config endpoint so the CLI can perform the OAuth flow.
func NewWithBroker(writeStore WriteStore, database *sql.DB, bs blob.BlobStore, broker *events.Broker, devIdentity, bootstrapAdmin, iapClientID, iapClientSecret string) http.Handler {
	return newServer(writeStore, database, bs, broker, devIdentity, bootstrapAdmin, iapClientID, iapClientSecret)
}

func newServer(writeStore WriteStore, database *sql.DB, bs blob.BlobStore, broker *events.Broker, devIdentity, bootstrapAdmin, iapClientID, iapClientSecret string) http.Handler {
	pc := policy.NewCache()
	s := &server{
		commitStore:     writeStore,
		policyCache:     pc,
		broker:          broker,
		globalAdmin:     bootstrapAdmin,
		iapClientID:     iapClientID,
		iapClientSecret: iapClientSecret,
	}
	if writeStore != nil {
		var emitter service.EventEmitter
		if broker != nil {
			emitter = broker
		}
		s.svc = service.New(writeStore, emitter, pc)
	}
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
	// Health check and well-known config are exempt from auth.
	outer := http.NewServeMux()
	outer.HandleFunc("GET /healthz", handleHealth)
	outer.HandleFunc("GET /.well-known/ds-config", s.handleDSConfig)

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

	// Minimal read-only web UI. Registered on `inner` so it shares IAP + RBAC
	// middleware. Only wires up when both a read store and write store are
	// present — read-only mode still works for browsing, but we need the write
	// store for repo/org listings.
	if s.readStore != nil && writeStore != nil {
		assemble := func(ctx context.Context, repo, branch string) (*model.AgentContextResponse, error) {
			return s.AssembleAgentContext(ctx, repo, branch, IdentityFromContext(ctx))
		}
		var uiHandler *ui.Handler
		var err error
		if os.Getenv("DEV_UI") != "" {
			uiHandler, err = ui.NewHandlerDev(s.readStore, writeStore, s.svc, assemble, IdentityFromContext)
		} else {
			uiHandler, err = ui.NewHandler(s.readStore, writeStore, s.svc, assemble, IdentityFromContext)
		}
		if err != nil {
			slog.Error("ui init failed", "error", err)
		} else {
			uiHandler.Register(inner)
		}
	}

	// Event subscription management (delegated auth: admin for global, creator for own).
	inner.HandleFunc("POST /subscriptions", s.handleCreateSubscription)
	inner.HandleFunc("GET /subscriptions", s.handleListSubscriptions)
	inner.HandleFunc("DELETE /subscriptions/{id}", s.handleDeleteSubscription)
	inner.HandleFunc("POST /subscriptions/{id}/resume", s.handleResumeSubscription)

	// CI job by-ID lookup (caller must have reader access to job.Repo).
	inner.HandleFunc("GET /ci-jobs/{id}", s.handleGetCIJob)

	// Global SSE stream (admin only).
	inner.HandleFunc("GET /events", s.handleSSEGlobalEvents)

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
	broker      *events.Broker
	svc         *service.Service
	// globalAdmin is the identity that may manage global resources like
	// event subscriptions. Corresponds to the --bootstrap-admin flag.
	globalAdmin string
	// iapClientID and iapClientSecret are advertised via /.well-known/ds-config
	// so CLI tools can perform the IAP OAuth flow.
	iapClientID     string
	iapClientSecret string
}

// handleDSConfig serves GET /.well-known/ds-config — unauthenticated.
// It advertises the server's authentication configuration so CLI tools can
// perform the appropriate OAuth flow.
func (s *server) handleDSConfig(w http.ResponseWriter, r *http.Request) {
	if s.iapClientID == "" {
		writeJSON(w, http.StatusOK, map[string]any{"auth": map[string]string{"type": "none"}})
		return
	}
	auth := map[string]any{
		"type":      "iap",
		"client_id": s.iapClientID,
	}
	if s.iapClientSecret != "" {
		auth["client_secret"] = s.iapClientSecret
	}
	writeJSON(w, http.StatusOK, map[string]any{"auth": auth})
}

func (s *server) handleCommit(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	var req model.CommitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Repo = repo

	if req.Branch == "" {
		writeError(w, http.StatusBadRequest, "branch is required")
		return
	}
	if len(req.Files) == 0 {
		writeError(w, http.StatusBadRequest, "at least one file is required")
		return
	}
	if req.Message == "" {
		writeError(w, http.StatusBadRequest, "message is required")
		return
	}
	for _, f := range req.Files {
		if f.Path == "" {
			writeError(w, http.StatusBadRequest, "file path is required")
			return
		}
	}

	// Author always comes from the authenticated identity; clients cannot override it.
	identity := IdentityFromContext(r.Context())

	resp, err := s.svc.Commit(r.Context(), identity, req)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrBranchNotFound):
			writeAPIError(w, ErrCodeBranchNotFound, http.StatusNotFound, "branch not found")
		case errors.Is(err, db.ErrBranchNotActive):
			writeAPIError(w, ErrCodeBranchNotActive, http.StatusConflict, "branch is not active")
		default:
			slog.Error("internal error", "op", "commit", "repo", repo, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	writeJSON(w, http.StatusCreated, resp)
}

// emit publishes an event to the broker if one is configured.
func (s *server) emit(ctx context.Context, e events.Event) {
	if s.broker != nil {
		s.broker.Emit(ctx, e)
	}
}

// requireGlobalAdmin checks that the current identity is the global admin.
// Returns false and writes 403 if not.
func (s *server) requireGlobalAdmin(w http.ResponseWriter, r *http.Request) bool {
	if s.globalAdmin == "" {
		writeError(w, http.StatusForbidden, "forbidden: no global admin configured")
		return false
	}
	identity := IdentityFromContext(r.Context())
	if identity != s.globalAdmin {
		writeError(w, http.StatusForbidden, "forbidden: global admin required")
		return false
	}
	return true
}

// requireRepoReadAccess checks that the current identity has at least reader
// access to repo (any assigned role counts). Returns false and writes 403 if not.
// Returns 500 for unexpected store errors.
func (s *server) requireRepoReadAccess(w http.ResponseWriter, r *http.Request, repo string) bool {
	identity := IdentityFromContext(r.Context())
	_, err := s.commitStore.GetRole(r.Context(), repo, identity)
	if err != nil {
		if errors.Is(err, db.ErrRoleNotFound) {
			writeError(w, http.StatusForbidden, "forbidden: no access to repo")
		} else {
			slog.Error("internal error", "op", "get_role", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return false
	}
	return true
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func notImplemented(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "not implemented")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
