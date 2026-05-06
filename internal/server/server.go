package server

import (
	"context"
	cryptorand "crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/dlorenc/docstore/internal/blob"
	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/events"
	"github.com/dlorenc/docstore/internal/logstore"
	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/policy"
	"github.com/dlorenc/docstore/internal/secrets"
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

	// CI job management
	InsertCIJob(ctx context.Context, repo, branch string, sequence int64, triggerType, triggerBranch, triggerBaseBranch, triggerProposalID string, permissions []string) (*model.CIJob, error)
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
// devIdentity, if non-empty, bypasses Google ID token validation (for local dev/testing).
// bootstrapAdmin, if non-empty, has admin access to any repo with no existing admin.
// writeStore provides write operations; pass nil if only read/health endpoints are needed.
// database provides read operations; pass nil if only write/health endpoints are needed.
func New(writeStore WriteStore, database *sql.DB, devIdentity, bootstrapAdmin string) http.Handler {
	return newServer(writeStore, database, nil, nil, devIdentity, bootstrapAdmin, "", "", nil)
}

// NewWithBlobStore is like New but also wires a BlobStore into the read store
// so that files stored externally can be fetched by the file endpoint.
func NewWithBlobStore(writeStore WriteStore, database *sql.DB, bs blob.BlobStore, devIdentity, bootstrapAdmin string) http.Handler {
	return newServer(writeStore, database, bs, nil, devIdentity, bootstrapAdmin, "", "", nil)
}

// NewWithBroker is like NewWithBlobStore but also wires an event Broker.
// Use this in production so mutation handlers can emit events.
// oauthClientID is optional; when set it is advertised via /.well-known/ds-config
// so the CLI can perform the appropriate OAuth flow.
// oauthClientSecret is used by the server-side OAuth callback handler.
func NewWithBroker(writeStore WriteStore, database *sql.DB, bs blob.BlobStore, broker *events.Broker, devIdentity, bootstrapAdmin, oauthClientID, oauthClientSecret string) http.Handler {
	return newServer(writeStore, database, bs, broker, devIdentity, bootstrapAdmin, oauthClientID, oauthClientSecret, nil)
}

// NewWithPresign is like NewWithBroker but also enables presigned archive URLs.
// archiveHMACSecret is the raw HMAC secret (not base64); archiveBaseURL is the
// public server base URL used when constructing presigned URLs.
// If archiveHMACSecret is nil, presigned archive URLs are disabled.
func NewWithPresign(writeStore WriteStore, database *sql.DB, bs blob.BlobStore, broker *events.Broker,
	devIdentity, bootstrapAdmin, oauthClientID, oauthClientSecret string,
	jobStore jobTokenStore, archiveHMACSecret []byte, archiveBaseURL string) http.Handler {
	return NewWithOIDC(writeStore, database, bs, broker,
		devIdentity, bootstrapAdmin, oauthClientID, oauthClientSecret,
		jobStore, archiveHMACSecret, archiveBaseURL,
		"", "", "", nil, nil, nil)
}

// NewWithOIDC is like NewWithPresign but also enables OIDC job token authentication
// for worker-facing endpoints. Workers presenting a valid job OIDC JWT bypass Google
// ID token validation and RBAC and are routed directly to the inner handler with
// their job identity.
//
// oidcJWKSURL is the JWKS endpoint of the OIDC issuer (e.g. https://oidc.docstore.dev/.well-known/jwks.json).
// oidcAudience is the expected audience claim (e.g. "docstore").
// oidcIssuer is the expected issuer claim (e.g. "https://oidc.docstore.dev").
// If oidcJWKSURL is empty, OIDC job token auth is disabled.
// ls is the LogStore used by POST /repos/:repo/-/check/:name/logs; if nil the endpoint returns 503.
// sessionSecret is the HMAC key for signing session cookies; nil disables web UI session auth.
// secretsSvc is the repo-secrets service used by /-/secrets endpoints; nil disables them (503).
func NewWithOIDC(writeStore WriteStore, database *sql.DB, bs blob.BlobStore, broker *events.Broker,
	devIdentity, bootstrapAdmin, oauthClientID, oauthClientSecret string,
	jobStore jobTokenStore, archiveHMACSecret []byte, archiveBaseURL string,
	oidcJWKSURL, oidcAudience, oidcIssuer string, ls logstore.LogStore, sessionSecret []byte,
	secretsSvc secrets.Service) http.Handler {
	pc := policy.NewCache()
	s := &server{
		commitStore:       writeStore,
		policyCache:       pc,
		broker:            broker,
		globalAdmin:       bootstrapAdmin,
		oauthClientID:     oauthClientID,
		oauthClientSecret: oauthClientSecret,
		sessionSecret:     sessionSecret,
		archiveHMACSecret: archiveHMACSecret,
		archiveBaseURL:    archiveBaseURL,
		jobTokenStore:     jobStore,
		oidcJWKSURL:       oidcJWKSURL,
		oidcAudience:      oidcAudience,
		oidcIssuer:        oidcIssuer,
		logStore:          ls,
		secrets:           secretsSvc,
	}
	if oidcJWKSURL != "" {
		s.oidcKeyCache = newKeyCacheForURL(oidcJWKSURL)
	}
	if oauthClientID != "" {
		s.googleKeyCache = newKeyCacheForURL(googleJWKURL)
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

func newServer(writeStore WriteStore, database *sql.DB, bs blob.BlobStore, broker *events.Broker, devIdentity, bootstrapAdmin, oauthClientID, oauthClientSecret string, sessionSecret []byte) http.Handler {
	pc := policy.NewCache()
	s := &server{
		commitStore:       writeStore,
		policyCache:       pc,
		broker:            broker,
		globalAdmin:       bootstrapAdmin,
		oauthClientID:     oauthClientID,
		oauthClientSecret: oauthClientSecret,
		sessionSecret:     sessionSecret,
	}
	if oauthClientID != "" {
		s.googleKeyCache = newKeyCacheForURL(googleJWKURL)
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

// jobRequiredPermission returns the permission name required to call the given
// endpoint via an OIDC job token. An empty string means the endpoint is not
// reachable via a job token (deny by default).
func jobRequiredPermission(endpoint string) string {
	switch {
	case endpoint == "check":
		return "checks"
	case endpoint == "commit" || endpoint == "merge" || endpoint == "rebase" || endpoint == "purge":
		return "contents"
	case endpoint == "branch" || strings.HasPrefix(endpoint, "branch/"):
		return "contents"
	case endpoint == "review":
		return "proposals"
	case endpoint == "comment" || strings.HasPrefix(endpoint, "comment/"):
		return "proposals"
	case endpoint == "proposals" || strings.HasPrefix(endpoint, "proposals/"):
		return "proposals"
	case endpoint == "issues" || strings.HasPrefix(endpoint, "issues/"):
		return "issues"
	case endpoint == "releases" || strings.HasPrefix(endpoint, "releases/"):
		return "releases"
	case endpoint == "ci/run":
		return "ci"
	default:
		return ""
	}
}

// jobEndpointAllowed reports whether a job with the given permissions is
// allowed to call the endpoint. "checks" is always granted regardless of
// the permissions slice (default permission). All other permissions must be
// explicitly listed.
func jobEndpointAllowed(endpoint string, permissions []string) bool {
	required := jobRequiredPermission(endpoint)
	if required == "" {
		return false
	}
	if required == "checks" {
		return true
	}
	for _, p := range permissions {
		if p == required {
			return true
		}
	}
	return false
}

// buildHandler wires up all routes and middleware for the given server.
// Extracted so tests can construct a server with injected dependencies and still
// get the full middleware stack.
func (s *server) buildHandler(devIdentity, bootstrapAdmin string, writeStore WriteStore) http.Handler {
	// Health check, well-known config, and OAuth endpoints are exempt from auth.
	outer := http.NewServeMux()
	outer.HandleFunc("GET /healthz", handleHealth)
	outer.HandleFunc("GET /.well-known/ds-config", s.handleDSConfig)
	outer.HandleFunc("GET /auth/login", s.handleAuthLogin)
	outer.HandleFunc("GET /auth/callback", s.handleAuthCallback)
	outer.HandleFunc("GET /auth/logout", s.handleAuthLogout)

	// All other routes require authentication.
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

	// Minimal read-only web UI. Registered on `inner` so it shares Google auth + RBAC
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

	// Chain: GoogleAuthMiddleware → RBACMiddleware (when store present) → routes.
	// Auth must run first to set identity in context before RBAC reads it.
	var routed http.Handler = inner
	if writeStore != nil {
		routed = RBACMiddleware(writeStore, bootstrapAdmin)(inner)
	}
	authHandler := GoogleAuthMiddleware(devIdentity, s.oauthClientID, s.sessionSecret)(routed)

	// Wrap the auth handler: intercept presign, HMAC-signed archive, and job
	// OIDC token requests before Google ID token validation. Everything else falls through
	// to authHandler.
	// Using "/" (not "/repos/") avoids the Go mux trailing-slash redirect that
	// would turn POST /repos into a 307 → POST /repos/.
	outer.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repoName, endpoint, ok := parseRepoPath(r.URL.Path)
		if ok && repoName != "" {
			// Presign request (worker → get presigned URL): auth via request_token.
			if endpoint == "archive/presign" && r.Method == http.MethodPost {
				r.SetPathValue("name", repoName)
				s.handleArchivePresign(w, r)
				return
			}
			// HMAC-verified archive download (BuildKit → fetch source): auth via sig.
			if endpoint == "archive" && r.Method == http.MethodGet && r.URL.Query().Get("sig") != "" {
				r.SetPathValue("name", repoName)
				s.handlePresignedArchive(w, r)
				return
			}
			// Log upload (worker → upload check logs): auth via request_token.
			// Endpoint format: check/{checkName}/logs
			if r.Method == http.MethodPost && strings.HasPrefix(endpoint, "check/") && strings.HasSuffix(endpoint, "/logs") {
				middle := strings.TrimPrefix(endpoint, "check/")
				checkName := strings.TrimSuffix(middle, "/logs")
				if checkName != "" {
					r.SetPathValue("name", repoName)
					r.SetPathValue("checkName", checkName)
					s.handleCheckLogs(w, r)
					return
				}
			}
			// CI config fetch (worker → get .docstore/ci.yaml): auth via request_token.
			if endpoint == "ci/config" && r.Method == http.MethodGet {
				r.SetPathValue("name", repoName)
				s.handleCIConfig(w, r)
				return
			}
			// Check run (worker → post check result): auth via request_token.
			// Only intercept when jobTokenStore is configured; fall through to OIDC/Google auth otherwise.
			if endpoint == "check" && r.Method == http.MethodPost && s.jobTokenStore != nil {
				r.SetPathValue("name", repoName)
				s.handleCICheck(w, r)
				return
			}
			// Reveal secrets (scheduler → decrypt repo secrets at dispatch time):
			// auth via request_token. Bypasses Google auth and RBAC because the
			// bearer token IS the credential. The handler enforces its own
			// gating policy on top.
			if endpoint == "secrets/reveal" && r.Method == http.MethodPost && s.jobTokenStore != nil {
				r.SetPathValue("name", repoName)
				s.handleRevealSecrets(w, r)
				return
			}
		}

		// Job OIDC token auth: if a Bearer token is present and OIDC is configured,
		// validate it as a job OIDC JWT. On success, inject job identity into context
		// and route directly to the inner mux (bypassing Google auth and RBAC).
		// On failure, return 401. If no Bearer token, fall through to authHandler.
		if s.oidcKeyCache != nil {
			auth := r.Header.Get("Authorization")
			if tok := strings.TrimPrefix(auth, "Bearer "); tok != "" && tok != auth {
				jobID, err := validateJobOIDCToken(tok, s.oidcKeyCache.get, s.oidcAudience, s.oidcIssuer)
				if err != nil {
					slog.Warn("job token auth failed", "reason", err, "path", r.URL.Path)
					writeAPIError(w, ErrCodeUnauthorized, http.StatusUnauthorized, "unauthenticated")
					return
				}
				// SECURITY: Enforce that OIDC-authenticated jobs can only write to
				// their own repo via the allowlisted endpoint. Read-only requests
				// (GET, HEAD) may access any accessible path. Write operations
				// must target the job's own repo AND be on the allowlist.
				// Write operations on non-repo-scoped paths (e.g. POST /repos,
				// POST /orgs) are not permitted for job tokens.
				if r.Method != http.MethodGet && r.Method != http.MethodHead {
					urlRepo, endpoint, ok := parseRepoPath(r.URL.Path)
					if !ok || urlRepo == "" {
						slog.Warn("job token non-repo write denied", "job_repo", jobID.Repo, "path", r.URL.Path, "method", r.Method)
						writeAPIError(w, ErrCodeForbidden, http.StatusForbidden, "forbidden: job token not permitted for this endpoint")
						return
					}
					if jobID.Repo != urlRepo {
						slog.Warn("job token repo mismatch", "job_repo", jobID.Repo, "url_repo", urlRepo, "path", r.URL.Path)
						writeAPIError(w, ErrCodeForbidden, http.StatusForbidden, "forbidden: job may only write to its own repo")
						return
					}
					// Permissions gate: check that the endpoint is permitted by
					// the job's declared permissions. "checks" is always the
					// default; additional endpoints are unlocked by the
					// permissions block in .docstore/ci.yaml.
					if !jobEndpointAllowed(endpoint, jobID.Permissions) {
						slog.Warn("job token endpoint not permitted", "job_repo", jobID.Repo, "endpoint", endpoint, "path", r.URL.Path, "permissions", jobID.Permissions)
						writeAPIError(w, ErrCodeForbidden, http.StatusForbidden, "forbidden: job token not permitted for this endpoint")
						return
					}
				}
				identity := "ci-job:" + jobID.JobID
				if rl := requestLogFromContext(r.Context()); rl != nil {
					rl.identity = identity
				}
				ctx := context.WithValue(r.Context(), jobIdentityKey, jobID)
				ctx = context.WithValue(ctx, identityKey, identity)
				inner.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}

		authHandler.ServeHTTP(w, r)
	}))
	return RequestLogger(outer)
}

// jobTokenStore is the subset of db.Store needed for request_token validation.
type jobTokenStore interface {
	LookupRequestToken(ctx context.Context, hashedToken string) (*model.CIJob, error)
}

type server struct {
	commitStore CommitStore
	readStore   readStore
	policyCache policyCache
	broker      *events.Broker
	svc         *service.Service
	// logFetcher reads CI log objects. Nil in production (uses real GCS);
	// injected in tests to avoid real GCS calls.
	logFetcher logFetcher
	// globalAdmin is the identity that may manage global resources like
	// event subscriptions. Corresponds to the --bootstrap-admin flag.
	globalAdmin string
	// oauthClientID is the Google OAuth 2.0 client ID. Advertised via
	// /.well-known/ds-config so CLI tools can perform the OAuth flow.
	// Also used to validate the audience claim of Google ID tokens.
	oauthClientID string
	// oauthClientSecret is the Google OAuth 2.0 client secret used by
	// the server-side /auth/callback handler to exchange authorization codes.
	oauthClientSecret string
	// sessionSecret is the HMAC key for signing session cookies.
	// nil disables session cookie auth for the web UI.
	sessionSecret []byte
	// archiveHMACSecret is the raw HMAC key for presigned archive URLs.
	// nil means the feature is disabled.
	archiveHMACSecret []byte
	// archiveBaseURL is the public server base URL used when constructing presigned URLs.
	archiveBaseURL string
	// jobTokenStore validates request_tokens for presigned archive URL issuance.
	jobTokenStore jobTokenStore
	// OIDC job token authentication fields.
	// oidcJWKSURL is the JWKS endpoint; empty disables OIDC auth.
	oidcJWKSURL  string
	oidcAudience string
	oidcIssuer   string
	oidcKeyCache *keyCache
	// googleKeyCache caches Google's public JWK keys for validating ID tokens
	// during the OAuth callback. Initialized at startup when oauthClientID is set.
	googleKeyCache *keyCache
	// logStore writes CI check logs; used by POST /repos/:repo/-/check/:name/logs.
	// nil means the endpoint is disabled (returns 503).
	logStore logstore.LogStore
	// secrets is the repo-level secrets service. nil disables the
	// /-/secrets endpoints (returns 503).
	secrets secrets.Service
	// emitter overrides the broker for event emission when non-nil. Used by
	// tests to capture events without a real broker; production wiring leaves
	// this nil and the broker handles delivery.
	emitter eventEmitter
}

// handleDSConfig serves GET /.well-known/ds-config — unauthenticated.
// It advertises the server's authentication configuration so CLI tools can
// perform the appropriate OAuth flow.
func (s *server) handleDSConfig(w http.ResponseWriter, r *http.Request) {
	if s.oauthClientID == "" {
		writeJSON(w, http.StatusOK, map[string]any{"auth": map[string]string{"type": "none"}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"auth": map[string]any{
		"type":      "oauth",
		"client_id": s.oauthClientID,
	}})
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

// eventEmitter is the subset of *events.Broker the server uses to publish
// domain events. Defining it as an interface lets tests inject an in-memory
// recorder without spinning up a real broker / Postgres event_log.
//
// *events.Broker satisfies this interface.
type eventEmitter interface {
	Emit(ctx context.Context, e events.Event)
}

// emit publishes an event to the configured broker or test emitter, if any.
// A non-nil emitter overrides the broker — production wiring leaves emitter
// nil and uses the broker; tests that want to capture events without a real
// broker set emitter directly.
func (s *server) emit(ctx context.Context, e events.Event) {
	switch {
	case s.emitter != nil:
		s.emitter.Emit(ctx, e)
	case s.broker != nil:
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

// --- OAuth2 web authentication handlers ---

// oauthStateCookieName is the cookie used to carry the CSRF state during OAuth.
const oauthStateCookieName = "ds_oauth_state"

// handleAuthLogin redirects the browser to Google's OAuth consent page.
func (s *server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if s.oauthClientID == "" || s.oauthClientSecret == "" || len(s.sessionSecret) == 0 {
		http.Error(w, "OAuth not configured", http.StatusServiceUnavailable)
		return
	}

	redirect := safeRedirect(r.URL.Query().Get("redirect"))

	stateBytes := make([]byte, 16)
	if _, err := cryptorand.Read(stateBytes); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	state := base64.RawURLEncoding.EncodeToString(stateBytes)

	// Store state and post-auth redirect in a short-lived cookie.
	stateCookie := &http.Cookie{
		Name:     oauthStateCookieName,
		Value:    state + "|" + redirect,
		Path:     "/auth/",
		MaxAge:   300, // 5 minutes
		HttpOnly: true,
		Secure:   isSecureRequest(r),
		SameSite: http.SameSiteLaxMode,
	}
	http.SetCookie(w, stateCookie)

	conf := s.oauthConfig(r)
	authURL := conf.AuthCodeURL(state, oauth2.AccessTypeOnline)
	http.Redirect(w, r, authURL, http.StatusFound)
}

// handleAuthCallback handles the OAuth2 authorization code callback from Google.
func (s *server) handleAuthCallback(w http.ResponseWriter, r *http.Request) {
	if s.oauthClientID == "" || s.oauthClientSecret == "" || len(s.sessionSecret) == 0 {
		http.Error(w, "OAuth not configured", http.StatusServiceUnavailable)
		return
	}

	// Validate state from cookie.
	stateCookie, err := r.Cookie(oauthStateCookieName)
	if err != nil {
		http.Error(w, "missing state cookie", http.StatusBadRequest)
		return
	}
	parts := strings.SplitN(stateCookie.Value, "|", 2)
	if len(parts) != 2 {
		http.Error(w, "malformed state cookie", http.StatusBadRequest)
		return
	}
	expectedState, redirect := parts[0], parts[1]

	if r.URL.Query().Get("state") != expectedState {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing authorization code", http.StatusBadRequest)
		return
	}

	// Exchange code for tokens.
	conf := s.oauthConfig(r)
	tok, err := conf.Exchange(r.Context(), code)
	if err != nil {
		slog.Warn("oauth exchange failed", "error", err)
		http.Error(w, "token exchange failed", http.StatusInternalServerError)
		return
	}

	idToken, _ := tok.Extra("id_token").(string)
	if idToken == "" {
		http.Error(w, "no id_token in response", http.StatusInternalServerError)
		return
	}

	// Validate the ID token and extract email.
	email, err := validateGoogleIDToken(idToken, s.googleKeyCache.get, s.oauthClientID)
	if err != nil {
		slog.Warn("oauth id_token invalid", "error", err)
		http.Error(w, "invalid id token", http.StatusUnauthorized)
		return
	}

	// Clear the state cookie.
	http.SetCookie(w, &http.Cookie{
		Name:     oauthStateCookieName,
		Value:    "",
		Path:     "/auth/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   isSecureRequest(r),
	})

	// Set session cookie (valid for 24 hours).
	expiry := time.Now().Add(24 * time.Hour)
	sessionCookie := createSessionCookie(email, expiry, s.sessionSecret, isSecureRequest(r))
	http.SetCookie(w, sessionCookie)

	http.Redirect(w, r, safeRedirect(redirect), http.StatusFound)
}

// safeRedirect validates that redirect is a safe relative path.
// It must start with "/" and must not be a protocol-relative URL ("//...")
// or contain "://" (absolute URL). Returns "/ui/" if the value is invalid.
func safeRedirect(redirect string) string {
	if redirect == "" || !strings.HasPrefix(redirect, "/") || strings.HasPrefix(redirect, "//") || strings.Contains(redirect, "://") {
		return "/ui/"
	}
	return redirect
}

// handleAuthLogout clears the session cookie and redirects to the home page.
func (s *server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   isSecureRequest(r),
	})
	http.Redirect(w, r, "/ui/", http.StatusFound)
}

// isSecureRequest returns true if the request was made over HTTPS, checking
// X-Forwarded-Proto first (for requests behind a TLS-terminating load balancer).
func isSecureRequest(r *http.Request) bool {
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		return proto == "https"
	}
	return r.TLS != nil
}

// oauthConfig returns an oauth2.Config for the server's OAuth client.
// The redirect URL uses the request's scheme and host.
func (s *server) oauthConfig(r *http.Request) *oauth2.Config {
	scheme := "https"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	} else if r.TLS == nil {
		scheme = "http"
	}
	redirectURL := fmt.Sprintf("%s://%s/auth/callback", scheme, r.Host)
	return &oauth2.Config{
		ClientID:     s.oauthClientID,
		ClientSecret: s.oauthClientSecret,
		RedirectURL:  redirectURL,
		Scopes:       []string{"openid", "email"},
		Endpoint:     google.Endpoint,
	}
}

func notImplemented(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "not implemented")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
