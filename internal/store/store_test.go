package store_test

import (
	"context"
	"database/sql"
	"testing"

	dbpkg "github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/store"
	"github.com/dlorenc/docstore/internal/testutil"
)

// seed inserts test data into the database. It creates:
//   - documents: hello.txt (v1, v2), world.txt (v1), deleted.txt (v1)
//   - file_commits on main:
//     seq 1: hello.txt v1, world.txt v1
//     seq 2: hello.txt v2 (update)
//     seq 3: deleted.txt v1
//     seq 4: deleted.txt delete
func seed(t *testing.T, db *sql.DB) {
	t.Helper()

	stmts := []string{
		// Documents
		`INSERT INTO documents (version_id, path, content, content_hash, created_by)
		 VALUES ('aaaaaaaa-0000-0000-0000-000000000001', 'hello.txt', 'hello world', 'hash_hello_v1', 'alice')`,
		`INSERT INTO documents (version_id, path, content, content_hash, created_by)
		 VALUES ('aaaaaaaa-0000-0000-0000-000000000002', 'hello.txt', 'hello world v2', 'hash_hello_v2', 'alice')`,
		`INSERT INTO documents (version_id, path, content, content_hash, created_by)
		 VALUES ('aaaaaaaa-0000-0000-0000-000000000003', 'world.txt', 'the world', 'hash_world_v1', 'bob')`,
		`INSERT INTO documents (version_id, path, content, content_hash, created_by)
		 VALUES ('aaaaaaaa-0000-0000-0000-000000000004', 'deleted.txt', 'gone soon', 'hash_deleted_v1', 'alice')`,

		// Commits rows — use OVERRIDING SYSTEM VALUE to insert specific sequence numbers.
		`INSERT INTO commits (sequence, branch, message, author) OVERRIDING SYSTEM VALUE VALUES
		 (1, 'main', 'initial commit', 'alice'),
		 (2, 'main', 'update hello',   'alice'),
		 (3, 'main', 'add deleted',    'bob'),
		 (4, 'main', 'remove deleted', 'bob')`,

		// Advance the BIGSERIAL sequence past the manually-inserted values.
		`SELECT setval('commits_sequence_seq', 4, true)`,

		// Sequence 1: initial commit of hello.txt and world.txt
		`INSERT INTO file_commits (commit_id, sequence, path, version_id, branch)
		 VALUES ('cccccccc-0000-0000-0000-000000000001', 1, 'hello.txt', 'aaaaaaaa-0000-0000-0000-000000000001', 'main')`,
		`INSERT INTO file_commits (commit_id, sequence, path, version_id, branch)
		 VALUES ('cccccccc-0000-0000-0000-000000000002', 1, 'world.txt', 'aaaaaaaa-0000-0000-0000-000000000003', 'main')`,

		// Sequence 2: update hello.txt
		`INSERT INTO file_commits (commit_id, sequence, path, version_id, branch)
		 VALUES ('cccccccc-0000-0000-0000-000000000003', 2, 'hello.txt', 'aaaaaaaa-0000-0000-0000-000000000002', 'main')`,

		// Sequence 3: add deleted.txt
		`INSERT INTO file_commits (commit_id, sequence, path, version_id, branch)
		 VALUES ('cccccccc-0000-0000-0000-000000000004', 3, 'deleted.txt', 'aaaaaaaa-0000-0000-0000-000000000004', 'main')`,

		// Sequence 4: delete deleted.txt (version_id = NULL)
		`INSERT INTO file_commits (commit_id, sequence, path, version_id, branch)
		 VALUES ('cccccccc-0000-0000-0000-000000000005', 4, 'deleted.txt', NULL, 'main')`,

		// Update main head
		`UPDATE branches SET head_sequence = 4 WHERE name = 'main'`,
	}

	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\nstatement: %s", err, stmt)
		}
	}
}

