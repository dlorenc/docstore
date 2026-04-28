package ui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/service"
	"github.com/dlorenc/docstore/internal/store"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

type fakeRead struct {
	branches map[string][]store.BranchInfo
	tree     map[string][]store.TreeEntry
	files    map[string]*store.FileContent
}

func (f *fakeRead) MaterializeTree(_ context.Context, repo, _ string, _ *int64, _ int, _ string) ([]store.TreeEntry, error) {
	return f.tree[repo], nil
}
func (f *fakeRead) GetFile(_ context.Context, repo, _, path string, _ *int64) (*store.FileContent, error) {
	return f.files[repo+":"+path], nil
}
func (f *fakeRead) GetBranch(_ context.Context, _, _ string) (*store.BranchInfo, error) {
	return nil, nil
}
func (f *fakeRead) ListBranches(_ context.Context, repo, _ string, _, _ bool) ([]store.BranchInfo, error) {
	return f.branches[repo], nil
}
func (f *fakeRead) GetFileHistory(_ context.Context, _, _, _ string, _ int, _ *int64) ([]store.FileHistoryEntry, error) {
	return nil, nil
}
func (f *fakeRead) GetChain(_ context.Context, _ string, _, _ int64) ([]store.ChainEntry, error) {
	return nil, nil
}
func (f *fakeRead) GetCommit(_ context.Context, _ string, _ int64) (*store.CommitDetail, error) {
	return nil, nil
}

type fakeWrite struct {
	repos         []model.Repo
	issues        []model.Issue
	issueComments []model.IssueComment
	miss          bool
	proposals     []*model.Proposal
	releases      []model.Release
}

