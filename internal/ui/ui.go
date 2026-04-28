// Package ui serves a minimal read-only web UI over the docstore HTTP API.
//
// The UI is server-rendered with html/template + HTMX. All assets are embedded
// so the server still ships as a single binary. The package does not perform
// any mutations; users who need to write data use the `ds` CLI.
package ui

import (
	"context"
	"embed"
	"io/fs"
	"net/http"
	"os"
	"time"

	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/store"
)

// ReadStore is the subset of server.ReadStore that the UI needs.
type ReadStore interface {
	MaterializeTree(ctx context.Context, repo, branch string, atSeq *int64, limit int, afterPath string) ([]store.TreeEntry, error)
	GetFile(ctx context.Context, repo, branch, path string, atSeq *int64) (*store.FileContent, error)
	GetBranch(ctx context.Context, repo, branch string) (*store.BranchInfo, error)
	ListBranches(ctx context.Context, repo, statusFilter string, includeDraft, onlyDraft bool) ([]store.BranchInfo, error)
	GetFileHistory(ctx context.Context, repo, branch, path string, limit int, afterSeq *int64) ([]store.FileHistoryEntry, error)
	GetChain(ctx context.Context, repo string, from, to int64) ([]store.ChainEntry, error)
	GetCommit(ctx context.Context, repo string, seq int64) (*store.CommitDetail, error)
}

// WriteStoreLite is the subset of server.WriteStore that the UI needs for
// listing repos and orgs, org invite acceptance, issue write operations,
// branch write operations, and review/comment/proposal write operations.
type WriteStoreLite interface {
	ListRepos(ctx context.Context) ([]model.Repo, error)
	ListOrgs(ctx context.Context) ([]model.Org, error)
	GetRepo(ctx context.Context, name string) (*model.Repo, error)
	ListReviewComments(ctx context.Context, repo, branch string, path *string) ([]model.ReviewComment, error)
	ListOrgMembers(ctx context.Context, org string) ([]model.OrgMember, error)
	ListOrgMemberships(ctx context.Context, identity string) ([]model.OrgMember, error)
	ListRoles(ctx context.Context, repo string) ([]model.Role, error)
	ListRolesByIdentity(ctx context.Context, identity string) ([]model.RepoRole, error)
	ListCheckRuns(ctx context.Context, repo, branch string, atSeq *int64, history bool) ([]model.CheckRun, error)
	ListIssues(ctx context.Context, repo, stateFilter, authorFilter, labelFilter string) ([]model.Issue, error)
	GetIssue(ctx context.Context, repo string, number int64) (*model.Issue, error)
	ListIssueComments(ctx context.Context, repo string, number int64) ([]model.IssueComment, error)
	GetInviteByToken(ctx context.Context, org, token string) (*model.OrgInvite, error)
	AcceptInvite(ctx context.Context, org, token, identity string) error
	ListInvites(ctx context.Context, org string) ([]model.OrgInvite, error)
	ListOrgRepos(ctx context.Context, owner string) ([]model.Repo, error)
	ListProposals(ctx context.Context, repo string, state *model.ProposalState, branch *string) ([]*model.Proposal, error)
	GetProposal(ctx context.Context, repo, proposalID string) (*model.Proposal, error)
	ListReleases(ctx context.Context, repo string, limit int, afterID string) ([]model.Release, error)
	GetRelease(ctx context.Context, repo, name string) (*model.Release, error)
	ListIssueRefs(ctx context.Context, repo string, number int64) ([]model.IssueRef, error)
	ListCIJobs(ctx context.Context, repo string, branch, status *string, limit int) ([]model.CIJob, error)
	GetCIJob(ctx context.Context, id string) (*model.CIJob, error)
	CreateOrg(ctx context.Context, name, createdBy string) (*model.Org, error)
	CreateRepo(ctx context.Context, req model.CreateRepoRequest) (*model.Repo, error)

	// Issue write operations.
	CreateIssue(ctx context.Context, repo, title, body, author string, labels []string) (*model.Issue, error)
	UpdateIssue(ctx context.Context, repo string, number int64, title, body *string, labels *[]string) (*model.Issue, error)
	CloseIssue(ctx context.Context, repo string, number int64, reason model.IssueCloseReason, closedBy string) (*model.Issue, error)
	ReopenIssue(ctx context.Context, repo string, number int64) (*model.Issue, error)
	CreateIssueComment(ctx context.Context, repo string, number int64, body, author string) (*model.IssueComment, error)
	UpdateIssueComment(ctx context.Context, repo, id, body string) (*model.IssueComment, error)
	DeleteIssueComment(ctx context.Context, repo, id string) error
	CreateIssueRef(ctx context.Context, repo string, number int64, refType model.IssueRefType, refID string) (*model.IssueRef, error)

	// Branch write operations.
	CreateBranch(ctx context.Context, req model.CreateBranchRequest) (*model.CreateBranchResponse, error)
	UpdateBranchDraft(ctx context.Context, repo, name string, draft bool) error
	DeleteBranch(ctx context.Context, repo, name string) error
	SetBranchAutoMerge(ctx context.Context, repo, name string, autoMerge bool) error

	// Review/comment/proposal write operations.
	CreateReview(ctx context.Context, repo, branch, reviewer string, status model.ReviewStatus, body string) (*model.Review, error)
	CreateReviewComment(ctx context.Context, repo, branch, path, versionID, body, author string, reviewID *string) (*model.ReviewComment, error)
	DeleteReviewComment(ctx context.Context, repo, id string) error
	CreateProposal(ctx context.Context, repo, branch, baseBranch, title, description, author string) (*model.Proposal, error)
	UpdateProposal(ctx context.Context, repo, proposalID string, title, description *string) (*model.Proposal, error)
	CloseProposal(ctx context.Context, repo, proposalID string) error
	// Org member/invite, role, and release write operations.
	AddOrgMember(ctx context.Context, org, identity string, role model.OrgRole, invitedBy string) error
	RemoveOrgMember(ctx context.Context, org, identity string) error
	CreateInvite(ctx context.Context, org, email string, role model.OrgRole, invitedBy, token string, expiresAt time.Time) (*model.OrgInvite, error)
	RevokeInvite(ctx context.Context, org, inviteID string) error
	SetRole(ctx context.Context, repo, identity string, role model.RoleType) error
	DeleteRole(ctx context.Context, repo, identity string) error
	CreateRelease(ctx context.Context, repo, name string, sequence int64, body, createdBy string) (*model.Release, error)
	DeleteRelease(ctx context.Context, repo, name string) error

	// Org/repo delete operations.
	DeleteOrg(ctx context.Context, name string) error
	DeleteRepo(ctx context.Context, name string) error

	// Commit write operation.
	Commit(ctx context.Context, req model.CommitRequest) (*model.CommitResponse, error)
}