func TestMaterializeTree(t *testing.T) {
	db := testutil.TestDB(t, dbpkg.RunMigrations)
	seed(t, db)
	s := store.New(db)
	ctx := context.Background()

	t.Run("head of main", func(t *testing.T) {
		entries, err := s.MaterializeTree(ctx, "default", "main", nil, 100, "")
		if err != nil {
			t.Fatalf("MaterializeTree: %v", err)
		}
		// Should see hello.txt (v2) and world.txt — deleted.txt is excluded.
		if len(entries) != 2 {
			t.Fatalf("expected 2 entries, got %d: %+v", len(entries), entries)
		}
		if entries[0].Path != "hello.txt" {
			t.Errorf("expected hello.txt, got %s", entries[0].Path)
		}
		if entries[0].ContentHash != "hash_hello_v2" {
			t.Errorf("expected hash_hello_v2, got %s", entries[0].ContentHash)
		}
		if entries[1].Path != "world.txt" {
			t.Errorf("expected world.txt, got %s", entries[1].Path)
		}
	})

	t.Run("at sequence 1", func(t *testing.T) {
		seq := int64(1)
		entries, err := s.MaterializeTree(ctx, "default", "main", &seq, 100, "")
		if err != nil {
			t.Fatalf("MaterializeTree: %v", err)
		}
		// At seq 1: hello.txt v1 and world.txt
		if len(entries) != 2 {
			t.Fatalf("expected 2 entries, got %d", len(entries))
		}
		if entries[0].ContentHash != "hash_hello_v1" {
			t.Errorf("expected hash_hello_v1, got %s", entries[0].ContentHash)
		}
	})

	t.Run("at sequence 3 includes deleted file", func(t *testing.T) {
		seq := int64(3)
		entries, err := s.MaterializeTree(ctx, "default", "main", &seq, 100, "")
		if err != nil {
			t.Fatalf("MaterializeTree: %v", err)
		}
		// At seq 3: hello.txt v2, world.txt, and deleted.txt (not yet deleted)
		if len(entries) != 3 {
			t.Fatalf("expected 3 entries, got %d: %+v", len(entries), entries)
		}
	})

	t.Run("pagination", func(t *testing.T) {
		entries, err := s.MaterializeTree(ctx, "default", "main", nil, 1, "")
		if err != nil {
			t.Fatalf("MaterializeTree: %v", err)
		}
		if len(entries) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(entries))
		}
		if entries[0].Path != "hello.txt" {
			t.Errorf("expected hello.txt first, got %s", entries[0].Path)
		}

		// Second page
		entries2, err := s.MaterializeTree(ctx, "default", "main", nil, 1, entries[0].Path)
		if err != nil {
			t.Fatalf("MaterializeTree page 2: %v", err)
		}
		if len(entries2) != 1 {
			t.Fatalf("expected 1 entry on page 2, got %d", len(entries2))
		}
		if entries2[0].Path != "world.txt" {
			t.Errorf("expected world.txt, got %s", entries2[0].Path)
		}
	})

	t.Run("pagination empty after last page", func(t *testing.T) {
		// Using the last path as cursor should return empty.
		entries, err := s.MaterializeTree(ctx, "default", "main", nil, 100, "world.txt")
		if err != nil {
			t.Fatalf("MaterializeTree: %v", err)
		}
		if len(entries) != 0 {
			t.Fatalf("expected 0 entries after last path, got %d: %+v", len(entries), entries)
		}
	})

	t.Run("pagination cursor past all entries", func(t *testing.T) {
		// A cursor lexicographically beyond all paths yields empty.
		entries, err := s.MaterializeTree(ctx, "default", "main", nil, 100, "zzz.txt")
		if err != nil {
			t.Fatalf("MaterializeTree: %v", err)
		}
		if len(entries) != 0 {
			t.Fatalf("expected 0 entries for cursor past all paths, got %d: %+v", len(entries), entries)
		}
	})

	t.Run("pagination limit zero uses default", func(t *testing.T) {
		// limit=0 should fall back to defaultLimit (100) and return all entries.
		entries, err := s.MaterializeTree(ctx, "default", "main", nil, 0, "")
		if err != nil {
			t.Fatalf("MaterializeTree: %v", err)
		}
		if len(entries) != 2 {
			t.Fatalf("expected 2 entries with limit=0, got %d", len(entries))
		}
	})

	t.Run("pagination limit exactly equals result count", func(t *testing.T) {
		// Limit equals the number of files; no truncation should occur.
		entries, err := s.MaterializeTree(ctx, "default", "main", nil, 2, "")
		if err != nil {
			t.Fatalf("MaterializeTree: %v", err)
		}
		if len(entries) != 2 {
			t.Fatalf("expected exactly 2 entries, got %d", len(entries))
		}
	})
}

