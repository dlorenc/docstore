package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/model"
)

// mockStore implements WriteStore for testing.
type mockStore struct {
	commitFn        func(ctx context.Context, req model.CommitRequest) (*model.CommitResponse, error)
	createBranchFn  func(ctx context.Context, req model.CreateBranchRequest) (*model.CreateBranchResponse, error)
	mergeFn         func(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error)
	deleteBranchFn  func(ctx context.Context, repo, name string) error
	rebaseFn        func(ctx context.Context, req model.RebaseRequest) (*model.RebaseResponse, []db.MergeConflict, error)
	createRepoFn    func(ctx context.Context, req model.CreateRepoRequest) (*model.Repo, error)
	deleteRepoFn    func(ctx context.Context, name string) error
	listReposFn     func(ctx context.Context) ([]model.Repo, error)
	getRepoFn       func(ctx context.Context, name string) (*model.Repo, error)
	createReviewFn  func(ctx context.Context, repo, branch, reviewer string, status model.ReviewStatus, body string) (*model.Review, error)
	listReviewsFn   func(ctx context.Context, repo, branch string, atSeq *int64) ([]model.Review, error)
	createCheckRunFn func(ctx context.Context, repo, branch, checkName string, status model.CheckRunStatus, reporter string) (*model.CheckRun, error)
	listCheckRunsFn  func(ctx context.Context, repo, branch string, atSeq *int64) ([]model.CheckRun, error)
	getRoleFn      func(ctx context.Context, repo, identity string) (*model.Role, error)
	setRoleFn      func(ctx context.Context, repo, identity string, role model.RoleType) error
	deleteRoleFn   func(ctx context.Context, repo, identity string) error
	listRolesFn    func(ctx context.Context, repo string) ([]model.Role, error)
	hasAdminFn     func(ctx context.Context, repo string) (bool, error)
	purgeFn        func(ctx context.Context, req db.PurgeRequest) (*db.PurgeResult, error)
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

func (m *mockStore) DeleteBranch(ctx context.Context, repo, name string) error {
	if m.deleteBranchFn != nil {
		return m.deleteBranchFn(ctx, repo, name)
	}
	return errors.New("not implemented")
}

func (m *mockStore) Rebase(ctx context.Context, req model.RebaseRequest) (*model.RebaseResponse, []db.MergeConflict, error) {
	if m.rebaseFn != nil {
		return m.rebaseFn(ctx, req)
	}
	return nil, nil, errors.New("not implemented")
}

// devID is the dev identity used in handler unit tests.
const devID = "test@example.com"

func (m *mockStore) CreateRepo(ctx context.Context, req model.CreateRepoRequest) (*model.Repo, error) {
	if m.createRepoFn != nil {
		return m.createRepoFn(ctx, req)
	}
	return nil, errors.New("not implemented")
}

func (m *mockStore) DeleteRepo(ctx context.Context, name string) error {
	if m.deleteRepoFn != nil {
		return m.deleteRepoFn(ctx, name)
	}
	return errors.New("not implemented")
}

func (m *mockStore) ListRepos(ctx context.Context) ([]model.Repo, error) {
	if m.listReposFn != nil {
		return m.listReposFn(ctx)
	}
	return nil, errors.New("not implemented")
}

func (m *mockStore) GetRepo(ctx context.Context, name string) (*model.Repo, error) {
	if m.getRepoFn != nil {
		return m.getRepoFn(ctx, name)
	}
	return &model.Repo{Name: name}, nil
}

func (m *mockStore) CreateReview(ctx context.Context, repo, branch, reviewer string, status model.ReviewStatus, body string) (*model.Review, error) {
	if m.createReviewFn != nil {
		return m.createReviewFn(ctx, repo, branch, reviewer, status, body)
	}
	return nil, errors.New("not implemented")
}

func (m *mockStore) ListReviews(ctx context.Context, repo, branch string, atSeq *int64) ([]model.Review, error) {
	if m.listReviewsFn != nil {
		return m.listReviewsFn(ctx, repo, branch, atSeq)
	}
	return nil, errors.New("not implemented")
}

func (m *mockStore) CreateCheckRun(ctx context.Context, repo, branch, checkName string, status model.CheckRunStatus, reporter string) (*model.CheckRun, error) {
	if m.createCheckRunFn != nil {
		return m.createCheckRunFn(ctx, repo, branch, checkName, status, reporter)
	}
	return nil, errors.New("not implemented")
}

func (m *mockStore) ListCheckRuns(ctx context.Context, repo, branch string, atSeq *int64) ([]model.CheckRun, error) {
	if m.listCheckRunsFn != nil {
		return m.listCheckRunsFn(ctx, repo, branch, atSeq)
	}
	return nil, errors.New("not implemented")
}

func (m *mockStore) GetRole(ctx context.Context, repo, identity string) (*model.Role, error) {
	if m.getRoleFn != nil {
		return m.getRoleFn(ctx, repo, identity)
	}
	return nil, db.ErrRoleNotFound
}

func (m *mockStore) SetRole(ctx context.Context, repo, identity string, role model.RoleType) error {
	if m.setRoleFn != nil {
		return m.setRoleFn(ctx, repo, identity, role)
	}
	return nil
}

func (m *mockStore) DeleteRole(ctx context.Context, repo, identity string) error {
	if m.deleteRoleFn != nil {
		return m.deleteRoleFn(ctx, repo, identity)
	}
	return db.ErrRoleNotFound
}

func (m *mockStore) ListRoles(ctx context.Context, repo string) ([]model.Role, error) {
	if m.listRolesFn != nil {
		return m.listRolesFn(ctx, repo)
	}
	return []model.Role{}, nil
}

func (m *mockStore) HasAdmin(ctx context.Context, repo string) (bool, error) {
	if m.hasAdminFn != nil {
		return m.hasAdminFn(ctx, repo)
	}
	return false, nil
}

func (m *mockStore) Purge(ctx context.Context, req db.PurgeRequest) (*db.PurgeResult, error) {
	if m.purgeFn != nil {
		return m.purgeFn(ctx, req)
	}
	return nil, errors.New("not implemented")
}

func TestHealthEndpoint(t *testing.T) {
	// /healthz is exempt from IAP auth, so devIdentity="" is fine here.
	srv := New(nil, nil, "", "")

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
	srv := New(nil, nil, devID, "")

	endpoints := []struct {
		method string
		path   string
	}{
		{"GET", "/repos/default/branch/main/status"},
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
			if req.Author != devID {
				t.Errorf("expected author %q from context, got %q", devID, req.Author)
			}
			return &model.CommitResponse{
				Sequence: 1,
				Files:    []model.CommitFileResult{{Path: "hello.txt", VersionID: &vid}},
			}, nil
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.CommitRequest{
		Branch:  "main",
		Files:   []model.FileChange{{Path: "hello.txt", Content: []byte("hello")}},
		Message: "initial commit",
	})

	req := httptest.NewRequest(http.MethodPost, "/repos/default/commit", bytes.NewReader(body))
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
	}, nil, devID, devID)

	tests := []struct {
		name string
		body model.CommitRequest
	}{
		{"missing branch", model.CommitRequest{Files: []model.FileChange{{Path: "a.txt", Content: []byte("x")}}, Message: "m"}},
		{"missing files", model.CommitRequest{Branch: "main", Message: "m"}},
		{"missing message", model.CommitRequest{Branch: "main", Files: []model.FileChange{{Path: "a.txt", Content: []byte("x")}}}},
		{"empty file path", model.CommitRequest{Branch: "main", Files: []model.FileChange{{Path: "", Content: []byte("x")}}, Message: "m"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _ := json.Marshal(tt.body)
			req := httptest.NewRequest(http.MethodPost, "/repos/default/commit", bytes.NewReader(b))
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
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.CommitRequest{
		Branch:  "nonexistent",
		Files:   []model.FileChange{{Path: "a.txt", Content: []byte("x")}},
		Message: "m",
	})

	req := httptest.NewRequest(http.MethodPost, "/repos/default/commit", bytes.NewReader(body))
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
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.CommitRequest{
		Branch:  "merged-branch",
		Files:   []model.FileChange{{Path: "a.txt", Content: []byte("x")}},
		Message: "m",
	})

	req := httptest.NewRequest(http.MethodPost, "/repos/default/commit", bytes.NewReader(body))
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
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.CommitRequest{
		Branch:  "main",
		Files:   []model.FileChange{{Path: "a.txt", Content: []byte("x")}},
		Message: "m",
	})

	req := httptest.NewRequest(http.MethodPost, "/repos/default/commit", bytes.NewReader(body))
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
	}, nil, devID, devID)

	req := httptest.NewRequest(http.MethodPost, "/repos/default/commit", bytes.NewReader([]byte("not json")))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

