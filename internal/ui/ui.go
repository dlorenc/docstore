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
// listing repos and orgs, and for org invite acceptance.
type WriteStoreLite interface {
	ListRepos(ctx context.Context) ([]model.Repo, error)
	ListOrgs(ctx context.Context) ([]model.Org, error)
	GetRepo(ctx context.Context, name string) (*model.Repo, error)
	ListReviewComments(ctx context.Context, repo, branch string, path *string) ([]model.ReviewComment, error)
	ListOrgMembers(ctx context.Context, org string) ([]model.OrgMember, error)
	ListRoles(ctx context.Context, repo string) ([]model.Role, error)
	ListCheckRuns(ctx context.Context, repo, branch string, atSeq *int64, history bool) ([]model.CheckRun, error)
	ListIssues(ctx context.Context, repo, stateFilter, authorFilter string) ([]model.Issue, error)
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
	}, nil
}

// Register wires UI routes onto the provided mux. Caller is responsible for
// placing this mux behind the same middleware chain used by the JSON API.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /ui/{$}", h.handleRepos)
	mux.HandleFunc("GET /ui/o/{org}", h.handleOrg)
	mux.HandleFunc("GET /ui/r/{owner}/{name}", h.handleBranches)
	mux.HandleFunc("GET /ui/r/{owner}/{name}/b/{branch}", h.handleBranchDetail)
	mux.HandleFunc("GET /ui/_/r/{owner}/{name}/b/{branch}/checks", h.handleChecksPartial)
	mux.HandleFunc("GET /ui/_/r/{owner}/{name}/b/{branch}/comments", h.handleReviewCommentsPartial)
	mux.HandleFunc("GET /ui/r/{owner}/{name}/b/{branch}/log", h.handleCommitLog)
	mux.HandleFunc("GET /ui/_/r/{owner}/{name}/b/{branch}/log", h.handleLogRowsPartial)
	mux.HandleFunc("GET /ui/r/{owner}/{name}/b/{branch}/c/{seq}", h.handleCommitDetail)
	mux.HandleFunc("GET /ui/_/r/{owner}/{name}/b/{branch}/check-history", h.handleCheckHistoryPartial)
	mux.HandleFunc("GET /ui/r/{owner}/{name}/f/{path...}", h.handleFile)
	mux.HandleFunc("GET /ui/r/{owner}/{name}/issues", h.handleIssues)
	mux.HandleFunc("GET /ui/r/{owner}/{name}/issues/{number}", h.handleIssueDetail)
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
	mux.Handle("GET /ui/static/", http.StripPrefix("/ui/static/", http.FileServer(http.FS(h.staticSub))))
}