func TestGetFile(t *testing.T) {
	db := testutil.TestDB(t, dbpkg.RunMigrations)
	seed(t, db)
	s := store.New(db)
	ctx := context.Background()

	t.Run("current version", func(t *testing.T) {
		fc, err := s.GetFile(ctx, "default", "main", "hello.txt", nil)
		if err != nil {
			t.Fatalf("GetFile: %v", err)
		}
		if fc == nil {
			t.Fatal("expected file, got nil")
		}
		if string(fc.Content) != "hello world v2" {
			t.Errorf("expected 'hello world v2', got %q", string(fc.Content))
		}
		if fc.ContentHash != "hash_hello_v2" {
			t.Errorf("expected hash_hello_v2, got %s", fc.ContentHash)
		}
	})

	t.Run("at sequence 1", func(t *testing.T) {
		seq := int64(1)
		fc, err := s.GetFile(ctx, "default", "main", "hello.txt", &seq)
		if err != nil {
			t.Fatalf("GetFile: %v", err)
		}
		if fc == nil {
			t.Fatal("expected file, got nil")
		}
		if string(fc.Content) != "hello world" {
			t.Errorf("expected 'hello world', got %q", string(fc.Content))
		}
	})

	t.Run("deleted file returns nil", func(t *testing.T) {
		fc, err := s.GetFile(ctx, "default", "main", "deleted.txt", nil)
		if err != nil {
			t.Fatalf("GetFile: %v", err)
		}
		if fc != nil {
			t.Errorf("expected nil for deleted file, got %+v", fc)
		}
	})

	t.Run("nonexistent file", func(t *testing.T) {
		fc, err := s.GetFile(ctx, "default", "main", "nope.txt", nil)
		if err != nil {
			t.Fatalf("GetFile: %v", err)
		}
		if fc != nil {
			t.Errorf("expected nil for nonexistent file, got %+v", fc)
		}
	})
}

