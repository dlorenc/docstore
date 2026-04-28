package ui

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/store"
)

// ---------------------------------------------------------------------------
// Page data types
// ---------------------------------------------------------------------------

const logPageSize = 25

type reposPage struct {
	Orgs []orgGroup
}

type orgGroup struct {
	Name  string
	Repos []model.Repo
}

type orgPage struct {
	Org     string
	Repos   []model.Repo
	Members []model.OrgMember
	Invites []model.OrgInvite
}

type branchesPage struct {
	Repo      model.Repo
	Active    []branchRow
	Merged    []branchRow
	Abandoned []branchRow
	Members   []model.OrgMember
	Roles     []model.Role
}

type branchRow struct {
	Name         string
	HeadSequence int64
	BaseSequence int64
	Draft        bool
	AutoMerge    bool
	Status       string
}

type reviewCommentGroup struct {
	Path     string
	Comments []model.ReviewComment
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
	Repo        model.Repo
	Branch      string
	AtSeq       *int64
	Path        string
	File        *fileView
	Tree        []treeRow
	ParentDir   string
	FileHistory []store.FileHistoryEntry
	Branches    []string
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

type commitLogRow struct {
	Seq     int64
	Branch  string
	Author  string
	Message string
	Time    time.Time
}

type logPage struct {
	Repo      model.Repo
	Branch    string
	Rows      []commitLogRow
	HasMore   bool
	NextAfter int64
}

type logRowsData struct {
	Repo      model.Repo
	Branch    string
	Rows      []commitLogRow
	HasMore   bool
	NextAfter int64
}

type commitDetailPage struct {
	Repo   model.Repo
	Branch string
	Commit *store.CommitDetail
}

type issuesPage struct {
	Repo   model.Repo
	Issues []model.Issue
	State  string
}

type issueDetailPage struct {
	Repo     model.Repo
	Issue    *model.Issue
	Comments []model.IssueComment
	Refs     []model.IssueRef
}

type acceptInvitePage struct {
	Org   string
	Token string
	Role  model.OrgRole
	Err   string
}

type proposalsPage struct {
	Repo      model.Repo
	Proposals []*model.Proposal
	State     string
}

type proposalDetailPage struct {
	Repo     model.Repo
	Proposal *model.Proposal
}

type releasesPage struct {
	Repo     model.Repo
	Releases []model.Release
}

type releaseDetailPage struct {
	Repo    model.Repo
	Release *model.Release
}

type ciJobsPage struct {
	Repo   model.Repo
	Jobs   []model.CIJob
	Status string
}

type ciJobDetailPage struct {
	Repo model.Repo
	Job  *model.CIJob
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

// handleOrg renders the org overview page: repos, members, and pending invites.
func (h *Handler) handleOrg(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	ctx := r.Context()

	repos, err := h.write.ListOrgRepos(ctx, org)
	if err != nil {
		slog.Error("ui list org repos", "org", org, "error", err)
		h.renderError(w, http.StatusInternalServerError, "could not load repos")
		return
	}
	members, err := h.write.ListOrgMembers(ctx, org)
	if err != nil {
		slog.Error("ui list org members", "org", org, "error", err)
		h.renderError(w, http.StatusInternalServerError, "could not load members")
		return
	}
	invites, err := h.write.ListInvites(ctx, org)
	if err != nil {
		slog.Error("ui list invites", "org", org, "error", err)
		h.renderError(w, http.StatusInternalServerError, "could not load invites")
		return
	}
	var pending []model.OrgInvite
	for _, inv := range invites {
		if inv.AcceptedAt == nil {
			pending = append(pending, inv)
		}
	}

	h.render(w, h.tmpl.org, "layout.html", pageData{
		Title: org,
		Breadcrumbs: []crumb{
			{Label: "repos", Href: "/ui/"},
			{Label: org, Href: ""},
		},
		Body: orgPage{
			Org:     org,
			Repos:   repos,
			Members: members,
			Invites: pending,
		},
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

	members, err := h.write.ListOrgMembers(ctx, repo.Owner)
	if err != nil {
		slog.Error("ui list org members", "org", repo.Owner, "error", err)
		// Non-fatal: render page without members.
		members = nil
	}
	roles, err := h.write.ListRoles(ctx, repoName)
	if err != nil {
		slog.Error("ui list roles", "repo", repoName, "error", err)
		// Non-fatal: render page without roles.
		roles = nil
	}

	page := branchesPage{Repo: *repo, Members: members, Roles: roles}
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

// handleCheckHistoryPartial returns the attempt history for a single named
// check as an HTML fragment (no layout wrapper) for HTMX inline expansion.
func (h *Handler) handleCheckHistoryPartial(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	branch := r.PathValue("branch")
	checkName := r.URL.Query().Get("check")
	repoName := owner + "/" + name

	if checkName == "" {
		http.Error(w, "check query parameter required", http.StatusBadRequest)
		return
	}

	runs, err := h.write.ListCheckRuns(r.Context(), repoName, branch, nil, true)
	if err != nil {
		slog.Error("ui list check runs history", "repo", repoName, "branch", branch, "error", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}

	var filtered []model.CheckRun
	for _, run := range runs {
		if run.CheckName == checkName {
			filtered = append(filtered, run)
		}
	}

	h.render(w, h.tmpl.checkHistory, "check_history.html", filtered)
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

	branchList, err := h.read.ListBranches(r.Context(), repoName, "", true, false)
	if err != nil {
		slog.Error("ui list branches for file page", "repo", repoName, "error", err)
		h.renderError(w, http.StatusInternalServerError, "could not load branches")
		return
	}
	branchNames := make([]string, 0, len(branchList))
	for _, b := range branchList {
		branchNames = append(branchNames, b.Name)
	}

	page := filePage{
		Repo:      *repo,
		Branch:    branch,
		AtSeq:     atSeq,
		Path:      path,
		Tree:      tree,
		ParentDir: parentDir,
		Branches:  branchNames,
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

		hist, err := h.read.GetFileHistory(r.Context(), repoName, branch, path, 20, nil)
		if err != nil {
			slog.Error("ui get file history", "repo", repoName, "path", path, "error", err)
			// Non-fatal: render without history.
		} else {
			page.FileHistory = hist
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

// handleIssues renders the issues list page for a repo with open/closed tab state.
func (h *Handler) handleIssues(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	repoName := owner + "/" + name
	ctx := r.Context()

	state := r.URL.Query().Get("state")
	if state != "open" && state != "closed" {
		state = "open"
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

	issues, err := h.write.ListIssues(ctx, repoName, state, "")
	if err != nil {
		slog.Error("ui list issues", "repo", repoName, "error", err)
		h.renderError(w, http.StatusInternalServerError, "could not load issues")
		return
	}

	page := issuesPage{Repo: *repo, Issues: issues, State: state}
	h.render(w, h.tmpl.issues, "layout.html", pageData{
		Title: repoName + " / issues",
		Breadcrumbs: []crumb{
			{Label: "repos", Href: "/ui/"},
			{Label: repoName, Href: "/ui/r/" + repoName},
			{Label: "issues", Href: ""},
		},
		Body: page,
	})
}

// handleIssuesPartial returns just the issues table rows for tab-filter swapping.
func (h *Handler) handleIssuesPartial(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	repoName := owner + "/" + name
	ctx := r.Context()

	state := r.URL.Query().Get("state")
	if state != "open" && state != "closed" {
		state = "open"
	}

	repo, err := h.write.GetRepo(ctx, repoName)
	if err != nil {
		slog.Error("ui issues partial get repo", "repo", repoName, "error", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}

	issues, err := h.write.ListIssues(ctx, repoName, state, "")
	if err != nil {
		slog.Error("ui issues partial list", "repo", repoName, "error", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}

	h.render(w, h.tmpl.issuesRows, "issues_rows.html", issuesPage{Repo: *repo, Issues: issues, State: state})
}

// handleReviewCommentsPartial returns the inline review comments for a branch,
// grouped by file path, as an HTMX partial.
func (h *Handler) handleReviewCommentsPartial(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	branch := r.PathValue("branch")
	repoName := owner + "/" + name

	comments, err := h.write.ListReviewComments(r.Context(), repoName, branch, nil)
	if err != nil {
		slog.Error("ui list review comments", "repo", repoName, "branch", branch, "error", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}

	// Group by file path.
	order := []string{}
	byPath := map[string][]model.ReviewComment{}
	for _, c := range comments {
		if _, seen := byPath[c.Path]; !seen {
			order = append(order, c.Path)
		}
		byPath[c.Path] = append(byPath[c.Path], c)
	}
	groups := make([]reviewCommentGroup, 0, len(order))
	for _, p := range order {
		groups = append(groups, reviewCommentGroup{Path: p, Comments: byPath[p]})
	}

	h.render(w, h.tmpl.reviewComments, "review_comments.html", groups)
}

// handleIssueDetail renders the detail view for a single issue.
func (h *Handler) handleIssueDetail(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	numberStr := r.PathValue("number")
	repoName := owner + "/" + name
	ctx := r.Context()

	number, err := strconv.ParseInt(numberStr, 10, 64)
	if err != nil {
		h.renderError(w, http.StatusBadRequest, "invalid issue number")
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

	issue, err := h.write.GetIssue(ctx, repoName, number)
	if err != nil {
		if errors.Is(err, db.ErrIssueNotFound) {
			h.renderError(w, http.StatusNotFound, "issue not found")
			return
		}
		slog.Error("ui get issue", "repo", repoName, "number", number, "error", err)
		h.renderError(w, http.StatusInternalServerError, "could not load issue")
		return
	}

	comments, err := h.write.ListIssueComments(ctx, repoName, number)
	if err != nil {
		slog.Error("ui list issue comments", "repo", repoName, "number", number, "error", err)
		h.renderError(w, http.StatusInternalServerError, "could not load comments")
		return
	}

	refs, err := h.write.ListIssueRefs(ctx, repoName, number)
	if err != nil {
		slog.Error("ui list issue refs", "repo", repoName, "number", number, "error", err)
		// Non-fatal: render page without refs.
		refs = nil
	}

	page := issueDetailPage{Repo: *repo, Issue: issue, Comments: comments, Refs: refs}
	h.render(w, h.tmpl.issueDetail, "layout.html", pageData{
		Title: repoName + " / issue #" + numberStr,
		Breadcrumbs: []crumb{
			{Label: "repos", Href: "/ui/"},
			{Label: repoName, Href: "/ui/r/" + repoName},
			{Label: "issues", Href: "/ui/r/" + repoName + "/issues"},
			{Label: "#" + numberStr, Href: ""},
		},
		Body: page,
	})
}

// proposalStateFromQuery parses the "state" query param into a ProposalState
// pointer. Unknown or empty values default to open.
func proposalStateFromQuery(s string) (model.ProposalState, *model.ProposalState) {
	switch s {
	case "closed":
		st := model.ProposalClosed
		return st, &st
	case "merged":
		st := model.ProposalMerged
		return st, &st
	default:
		st := model.ProposalOpen
		return st, &st
	}
}

// handleProposals renders the proposals list for a repo with state filter tabs.
func (h *Handler) handleProposals(w http.ResponseWriter, r *http.Request) {
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

	stateStr := r.URL.Query().Get("state")
	if stateStr == "" {
		stateStr = "open"
	}
	_, ps := proposalStateFromQuery(stateStr)

	proposals, err := h.write.ListProposals(ctx, repoName, ps, nil)
	if err != nil {
		slog.Error("ui list proposals", "repo", repoName, "error", err)
		h.renderError(w, http.StatusInternalServerError, "could not load proposals")
		return
	}

	h.render(w, h.tmpl.proposals, "layout.html", pageData{
		Title: repoName + " / proposals",
		Breadcrumbs: []crumb{
			{Label: "repos", Href: "/ui/"},
			{Label: repoName, Href: "/ui/r/" + repoName},
			{Label: "proposals", Href: ""},
		},
		Body: proposalsPage{Repo: *repo, Proposals: proposals, State: stateStr},
	})
}

// handleProposalsPartial returns just the proposals rows for HTMX tab swapping.
func (h *Handler) handleProposalsPartial(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	repoName := owner + "/" + name
	ctx := r.Context()

	stateStr := r.URL.Query().Get("state")
	if stateStr == "" {
		stateStr = "open"
	}
	_, ps := proposalStateFromQuery(stateStr)

	proposals, err := h.write.ListProposals(ctx, repoName, ps, nil)
	if err != nil {
		slog.Error("ui list proposals partial", "repo", repoName, "error", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	h.render(w, h.tmpl.proposalsRows, "proposals_rows.html", proposals)
}

// handleProposalDetail renders the detail view for a single proposal.
func (h *Handler) handleProposalDetail(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	id := r.PathValue("id")
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

	proposal, err := h.write.GetProposal(ctx, repoName, id)
	if err != nil {
		slog.Error("ui get proposal", "repo", repoName, "id", id, "error", err)
		h.renderError(w, http.StatusInternalServerError, "could not load proposal")
		return
	}
	if proposal == nil {
		h.renderError(w, http.StatusNotFound, "proposal not found")
		return
	}

	h.render(w, h.tmpl.proposalDetail, "layout.html", pageData{
		Title: repoName + " / proposals / " + id,
		Breadcrumbs: []crumb{
			{Label: "repos", Href: "/ui/"},
			{Label: repoName, Href: "/ui/r/" + repoName},
			{Label: "proposals", Href: "/ui/r/" + repoName + "/proposals"},
			{Label: proposal.Title, Href: ""},
		},
		Body: proposalDetailPage{Repo: *repo, Proposal: proposal},
	})
}

// handleReleases renders the releases list for a repo.
func (h *Handler) handleReleases(w http.ResponseWriter, r *http.Request) {
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

	releases, err := h.write.ListReleases(ctx, repoName, 100, "")
	if err != nil {
		slog.Error("ui list releases", "repo", repoName, "error", err)
		h.renderError(w, http.StatusInternalServerError, "could not load releases")
		return
	}

	h.render(w, h.tmpl.releases, "layout.html", pageData{
		Title: repoName + " / releases",
		Breadcrumbs: []crumb{
			{Label: "repos", Href: "/ui/"},
			{Label: repoName, Href: "/ui/r/" + repoName},
			{Label: "releases", Href: ""},
		},
		Body: releasesPage{Repo: *repo, Releases: releases},
	})
}

// handleReleaseDetail renders the detail view for a single release.
func (h *Handler) handleReleaseDetail(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	rname := r.PathValue("rname")
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

	release, err := h.write.GetRelease(ctx, repoName, rname)
	if err != nil {
		slog.Error("ui get release", "repo", repoName, "name", rname, "error", err)
		h.renderError(w, http.StatusInternalServerError, "could not load release")
		return
	}
	if release == nil {
		h.renderError(w, http.StatusNotFound, "release not found")
		return
	}

	h.render(w, h.tmpl.releaseDetail, "layout.html", pageData{
		Title: repoName + " / releases / " + rname,
		Breadcrumbs: []crumb{
			{Label: "repos", Href: "/ui/"},
			{Label: repoName, Href: "/ui/r/" + repoName},
			{Label: "releases", Href: "/ui/r/" + repoName + "/releases"},
			{Label: rname, Href: ""},
		},
		Body: releaseDetailPage{Repo: *repo, Release: release},
	})
}

// handleCIJobs renders the CI jobs list for a repo with optional status filter.
func (h *Handler) handleCIJobs(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	repoName := owner + "/" + name
	ctx := r.Context()

	statusFilter := r.URL.Query().Get("status")

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

	var statusPtr *string
	if statusFilter != "" {
		statusPtr = &statusFilter
	}

	jobs, err := h.write.ListCIJobs(ctx, repoName, nil, statusPtr, 100)
	if err != nil {
		slog.Error("ui list ci jobs", "repo", repoName, "error", err)
		h.renderError(w, http.StatusInternalServerError, "could not load ci jobs")
		return
	}

	h.render(w, h.tmpl.ciJobs, "layout.html", pageData{
		Title: repoName + " / ci-jobs",
		Breadcrumbs: []crumb{
			{Label: "repos", Href: "/ui/"},
			{Label: repoName, Href: "/ui/r/" + repoName},
			{Label: "ci-jobs", Href: ""},
		},
		Body: ciJobsPage{Repo: *repo, Jobs: jobs, Status: statusFilter},
	})
}

// handleCIJobDetail renders the detail view for a single CI job.
func (h *Handler) handleCIJobDetail(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	id := r.PathValue("id")
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

	job, err := h.write.GetCIJob(ctx, id)
	if err != nil {
		slog.Error("ui get ci job", "id", id, "error", err)
		h.renderError(w, http.StatusInternalServerError, "could not load ci job")
		return
	}
	if job == nil {
		h.renderError(w, http.StatusNotFound, "ci job not found")
		return
	}

	h.render(w, h.tmpl.ciJobDetail, "layout.html", pageData{
		Title: repoName + " / ci-jobs / " + id,
		Breadcrumbs: []crumb{
			{Label: "repos", Href: "/ui/"},
			{Label: repoName, Href: "/ui/r/" + repoName},
			{Label: "ci-jobs", Href: "/ui/r/" + repoName + "/ci-jobs"},
			{Label: id[:8], Href: ""},
		},
		Body: ciJobDetailPage{Repo: *repo, Job: job},
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

// handleCommitLog renders the paginated commit log for a branch.
func (h *Handler) handleCommitLog(w http.ResponseWriter, r *http.Request) {
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

	bi, err := h.read.GetBranch(r.Context(), repoName, branch)
	if err != nil {
		slog.Error("ui get branch", "repo", repoName, "branch", branch, "error", err)
		h.renderError(w, http.StatusInternalServerError, "could not load branch")
		return
	}
	if bi == nil {
		h.renderError(w, http.StatusNotFound, "branch not found: "+branch)
		return
	}

	headSeq := bi.HeadSequence
	from := headSeq - int64(logPageSize) + 1
	if from < 1 {
		from = 1
	}

	entries, err := h.read.GetChain(r.Context(), repoName, from, headSeq)
	if err != nil {
		slog.Error("ui get chain", "repo", repoName, "error", err)
		h.renderError(w, http.StatusInternalServerError, "could not load commits")
		return
	}

	rows := chainToLogRows(entries, branch)
	slices.Reverse(rows)

	var nextAfter int64
	hasMore := from > 1
	if hasMore {
		nextAfter = from
	}

	h.render(w, h.tmpl.commitLog, "layout.html", pageData{
		Title: repoName + " / " + branch + " / log",
		Breadcrumbs: []crumb{
			{Label: "repos", Href: "/ui/"},
			{Label: repoName, Href: "/ui/r/" + repoName},
			{Label: branch, Href: "/ui/r/" + repoName + "/b/" + branch},
			{Label: "log", Href: ""},
		},
		Body: logPage{
			Repo:      *repo,
			Branch:    branch,
			Rows:      rows,
			HasMore:   hasMore,
			NextAfter: nextAfter,
		},
	})
}

// handleLogRowsPartial returns just the table rows for HTMX "load more".
func (h *Handler) handleLogRowsPartial(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	branch := r.PathValue("branch")
	repoName := owner + "/" + name

	afterStr := r.URL.Query().Get("after")
	if afterStr == "" {
		http.Error(w, "after parameter required", http.StatusBadRequest)
		return
	}
	after, err := strconv.ParseInt(afterStr, 10, 64)
	if err != nil || after < 1 {
		http.Error(w, "invalid after parameter", http.StatusBadRequest)
		return
	}

	to := after - 1
	if to < 1 {
		h.render(w, h.tmpl.logRows, "log_rows.html", logRowsData{})
		return
	}

	from := to - int64(logPageSize) + 1
	if from < 1 {
		from = 1
	}

	entries, err := h.read.GetChain(r.Context(), repoName, from, to)
	if err != nil {
		slog.Error("ui get chain partial", "repo", repoName, "error", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}

	rows := chainToLogRows(entries, branch)
	slices.Reverse(rows)

	var nextAfter int64
	hasMore := from > 1
	if hasMore {
		nextAfter = from
	}

	repo := &model.Repo{Name: repoName, Owner: owner}
	h.render(w, h.tmpl.logRows, "log_rows.html", logRowsData{
		Repo:      *repo,
		Branch:    branch,
		Rows:      rows,
		HasMore:   hasMore,
		NextAfter: nextAfter,
	})
}

// handleCommitDetail renders a single commit with its changed files.
func (h *Handler) handleCommitDetail(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	branch := r.PathValue("branch")
	seqStr := r.PathValue("seq")
	repoName := owner + "/" + name

	seq, err := strconv.ParseInt(seqStr, 10, 64)
	if err != nil || seq < 1 {
		h.renderError(w, http.StatusBadRequest, "invalid seq")
		return
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

	commit, err := h.read.GetCommit(r.Context(), repoName, seq)
	if err != nil {
		slog.Error("ui get commit", "repo", repoName, "seq", seq, "error", err)
		h.renderError(w, http.StatusInternalServerError, "could not load commit")
		return
	}
	if commit == nil {
		h.renderError(w, http.StatusNotFound, fmt.Sprintf("commit %d not found", seq))
		return
	}

	h.render(w, h.tmpl.commitDetail, "layout.html", pageData{
		Title: fmt.Sprintf("%s / %s / commit %d", repoName, branch, seq),
		Breadcrumbs: []crumb{
			{Label: "repos", Href: "/ui/"},
			{Label: repoName, Href: "/ui/r/" + repoName},
			{Label: branch, Href: "/ui/r/" + repoName + "/b/" + branch},
			{Label: "log", Href: "/ui/r/" + repoName + "/b/" + branch + "/log"},
			{Label: fmt.Sprintf("seq %d", seq), Href: ""},
		},
		Body: commitDetailPage{
			Repo:   *repo,
			Branch: branch,
			Commit: commit,
		},
	})
}

// handleAcceptInvite renders the invite acceptance confirmation page (GET) and
// processes the acceptance (POST).
func (h *Handler) handleAcceptInvite(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	token := r.PathValue("token")
	ctx := r.Context()

	renderPage := func(role model.OrgRole, errMsg string) {
		h.render(w, h.tmpl.acceptInvite, "layout.html", pageData{
			Title: "Accept invitation · " + org,
			Breadcrumbs: []crumb{
				{Label: "repos", Href: "/ui/"},
			},
			Body: acceptInvitePage{
				Org:   org,
				Token: token,
				Role:  role,
				Err:   errMsg,
			},
		})
	}

	// Look up invite details so we can show the role on GET and re-render on POST error.
	invite, err := h.write.GetInviteByToken(ctx, org, token)
	if err != nil {
		if errors.Is(err, db.ErrInviteNotFound) {
			h.renderError(w, http.StatusNotFound, "invite not found")
			return
		}
		slog.Error("ui get invite by token", "org", org, "error", err)
		h.renderError(w, http.StatusInternalServerError, "could not load invite")
		return
	}

	if r.Method == http.MethodGet {
		renderPage(invite.Role, "")
		return
	}

	// POST: accept the invite.
	var identity string
	if h.identity != nil {
		identity = h.identity(ctx)
	}

	if err := h.write.AcceptInvite(ctx, org, token, identity); err != nil {
		switch {
		case errors.Is(err, db.ErrInviteNotFound):
			h.renderError(w, http.StatusNotFound, "invite not found")
		case errors.Is(err, db.ErrInviteExpired):
			renderPage(invite.Role, "This invitation has expired.")
		case errors.Is(err, db.ErrInviteAlreadyAccepted):
			renderPage(invite.Role, "This invitation has already been accepted.")
		case errors.Is(err, db.ErrEmailMismatch):
			renderPage(invite.Role, "This invitation was sent to a different email address.")
		default:
			slog.Error("ui accept invite", "org", org, "error", err)
			h.renderError(w, http.StatusInternalServerError, "could not accept invite")
		}
		return
	}

	http.Redirect(w, r, "/ui/o/"+org, http.StatusSeeOther)
}

// chainToLogRows filters and converts ChainEntries to commitLogRows.
// Only entries for the named branch are included.
func chainToLogRows(entries []store.ChainEntry, branch string) []commitLogRow {
	rows := make([]commitLogRow, 0, len(entries))
	for _, e := range entries {
		if e.Branch != branch {
			continue
		}
		rows = append(rows, commitLogRow{
			Seq:     e.Sequence,
			Branch:  e.Branch,
			Author:  e.Author,
			Message: e.Message,
			Time:    e.CreatedAt,
		})
	}
	return rows
}