// --- POST /repos/default/branch tests ---

func TestHandleCreateBranch_Success(t *testing.T) {
	store := &mockStore{
		createBranchFn: func(ctx context.Context, req model.CreateBranchRequest) (*model.CreateBranchResponse, error) {
			return &model.CreateBranchResponse{Name: req.Name, BaseSequence: 5}, nil
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.CreateBranchRequest{Name: "feature/test"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/branch", bytes.NewReader(body))
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
	srv := New(&mockStore{}, nil, devID, devID)

	body, _ := json.Marshal(model.CreateBranchRequest{Name: ""})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/branch", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandleCreateBranch_CannotCreateMain(t *testing.T) {
	srv := New(&mockStore{}, nil, devID, devID)

	body, _ := json.Marshal(model.CreateBranchRequest{Name: "main"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/branch", bytes.NewReader(body))
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
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.CreateBranchRequest{Name: "feature/exists"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/branch", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rec.Code)
	}
}

func TestHandleCreateBranch_RepoNotFound(t *testing.T) {
	store := &mockStore{
		createBranchFn: func(ctx context.Context, req model.CreateBranchRequest) (*model.CreateBranchResponse, error) {
			return nil, db.ErrRepoNotFound
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.CreateBranchRequest{Name: "feature/x"})
	req := httptest.NewRequest(http.MethodPost, "/repos/nonexistent/branch", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// --- validateRepo / nonexistent repo tests ---

func TestHandleTree_RepoNotFound(t *testing.T) {
	store := &mockStore{
		getRepoFn: func(ctx context.Context, name string) (*model.Repo, error) {
			return nil, db.ErrRepoNotFound
		},
	}
	srv := New(store, nil, devID, devID)

	req := httptest.NewRequest(http.MethodGet, "/repos/ghost/tree?branch=main", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestHandleBranches_RepoNotFound(t *testing.T) {
	store := &mockStore{
		getRepoFn: func(ctx context.Context, name string) (*model.Repo, error) {
			return nil, db.ErrRepoNotFound
		},
	}
	srv := New(store, nil, devID, devID)

	req := httptest.NewRequest(http.MethodGet, "/repos/ghost/branches", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestHandleDiff_RepoNotFound(t *testing.T) {
	store := &mockStore{
		getRepoFn: func(ctx context.Context, name string) (*model.Repo, error) {
			return nil, db.ErrRepoNotFound
		},
	}
	srv := New(store, nil, devID, devID)

	req := httptest.NewRequest(http.MethodGet, "/repos/ghost/diff?branch=main", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestHandleFile_RepoNotFound(t *testing.T) {
	store := &mockStore{
		getRepoFn: func(ctx context.Context, name string) (*model.Repo, error) {
			return nil, db.ErrRepoNotFound
		},
	}
	srv := New(store, nil, devID, devID)

	req := httptest.NewRequest(http.MethodGet, "/repos/ghost/file/foo.txt", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// --- POST /repos/default/merge tests ---

func TestHandleMerge_Success(t *testing.T) {
	store := &mockStore{
		mergeFn: func(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error) {
			return &model.MergeResponse{Sequence: 10}, nil, nil
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.MergeRequest{Branch: "feature/test"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/merge", bytes.NewReader(body))
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
	srv := New(&mockStore{}, nil, devID, devID)

	body, _ := json.Marshal(model.MergeRequest{Branch: ""})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/merge", bytes.NewReader(body))
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
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.MergeRequest{Branch: "feature/conflict"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/merge", bytes.NewReader(body))
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
	if errResp.Conflicts[0].MainVersionID != "v1" {
		t.Errorf("expected main_version_id v1, got %q", errResp.Conflicts[0].MainVersionID)
	}
	if errResp.Conflicts[0].BranchVersionID != "v2" {
		t.Errorf("expected branch_version_id v2, got %q", errResp.Conflicts[0].BranchVersionID)
	}
}

func TestHandleMerge_BranchNotFound(t *testing.T) {
	store := &mockStore{
		mergeFn: func(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error) {
			return nil, nil, db.ErrBranchNotFound
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.MergeRequest{Branch: "nonexistent"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/merge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestHandleMerge_BranchNotActive(t *testing.T) {
	store := &mockStore{
		mergeFn: func(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error) {
			return nil, nil, db.ErrBranchNotActive
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.MergeRequest{Branch: "merged-branch"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/merge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleMerge_CannotMergeMain(t *testing.T) {
	srv := New(&mockStore{}, nil, devID, devID)

	body, _ := json.Marshal(model.MergeRequest{Branch: "main"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/merge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleMerge_AuthorFromIdentity verifies that the authenticated identity
// is always used as the merge author, regardless of what is in the request body.
func TestHandleMerge_AuthorFromIdentity(t *testing.T) {
	const identity = "alice@example.com"
	var capturedAuthor string
	store := &mockStore{
		mergeFn: func(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error) {
			capturedAuthor = req.Author
			return &model.MergeResponse{Sequence: 1}, nil, nil
		},
	}
	srv := New(store, nil, identity, identity)

	// Send a different author in the body — it must be overridden by the identity.
	body, _ := json.Marshal(model.MergeRequest{Branch: "feature/test", Author: "ignored@example.com"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/merge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if capturedAuthor != identity {
		t.Errorf("expected author %q from identity, got %q", identity, capturedAuthor)
	}
}

// --- DELETE /repos/default/branch/:name tests ---

func TestHandleDeleteBranch_Success(t *testing.T) {
	store := &mockStore{
		deleteBranchFn: func(ctx context.Context, repo, name string) error {
			if name != "feature/delete-me" {
				t.Errorf("expected name feature/delete-me, got %q", name)
			}
			return nil
		},
	}
	srv := New(store, nil, devID, devID)

	req := httptest.NewRequest(http.MethodDelete, "/repos/default/branch/feature/delete-me", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleDeleteBranch_CannotDeleteMain(t *testing.T) {
	srv := New(&mockStore{}, nil, devID, devID)

	req := httptest.NewRequest(http.MethodDelete, "/repos/default/branch/main", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandleDeleteBranch_NotFound(t *testing.T) {
	store := &mockStore{
		deleteBranchFn: func(ctx context.Context, repo, name string) error {
			return db.ErrBranchNotFound
		},
	}
	srv := New(store, nil, devID, devID)

	req := httptest.NewRequest(http.MethodDelete, "/repos/default/branch/nonexistent", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestHandleDeleteBranch_AlreadyMerged(t *testing.T) {
	store := &mockStore{
		deleteBranchFn: func(ctx context.Context, repo, name string) error {
			return db.ErrBranchNotActive
		},
	}
	srv := New(store, nil, devID, devID)

	req := httptest.NewRequest(http.MethodDelete, "/repos/default/branch/merged-branch", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rec.Code)
	}
}

func TestHandleDeleteBranch_AlreadyAbandoned(t *testing.T) {
	store := &mockStore{
		deleteBranchFn: func(ctx context.Context, repo, name string) error {
			return db.ErrBranchNotActive
		},
	}
	srv := New(store, nil, devID, devID)

	req := httptest.NewRequest(http.MethodDelete, "/repos/default/branch/abandoned-branch", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rec.Code)
	}
}

// --- POST /repos/default/rebase tests ---

func TestHandleRebase_Success(t *testing.T) {
	store := &mockStore{
		rebaseFn: func(ctx context.Context, req model.RebaseRequest) (*model.RebaseResponse, []db.MergeConflict, error) {
			if req.Branch != "feature/test" {
				t.Errorf("expected branch feature/test, got %q", req.Branch)
			}
			return &model.RebaseResponse{
				NewBaseSequence: 5,
				NewHeadSequence: 7,
				CommitsReplayed: 2,
			}, nil, nil
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.RebaseRequest{Branch: "feature/test"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/rebase", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp model.RebaseResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.NewBaseSequence != 5 {
		t.Errorf("expected base_sequence 5, got %d", resp.NewBaseSequence)
	}
	if resp.NewHeadSequence != 7 {
		t.Errorf("expected head_sequence 7, got %d", resp.NewHeadSequence)
	}
	if resp.CommitsReplayed != 2 {
		t.Errorf("expected commits_replayed 2, got %d", resp.CommitsReplayed)
	}
}

func TestHandleRebase_Conflict(t *testing.T) {
	store := &mockStore{
		rebaseFn: func(ctx context.Context, req model.RebaseRequest) (*model.RebaseResponse, []db.MergeConflict, error) {
			return nil, []db.MergeConflict{
				{Path: "conflict.txt", MainVersionID: "v1", BranchVersionID: "v2"},
			}, db.ErrRebaseConflict
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.RebaseRequest{Branch: "feature/conflict"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/rebase", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var errResp model.RebaseConflictError
	if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(errResp.Conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(errResp.Conflicts))
	}
	if errResp.Conflicts[0].Path != "conflict.txt" {
		t.Errorf("expected conflict.txt, got %q", errResp.Conflicts[0].Path)
	}
	if errResp.Conflicts[0].MainVersionID != "v1" {
		t.Errorf("expected main_version_id v1, got %q", errResp.Conflicts[0].MainVersionID)
	}
	if errResp.Conflicts[0].BranchVersionID != "v2" {
		t.Errorf("expected branch_version_id v2, got %q", errResp.Conflicts[0].BranchVersionID)
	}
}

func TestHandleRebase_BranchNotFound(t *testing.T) {
	store := &mockStore{
		rebaseFn: func(ctx context.Context, req model.RebaseRequest) (*model.RebaseResponse, []db.MergeConflict, error) {
			return nil, nil, db.ErrBranchNotFound
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.RebaseRequest{Branch: "nonexistent"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/rebase", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestHandleRebase_CannotRebaseMain(t *testing.T) {
	srv := New(&mockStore{}, nil, devID, devID)

	body, _ := json.Marshal(model.RebaseRequest{Branch: "main"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/rebase", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleRebase_BranchNotActive(t *testing.T) {
	store := &mockStore{
		rebaseFn: func(ctx context.Context, req model.RebaseRequest) (*model.RebaseResponse, []db.MergeConflict, error) {
			return nil, nil, db.ErrBranchNotActive
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.RebaseRequest{Branch: "merged-branch"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/rebase", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandleCommit_RepoNotFound(t *testing.T) {
	store := &mockStore{
		getRepoFn: func(ctx context.Context, name string) (*model.Repo, error) {
			return nil, db.ErrRepoNotFound
		},
		commitFn: func(ctx context.Context, req model.CommitRequest) (*model.CommitResponse, error) {
			t.Fatal("store.Commit should not be called when repo does not exist")
			return nil, nil
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.CommitRequest{
		Branch:  "main",
		Files:   []model.FileChange{{Path: "a.txt", Content: []byte("x")}},
		Message: "m",
	})
	req := httptest.NewRequest(http.MethodPost, "/repos/ghost/commit", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleMerge_RepoNotFound(t *testing.T) {
	store := &mockStore{
		getRepoFn: func(ctx context.Context, name string) (*model.Repo, error) {
			return nil, db.ErrRepoNotFound
		},
		mergeFn: func(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error) {
			t.Fatal("store.Merge should not be called when repo does not exist")
			return nil, nil, nil
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.MergeRequest{Branch: "feature/test"})
	req := httptest.NewRequest(http.MethodPost, "/repos/ghost/merge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleRebase_RepoNotFound(t *testing.T) {
	store := &mockStore{
		getRepoFn: func(ctx context.Context, name string) (*model.Repo, error) {
			return nil, db.ErrRepoNotFound
		},
		rebaseFn: func(ctx context.Context, req model.RebaseRequest) (*model.RebaseResponse, []db.MergeConflict, error) {
			t.Fatal("store.Rebase should not be called when repo does not exist")
			return nil, nil, nil
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.RebaseRequest{Branch: "feature/test"})
	req := httptest.NewRequest(http.MethodPost, "/repos/ghost/rebase", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Review handler tests
// ---------------------------------------------------------------------------

func TestHandleReview_Success(t *testing.T) {
	const identity = "reviewer@example.com"
	store := &mockStore{
		createReviewFn: func(ctx context.Context, repo, branch, reviewer string, status model.ReviewStatus, body string) (*model.Review, error) {
			if repo != "default" {
				t.Errorf("expected repo default, got %q", repo)
			}
			if branch != "feature/x" {
				t.Errorf("expected branch feature/x, got %q", branch)
			}
			if reviewer != identity {
				t.Errorf("expected reviewer %q from context, got %q", identity, reviewer)
			}
			if status != model.ReviewApproved {
				t.Errorf("expected status approved, got %q", status)
			}
			if body != "LGTM" {
				t.Errorf("expected body LGTM, got %q", body)
			}
			return &model.Review{ID: "review-uuid", Sequence: 5}, nil
		},
	}
	srv := New(store, nil, identity, identity)

	b, _ := json.Marshal(model.CreateReviewRequest{Branch: "feature/x", Status: model.ReviewApproved, Body: "LGTM"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/review", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp model.CreateReviewResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != "review-uuid" {
		t.Errorf("expected id review-uuid, got %q", resp.ID)
	}
	if resp.Sequence != 5 {
		t.Errorf("expected sequence 5, got %d", resp.Sequence)
	}
}

func TestHandleReview_SelfApproval(t *testing.T) {
	store := &mockStore{
		createReviewFn: func(ctx context.Context, repo, branch, reviewer string, status model.ReviewStatus, body string) (*model.Review, error) {
			return nil, db.ErrSelfApproval
		},
	}
	srv := New(store, nil, devID, devID)

	b, _ := json.Marshal(model.CreateReviewRequest{Branch: "feature/x", Status: model.ReviewApproved})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/review", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestHandleReview_BranchNotFound(t *testing.T) {
	store := &mockStore{
		createReviewFn: func(ctx context.Context, repo, branch, reviewer string, status model.ReviewStatus, body string) (*model.Review, error) {
			return nil, db.ErrBranchNotFound
		},
	}
	srv := New(store, nil, devID, devID)

	b, _ := json.Marshal(model.CreateReviewRequest{Branch: "nonexistent", Status: model.ReviewApproved})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/review", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Check-run handler tests
// ---------------------------------------------------------------------------

func TestHandleCheck_Success(t *testing.T) {
	const identity = "ci-bot@example.com"
	store := &mockStore{
		createCheckRunFn: func(ctx context.Context, repo, branch, checkName string, status model.CheckRunStatus, reporter string) (*model.CheckRun, error) {
			if repo != "default" {
				t.Errorf("expected repo default, got %q", repo)
			}
			if branch != "feature/x" {
				t.Errorf("expected branch feature/x, got %q", branch)
			}
			if checkName != "ci/build" {
				t.Errorf("expected check_name ci/build, got %q", checkName)
			}
			if status != model.CheckRunPassed {
				t.Errorf("expected status passed, got %q", status)
			}
			if reporter != identity {
				t.Errorf("expected reporter %q from context, got %q", identity, reporter)
			}
			return &model.CheckRun{ID: "check-uuid", Sequence: 3}, nil
		},
	}
	srv := New(store, nil, identity, identity)

	b, _ := json.Marshal(model.CreateCheckRunRequest{Branch: "feature/x", CheckName: "ci/build", Status: model.CheckRunPassed})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/check", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp model.CreateCheckRunResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != "check-uuid" {
		t.Errorf("expected id check-uuid, got %q", resp.ID)
	}
	if resp.Sequence != 3 {
		t.Errorf("expected sequence 3, got %d", resp.Sequence)
	}
}

func TestHandleCheck_BranchNotFound(t *testing.T) {
	store := &mockStore{
		createCheckRunFn: func(ctx context.Context, repo, branch, checkName string, status model.CheckRunStatus, reporter string) (*model.CheckRun, error) {
			return nil, db.ErrBranchNotFound
		},
	}
	srv := New(store, nil, devID, devID)

	b, _ := json.Marshal(model.CreateCheckRunRequest{Branch: "nonexistent", CheckName: "ci/build", Status: model.CheckRunPassed})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/check", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// GET reviews/checks handler tests
// ---------------------------------------------------------------------------

func TestHandleGetReviews(t *testing.T) {
	store := &mockStore{
		listReviewsFn: func(ctx context.Context, repo, branch string, atSeq *int64) ([]model.Review, error) {
			if repo != "default" {
				t.Errorf("expected repo default, got %q", repo)
			}
			if branch != "feature/x" {
				t.Errorf("expected branch feature/x, got %q", branch)
			}
			return []model.Review{
				{ID: "r1", Branch: "feature/x", Reviewer: "alice", Sequence: 2, Status: model.ReviewApproved},
			}, nil
		},
	}
	srv := New(store, nil, devID, devID)

	req := httptest.NewRequest(http.MethodGet, "/repos/default/branch/feature/x/reviews", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var reviews []model.Review
	if err := json.NewDecoder(rec.Body).Decode(&reviews); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(reviews) != 1 {
		t.Fatalf("expected 1 review, got %d", len(reviews))
	}
	if reviews[0].ID != "r1" {
		t.Errorf("expected id r1, got %q", reviews[0].ID)
	}
}

func TestHandleGetChecks(t *testing.T) {
	store := &mockStore{
		listCheckRunsFn: func(ctx context.Context, repo, branch string, atSeq *int64) ([]model.CheckRun, error) {
			if repo != "default" {
				t.Errorf("expected repo default, got %q", repo)
			}
			if branch != "feature/x" {
				t.Errorf("expected branch feature/x, got %q", branch)
			}
			return []model.CheckRun{
				{ID: "cr1", Branch: "feature/x", CheckName: "ci/build", Sequence: 2, Status: model.CheckRunPassed, Reporter: "ci-bot"},
			}, nil
		},
	}
	srv := New(store, nil, devID, devID)

	req := httptest.NewRequest(http.MethodGet, "/repos/default/branch/feature/x/checks", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var checkRuns []model.CheckRun
	if err := json.NewDecoder(rec.Body).Decode(&checkRuns); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(checkRuns) != 1 {
		t.Fatalf("expected 1 check run, got %d", len(checkRuns))
	}
	if checkRuns[0].ID != "cr1" {
		t.Errorf("expected id cr1, got %q", checkRuns[0].ID)
	}
}

func TestHandlePurge_Success(t *testing.T) {
	ms := &mockStore{
		purgeFn: func(ctx context.Context, req db.PurgeRequest) (*db.PurgeResult, error) {
			if req.Repo != "default" {
				t.Errorf("expected repo 'default', got %q", req.Repo)
			}
			if req.OlderThan != 30*24*time.Hour {
				t.Errorf("expected 30d duration, got %v", req.OlderThan)
			}
			if req.DryRun {
				t.Error("expected dry_run=false")
			}
			return &db.PurgeResult{
				BranchesPurged:     3,
				FileCommitsDeleted: 12,
				CommitsDeleted:     4,
				DocumentsDeleted:   2,
				ReviewsDeleted:     1,
				CheckRunsDeleted:   1,
			}, nil
		},
	}
	srv := New(ms, nil, devID, devID)

	body, _ := json.Marshal(map[string]interface{}{"older_than": "30d"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/purge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp model.PurgeResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.BranchesPurged != 3 {
		t.Errorf("expected 3 branches_purged, got %d", resp.BranchesPurged)
	}
	if resp.FileCommitsDeleted != 12 {
		t.Errorf("expected 12 file_commits_deleted, got %d", resp.FileCommitsDeleted)
	}
}

func TestHandlePurge_DryRun(t *testing.T) {
	ms := &mockStore{
		purgeFn: func(ctx context.Context, req db.PurgeRequest) (*db.PurgeResult, error) {
			if !req.DryRun {
				t.Error("expected dry_run=true")
			}
			return &db.PurgeResult{BranchesPurged: 2, CommitsDeleted: 5}, nil
		},
	}
	srv := New(ms, nil, devID, devID)

	body, _ := json.Marshal(map[string]interface{}{"older_than": "7d", "dry_run": true})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/purge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp model.PurgeResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.BranchesPurged != 2 {
		t.Errorf("expected 2 branches_purged, got %d", resp.BranchesPurged)
	}
}

func TestHandlePurge_InvalidDuration(t *testing.T) {
	srv := New(&mockStore{}, nil, devID, devID)

	tests := []struct {
		name    string
		payload string
	}{
		{"missing older_than", `{}`},
		{"not a day format", `{"older_than":"90h"}`},
		{"zero days", `{"older_than":"0d"}`},
		{"negative days", `{"older_than":"-1d"}`},
		{"non-numeric", `{"older_than":"abcd"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/repos/default/purge", bytes.NewReader([]byte(tt.payload)))
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHandlePurge_RepoNotFound(t *testing.T) {
	ms := &mockStore{
		purgeFn: func(ctx context.Context, req db.PurgeRequest) (*db.PurgeResult, error) {
			return nil, db.ErrRepoNotFound
		},
	}
	srv := New(ms, nil, devID, devID)

	body, _ := json.Marshal(map[string]interface{}{"older_than": "30d"})
	req := httptest.NewRequest(http.MethodPost, "/repos/noexist/purge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestParseDayDuration(t *testing.T) {
	tests := []struct {
		input   string
		want    time.Duration
		wantErr bool
	}{
		{"1d", 24 * time.Hour, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"30d", 30 * 24 * time.Hour, false},
		{"365d", 365 * 24 * time.Hour, false},
		// invalid inputs
		{"0d", 0, true},
		{"-1d", 0, true},
		{"7h", 0, true},
		{"7", 0, true},
		{"d", 0, true},
		{"", 0, true},
		{"abcd", 0, true},
		{"1.5d", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseDayDuration(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseDayDuration(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("parseDayDuration(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