func TestGetFileHistory(t *testing.T) {
	db := testutil.TestDB(t, dbpkg.RunMigrations)
	seed(t, db)
	s := store.New(db)
	ctx := context.Background()

	t.Run("hello.txt history", func(t *testing.T) {
		entries, err := s.GetFileHistory(ctx, "default", "main", "hello.txt", 100, nil)
		if err != nil {
			t.Fatalf("GetFileHistory: %v", err)
		}
		if len(entries) != 2 {
			t.Fatalf("expected 2 history entries, got %d", len(entries))
		}
		// Most recent first
		if entries[0].Sequence != 2 {
			t.Errorf("expected sequence 2 first, got %d", entries[0].Sequence)
		}
		if entries[1].Sequence != 1 {
			t.Errorf("expected sequence 1 second, got %d", entries[1].Sequence)
		}
	})

	t.Run("deleted.txt history includes delete", func(t *testing.T) {
		entries, err := s.GetFileHistory(ctx, "default", "main", "deleted.txt", 100, nil)
		if err != nil {
			t.Fatalf("GetFileHistory: %v", err)
		}
		if len(entries) != 2 {
			t.Fatalf("expected 2 history entries, got %d", len(entries))
		}
		// seq 4 is the delete (version_id nil)
		if entries[0].Sequence != 4 {
			t.Errorf("expected sequence 4, got %d", entries[0].Sequence)
		}
		if entries[0].VersionID != nil {
			t.Errorf("expected nil version_id for delete, got %v", *entries[0].VersionID)
		}
	})

	t.Run("pagination with after cursor", func(t *testing.T) {
		after := int64(2)
		entries, err := s.GetFileHistory(ctx, "default", "main", "hello.txt", 100, &after)
		if err != nil {
			t.Fatalf("GetFileHistory: %v", err)
		}
		if len(entries) != 1 {
			t.Fatalf("expected 1 entry after seq 2, got %d", len(entries))
		}
		if entries[0].Sequence != 1 {
			t.Errorf("expected sequence 1, got %d", entries[0].Sequence)
		}
	})

	t.Run("pagination empty after oldest entry", func(t *testing.T) {
		// Cursor at the oldest sequence means nothing older exists.
		after := int64(1)
		entries, err := s.GetFileHistory(ctx, "default", "main", "hello.txt", 100, &after)
		if err != nil {
			t.Fatalf("GetFileHistory: %v", err)
		}
		if len(entries) != 0 {
			t.Fatalf("expected 0 entries after oldest sequence, got %d: %+v", len(entries), entries)
		}
	})

	t.Run("pagination limit zero uses default", func(t *testing.T) {
		// limit=0 should fall back to defaultLimit (100) and return all entries.
		entries, err := s.GetFileHistory(ctx, "default", "main", "hello.txt", 0, nil)
		if err != nil {
			t.Fatalf("GetFileHistory: %v", err)
		}
		if len(entries) != 2 {
			t.Fatalf("expected 2 history entries with limit=0, got %d", len(entries))
		}
	})

	t.Run("pagination multi-page traversal", func(t *testing.T) {
		// Page through hello.txt history one entry at a time.
		page1, err := s.GetFileHistory(ctx, "default", "main", "hello.txt", 1, nil)
		if err != nil {
			t.Fatalf("GetFileHistory page 1: %v", err)
		}
		if len(page1) != 1 {
			t.Fatalf("expected 1 entry on page 1, got %d", len(page1))
		}
		if page1[0].Sequence != 2 {
			t.Errorf("expected sequence 2 on page 1, got %d", page1[0].Sequence)
		}

		// Cursor at sequence 2 → page 2 should return sequence 1.
		cursor := page1[0].Sequence
		page2, err := s.GetFileHistory(ctx, "default", "main", "hello.txt", 1, &cursor)
		if err != nil {
			t.Fatalf("GetFileHistory page 2: %v", err)
		}
		if len(page2) != 1 {
			t.Fatalf("expected 1 entry on page 2, got %d", len(page2))
		}
		if page2[0].Sequence != 1 {
			t.Errorf("expected sequence 1 on page 2, got %d", page2[0].Sequence)
		}

		// Cursor at sequence 1 → page 3 should be empty.
		cursor = page2[0].Sequence
		page3, err := s.GetFileHistory(ctx, "default", "main", "hello.txt", 1, &cursor)
		if err != nil {
			t.Fatalf("GetFileHistory page 3: %v", err)
		}
		if len(page3) != 0 {
			t.Fatalf("expected empty page 3, got %d entries", len(page3))
		}
	})
}