// AssembleFn builds the full branch context snapshot used by the branch detail
// page. The server wraps *server.AssembleAgentContext with an identity lookup
// and injects the result, so the UI never sees identity directly.
type AssembleFn func(ctx context.Context, repo, branch string) (*model.AgentContextResponse, error)

// IdentityFn extracts the authenticated caller identity from a request context.
// It is provided by the server at Handler construction time so the UI never
// needs to import the server package (which would create a circular dependency).
type IdentityFn func(ctx context.Context) string

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

// Handler renders the web UI.
type Handler struct {
	read      ReadStore
	write     WriteStoreLite
	assemble  AssembleFn
	identity  IdentityFn
	tmpl      *templateSet
	staticSub fs.FS
	// devMode disables the Secure flag on the CSRF cookie so the UI works
	// over plain HTTP in local development.
	devMode bool
	// Emit, if non-nil, is called after each successful write operation to
	// publish a domain event. The server wires this to the event broker.
	Emit func(ctx context.Context, event any)
}

// emit publishes an event if an emitter is configured.
func (h *Handler) emit(ctx context.Context, event any) {
	if h.Emit != nil {
		h.Emit(ctx, event)
	}
}

// NewHandler constructs a UI handler wired to the given data sources.
func NewHandler(read ReadStore, write WriteStoreLite, assemble AssembleFn, identity IdentityFn) (*Handler, error) {
	t, err := parseTemplates(templatesFS)
	if err != nil {
		return nil, err
	}
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	return &Handler{
		read:      read,
		write:     write,
		assemble:  assemble,
		identity:  identity,
		tmpl:      t,
		staticSub: sub,
	}, nil
}

