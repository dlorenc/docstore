package ui

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dlorenc/docstore/internal/db"
	evtypes "github.com/dlorenc/docstore/internal/events/types"
	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/service"
	"github.com/dlorenc/docstore/internal/store"
)

// ---------------------------------------------------------------------------
// Page data types
// ---------------------------------------------------------------------------

const logPageSize = 25

type myOrgEntry struct {
	Name      string
	Role      model.OrgRole
	RepoCount int
}

type reposPage struct {
	MyOrgs         []myOrgEntry
	Orgs           []orgGroup
	ShowGetStarted bool
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
	Err     string
}

type branchesPage struct {
	Repo      model.Repo
	Active    []branchRow
	Merged    []branchRow
	Abandoned []branchRow
	Roles     []model.Role
	Err       string
}

type repoSettingsPage struct {
	Repo  model.Repo
	Roles []model.Role
	Err   string
}

type branchRow struct {
	Name         string
	HeadSequence int64
	BaseSequence int64
	Draft        bool
	AutoMerge    bool
	Status       string
	Proposal     *model.Proposal
}

type reviewCommentGroup struct {
	Path     string
	Comments []model.ReviewComment
}

type reviewCommentsData struct {
	RepoName string
	Branch   string
	Groups   []reviewCommentGroup
}

type branchDetailPage struct {
	Repo            model.Repo
	Ctx             *model.AgentContextResponse
	Blockers        []string
	PassedCheckCnt  int
	PendingCheckCnt int
	FailedCheckCnt  int
	Err             string
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
	Label  string
}

type issueDetailPage struct {
	Repo     model.Repo
	Issue    *model.Issue
	Comments []model.IssueComment
	Refs     []model.IssueRef
	Err      string
}

type newIssuePage struct {
	Repo       model.Repo
	Err        string
	FormTitle  string
	FormBody   string
	FormLabels string
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
	Err       string
}

type proposalDetailPage struct {
	Repo     model.Repo
	Proposal *model.Proposal
	Err      string
}

