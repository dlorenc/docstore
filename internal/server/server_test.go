package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dlorenc/docstore/internal/model"
)

// --- Mock Store ---

type mockStore struct {
	createBranchFn func(ctx context.Context, name string) (*model.Branch, error)
	deleteBranchFn func(ctx context.Context, name string) error
	listBranchesFn func(ctx context.Context, status string) ([]model.Branch, error)
	mergeFn        func(ctx context.Context, branch string) (*model.MergeResponse, []model.ConflictEntry, error)
	rebaseFn       func(ctx context.Context, branch string) (*model.RebaseResponse, []model.ConflictEntry, error)
	diffFn         func(ctx context.Context, branch string) (*model.DiffResponse, error)
}

func (m *mockStore) CreateBranch(ctx context.Context, name string) (*model.Branch, error) {
	return m.createBranchFn(ctx, name)
}

func (m *mockStore) DeleteBranch(ctx context.Context, name string) error {
	return m.deleteBranchFn(ctx, name)
}

func (m *mockStore) ListBranches(ctx context.Context, status string) ([]model.Branch, error) {
	return m.listBranchesFn(ctx, status)
}

func (m *mockStore) Merge(ctx context.Context, branch string) (*model.MergeResponse, []model.ConflictEntry, error) {
	return m.mergeFn(ctx, branch)
}

func (m *mockStore) Rebase(ctx context.Context, branch string) (*model.RebaseResponse, []model.ConflictEntry, error) {
	return m.rebaseFn(ctx, branch)
}

func (m *mockStore) Diff(ctx context.Context, branch string) (*model.DiffResponse, error) {
	return m.diffFn(ctx, branch)
}

// --- Health ---

func TestHealthEndpoint(t *testing.T) {
	srv := New(nil)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("expected status ok, got %q", body["status"])
	}
}

func TestNotImplementedEndpoints(t *testing.T) {
	srv := New(nil)
	endpoints := []struct {
		method string
		path   string
	}{
		{"GET", "/tree"},
		{"POST", "/commit"},
		{"POST", "/review"},
		{"POST", "/check"},
	}

	for _, ep := range endpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			req := httptest.NewRequest(ep.method, ep.path, nil)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotImplemented {
				t.Fatalf("expected 501, got %d", rec.Code)
			}
		})
	}
}

// --- POST /branch ---