func (f *fakeWrite) ListRepos(_ context.Context) ([]model.Repo, error) { return f.repos, nil }
func (f *fakeWrite) ListOrgs(_ context.Context) ([]model.Org, error)   { return nil, nil }
func (f *fakeWrite) GetRepo(_ context.Context, name string) (*model.Repo, error) {
	if f.miss {
		return nil, db.ErrRepoNotFound
	}
	for _, r := range f.repos {
		if r.Name == name {
			return &r, nil
		}
	}
	return nil, db.ErrRepoNotFound
}
func (f *fakeWrite) ListReviewComments(_ context.Context, _, _ string, _ *string) ([]model.ReviewComment, error) {
	return nil, nil
}
func (f *fakeWrite) ListOrgMembers(_ context.Context, _ string) ([]model.OrgMember, error) {
	return nil, nil
}
func (f *fakeWrite) ListOrgMemberships(_ context.Context, _ string) ([]model.OrgMember, error) {
	return nil, nil
}
func (f *fakeWrite) ListRoles(_ context.Context, _ string) ([]model.Role, error) {
	return nil, nil
}
func (f *fakeWrite) ListRolesByIdentity(_ context.Context, _ string) ([]model.RepoRole, error) {
	return nil, nil
}
func (f *fakeWrite) ListCheckRuns(_ context.Context, _, _ string, _ *int64, _ bool) ([]model.CheckRun, error) {
	return nil, nil
}
func (f *fakeWrite) ListIssues(_ context.Context, _, _, _, _ string) ([]model.Issue, error) {
	return f.issues, nil
}
func (f *fakeWrite) GetIssue(_ context.Context, _ string, number int64) (*model.Issue, error) {
	for _, iss := range f.issues {
		if iss.Number == number {
			return &iss, nil
		}
	}
	return nil, db.ErrIssueNotFound
}
func (f *fakeWrite) ListIssueComments(_ context.Context, _ string, _ int64) ([]model.IssueComment, error) {
	return f.issueComments, nil
}
func (f *fakeWrite) GetInviteByToken(_ context.Context, _, _ string) (*model.OrgInvite, error) {
	return nil, db.ErrInviteNotFound
}
func (f *fakeWrite) AcceptInvite(_ context.Context, _, _, _ string) error {
	return nil
}
func (f *fakeWrite) ListInvites(_ context.Context, _ string) ([]model.OrgInvite, error) {
	return nil, nil
}
func (f *fakeWrite) ListOrgRepos(_ context.Context, owner string) ([]model.Repo, error) {
	var out []model.Repo
	for _, r := range f.repos {
		if r.Owner == owner {
			out = append(out, r)
		}
	}
	return out, nil
}
func (f *fakeWrite) ListProposals(_ context.Context, _ string, state *model.ProposalState, _ *string) ([]*model.Proposal, error) {
	if state == nil {
		return f.proposals, nil
	}
	var out []*model.Proposal
	for _, p := range f.proposals {
		if p.State == *state {
			out = append(out, p)
		}
	}
	return out, nil
}
func (f *fakeWrite) GetProposal(_ context.Context, _, id string) (*model.Proposal, error) {
	for _, p := range f.proposals {
		if p.ID == id {
			return p, nil
		}
	}
	return nil, nil
}
func (f *fakeWrite) ListReleases(_ context.Context, _ string, _ int, _ string) ([]model.Release, error) {
	return f.releases, nil
}
func (f *fakeWrite) GetRelease(_ context.Context, _, name string) (*model.Release, error) {
	for _, r := range f.releases {
		if r.Name == name {
			return &r, nil
		}
	}
	return nil, nil
}
func (f *fakeWrite) ListIssueRefs(_ context.Context, _ string, _ int64) ([]model.IssueRef, error) {
	return nil, nil
}
func (f *fakeWrite) CreateIssue(_ context.Context, _, _, _, _ string, _ []string) (*model.Issue, error) {
	iss := &model.Issue{ID: "new-id", Number: 99, Title: "new", Author: "test", State: "open"}
	return iss, nil
}
func (f *fakeWrite) UpdateIssue(_ context.Context, _ string, _ int64, _, _ *string, _ *[]string) (*model.Issue, error) {
	return nil, nil
}
func (f *fakeWrite) CloseIssue(_ context.Context, _ string, _ int64, _ model.IssueCloseReason, _ string) (*model.Issue, error) {
	return nil, nil
}
func (f *fakeWrite) ReopenIssue(_ context.Context, _ string, _ int64) (*model.Issue, error) {
	return nil, nil
}
func (f *fakeWrite) CreateIssueComment(_ context.Context, _ string, _ int64, _, _ string) (*model.IssueComment, error) {
	return &model.IssueComment{ID: "c-new"}, nil
}
func (f *fakeWrite) GetIssueComment(_ context.Context, _, id string) (*model.IssueComment, error) {
	for _, c := range f.issueComments {
		if c.ID == id {
			return &c, nil
		}
	}
	return nil, db.ErrIssueCommentNotFound
}
func (f *fakeWrite) UpdateIssueComment(_ context.Context, _, _, _ string) (*model.IssueComment, error) {
	return nil, nil
}
func (f *fakeWrite) DeleteIssueComment(_ context.Context, _, _ string) error {
	return nil
}
func (f *fakeWrite) CreateIssueRef(_ context.Context, _ string, _ int64, _ model.IssueRefType, _ string) (*model.IssueRef, error) {
	return &model.IssueRef{ID: "r-new"}, nil
}
func (f *fakeWrite) ListCIJobs(_ context.Context, _ string, _, _ *string, _ int) ([]model.CIJob, error) {
	return nil, nil
}
func (f *fakeWrite) GetCIJob(_ context.Context, _ string) (*model.CIJob, error) {
	return nil, nil
}
func (f *fakeWrite) CreateOrg(_ context.Context, name, _ string) (*model.Org, error) {
	if f.miss {
		return nil, db.ErrOrgExists
	}
	return &model.Org{Name: name}, nil
}
func (f *fakeWrite) CreateRepo(_ context.Context, req model.CreateRepoRequest) (*model.Repo, error) {
	if f.miss {
		return nil, db.ErrRepoExists
	}
	return &model.Repo{Name: req.FullName(), Owner: req.Owner}, nil
}
func (f *fakeWrite) CreateBranch(_ context.Context, req model.CreateBranchRequest) (*model.CreateBranchResponse, error) {
	return &model.CreateBranchResponse{Name: req.Name}, nil
}
func (f *fakeWrite) UpdateBranchDraft(_ context.Context, _, _ string, _ bool) error {
	return nil
}
func (f *fakeWrite) DeleteBranch(_ context.Context, _, _ string) error {
	return nil
}
func (f *fakeWrite) SetBranchAutoMerge(_ context.Context, _, _ string, _ bool) error {
	return nil
}
func (f *fakeWrite) AddOrgMember(_ context.Context, _, _ string, _ model.OrgRole, _ string) error {
	return nil
}
func (f *fakeWrite) RemoveOrgMember(_ context.Context, _, _ string) error { return nil }
func (f *fakeWrite) CreateInvite(_ context.Context, _, _ string, _ model.OrgRole, _, _ string, _ time.Time) (*model.OrgInvite, error) {
	return &model.OrgInvite{}, nil
}
func (f *fakeWrite) RevokeInvite(_ context.Context, _, _ string) error { return nil }
func (f *fakeWrite) GetRole(_ context.Context, _, _ string) (*model.Role, error) {
	return nil, nil
}
func (f *fakeWrite) SetRole(_ context.Context, _, _ string, _ model.RoleType) error {
	return nil
}
func (f *fakeWrite) DeleteRole(_ context.Context, _, _ string) error { return nil }
func (f *fakeWrite) CreateRelease(_ context.Context, _, _ string, _ int64, _, _ string) (*model.Release, error) {
	return &model.Release{}, nil
}
func (f *fakeWrite) DeleteRelease(_ context.Context, _, _ string) error { return nil }
func (f *fakeWrite) DeleteOrg(_ context.Context, _ string) error       { return nil }
func (f *fakeWrite) DeleteRepo(_ context.Context, _ string) error      { return nil }
func (f *fakeWrite) Commit(_ context.Context, _ model.CommitRequest) (*model.CommitResponse, error) {
	return &model.CommitResponse{Sequence: 1}, nil
}

// Compile-time check that fakeWrite satisfies WriteStoreLite.
var _ WriteStoreLite = (*fakeWrite)(nil)

