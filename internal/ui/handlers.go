package ui

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/store"
)

// ---------------------------------------------------------------------------
// Page data types
// ---------------------------------------------------------------------------

type reposPage struct {
	Orgs []orgGroup
}

type orgGroup struct {
	Name  string
	Repos []model.Repo
}

type branchesPage struct {
	Repo      model.Repo
	Active    []branchRow
	Merged    []branchRow
	Abandoned []branchRow
}

type branchRow struct {
	Name         string
	HeadSequence int64
	BaseSequence int64
	Draft        bool
	AutoMerge    bool
	Status       string
}

type branchDetailPage struct {
	Repo            model.Repo
	Ctx             *model.AgentContextResponse
	Blockers        []string
	PassedCheckCnt  int
	PendingCheckCnt int
	FailedCheckCnt  int
}

type filePage struct {
	Repo      model.Repo
	Branch    string
	AtSeq     *int64
	Path      string
	File      *fileView
	Tree      []treeRow
	ParentDir string
}

type fileView struct {
	Path        string
	VersionID   string
	ContentHash string
	Content     []byte
	ContentType string
}

type treeRow struct {
	Name  string
	Path  string
	IsDir bool
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// handleRepos renders the landing page listing orgs and their repos.
func (h *Handler) handleRepos(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	repos, err := h.write.ListRepos(ctx)
	if err != nil {
		slog.Error("ui list repos", "error", err)
		h.renderError(w, http.StatusInternalServerError, "could not load repos")
		return
	}
	byOrg := map[string][]model.Repo{}
	for _, rp := range repos {
		byOrg[rp.Owner] = append(byOrg[rp.Owner], rp)
	}
	orgs := make([]orgGroup, 0, len(byOrg))
	for name, list := range byOrg {
		slices.SortFunc(list, func(a, b model.Repo) int { return strings.Compare(a.Name, b.Name) })
		orgs = append(orgs, orgGroup{Name: name, Repos: list})
	}
	slices.SortFunc(orgs, func(a, b orgGroup) int { return strings.Compare(a.Name, b.Name) })

	h.render(w, h.tmpl.repos, "layout.html", pageData{
		Title:       "Repos",
		Breadcrumbs: []crumb{{Label: "repos", Href: "/ui/"}},
		Body:        reposPage{Orgs: orgs},
	})
}

// handleBranches renders the branch list for a single repo.
func (h *Handler) handleBranches(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	repoName := owner + "/" + name
	ctx := r.Context()

	repo, err := h.write.GetRepo(ctx, repoName)
	if err != nil {
		if errors.Is(err, db.ErrRepoNotFound) {
			h.renderError(w, http.StatusNotFound, "repo not found: "+repoName)
			return
		}
		slog.Error("ui get repo", "repo", repoName, "error", err)
		h.renderError(w, http.StatusInternalServerError, "could not load repo")
		return
	}

	branches, err := h.read.ListBranches(ctx, repoName, "", true, false)
	if err != nil {
		slog.Error("ui list branches", "repo", repoName, "error", err)
		h.renderError(w, http.StatusInternalServerError, "could not load branches")
		return
	}

	page := branchesPage{Repo: *repo}
	for _, b := range branches {
		row := branchRow{
			Name:         b.Name,
			HeadSequence: b.HeadSequence,
			BaseSequence: b.BaseSequence,
			Draft:        b.Draft,
			AutoMerge:    b.AutoMerge,
			Status:       b.Status,
		}
		switch b.Status {
		case "merged":
			page.Merged = append(page.Merged, row)
		case "abandoned":
			page.Abandoned = append(page.Abandoned, row)
		default:
			page.Active = append(page.Active, row)
		}
	}

	h.render(w, h.tmpl.branches, "layout.html", pageData{
		Title: repoName + " / branches",
		Breadcrumbs: []crumb{
			{Label: "repos", Href: "/ui/"},
			{Label: repoName, Href: "/ui/r/" + repoName},
		},
		Body: page,
	})
}

// handleBranchDetail renders the diff + reviews + checks + policy view.
func (h *Handler) handleBranchDetail(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	branch := r.PathValue("branch")
	repoName := owner + "/" + name

	repo, err := h.write.GetRepo(r.Context(), repoName)
	if err != nil {
		if errors.Is(err, db.ErrRepoNotFound) {
			h.renderError(w, http.StatusNotFound, "repo not found: "+repoName)
			return
		}
		slog.Error("ui get repo", "repo", repoName, "error", err)
		h.renderError(w, http.StatusInternalServerError, "could not load repo")
		return
	}

	if h.assemble == nil {
		h.renderError(w, http.StatusServiceUnavailable, "agent context assembler not configured")
		return
	}
	actCtx, err := h.assemble(r.Context(), repoName, branch)
	if err != nil {
		// Matches the sentinel message from server.ErrAgentContextBranchNotFound.
		// We avoid importing internal/server to stay free of circular deps.
		if strings.Contains(err.Error(), "branch not found") {
			h.renderError(w, http.StatusNotFound, "branch not found: "+branch)
			return
		}
		slog.Error("ui assemble agent context", "repo", repoName, "branch", branch, "error", err)
		h.renderError(w, http.StatusInternalServerError, "could not load branch")
		return
	}

	page := branchDetailPage{Repo: *repo, Ctx: actCtx}
	for _, p := range actCtx.Policies {
		if !p.Pass {
			reason := p.Reason
			if reason == "" {
				reason = p.Name
			}
			page.Blockers = append(page.Blockers, reason)
		}
	}
	for _, c := range actCtx.CheckRuns {
		switch c.Status {
		case model.CheckRunPassed:
			page.PassedCheckCnt++
		case model.CheckRunPending:
			page.PendingCheckCnt++
			page.Blockers = append(page.Blockers, c.CheckName+" pending")
		case model.CheckRunFailed:
			page.FailedCheckCnt++
			page.Blockers = append(page.Blockers, c.CheckName+" failed")
		}
	}

	h.render(w, h.tmpl.branchDetail, "layout.html", pageData{
		Title: repoName + " / " + branch,
		Breadcrumbs: []crumb{
			{Label: "repos", Href: "/ui/"},
			{Label: repoName, Href: "/ui/r/" + repoName},
			{Label: branch, Href: ""},
		},
		Body: page,
	})
}

// handleChecksPartial returns just the checks table for HTMX polling.
func (h *Handler) handleChecksPartial(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	branch := r.PathValue("branch")
	repoName := owner + "/" + name

	if h.assemble == nil {
		http.Error(w, "assembler not configured", http.StatusServiceUnavailable)
		return
	}
	actCtx, err := h.assemble(r.Context(), repoName, branch)
	if err != nil {
		slog.Error("ui assemble checks partial", "repo", repoName, "branch", branch, "error", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	h.render(w, h.tmpl.branchChecks, "branch_checks.html", actCtx)
}

// handleFile renders a file viewer with a sibling tree pane.
func (h *Handler) handleFile(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	path := r.PathValue("path")
	repoName := owner + "/" + name

	branch := r.URL.Query().Get("branch")
	if branch == "" {
		branch = "main"
	}
	var atSeq *int64
	if v := r.URL.Query().Get("at"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			h.renderError(w, http.StatusBadRequest, "invalid 'at' parameter")
			return
		}
		atSeq = &n
	}

	repo, err := h.write.GetRepo(r.Context(), repoName)
	if err != nil {
		if errors.Is(err, db.ErrRepoNotFound) {
			h.renderError(w, http.StatusNotFound, "repo not found: "+repoName)
			return
		}
		slog.Error("ui get repo", "repo", repoName, "error", err)
		h.renderError(w, http.StatusInternalServerError, "could not load repo")
		return
	}

	parentDir := ""
	if i := strings.LastIndex(path, "/"); i >= 0 {
		parentDir = path[:i]
	}

	entries, err := h.read.MaterializeTree(r.Context(), repoName, branch, atSeq, 500, "")
	if err != nil {
		slog.Error("ui materialize tree", "repo", repoName, "branch", branch, "error", err)
		h.renderError(w, http.StatusInternalServerError, "could not load tree")
		return
	}
	tree := siblingTreeRows(entries, parentDir)

	page := filePage{
		Repo:      *repo,
		Branch:    branch,
		AtSeq:     atSeq,
		Path:      path,
		Tree:      tree,
		ParentDir: parentDir,
	}

	if path != "" {
		fc, err := h.read.GetFile(r.Context(), repoName, branch, path, atSeq)
		if err != nil {
			slog.Error("ui get file", "repo", repoName, "path", path, "error", err)
			h.renderError(w, http.StatusInternalServerError, "could not load file")
			return
		}
		if fc != nil {
			page.File = &fileView{
				Path:        fc.Path,
				VersionID:   fc.VersionID,
				ContentHash: fc.ContentHash,
				Content:     fc.Content,
				ContentType: fc.ContentType,
			}
		}
	}

	h.render(w, h.tmpl.fileView, "layout.html", pageData{
		Title: repoName + " / " + path,
		Breadcrumbs: []crumb{
			{Label: "repos", Href: "/ui/"},
			{Label: repoName, Href: "/ui/r/" + repoName},
			{Label: branch + ":" + path, Href: ""},
		},
		Body: page,
	})
}

const commitLogPageSize = 50

type commitLogPage struct {
	Repo   model.Repo
	Branch string
	Rows   []commitLogRow
}

type commitLogRow struct {
	Seq     int64
	Message string
	Author  string
	Time    string
	// NextAfter is the seq to use in the HTMX load-more URL; non-zero only on
	// the last row when there may be more commits.
	NextAfter int64
}

type commitDetailPage struct {
	Repo   model.Repo
	Branch string
	Seq    int64
	Detail *store.CommitDetail
}

// handleCommitLog renders the paginated commit log for a branch.
func (h *Handler) handleCommitLog(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	branch := r.PathValue("branch")
	if branch == "" {
		branch = "main"
	}
	repoName := owner + "/" + name
	ctx := r.Context()

	repo, err := h.write.GetRepo(ctx, repoName)
	if err != nil {
		if errors.Is(err, db.ErrRepoNotFound) {
			h.renderError(w, http.StatusNotFound, "repo not found: "+repoName)
			return
		}
		slog.Error("ui get repo", "repo", repoName, "error", err)
		h.renderError(w, http.StatusInternalServerError, "could not load repo")
		return
	}

	bi, err := h.read.GetBranch(ctx, repoName, branch)
	if err != nil {
		slog.Error("ui get branch", "repo", repoName, "branch", branch, "error", err)
		h.renderError(w, http.StatusInternalServerError, "could not load branch")
		return
	}
	if bi == nil {
		h.renderError(w, http.StatusNotFound, "branch not found: "+branch)
		return
	}

	rows, nextAfter := h.loadCommitRows(ctx, repoName, bi.HeadSequence, 0)

	page := commitLogPage{
		Repo:   *repo,
		Branch: branch,
		Rows:   rows,
	}
	if len(rows) > 0 && nextAfter > 0 {
		page.Rows[len(page.Rows)-1].NextAfter = nextAfter
	}

	h.render(w, h.tmpl.commitLog, "layout.html", pageData{
		Title: repoName + " / " + branch + " / log",
		Breadcrumbs: []crumb{
			{Label: "repos", Href: "/ui/"},
			{Label: repoName, Href: "/ui/r/" + repoName},
			{Label: branch, Href: "/ui/r/" + repoName + "/b/" + branch},
			{Label: "log", Href: ""},
		},
		Body: page,
	})
}

// handleCommitLogPartial returns table rows for HTMX "load more" pagination.
func (h *Handler) handleCommitLogPartial(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	branch := r.PathValue("branch")
	if branch == "" {
		branch = "main"
	}
	repoName := owner + "/" + name

	afterStr := r.URL.Query().Get("after")
	if afterStr == "" {
		http.Error(w, "after param required", http.StatusBadRequest)
		return
	}
	after, err := strconv.ParseInt(afterStr, 10, 64)
	if err != nil || after <= 0 {
		http.Error(w, "invalid after param", http.StatusBadRequest)
		return
	}

	rows, nextAfter := h.loadCommitRows(r.Context(), repoName, after-1, 0)
	if len(rows) > 0 && nextAfter > 0 {
		rows[len(rows)-1].NextAfter = nextAfter
	}

	h.render(w, h.tmpl.commitLogRows, "log_rows.html", commitLogRowsPartial{
		Repo:   repoName,
		Branch: branch,
		Rows:   rows,
	})
}

// commitLogRowsPartial is the data type for the HTMX log rows fragment.
type commitLogRowsPartial struct {
	Repo   string
	Branch string
	Rows   []commitLogRow
}

// loadCommitRows loads up to commitLogPageSize rows ending at toSeq (inclusive).
// It returns rows in newest-first order and a nextAfter value (the seq to pass
// as ?after= for the next page), or 0 if there are no more rows.
func (h *Handler) loadCommitRows(ctx context.Context, repo string, toSeq, _ int64) ([]commitLogRow, int64) {
	if toSeq <= 0 {
		return nil, 0
	}
	from := toSeq - commitLogPageSize + 1
	if from < 1 {
		from = 1
	}
	entries, err := h.read.GetChain(ctx, repo, from, toSeq)
	if err != nil {
		slog.Error("ui get chain", "repo", repo, "error", err)
		return nil, 0
	}

	// Reverse to show newest first.
	rows := make([]commitLogRow, len(entries))
	for i, e := range entries {
		rows[len(entries)-1-i] = commitLogRow{
			Seq:     e.Sequence,
			Message: e.Message,
			Author:  e.Author,
			Time:    relTime(e.CreatedAt),
		}
	}

	var nextAfter int64
	if from > 1 {
		nextAfter = from // caller should pass from as ?after= to get the previous page
	}
	return rows, nextAfter
}

// handleCommitDetail renders the detail view for a single commit.
func (h *Handler) handleCommitDetail(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	branch := r.PathValue("branch")
	seqStr := r.PathValue("seq")
	repoName := owner + "/" + name
	ctx := r.Context()

	seq, err := strconv.ParseInt(seqStr, 10, 64)
	if err != nil || seq <= 0 {
		h.renderError(w, http.StatusBadRequest, "invalid sequence number")
		return
	}

	repo, err := h.write.GetRepo(ctx, repoName)
	if err != nil {
		if errors.Is(err, db.ErrRepoNotFound) {
			h.renderError(w, http.StatusNotFound, "repo not found: "+repoName)
			return
		}
		slog.Error("ui get repo", "repo", repoName, "error", err)
		h.renderError(w, http.StatusInternalServerError, "could not load repo")
		return
	}

	detail, err := h.read.GetCommit(ctx, repoName, seq)
	if err != nil {
		slog.Error("ui get commit", "repo", repoName, "seq", seq, "error", err)
		h.renderError(w, http.StatusInternalServerError, "could not load commit")
		return
	}
	if detail == nil {
		h.renderError(w, http.StatusNotFound, "commit not found")
		return
	}

	page := commitDetailPage{
		Repo:   *repo,
		Branch: branch,
		Seq:    seq,
		Detail: detail,
	}

	h.render(w, h.tmpl.commitDetail, "layout.html", pageData{
		Title: repoName + " / commit " + seqStr,
		Breadcrumbs: []crumb{
			{Label: "repos", Href: "/ui/"},
			{Label: repoName, Href: "/ui/r/" + repoName},
			{Label: branch, Href: "/ui/r/" + repoName + "/b/" + branch},
			{Label: "log", Href: "/ui/r/" + repoName + "/b/" + branch + "/log"},
			{Label: "#" + seqStr, Href: ""},
		},
		Body: page,
	})
}

// siblingTreeRows returns the immediate children of dir within entries, with
// synthesized directory rows for paths that have a deeper subtree.
func siblingTreeRows(entries []store.TreeEntry, dir string) []treeRow {
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	seen := map[string]bool{}
	var rows []treeRow
	for _, e := range entries {
		rest, ok := strings.CutPrefix(e.Path, prefix)
		if !ok || rest == "" {
			continue
		}
		dirName, _, hasSlash := strings.Cut(rest, "/")
		if !hasSlash {
			if seen[rest] {
				continue
			}
			seen[rest] = true
			rows = append(rows, treeRow{Name: rest, Path: e.Path, IsDir: false})
			continue
		}
		if seen[dirName] {
			continue
		}
		seen[dirName] = true
		rows = append(rows, treeRow{Name: dirName, Path: prefix + dirName, IsDir: true})
	}
	slices.SortFunc(rows, func(a, b treeRow) int {
		if a.IsDir != b.IsDir {
			if a.IsDir {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return rows
}