func TestGetCommit(t *testing.T) {
	db := testutil.TestDB(t, dbpkg.RunMigrations)
	seed(t, db)
	s := store.New(db)
	ctx := context.Background()

	t.Run("multi-file commit", func(t *testing.T) {
		detail, err := s.GetCommit(ctx, "default", 1)
		if err != nil {
			t.Fatalf("GetCommit: %v", err)
		}
		if detail == nil {
			t.Fatal("expected commit detail, got nil")
		}
		if detail.Sequence != 1 {
			t.Errorf("expected sequence 1, got %d", detail.Sequence)
		}
		if detail.Message != "initial commit" {
			t.Errorf("expected 'initial commit', got %q", detail.Message)
		}
		if detail.Author != "alice" {
			t.Errorf("expected author alice, got %q", detail.Author)
		}
		if len(detail.Files) != 2 {
			t.Fatalf("expected 2 files in commit, got %d", len(detail.Files))
		}
		// Files ordered by path
		if detail.Files[0].Path != "hello.txt" {
			t.Errorf("expected hello.txt first, got %s", detail.Files[0].Path)
		}
		if detail.Files[1].Path != "world.txt" {
			t.Errorf("expected world.txt second, got %s", detail.Files[1].Path)
		}
	})

	t.Run("single-file commit", func(t *testing.T) {
		detail, err := s.GetCommit(ctx, "default", 2)
		if err != nil {
			t.Fatalf("GetCommit: %v", err)
		}
		if detail == nil {
			t.Fatal("expected commit detail, got nil")
		}
		if len(detail.Files) != 1 {
			t.Fatalf("expected 1 file, got %d", len(detail.Files))
		}
	})

	t.Run("delete commit has nil version_id", func(t *testing.T) {
		detail, err := s.GetCommit(ctx, "default", 4)
		if err != nil {
			t.Fatalf("GetCommit: %v", err)
		}
		if detail == nil {
			t.Fatal("expected commit detail, got nil")
		}
		if detail.Files[0].VersionID != nil {
			t.Errorf("expected nil version_id for delete commit")
		}
	})

	t.Run("nonexistent sequence", func(t *testing.T) {
		detail, err := s.GetCommit(ctx, "default", 999)
		if err != nil {
			t.Fatalf("GetCommit: %v", err)
		}
		if detail != nil {
			t.Errorf("expected nil for nonexistent sequence, got %+v", detail)
		}
	})
}

func TestListBranches(t *testing.T) {
	db := testutil.TestDB(t, dbpkg.RunMigrations)
	seed(t, db)
	s := store.New(db)
	ctx := context.Background()

	// Add a feature branch.
	stmts := []string{
		`INSERT INTO branches (name, head_sequence, base_sequence, status) VALUES ('feature/a', 5, 2, 'active')`,
		`INSERT INTO branches (name, head_sequence, base_sequence, status) VALUES ('feature/merged', 3, 1, 'merged')`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt)
		}
	}

	t.Run("all branches", func(t *testing.T) {
		branches, err := s.ListBranches(ctx, "default", "")
		if err != nil {
			t.Fatalf("ListBranches: %v", err)
		}
		if len(branches) != 3 {
			t.Fatalf("expected 3 branches, got %d: %+v", len(branches), branches)
		}
		// Ordered by name
		if branches[0].Name != "feature/a" {
			t.Errorf("expected feature/a first, got %s", branches[0].Name)
		}
	})

	t.Run("filter active", func(t *testing.T) {
		branches, err := s.ListBranches(ctx, "default", "active")
		if err != nil {
			t.Fatalf("ListBranches: %v", err)
		}
		if len(branches) != 2 {
			t.Fatalf("expected 2 active branches, got %d", len(branches))
		}
	})

	t.Run("filter merged", func(t *testing.T) {
		branches, err := s.ListBranches(ctx, "default", "merged")
		if err != nil {
			t.Fatalf("ListBranches: %v", err)
		}
		if len(branches) != 1 {
			t.Fatalf("expected 1 merged branch, got %d", len(branches))
		}
		if branches[0].Name != "feature/merged" {
			t.Errorf("expected feature/merged, got %s", branches[0].Name)
		}
	})
}