func (f *fakeWrite) CreateReview(_ context.Context, _, _, _ string, _ model.ReviewStatus, _ string) (*model.Review, error) {
	return &model.Review{}, nil
}
func (f *fakeWrite) CreateReviewComment(_ context.Context, _, _, _, _, _, _ string, _ *string) (*model.ReviewComment, error) {
	return &model.ReviewComment{}, nil
}
func (f *fakeWrite) DeleteReviewComment(_ context.Context, _, _ string) error { return nil }
func (f *fakeWrite) GetReviewComment(_ context.Context, _, id string) (*model.ReviewComment, error) {
	return &model.ReviewComment{ID: id, Author: ""}, nil
}
func (f *fakeWrite) CreateProposal(_ context.Context, _, _, _, _, _, _ string) (*model.Proposal, error) {
	return &model.Proposal{ID: "new-p"}, nil
}
func (f *fakeWrite) UpdateProposal(_ context.Context, _, _ string, _, _ *string) (*model.Proposal, error) {
	if len(f.proposals) > 0 {
		return f.proposals[0], nil
	}
	return &model.Proposal{ID: "updated-p", Title: "updated"}, nil
}
func (f *fakeWrite) CloseProposal(_ context.Context, _, _ string) error { return nil }
func newFakeAssembler(branchName string) AssembleFn {
	return func(_ context.Context, _, branch string) (*model.AgentContextResponse, error) {
		if branch != branchName {
			return nil, fmt.Errorf("branch not found")
		}
		return &model.AgentContextResponse{
			Branch: model.Branch{
				Name:         branchName,
				HeadSequence: 42,
				BaseSequence: 10,
				Status:       model.BranchStatusActive,
			},
			Diff: model.DiffResponse{
				BranchChanges: []model.DiffEntry{{Path: "docs/hello.md", VersionID: strPtr("v-abc123abc123")}},
			},
			Reviews:   []model.Review{},
			CheckRuns: []model.CheckRun{},
			Policies:  []model.PolicyResult{{Name: "at-least-one-review", Pass: false, Reason: "needs 1 more approval"}},
			Mergeable: false,
		}, nil
	}
}

func strPtr(s string) *string { return &s }

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func newTestHandler(t *testing.T, r ReadStore, w *fakeWrite, a AssembleFn) http.Handler {
	t.Helper()
	svc := service.New(w, nil, nil)
	h, err := NewHandler(r, w, svc, a, nil)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	mux := http.NewServeMux()
	h.Register(mux)
	return mux
}

func getStatusAndBody(t *testing.T, h http.Handler, target string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

func TestHandleRepos_RendersOrgsAndRepos(t *testing.T) {
	w := &fakeWrite{repos: []model.Repo{
		{Name: "acme/a", Owner: "acme", CreatedBy: "me", CreatedAt: time.Now().Add(-1 * time.Hour)},
		{Name: "acme/b", Owner: "acme", CreatedBy: "me", CreatedAt: time.Now()},
		{Name: "beta/x", Owner: "beta", CreatedBy: "you", CreatedAt: time.Now()},
	}}
	h := newTestHandler(t, &fakeRead{}, w, nil)

	code, body := getStatusAndBody(t, h, "/ui/")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", code, body)
	}
	for _, want := range []string{"acme/a", "acme/b", "beta/x", "docstore"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestHandleBranches_UnknownRepo_Returns404HTML(t *testing.T) {
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{miss: true}, nil)
	code, body := getStatusAndBody(t, h, "/ui/r/unknown/unknown")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
	if !strings.Contains(body, "<!doctype html>") {
		t.Errorf("expected HTML error page, got: %s", body)
	}
	if !strings.Contains(body, "repo not found") {
		t.Errorf("expected 'repo not found' message")
	}
}

func TestHandleBranches_RendersSections(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme", CreatedAt: time.Now(), CreatedBy: "me"}}
	r := &fakeRead{branches: map[string][]store.BranchInfo{
		"acme/a": {
			{Name: "feature-1", HeadSequence: 5, BaseSequence: 1, Status: "active"},
			{Name: "old", HeadSequence: 10, BaseSequence: 1, Status: "merged"},
		},
	}}
	h := newTestHandler(t, r, &fakeWrite{repos: repos}, nil)

	code, body := getStatusAndBody(t, h, "/ui/r/acme/a")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if !strings.Contains(body, "feature-1") || !strings.Contains(body, "Active (1)") {
		t.Errorf("active section missing: %s", body)
	}
	if !strings.Contains(body, "Merged (1)") {
		t.Errorf("merged section missing")
	}
}

func TestHandleBranchDetail_RendersContext(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme", CreatedAt: time.Now(), CreatedBy: "me"}}
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{repos: repos}, newFakeAssembler("feat-x"))

	code, body := getStatusAndBody(t, h, "/ui/r/acme/a/b/feat-x")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", code, body)
	}
	for _, want := range []string{"feat-x", "docs/hello.md", "at-least-one-review", "Blocked"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestHandleBranchDetail_BranchNotFound_Returns404(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme", CreatedAt: time.Now(), CreatedBy: "me"}}
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{repos: repos}, newFakeAssembler("only-feat"))
	code, body := getStatusAndBody(t, h, "/ui/r/acme/a/b/does-not-exist")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, body=%s", code, body)
	}
}

