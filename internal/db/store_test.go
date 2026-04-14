package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/testutil"
)

func TestCommit_SingleFile(t *testing.T) {
	d := testutil.TestDB(t, MigrationSQL)
	s := NewStore(d)
	ctx := context.Background()

	resp, err := s.Commit(ctx, model.CommitRequest{
		Branch:  "main",
		Files:   []model.FileChange{{Path: "hello.txt", Content: []byte("hello world")}},
		Message: "first commit",
		Author:  "alice@example.com",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	if resp.Sequence != 1 {
		t.Errorf("expected sequence 1, got %d", resp.Sequence)
	}
	if len(resp.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(resp.Files))
	}
	if resp.Files[0].Path != "hello.txt" {
		t.Errorf("expected path hello.txt, got %q", resp.Files[0].Path)
	}
	if resp.Files[0].VersionID == nil {
		t.Fatal("expected non-nil version_id")
	}

	// Verify branch head was advanced.
	var headSeq int64
	err = d.QueryRow("SELECT head_sequence FROM branches WHERE name = 'main'").Scan(&headSeq)
	if err != nil {
		t.Fatalf("query branch: %v", err)
	}
	if headSeq != 1 {
		t.Errorf("expected branch head 1, got %d", headSeq)
	}
}

func TestCommit_MultipleFiles(t *testing.T) {
	d := testutil.TestDB(t, MigrationSQL)
	s := NewStore(d)
	ctx := context.Background()

	resp, err := s.Commit(ctx, model.CommitRequest{
		Branch: "main",
		Files: []model.FileChange{
			{Path: "a.txt", Content: []byte("aaa")},
			{Path: "b.txt", Content: []byte("bbb")},
			{Path: "c.txt", Content: []byte("ccc")},
		},
		Message: "add three files",
		Author:  "alice@example.com",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	if resp.Sequence != 1 {
		t.Errorf("expected sequence 1, got %d", resp.Sequence)
	}
	if len(resp.Files) != 3 {
		t.Fatalf("expected 3 files, got %d", len(resp.Files))
	}

	// All file_commits should share the same sequence.
	var count int
	err = d.QueryRow("SELECT count(*) FROM file_commits WHERE sequence = $1", resp.Sequence).Scan(&count)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 file_commits at sequence %d, got %d", resp.Sequence, count)
	}
}

func TestCommit_ContentDedup(t *testing.T) {
	d := testutil.TestDB(t, MigrationSQL)
	s := NewStore(d)
	ctx := context.Background()

	content := []byte("identical content")

	// Commit the same content as two different files.
	resp, err := s.Commit(ctx, model.CommitRequest{
		Branch: "main",
		Files: []model.FileChange{
			{Path: "file1.txt", Content: content},
			{Path: "file2.txt", Content: content},
		},
		Message: "dedup test",
		Author:  "alice@example.com",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Both files should reference the same version_id.
	if *resp.Files[0].VersionID != *resp.Files[1].VersionID {
		t.Errorf("expected same version_id for identical content, got %q and %q",
			*resp.Files[0].VersionID, *resp.Files[1].VersionID)
	}

	// Only one document row should exist.
	h := sha256.Sum256(content)
	hash := hex.EncodeToString(h[:])
	var docCount int
	err = d.QueryRow("SELECT count(*) FROM documents WHERE content_hash = $1", hash).Scan(&docCount)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if docCount != 1 {
		t.Errorf("expected 1 document, got %d", docCount)
	}
}

func TestCommit_ContentDedupAcrossCommits(t *testing.T) {
	d := testutil.TestDB(t, MigrationSQL)
	s := NewStore(d)
	ctx := context.Background()

	content := []byte("shared content")

	resp1, err := s.Commit(ctx, model.CommitRequest{
		Branch:  "main",
		Files:   []model.FileChange{{Path: "first.txt", Content: content}},
		Message: "commit 1",
		Author:  "alice@example.com",
	})
	if err != nil {
		t.Fatalf("commit 1: %v", err)
	}

	resp2, err := s.Commit(ctx, model.CommitRequest{
		Branch:  "main",
		Files:   []model.FileChange{{Path: "second.txt", Content: content}},
		Message: "commit 2",
		Author:  "bob@example.com",
	})
	if err != nil {
		t.Fatalf("commit 2: %v", err)
	}

	// Same version_id across commits.
	if *resp1.Files[0].VersionID != *resp2.Files[0].VersionID {
		t.Errorf("expected same version_id across commits, got %q and %q",
			*resp1.Files[0].VersionID, *resp2.Files[0].VersionID)
	}

	// Sequences should increment.
	if resp2.Sequence != 2 {
		t.Errorf("expected sequence 2, got %d", resp2.Sequence)
	}
}

func TestCommit_SequenceIncrementsPerCommit(t *testing.T) {
	d := testutil.TestDB(t, MigrationSQL)
	s := NewStore(d)
	ctx := context.Background()

	for i := 1; i <= 3; i++ {
		resp, err := s.Commit(ctx, model.CommitRequest{
			Branch:  "main",
			Files:   []model.FileChange{{Path: "file.txt", Content: []byte("v" + string(rune('0'+i)))}},
			Message: "commit",
			Author:  "alice@example.com",
		})
		if err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
		if resp.Sequence != int64(i) {
			t.Errorf("commit %d: expected sequence %d, got %d", i, i, resp.Sequence)
		}
	}
}

func TestCommit_DeleteFile(t *testing.T) {
	d := testutil.TestDB(t, MigrationSQL)
	s := NewStore(d)
	ctx := context.Background()

	// First, create a file.
	_, err := s.Commit(ctx, model.CommitRequest{
		Branch:  "main",
		Files:   []model.FileChange{{Path: "doomed.txt", Content: []byte("will be deleted")}},
		Message: "create file",
		Author:  "alice@example.com",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Delete the file (nil content).
	resp, err := s.Commit(ctx, model.CommitRequest{
		Branch:  "main",
		Files:   []model.FileChange{{Path: "doomed.txt", Content: nil}},
		Message: "delete file",
		Author:  "alice@example.com",
	})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}

	if resp.Sequence != 2 {
		t.Errorf("expected sequence 2, got %d", resp.Sequence)
	}
	if resp.Files[0].VersionID != nil {
		t.Errorf("expected nil version_id for delete, got %v", resp.Files[0].VersionID)
	}

	// The file_commit should have NULL version_id.
	var versionID *string
	err = d.QueryRow(
		"SELECT version_id FROM file_commits WHERE sequence = $1 AND path = 'doomed.txt'",
		resp.Sequence,
	).Scan(&versionID)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if versionID != nil {
		t.Errorf("expected NULL version_id in DB, got %v", *versionID)
	}
}

func TestCommit_BranchNotFound(t *testing.T) {
	d := testutil.TestDB(t, MigrationSQL)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.Commit(ctx, model.CommitRequest{
		Branch:  "nonexistent",
		Files:   []model.FileChange{{Path: "a.txt", Content: []byte("x")}},
		Message: "m",
		Author:  "a",
	})
	if err != ErrBranchNotFound {
		t.Fatalf("expected ErrBranchNotFound, got %v", err)
	}
}

func TestCommit_BranchNotActive(t *testing.T) {
	d := testutil.TestDB(t, MigrationSQL)
	s := NewStore(d)
	ctx := context.Background()

	// Create a branch and mark it as merged.
	_, err := d.Exec(
		"INSERT INTO branches (name, head_sequence, base_sequence, status) VALUES ('merged-br', 0, 0, 'merged')",
	)
	if err != nil {
		t.Fatalf("insert branch: %v", err)
	}

	_, err = s.Commit(ctx, model.CommitRequest{
		Branch:  "merged-br",
		Files:   []model.FileChange{{Path: "a.txt", Content: []byte("x")}},
		Message: "m",
		Author:  "a",
	})
	if err != ErrBranchNotActive {
		t.Fatalf("expected ErrBranchNotActive, got %v", err)
	}
}

// --- CreateBranch tests ---

func TestCreateBranch_Success(t *testing.T) {
	d := testutil.TestDB(t, MigrationSQL)
	s := NewStore(d)
	ctx := context.Background()

	// First commit to main to advance head.
	_, err := s.Commit(ctx, model.CommitRequest{
		Branch:  "main",
		Files:   []model.FileChange{{Path: "file.txt", Content: []byte("hello")}},
		Message: "initial",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	resp, err := s.CreateBranch(ctx, model.CreateBranchRequest{Name: "feature/test"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	if resp.Name != "feature/test" {
		t.Errorf("expected name feature/test, got %q", resp.Name)
	}
	if resp.BaseSequence != 1 {
		t.Errorf("expected base_sequence 1, got %d", resp.BaseSequence)
	}

	// Verify branch row exists.
	var headSeq, baseSeq int64
	var status string
	err = d.QueryRow("SELECT head_sequence, base_sequence, status FROM branches WHERE name = 'feature/test'").Scan(&headSeq, &baseSeq, &status)
	if err != nil {
		t.Fatalf("query branch: %v", err)
	}
	if headSeq != 1 || baseSeq != 1 {
		t.Errorf("expected head=1, base=1, got head=%d, base=%d", headSeq, baseSeq)
	}
	if status != "active" {
		t.Errorf("expected active, got %q", status)
	}
}

func TestCreateBranch_Duplicate(t *testing.T) {
	d := testutil.TestDB(t, MigrationSQL)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.CreateBranch(ctx, model.CreateBranchRequest{Name: "feature/dup"})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}

	_, err = s.CreateBranch(ctx, model.CreateBranchRequest{Name: "feature/dup"})
	if err != ErrBranchExists {
		t.Fatalf("expected ErrBranchExists, got %v", err)
	}
}

// --- Merge tests ---

func TestMerge_Success(t *testing.T) {
	d := testutil.TestDB(t, MigrationSQL)
	s := NewStore(d)
	ctx := context.Background()

	// Commit to main.
	_, err := s.Commit(ctx, model.CommitRequest{
		Branch:  "main",
		Files:   []model.FileChange{{Path: "base.txt", Content: []byte("base")}},
		Message: "initial",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Create branch.
	_, err = s.CreateBranch(ctx, model.CreateBranchRequest{Name: "feature/merge"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	// Commit to branch.
	_, err = s.Commit(ctx, model.CommitRequest{
		Branch:  "feature/merge",
		Files:   []model.FileChange{{Path: "new.txt", Content: []byte("new file")}},
		Message: "add new file",
		Author:  "bob",
	})
	if err != nil {
		t.Fatalf("branch commit: %v", err)
	}

	// Merge.
	resp, conflicts, err := s.Merge(ctx, model.MergeRequest{Branch: "feature/merge", Author: "carol"})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if conflicts != nil {
		t.Fatalf("expected no conflicts, got %v", conflicts)
	}
	// With global BIGSERIAL sequences: commit-to-main=1, commit-to-branch=2, merge=3.
	if resp.Sequence != 3 {
		t.Errorf("expected merge sequence 3, got %d", resp.Sequence)
	}

	// Verify main head advanced.
	var mainHead int64
	err = d.QueryRow("SELECT head_sequence FROM branches WHERE name = 'main'").Scan(&mainHead)
	if err != nil {
		t.Fatalf("query main: %v", err)
	}
	if mainHead != 3 {
		t.Errorf("expected main head 3, got %d", mainHead)
	}

	// Verify branch is marked as merged.
	var status string
	err = d.QueryRow("SELECT status FROM branches WHERE name = 'feature/merge'").Scan(&status)
	if err != nil {
		t.Fatalf("query branch: %v", err)
	}
	if status != "merged" {
		t.Errorf("expected merged, got %q", status)
	}

	// Verify new.txt is visible on main.
	var count int
	err = d.QueryRow("SELECT count(*) FROM file_commits WHERE branch = 'main' AND path = 'new.txt'").Scan(&count)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 merge commit for new.txt on main, got %d", count)
	}

	// Verify merge commit author is the one passed in the request.
	// After schema change, author lives in commits, not file_commits.
	var author string
	err = d.QueryRow(`
		SELECT c.author FROM file_commits fc
		JOIN commits c ON c.sequence = fc.sequence
		WHERE fc.branch = 'main' AND fc.path = 'new.txt' AND fc.sequence = $1`,
		resp.Sequence).Scan(&author)
	if err != nil {
		t.Fatalf("query author: %v", err)
	}
	if author != "carol" {
		t.Errorf("expected merge author 'carol', got %q", author)
	}
}

func TestMerge_Conflict(t *testing.T) {
	d := testutil.TestDB(t, MigrationSQL)
	s := NewStore(d)
	ctx := context.Background()

	// Commit to main.
	_, err := s.Commit(ctx, model.CommitRequest{
		Branch:  "main",
		Files:   []model.FileChange{{Path: "shared.txt", Content: []byte("original")}},
		Message: "initial",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Create branch.
	_, err = s.CreateBranch(ctx, model.CreateBranchRequest{Name: "feature/conflict"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	// Change shared.txt on both main and branch.
	_, err = s.Commit(ctx, model.CommitRequest{
		Branch:  "main",
		Files:   []model.FileChange{{Path: "shared.txt", Content: []byte("main version")}},
		Message: "main edit",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("main commit: %v", err)
	}

	_, err = s.Commit(ctx, model.CommitRequest{
		Branch:  "feature/conflict",
		Files:   []model.FileChange{{Path: "shared.txt", Content: []byte("branch version")}},
		Message: "branch edit",
		Author:  "bob",
	})
	if err != nil {
		t.Fatalf("branch commit: %v", err)
	}

	// Merge should fail with conflict.
	_, conflicts, err := s.Merge(ctx, model.MergeRequest{Branch: "feature/conflict"})
	if err != ErrMergeConflict {
		t.Fatalf("expected ErrMergeConflict, got %v", err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(conflicts))
	}
	if conflicts[0].Path != "shared.txt" {
		t.Errorf("expected conflict on shared.txt, got %q", conflicts[0].Path)
	}

	// Branch should still be active (merge was aborted).
	var status string
	err = d.QueryRow("SELECT status FROM branches WHERE name = 'feature/conflict'").Scan(&status)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if status != "active" {
		t.Errorf("expected active, got %q", status)
	}
}

func TestMerge_BranchNotFound(t *testing.T) {
	d := testutil.TestDB(t, MigrationSQL)
	s := NewStore(d)
	ctx := context.Background()

	_, _, err := s.Merge(ctx, model.MergeRequest{Branch: "nonexistent"})
	if err != ErrBranchNotFound {
		t.Fatalf("expected ErrBranchNotFound, got %v", err)
	}
}

func TestMerge_BranchNotActive(t *testing.T) {
	d := testutil.TestDB(t, MigrationSQL)
	s := NewStore(d)
	ctx := context.Background()

	// Create and immediately mark merged.
	_, err := d.Exec("INSERT INTO branches (name, head_sequence, base_sequence, status) VALUES ('already-merged', 0, 0, 'merged')")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	_, _, err = s.Merge(ctx, model.MergeRequest{Branch: "already-merged"})
	if err != ErrBranchNotActive {
		t.Fatalf("expected ErrBranchNotActive, got %v", err)
	}
}

// --- DeleteBranch tests ---

func TestDeleteBranch_Success(t *testing.T) {
	d := testutil.TestDB(t, MigrationSQL)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.CreateBranch(ctx, model.CreateBranchRequest{Name: "feature/delete-me"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	if err := s.DeleteBranch(ctx, "feature/delete-me"); err != nil {
		t.Fatalf("delete branch: %v", err)
	}

	var status string
	err = d.QueryRow("SELECT status FROM branches WHERE name = 'feature/delete-me'").Scan(&status)
	if err != nil {
		t.Fatalf("query branch: %v", err)
	}
	if status != "abandoned" {
		t.Errorf("expected status 'abandoned', got %q", status)
	}
}

func TestDeleteBranch_NotFound(t *testing.T) {
	d := testutil.TestDB(t, MigrationSQL)
	s := NewStore(d)
	ctx := context.Background()

	err := s.DeleteBranch(ctx, "nonexistent")
	if err != ErrBranchNotFound {
		t.Fatalf("expected ErrBranchNotFound, got %v", err)
	}
}

func TestDeleteBranch_AlreadyMerged(t *testing.T) {
	d := testutil.TestDB(t, MigrationSQL)
	s := NewStore(d)
	ctx := context.Background()

	_, err := d.Exec("INSERT INTO branches (name, head_sequence, base_sequence, status) VALUES ('merged-br', 0, 0, 'merged')")
	if err != nil {
		t.Fatalf("insert branch: %v", err)
	}

	err = s.DeleteBranch(ctx, "merged-br")
	if err != ErrBranchNotActive {
		t.Fatalf("expected ErrBranchNotActive, got %v", err)
	}
}

func TestDeleteBranch_AlreadyAbandoned(t *testing.T) {
	d := testutil.TestDB(t, MigrationSQL)
	s := NewStore(d)
	ctx := context.Background()

	_, err := d.Exec("INSERT INTO branches (name, head_sequence, base_sequence, status) VALUES ('abandoned-br', 0, 0, 'abandoned')")
	if err != nil {
		t.Fatalf("insert branch: %v", err)
	}

	err = s.DeleteBranch(ctx, "abandoned-br")
	if err != ErrBranchNotActive {
		t.Fatalf("expected ErrBranchNotActive, got %v", err)
	}
}

func TestMerge_EmptyBranch(t *testing.T) {
	d := testutil.TestDB(t, MigrationSQL)
	s := NewStore(d)
	ctx := context.Background()

	// Create branch with no commits.
	_, err := s.CreateBranch(ctx, model.CreateBranchRequest{Name: "feature/empty"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	resp, conflicts, err := s.Merge(ctx, model.MergeRequest{Branch: "feature/empty"})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if conflicts != nil {
		t.Fatalf("expected no conflicts, got %v", conflicts)
	}
	if resp.Sequence != 0 {
		t.Errorf("expected sequence 0 for empty merge, got %d", resp.Sequence)
	}

	// Branch should still be marked as merged.
	var status string
	err = d.QueryRow("SELECT status FROM branches WHERE name = 'feature/empty'").Scan(&status)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if status != "merged" {
		t.Errorf("expected merged, got %q", status)
	}
}
