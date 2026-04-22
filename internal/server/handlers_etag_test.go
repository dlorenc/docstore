package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/store"
)

// ---------------------------------------------------------------------------
// Branch ETag tests
// ---------------------------------------------------------------------------

func TestGetBranch_ReturnsETag(t *testing.T) {
	rs := &mockReadStore{
		getBranchFn: func(_ context.Context, _, branch string) (*store.BranchInfo, error) {
			return &store.BranchInfo{Name: branch, HeadSequence: 7}, nil
		},
	}
	srv := NewWithReadStore(&mockStore{}, rs, devID, devID)

	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/branch/feat", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("expected ETag header, got none")
	}
	want := computeETag("default/default", "feat", "7")
	if etag != want {
		t.Errorf("ETag = %q; want %q", etag, want)
	}
}

func TestGetBranch_NoReadStore_Returns503(t *testing.T) {
	// New with nil database → no readStore
	srv := New(&mockStore{}, nil, devID, devID)

	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/branch/feat", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestGetBranch_NotFound_Returns404(t *testing.T) {
	rs := &mockReadStore{
		getBranchFn: func(_ context.Context, _, _ string) (*store.BranchInfo, error) {
			return nil, nil // branch not found
		},
	}
	srv := NewWithReadStore(&mockStore{}, rs, devID, devID)

	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/branch/nonexistent", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestUpdateBranch_WithNoIfMatch_Proceeds(t *testing.T) {
	ms := &mockStore{
		updateBranchDraftFn: func(_ context.Context, _, _ string, _ bool) error {
			return nil
		},
	}
	// readStore's GetBranch won't be called (no If-Match header)
	srv := NewWithReadStore(ms, &mockReadStore{}, devID, devID)

	body, _ := json.Marshal(model.UpdateBranchRequest{Draft: true})
	req := httptest.NewRequest(http.MethodPatch, "/repos/default/default/-/branch/feat", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestUpdateBranch_WithCorrectIfMatch_Succeeds(t *testing.T) {
	const repo, branch = "default/default", "feat"
	bi := &store.BranchInfo{Name: branch, HeadSequence: 3}
	ms := &mockStore{
		updateBranchDraftFn: func(_ context.Context, _, _ string, _ bool) error {
			return nil
		},
	}
	rs := &mockReadStore{
		getBranchFn: func(_ context.Context, _, _ string) (*store.BranchInfo, error) {
			return bi, nil
		},
	}
	srv := NewWithReadStore(ms, rs, devID, devID)

	etag := computeETag(repo, branch, "3")
	body, _ := json.Marshal(model.UpdateBranchRequest{Draft: true})
	req := httptest.NewRequest(http.MethodPatch, "/repos/"+repo+"/-/branch/"+branch, bytes.NewReader(body))
	req.Header.Set("If-Match", etag)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestUpdateBranch_WithStaleIfMatch_Returns412(t *testing.T) {
	const repo, branch = "default/default", "feat"
	bi := &store.BranchInfo{Name: branch, HeadSequence: 5}
	rs := &mockReadStore{
		getBranchFn: func(_ context.Context, _, _ string) (*store.BranchInfo, error) {
			return bi, nil
		},
	}
	srv := NewWithReadStore(&mockStore{}, rs, devID, devID)

	staleETag := computeETag(repo, branch, "3") // sequence 3 != actual 5
	body, _ := json.Marshal(model.UpdateBranchRequest{Draft: true})
	req := httptest.NewRequest(http.MethodPatch, "/repos/"+repo+"/-/branch/"+branch, bytes.NewReader(body))
	req.Header.Set("If-Match", staleETag)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected 412, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp APIError
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Code != ErrCodePreconditionFailed {
		t.Errorf("code = %q; want %q", resp.Code, ErrCodePreconditionFailed)
	}
}

func TestEnableAutoMerge_WithStaleIfMatch_Returns412(t *testing.T) {
	const repo, branch = "default/default", "feat"
	bi := &store.BranchInfo{Name: branch, HeadSequence: 9}
	rs := &mockReadStore{
		getBranchFn: func(_ context.Context, _, _ string) (*store.BranchInfo, error) {
			return bi, nil
		},
	}
	srv := NewWithReadStore(&mockStore{}, rs, devID, devID)

	staleETag := computeETag(repo, branch, "1")
	req := httptest.NewRequest(http.MethodPost, "/repos/"+repo+"/-/branch/"+branch+"/auto-merge", nil)
	req.Header.Set("If-Match", staleETag)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected 412, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestDisableAutoMerge_WithStaleIfMatch_Returns412(t *testing.T) {
	const repo, branch = "default/default", "feat"
	bi := &store.BranchInfo{Name: branch, HeadSequence: 9}
	rs := &mockReadStore{
		getBranchFn: func(_ context.Context, _, _ string) (*store.BranchInfo, error) {
			return bi, nil
		},
	}
	srv := NewWithReadStore(&mockStore{}, rs, devID, devID)

	staleETag := computeETag(repo, branch, "1")
	req := httptest.NewRequest(http.MethodDelete, "/repos/"+repo+"/-/branch/"+branch+"/auto-merge", nil)
	req.Header.Set("If-Match", staleETag)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected 412, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Proposal ETag tests
// ---------------------------------------------------------------------------

func makeTestProposal(id string, updatedAt time.Time) *model.Proposal {
	return &model.Proposal{
		ID:        id,
		Repo:      "default/default",
		Branch:    "feat",
		Title:     "Test proposal",
		Author:    devID,
		State:     model.ProposalOpen,
		UpdatedAt: updatedAt,
		CreatedAt: updatedAt,
	}
}

func TestGetProposal_ReturnsETag(t *testing.T) {
	ts := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	p := makeTestProposal("prop-1", ts)
	ms := &mockStore{
		getProposalFn: func(_ context.Context, _, _ string) (*model.Proposal, error) {
			return p, nil
		},
	}
	srv := NewWithReadStore(ms, &mockReadStore{}, devID, devID)

	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/proposals/prop-1", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("expected ETag header, got none")
	}
	want := computeETag(p.ID, fmt.Sprintf("%d", ts.UnixNano()))
	if etag != want {
		t.Errorf("ETag = %q; want %q", etag, want)
	}
}

func TestUpdateProposal_WithCorrectIfMatch_Succeeds(t *testing.T) {
	ts := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	p := makeTestProposal("prop-2", ts)
	newTitle := "Updated"
	ms := &mockStore{
		getProposalFn: func(_ context.Context, _, _ string) (*model.Proposal, error) {
			return p, nil
		},
		updateProposalFn: func(_ context.Context, _, _ string, _, _ *string) (*model.Proposal, error) {
			updated := *p
			updated.Title = newTitle
			return &updated, nil
		},
	}
	srv := NewWithReadStore(ms, &mockReadStore{}, devID, devID)

	etag := computeETag(p.ID, fmt.Sprintf("%d", ts.UnixNano()))
	body, _ := json.Marshal(model.UpdateProposalRequest{Title: &newTitle})
	req := httptest.NewRequest(http.MethodPatch, "/repos/default/default/-/proposals/prop-2", bytes.NewReader(body))
	req.Header.Set("If-Match", etag)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestUpdateProposal_WithStaleIfMatch_Returns412(t *testing.T) {
	ts := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	p := makeTestProposal("prop-3", ts)
	ms := &mockStore{
		getProposalFn: func(_ context.Context, _, _ string) (*model.Proposal, error) {
			return p, nil
		},
	}
	srv := NewWithReadStore(ms, &mockReadStore{}, devID, devID)

	staleETag := computeETag(p.ID, "0") // wrong timestamp
	newTitle := "Updated"
	body, _ := json.Marshal(model.UpdateProposalRequest{Title: &newTitle})
	req := httptest.NewRequest(http.MethodPatch, "/repos/default/default/-/proposals/prop-3", bytes.NewReader(body))
	req.Header.Set("If-Match", staleETag)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected 412, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp APIError
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Code != ErrCodePreconditionFailed {
		t.Errorf("code = %q; want %q", resp.Code, ErrCodePreconditionFailed)
	}
}

func TestUpdateProposal_WithNoIfMatch_Proceeds(t *testing.T) {
	ts := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	p := makeTestProposal("prop-4", ts)
	newTitle := "Updated"
	ms := &mockStore{
		getProposalFn: func(_ context.Context, _, _ string) (*model.Proposal, error) {
			return p, nil
		},
		updateProposalFn: func(_ context.Context, _, _ string, _, _ *string) (*model.Proposal, error) {
			updated := *p
			updated.Title = newTitle
			return &updated, nil
		},
	}
	srv := NewWithReadStore(ms, &mockReadStore{}, devID, devID)

	body, _ := json.Marshal(model.UpdateProposalRequest{Title: &newTitle})
	req := httptest.NewRequest(http.MethodPatch, "/repos/default/default/-/proposals/prop-4", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestCloseProposal_WithStaleIfMatch_Returns412(t *testing.T) {
	ts := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	p := makeTestProposal("prop-5", ts)
	ms := &mockStore{
		getProposalFn: func(_ context.Context, _, _ string) (*model.Proposal, error) {
			return p, nil
		},
	}
	srv := NewWithReadStore(ms, &mockReadStore{}, devID, devID)

	staleETag := computeETag(p.ID, "0")
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/proposals/prop-5/close", nil)
	req.Header.Set("If-Match", staleETag)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected 412, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestCloseProposal_WithNoIfMatch_Proceeds(t *testing.T) {
	ts := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	p := makeTestProposal("prop-6", ts)
	ms := &mockStore{
		getProposalFn: func(_ context.Context, _, _ string) (*model.Proposal, error) {
			return p, nil
		},
		closeProposalFn: func(_ context.Context, _, _ string) error {
			return nil
		},
	}
	srv := NewWithReadStore(ms, &mockReadStore{}, devID, devID)

	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/proposals/prop-6/close", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// computeETag unit tests
// ---------------------------------------------------------------------------

func TestComputeETag_Deterministic(t *testing.T) {
	e1 := computeETag("repo", "branch", "42")
	e2 := computeETag("repo", "branch", "42")
	if e1 != e2 {
		t.Errorf("computeETag not deterministic: %q != %q", e1, e2)
	}
}

func TestComputeETag_DifferentInputsDifferentOutput(t *testing.T) {
	e1 := computeETag("repo", "branch", "42")
	e2 := computeETag("repo", "branch", "43")
	if e1 == e2 {
		t.Errorf("expected different ETags for different sequences, got %q", e1)
	}
}

func TestComputeETag_IsQuoted(t *testing.T) {
	e := computeETag("x")
	if len(e) < 2 || e[0] != '"' || e[len(e)-1] != '"' {
		t.Errorf("expected quoted ETag, got %q", e)
	}
}

func TestErrCodePreconditionFailed_MapsTo412(t *testing.T) {
	code := statusToCode(http.StatusPreconditionFailed)
	if code != ErrCodePreconditionFailed {
		t.Errorf("statusToCode(412) = %q; want %q", code, ErrCodePreconditionFailed)
	}
}

// ---------------------------------------------------------------------------
// If-Match: * wildcard tests (RFC 7232 §3.2)
// ---------------------------------------------------------------------------

func TestUpdateBranch_WithWildcardIfMatch_Succeeds(t *testing.T) {
	ms := &mockStore{
		updateBranchDraftFn: func(_ context.Context, _, _ string, _ bool) error {
			return nil
		},
	}
	rs := &mockReadStore{
		getBranchFn: func(_ context.Context, _, _ string) (*store.BranchInfo, error) {
			return &store.BranchInfo{Name: "feat", HeadSequence: 5}, nil
		},
	}
	srv := NewWithReadStore(ms, rs, devID, devID)

	body, _ := json.Marshal(model.UpdateBranchRequest{Draft: true})
	req := httptest.NewRequest(http.MethodPatch, "/repos/default/default/-/branch/feat", bytes.NewReader(body))
	req.Header.Set("If-Match", "*")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for If-Match: *, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestUpdateProposal_WithWildcardIfMatch_Succeeds(t *testing.T) {
	ts := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	p := makeTestProposal("prop-wc", ts)
	newTitle := "Wildcard update"
	ms := &mockStore{
		getProposalFn: func(_ context.Context, _, _ string) (*model.Proposal, error) {
			return p, nil
		},
		updateProposalFn: func(_ context.Context, _, _ string, _, _ *string) (*model.Proposal, error) {
			updated := *p
			updated.Title = newTitle
			return &updated, nil
		},
	}
	srv := NewWithReadStore(ms, &mockReadStore{}, devID, devID)

	body, _ := json.Marshal(model.UpdateProposalRequest{Title: &newTitle})
	req := httptest.NewRequest(http.MethodPatch, "/repos/default/default/-/proposals/prop-wc", bytes.NewReader(body))
	req.Header.Set("If-Match", "*")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for If-Match: *, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleDeleteBranch If-Match tests
// ---------------------------------------------------------------------------

func TestDeleteBranch_WithNoIfMatch_Proceeds(t *testing.T) {
	ms := &mockStore{
		deleteBranchFn: func(_ context.Context, _, _ string) error {
			return nil
		},
	}
	srv := NewWithReadStore(ms, &mockReadStore{}, devID, devID)

	req := httptest.NewRequest(http.MethodDelete, "/repos/default/default/-/branch/feat", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteBranch_WithCorrectIfMatch_Succeeds(t *testing.T) {
	const repo, branch = "default/default", "feat"
	bi := &store.BranchInfo{Name: branch, HeadSequence: 4}
	ms := &mockStore{
		deleteBranchFn: func(_ context.Context, _, _ string) error {
			return nil
		},
	}
	rs := &mockReadStore{
		getBranchFn: func(_ context.Context, _, _ string) (*store.BranchInfo, error) {
			return bi, nil
		},
	}
	srv := NewWithReadStore(ms, rs, devID, devID)

	etag := computeETag(repo, branch, "4")
	req := httptest.NewRequest(http.MethodDelete, "/repos/"+repo+"/-/branch/"+branch, nil)
	req.Header.Set("If-Match", etag)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteBranch_WithStaleIfMatch_Returns412(t *testing.T) {
	const repo, branch = "default/default", "feat"
	bi := &store.BranchInfo{Name: branch, HeadSequence: 8}
	rs := &mockReadStore{
		getBranchFn: func(_ context.Context, _, _ string) (*store.BranchInfo, error) {
			return bi, nil
		},
	}
	srv := NewWithReadStore(&mockStore{}, rs, devID, devID)

	staleETag := computeETag(repo, branch, "2") // sequence 2 != actual 8
	req := httptest.NewRequest(http.MethodDelete, "/repos/"+repo+"/-/branch/"+branch, nil)
	req.Header.Set("If-Match", staleETag)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected 412, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp APIError
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Code != ErrCodePreconditionFailed {
		t.Errorf("code = %q; want %q", resp.Code, ErrCodePreconditionFailed)
	}
}