type releasesPage struct {
	Repo     model.Repo
	Releases []model.Release
	Err      string
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

type createOrgPage struct {
	Name string
	Err  string
}

type createRepoPage struct {
	Owner string
	Name  string
	Orgs  []model.Org
	Err   string
}

type commitFormPage struct {
	Repo            model.Repo
	Branch          string
	FormMessage     string
	FormFilePath    string
	FormFileContent string
	Err             string
}

type userProfilePage struct {
	Identity    string
	Memberships []model.OrgMember
	RepoRoles   []model.RepoRole
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
		h.renderError(w, r, http.StatusInternalServerError, "could not load repos")
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

	var myOrgs []myOrgEntry
	showGetStarted := false
	if h.identity != nil {
		identity := h.identity(ctx)
		if identity != "" {
			memberships, merr := h.write.ListOrgMemberships(ctx, identity)
			if merr != nil {
				slog.Error("ui list org memberships", "error", merr)
				// non-fatal: render page without personalised section
			} else {
				for _, m := range memberships {
					myOrgs = append(myOrgs, myOrgEntry{
						Name:      m.Org,
						Role:      m.Role,
						RepoCount: len(byOrg[m.Org]),
					})
				}
				if len(myOrgs) == 0 {
					// Only show the "Get started" card if the user also has no
					// repo roles. A user who created a repo has an admin role
					// even without any org membership, so they are not new.
					roles, rerr := h.write.ListRolesByIdentity(ctx, identity)
					if rerr != nil {
						slog.Error("ui list repo roles for get-started check", "error", rerr)
					} else {
						showGetStarted = len(roles) == 0
					}
				}
			}
		}
	}

	h.render(w, r, h.tmpl.repos, "layout.html", pageData{
		Title:       "Repos",
		Breadcrumbs: []crumb{{Label: "repos", Href: "/ui/"}},
		Body:        reposPage{MyOrgs: myOrgs, Orgs: orgs, ShowGetStarted: showGetStarted},
	})
}

// handleUserProfile renders the profile page for the given identity, showing
// org memberships and repo roles.
func (h *Handler) handleUserProfile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	identity := r.PathValue("identity")

	memberships, err := h.write.ListOrgMemberships(ctx, identity)
	if err != nil {
		slog.Error("ui list org memberships for profile", "identity", identity, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load org memberships")
		return
	}

	repoRoles, err := h.write.ListRolesByIdentity(ctx, identity)
	if err != nil {
		slog.Error("ui list repo roles for profile", "identity", identity, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load repo roles")
		return
	}

	h.render(w, r, h.tmpl.userProfile, "layout.html", pageData{
		Title:       identity,
		Breadcrumbs: []crumb{{Label: "repos", Href: "/ui/"}, {Label: identity, Href: "/ui/u/" + url.PathEscape(identity)}},
		Body: userProfilePage{
			Identity:    identity,
			Memberships: memberships,
			RepoRoles:   repoRoles,
		},
	})
}

// handleOrg renders the org overview page: repos, members, and pending invites.
func (h *Handler) handleOrg(w http.ResponseWriter, r *http.Request) {
	h.renderOrgPage(w, r, r.PathValue("org"), "")
}

// handleBranches renders the branch list for a single repo.
func (h *Handler) handleBranches(w http.ResponseWriter, r *http.Request) {
	h.renderBranchesPage(w, r, r.PathValue("owner"), r.PathValue("name"), "")
}

// handleBranchDetail renders the diff + reviews + checks + policy view.
func (h *Handler) handleBranchDetail(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	branch := r.PathValue("branch")
	h.renderBranchDetail(w, r, owner+"/"+name, branch, "")
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
	h.render(w, r, h.tmpl.branchChecks, "branch_checks.html", actCtx)
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

	h.render(w, r, h.tmpl.checkHistory, "check_history.html", filtered)
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
			h.renderError(w, r, http.StatusBadRequest, "invalid 'at' parameter")
			return
		}
		atSeq = &n
	}

	repo, err := h.write.GetRepo(r.Context(), repoName)
	if err != nil {
		if errors.Is(err, db.ErrRepoNotFound) {
			h.renderError(w, r, http.StatusNotFound, "repo not found: "+repoName)
			return
		}
		slog.Error("ui get repo", "repo", repoName, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load repo")
		return
	}

	parentDir := ""
	if i := strings.LastIndex(path, "/"); i >= 0 {
		parentDir = path[:i]
	}

	entries, err := h.read.MaterializeTree(r.Context(), repoName, branch, atSeq, 500, "")
	if err != nil {
		slog.Error("ui materialize tree", "repo", repoName, "branch", branch, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load tree")
		return
	}
	tree := siblingTreeRows(entries, parentDir)

	branchList, err := h.read.ListBranches(r.Context(), repoName, "", true, false)
	if err != nil {
		slog.Error("ui list branches for file page", "repo", repoName, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load branches")
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
			h.renderError(w, r, http.StatusInternalServerError, "could not load file")
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

	h.render(w, r, h.tmpl.fileView, "layout.html", pageData{
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
	label := r.URL.Query().Get("label")

	repo, err := h.write.GetRepo(ctx, repoName)
	if err != nil {
		if errors.Is(err, db.ErrRepoNotFound) {
			h.renderError(w, r, http.StatusNotFound, "repo not found: "+repoName)
			return
		}
		slog.Error("ui get repo", "repo", repoName, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load repo")
		return
	}

	issues, err := h.write.ListIssues(ctx, repoName, state, "", label)
	if err != nil {
		slog.Error("ui list issues", "repo", repoName, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load issues")
		return
	}

	page := issuesPage{Repo: *repo, Issues: issues, State: state, Label: label}
	h.render(w, r, h.tmpl.issues, "layout.html", pageData{
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
	label := r.URL.Query().Get("label")

	repo, err := h.write.GetRepo(ctx, repoName)
	if err != nil {
		slog.Error("ui issues partial get repo", "repo", repoName, "error", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}

	issues, err := h.write.ListIssues(ctx, repoName, state, "", label)
	if err != nil {
		slog.Error("ui issues partial list", "repo", repoName, "error", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}

	h.render(w, r, h.tmpl.issuesRows, "issues_rows.html", issuesPage{Repo: *repo, Issues: issues, State: state, Label: label})
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

	h.render(w, r, h.tmpl.reviewComments, "review_comments.html", reviewCommentsData{
		RepoName: repoName,
		Branch:   branch,
		Groups:   groups,
	})
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
		h.renderError(w, r, http.StatusBadRequest, "invalid issue number")
		return
	}

	repo, err := h.write.GetRepo(ctx, repoName)
	if err != nil {
		if errors.Is(err, db.ErrRepoNotFound) {
			h.renderError(w, r, http.StatusNotFound, "repo not found: "+repoName)
			return
		}
		slog.Error("ui get repo", "repo", repoName, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load repo")
		return
	}

	issue, err := h.write.GetIssue(ctx, repoName, number)
	if err != nil {
		if errors.Is(err, db.ErrIssueNotFound) {
			h.renderError(w, r, http.StatusNotFound, "issue not found")
			return
		}
		slog.Error("ui get issue", "repo", repoName, "number", number, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load issue")
		return
	}

	comments, err := h.write.ListIssueComments(ctx, repoName, number)
	if err != nil {
		slog.Error("ui list issue comments", "repo", repoName, "number", number, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load comments")
		return
	}

	refs, err := h.write.ListIssueRefs(ctx, repoName, number)
	if err != nil {
		slog.Error("ui list issue refs", "repo", repoName, "number", number, "error", err)
		// Non-fatal: render page without refs.
		refs = nil
	}

	page := issueDetailPage{Repo: *repo, Issue: issue, Comments: comments, Refs: refs}
	if errMsg := r.URL.Query().Get("error"); errMsg != "" {
		page.Err = errMsg
	}
	h.render(w, r, h.tmpl.issueDetail, "layout.html", pageData{
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
			h.renderError(w, r, http.StatusNotFound, "repo not found: "+repoName)
			return
		}
		slog.Error("ui get repo", "repo", repoName, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load repo")
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
		h.renderError(w, r, http.StatusInternalServerError, "could not load proposals")
		return
	}

	h.render(w, r, h.tmpl.proposals, "layout.html", pageData{
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
	h.render(w, r, h.tmpl.proposalsRows, "proposals_rows.html", proposals)
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
			h.renderError(w, r, http.StatusNotFound, "repo not found: "+repoName)
			return
		}
		slog.Error("ui get repo", "repo", repoName, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load repo")
		return
	}

	proposal, err := h.write.GetProposal(ctx, repoName, id)
	if err != nil {
		slog.Error("ui get proposal", "repo", repoName, "id", id, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load proposal")
		return
	}
	if proposal == nil {
		h.renderError(w, r, http.StatusNotFound, "proposal not found")
		return
	}

	h.render(w, r, h.tmpl.proposalDetail, "layout.html", pageData{
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
	h.renderReleasesPage(w, r, r.PathValue("owner"), r.PathValue("name"), "")
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
			h.renderError(w, r, http.StatusNotFound, "repo not found: "+repoName)
			return
		}
		slog.Error("ui get repo", "repo", repoName, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load repo")
		return
	}

	release, err := h.write.GetRelease(ctx, repoName, rname)
	if err != nil {
		slog.Error("ui get release", "repo", repoName, "name", rname, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load release")
		return
	}
	if release == nil {
		h.renderError(w, r, http.StatusNotFound, "release not found")
		return
	}

	h.render(w, r, h.tmpl.releaseDetail, "layout.html", pageData{
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
			h.renderError(w, r, http.StatusNotFound, "repo not found: "+repoName)
			return
		}
		slog.Error("ui get repo", "repo", repoName, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load repo")
		return
	}

	var statusPtr *string
	if statusFilter != "" {
		statusPtr = &statusFilter
	}

	jobs, err := h.write.ListCIJobs(ctx, repoName, nil, statusPtr, 100)
	if err != nil {
		slog.Error("ui list ci jobs", "repo", repoName, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load ci jobs")
		return
	}

	h.render(w, r, h.tmpl.ciJobs, "layout.html", pageData{
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
			h.renderError(w, r, http.StatusNotFound, "repo not found: "+repoName)
			return
		}
		slog.Error("ui get repo", "repo", repoName, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load repo")
		return
	}

	job, err := h.write.GetCIJob(ctx, id)
	if err != nil {
		slog.Error("ui get ci job", "id", id, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load ci job")
		return
	}
	if job == nil {
		h.renderError(w, r, http.StatusNotFound, "ci job not found")
		return
	}

	h.render(w, r, h.tmpl.ciJobDetail, "layout.html", pageData{
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

// handleRepoLog redirects /ui/r/{owner}/{name}/log to the main branch log.
func (h *Handler) handleRepoLog(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	http.Redirect(w, r, "/ui/r/"+owner+"/"+name+"/b/main/log", http.StatusFound)
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
			h.renderError(w, r, http.StatusNotFound, "repo not found: "+repoName)
			return
		}
		slog.Error("ui get repo", "repo", repoName, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load repo")
		return
	}

	bi, err := h.read.GetBranch(r.Context(), repoName, branch)
	if err != nil {
		slog.Error("ui get branch", "repo", repoName, "branch", branch, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load branch")
		return
	}
	if bi == nil {
		h.renderError(w, r, http.StatusNotFound, "branch not found: "+branch)
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
		h.renderError(w, r, http.StatusInternalServerError, "could not load commits")
		return
	}

	rows := chainToLogRows(entries, branch)
	slices.Reverse(rows)

	var nextAfter int64
	hasMore := from > 1
	if hasMore {
		nextAfter = from
	}

	h.render(w, r, h.tmpl.commitLog, "layout.html", pageData{
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
		h.render(w, r, h.tmpl.logRows, "log_rows.html", logRowsData{})
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
	h.render(w, r, h.tmpl.logRows, "log_rows.html", logRowsData{
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
		h.renderError(w, r, http.StatusBadRequest, "invalid seq")
		return
	}

	repo, err := h.write.GetRepo(r.Context(), repoName)
	if err != nil {
		if errors.Is(err, db.ErrRepoNotFound) {
			h.renderError(w, r, http.StatusNotFound, "repo not found: "+repoName)
			return
		}
		slog.Error("ui get repo", "repo", repoName, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load repo")
		return
	}

	commit, err := h.read.GetCommit(r.Context(), repoName, seq)
	if err != nil {
		slog.Error("ui get commit", "repo", repoName, "seq", seq, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load commit")
		return
	}
	if commit == nil {
		h.renderError(w, r, http.StatusNotFound, fmt.Sprintf("commit %d not found", seq))
		return
	}

	h.render(w, r, h.tmpl.commitDetail, "layout.html", pageData{
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
		h.render(w, r, h.tmpl.acceptInvite, "layout.html", pageData{
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
			h.renderError(w, r, http.StatusNotFound, "invite not found")
			return
		}
		slog.Error("ui get invite by token", "org", org, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load invite")
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
			h.renderError(w, r, http.StatusNotFound, "invite not found")
		case errors.Is(err, db.ErrInviteExpired):
			renderPage(invite.Role, "This invitation has expired.")
		case errors.Is(err, db.ErrInviteAlreadyAccepted):
			renderPage(invite.Role, "This invitation has already been accepted.")
		case errors.Is(err, db.ErrEmailMismatch):
			renderPage(invite.Role, "This invitation was sent to a different email address.")
		default:
			slog.Error("ui accept invite", "org", org, "error", err)
			h.renderError(w, r, http.StatusInternalServerError, "could not accept invite")
		}
		return
	}

	http.Redirect(w, r, "/ui/o/"+org, http.StatusSeeOther)
}

// generateInviteToken returns a 32-byte random hex string for use as an invite token.
func generateInviteToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// renderOrgPage loads org data and renders the org page, optionally with an error message.
func (h *Handler) renderOrgPage(w http.ResponseWriter, r *http.Request, org, errMsg string) {
	ctx := r.Context()

	repos, err := h.write.ListOrgRepos(ctx, org)
	if err != nil {
		slog.Error("ui list org repos", "org", org, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load repos")
		return
	}
	members, err := h.write.ListOrgMembers(ctx, org)
	if err != nil {
		slog.Error("ui list org members", "org", org, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load members")
		return
	}
	invites, err := h.write.ListInvites(ctx, org)
	if err != nil {
		slog.Error("ui list invites", "org", org, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load invites")
		return
	}
	var pending []model.OrgInvite
	for _, inv := range invites {
		if inv.AcceptedAt == nil {
			pending = append(pending, inv)
		}
	}

	h.render(w, r, h.tmpl.org, "layout.html", pageData{
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
			Err:     errMsg,
		},
	})
}

// handleAddOrgMember processes POST /ui/o/{org}/members to add an org member.
func (h *Handler) handleAddOrgMember(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	ctx := r.Context()

	if err := r.ParseForm(); err != nil {
		h.renderOrgPage(w, r, org, "invalid form data")
		return
	}
	identity := strings.TrimSpace(r.FormValue("identity"))
	role := model.OrgRole(r.FormValue("role"))

	if identity == "" {
		h.renderOrgPage(w, r, org, "identity is required")
		return
	}
	switch role {
	case model.OrgRoleOwner, model.OrgRoleMember:
	default:
		h.renderOrgPage(w, r, org, "invalid role; must be 'owner' or 'member'")
		return
	}

	var invitedBy string
	if h.identity != nil {
		invitedBy = h.identity(ctx)
	}

	if err := h.write.AddOrgMember(ctx, org, identity, role, invitedBy); err != nil {
		slog.Error("ui add org member", "org", org, "identity", identity, "error", err)
		h.renderOrgPage(w, r, org, "could not add member: "+err.Error())
		return
	}

	h.emit(ctx, evtypes.OrgMemberAdded{
		Org:      org,
		Identity: identity,
		Role:     string(role),
		AddedBy:  invitedBy,
	})
	http.Redirect(w, r, "/ui/o/"+org, http.StatusSeeOther)
}

// handleRemoveOrgMember processes POST /ui/o/{org}/members/{identity}/remove.
func (h *Handler) handleRemoveOrgMember(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	identity := r.PathValue("identity")
	ctx := r.Context()

	if err := h.write.RemoveOrgMember(ctx, org, identity); err != nil {
		switch {
		case errors.Is(err, db.ErrOrgMemberNotFound):
			h.renderOrgPage(w, r, org, "member not found")
		default:
			slog.Error("ui remove org member", "org", org, "identity", identity, "error", err)
			h.renderOrgPage(w, r, org, "could not remove member")
		}
		return
	}

	var removedBy string
	if h.identity != nil {
		removedBy = h.identity(ctx)
	}
	h.emit(ctx, evtypes.OrgMemberRemoved{
		Org:       org,
		Identity:  identity,
		RemovedBy: removedBy,
	})
	http.Redirect(w, r, "/ui/o/"+org, http.StatusSeeOther)
}

// handleCreateInvite processes POST /ui/o/{org}/invites to create an org invite.
func (h *Handler) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	ctx := r.Context()

	if err := r.ParseForm(); err != nil {
		h.renderOrgPage(w, r, org, "invalid form data")
		return
	}
	email := strings.TrimSpace(r.FormValue("email"))
	role := model.OrgRole(r.FormValue("role"))

	if email == "" {
		h.renderOrgPage(w, r, org, "email is required")
		return
	}
	switch role {
	case model.OrgRoleOwner, model.OrgRoleMember:
	default:
		h.renderOrgPage(w, r, org, "invalid role; must be 'owner' or 'member'")
		return
	}

	token, err := generateInviteToken()
	if err != nil {
		slog.Error("ui generate invite token", "org", org, "error", err)
		h.renderOrgPage(w, r, org, "could not generate invite token")
		return
	}

	var invitedBy string
	if h.identity != nil {
		invitedBy = h.identity(ctx)
	}
	expiresAt := time.Now().Add(7 * 24 * time.Hour)

	if _, err := h.write.CreateInvite(ctx, org, email, role, invitedBy, token, expiresAt); err != nil {
		slog.Error("ui create invite", "org", org, "email", email, "error", err)
		h.renderOrgPage(w, r, org, "could not create invite: "+err.Error())
		return
	}

	http.Redirect(w, r, "/ui/o/"+org, http.StatusSeeOther)
}

// handleRevokeInvite processes POST /ui/o/{org}/invites/{id}/revoke.
func (h *Handler) handleRevokeInvite(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	inviteID := r.PathValue("id")
	ctx := r.Context()

	if err := h.write.RevokeInvite(ctx, org, inviteID); err != nil {
		switch {
		case errors.Is(err, db.ErrInviteNotFound):
			h.renderOrgPage(w, r, org, "invite not found")
		default:
			slog.Error("ui revoke invite", "org", org, "invite_id", inviteID, "error", err)
			h.renderOrgPage(w, r, org, "could not revoke invite")
		}
		return
	}

	http.Redirect(w, r, "/ui/o/"+org, http.StatusSeeOther)
}

// renderBranchesPage loads repo/branch/member/role data and renders the branches page with an optional error.
func (h *Handler) renderBranchesPage(w http.ResponseWriter, r *http.Request, owner, name, errMsg string) {
	repoName := owner + "/" + name
	ctx := r.Context()

	repo, err := h.write.GetRepo(ctx, repoName)
	if err != nil {
		if errors.Is(err, db.ErrRepoNotFound) {
			h.renderError(w, r, http.StatusNotFound, "repo not found: "+repoName)
			return
		}
		slog.Error("ui get repo", "repo", repoName, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load repo")
		return
	}

	branches, err := h.read.ListBranches(ctx, repoName, "", true, false)
	if err != nil {
		slog.Error("ui list branches", "repo", repoName, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load branches")
		return
	}

	roles, err := h.write.ListRoles(ctx, repoName)
	if err != nil {
		slog.Error("ui list roles", "repo", repoName, "error", err)
		roles = nil
	}

	openState := model.ProposalOpen
	proposalList, err := h.write.ListProposals(ctx, repoName, &openState, nil)
	if err != nil {
		slog.Error("ui list proposals", "repo", repoName, "error", err)
	}
	proposalByBranch := make(map[string]*model.Proposal)
	for _, p := range proposalList {
		if _, exists := proposalByBranch[p.Branch]; !exists {
			proposalByBranch[p.Branch] = p
		}
	}

	page := branchesPage{Repo: *repo, Roles: roles, Err: errMsg}
	for _, b := range branches {
		row := branchRow{
			Name:         b.Name,
			HeadSequence: b.HeadSequence,
			BaseSequence: b.BaseSequence,
			Draft:        b.Draft,
			AutoMerge:    b.AutoMerge,
			Status:       b.Status,
			Proposal:     proposalByBranch[b.Name],
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

	h.render(w, r, h.tmpl.branches, "layout.html", pageData{
		Title: repoName + " / branches",
		Breadcrumbs: []crumb{
			{Label: "repos", Href: "/ui/"},
			{Label: repoName, Href: "/ui/r/" + repoName},
		},
		Body: page,
	})
}

// handleRepoSettings renders the settings page for a single repo.
func (h *Handler) handleRepoSettings(w http.ResponseWriter, r *http.Request) {
	h.renderRepoSettingsPage(w, r, r.PathValue("owner"), r.PathValue("name"), "")
}

// renderRepoSettingsPage loads repo/role data and renders the settings page with an optional error.
func (h *Handler) renderRepoSettingsPage(w http.ResponseWriter, r *http.Request, owner, name, errMsg string) {
	repoName := owner + "/" + name
	ctx := r.Context()

	repo, err := h.write.GetRepo(ctx, repoName)
	if err != nil {
		if errors.Is(err, db.ErrRepoNotFound) {
			h.renderError(w, r, http.StatusNotFound, "repo not found: "+repoName)
			return
		}
		slog.Error("ui get repo", "repo", repoName, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load repo")
		return
	}

	roles, err := h.write.ListRoles(ctx, repoName)
	if err != nil {
		slog.Error("ui list roles", "repo", repoName, "error", err)
		roles = nil
	}

	h.render(w, r, h.tmpl.repoSettings, "layout.html", pageData{
		Title: repoName + " / settings",
		Breadcrumbs: []crumb{
			{Label: "repos", Href: "/ui/"},
			{Label: repoName, Href: "/ui/r/" + repoName},
			{Label: "settings", Href: ""},
		},
		Body: repoSettingsPage{Repo: *repo, Roles: roles, Err: errMsg},
	})
}

// handleSetRole processes POST /ui/r/{owner}/{name}/roles to grant or update a repo role.
func (h *Handler) handleSetRole(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	repoName := owner + "/" + name
	ctx := r.Context()

	if err := r.ParseForm(); err != nil {
		h.renderRepoSettingsPage(w, r, owner, name, "invalid form data")
		return
	}
	identity := strings.TrimSpace(r.FormValue("identity"))
	role := model.RoleType(r.FormValue("role"))

	if identity == "" {
		h.renderRepoSettingsPage(w, r, owner, name, "identity is required")
		return
	}
	switch role {
	case model.RoleReader, model.RoleWriter, model.RoleMaintainer, model.RoleAdmin:
	default:
		h.renderRepoSettingsPage(w, r, owner, name, "invalid role; must be reader, writer, maintainer, or admin")
		return
	}

	if err := h.write.SetRole(ctx, repoName, identity, role); err != nil {
		slog.Error("ui set role", "repo", repoName, "identity", identity, "error", err)
		h.renderRepoSettingsPage(w, r, owner, name, "could not set role: "+err.Error())
		return
	}

	var changedBy string
	if h.identity != nil {
		changedBy = h.identity(ctx)
	}
	h.emit(ctx, evtypes.RoleChanged{
		Repo:      repoName,
		Identity:  identity,
		Role:      string(role),
		ChangedBy: changedBy,
	})
	http.Redirect(w, r, "/ui/r/"+repoName+"/settings", http.StatusSeeOther)
}

// handleDeleteRole processes POST /ui/r/{owner}/{name}/roles/{identity}/delete.
func (h *Handler) handleDeleteRole(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	identity := r.PathValue("identity")
	repoName := owner + "/" + name
	ctx := r.Context()

	if err := h.write.DeleteRole(ctx, repoName, identity); err != nil {
		switch {
		case errors.Is(err, db.ErrRoleNotFound):
			h.renderRepoSettingsPage(w, r, owner, name, "role not found")
		default:
			slog.Error("ui delete role", "repo", repoName, "identity", identity, "error", err)
			h.renderRepoSettingsPage(w, r, owner, name, "could not delete role")
		}
		return
	}

	var changedBy string
	if h.identity != nil {
		changedBy = h.identity(ctx)
	}
	h.emit(ctx, evtypes.RoleChanged{
		Repo:      repoName,
		Identity:  identity,
		Role:      "", // empty means removed
		ChangedBy: changedBy,
	})
	http.Redirect(w, r, "/ui/r/"+repoName+"/settings", http.StatusSeeOther)
}

// renderReleasesPage loads releases for a repo and renders the releases page with an optional error.
func (h *Handler) renderReleasesPage(w http.ResponseWriter, r *http.Request, owner, name, errMsg string) {
	repoName := owner + "/" + name
	ctx := r.Context()

	repo, err := h.write.GetRepo(ctx, repoName)
	if err != nil {
		if errors.Is(err, db.ErrRepoNotFound) {
			h.renderError(w, r, http.StatusNotFound, "repo not found: "+repoName)
			return
		}
		slog.Error("ui get repo", "repo", repoName, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load repo")
		return
	}

	releases, err := h.write.ListReleases(ctx, repoName, 100, "")
	if err != nil {
		slog.Error("ui list releases", "repo", repoName, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load releases")
		return
	}

	h.render(w, r, h.tmpl.releases, "layout.html", pageData{
		Title: repoName + " / releases",
		Breadcrumbs: []crumb{
			{Label: "repos", Href: "/ui/"},
			{Label: repoName, Href: "/ui/r/" + repoName},
			{Label: "releases", Href: ""},
		},
		Body: releasesPage{Repo: *repo, Releases: releases, Err: errMsg},
	})
}

// handleCreateRelease processes POST /ui/r/{owner}/{name}/releases to create a release.
func (h *Handler) handleCreateRelease(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	repoName := owner + "/" + name
	ctx := r.Context()

	if err := r.ParseForm(); err != nil {
		h.renderReleasesPage(w, r, owner, name, "invalid form data")
		return
	}
	relName := strings.TrimSpace(r.FormValue("name"))
	body := r.FormValue("body")
	seqStr := strings.TrimSpace(r.FormValue("sequence"))

	if relName == "" {
		h.renderReleasesPage(w, r, owner, name, "release name is required")
		return
	}

	var sequence int64
	if seqStr != "" {
		n, err := strconv.ParseInt(seqStr, 10, 64)
		if err != nil || n < 1 {
			h.renderReleasesPage(w, r, owner, name, "invalid sequence number")
			return
		}
		sequence = n
	} else {
		// Default to main branch head.
		bi, err := h.read.GetBranch(ctx, repoName, "main")
		if err != nil || bi == nil {
			slog.Error("ui create release get main head", "repo", repoName, "error", err)
			h.renderReleasesPage(w, r, owner, name, "could not resolve main branch head")
			return
		}
		sequence = bi.HeadSequence
	}

	var createdBy string
	if h.identity != nil {
		createdBy = h.identity(ctx)
	}

	if _, err := h.write.CreateRelease(ctx, repoName, relName, sequence, body, createdBy); err != nil {
		slog.Error("ui create release", "repo", repoName, "release", relName, "error", err)
		h.renderReleasesPage(w, r, owner, name, "could not create release: "+err.Error())
		return
	}

	h.emit(ctx, evtypes.ReleaseCreated{
		Repo:      repoName,
		Name:      relName,
		Sequence:  sequence,
		CreatedBy: createdBy,
	})
	http.Redirect(w, r, "/ui/r/"+repoName+"/releases", http.StatusSeeOther)
}

// handleDeleteRelease processes POST /ui/r/{owner}/{name}/releases/{rname}/delete.
func (h *Handler) handleDeleteRelease(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	rname := r.PathValue("rname")
	repoName := owner + "/" + name
	ctx := r.Context()

	if err := h.write.DeleteRelease(ctx, repoName, rname); err != nil {
		switch {
		case errors.Is(err, db.ErrReleaseNotFound):
			h.renderError(w, r, http.StatusNotFound, "release not found")
		default:
			slog.Error("ui delete release", "repo", repoName, "release", rname, "error", err)
			h.renderError(w, r, http.StatusInternalServerError, "could not delete release")
		}
		return
	}

	var deletedBy string
	if h.identity != nil {
		deletedBy = h.identity(ctx)
	}
	h.emit(ctx, evtypes.ReleaseDeleted{
		Repo:      repoName,
		Name:      rname,
		DeletedBy: deletedBy,
	})
	http.Redirect(w, r, "/ui/r/"+repoName+"/releases", http.StatusSeeOther)
}

// handleCreateOrg renders the new-org form (GET) and creates the org (POST).
func (h *Handler) handleCreateOrg(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	renderPage := func(name, errMsg string) {
		h.render(w, r, h.tmpl.createOrg, "layout.html", pageData{
			Title: "New organisation",
			Breadcrumbs: []crumb{
				{Label: "repos", Href: "/ui/"},
				{Label: "new org", Href: ""},
			},
			Body: createOrgPage{Name: name, Err: errMsg},
		})
	}

	if r.Method == http.MethodGet {
		renderPage("", "")
		return
	}

	// POST: create the org.
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		renderPage("", "Organisation name is required.")
		return
	}

	var identity string
	if h.identity != nil {
		identity = h.identity(ctx)
	}

	if _, err := h.svc.CreateOrg(ctx, identity, name); err != nil {
		switch {
		case errors.Is(err, db.ErrOrgExists):
			renderPage(name, "An organisation with that name already exists.")
		default:
			slog.Error("ui create org", "name", name, "error", err)
			renderPage(name, "Could not create organisation: "+err.Error())
		}
		return
	}

	http.Redirect(w, r, "/ui/o/"+name, http.StatusSeeOther)
}

// handleCreateRepo renders the new-repo form (GET) and creates the repo (POST).
func (h *Handler) handleCreateRepo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	orgs, err := h.write.ListOrgs(ctx)
	if err != nil {
		slog.Error("ui create repo list orgs", "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load organisations")
		return
	}

	renderPage := func(owner, name, errMsg string) {
		h.render(w, r, h.tmpl.createRepo, "layout.html", pageData{
			Title: "New repository",
			Breadcrumbs: []crumb{
				{Label: "repos", Href: "/ui/"},
				{Label: "new repo", Href: ""},
			},
			Body: createRepoPage{Owner: owner, Name: name, Orgs: orgs, Err: errMsg},
		})
	}

	if r.Method == http.MethodGet {
		renderPage("", "", "")
		return
	}

	// POST: create the repo.
	owner := strings.TrimSpace(r.FormValue("owner"))
	name := strings.TrimSpace(r.FormValue("name"))

	if owner == "" || name == "" {
		renderPage(owner, name, "Organisation and repository name are both required.")
		return
	}

	var identity string
	if h.identity != nil {
		identity = h.identity(ctx)
	}

	req := model.CreateRepoRequest{Owner: owner, Name: name, CreatedBy: identity}
	if _, err := h.svc.CreateRepo(ctx, identity, req); err != nil {
		switch {
		case errors.Is(err, db.ErrRepoExists):
			renderPage(owner, name, "A repository with that name already exists in this organisation.")
		case errors.Is(err, db.ErrOrgNotFound):
			renderPage(owner, name, "Organisation not found.")
		default:
			slog.Error("ui create repo", "owner", owner, "name", name, "error", err)
			renderPage(owner, name, "Could not create repository: "+err.Error())
		}
		return
	}

	http.Redirect(w, r, "/ui/r/"+owner+"/"+name, http.StatusSeeOther)
}

// issueURL returns the UI URL for a specific issue detail page.
func issueURL(repoName, numberStr string) string {
	return "/ui/r/" + repoName + "/issues/" + numberStr
}

// handleNewIssue renders the new-issue form (GET) and creates an issue (POST).
func (h *Handler) handleNewIssue(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	repoName := owner + "/" + name
	ctx := r.Context()

	repo, err := h.write.GetRepo(ctx, repoName)
	if err != nil {
		if errors.Is(err, db.ErrRepoNotFound) {
			h.renderError(w, r, http.StatusNotFound, "repo not found: "+repoName)
			return
		}
		slog.Error("ui get repo", "repo", repoName, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load repo")
		return
	}

	renderForm := func(formTitle, formBody, formLabels, errMsg string) {
		h.render(w, r, h.tmpl.newIssue, "layout.html", pageData{
			Title: repoName + " / new issue",
			Breadcrumbs: []crumb{
				{Label: "repos", Href: "/ui/"},
				{Label: repoName, Href: "/ui/r/" + repoName},
				{Label: "issues", Href: "/ui/r/" + repoName + "/issues"},
				{Label: "new", Href: ""},
			},
			Body: newIssuePage{
				Repo:       *repo,
				Err:        errMsg,
				FormTitle:  formTitle,
				FormBody:   formBody,
				FormLabels: formLabels,
			},
		})
	}

	if r.Method == http.MethodGet {
		renderForm("", "", "", "")
		return
	}

	// POST: create the issue.
	if err := r.ParseForm(); err != nil {
		renderForm("", "", "", "invalid form data")
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	body := strings.TrimSpace(r.FormValue("body"))
	labelsStr := strings.TrimSpace(r.FormValue("labels"))

	if title == "" {
		renderForm(title, body, labelsStr, "title is required")
		return
	}

	var labels []string
	for _, l := range strings.Split(labelsStr, ",") {
		if l = strings.TrimSpace(l); l != "" {
			labels = append(labels, l)
		}
	}

	var identity string
	if h.identity != nil {
		identity = h.identity(ctx)
	}

	iss, err := h.svc.CreateIssue(ctx, identity, repoName, title, body, labels)
	if err != nil {
		slog.Error("ui create issue", "repo", repoName, "error", err)
		renderForm(title, body, labelsStr, "could not create issue")
		return
	}

	http.Redirect(w, r, fmt.Sprintf("/ui/r/%s/issues/%d", repoName, iss.Number), http.StatusSeeOther)
}

// handleEditIssue processes the edit-issue form POST, updating title/body/labels.
func (h *Handler) handleEditIssue(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	numberStr := r.PathValue("number")
	repoName := owner + "/" + name
	ctx := r.Context()

	number, err := strconv.ParseInt(numberStr, 10, 64)
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, "invalid issue number")
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, issueURL(repoName, numberStr)+"?error=invalid+form", http.StatusSeeOther)
		return
	}

	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		http.Redirect(w, r, issueURL(repoName, numberStr)+"?error=title+is+required", http.StatusSeeOther)
		return
	}

	body := r.FormValue("body")
	labelsStr := r.FormValue("labels")
	var labels []string
	for _, l := range strings.Split(labelsStr, ",") {
		if l = strings.TrimSpace(l); l != "" {
			labels = append(labels, l)
		}
	}
	labelsPtr := &labels

	var identity string
	if h.identity != nil {
		identity = h.identity(ctx)
	}
	role := h.svc.GetRole(ctx, repoName, identity)
	if _, err := h.svc.UpdateIssue(ctx, identity, role, repoName, number, &title, &body, labelsPtr); err != nil {
		if errors.Is(err, service.ErrForbidden) {
			http.Redirect(w, r, issueURL(repoName, numberStr)+"?error=forbidden", http.StatusSeeOther)
			return
		}
		slog.Error("ui edit issue", "repo", repoName, "number", number, "error", err)
		http.Redirect(w, r, issueURL(repoName, numberStr)+"?error="+url.QueryEscape("could not update issue"), http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, issueURL(repoName, numberStr), http.StatusSeeOther)
}

// handleIssueClose processes the close-issue form POST.
func (h *Handler) handleIssueClose(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	numberStr := r.PathValue("number")
	repoName := owner + "/" + name
	ctx := r.Context()

	number, err := strconv.ParseInt(numberStr, 10, 64)
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, "invalid issue number")
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, issueURL(repoName, numberStr)+"?error=invalid+form", http.StatusSeeOther)
		return
	}

	reason := model.IssueCloseReason(r.FormValue("reason"))
	switch reason {
	case model.IssueCloseReasonCompleted, model.IssueCloseReasonNotPlanned, model.IssueCloseReasonDuplicate:
		// valid
	default:
		reason = model.IssueCloseReasonCompleted
	}

	var identity string
	if h.identity != nil {
		identity = h.identity(ctx)
	}

	role := h.svc.GetRole(ctx, repoName, identity)
	if _, err := h.svc.CloseIssue(ctx, identity, role, repoName, number, reason); err != nil {
		if errors.Is(err, service.ErrForbidden) {
			http.Redirect(w, r, issueURL(repoName, numberStr)+"?error=forbidden", http.StatusSeeOther)
			return
		}
		slog.Error("ui close issue", "repo", repoName, "number", number, "error", err)
		http.Redirect(w, r, issueURL(repoName, numberStr)+"?error="+url.QueryEscape("could not close issue"), http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, issueURL(repoName, numberStr), http.StatusSeeOther)
}

// handleIssueReopen processes the reopen-issue form POST.
func (h *Handler) handleIssueReopen(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	numberStr := r.PathValue("number")
	repoName := owner + "/" + name

	number, err := strconv.ParseInt(numberStr, 10, 64)
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, "invalid issue number")
		return
	}

	var identity string
	if h.identity != nil {
		identity = h.identity(r.Context())
	}
	role := h.svc.GetRole(r.Context(), repoName, identity)
	if _, err := h.svc.ReopenIssue(r.Context(), identity, role, repoName, number); err != nil {
		if errors.Is(err, service.ErrForbidden) {
			http.Redirect(w, r, issueURL(repoName, numberStr)+"?error=forbidden", http.StatusSeeOther)
			return
		}
		slog.Error("ui reopen issue", "repo", repoName, "number", number, "error", err)
		http.Redirect(w, r, issueURL(repoName, numberStr)+"?error="+url.QueryEscape("could not reopen issue"), http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, issueURL(repoName, numberStr), http.StatusSeeOther)
}

// handleCreateIssueComment processes the add-comment form POST.
func (h *Handler) handleCreateIssueComment(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	numberStr := r.PathValue("number")
	repoName := owner + "/" + name
	ctx := r.Context()

	number, err := strconv.ParseInt(numberStr, 10, 64)
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, "invalid issue number")
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, issueURL(repoName, numberStr)+"?error=invalid+form", http.StatusSeeOther)
		return
	}

	body := strings.TrimSpace(r.FormValue("body"))
	if body == "" {
		http.Redirect(w, r, issueURL(repoName, numberStr)+"?error=comment+body+is+required", http.StatusSeeOther)
		return
	}

	var identity string
	if h.identity != nil {
		identity = h.identity(ctx)
	}

	if _, err := h.svc.CreateIssueComment(ctx, identity, repoName, number, body); err != nil {
		slog.Error("ui create issue comment", "repo", repoName, "number", number, "error", err)
		http.Redirect(w, r, issueURL(repoName, numberStr)+"?error="+url.QueryEscape("could not post comment"), http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, issueURL(repoName, numberStr), http.StatusSeeOther)
}

// handleEditIssueComment processes the edit-comment form POST.
func (h *Handler) handleEditIssueComment(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	numberStr := r.PathValue("number")
	commentID := r.PathValue("id")
	repoName := owner + "/" + name
	ctx := r.Context()

	number, err := strconv.ParseInt(numberStr, 10, 64)
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, "invalid issue number")
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, issueURL(repoName, numberStr)+"?error=invalid+form", http.StatusSeeOther)
		return
	}

	body := strings.TrimSpace(r.FormValue("body"))
	if body == "" {
		http.Redirect(w, r, issueURL(repoName, numberStr)+"?error=comment+body+is+required", http.StatusSeeOther)
		return
	}

	var identity string
	if h.identity != nil {
		identity = h.identity(ctx)
	}
	role := h.svc.GetRole(ctx, repoName, identity)
	if _, err := h.svc.UpdateIssueComment(ctx, identity, role, repoName, number, commentID, body); err != nil {
		if errors.Is(err, service.ErrForbidden) {
			http.Redirect(w, r, issueURL(repoName, numberStr)+"?error=forbidden", http.StatusSeeOther)
			return
		}
		slog.Error("ui edit issue comment", "repo", repoName, "comment", commentID, "error", err)
		http.Redirect(w, r, issueURL(repoName, numberStr)+"?error="+url.QueryEscape("could not update comment"), http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, issueURL(repoName, numberStr), http.StatusSeeOther)
}

// handleDeleteIssueComment processes the delete-comment form POST.
func (h *Handler) handleDeleteIssueComment(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	numberStr := r.PathValue("number")
	commentID := r.PathValue("id")
	repoName := owner + "/" + name
	ctx := r.Context()

	var identity string
	if h.identity != nil {
		identity = h.identity(ctx)
	}
	role := h.svc.GetRole(ctx, repoName, identity)
	if err := h.svc.DeleteIssueComment(ctx, identity, role, repoName, commentID); err != nil {
		if errors.Is(err, service.ErrForbidden) {
			http.Redirect(w, r, issueURL(repoName, numberStr)+"?error=forbidden", http.StatusSeeOther)
			return
		}
		slog.Error("ui delete issue comment", "repo", repoName, "comment", commentID, "error", err)
		http.Redirect(w, r, issueURL(repoName, numberStr)+"?error="+url.QueryEscape("could not delete comment"), http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, issueURL(repoName, numberStr), http.StatusSeeOther)
}

// handleAddIssueRef processes the add-reference form POST.
func (h *Handler) handleAddIssueRef(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	numberStr := r.PathValue("number")
	repoName := owner + "/" + name
	ctx := r.Context()

	number, err := strconv.ParseInt(numberStr, 10, 64)
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, "invalid issue number")
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, issueURL(repoName, numberStr)+"?error=invalid+form", http.StatusSeeOther)
		return
	}

	refType := model.IssueRefType(r.FormValue("ref_type"))
	refID := strings.TrimSpace(r.FormValue("ref_id"))

	if refID == "" {
		http.Redirect(w, r, issueURL(repoName, numberStr)+"?error=ref+ID+is+required", http.StatusSeeOther)
		return
	}
	switch refType {
	case model.IssueRefTypeProposal, model.IssueRefTypeCommit:
		// valid
	default:
		http.Redirect(w, r, issueURL(repoName, numberStr)+"?error=invalid+ref+type", http.StatusSeeOther)
		return
	}

	if _, err := h.write.CreateIssueRef(ctx, repoName, number, refType, refID); err != nil {
		slog.Error("ui add issue ref", "repo", repoName, "number", number, "error", err)
		http.Redirect(w, r, issueURL(repoName, numberStr)+"?error="+url.QueryEscape("could not add reference"), http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, issueURL(repoName, numberStr), http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// Branch write handlers (POST-redirect-GET pattern)
// ---------------------------------------------------------------------------

// handleUICreateBranch processes the create-branch form on the branch list page.
func (h *Handler) handleUICreateBranch(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	repoName := owner + "/" + name

	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/ui/r/"+repoName+"?err=invalid+form", http.StatusSeeOther)
		return
	}

	branchName := strings.TrimSpace(r.FormValue("branch_name"))
	if branchName == "" {
		http.Redirect(w, r, "/ui/r/"+repoName+"?err=branch+name+is+required", http.StatusSeeOther)
		return
	}

	draft := r.FormValue("draft") == "1"
	resp, err := h.write.CreateBranch(r.Context(), model.CreateBranchRequest{
		Repo:  repoName,
		Name:  branchName,
		Draft: draft,
	})
	if err != nil {
		slog.Error("ui create branch", "repo", repoName, "branch", branchName, "error", err)
		errMsg := "create failed: " + err.Error()
		http.Redirect(w, r, "/ui/r/"+repoName+"?err="+url.QueryEscape(errMsg), http.StatusSeeOther)
		return
	}

	var identity string
	if h.identity != nil {
		identity = h.identity(r.Context())
	}
	h.emit(r.Context(), evtypes.BranchCreated{
		Repo:         repoName,
		Branch:       branchName,
		BaseSequence: resp.BaseSequence,
		CreatedBy:    identity,
	})
	http.Redirect(w, r, "/ui/r/"+repoName, http.StatusSeeOther)
}

// handleUIDeleteBranch processes the delete-branch form on the branch list and detail pages.
func (h *Handler) handleUIDeleteBranch(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	repoName := owner + "/" + name

	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/ui/r/"+repoName+"?err=invalid+form", http.StatusSeeOther)
		return
	}

	branchName := strings.TrimSpace(r.FormValue("branch"))
	if branchName == "" {
		http.Redirect(w, r, "/ui/r/"+repoName+"?err=branch+name+is+required", http.StatusSeeOther)
		return
	}

	if err := h.write.DeleteBranch(r.Context(), repoName, branchName); err != nil {
		slog.Error("ui delete branch", "repo", repoName, "branch", branchName, "error", err)
		errMsg := "delete failed: " + err.Error()
		// Redirect to detail page if coming from there, otherwise branch list.
		ref := r.FormValue("ref")
		if ref == "detail" {
			http.Redirect(w, r, "/ui/r/"+repoName+"/b/"+url.PathEscape(branchName)+"?err="+url.QueryEscape(errMsg), http.StatusSeeOther)
		} else {
			http.Redirect(w, r, "/ui/r/"+repoName+"?err="+url.QueryEscape(errMsg), http.StatusSeeOther)
		}
		return
	}

	var identity string
	if h.identity != nil {
		identity = h.identity(r.Context())
	}
	h.emit(r.Context(), evtypes.BranchAbandoned{
		Repo:        repoName,
		Branch:      branchName,
		AbandonedBy: identity,
	})
	http.Redirect(w, r, "/ui/r/"+repoName, http.StatusSeeOther)
}

// handleUIPromoteBranch promotes a draft branch to active (sets draft=false).
func (h *Handler) handleUIPromoteBranch(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	repoName := owner + "/" + name

	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/ui/r/"+repoName+"?err=invalid+form", http.StatusSeeOther)
		return
	}

	branchName := strings.TrimSpace(r.FormValue("branch"))
	if branchName == "" {
		http.Redirect(w, r, "/ui/r/"+repoName+"?err=branch+name+is+required", http.StatusSeeOther)
		return
	}

	if err := h.write.UpdateBranchDraft(r.Context(), repoName, branchName, false); err != nil {
		slog.Error("ui promote branch", "repo", repoName, "branch", branchName, "error", err)
		errMsg := "promote failed: " + err.Error()
		ref := r.FormValue("ref")
		if ref == "detail" {
			http.Redirect(w, r, "/ui/r/"+repoName+"/b/"+url.PathEscape(branchName)+"?err="+url.QueryEscape(errMsg), http.StatusSeeOther)
		} else {
			http.Redirect(w, r, "/ui/r/"+repoName+"?err="+url.QueryEscape(errMsg), http.StatusSeeOther)
		}
		return
	}

	var identity string
	if h.identity != nil {
		identity = h.identity(r.Context())
	}
	h.emit(r.Context(), evtypes.BranchDraftUpdated{
		Repo:      repoName,
		Branch:    branchName,
		Draft:     false,
		UpdatedBy: identity,
	})
	ref := r.FormValue("ref")
	if ref == "detail" {
		http.Redirect(w, r, "/ui/r/"+repoName+"/b/"+url.PathEscape(branchName), http.StatusSeeOther)
	} else {
		http.Redirect(w, r, "/ui/r/"+repoName, http.StatusSeeOther)
	}
}

// handleUISetAutoMerge enables or disables auto-merge on a branch.
func (h *Handler) handleUISetAutoMerge(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	repoName := owner + "/" + name

	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/ui/r/"+repoName+"?err=invalid+form", http.StatusSeeOther)
		return
	}

	branchName := strings.TrimSpace(r.FormValue("branch"))
	if branchName == "" {
		http.Redirect(w, r, "/ui/r/"+repoName+"?err=branch+name+is+required", http.StatusSeeOther)
		return
	}

	enable := r.FormValue("enable") == "1"
	if err := h.write.SetBranchAutoMerge(r.Context(), repoName, branchName, enable); err != nil {
		slog.Error("ui set auto-merge", "repo", repoName, "branch", branchName, "enable", enable, "error", err)
		errMsg := "auto-merge update failed: " + err.Error()
		ref := r.FormValue("ref")
		if ref == "detail" {
			http.Redirect(w, r, "/ui/r/"+repoName+"/b/"+url.PathEscape(branchName)+"?err="+url.QueryEscape(errMsg), http.StatusSeeOther)
		} else {
			http.Redirect(w, r, "/ui/r/"+repoName+"?err="+url.QueryEscape(errMsg), http.StatusSeeOther)
		}
		return
	}

	ref := r.FormValue("ref")
	if ref == "detail" {
		http.Redirect(w, r, "/ui/r/"+repoName+"/b/"+url.PathEscape(branchName), http.StatusSeeOther)
	} else {
		http.Redirect(w, r, "/ui/r/"+repoName, http.StatusSeeOther)
	}

}


// Write handlers (POST forms)
// ---------------------------------------------------------------------------

// renderBranchDetail is the shared helper that loads and renders the branch
// detail page. It is called by the GET handler and by POST handlers on error.
func (h *Handler) renderBranchDetail(w http.ResponseWriter, r *http.Request, repoName, branch, errMsg string) {
	repo, err := h.write.GetRepo(r.Context(), repoName)
	if err != nil {
		if errors.Is(err, db.ErrRepoNotFound) {
			h.renderError(w, r, http.StatusNotFound, "repo not found: "+repoName)
			return
		}
		slog.Error("ui get repo", "repo", repoName, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load repo")
		return
	}

	if h.assemble == nil {
		h.renderError(w, r, http.StatusServiceUnavailable, "agent context assembler not configured")
		return
	}
	actCtx, err := h.assemble(r.Context(), repoName, branch)
	if err != nil {
		if strings.Contains(err.Error(), "branch not found") {
			h.renderError(w, r, http.StatusNotFound, "branch not found: "+branch)
			return
		}
		slog.Error("ui assemble agent context", "repo", repoName, "branch", branch, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load branch")
		return
	}

	page := branchDetailPage{Repo: *repo, Ctx: actCtx, Err: errMsg}
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

	h.render(w, r, h.tmpl.branchDetail, "layout.html", pageData{
		Title: repoName + " / " + branch,
		Breadcrumbs: []crumb{
			{Label: "repos", Href: "/ui/"},
			{Label: repoName, Href: "/ui/r/" + repoName},
			{Label: branch, Href: ""},
		},
		Body: page,
	})
}

// handleSubmitReview processes a POST form to submit a review on a branch.
func (h *Handler) handleSubmitReview(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	branch := r.PathValue("branch")
	repoName := owner + "/" + name

	if err := r.ParseForm(); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "invalid form data")
		return
	}

	status := model.ReviewStatus(r.FormValue("status"))
	body := r.FormValue("body")

	if status == "" {
		h.renderBranchDetail(w, r, repoName, branch, "status is required")
		return
	}

	var identity string
	if h.identity != nil {
		identity = h.identity(r.Context())
	}

	if _, err := h.svc.CreateReview(r.Context(), identity, repoName, branch, status, body); err != nil {
		slog.Error("ui create review", "repo", repoName, "branch", branch, "error", err)
		h.renderBranchDetail(w, r, repoName, branch, "could not submit review: "+err.Error())
		return
	}

	http.Redirect(w, r, "/ui/r/"+repoName+"/b/"+url.PathEscape(branch), http.StatusSeeOther)
}

// handlePostComment processes a POST form to add an inline review comment.
func (h *Handler) handlePostComment(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	branch := r.PathValue("branch")
	repoName := owner + "/" + name

	if err := r.ParseForm(); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "invalid form data")
		return
	}

	path := r.FormValue("path")
	versionID := r.FormValue("version_id")
	body := r.FormValue("body")

	if path == "" || versionID == "" || body == "" {
		h.renderBranchDetail(w, r, repoName, branch, "path, version_id, and body are required")
		return
	}

	var identity string
	if h.identity != nil {
		identity = h.identity(r.Context())
	}

	if _, err := h.write.CreateReviewComment(r.Context(), repoName, branch, path, versionID, body, identity, nil); err != nil {
		slog.Error("ui create review comment", "repo", repoName, "branch", branch, "error", err)
		h.renderBranchDetail(w, r, repoName, branch, "could not post comment: "+err.Error())
		return
	}

	http.Redirect(w, r, "/ui/r/"+repoName+"/b/"+url.PathEscape(branch), http.StatusSeeOther)
}

// handleDeleteComment processes a POST form to delete an inline review comment.
func (h *Handler) handleDeleteComment(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	branch := r.PathValue("branch")
	id := r.PathValue("id")
	repoName := owner + "/" + name

	var identity string
	if h.identity != nil {
		identity = h.identity(r.Context())
	}
	role := h.svc.GetRole(r.Context(), repoName, identity)
	if err := h.svc.DeleteReviewComment(r.Context(), identity, role, repoName, id); err != nil {
		if errors.Is(err, service.ErrForbidden) {
			http.Redirect(w, r, "/ui/r/"+repoName+"/b/"+url.PathEscape(branch)+"?err=forbidden", http.StatusSeeOther)
			return
		}
		slog.Error("ui delete review comment", "repo", repoName, "id", id, "error", err)
		h.renderBranchDetail(w, r, repoName, branch, "could not delete comment: "+err.Error())
		return
	}

	http.Redirect(w, r, "/ui/r/"+repoName+"/b/"+url.PathEscape(branch), http.StatusSeeOther)
}

// handleCreateProposalUI processes a POST form to create a new proposal.
func (h *Handler) handleCreateProposalUI(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	repoName := owner + "/" + name
	ctx := r.Context()

	if err := r.ParseForm(); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "invalid form data")
		return
	}

	branch := r.FormValue("branch")
	baseBranch := r.FormValue("base_branch")
	title := r.FormValue("title")
	description := r.FormValue("description")

	renderWithErr := func(errMsg string) {
		repo, err := h.write.GetRepo(ctx, repoName)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "could not load repo")
			return
		}
		stateStr := "open"
		_, ps := proposalStateFromQuery(stateStr)
		proposals, err := h.write.ListProposals(ctx, repoName, ps, nil)
		if err != nil {
			proposals = nil
		}
		h.render(w, r, h.tmpl.proposals, "layout.html", pageData{
			Title: repoName + " / proposals",
			Breadcrumbs: []crumb{
				{Label: "repos", Href: "/ui/"},
				{Label: repoName, Href: "/ui/r/" + repoName},
				{Label: "proposals", Href: ""},
			},
			Body: proposalsPage{Repo: *repo, Proposals: proposals, State: stateStr, Err: errMsg},
		})
	}

	if branch == "" || baseBranch == "" || title == "" {
		renderWithErr("branch, base_branch, and title are required")
		return
	}

	var identity string
	if h.identity != nil {
		identity = h.identity(ctx)
	}

	proposal, err := h.svc.CreateProposal(ctx, identity, repoName, branch, baseBranch, title, description)
	if err != nil {
		slog.Error("ui create proposal", "repo", repoName, "error", err)
		renderWithErr("could not create proposal: " + err.Error())
		return
	}

	http.Redirect(w, r, "/ui/r/"+repoName+"/proposals/"+proposal.ID, http.StatusSeeOther)
}

// handleEditProposal processes a POST form to update a proposal's title/description.
func (h *Handler) handleEditProposal(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	id := r.PathValue("id")
	repoName := owner + "/" + name
	ctx := r.Context()

	if err := r.ParseForm(); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "invalid form data")
		return
	}

	titleVal := r.FormValue("title")
	descVal := r.FormValue("description")

	renderWithErr := func(proposal *model.Proposal, errMsg string) {
		repo, err := h.write.GetRepo(ctx, repoName)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "could not load repo")
			return
		}
		h.render(w, r, h.tmpl.proposalDetail, "layout.html", pageData{
			Title: repoName + " / proposals / " + id,
			Breadcrumbs: []crumb{
				{Label: "repos", Href: "/ui/"},
				{Label: repoName, Href: "/ui/r/" + repoName},
				{Label: "proposals", Href: "/ui/r/" + repoName + "/proposals"},
				{Label: proposal.Title, Href: ""},
			},
			Body: proposalDetailPage{Repo: *repo, Proposal: proposal, Err: errMsg},
		})
	}

	// Load current proposal for error re-render.
	proposal, err := h.write.GetProposal(ctx, repoName, id)
	if err != nil || proposal == nil {
		h.renderError(w, r, http.StatusNotFound, "proposal not found")
		return
	}

	if titleVal == "" {
		renderWithErr(proposal, "title is required")
		return
	}

	var identity string
	if h.identity != nil {
		identity = h.identity(ctx)
	}
	role := h.svc.GetRole(ctx, repoName, identity)
	updated, err := h.svc.UpdateProposal(ctx, identity, role, repoName, id, &titleVal, &descVal)
	if err != nil {
		if errors.Is(err, service.ErrForbidden) {
			p2, _ := h.write.GetProposal(ctx, repoName, id)
			if p2 == nil {
				p2 = proposal
			}
			renderWithErr(p2, "forbidden: must be proposal author or maintainer")
			return
		}
		slog.Error("ui update proposal", "repo", repoName, "id", id, "error", err)
		renderWithErr(proposal, "could not update proposal: "+err.Error())
		return
	}

	http.Redirect(w, r, "/ui/r/"+repoName+"/proposals/"+updated.ID, http.StatusSeeOther)
}

// handleCloseProposalUI processes a POST form to close a proposal.
func (h *Handler) handleCloseProposalUI(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	id := r.PathValue("id")
	repoName := owner + "/" + name
	ctx := r.Context()

	var identity string
	if h.identity != nil {
		identity = h.identity(ctx)
	}
	role := h.svc.GetRole(ctx, repoName, identity)
	if err := h.svc.CloseProposal(ctx, identity, role, repoName, id); err != nil {
		if errors.Is(err, service.ErrForbidden) {
			proposal, _ := h.write.GetProposal(ctx, repoName, id)
			repo, _ := h.write.GetRepo(ctx, repoName)
			if proposal != nil && repo != nil {
				h.render(w, r, h.tmpl.proposalDetail, "layout.html", pageData{
					Title: repoName + " / proposals / " + id,
					Breadcrumbs: []crumb{
						{Label: "repos", Href: "/ui/"},
						{Label: repoName, Href: "/ui/r/" + repoName},
						{Label: "proposals", Href: "/ui/r/" + repoName + "/proposals"},
						{Label: proposal.Title, Href: ""},
					},
					Body: proposalDetailPage{Repo: *repo, Proposal: proposal, Err: "forbidden: must be proposal author or maintainer"},
				})
				return
			}
			h.renderError(w, r, http.StatusForbidden, "forbidden")
			return
		}
		slog.Error("ui close proposal", "repo", repoName, "id", id, "error", err)

		// Re-render proposal detail with error.
		proposal, getErr := h.write.GetProposal(ctx, repoName, id)
		if getErr != nil || proposal == nil {
			h.renderError(w, r, http.StatusInternalServerError, "could not close proposal")
			return
		}
		repo, getErr := h.write.GetRepo(ctx, repoName)
		if getErr != nil {
			h.renderError(w, r, http.StatusInternalServerError, "could not load repo")
			return
		}
		h.render(w, r, h.tmpl.proposalDetail, "layout.html", pageData{
			Title: repoName + " / proposals / " + id,
			Breadcrumbs: []crumb{
				{Label: "repos", Href: "/ui/"},
				{Label: repoName, Href: "/ui/r/" + repoName},
				{Label: "proposals", Href: "/ui/r/" + repoName + "/proposals"},
				{Label: proposal.Title, Href: ""},
			},
			Body: proposalDetailPage{Repo: *repo, Proposal: proposal, Err: "could not close proposal: " + err.Error()},
		})
		return
	}

	http.Redirect(w, r, "/ui/r/"+repoName+"/proposals/"+id, http.StatusSeeOther)
}

// handleUIDeleteOrg processes POST /ui/o/{org}/delete to delete an org.
// The form must include a "confirm" field matching the org name.
func (h *Handler) handleUIDeleteOrg(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	ctx := r.Context()

	if err := r.ParseForm(); err != nil {
		h.renderOrgPage(w, r, org, "invalid form data")
		return
	}
	confirm := r.FormValue("confirm")
	if confirm != org {
		h.renderOrgPage(w, r, org, "confirmation does not match org name")
		return
	}

	if err := h.write.DeleteOrg(ctx, org); err != nil {
		switch {
		case errors.Is(err, db.ErrOrgNotFound):
			h.renderError(w, r, http.StatusNotFound, "org not found: "+org)
		case errors.Is(err, db.ErrOrgHasRepos):
			h.renderOrgPage(w, r, org, "cannot delete org: org still has repos; delete them first")
		default:
			slog.Error("ui delete org", "org", org, "error", err)
			h.renderOrgPage(w, r, org, "could not delete org: "+err.Error())
		}
		return
	}

	var deletedBy string
	if h.identity != nil {
		deletedBy = h.identity(ctx)
	}
	h.emit(ctx, evtypes.OrgDeleted{Org: org, DeletedBy: deletedBy})
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}

// handleUIDeleteRepo processes POST /ui/r/{owner}/{name}/-/delete-repo to delete a repo.
// The form must include a "confirm" field matching the short repo name.
func (h *Handler) handleUIDeleteRepo(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	repoName := owner + "/" + name
	ctx := r.Context()

	if err := r.ParseForm(); err != nil {
		h.renderRepoSettingsPage(w, r, owner, name, "invalid form data")
		return
	}
	confirm := r.FormValue("confirm")
	if confirm != name {
		h.renderRepoSettingsPage(w, r, owner, name, "confirmation does not match repo name")
		return
	}

	if err := h.write.DeleteRepo(ctx, repoName); err != nil {
		switch {
		case errors.Is(err, db.ErrRepoNotFound):
			h.renderError(w, r, http.StatusNotFound, "repo not found: "+repoName)
		default:
			slog.Error("ui delete repo", "repo", repoName, "error", err)
			h.renderRepoSettingsPage(w, r, owner, name, "could not delete repo: "+err.Error())
		}
		return
	}

	var deletedBy string
	if h.identity != nil {
		deletedBy = h.identity(ctx)
	}
	h.emit(ctx, evtypes.RepoDeleted{Repo: repoName, DeletedBy: deletedBy})
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}

// chainToLogRows filters and converts ChainEntries to commitLogRows.
// handleNewCommit renders the commit form (GET) and submits a commit (POST).
func (h *Handler) handleNewCommit(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	branch := r.PathValue("branch")
	repoName := owner + "/" + name
	ctx := r.Context()

	repo, err := h.write.GetRepo(ctx, repoName)
	if err != nil {
		if errors.Is(err, db.ErrRepoNotFound) {
			h.renderError(w, r, http.StatusNotFound, "repo not found: "+repoName)
			return
		}
		slog.Error("ui get repo", "repo", repoName, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "could not load repo")
		return
	}

	renderForm := func(formMessage, formFilePath, formFileContent, errMsg string) {
		h.render(w, r, h.tmpl.newCommit, "layout.html", pageData{
			Title: repoName + " / " + branch + " / commit",
			Breadcrumbs: []crumb{
				{Label: "repos", Href: "/ui/"},
				{Label: repoName, Href: "/ui/r/" + repoName},
				{Label: branch, Href: "/ui/r/" + repoName + "/b/" + url.PathEscape(branch)},
				{Label: "commit", Href: ""},
			},
			Body: commitFormPage{
				Repo:            *repo,
				Branch:          branch,
				FormMessage:     formMessage,
				FormFilePath:    formFilePath,
				FormFileContent: formFileContent,
				Err:             errMsg,
			},
		})
	}

	if r.Method == http.MethodGet {
		renderForm("", "", "", "")
		return
	}

	// POST: submit the commit.
	if err := r.ParseForm(); err != nil {
		renderForm("", "", "", "invalid form data")
		return
	}

	message := strings.TrimSpace(r.FormValue("message"))
	filePath := strings.TrimSpace(r.FormValue("file_path"))
	fileContent := r.FormValue("file_content")

	if message == "" {
		renderForm(message, filePath, fileContent, "message is required")
		return
	}
	if filePath == "" {
		renderForm(message, filePath, fileContent, "file path is required")
		return
	}

	var identity string
	if h.identity != nil {
		identity = h.identity(ctx)
	}

	req := model.CommitRequest{
		Repo:   repoName,
		Branch: branch,
		Message: message,
		Files: []model.FileChange{
			{Path: filePath, Content: []byte(fileContent)},
		},
	}
	if _, err := h.svc.Commit(ctx, identity, req); err != nil {
		slog.Error("ui commit", "repo", repoName, "branch", branch, "error", err)
		renderForm(message, filePath, fileContent, "could not commit: "+err.Error())
		return
	}

	http.Redirect(w, r, "/ui/r/"+repoName+"/b/"+url.PathEscape(branch), http.StatusSeeOther)
}

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

