package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/model"
)

// mockStore implements WriteStore for testing.
type mockStore struct {
	commitFn        func(ctx context.Context, req model.CommitRequest) (*model.CommitResponse, error)
	createBranchFn  func(ctx context.Context, req model.CreateBranchRequest) (*model.CreateBranchResponse, error)
	mergeFn         func(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error)
	deleteBranchFn  func(ctx context.Context, name string) error
}

func (m *mockStore) Commit(ctx context.Context, req model.CommitRequest) (*model.CommitResponse, error) {
	return m.commitFn(ctx, req)
}

func (m *mockStore) CreateBranch(ctx context.Context, req model.CreateBranchRequest) (*model.CreateBranchResponse, error) {
	if m.createBranchFn != nil {
		return m.createBranchFn(ctx, req)
	}
	return nil, errors.New("not implemented")
}

func (m *mockStore) Merge(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error) {
	if m.mergeFn != nil {
		return m.mergeFn(ctx, req)
	}
	return nil, nil, errors.New("not implemented")
}

func (m *mockStore) DeleteBranch(ctx context.Context, name string) error {
	if m.deleteBranchFn != nil {
		return m.deleteBranchFn(ctx, name)
	}
	return errors.New("not implemented")
}

func TestHealthEndpoint(t *testing.T) {
	srv := New(nil, nil)

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
	srv := New(nil, nil)

	endpoints := []struct {
		method string
		path   string
	}{
		{"POST", "/rebase"},
		{"POST", "/review"},
		{"POST", "/check"},
		{"GET", "/branch/main/status"},
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

func TestHandleCommit_Success(t *testing.T) {
	vid := "abc-123"
	store := &mockStore{
		commitFn: func(ctx context.Context, req model.CommitRequest) (*model.CommitResponse, error) {
			if req.Branch != "main" {
				t.Errorf("expected branch main, got %q", req.Branch)
			}
			if len(req.Files) != 1 {
				t.Errorf("expected 1 file, got %d", len(req.Files))
			}
			return &model.CommitResponse{
				Sequence: 1,
				Files:    []model.CommitFileResult{{Path: "hello.txt", VersionID: &vid}},
			}, nil
		},
	}
	srv := New(store, nil)

	body, _ := json.Marshal(model.CommitRequest{
		Branch:  "main",
		Files:   []model.FileChange{{Path: "hello.txt", Content: []byte("hello")}},
		Message: "initial commit",
		Author:  "alice@example.com",
	})

	req := httptest.NewRequest(http.MethodPost, "/commit", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp model.CommitResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if resp.Sequence != 1 {
		t.Errorf("expected sequence 1, got %d", resp.Sequence)
	}
	if len(resp.Files) != 1 || resp.Files[0].Path != "hello.txt" {
		t.Errorf("unexpected files: %+v", resp.Files)
	}
}

func TestHandleCommit_ValidationErrors(t *testing.T) {
	srv := New(&mockStore{
		commitFn: func(ctx context.Context, req model.CommitRequest) (*model.CommitResponse, error) {
			t.Fatal("store should not be called on validation error")
			return nil, nil
		},
	}, nil)

	tests := []struct {
		name string
		body model.CommitRequest
	}{
		{"missing branch", model.CommitRequest{Files: []model.FileChange{{Path: "a.txt", Content: []byte("x")}}, Message: "m", Author: "a"}},
		{"missing files", model.CommitRequest{Branch: "main", Message: "m", Author: "a"}},
		{"missing message", model.CommitRequest{Branch: "main", Files: []model.FileChange{{Path: "a.txt", Content: []byte("x")}}, Author: "a"}},
		{"missing author", model.CommitRequest{Branch: "main", Files: []model.FileChange{{Path: "a.txt", Content: []byte("x")}}, Message: "m"}},
		{"empty file path", model.CommitRequest{Branch: "main", Files: []model.FileChange{{Path: "", Content: []byte("x")}}, Message: "m", Author: "a"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _ := json.Marshal(tt.body)
			req := httptest.NewRequest(http.MethodPost, "/commit", bytes.NewReader(b))
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHandleCommit_BranchNotFound(t *testing.T) {
	store := &mockStore{
		commitFn: func(ctx context.Context, req model.CommitRequest) (*model.CommitResponse, error) {
			return nil, db.ErrBranchNotFound
		},
	}
	srv := New(store, nil)

	body, _ := json.Marshal(model.CommitRequest{
		Branch:  "nonexistent",
		Files:   []model.FileChange{{Path: "a.txt", Content: []byte("x")}},
		Message: "m",
		Author:  "a",
	})

	req := httptest.NewRequest(http.MethodPost, "/commit", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestHandleCommit_BranchNotActive(t *testing.T) {
	store := &mockStore{
		commitFn: func(ctx context.Context, req model.CommitRequest) (*model.CommitResponse, error) {
			return nil, db.ErrBranchNotActive
		},
	}
	srv := New(store, nil)

	body, _ := json.Marshal(model.CommitRequest{
		Branch:  "merged-branch",
		Files:   []model.FileChange{{Path: "a.txt", Content: []byte("x")}},
		Message: "m",
		Author:  "a",
	})

	req := httptest.NewRequest(http.MethodPost, "/commit", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rec.Code)
	}
}

func TestHandleCommit_InternalError(t *testing.T) {
	store := &mockStore{
		commitFn: func(ctx context.Context, req model.CommitRequest) (*model.CommitResponse, error) {
			return nil, errors.New("something went wrong")
		},
	}
	srv := New(store, nil)

	body, _ := json.Marshal(model.CommitRequest{
		Branch:  "main",
		Files:   []model.FileChange{{Path: "a.txt", Content: []byte("x")}},
		Message: "m",
		Author:  "a",
	})

	req := httptest.NewRequest(http.MethodPost, "/commit", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
}

func TestHandleCommit_InvalidJSON(t *testing.T) {
	srv := New(&mockStore{
		commitFn: func(ctx context.Context, req model.CommitRequest) (*model.CommitResponse, error) {
			t.Fatal("store should not be called on invalid JSON")
			return nil, nil
		},
	}, nil)

	req := httptest.NewRequest(http.MethodPost, "/commit", bytes.NewReader([]byte("not json")))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

// --- POST /branch tests ---

func TestHandleCreateBranch_Success(t *testing.T) {
	store := &mockStore{
		createBranchFn: func(ctx context.Context, req model.CreateBranchRequest) (*model.CreateBranchResponse, error) {
			return &model.CreateBranchResponse{Name: req.Name, BaseSequence: 5}, nil
		},
	}
	srv := New(store, nil)

	body, _ := json.Marshal(model.CreateBranchRequest{Name: "feature/test"})
	req := httptest.NewRequest(http.MethodPost, "/branch", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp model.CreateBranchResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Name != "feature/test" {
		t.Errorf("expected name feature/test, got %q", resp.Name)
	}
	if resp.BaseSequence != 5 {
		t.Errorf("expected base_sequence 5, got %d", resp.BaseSequence)
	}
}

func TestHandleCreateBranch_MissingName(t *testing.T) {
	srv := New(&mockStore{}, nil)

	body, _ := json.Marshal(model.CreateBranchRequest{Name: ""})
	req := httptest.NewRequest(http.MethodPost, "/branch", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandleCreateBranch_CannotCreateMain(t *testing.T) {
	srv := New(&mockStore{}, nil)

	body, _ := json.Marshal(model.CreateBranchRequest{Name: "main"})
	req := httptest.NewRequest(http.MethodPost, "/branch", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandleCreateBranch_AlreadyExists(t *testing.T) {
	store := &mockStore{
		createBranchFn: func(ctx context.Context, req model.CreateBranchRequest) (*model.CreateBranchResponse, error) {
			return nil, db.ErrBranchExists
		},
	}
	srv := New(store, nil)

	body, _ := json.Marshal(model.CreateBranchRequest{Name: "feature/exists"})
	req := httptest.NewRequest(http.MethodPost, "/branch", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rec.Code)
	}
}

// --- POST /merge tests ---

func TestHandleMerge_Success(t *testing.T) {
	store := &mockStore{
		mergeFn: func(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error) {
			return &model.MergeResponse{Sequence: 10}, nil, nil
		},
	}
	srv := New(store, nil)

	body, _ := json.Marshal(model.MergeRequest{Branch: "feature/test"})
	req := httptest.NewRequest(http.MethodPost, "/merge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp model.MergeResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Sequence != 10 {
		t.Errorf("expected sequence 10, got %d", resp.Sequence)
	}
}

func TestHandleMerge_MissingBranch(t *testing.T) {
	srv := New(&mockStore{}, nil)

	body, _ := json.Marshal(model.MergeRequest{Branch: ""})
	req := httptest.NewRequest(http.MethodPost, "/merge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandleMerge_Conflict(t *testing.T) {
	store := &mockStore{
		mergeFn: func(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error) {
			return nil, []db.MergeConflict{
				{Path: "conflict.txt", MainVersionID: "v1", BranchVersionID: "v2"},
			}, db.ErrMergeConflict
		},
	}
	srv := New(store, nil)

	body, _ := json.Marshal(model.MergeRequest{Branch: "feature/conflict"})
	req := httptest.NewRequest(http.MethodPost, "/merge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var errResp model.MergeConflictError
	if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(errResp.Conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(errResp.Conflicts))
	}
	if errResp.Conflicts[0].Path != "conflict.txt" {
		t.Errorf("expected conflict.txt, got %q", errResp.Conflicts[0].Path)
	}
}

func TestHandleMerge_BranchNotFound(t *testing.T) {
	store := &mockStore{
		mergeFn: func(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error) {
			return nil, nil, db.ErrBranchNotFound
		},
	}
	srv := New(store, nil)

	body, _ := json.Marshal(model.MergeRequest{Branch: "nonexistent"})
	req := httptest.NewRequest(http.MethodPost, "/merge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestHandleMerge_CannotMergeMain(t *testing.T) {
	srv := New(&mockStore{}, nil)

	body, _ := json.Marshal(model.MergeRequest{Branch: "main"})
	req := httptest.NewRequest(http.MethodPost, "/merge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleMerge_AuthorFromHeader(t *testing.T) {
	var capturedAuthor string
	store := &mockStore{
		mergeFn: func(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error) {
			capturedAuthor = req.Author
			return &model.MergeResponse{Sequence: 1}, nil, nil
		},
	}
	srv := New(store, nil)

	body, _ := json.Marshal(model.MergeRequest{Branch: "feature/test"})
	req := httptest.NewRequest(http.MethodPost, "/merge", bytes.NewReader(body))
	req.Header.Set("X-DocStore-Identity", "alice@example.com")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if capturedAuthor != "alice@example.com" {
		t.Errorf("expected author alice@example.com, got %q", capturedAuthor)
	}
}

func TestHandleMerge_AuthorFromBody(t *testing.T) {
	var capturedAuthor string
	store := &mockStore{
		mergeFn: func(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error) {
			capturedAuthor = req.Author
			return &model.MergeResponse{Sequence: 1}, nil, nil
		},
	}
	srv := New(store, nil)

	body, _ := json.Marshal(model.MergeRequest{Branch: "feature/test", Author: "bob@example.com"})
	req := httptest.NewRequest(http.MethodPost, "/merge", bytes.NewReader(body))
	req.Header.Set("X-DocStore-Identity", "alice@example.com")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	// Body author takes precedence over header.
	if capturedAuthor != "bob@example.com" {
		t.Errorf("expected author bob@example.com, got %q", capturedAuthor)
	}
}

func TestHandleMerge_DefaultAuthorSystem(t *testing.T) {
	var capturedAuthor string
	store := &mockStore{
		mergeFn: func(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error) {
			capturedAuthor = req.Author
			return &model.MergeResponse{Sequence: 1}, nil, nil
		},
	}
	srv := New(store, nil)

	body, _ := json.Marshal(model.MergeRequest{Branch: "feature/test"})
	req := httptest.NewRequest(http.MethodPost, "/merge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if capturedAuthor != "system" {
		t.Errorf("expected default author 'system', got %q", capturedAuthor)
	}
}

// --- DELETE /branch/:name tests ---

func TestHandleDeleteBranch_Success(t *testing.T) {
	store := &mockStore{
		deleteBranchFn: func(ctx context.Context, name string) error {
			if name != "feature/delete-me" {
				t.Errorf("expected name feature/delete-me, got %q", name)
			}
			return nil
		},
	}
	srv := New(store, nil)

	req := httptest.NewRequest(http.MethodDelete, "/branch/feature/delete-me", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleDeleteBranch_CannotDeleteMain(t *testing.T) {
	srv := New(&mockStore{}, nil)

	req := httptest.NewRequest(http.MethodDelete, "/branch/main", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandleDeleteBranch_NotFound(t *testing.T) {
	store := &mockStore{
		deleteBranchFn: func(ctx context.Context, name string) error {
			return db.ErrBranchNotFound
		},
	}
	srv := New(store, nil)

	req := httptest.NewRequest(http.MethodDelete, "/branch/nonexistent", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestHandleDeleteBranch_AlreadyMerged(t *testing.T) {
	store := &mockStore{
		deleteBranchFn: func(ctx context.Context, name string) error {
			return db.ErrBranchNotActive
		},
	}
	srv := New(store, nil)

	req := httptest.NewRequest(http.MethodDelete, "/branch/merged-branch", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rec.Code)
	}
}

func TestHandleDeleteBranch_AlreadyAbandoned(t *testing.T) {
	store := &mockStore{
		deleteBranchFn: func(ctx context.Context, name string) error {
			return db.ErrBranchNotActive
		},
	}
	srv := New(store, nil)

	req := httptest.NewRequest(http.MethodDelete, "/branch/abandoned-branch", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rec.Code)
	}
}