func TestHandleChecksPartial_ReturnsFragment(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme"}}
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{repos: repos}, newFakeAssembler("feat-x"))
	code, body := getStatusAndBody(t, h, "/ui/_/r/acme/a/b/feat-x/checks")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if strings.Contains(body, "<!doctype html>") {
		t.Errorf("expected fragment, got full layout")
	}
	if !strings.Contains(body, "no checks yet") {
		t.Errorf("expected empty-checks marker, got: %s", body)
	}
}

func TestHandleCheckHistoryPartial_ReturnsFragment(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme"}}
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{repos: repos}, newFakeAssembler("feat-x"))

	// Missing check param → 400
	code, _ := getStatusAndBody(t, h, "/ui/_/r/acme/a/b/feat-x/check-history")
	if code != http.StatusBadRequest {
		t.Fatalf("missing check param: status = %d, want 400", code)
	}

	// With check param → 200 fragment (no layout)
	code, body := getStatusAndBody(t, h, "/ui/_/r/acme/a/b/feat-x/check-history?check=lint")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if strings.Contains(body, "<!doctype html>") {
		t.Errorf("expected fragment, got full layout")
	}
	if !strings.Contains(body, "no history found") {
		t.Errorf("expected empty-history marker, got: %s", body)
	}
}

func TestHandleFile_RendersTreeAndContent(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme", CreatedAt: time.Now()}}
	tree := []store.TreeEntry{
		{Path: "README.md", VersionID: "v-1"},
		{Path: "docs/hello.md", VersionID: "v-2"},
		{Path: "docs/nested/deep.md", VersionID: "v-3"},
	}
	fc := &store.FileContent{Path: "docs/hello.md", VersionID: "v-2", ContentHash: "abc", Content: []byte("line1\nline2\n")}
	r := &fakeRead{
		tree:  map[string][]store.TreeEntry{"acme/a": tree},
		files: map[string]*store.FileContent{"acme/a:docs/hello.md": fc},
	}
	h := newTestHandler(t, r, &fakeWrite{repos: repos}, nil)

	code, body := getStatusAndBody(t, h, "/ui/r/acme/a/f/docs/hello.md?branch=main")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", code, body)
	}
	for _, want := range []string{"hello.md", "line1", "line2", "nested"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in body", want)
		}
	}
}

func TestHandleIssues_RendersTable(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme", CreatedAt: time.Now(), CreatedBy: "me"}}
	issues := []model.Issue{
		{ID: "i1", Repo: "acme/a", Number: 1, Title: "first bug", Author: "alice", State: "open", CreatedAt: time.Now()},
		{ID: "i2", Repo: "acme/a", Number: 2, Title: "second bug", Author: "bob", State: "open", CreatedAt: time.Now()},
	}
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{repos: repos, issues: issues}, nil)

	code, body := getStatusAndBody(t, h, "/ui/r/acme/a/issues")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", code, body)
	}
	for _, want := range []string{"first bug", "second bug", "alice", "bob", "#1", "#2"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestHandleIssues_DefaultsToOpen(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme", CreatedAt: time.Now(), CreatedBy: "me"}}
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{repos: repos}, nil)

	code, body := getStatusAndBody(t, h, "/ui/r/acme/a/issues")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", code, body)
	}
	// Active tab should be "open"
	if !strings.Contains(body, `tab active`) {
		t.Errorf("expected active tab in body")
	}
}

func TestHandleIssues_UnknownRepo_Returns404(t *testing.T) {
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{miss: true}, nil)
	code, _ := getStatusAndBody(t, h, "/ui/r/unknown/repo/issues")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
}

func TestHandleIssuesPartial_ReturnsFragment(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme", CreatedAt: time.Now(), CreatedBy: "me"}}
	issues := []model.Issue{
		{ID: "i1", Repo: "acme/a", Number: 3, Title: "partial issue", Author: "carol", State: "closed", CreatedAt: time.Now()},
	}
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{repos: repos, issues: issues}, nil)

	code, body := getStatusAndBody(t, h, "/ui/_/r/acme/a/issues?state=closed")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if strings.Contains(body, "<!doctype html>") {
		t.Errorf("expected fragment, got full layout")
	}
	if !strings.Contains(body, "partial issue") {
		t.Errorf("expected issue title in fragment, got: %s", body)
	}
}

func TestHandleIssueDetail_RendersIssueAndComments(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme", CreatedAt: time.Now(), CreatedBy: "me"}}
	issues := []model.Issue{
		{ID: "i1", Repo: "acme/a", Number: 7, Title: "detail issue", Author: "dave", State: "open",
			Body: "some description", Labels: []string{"bug", "priority"}, CreatedAt: time.Now()},
	}
	comments := []model.IssueComment{
		{ID: "c1", IssueID: "i1", Repo: "acme/a", Body: "nice comment", Author: "eve", CreatedAt: time.Now()},
	}
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{repos: repos, issues: issues, issueComments: comments}, nil)

	code, body := getStatusAndBody(t, h, "/ui/r/acme/a/issues/7")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", code, body)
	}
	for _, want := range []string{"detail issue", "dave", "some description", "bug", "priority", "nice comment", "eve"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestHandleIssueDetail_NotFound_Returns404(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme", CreatedAt: time.Now(), CreatedBy: "me"}}
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{repos: repos}, nil)

	code, _ := getStatusAndBody(t, h, "/ui/r/acme/a/issues/999")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
}