func TestCreateBranch(t *testing.T) {
	ms := &mockStore{
		createBranchFn: func(_ context.Context, name string) (*model.Branch, error) {
			return &model.Branch{
				Name:         name,
				HeadSequence: 5,
				BaseSequence: 5,
				Status:       model.BranchStatusActive,
			}, nil
		},
	}
	srv := New(ms)

	body := `{"name":"feature/test"}`
	req := httptest.NewRequest(http.MethodPost, "/branch", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp model.CreateBranchResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Name != "feature/test" {
		t.Fatalf("expected name feature/test, got %q", resp.Name)
	}
	if resp.BaseSequence != 5 {
		t.Fatalf("expected base_sequence 5, got %d", resp.BaseSequence)
	}
}

func TestCreateBranch_MissingName(t *testing.T) {
	srv := New(&mockStore{})
	body := `{}`
	req := httptest.NewRequest(http.MethodPost, "/branch", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestCreateBranch_MainForbidden(t *testing.T) {
	srv := New(&mockStore{})
	body := `{"name":"main"}`
	req := httptest.NewRequest(http.MethodPost, "/branch", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestCreateBranch_AlreadyExists(t *testing.T) {
	ms := &mockStore{
		createBranchFn: func(_ context.Context, _ string) (*model.Branch, error) {
			return nil, model.ErrBranchExists
		},
	}
	srv := New(ms)

	body := `{"name":"feature/exists"}`
	req := httptest.NewRequest(http.MethodPost, "/branch", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rec.Code)
	}
}

func TestCreateBranch_InvalidJSON(t *testing.T) {
	srv := New(&mockStore{})
	req := httptest.NewRequest(http.MethodPost, "/branch", bytes.NewBufferString("not json"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

// --- DELETE /branch/{name} ---

func TestDeleteBranch(t *testing.T) {
	ms := &mockStore{
		deleteBranchFn: func(_ context.Context, _ string) error {
			return nil
		},
	}
	srv := New(ms)

	req := httptest.NewRequest(http.MethodDelete, "/branch/feature/old", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp model.DeleteBranchResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != model.BranchStatusAbandoned {
		t.Fatalf("expected status abandoned, got %q", resp.Status)
	}
}

func TestDeleteBranch_MainForbidden(t *testing.T) {
	srv := New(&mockStore{})
	req := httptest.NewRequest(http.MethodDelete, "/branch/main", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestDeleteBranch_NotFound(t *testing.T) {
	ms := &mockStore{
		deleteBranchFn: func(_ context.Context, _ string) error {
			return model.ErrBranchNotFound
		},
	}
	srv := New(ms)

	req := httptest.NewRequest(http.MethodDelete, "/branch/nonexistent", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestDeleteBranch_NotActive(t *testing.T) {
	ms := &mockStore{
		deleteBranchFn: func(_ context.Context, _ string) error {
			return model.ErrBranchNotActive
		},
	}
	srv := New(ms)

	req := httptest.NewRequest(http.MethodDelete, "/branch/already-merged", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

// --- GET /branches ---

func TestListBranches(t *testing.T) {
	ms := &mockStore{
		listBranchesFn: func(_ context.Context, status string) ([]model.Branch, error) {
			branches := []model.Branch{
				{Name: "main", HeadSequence: 10, BaseSequence: 0, Status: model.BranchStatusActive},
				{Name: "feature/a", HeadSequence: 5, BaseSequence: 3, Status: model.BranchStatusActive},
			}
			if status == "active" {
				return branches, nil
			}
			return append(branches, model.Branch{
				Name: "feature/old", HeadSequence: 2, BaseSequence: 1, Status: model.BranchStatusMerged,
			}), nil
		},
	}
	srv := New(ms)

	// Without filter.
	req := httptest.NewRequest(http.MethodGet, "/branches", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp model.BranchesResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Branches) != 3 {
		t.Fatalf("expected 3 branches, got %d", len(resp.Branches))
	}

	// With status filter.
	req = httptest.NewRequest(http.MethodGet, "/branches?status=active", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Branches) != 2 {
		t.Fatalf("expected 2 active branches, got %d", len(resp.Branches))
	}
}

// --- POST /merge ---

func TestMerge_Success(t *testing.T) {
	ms := &mockStore{
		mergeFn: func(_ context.Context, _ string) (*model.MergeResponse, []model.ConflictEntry, error) {
			return &model.MergeResponse{Sequence: 42}, nil, nil
		},
	}
	srv := New(ms)

	body := `{"branch":"feature/done"}`
	req := httptest.NewRequest(http.MethodPost, "/merge", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp model.MergeResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Sequence != 42 {
		t.Fatalf("expected sequence 42, got %d", resp.Sequence)
	}
}

func TestMerge_Conflict(t *testing.T) {
	ms := &mockStore{
		mergeFn: func(_ context.Context, _ string) (*model.MergeResponse, []model.ConflictEntry, error) {
			return nil, []model.ConflictEntry{
				{Path: "src/main.go", MainVersionID: "v1", BranchVersionID: "v2"},
			}, nil
		},
	}
	srv := New(ms)

	body := `{"branch":"feature/conflicting"}`
	req := httptest.NewRequest(http.MethodPost, "/merge", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp model.MergeConflictError
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(resp.Conflicts))
	}
	if resp.Conflicts[0].Path != "src/main.go" {
		t.Fatalf("expected conflict on src/main.go, got %q", resp.Conflicts[0].Path)
	}
}

func TestMerge_BranchNotFound(t *testing.T) {
	ms := &mockStore{
		mergeFn: func(_ context.Context, _ string) (*model.MergeResponse, []model.ConflictEntry, error) {
			return nil, nil, model.ErrBranchNotFound
		},
	}
	srv := New(ms)

	body := `{"branch":"feature/gone"}`
	req := httptest.NewRequest(http.MethodPost, "/merge", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestMerge_MissingBranch(t *testing.T) {
	srv := New(&mockStore{})
	body := `{}`
	req := httptest.NewRequest(http.MethodPost, "/merge", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestMerge_MainForbidden(t *testing.T) {
	srv := New(&mockStore{})
	body := `{"branch":"main"}`
	req := httptest.NewRequest(http.MethodPost, "/merge", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestMerge_NotActive(t *testing.T) {
	ms := &mockStore{
		mergeFn: func(_ context.Context, _ string) (*model.MergeResponse, []model.ConflictEntry, error) {
			return nil, nil, model.ErrBranchNotActive
		},
	}
	srv := New(ms)

	body := `{"branch":"feature/merged"}`
	req := httptest.NewRequest(http.MethodPost, "/merge", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

// --- POST /rebase ---

func TestRebase_Success(t *testing.T) {
	ms := &mockStore{
		rebaseFn: func(_ context.Context, _ string) (*model.RebaseResponse, []model.ConflictEntry, error) {
			return &model.RebaseResponse{NewBaseSequence: 10, NewHeadSequence: 12}, nil, nil
		},
	}
	srv := New(ms)

	body := `{"branch":"feature/update"}`
	req := httptest.NewRequest(http.MethodPost, "/rebase", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp model.RebaseResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.NewBaseSequence != 10 {
		t.Fatalf("expected new_base_sequence 10, got %d", resp.NewBaseSequence)
	}
	if resp.NewHeadSequence != 12 {
		t.Fatalf("expected new_head_sequence 12, got %d", resp.NewHeadSequence)
	}
}

func TestRebase_Conflict(t *testing.T) {
	ms := &mockStore{
		rebaseFn: func(_ context.Context, _ string) (*model.RebaseResponse, []model.ConflictEntry, error) {
			return nil, []model.ConflictEntry{
				{Path: "config.yaml", MainVersionID: "v3", BranchVersionID: "v4"},
			}, nil
		},
	}
	srv := New(ms)

	body := `{"branch":"feature/conflicting"}`
	req := httptest.NewRequest(http.MethodPost, "/rebase", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp model.RebaseConflictError
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(resp.Conflicts))
	}
}

func TestRebase_MissingBranch(t *testing.T) {
	srv := New(&mockStore{})
	body := `{}`
	req := httptest.NewRequest(http.MethodPost, "/rebase", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestRebase_MainForbidden(t *testing.T) {
	srv := New(&mockStore{})
	body := `{"branch":"main"}`
	req := httptest.NewRequest(http.MethodPost, "/rebase", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestRebase_NotFound(t *testing.T) {
	ms := &mockStore{
		rebaseFn: func(_ context.Context, _ string) (*model.RebaseResponse, []model.ConflictEntry, error) {
			return nil, nil, model.ErrBranchNotFound
		},
	}
	srv := New(ms)

	body := `{"branch":"feature/gone"}`
	req := httptest.NewRequest(http.MethodPost, "/rebase", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// --- GET /diff ---

func TestDiff_Success(t *testing.T) {
	vid := "version-123"
	ms := &mockStore{
		diffFn: func(_ context.Context, branch string) (*model.DiffResponse, error) {
			return &model.DiffResponse{
				Changed: []model.DiffEntry{
					{Path: "src/new.go", VersionID: &vid},
				},
				Conflicts: []model.ConflictEntry{},
			}, nil
		},
	}
	srv := New(ms)

	req := httptest.NewRequest(http.MethodGet, "/diff?branch=feature/x", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp model.DiffResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Changed) != 1 {
		t.Fatalf("expected 1 changed file, got %d", len(resp.Changed))
	}
	if resp.Changed[0].Path != "src/new.go" {
		t.Fatalf("expected changed path src/new.go, got %q", resp.Changed[0].Path)
	}
}

func TestDiff_WithConflicts(t *testing.T) {
	ms := &mockStore{
		diffFn: func(_ context.Context, _ string) (*model.DiffResponse, error) {
			vid := "v1"
			return &model.DiffResponse{
				Changed: []model.DiffEntry{
					{Path: "shared.go", VersionID: &vid},
				},
				Conflicts: []model.ConflictEntry{
					{Path: "shared.go", MainVersionID: "v2", BranchVersionID: "v1"},
				},
			}, nil
		},
	}
	srv := New(ms)

	req := httptest.NewRequest(http.MethodGet, "/diff?branch=feature/conflict", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp model.DiffResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(resp.Conflicts))
	}
}

func TestDiff_MissingBranch(t *testing.T) {
	srv := New(&mockStore{})
	req := httptest.NewRequest(http.MethodGet, "/diff", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestDiff_NotFound(t *testing.T) {
	ms := &mockStore{
		diffFn: func(_ context.Context, _ string) (*model.DiffResponse, error) {
			return nil, model.ErrBranchNotFound
		},
	}
	srv := New(ms)

	req := httptest.NewRequest(http.MethodGet, "/diff?branch=nonexistent", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}
