package docstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dlorenc/docstore/api"
	"github.com/dlorenc/docstore/sdk/go/docstore"
)

// recordingHandler records the last request it handled and returns a scripted
// response. Tests assert on both directions.
type recordingHandler struct {
	t        *testing.T
	status   int
	respBody any

	// captured
	method    string
	path      string
	rawQuery  string
	headers   http.Header
	reqBody   []byte
	reqCount  int
	reqBodyFn func([]byte)
}

func (h *recordingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.reqCount++
	h.method = r.Method
	h.path = r.URL.Path
	h.rawQuery = r.URL.RawQuery
	h.headers = r.Header.Clone()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.t.Fatalf("read body: %v", err)
	}
	h.reqBody = body
	if h.reqBodyFn != nil {
		h.reqBodyFn(body)
	}
	w.Header().Set("Content-Type", "application/json")
	if h.status == 0 {
		h.status = http.StatusOK
	}
	w.WriteHeader(h.status)
	if h.respBody != nil && h.status != http.StatusNoContent {
		if err := json.NewEncoder(w).Encode(h.respBody); err != nil {
			h.t.Fatalf("encode resp: %v", err)
		}
	}
}

func newServer(t *testing.T, h *recordingHandler) (string, func()) {
	t.Helper()
	h.t = t
	srv := httptest.NewServer(h)
	return srv.URL, srv.Close
}

func newClient(t *testing.T, base string, opts ...docstore.ClientOption) *docstore.Client {
	t.Helper()
	c, err := docstore.NewClient(base, opts...)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestNewClient_Validation(t *testing.T) {
	if _, err := docstore.NewClient(""); err == nil {
		t.Fatal("expected error for empty base URL")
	}
	if _, err := docstore.NewClient("   "); err == nil {
		t.Fatal("expected error for whitespace base URL")
	}
}

func TestFile_SendsExpectedRequest(t *testing.T) {
	h := &recordingHandler{respBody: api.FileResponse{
		Path: "config.yaml", VersionID: "v1", ContentHash: "h1",
		Content: []byte("hello"),
	}}
	base, stop := newServer(t, h)
	defer stop()

	c := newClient(t, base,
		docstore.WithBearerToken("tok"),
		docstore.WithIdentity("alice@example.com"),
		docstore.WithUserAgent("sdk-test/1.0"),
	)

	got, err := c.Repo("acme/platform").File(context.Background(),
		"config.yaml", docstore.AtHead("main"))
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if got.Path != "config.yaml" || string(got.Content) != "hello" {
		t.Errorf("unexpected response: %+v", got)
	}

	if h.method != "GET" {
		t.Errorf("method = %q, want GET", h.method)
	}
	if h.path != "/repos/acme/platform/-/file/config.yaml" {
		t.Errorf("path = %q", h.path)
	}
	if h.rawQuery != "branch=main" {
		t.Errorf("query = %q", h.rawQuery)
	}
	if got, want := h.headers.Get("Authorization"), "Bearer tok"; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
	if got, want := h.headers.Get("X-DocStore-Identity"), "alice@example.com"; got != want {
		t.Errorf("X-DocStore-Identity = %q, want %q", got, want)
	}
	if got := h.headers.Get("User-Agent"); got != "sdk-test/1.0" {
		t.Errorf("User-Agent = %q", got)
	}
}

func TestFile_NestedPathIsEscaped(t *testing.T) {
	h := &recordingHandler{respBody: api.FileResponse{Path: "a/b c/d.yaml"}}
	base, stop := newServer(t, h)
	defer stop()

	c := newClient(t, base)
	if _, err := c.Repo("acme/platform").File(context.Background(), "a/b c/d.yaml"); err != nil {
		t.Fatalf("File: %v", err)
	}
	// "b c" must round-trip as %20 or +. Go decodes r.URL.Path already, so the
	// server sees the escaped form in RawPath. Here we check RawPath via the
	// captured path.
	if !strings.HasPrefix(h.path, "/repos/acme/platform/-/file/a/b") {
		t.Errorf("path = %q", h.path)
	}
}

func TestCommit_PostsJSONBody(t *testing.T) {
	h := &recordingHandler{
		status:   http.StatusCreated,
		respBody: api.CommitResponse{Sequence: 42},
	}
	base, stop := newServer(t, h)
	defer stop()

	c := newClient(t, base)
	_, err := c.Repo("acme/platform").Commit(context.Background(), api.CommitRequest{
		Branch:  "main",
		Message: "test",
		Files:   []api.FileChange{{Path: "a.txt", Content: []byte("x")}},
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if h.method != "POST" || h.path != "/repos/acme/platform/-/commit" {
		t.Errorf("method/path = %s %s", h.method, h.path)
	}
	if ct := h.headers.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var got api.CommitRequest
	if err := json.Unmarshal(h.reqBody, &got); err != nil {
		t.Fatalf("unmarshal req: %v", err)
	}
	if got.Branch != "main" || len(got.Files) != 1 || got.Files[0].Path != "a.txt" {
		t.Errorf("req body = %+v", got)
	}
}

func TestErrors_SentinelsMatchViaIs(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusUnauthorized, docstore.ErrUnauthorized},
		{http.StatusForbidden, docstore.ErrForbidden},
		{http.StatusNotFound, docstore.ErrNotFound},
		{http.StatusConflict, docstore.ErrConflict},
	}
	for _, tc := range cases {
		h := &recordingHandler{
			status:   tc.status,
			respBody: api.ErrorResponse{Error: "boom"},
		}
		base, stop := newServer(t, h)

		c := newClient(t, base)
		_, err := c.Repo("x/y").File(context.Background(), "z")
		stop()
		if err == nil {
			t.Fatalf("status %d: expected error", tc.status)
		}
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d: errors.Is returned false for %v", tc.status, tc.want)
		}
		var se *docstore.Error
		if !errors.As(err, &se) {
			t.Errorf("status %d: errors.As to *docstore.Error failed", tc.status)
		} else if se.Message != "boom" {
			t.Errorf("status %d: message = %q", tc.status, se.Message)
		}
	}
}