func TestHandleIssueDetail_InvalidNumber_Returns400(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme", CreatedAt: time.Now(), CreatedBy: "me"}}
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{repos: repos}, nil)

	code, _ := getStatusAndBody(t, h, "/ui/r/acme/a/issues/notanumber")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}

func TestHandleOrg_RendersReposMembersInvites(t *testing.T) {
	repos := []model.Repo{
		{Name: "acme/a", Owner: "acme", CreatedBy: "me", CreatedAt: time.Now().Add(-1 * time.Hour)},
		{Name: "acme/b", Owner: "acme", CreatedBy: "me", CreatedAt: time.Now()},
		{Name: "beta/x", Owner: "beta", CreatedBy: "you", CreatedAt: time.Now()},
	}
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{repos: repos}, nil)

	code, body := getStatusAndBody(t, h, "/ui/o/acme")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", code, body)
	}
	for _, want := range []string{"acme/a", "acme/b", "Members", "Pending invites"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	if strings.Contains(body, "beta/x") {
		t.Errorf("body should not contain repo from different org")
	}
}

func TestSiblingTreeRows_DedupsDirs(t *testing.T) {
	entries := []store.TreeEntry{
		{Path: "a.md"},
		{Path: "dir/b.md"},
		{Path: "dir/c.md"},
		{Path: "dir/sub/d.md"},
	}
	rows := siblingTreeRows(entries, "")
	if len(rows) != 2 {
		t.Fatalf("rows=%+v", rows)
	}
	if !rows[0].IsDir || rows[0].Name != "dir" {
		t.Errorf("want dir row first, got %+v", rows[0])
	}
	if rows[1].IsDir || rows[1].Name != "a.md" {
		t.Errorf("want file a.md second, got %+v", rows[1])
	}
}

func TestHandleProposals_RendersList(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme", CreatedAt: time.Now(), CreatedBy: "me"}}
	ps := model.ProposalOpen
	w := &fakeWrite{
		repos: repos,
		proposals: []*model.Proposal{
			{ID: "p-1", Repo: "acme/a", Branch: "feat-x", BaseBranch: "main", Title: "My Proposal", Author: "alice", State: ps, CreatedAt: time.Now()},
		},
	}
	h := newTestHandler(t, &fakeRead{}, w, nil)

	code, body := getStatusAndBody(t, h, "/ui/r/acme/a/proposals?state=open")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", code, body)
	}
	for _, want := range []string{"My Proposal", "alice", "main", "open"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestHandleProposalsPartial_ReturnsFragment(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme"}}
	ps := model.ProposalOpen
	w := &fakeWrite{
		repos: repos,
		proposals: []*model.Proposal{
			{ID: "p-1", Title: "Frag Proposal", Author: "bob", State: ps, BaseBranch: "main", CreatedAt: time.Now()},
		},
	}
	h := newTestHandler(t, &fakeRead{}, w, nil)

	code, body := getStatusAndBody(t, h, "/ui/_/r/acme/a/proposals?state=open")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if strings.Contains(body, "<!doctype html>") {
		t.Errorf("expected fragment, got full layout")
	}
	if !strings.Contains(body, "Frag Proposal") {
		t.Errorf("expected proposal title in fragment, got: %s", body)
	}
}

func TestHandleProposalDetail_RendersDetail(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme", CreatedAt: time.Now(), CreatedBy: "me"}}
	ps := model.ProposalOpen
	w := &fakeWrite{
		repos: repos,
		proposals: []*model.Proposal{
			{ID: "p-abc", Repo: "acme/a", Branch: "feat-x", BaseBranch: "main", Title: "Detail Proposal", Description: "some details", Author: "carol", State: ps, CreatedAt: time.Now()},
		},
	}
	h := newTestHandler(t, &fakeRead{}, w, nil)

	code, body := getStatusAndBody(t, h, "/ui/r/acme/a/proposals/p-abc")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", code, body)
	}
	for _, want := range []string{"Detail Proposal", "carol", "feat-x", "some details"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestHandleProposalDetail_NotFound_Returns404(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme", CreatedAt: time.Now()}}
	w := &fakeWrite{repos: repos}
	h := newTestHandler(t, &fakeRead{}, w, nil)

	code, _ := getStatusAndBody(t, h, "/ui/r/acme/a/proposals/nonexistent")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
}

func TestHandleReleases_RendersList(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme", CreatedAt: time.Now(), CreatedBy: "me"}}
	w := &fakeWrite{
		repos: repos,
		releases: []model.Release{
			{ID: "r-1", Repo: "acme/a", Name: "v1.0.0", Sequence: 42, Body: "first release", CreatedBy: "dave", CreatedAt: time.Now()},
		},
	}
	h := newTestHandler(t, &fakeRead{}, w, nil)

	code, body := getStatusAndBody(t, h, "/ui/r/acme/a/releases")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", code, body)
	}
	for _, want := range []string{"v1.0.0", "42", "dave", "first release"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestHandleReleaseDetail_RendersDetail(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme", CreatedAt: time.Now(), CreatedBy: "me"}}
	w := &fakeWrite{
		repos: repos,
		releases: []model.Release{
			{ID: "r-2", Repo: "acme/a", Name: "v2.0.0", Sequence: 99, Body: "second release notes", CreatedBy: "eve", CreatedAt: time.Now()},
		},
	}
	h := newTestHandler(t, &fakeRead{}, w, nil)

	code, body := getStatusAndBody(t, h, "/ui/r/acme/a/releases/v2.0.0")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", code, body)
	}
	for _, want := range []string{"v2.0.0", "99", "eve", "second release notes"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestHandleReleaseDetail_NotFound_Returns404(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme", CreatedAt: time.Now()}}
	w := &fakeWrite{repos: repos}
	h := newTestHandler(t, &fakeRead{}, w, nil)

	code, _ := getStatusAndBody(t, h, "/ui/r/acme/a/releases/nonexistent")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
}

// postForm issues a POST with application/x-www-form-urlencoded body.
// It automatically includes a CSRF token (cookie + form field) so that the
// CSRF middleware passes. It does not follow redirects; the raw response is
// returned.
func postForm(t *testing.T, h http.Handler, target string, vals url.Values) (int, string, string) {
	t.Helper()
	const testCSRFToken = "test-csrf-token-0000000000000000"
	if vals == nil {
		vals = url.Values{}
	}
	vals.Set(csrfFieldName, testCSRFToken)
	body := vals.Encode()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: testCSRFToken})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String(), rec.Header().Get("Location")
}

func TestHandleCreateOrg_GET_RendersForm(t *testing.T) {
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{}, nil)
	code, body := getStatusAndBody(t, h, "/ui/orgs/new")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", code, body)
	}
	for _, want := range []string{"New organisation", `name="name"`, "Create organisation"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestHandleCreateOrg_POST_RedirectsOnSuccess(t *testing.T) {
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{}, nil)
	code, _, loc := postForm(t, h, "/ui/orgs/new", url.Values{"name": {"acme"}})
	if code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", code)
	}
	if loc != "/ui/o/acme" {
		t.Errorf("location = %q, want /ui/o/acme", loc)
	}
}

