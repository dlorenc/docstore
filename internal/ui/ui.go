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
}

// WriteStoreLite is the subset of server.WriteStore that the UI needs for
// listing repos and orgs. The UI never calls mutating methods.
type WriteStoreLite interface {
	ListRepos(ctx context.Context) ([]model.Repo, error)
	ListOrgs(ctx context.Context) ([]model.Org, error)
	GetRepo(ctx context.Context, name string) (*model.Repo, error)
}

// AssembleFn builds the full branch context snapshot used by the branch detail
// page. The server wraps *server.AssembleAgentContext with an identity lookup
// and injects the result, so the UI never sees identity directly.
type AssembleFn func(ctx context.Context, repo, branch string) (*model.AgentContextResponse, error)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

// Handler renders the web UI.
type Handler struct {
	read      ReadStore
	write     WriteStoreLite
	assemble  AssembleFn
	tmpl      *templateSet
	staticSub fs.FS
}

// NewHandler constructs a UI handler wired to the given data sources.
func NewHandler(read ReadStore, write WriteStoreLite, assemble AssembleFn) (*Handler, error) {
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
		tmpl:      t,
		staticSub: sub,
	}, nil
}

// NewHandlerDev is like NewHandler but reads templates and static files from
// the local filesystem at runtime instead of the embedded copies. Use this
// during development so template edits take effect on the next request without
// recompiling. The server must be run from the repository root so that the
// path "internal/ui" resolves correctly.
func NewHandlerDev(read ReadStore, write WriteStoreLite, assemble AssembleFn) (*Handler, error) {
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
		tmpl:      t,
		staticSub: static,
	}, nil
}

// Register wires UI routes onto the provided mux. Caller is responsible for
// placing this mux behind the same middleware chain used by the JSON API.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /ui/{$}", h.handleRepos)
	mux.HandleFunc("GET /ui/r/{owner}/{name}", h.handleBranches)
	mux.HandleFunc("GET /ui/r/{owner}/{name}/b/{branch}", h.handleBranchDetail)
	mux.HandleFunc("GET /ui/_/r/{owner}/{name}/b/{branch}/checks", h.handleChecksPartial)
	mux.HandleFunc("GET /ui/r/{owner}/{name}/f/{path...}", h.handleFile)
	mux.Handle("GET /ui/static/", http.StripPrefix("/ui/static/", http.FileServer(http.FS(h.staticSub))))
}