// NewHandlerDev is like NewHandler but reads templates and static files from
// the local filesystem at runtime instead of the embedded copies. Use this
// during development so template edits take effect on the next request without
// recompiling. The server must be run from the repository root so that the
// path "internal/ui" resolves correctly.
func NewHandlerDev(read ReadStore, write WriteStoreLite, assemble AssembleFn, identity IdentityFn) (*Handler, error) {
	root := os.DirFS("internal/ui")
	t, err := parseTemplates(root)
	if err != nil {
		return nil, err
	}
	static, err := fs.Sub(root, "static")
	if err != nil {
		return nil, err
	}
	return &Handler{
		read:      read,
		write:     write,
		assemble:  assemble,
		identity:  identity,
		tmpl:      t,
		staticSub: static,
		devMode:   true,
	}, nil
}

// Register wires UI routes onto the provided mux behind CSRF middleware.
// Caller is responsible for placing this mux behind the same middleware chain
// used by the JSON API.
func (h *Handler) Register(mux *http.ServeMux) {
	inner := http.NewServeMux()
	h.registerRoutes(inner)
	// Wrap all UI routes with CSRF protection. Secure=true in production;
	// Secure=false in dev mode so the cookie works over plain HTTP.
	mux.Handle("/ui/", CSRFMiddleware(!h.devMode, inner))
}