func TestHandleCreateOrg_POST_EmptyName_ReturnsError(t *testing.T) {
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{}, nil)
	code, body, _ := postForm(t, h, "/ui/orgs/new", url.Values{"name": {""}})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with error", code)
	}
	if !strings.Contains(body, "required") {
		t.Errorf("expected required error in body: %s", body)
	}
}

func TestHandleCreateOrg_POST_DuplicateName_ReturnsError(t *testing.T) {
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{miss: true}, nil)
	code, body, _ := postForm(t, h, "/ui/orgs/new", url.Values{"name": {"acme"}})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with error", code)
	}
	if !strings.Contains(body, "already exists") {
		t.Errorf("expected already-exists error in body: %s", body)
	}
}

func TestHandleCreateRepo_GET_RendersForm(t *testing.T) {
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{}, nil)
	code, body := getStatusAndBody(t, h, "/ui/repos/new")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", code, body)
	}
	for _, want := range []string{"New repository", `name="name"`, "Create repository"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestHandleCreateRepo_POST_RedirectsOnSuccess(t *testing.T) {
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{}, nil)
	code, _, loc := postForm(t, h, "/ui/repos/new", url.Values{"owner": {"acme"}, "name": {"myrepo"}})
	if code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", code)
	}
	if loc != "/ui/r/acme/myrepo" {
		t.Errorf("location = %q, want /ui/r/acme/myrepo", loc)
	}
}

func TestHandleCreateRepo_POST_MissingFields_ReturnsError(t *testing.T) {
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{}, nil)
	code, body, _ := postForm(t, h, "/ui/repos/new", url.Values{"owner": {"acme"}, "name": {""}})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with error", code)
	}
	if !strings.Contains(body, "required") {
		t.Errorf("expected required error in body: %s", body)
	}
}

func TestHandleCreateRepo_POST_DuplicateRepo_ReturnsError(t *testing.T) {
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{miss: true}, nil)
	code, body, _ := postForm(t, h, "/ui/repos/new", url.Values{"owner": {"acme"}, "name": {"myrepo"}})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with error", code)
	}
	if !strings.Contains(body, "already exists") {
		t.Errorf("expected already-exists error in body: %s", body)
	}
}

func TestHandleRepos_ShowsNewButtons(t *testing.T) {
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{}, nil)
	code, body := getStatusAndBody(t, h, "/ui/")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	for _, want := range []string{"/ui/orgs/new", "/ui/repos/new"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing link %q", want)
		}
	}
}

// ---------------------------------------------------------------------------
// Write handler tests
// ---------------------------------------------------------------------------

