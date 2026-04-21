package ui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/model"
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

type fakeWrite struct {
	repos []model.Repo
	miss  bool
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

func newTestHandler(t *testing.T, r ReadStore, w WriteStoreLite, a AssembleFn) http.Handler {
	t.Helper()
	h, err := NewHandler(r, w, a)
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
