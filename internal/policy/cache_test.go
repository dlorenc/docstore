package policy_test

import (
	"context"
	"testing"

	"github.com/dlorenc/docstore/internal/policy"
	"github.com/dlorenc/docstore/internal/store"
)

// mockReadStore is a minimal ReadStore for cache tests.
// It counts how many times Load is called and returns no policies or OWNERS.
type mockReadStore struct {
	calls int
}

func (m *mockReadStore) MaterializeTree(ctx context.Context, repo, branch string, atSeq *int64, limit int, afterPath string) ([]store.TreeEntry, error) {
	m.calls++
	return nil, nil
}

func (m *mockReadStore) GetFile(ctx context.Context, repo, branch, path string, atSeq *int64) (*store.FileContent, error) {
	return nil, nil
}

// TestCachePerRepo verifies that Invalidate(repoA) does not evict repoB's
// cached entry.
func TestCachePerRepo(t *testing.T) {
	ctx := context.Background()
	c := policy.NewCache()

	rs := &mockReadStore{}

	// Load both repos — each triggers one MaterializeTree call.
	if _, _, err := c.Load(ctx, "repoA", rs); err != nil {
		t.Fatalf("Load(repoA): %v", err)
	}
	if _, _, err := c.Load(ctx, "repoB", rs); err != nil {
		t.Fatalf("Load(repoB): %v", err)
	}
	callsAfterBothLoaded := rs.calls

	// Invalidate only repoA.
	c.Invalidate("repoA")

	// Load repoA again — should re-call the store.
	if _, _, err := c.Load(ctx, "repoA", rs); err != nil {
		t.Fatalf("Load(repoA) after invalidate: %v", err)
	}
	if rs.calls <= callsAfterBothLoaded {
		t.Error("expected repoA to be reloaded after Invalidate")
	}

	callsBeforeRepoB := rs.calls

	// Load repoB again — should NOT re-call the store (still cached).
	if _, _, err := c.Load(ctx, "repoB", rs); err != nil {
		t.Fatalf("Load(repoB) after invalidating repoA: %v", err)
	}
	if rs.calls != callsBeforeRepoB {
		t.Error("expected repoB to remain cached after repoA was invalidated")
	}
}

// TestCacheInvalidatedAfterMerge verifies that after Invalidate(repo) the next
// Load triggers a reload (store is queried again) rather than serving the
// cached entry.
func TestCacheInvalidatedAfterMerge(t *testing.T) {
	ctx := context.Background()
	c := policy.NewCache()
	rs := &mockReadStore{}

	// First Load — populates the cache.
	if _, _, err := c.Load(ctx, "testrepo", rs); err != nil {
		t.Fatalf("Load: %v", err)
	}
	callsAfterFirst := rs.calls
	if callsAfterFirst == 0 {
		t.Fatal("expected at least one store call on first Load")
	}

	// Second Load — should be served from cache, no new store calls.
	if _, _, err := c.Load(ctx, "testrepo", rs); err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if rs.calls != callsAfterFirst {
		t.Errorf("expected no new store calls on cached Load, got %d total (was %d)", rs.calls, callsAfterFirst)
	}

	// Invalidate the repo — clears the cache entry.
	c.Invalidate("testrepo")

	// Third Load — should re-query the store.
	if _, _, err := c.Load(ctx, "testrepo", rs); err != nil {
		t.Fatalf("Load after Invalidate: %v", err)
	}
	if rs.calls == callsAfterFirst {
		t.Error("expected store to be queried again after Invalidate")
	}
}