// registerRoutes adds all UI route handlers to the provided mux. Called by
// Register after wrapping the mux with CSRF middleware.
func (h *Handler) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /ui/{$}", h.handleRepos)
	mux.HandleFunc("GET /ui/u/{identity}", h.handleUserProfile)
	mux.HandleFunc("GET /ui/o/{org}", h.handleOrg)
	mux.HandleFunc("GET /ui/r/{owner}/{name}", h.handleBranches)
	mux.HandleFunc("GET /ui/r/{owner}/{name}/settings", h.handleRepoSettings)
	mux.HandleFunc("GET /ui/r/{owner}/{name}/b/{branch}", h.handleBranchDetail)
	mux.HandleFunc("GET /ui/_/r/{owner}/{name}/b/{branch}/checks", h.handleChecksPartial)
	mux.HandleFunc("GET /ui/_/r/{owner}/{name}/b/{branch}/comments", h.handleReviewCommentsPartial)
	mux.HandleFunc("GET /ui/r/{owner}/{name}/b/{branch}/log", h.handleCommitLog)
	mux.HandleFunc("GET /ui/_/r/{owner}/{name}/b/{branch}/log", h.handleLogRowsPartial)
	mux.HandleFunc("GET /ui/r/{owner}/{name}/b/{branch}/c/{seq}", h.handleCommitDetail)
	mux.HandleFunc("GET /ui/_/r/{owner}/{name}/b/{branch}/check-history", h.handleCheckHistoryPartial)
	mux.HandleFunc("GET /ui/r/{owner}/{name}/f/{path...}", h.handleFile)
	mux.HandleFunc("GET /ui/r/{owner}/{name}/issues", h.handleIssues)
	mux.HandleFunc("GET /ui/r/{owner}/{name}/issues/new", h.handleNewIssue)
	mux.HandleFunc("POST /ui/r/{owner}/{name}/issues/new", h.handleNewIssue)
	mux.HandleFunc("GET /ui/r/{owner}/{name}/issues/{number}", h.handleIssueDetail)
	mux.HandleFunc("POST /ui/r/{owner}/{name}/issues/{number}/edit", h.handleEditIssue)
	mux.HandleFunc("POST /ui/r/{owner}/{name}/issues/{number}/close", h.handleIssueClose)
	mux.HandleFunc("POST /ui/r/{owner}/{name}/issues/{number}/reopen", h.handleIssueReopen)
	mux.HandleFunc("POST /ui/r/{owner}/{name}/issues/{number}/comments", h.handleCreateIssueComment)
	mux.HandleFunc("POST /ui/r/{owner}/{name}/issues/{number}/comments/{id}/edit", h.handleEditIssueComment)
	mux.HandleFunc("POST /ui/r/{owner}/{name}/issues/{number}/comments/{id}/delete", h.handleDeleteIssueComment)
	mux.HandleFunc("POST /ui/r/{owner}/{name}/issues/{number}/refs", h.handleAddIssueRef)
	mux.HandleFunc("GET /ui/_/r/{owner}/{name}/issues", h.handleIssuesPartial)
	mux.HandleFunc("GET /ui/r/{owner}/{name}/proposals", h.handleProposals)
	mux.HandleFunc("GET /ui/_/r/{owner}/{name}/proposals", h.handleProposalsPartial)
	mux.HandleFunc("GET /ui/r/{owner}/{name}/proposals/{id}", h.handleProposalDetail)
	mux.HandleFunc("GET /ui/r/{owner}/{name}/releases", h.handleReleases)
	mux.HandleFunc("GET /ui/r/{owner}/{name}/releases/{rname}", h.handleReleaseDetail)
	mux.HandleFunc("GET /ui/r/{owner}/{name}/ci-jobs", h.handleCIJobs)
	mux.HandleFunc("GET /ui/r/{owner}/{name}/ci-jobs/{id}", h.handleCIJobDetail)
	mux.HandleFunc("GET /ui/o/{org}/invites/{token}/accept", h.handleAcceptInvite)
	mux.HandleFunc("POST /ui/o/{org}/invites/{token}/accept", h.handleAcceptInvite)
	mux.HandleFunc("GET /ui/orgs/new", h.handleCreateOrg)
	mux.HandleFunc("POST /ui/orgs/new", h.handleCreateOrg)
	mux.HandleFunc("GET /ui/repos/new", h.handleCreateRepo)
	mux.HandleFunc("POST /ui/repos/new", h.handleCreateRepo)

	// Branch write operations (POST-redirect-GET pattern).
	mux.HandleFunc("POST /ui/r/{owner}/{name}/-/create-branch", h.handleUICreateBranch)
	mux.HandleFunc("POST /ui/r/{owner}/{name}/-/delete-branch", h.handleUIDeleteBranch)
	mux.HandleFunc("POST /ui/r/{owner}/{name}/-/promote-branch", h.handleUIPromoteBranch)
	mux.HandleFunc("POST /ui/r/{owner}/{name}/-/set-auto-merge", h.handleUISetAutoMerge)

	// Write operations — POST forms from branch detail and proposals pages.
	mux.HandleFunc("POST /ui/r/{owner}/{name}/b/{branch}/review", h.handleSubmitReview)
	mux.HandleFunc("POST /ui/r/{owner}/{name}/b/{branch}/comment", h.handlePostComment)
	mux.HandleFunc("POST /ui/r/{owner}/{name}/b/{branch}/comment/{id}/delete", h.handleDeleteComment)
	mux.HandleFunc("POST /ui/r/{owner}/{name}/proposals", h.handleCreateProposalUI)
	mux.HandleFunc("POST /ui/r/{owner}/{name}/proposals/{id}/edit", h.handleEditProposal)
	mux.HandleFunc("POST /ui/r/{owner}/{name}/proposals/{id}/close", h.handleCloseProposalUI)
	// Write operations: org members.
	mux.HandleFunc("POST /ui/o/{org}/members", h.handleAddOrgMember)
	mux.HandleFunc("POST /ui/o/{org}/members/{identity}/remove", h.handleRemoveOrgMember)
	// Write operations: org invites.
	mux.HandleFunc("POST /ui/o/{org}/invites", h.handleCreateInvite)
	mux.HandleFunc("POST /ui/o/{org}/invites/{id}/revoke", h.handleRevokeInvite)
	// Write operations: repo roles.
	mux.HandleFunc("POST /ui/r/{owner}/{name}/roles", h.handleSetRole)
	mux.HandleFunc("POST /ui/r/{owner}/{name}/roles/{identity}/delete", h.handleDeleteRole)
	// Write operations: releases.
	mux.HandleFunc("POST /ui/r/{owner}/{name}/releases", h.handleCreateRelease)
	mux.HandleFunc("POST /ui/r/{owner}/{name}/releases/{rname}/delete", h.handleDeleteRelease)
	// Write operations: delete org/repo.
	mux.HandleFunc("POST /ui/o/{org}/delete", h.handleUIDeleteOrg)
	mux.HandleFunc("POST /ui/r/{owner}/{name}/-/delete-repo", h.handleUIDeleteRepo)
	// Commit form.
	mux.HandleFunc("GET /ui/r/{owner}/{name}/b/{branch}/commit", h.handleNewCommit)
	mux.HandleFunc("POST /ui/r/{owner}/{name}/b/{branch}/commit", h.handleNewCommit)

	mux.Handle("GET /ui/static/", http.StripPrefix("/ui/static/", http.FileServer(http.FS(h.staticSub))))
}