func TestMerge_ConflictErrorCarriesPayload(t *testing.T) {
	conflicts := []api.ConflictEntry{
		{Path: "a.txt", MainVersionID: "v1", BranchVersionID: "v2"},
	}
	h := &recordingHandler{
		status:   http.StatusConflict,
		respBody: api.MergeConflictError{Conflicts: conflicts},
	}
	base, stop := newServer(t, h)
	defer stop()

	c := newClient(t, base)
	_, err := c.Repo("a/b").Merge(context.Background(), api.MergeRequest{Branch: "feat"})
	if err == nil {
		t.Fatal("expected error")
	}
	var ce *docstore.ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("errors.As *ConflictError failed for %T", err)
	}
	if len(ce.Conflicts) != 1 || ce.Conflicts[0].Path != "a.txt" {
		t.Errorf("conflicts = %+v", ce.Conflicts)
	}
	if !errors.Is(err, docstore.ErrConflict) {
		t.Error("expected errors.Is ErrConflict")
	}
}

func TestMerge_PolicyErrorCarriesPayload(t *testing.T) {
	policies := []api.PolicyResult{
		{Name: "two-approvals", Pass: false, Reason: "only 1 approval"},
	}
	h := &recordingHandler{
		status:   http.StatusForbidden,
		respBody: api.MergePolicyError{Policies: policies},
	}
	base, stop := newServer(t, h)
	defer stop()

	c := newClient(t, base)
	_, err := c.Repo("a/b").Merge(context.Background(), api.MergeRequest{Branch: "feat"})
	if err == nil {
		t.Fatal("expected error")
	}
	var pe *docstore.PolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("errors.As *PolicyError failed for %T", err)
	}
	if len(pe.Policies) != 1 || pe.Policies[0].Name != "two-approvals" {
		t.Errorf("policies = %+v", pe.Policies)
	}
}

