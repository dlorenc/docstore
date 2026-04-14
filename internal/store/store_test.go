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

		// Sequence 1: initial commit of hello.txt and world.txt
		`INSERT INTO file_commits (commit_id, sequence, path, version_id, branch, message, author)
		 VALUES ('cccccccc-0000-0000-0000-000000000001', 1, 'hello.txt', 'aaaaaaaa-0000-0000-0000-000000000001', 'main', 'initial commit', 'alice')`,
		`INSERT INTO file_commits (commit_id, sequence, path, version_id, branch, message, author)
		 VALUES ('cccccccc-0000-0000-0000-000000000002', 1, 'world.txt', 'aaaaaaaa-0000-0000-0000-000000000003', 'main', 'initial commit', 'alice')`,

		// Sequence 2: update hello.txt
		`INSERT INTO file_commits (commit_id, sequence, path, version_id, branch, message, author)
		 VALUES ('cccccccc-0000-0000-0000-000000000003', 2, 'hello.txt', 'aaaaaaaa-0000-0000-0000-000000000002', 'main', 'update hello', 'alice')`,

		// Sequence 3: add deleted.txt
		`INSERT INTO file_commits (commit_id, sequence, path, version_id, branch, message, author)
		 VALUES ('cccccccc-0000-0000-0000-000000000004', 3, 'deleted.txt', 'aaaaaaaa-0000-0000-0000-000000000004', 'main', 'add deleted', 'bob')`,

		// Sequence 4: delete deleted.txt (version_id = NULL)
		`INSERT INTO file_commits (commit_id, sequence, path, version_id, branch, message, author)
		 VALUES ('cccccccc-0000-0000-0000-000000000005', 4, 'deleted.txt', NULL, 'main', 'remove deleted', 'bob')`,

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
	db := testutil.TestDB(t, dbpkg.MigrationSQL)
	seed(t, db)
	s := store.New(db)
	ctx := context.Background()

	t.Run("head of main", func(t *testing.T) {
		entries, err := s.MaterializeTree(ctx, "main", nil, 100, "")
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
		entries, err := s.MaterializeTree(ctx, "main", &seq, 100, "")
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
		entries, err := s.MaterializeTree(ctx, "main", &seq, 100, "")
		if err != nil {
			t.Fatalf("MaterializeTree: %v", err)
		}
		// At seq 3: hello.txt v2, world.txt, and deleted.txt (not yet deleted)
		if len(entries) != 3 {
			t.Fatalf("expected 3 entries, got %d: %+v", len(entries), entries)
		}
	})

	t.Run("pagination", func(t *testing.T) {
		entries, err := s.MaterializeTree(ctx, "main", nil, 1, "")
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
		entries2, err := s.MaterializeTree(ctx, "main", nil, 1, entries[0].Path)
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
}

func TestGetFile(t *testing.T) {
	db := testutil.TestDB(t, dbpkg.MigrationSQL)
	seed(t, db)
	s := store.New(db)
	ctx := context.Background()

	t.Run("current version", func(t *testing.T) {
		fc, err := s.GetFile(ctx, "main", "hello.txt", nil)
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
		fc, err := s.GetFile(ctx, "main", "hello.txt", &seq)
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
		fc, err := s.GetFile(ctx, "main", "deleted.txt", nil)
		if err != nil {
			t.Fatalf("GetFile: %v", err)
		}
		if fc != nil {
			t.Errorf("expected nil for deleted file, got %+v", fc)
		}
	})

	t.Run("nonexistent file", func(t *testing.T) {
		fc, err := s.GetFile(ctx, "main", "nope.txt", nil)
		if err != nil {
			t.Fatalf("GetFile: %v", err)
		}
		if fc != nil {
			t.Errorf("expected nil for nonexistent file, got %+v", fc)
		}
	})
}

func TestGetFileHistory(t *testing.T) {
	db := testutil.TestDB(t, dbpkg.MigrationSQL)
	seed(t, db)
	s := store.New(db)
	ctx := context.Background()

	t.Run("hello.txt history", func(t *testing.T) {
		entries, err := s.GetFileHistory(ctx, "main", "hello.txt", 100, nil)
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
		entries, err := s.GetFileHistory(ctx, "main", "deleted.txt", 100, nil)
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
		entries, err := s.GetFileHistory(ctx, "main", "hello.txt", 100, &after)
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
}

func TestGetCommit(t *testing.T) {
	db := testutil.TestDB(t, dbpkg.MigrationSQL)
	seed(t, db)
	s := store.New(db)
	ctx := context.Background()

	t.Run("multi-file commit", func(t *testing.T) {
		detail, err := s.GetCommit(ctx, 1)
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
		detail, err := s.GetCommit(ctx, 2)
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
		detail, err := s.GetCommit(ctx, 4)
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
		detail, err := s.GetCommit(ctx, 999)
		if err != nil {
			t.Fatalf("GetCommit: %v", err)
		}
		if detail != nil {
			t.Errorf("expected nil for nonexistent sequence, got %+v", detail)
		}
	})
}

func TestBranchTree(t *testing.T) {
	db := testutil.TestDB(t, dbpkg.MigrationSQL)
	seed(t, db)
	s := store.New(db)
	ctx := context.Background()

	// Create a feature branch forked from main at sequence 2
	stmts := []string{
		`INSERT INTO branches (name, head_sequence, base_sequence, status) VALUES ('feature/test', 5, 2, 'active')`,
		// Add a new file on the branch
		`INSERT INTO documents (version_id, path, content, content_hash, created_by)
		 VALUES ('aaaaaaaa-0000-0000-0000-000000000010', 'new.txt', 'new file', 'hash_new', 'carol')`,
		`INSERT INTO file_commits (commit_id, sequence, path, version_id, branch, message, author)
		 VALUES ('cccccccc-0000-0000-0000-000000000010', 5, 'new.txt', 'aaaaaaaa-0000-0000-0000-000000000010', 'feature/test', 'add new file', 'carol')`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed branch: %v\n%s", err, stmt)
		}
	}

	entries, err := s.MaterializeTree(ctx, "feature/test", nil, 100, "")
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