func TestHandleSubmitReview_RedirectsOnSuccess(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme", CreatedAt: time.Now()}}
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{repos: repos}, newFakeAssembler("feat-x"))

	code, _, _ := postForm(t, h, "/ui/r/acme/a/b/feat-x/review", url.Values{
		"status": {"approved"},
		"body": {"looks good"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (redirect)", code)
	}
}

func TestHandleSubmitReview_MissingStatus_RerendersPage(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme", CreatedAt: time.Now()}}
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{repos: repos}, newFakeAssembler("feat-x"))

	code, body, _ := postForm(t, h, "/ui/r/acme/a/b/feat-x/review", url.Values{})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !strings.Contains(body, "status is required") {
		t.Errorf("expected error message in body, got: %s", body)
	}
}

func TestHandlePostComment_RedirectsOnSuccess(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme", CreatedAt: time.Now()}}
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{repos: repos}, newFakeAssembler("feat-x"))

	code, _, _ := postForm(t, h, "/ui/r/acme/a/b/feat-x/comment", url.Values{
		"path": {"docs/hello.md"},
		"version_id": {"v-abc"},
		"body": {"nice file"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (redirect)", code)
	}
}

func TestHandlePostComment_MissingFields_RerendersPage(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme", CreatedAt: time.Now()}}
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{repos: repos}, newFakeAssembler("feat-x"))

	code, body, _ := postForm(t, h, "/ui/r/acme/a/b/feat-x/comment", url.Values{
		"path": {"docs/hello.md"},
		// missing version_id and body
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !strings.Contains(body, "path, version_id, and body are required") {
		t.Errorf("expected error message in body, got: %s", body)
	}
}

func TestHandleDeleteComment_RedirectsOnSuccess(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme", CreatedAt: time.Now()}}
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{repos: repos}, newFakeAssembler("feat-x"))

	code, _, _ := postForm(t, h, "/ui/r/acme/a/b/feat-x/comment/cmt-1/delete", nil)
	if code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (redirect)", code)
	}
}