func TestGetDiff(t *testing.T) {
	db := testutil.TestDB(t, dbpkg.RunMigrations)
	seed(t, db)
	s := store.New(db)
	ctx := context.Background()

	// Create a feature branch forked at sequence 2.
	stmts := []string{
		`INSERT INTO branches (name, head_sequence, base_sequence, status) VALUES ('feature/diff', 6, 2, 'active')`,
		// Branch changes: add new.txt, modify hello.txt
		`INSERT INTO documents (version_id, path, content, content_hash, created_by)
		 VALUES ('aaaaaaaa-0000-0000-0000-000000000020', 'new.txt', 'new file', 'hash_new', 'carol')`,
		`INSERT INTO documents (version_id, path, content, content_hash, created_by)
		 VALUES ('aaaaaaaa-0000-0000-0000-000000000021', 'hello.txt', 'hello v3 branch', 'hash_hello_v3', 'carol')`,
		// Insert commits rows for sequences 5 and 6.
		`INSERT INTO commits (sequence, branch, message, author) OVERRIDING SYSTEM VALUE VALUES
		 (5, 'feature/diff', 'add new',      'carol'),
		 (6, 'feature/diff', 'update hello', 'carol')`,
		`SELECT setval('commits_sequence_seq', 6, true)`,
		`INSERT INTO file_commits (commit_id, sequence, path, version_id, branch)
		 VALUES ('cccccccc-0000-0000-0000-000000000020', 5, 'new.txt', 'aaaaaaaa-0000-0000-0000-000000000020', 'feature/diff')`,
		`INSERT INTO file_commits (commit_id, sequence, path, version_id, branch)
		 VALUES ('cccccccc-0000-0000-0000-000000000021', 6, 'hello.txt', 'aaaaaaaa-0000-0000-0000-000000000021', 'feature/diff')`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt)
		}
	}

	t.Run("diff shows branch changes", func(t *testing.T) {
		result, err := s.GetDiff(ctx, "default", "feature/diff")
		if err != nil {
			t.Fatalf("GetDiff: %v", err)
		}
		if result == nil {
			t.Fatal("expected diff result, got nil")
		}
		if len(result.BranchChanges) != 2 {
			t.Fatalf("expected 2 changed files, got %d: %+v", len(result.BranchChanges), result.BranchChanges)
		}
	})

	t.Run("diff detects conflicts", func(t *testing.T) {
		// Main also changed hello.txt at seq 3-4 (deleted.txt at seq 3, 4),
		// but hello.txt was only changed at seq 2 on main (which is <= base_sequence=2).
		// So no conflicts by default from the seed data because main changes at seq 3,4 are deleted.txt.
		result, err := s.GetDiff(ctx, "default", "feature/diff")
		if err != nil {
			t.Fatalf("GetDiff: %v", err)
		}
		// hello.txt changed on main at seq 2 which is <= base (2), not a conflict.
		// deleted.txt changed on main at seq 3,4 but not on branch. Not a conflict.
		if len(result.Conflicts) != 0 {
			t.Errorf("expected 0 conflicts, got %d: %+v", len(result.Conflicts), result.Conflicts)
		}
	})

	t.Run("diff includes main_changes", func(t *testing.T) {
		// Main changed deleted.txt at seq 3 (add) and seq 4 (delete) since base=2.
		result, err := s.GetDiff(ctx, "default", "feature/diff")
		if err != nil {
			t.Fatalf("GetDiff: %v", err)
		}
		if len(result.MainChanges) != 1 {
			t.Fatalf("expected 1 main change (deleted.txt), got %d: %+v", len(result.MainChanges), result.MainChanges)
		}
		if result.MainChanges[0].Path != "deleted.txt" {
			t.Errorf("expected main change on deleted.txt, got %q", result.MainChanges[0].Path)
		}
	})

	t.Run("nonexistent branch", func(t *testing.T) {
		result, err := s.GetDiff(ctx, "default", "nonexistent")
		if err != nil {
			t.Fatalf("GetDiff: %v", err)
		}
		if result != nil {
			t.Errorf("expected nil for nonexistent branch, got %+v", result)
		}
	})
}

