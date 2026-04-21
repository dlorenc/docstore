package automerge

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	dbpkg "github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/events"
	mergeutil "github.com/dlorenc/docstore/internal/merge"
	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/store"
	"github.com/dlorenc/docstore/internal/testutil"
)

// ---------------------------------------------------------------------------
// Test infrastructure
// ---------------------------------------------------------------------------

var sharedAdminDSN string

func TestMain(m *testing.M) {
	dsn, cleanup, err := testutil.StartSharedPostgres()
	if err != nil {
		fmt.Fprintf(os.Stderr, "start shared postgres: %v\n", err)
		os.Exit(1)
	}
	sharedAdminDSN = dsn
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// ---------------------------------------------------------------------------
// Mock Store (implements automerge.Store)
// ---------------------------------------------------------------------------

type mockStore struct {
	setBranchAutoMergeFn func(ctx context.Context, repo, name string, autoMerge bool) error
	mergeFn              func(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []dbpkg.MergeConflict, error)
	mergeProposalFn      func(ctx context.Context, repo, branch string) (*model.Proposal, error)
	listReviewsFn        func(ctx context.Context, repo, branch string, atSeq *int64) ([]model.Review, error)
	listCheckRunsFn      func(ctx context.Context, repo, branch string, atSeq *int64, history bool) ([]model.CheckRun, error)
	listProposalsFn      func(ctx context.Context, repo string, state *model.ProposalState, branch *string) ([]*model.Proposal, error)
	getRoleFn            func(ctx context.Context, repo, identity string) (*model.Role, error)

	// track calls
	setBranchAutoMergeCalled bool
	setBranchAutoMergeValue  bool
	mergeCalled              bool
}

func (m *mockStore) SetBranchAutoMerge(ctx context.Context, repo, name string, autoMerge bool) error {
	m.setBranchAutoMergeCalled = true
	m.setBranchAutoMergeValue = autoMerge
	if m.setBranchAutoMergeFn != nil {
		return m.setBranchAutoMergeFn(ctx, repo, name, autoMerge)
	}
	return nil
}

func (m *mockStore) Merge(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []dbpkg.MergeConflict, error) {
	m.mergeCalled = true
	if m.mergeFn != nil {
		return m.mergeFn(ctx, req)
	}
	return &model.MergeResponse{Sequence: 1}, nil, nil
}

func (m *mockStore) MergeProposal(ctx context.Context, repo, branch string) (*model.Proposal, error) {
	if m.mergeProposalFn != nil {
		return m.mergeProposalFn(ctx, repo, branch)
	}
	return nil, nil
}

func (m *mockStore) ListReviews(ctx context.Context, repo, branch string, atSeq *int64) ([]model.Review, error) {
	if m.listReviewsFn != nil {
		return m.listReviewsFn(ctx, repo, branch, atSeq)
	}
	return nil, nil
}

func (m *mockStore) ListCheckRuns(ctx context.Context, repo, branch string, atSeq *int64, history bool) ([]model.CheckRun, error) {
	if m.listCheckRunsFn != nil {
		return m.listCheckRunsFn(ctx, repo, branch, atSeq, history)
	}
	return nil, nil
}

func (m *mockStore) ListProposals(ctx context.Context, repo string, state *model.ProposalState, branch *string) ([]*model.Proposal, error) {
	if m.listProposalsFn != nil {
		return m.listProposalsFn(ctx, repo, state, branch)
	}
	return nil, nil
}

func (m *mockStore) GetRole(ctx context.Context, repo, identity string) (*model.Role, error) {
	if m.getRoleFn != nil {
		return m.getRoleFn(ctx, repo, identity)
	}
	return nil, nil
}

// ---------------------------------------------------------------------------
// Mock ReadStore (implements mergeutil.ReadStore)
// ---------------------------------------------------------------------------

type mockReadStore struct {
	getBranchFn func(ctx context.Context, repo, branch string) (*store.BranchInfo, error)
}

func (m *mockReadStore) GetBranch(ctx context.Context, repo, branch string) (*store.BranchInfo, error) {
	if m.getBranchFn != nil {
		return m.getBranchFn(ctx, repo, branch)
	}
	return nil, nil
}

func (m *mockReadStore) GetDiff(ctx context.Context, repo, branch string) (*store.DiffResult, error) {
	return &store.DiffResult{}, nil
}

func (m *mockReadStore) MaterializeTree(ctx context.Context, repo, branch string, atSeq *int64, limit int, afterPath string) ([]store.TreeEntry, error) {
	return nil, nil
}

func (m *mockReadStore) GetFile(ctx context.Context, repo, branch, path string, atSeq *int64) (*store.FileContent, error) {
	return nil, nil
}

var _ mergeutil.ReadStore = (*mockReadStore)(nil)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// activeBranch returns a BranchInfo that is active, non-draft, and auto-merge enabled.
func activeBranch() *store.BranchInfo {
	return &store.BranchInfo{
		Name:      "my-feature",
		Status:    "active",
		Draft:     false,
		AutoMerge: true,
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestTryAutoMerge_SkipsWhenDisabled(t *testing.T) {
	rs := &mockReadStore{
		getBranchFn: func(_ context.Context, _, _ string) (*store.BranchInfo, error) {
			return &store.BranchInfo{Name: "my-feature", Status: "active", Draft: false, AutoMerge: false}, nil
		},
	}
	ms := &mockStore{}
	broker := events.NewBroker(nil)
	w := New(broker, ms, rs, nil)

	w.tryAutoMerge(context.Background(), "myrepo", "my-feature")

	if ms.mergeCalled {
		t.Error("expected Merge not to be called when AutoMerge=false")
	}
}

func TestTryAutoMerge_SkipsWhenDraft(t *testing.T) {
	rs := &mockReadStore{
		getBranchFn: func(_ context.Context, _, _ string) (*store.BranchInfo, error) {
			return &store.BranchInfo{Name: "my-feature", Status: "active", Draft: true, AutoMerge: true}, nil
		},
	}
	ms := &mockStore{}
	broker := events.NewBroker(nil)
	w := New(broker, ms, rs, nil)

	w.tryAutoMerge(context.Background(), "myrepo", "my-feature")

	if ms.mergeCalled {
		t.Error("expected Merge not to be called when Draft=true")
	}
}

func TestTryAutoMerge_SkipsWhenNotActive(t *testing.T) {
	rs := &mockReadStore{
		getBranchFn: func(_ context.Context, _, _ string) (*store.BranchInfo, error) {
			return &store.BranchInfo{Name: "my-feature", Status: "closed", Draft: false, AutoMerge: true}, nil
		},
	}
	ms := &mockStore{}
	broker := events.NewBroker(nil)
	w := New(broker, ms, rs, nil)

	w.tryAutoMerge(context.Background(), "myrepo", "my-feature")

	if ms.mergeCalled {
		t.Error("expected Merge not to be called when Status=closed")
	}
}

func TestTryAutoMerge_MergesWhenAllowed(t *testing.T) {
	d := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	rs := &mockReadStore{
		getBranchFn: func(_ context.Context, _, _ string) (*store.BranchInfo, error) {
			return activeBranch(), nil
		},
	}
	ms := &mockStore{}
	broker := events.NewBroker(d)
	w := New(broker, ms, rs, nil) // nil policyCache → all merges allowed

	seq, _ := broker.CurrentSeq(context.Background())
	w.tryAutoMerge(context.Background(), "myrepo", "my-feature")

	if !ms.mergeCalled {
		t.Fatal("expected Merge to be called")
	}

	evs, err := broker.Poll(context.Background(), "*", seq, []string{"com.docstore.branch.merged"})
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(evs) == 0 {
		t.Error("expected BranchMerged event in event_log")
	}
	if len(evs) > 0 && evs[0].Type != "com.docstore.branch.merged" {
		t.Errorf("expected branch.merged event, got %q", evs[0].Type)
	}
}

func TestTryAutoMerge_DisablesOnConflict(t *testing.T) {
	d := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	rs := &mockReadStore{
		getBranchFn: func(_ context.Context, _, _ string) (*store.BranchInfo, error) {
			return activeBranch(), nil
		},
	}
	ms := &mockStore{
		mergeFn: func(_ context.Context, _ model.MergeRequest) (*model.MergeResponse, []dbpkg.MergeConflict, error) {
			return nil, []dbpkg.MergeConflict{{Path: "README.md"}}, dbpkg.ErrMergeConflict
		},
	}
	broker := events.NewBroker(d)
	w := New(broker, ms, rs, nil)

	seq, _ := broker.CurrentSeq(context.Background())
	w.tryAutoMerge(context.Background(), "myrepo", "my-feature")

	if !ms.setBranchAutoMergeCalled {
		t.Fatal("expected SetBranchAutoMerge to be called on conflict")
	}
	if ms.setBranchAutoMergeValue {
		t.Error("expected SetBranchAutoMerge(false) on conflict")
	}

	evs, err := broker.Poll(context.Background(), "*", seq, []string{"com.docstore.branch.automerge.failed"})
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(evs) == 0 {
		t.Error("expected BranchAutoMergeFailed event in event_log")
	}
}

func TestTryAutoMerge_EmitsProposalMerged(t *testing.T) {
	d := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	rs := &mockReadStore{
		getBranchFn: func(_ context.Context, _, _ string) (*store.BranchInfo, error) {
			return activeBranch(), nil
		},
	}
	proposalID := "prop-1"
	ms := &mockStore{
		mergeProposalFn: func(_ context.Context, _, _ string) (*model.Proposal, error) {
			return &model.Proposal{ID: proposalID, Branch: "my-feature", BaseBranch: "main"}, nil
		},
	}
	broker := events.NewBroker(d)
	w := New(broker, ms, rs, nil)

	seq, _ := broker.CurrentSeq(context.Background())
	w.tryAutoMerge(context.Background(), "myrepo", "my-feature")

	if !ms.mergeCalled {
		t.Fatal("expected Merge to be called")
	}

	evs, err := broker.Poll(context.Background(), "*", seq, []string{"com.docstore.proposal.merged"})
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(evs) == 0 {
		t.Error("expected ProposalMerged event in event_log")
	}
}

func TestTryAutoMerge_HandlesNilBranch(t *testing.T) {
	rs := &mockReadStore{
		getBranchFn: func(_ context.Context, _, _ string) (*store.BranchInfo, error) {
			return nil, nil
		},
	}
	ms := &mockStore{}
	broker := events.NewBroker(nil)
	w := New(broker, ms, rs, nil)

	// Must not panic.
	w.tryAutoMerge(context.Background(), "myrepo", "nonexistent")

	if ms.mergeCalled {
		t.Error("expected Merge not to be called when branch is nil")
	}
}

func TestRun_CurrentSeqFailRetryExitsOnContextCancel(t *testing.T) {
	// Use a closed *sql.DB for the broker so CurrentSeq always fails, forcing
	// the retry loop in Run().  The context cancels quickly so the worker must
	// exit cleanly without ever calling Merge.
	brokenDB, err := sql.Open("postgres", sharedAdminDSN)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	brokenDB.Close() // closed pool → every query returns sql.ErrConnDone

	broker := events.NewBroker(brokenDB)
	ms := &mockStore{}
	rs := &mockReadStore{}
	w := New(broker, ms, rs, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	w.Run(ctx) // must return, not hang; must not proceed with sinceSeq=0

	if ms.mergeCalled {
		t.Error("Merge should not be called when CurrentSeq fails at startup")
	}
}

func TestRun_EmptyBranchEventSkipped(t *testing.T) {
	d := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)

	var getBranchCalledWithEmpty bool
	rs := &mockReadStore{
		getBranchFn: func(_ context.Context, _, branch string) (*store.BranchInfo, error) {
			if branch == "" {
				getBranchCalledWithEmpty = true
			}
			return nil, nil
		},
	}
	ms := &mockStore{}
	broker := events.NewBroker(d)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// After the worker starts and enters its first WaitForEvent (a few ms),
	// inject an event whose "data" payload has no branch field, then Notify
	// so the worker wakes and processes it.  After processing we cancel the
	// context so Run() returns.
	go func() {
		time.Sleep(100 * time.Millisecond)
		d.ExecContext(ctx, //nolint:errcheck
			`INSERT INTO event_log (repo, type, payload) VALUES
			 ('myrepo', 'com.docstore.check.reported',
			  '{"type":"com.docstore.check.reported","source":"/repos/myrepo","data":{}}')`)
		broker.Notify()
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	w := New(broker, ms, rs, nil)
	w.Run(ctx)

	if getBranchCalledWithEmpty {
		t.Error("GetBranch should not be called with empty branch name")
	}
	if ms.mergeCalled {
		t.Error("Merge should not be called when event has empty branch field")
	}
}
