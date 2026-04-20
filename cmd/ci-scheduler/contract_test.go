package main

// contract_test.go — cross-package contract test for the /branches wire format.
//
// Regression: a previous commit changed fetchBranchHead to decode
// model.BranchesResponse{"branches":[...]} while the real server handler
// returns a bare []Branch JSON array. Both sides were wrong in sync so CI
// stayed green; this test catches the mismatch going forward.
//
// The test spins up a real internal/server handler (not a hand-rolled mock),
// calls GET /repos/.../branches, and feeds the response body into the
// fetchBranchHead decoder. It fails if either side changes the envelope
// without the other following.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/server"
	"github.com/dlorenc/docstore/internal/store"
)

// contractWriteStore is a minimal WriteStore stub for the contract test.
// Only GetRepo and HasAdmin are overridden; the embedded nil interface panics
// if any other method is called, making unintended call paths immediately obvious.
type contractWriteStore struct {
	server.WriteStore // nil — panics if any unimplemented method is invoked
}

func (w *contractWriteStore) GetRepo(_ context.Context, name string) (*model.Repo, error) {
	return &model.Repo{Name: name}, nil
}

// HasAdmin returns (false, nil) so the bootstrap-admin grant fires and the
// request gets admin access without needing a real role entry.
func (w *contractWriteStore) HasAdmin(_ context.Context, _ string) (bool, error) {
	return false, nil
}

// contractReadStore is a minimal ReadStore stub for the contract test.
// Only ListBranches is overridden; handleBranches only calls that method.
type contractReadStore struct {
	server.ReadStore // nil — panics if any unimplemented method is invoked
	branches         []store.BranchInfo
}

func (r *contractReadStore) ListBranches(_ context.Context, _, _ string, _, _ bool) ([]store.BranchInfo, error) {
	return r.branches, nil
}

// TestBranchesEndpointContract verifies that the real server handler and the
// fetchBranchHead decoder agree on the response envelope for GET /-/branches.
//
// The test will fail if:
//   - the server wraps the array (e.g. {"branches":[...]}) — fetchBranchHead
//     decodes into an empty []Branch and "main" is not found;
//   - fetchBranchHead wraps the decode target (e.g. BranchesResponse) — the
//     bare-array JSON from the server fails to decode into the struct.
func TestBranchesEndpointContract(t *testing.T) {
	const wantSeq = int64(77)

	testBranches := []store.BranchInfo{
		{Name: "main", HeadSequence: wantSeq, Status: "active"},
		{Name: "feat/other", HeadSequence: 3, Status: "active"},
	}

	// Spin up a real internal/server handler with minimal test doubles.
	// devIdentity == bootstrapAdmin so the request is granted admin access
	// without a real role entry in the store.
	ws := &contractWriteStore{}
	rs := &contractReadStore{branches: testBranches}
	handler := server.NewWithReadStore(ws, rs, "contractdev", "contractdev")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// fetchBranchHead builds the URL itself, so only docstoreURL is needed.
	sched := &scheduler{docstoreURL: srv.URL, httpClient: &http.Client{}}

	seq, err := sched.fetchBranchHead(context.Background(), "testrepo", "main")
	if err != nil {
		t.Fatalf("fetchBranchHead failed (likely envelope mismatch): %v", err)
	}
	if seq != wantSeq {
		t.Errorf("head sequence = %d, want %d", seq, wantSeq)
	}
}
