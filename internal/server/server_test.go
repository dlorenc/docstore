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

// mockStore implements CommitStore for testing.
type mockStore struct {
	commitFn func(ctx context.Context, req model.CommitRequest) (*model.CommitResponse, error)
}

func (m *mockStore) Commit(ctx context.Context, req model.CommitRequest) (*model.CommitResponse, error) {
	return m.commitFn(ctx, req)
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
		{"GET", "/diff"},
		{"GET", "/branches"},
		{"POST", "/branch"},
		{"POST", "/merge"},
		{"POST", "/rebase"},
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
