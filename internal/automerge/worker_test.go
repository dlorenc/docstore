package automerge

import (
	"context"
	"testing"

	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/events"
	evtypes "github.com/dlorenc/docstore/internal/events/types"
	mergeutil "github.com/dlorenc/docstore/internal/merge"
	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/store"
)

// ---------------------------------------------------------------------------
// Mock Store (implements automerge.Store)
// ---------------------------------------------------------------------------

type mockStore struct {
	setBranchAutoMergeFn func(ctx context.Context, repo, name string, autoMerge bool) error
	mergeFn              func(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error)
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

func (m *mockStore) Merge(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error) {
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

// Ensure mockReadStore also satisfies mergeutil.QueryStore (via mockStore) —
// the worker passes w.store as the QueryStore argument to EvaluatePolicy.
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

// subscribeAll subscribes to all event types and returns a buffered channel.
func subscribeAll(broker *events.Broker) <-chan events.Event {
	ch, _ := broker.Subscribe("*", []string{
		"com.docstore.branch.merged",
		"com.docstore.branch.automerge.failed",
		"com.docstore.proposal.merged",
	})
	return ch
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
	rs := &mockReadStore{
		getBranchFn: func(_ context.Context, _, _ string) (*store.BranchInfo, error) {
			return activeBranch(), nil
		},
	}
	ms := &mockStore{}
	broker := events.NewBroker(nil)
	evCh := subscribeAll(broker)
	w := New(broker, ms, rs, nil) // nil policyCache → all merges allowed

	w.tryAutoMerge(context.Background(), "myrepo", "my-feature")

	if !ms.mergeCalled {
		t.Fatal("expected Merge to be called")
	}

	// Expect a BranchMerged event.
	select {
	case ev := <-evCh:
		if _, ok := ev.(evtypes.BranchMerged); !ok {
			t.Errorf("expected BranchMerged event, got %T", ev)
		}
	default:
		t.Error("expected BranchMerged event but channel was empty")
	}
}

func TestTryAutoMerge_DisablesOnConflict(t *testing.T) {
	rs := &mockReadStore{
		getBranchFn: func(_ context.Context, _, _ string) (*store.BranchInfo, error) {
			return activeBranch(), nil
		},
	}
	ms := &mockStore{
		mergeFn: func(_ context.Context, _ model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error) {
			return nil, []db.MergeConflict{{Path: "README.md"}}, db.ErrMergeConflict
		},
	}
	broker := events.NewBroker(nil)
	evCh := subscribeAll(broker)
	w := New(broker, ms, rs, nil)

	w.tryAutoMerge(context.Background(), "myrepo", "my-feature")

	if !ms.setBranchAutoMergeCalled {
		t.Fatal("expected SetBranchAutoMerge to be called on conflict")
	}
	if ms.setBranchAutoMergeValue {
		t.Error("expected SetBranchAutoMerge(false) on conflict")
	}

	// Expect a BranchAutoMergeFailed event.
	select {
	case ev := <-evCh:
		if _, ok := ev.(evtypes.BranchAutoMergeFailed); !ok {
			t.Errorf("expected BranchAutoMergeFailed event, got %T", ev)
		}
	default:
		t.Error("expected BranchAutoMergeFailed event but channel was empty")
	}
}

func TestTryAutoMerge_EmitsProposalMerged(t *testing.T) {
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
	broker := events.NewBroker(nil)
	evCh := subscribeAll(broker)
	// Also subscribe to proposal.merged
	propCh, _ := broker.Subscribe("*", []string{"com.docstore.proposal.merged"})
	w := New(broker, ms, rs, nil)

	w.tryAutoMerge(context.Background(), "myrepo", "my-feature")

	if !ms.mergeCalled {
		t.Fatal("expected Merge to be called")
	}

	// Drain evCh looking for ProposalMerged; also check propCh.
	var gotProposalMerged bool
	for i := 0; i < 2; i++ {
		select {
		case ev := <-evCh:
			if _, ok := ev.(evtypes.ProposalMerged); ok {
				gotProposalMerged = true
			}
		case ev := <-propCh:
			if _, ok := ev.(evtypes.ProposalMerged); ok {
				gotProposalMerged = true
			}
		default:
		}
	}
	if !gotProposalMerged {
		t.Error("expected ProposalMerged event to be emitted")
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