func TestHandleCreateProposalUI_RedirectsOnSuccess(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme", CreatedAt: time.Now()}}
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{repos: repos}, nil)

	code, _, _ := postForm(t, h, "/ui/r/acme/a/proposals", url.Values{
		"branch": {"feat-x"},
		"base_branch": {"main"},
		"title": {"My proposal"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (redirect)", code)
	}
}

func TestHandleCreateProposalUI_MissingFields_RerendersPage(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme", CreatedAt: time.Now()}}
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{repos: repos}, nil)

	code, body, _ := postForm(t, h, "/ui/r/acme/a/proposals", url.Values{
		"branch": {"feat-x"},
		// missing base_branch and title
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !strings.Contains(body, "branch, base_branch, and title are required") {
		t.Errorf("expected error message in body, got: %s", body)
	}
}

func TestHandleEditProposal_RedirectsOnSuccess(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme", CreatedAt: time.Now()}}
	ps := model.ProposalOpen
	w := &fakeWrite{
		repos: repos,
		proposals: []*model.Proposal{
			{ID: "p-1", Repo: "acme/a", Branch: "feat-x", BaseBranch: "main", Title: "Old title", Author: "", State: ps, CreatedAt: time.Now()},
		},
	}
	h := newTestHandler(t, &fakeRead{}, w, nil)

	code, _, _ := postForm(t, h, "/ui/r/acme/a/proposals/p-1/edit", url.Values{
		"title": {"New title"},
		"description": {"updated description"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (redirect)", code)
	}
}

func TestHandleCloseProposalUI_RedirectsOnSuccess(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme", CreatedAt: time.Now()}}
	ps := model.ProposalOpen
	w := &fakeWrite{
		repos: repos,
		proposals: []*model.Proposal{
			{ID: "p-1", Repo: "acme/a", Branch: "feat-x", BaseBranch: "main", Title: "My proposal", Author: "", State: ps, CreatedAt: time.Now()},
		},
	}
	h := newTestHandler(t, &fakeRead{}, w, nil)

	code, _, _ := postForm(t, h, "/ui/r/acme/a/proposals/p-1/close", nil)
	if code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (redirect)", code)
	}
}

func TestHandleNewCommit_GET_RendersForm(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme", CreatedAt: time.Now(), CreatedBy: "me"}}
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{repos: repos}, nil)

	code, body := getStatusAndBody(t, h, "/ui/r/acme/a/b/feat-x/commit")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", code, body)
	}
	for _, want := range []string{"feat-x", "Commit message", `name="file_path"`, `name="file_content"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestHandleNewCommit_POST_RedirectsOnSuccess(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme", CreatedAt: time.Now()}}
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{repos: repos}, nil)

	code, _, loc := postForm(t, h, "/ui/r/acme/a/b/feat-x/commit", url.Values{
		"message":      {"add a file"},
		"file_path":    {"docs/hello.md"},
		"file_content": {"hello world"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (redirect)", code)
	}
	if loc != "/ui/r/acme/a/b/feat-x" {
		t.Errorf("location = %q, want /ui/r/acme/a/b/feat-x", loc)
	}
}

func TestHandleNewCommit_POST_MissingMessage_RerendersForm(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme", CreatedAt: time.Now()}}
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{repos: repos}, nil)

	code, body, _ := postForm(t, h, "/ui/r/acme/a/b/feat-x/commit", url.Values{
		"file_path":    {"docs/hello.md"},
		"file_content": {"content"},
		// message omitted
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with error", code)
	}
	if !strings.Contains(body, "message is required") {
		t.Errorf("expected error in body, got: %s", body)
	}
}

func TestHandleNewCommit_POST_MissingFilePath_RerendersForm(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme", CreatedAt: time.Now()}}
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{repos: repos}, nil)

	code, body, _ := postForm(t, h, "/ui/r/acme/a/b/feat-x/commit", url.Values{
		"message":      {"add something"},
		"file_content": {"content"},
		// file_path omitted
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with error", code)
	}
	if !strings.Contains(body, "file path is required") {
		t.Errorf("expected error in body, got: %s", body)
	}
}

func TestHandleNewCommit_UnknownRepo_Returns404(t *testing.T) {
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{miss: true}, nil)
	code, _ := getStatusAndBody(t, h, "/ui/r/unknown/repo/b/feat-x/commit")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
}

func TestHandleRepoSettings_RendersPage(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme", CreatedAt: time.Now(), CreatedBy: "me"}}
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{repos: repos}, nil)

	code, body := getStatusAndBody(t, h, "/ui/r/acme/a/settings")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", code, body)
	}
	for _, want := range []string{"settings", "Roles", "Danger zone", "Grant role", "Delete this repository"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestHandleRepoSettings_UnknownRepo_Returns404(t *testing.T) {
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{miss: true}, nil)
	code, _ := getStatusAndBody(t, h, "/ui/r/unknown/repo/settings")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
}

func TestHandleSetRole_RedirectsToSettings(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme", CreatedAt: time.Now()}}
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{repos: repos}, nil)

	code, _, loc := postForm(t, h, "/ui/r/acme/a/roles", url.Values{
		"identity": {"alice@example.com"},
		"role":     {"reader"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", code)
	}
	if loc != "/ui/r/acme/a/settings" {
		t.Errorf("location = %q, want /ui/r/acme/a/settings", loc)
	}
}

func TestHandleDeleteRole_RedirectsToSettings(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme", CreatedAt: time.Now()}}
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{repos: repos}, nil)

	code, _, loc := postForm(t, h, "/ui/r/acme/a/roles/alice@example.com/delete", nil)
	if code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", code)
	}
	if loc != "/ui/r/acme/a/settings" {
		t.Errorf("location = %q, want /ui/r/acme/a/settings", loc)
	}
}

func TestHandleUserProfile_RendersPage(t *testing.T) {
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{}, nil)
	code, body := getStatusAndBody(t, h, "/ui/u/alice@example.com")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", code, body)
	}
	if !strings.Contains(body, "alice@example.com") {
		t.Errorf("body missing identity")
	}
	if !strings.Contains(body, "Org memberships") {
		t.Errorf("body missing org memberships section")
	}
	if !strings.Contains(body, "Repo roles") {
		t.Errorf("body missing repo roles section")
	}
}

func TestHandleBranches_NoRolesOrDangerZone(t *testing.T) {
	repos := []model.Repo{{Name: "acme/a", Owner: "acme", CreatedAt: time.Now(), CreatedBy: "me"}}
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{repos: repos}, nil)

	_, body := getStatusAndBody(t, h, "/ui/r/acme/a")
	if strings.Contains(body, "Danger zone") {
		t.Errorf("branches page should not contain Danger zone section")
	}
	if strings.Contains(body, "Grant role") {
		t.Errorf("branches page should not contain Grant role form")
	}
}

// ---------------------------------------------------------------------------
// CSRF middleware tests
// ---------------------------------------------------------------------------

func TestCSRFMiddleware_MissingTokenReturns403(t *testing.T) {
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{}, nil)

	// POST without any cookie or form field — must be rejected.
	req := httptest.NewRequest(http.MethodPost, "/ui/orgs/new", strings.NewReader("name=acme"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestCSRFMiddleware_MismatchedTokenReturns403(t *testing.T) {
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{}, nil)

	// Cookie token differs from form field token.
	body := url.Values{csrfFieldName: {"token-a"}, "name": {"acme"}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/ui/orgs/new", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "token-b"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestCSRFMiddleware_GETSetsTokenCookie(t *testing.T) {
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{}, nil)

	// GET request should succeed and set the CSRF cookie.
	code, _ := getStatusAndBody(t, h, "/ui/orgs/new")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
}

func TestCSRFMiddleware_FormContainsCsrfField(t *testing.T) {
	h := newTestHandler(t, &fakeRead{}, &fakeWrite{}, nil)

	// GET the form page: the rendered HTML must contain the hidden CSRF field.
	// Because the test handler goes through CSRF middleware, a token is set.
	code, body := getStatusAndBody(t, h, "/ui/orgs/new")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !strings.Contains(body, `name="csrf_token"`) {
		t.Errorf("rendered form does not contain csrf_token hidden field; body snippet: %.200s", body)
	}
}