func TestGetDiff_WithConflict(t *testing.T) {
	db := testutil.TestDB(t, dbpkg.RunMigrations)
	s := store.New(db)
	ctx := context.Background()

	// Minimal setup: commit to main, create branch, change same file on both.
	stmts := []string{
		// Commits rows for sequences 1, 2, 3.
		`INSERT INTO commits (sequence, branch, message, author) OVERRIDING SYSTEM VALUE VALUES
		 (1, 'main',             'init',        'alice'),
		 (2, 'main',             'main edit',   'alice'),
		 (3, 'feature/conflict', 'branch edit', 'bob')`,
		`SELECT setval('commits_sequence_seq', 3, true)`,

		// Initial main commit
		`INSERT INTO documents (version_id, path, content, content_hash, created_by)
		 VALUES ('dddddddd-0000-0000-0000-000000000001', 'shared.txt', 'original', 'hash_orig', 'alice')`,
		`INSERT INTO file_commits (commit_id, sequence, path, version_id, branch)
		 VALUES ('eeeeeeee-0000-0000-0000-000000000001', 1, 'shared.txt', 'dddddddd-0000-0000-0000-000000000001', 'main')`,
		`UPDATE branches SET head_sequence = 1 WHERE name = 'main'`,

		// Branch at base=1
		`INSERT INTO branches (name, head_sequence, base_sequence, status) VALUES ('feature/conflict', 3, 1, 'active')`,

		// Main changes shared.txt after branch point
		`INSERT INTO documents (version_id, path, content, content_hash, created_by)
		 VALUES ('dddddddd-0000-0000-0000-000000000002', 'shared.txt', 'main edit', 'hash_main', 'alice')`,
		`INSERT INTO file_commits (commit_id, sequence, path, version_id, branch)
		 VALUES ('eeeeeeee-0000-0000-0000-000000000002', 2, 'shared.txt', 'dddddddd-0000-0000-0000-000000000002', 'main')`,

		// Branch also changes shared.txt
		`INSERT INTO documents (version_id, path, content, content_hash, created_by)
		 VALUES ('dddddddd-0000-0000-0000-000000000003', 'shared.txt', 'branch edit', 'hash_branch', 'bob')`,
		`INSERT INTO file_commits (commit_id, sequence, path, version_id, branch)
		 VALUES ('eeeeeeee-0000-0000-0000-000000000003', 3, 'shared.txt', 'dddddddd-0000-0000-0000-000000000003', 'feature/conflict')`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt)
		}
	}

	result, err := s.GetDiff(ctx, "default", "feature/conflict")
	if err != nil {
		t.Fatalf("GetDiff: %v", err)
	}
	if len(result.BranchChanges) != 1 {
		t.Fatalf("expected 1 changed, got %d", len(result.BranchChanges))
	}
	if len(result.Conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(result.Conflicts))
	}
	if result.Conflicts[0].Path != "shared.txt" {
		t.Errorf("expected conflict on shared.txt, got %q", result.Conflicts[0].Path)
	}
}

func TestBranchTree(t *testing.T) {
	db := testutil.TestDB(t, dbpkg.RunMigrations)
	seed(t, db)
	s := store.New(db)
	ctx := context.Background()

	// Create a feature branch forked from main at sequence 2
	stmts := []string{
		`INSERT INTO branches (name, head_sequence, base_sequence, status) VALUES ('feature/test', 5, 2, 'active')`,
		// Add a new file on the branch
		`INSERT INTO documents (version_id, path, content, content_hash, created_by)
		 VALUES ('aaaaaaaa-0000-0000-0000-000000000010', 'new.txt', 'new file', 'hash_new', 'carol')`,
		// Insert a commits row for sequence 5.
		`INSERT INTO commits (sequence, branch, message, author) OVERRIDING SYSTEM VALUE VALUES
		 (5, 'feature/test', 'add new file', 'carol')`,
		`SELECT setval('commits_sequence_seq', 5, true)`,
		`INSERT INTO file_commits (commit_id, sequence, path, version_id, branch)
		 VALUES ('cccccccc-0000-0000-0000-000000000010', 5, 'new.txt', 'aaaaaaaa-0000-0000-0000-000000000010', 'feature/test')`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed branch: %v\n%s", err, stmt)
		}
	}

	entries, err := s.MaterializeTree(ctx, "default", "feature/test", nil, 100, "")
	if err != nil {
		t.Fatalf("MaterializeTree: %v", err)
	}

	// Branch inherits main at seq 2: hello.txt (v2), world.txt
	// Plus branch's own: new.txt
	// Note: deleted.txt was added at seq 3 and deleted at seq 4 on main,
	// both after the branch point (seq 2), so it doesn't appear.
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d: %+v", len(entries), entries)
	}

	paths := make(map[string]bool)
	for _, e := range entries {
		paths[e.Path] = true
	}
	for _, want := range []string{"hello.txt", "new.txt", "world.txt"} {
		if !paths[want] {
			t.Errorf("expected %s in tree", want)
		}
	}
}
