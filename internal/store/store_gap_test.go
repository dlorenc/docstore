package store_test

import (
	"context"
	"testing"

	dbpkg "github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/store"
	"github.com/dlorenc/docstore/internal/testutil"
)

// ---------------------------------------------------------------------------
// GetBranch tests
// ---------------------------------------------------------------------------

func TestGetBranch_Found(t *testing.T) {
	t.Parallel()
	db := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	seed(t, db)
	s := store.New(db)
	ctx := context.Background()

	// "main" is always seeded by migrations.
	b, err := s.GetBranch(ctx, "default/default", "main")
	if err != nil {
		t.Fatalf("GetBranch: %v", err)
	}
	if b == nil {
		t.Fatal("expected non-nil branch info")
	}
	if b.Name != "main" {
		t.Errorf("expected name main, got %q", b.Name)
	}
}

func TestGetBranch_NotFound(t *testing.T) {
	t.Parallel()
	db := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	s := store.New(db)
	ctx := context.Background()

	b, err := s.GetBranch(ctx, "default/default", "nonexistent-branch")
	if err != nil {
		t.Fatalf("GetBranch: unexpected error %v", err)
	}
	if b != nil {
		t.Errorf("expected nil for non-existent branch, got %+v", b)
	}
}

func TestGetBranch_AfterCommit(t *testing.T) {
	t.Parallel()
	db := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	s := store.New(db)
	ctx := context.Background()
	ds := dbpkg.NewStore(db)

	// Commit something to main to advance head_sequence.
	resp, err := ds.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "getbranch-test.txt", Content: []byte("hello")}},
		Message: "getbranch test commit",
		Author:  "alice@example.com",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	b, err := s.GetBranch(ctx, "default/default", "main")
	if err != nil {
		t.Fatalf("GetBranch: %v", err)
	}
	if b == nil {
		t.Fatal("expected non-nil branch")
	}
	if b.HeadSequence != resp.Sequence {
		t.Errorf("expected head_sequence %d, got %d", resp.Sequence, b.HeadSequence)
	}
}

func TestGetBranch_FeatureBranch(t *testing.T) {
	t.Parallel()
	db := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	s := store.New(db)
	ctx := context.Background()
	ds := dbpkg.NewStore(db)

	// Create a feature branch.
	if _, err := ds.CreateBranch(ctx, model.CreateBranchRequest{
		Repo: "default/default",
		Name: "feature/getbranch-test",
	}); err != nil {
		t.Fatalf("create branch: %v", err)
	}

	b, err := s.GetBranch(ctx, "default/default", "feature/getbranch-test")
	if err != nil {
		t.Fatalf("GetBranch: %v", err)
	}
	if b == nil {
		t.Fatal("expected non-nil branch info for feature branch")
	}
	if b.Name != "feature/getbranch-test" {
		t.Errorf("expected name feature/getbranch-test, got %q", b.Name)
	}
	if b.Status != "active" {
		t.Errorf("expected status active, got %q", b.Status)
	}
}

// ---------------------------------------------------------------------------
// GetChain tests
// ---------------------------------------------------------------------------

func TestGetChain_BasicRange(t *testing.T) {
	t.Parallel()
	db := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	seed(t, db)
	s := store.New(db)
	ctx := context.Background()

	// Seed inserts sequences 1-4 on main.
	entries, err := s.GetChain(ctx, "default/default", 1, 4)
	if err != nil {
		t.Fatalf("GetChain: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("expected 4 chain entries, got %d", len(entries))
	}

	// Sequences should be in ascending order.
	for i, e := range entries {
		want := int64(i + 1)
		if e.Sequence != want {
			t.Errorf("entry[%d]: expected sequence %d, got %d", i, want, e.Sequence)
		}
	}
}

func TestGetChain_SingleEntry(t *testing.T) {
	t.Parallel()
	db := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	seed(t, db)
	s := store.New(db)
	ctx := context.Background()

	entries, err := s.GetChain(ctx, "default/default", 2, 2)
	if err != nil {
		t.Fatalf("GetChain: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Sequence != 2 {
		t.Errorf("expected sequence 2, got %d", entries[0].Sequence)
	}
}

func TestGetChain_NoResults(t *testing.T) {
	t.Parallel()
	db := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	seed(t, db)
	s := store.New(db)
	ctx := context.Background()

	// Range that doesn't exist.
	entries, err := s.GetChain(ctx, "default/default", 100, 200)
	if err != nil {
		t.Fatalf("GetChain: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries for out-of-range, got %d", len(entries))
	}
}

func TestGetChain_FileListInEntry(t *testing.T) {
	t.Parallel()
	db := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	seed(t, db)
	s := store.New(db)
	ctx := context.Background()

	// Sequence 1 has hello.txt and world.txt.
	entries, err := s.GetChain(ctx, "default/default", 1, 1)
	if err != nil {
		t.Fatalf("GetChain: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if len(e.Files) != 2 {
		t.Errorf("expected 2 files in sequence 1, got %d: %+v", len(e.Files), e.Files)
	}

	fileNames := make(map[string]bool)
	for _, f := range e.Files {
		fileNames[f.Path] = true
	}
	for _, want := range []string{"hello.txt", "world.txt"} {
		if !fileNames[want] {
			t.Errorf("expected file %q in chain entry", want)
		}
	}
}

func TestGetChain_DeleteEntryHasEmptyHash(t *testing.T) {
	t.Parallel()
	db := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	seed(t, db)
	s := store.New(db)
	ctx := context.Background()

	// Sequence 4 is the delete of deleted.txt (version_id = NULL).
	entries, err := s.GetChain(ctx, "default/default", 4, 4)
	if err != nil {
		t.Fatalf("GetChain: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if len(e.Files) != 1 {
		t.Fatalf("expected 1 file in delete commit, got %d", len(e.Files))
	}
	if e.Files[0].Path != "deleted.txt" {
		t.Errorf("expected deleted.txt, got %q", e.Files[0].Path)
	}
	// Deletes have empty content hash.
	if e.Files[0].ContentHash != "" {
		t.Errorf("expected empty ContentHash for delete, got %q", e.Files[0].ContentHash)
	}
}