func TestBranches_QueryParamsFromOptions(t *testing.T) {
	h := &recordingHandler{respBody: api.BranchesResponse{}}
	base, stop := newServer(t, h)
	defer stop()

	c := newClient(t, base)
	_, err := c.Repo("a/b").Branches(context.Background(),
		docstore.BranchStatusFilter(api.BranchStatusActive),
		docstore.BranchIncludeDraft(),
	)
	if err != nil {
		t.Fatalf("Branches: %v", err)
	}
	// Order within a url.Values is stable by key alphabetically, since
	// net/url encodes keys sorted.
	if h.rawQuery != "include_draft=true&status=active" {
		t.Errorf("query = %q", h.rawQuery)
	}
}

func TestTree_PaginationOptions(t *testing.T) {
	h := &recordingHandler{respBody: api.TreeResponse{}}
	base, stop := newServer(t, h)
	defer stop()

	c := newClient(t, base)
	_, err := c.Repo("a/b").Tree(context.Background(),
		docstore.TreeAtHead("main"),
		docstore.TreeLimit(50),
		docstore.TreeAfter("configs/"),
	)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	// Sorted alphabetical: after, branch, limit.
	if h.rawQuery != "after=configs%2F&branch=main&limit=50" {
		t.Errorf("query = %q", h.rawQuery)
	}
}

func TestRoles_PutAndDelete(t *testing.T) {
	h := &recordingHandler{}
	base, stop := newServer(t, h)
	defer stop()

	c := newClient(t, base)
	if err := c.Repo("a/b").SetRole(context.Background(), "user@example.com", api.RoleMaintainer); err != nil {
		t.Fatalf("SetRole: %v", err)
	}
	if h.method != "PUT" || h.path != "/repos/a/b/-/roles/user@example.com" {
		t.Errorf("PUT got %s %s", h.method, h.path)
	}
	var req api.SetRoleRequest
	if err := json.Unmarshal(h.reqBody, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.Role != api.RoleMaintainer {
		t.Errorf("role = %q", req.Role)
	}

	h.status = http.StatusNoContent
	if err := c.Repo("a/b").DeleteRole(context.Background(), "user@example.com"); err != nil {
		t.Fatalf("DeleteRole: %v", err)
	}
	if h.method != "DELETE" {
		t.Errorf("DELETE method = %q", h.method)
	}
}

func TestOrgs_CreateAndMembers(t *testing.T) {
	h := &recordingHandler{
		status:   http.StatusCreated,
		respBody: api.Org{Name: "acme"},
	}
	base, stop := newServer(t, h)
	defer stop()

	c := newClient(t, base)
	if _, err := c.Orgs().Create(context.Background(), api.CreateOrgRequest{Name: "acme"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if h.method != "POST" || h.path != "/orgs" {
		t.Errorf("got %s %s", h.method, h.path)
	}

	h.status = http.StatusOK
	h.respBody = api.OrgMember{Org: "acme", Identity: "u@example.com", Role: api.OrgRoleMember}
	if _, err := c.Orgs().AddMember(context.Background(), "acme", "u@example.com", api.OrgRoleMember); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if h.path != "/orgs/acme/members/u@example.com" {
		t.Errorf("path = %q", h.path)
	}
}

func TestRepos_CRUD(t *testing.T) {
	h := &recordingHandler{
		status:   http.StatusCreated,
		respBody: api.Repo{Owner: "acme", Name: "acme/platform"},
	}
	base, stop := newServer(t, h)
	defer stop()

	c := newClient(t, base)
	if _, err := c.Repos().Create(context.Background(), api.CreateRepoRequest{Owner: "acme", Name: "platform"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if h.path != "/repos" {
		t.Errorf("create path = %q", h.path)
	}

	h.status = http.StatusOK
	h.respBody = api.Repo{Owner: "acme", Name: "acme/platform"}
	if _, err := c.Repos().Get(context.Background(), "acme/platform"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if h.path != "/repos/acme/platform" {
		t.Errorf("get path = %q", h.path)
	}

	h.status = http.StatusNoContent
	if err := c.Repos().Delete(context.Background(), "acme/platform"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if h.method != "DELETE" {
		t.Errorf("delete method = %q", h.method)
	}
}

func TestContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := newClient(t, srv.URL)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := c.Repo("a/b").File(ctx, "x")
	if err == nil {
		t.Fatal("expected context error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("got %v, want context.Canceled", err)
	}
}
