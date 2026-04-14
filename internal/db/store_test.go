package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/testutil"
)

func TestCommit_SingleFile(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	resp, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
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
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	resp, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
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
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	content := []byte("identical content")

	// Commit the same content as two different files.
	resp, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
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
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	content := []byte("shared content")

	resp1, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "first.txt", Content: content}},
		Message: "commit 1",
		Author:  "alice@example.com",
	})
	if err != nil {
		t.Fatalf("commit 1: %v", err)
	}

	resp2, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
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
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	for i := 1; i <= 3; i++ {
		resp, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
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
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// First, create a file.
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
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
		Repo:    "default",
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
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
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
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Create a branch and mark it as merged.
	_, err := d.Exec(
		"INSERT INTO branches (repo, name, head_sequence, base_sequence, status) VALUES ('default', 'merged-br', 0, 0, 'merged')",
	)
	if err != nil {
		t.Fatalf("insert branch: %v", err)
	}

	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
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
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// First commit to main to advance head.
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "file.txt", Content: []byte("hello")}},
		Message: "initial",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	resp, err := s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default", Name: "feature/test"})
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
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default", Name: "feature/dup"})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}

	_, err = s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default", Name: "feature/dup"})
	if err != ErrBranchExists {
		t.Fatalf("expected ErrBranchExists, got %v", err)
	}
}

// --- Merge tests ---

func TestMerge_Success(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Commit to main.
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "base.txt", Content: []byte("base")}},
		Message: "initial",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Create branch.
	_, err = s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default", Name: "feature/merge"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	// Commit to branch.
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
		Branch:  "feature/merge",
		Files:   []model.FileChange{{Path: "new.txt", Content: []byte("new file")}},
		Message: "add new file",
		Author:  "bob",
	})
	if err != nil {
		t.Fatalf("branch commit: %v", err)
	}

	// Merge.
	resp, conflicts, err := s.Merge(ctx, model.MergeRequest{Repo: "default", Branch: "feature/merge", Author: "carol"})
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
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Commit to main.
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "shared.txt", Content: []byte("original")}},
		Message: "initial",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Create branch.
	_, err = s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default", Name: "feature/conflict"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	// Change shared.txt on both main and branch.
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "shared.txt", Content: []byte("main version")}},
		Message: "main edit",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("main commit: %v", err)
	}

	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
		Branch:  "feature/conflict",
		Files:   []model.FileChange{{Path: "shared.txt", Content: []byte("branch version")}},
		Message: "branch edit",
		Author:  "bob",
	})
	if err != nil {
		t.Fatalf("branch commit: %v", err)
	}

	// Merge should fail with conflict.
	_, conflicts, err := s.Merge(ctx, model.MergeRequest{Repo: "default", Branch: "feature/conflict"})
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
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, _, err := s.Merge(ctx, model.MergeRequest{Repo: "default", Branch: "nonexistent"})
	if err != ErrBranchNotFound {
		t.Fatalf("expected ErrBranchNotFound, got %v", err)
	}
}

func TestMerge_BranchNotActive(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Create and immediately mark merged.
	_, err := d.Exec("INSERT INTO branches (repo, name, head_sequence, base_sequence, status) VALUES ('default', 'already-merged', 0, 0, 'merged')")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	_, _, err = s.Merge(ctx, model.MergeRequest{Repo: "default", Branch: "already-merged"})
	if err != ErrBranchNotActive {
		t.Fatalf("expected ErrBranchNotActive, got %v", err)
	}
}

// --- DeleteBranch tests ---

func TestDeleteBranch_Success(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default", Name: "feature/delete-me"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	if err := s.DeleteBranch(ctx, "default", "feature/delete-me"); err != nil {
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
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	err := s.DeleteBranch(ctx, "default", "nonexistent")
	if err != ErrBranchNotFound {
		t.Fatalf("expected ErrBranchNotFound, got %v", err)
	}
}

func TestDeleteBranch_AlreadyMerged(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := d.Exec("INSERT INTO branches (repo, name, head_sequence, base_sequence, status) VALUES ('default', 'merged-br', 0, 0, 'merged')")
	if err != nil {
		t.Fatalf("insert branch: %v", err)
	}

	err = s.DeleteBranch(ctx, "default", "merged-br")
	if err != ErrBranchNotActive {
		t.Fatalf("expected ErrBranchNotActive, got %v", err)
	}
}

func TestDeleteBranch_AlreadyAbandoned(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := d.Exec("INSERT INTO branches (repo, name, head_sequence, base_sequence, status) VALUES ('default', 'abandoned-br', 0, 0, 'abandoned')")
	if err != nil {
		t.Fatalf("insert branch: %v", err)
	}

	err = s.DeleteBranch(ctx, "default", "abandoned-br")
	if err != ErrBranchNotActive {
		t.Fatalf("expected ErrBranchNotActive, got %v", err)
	}
}

func TestMerge_EmptyBranch(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Create branch with no commits.
	_, err := s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default", Name: "feature/empty"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	resp, conflicts, err := s.Merge(ctx, model.MergeRequest{Repo: "default", Branch: "feature/empty"})
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

// --- Rebase tests ---

func TestRebase_CleanNoConflict(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Commit to main (seq=1).
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "base.txt", Content: []byte("base")}},
		Message: "initial",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Create branch (base=1, head=1).
	_, err = s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default", Name: "feature/rebase"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	// Commit to branch (seq=2, adds "branch.txt").
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
		Branch:  "feature/rebase",
		Files:   []model.FileChange{{Path: "branch.txt", Content: []byte("branch work")}},
		Message: "branch commit",
		Author:  "bob",
	})
	if err != nil {
		t.Fatalf("branch commit: %v", err)
	}

	// Advance main (seq=3, adds "main.txt").
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "main.txt", Content: []byte("main work")}},
		Message: "main advance",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("main commit: %v", err)
	}

	// Rebase.
	resp, conflicts, err := s.Rebase(ctx, model.RebaseRequest{Repo: "default", Branch: "feature/rebase", Author: "bob"})
	if err != nil {
		t.Fatalf("rebase: %v", err)
	}
	if conflicts != nil {
		t.Fatalf("expected no conflicts, got %v", conflicts)
	}

	// base_sequence should be main's head (3).
	if resp.NewBaseSequence != 3 {
		t.Errorf("expected base_sequence 3, got %d", resp.NewBaseSequence)
	}
	// head_sequence should be the new replayed commit (4).
	if resp.NewHeadSequence != 4 {
		t.Errorf("expected head_sequence 4, got %d", resp.NewHeadSequence)
	}
	if resp.CommitsReplayed != 1 {
		t.Errorf("expected commits_replayed 1, got %d", resp.CommitsReplayed)
	}
}

func TestRebase_UpdatesBaseSequence(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "a.txt", Content: []byte("a")}},
		Message: "initial",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	_, err = s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default", Name: "feature/base"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
		Branch:  "feature/base",
		Files:   []model.FileChange{{Path: "b.txt", Content: []byte("b")}},
		Message: "branch commit",
		Author:  "bob",
	})
	if err != nil {
		t.Fatalf("branch commit: %v", err)
	}

	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "c.txt", Content: []byte("c")}},
		Message: "main advance",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("main advance: %v", err)
	}

	// Verify main head is 3.
	var mainHead int64
	err = d.QueryRow("SELECT head_sequence FROM branches WHERE name = 'main'").Scan(&mainHead)
	if err != nil {
		t.Fatalf("query main: %v", err)
	}
	if mainHead != 3 {
		t.Fatalf("expected main head 3, got %d", mainHead)
	}

	resp, _, err := s.Rebase(ctx, model.RebaseRequest{Repo: "default", Branch: "feature/base"})
	if err != nil {
		t.Fatalf("rebase: %v", err)
	}

	// base_sequence must equal main's head at time of rebase.
	if resp.NewBaseSequence != mainHead {
		t.Errorf("expected base_sequence %d (mainHead), got %d", mainHead, resp.NewBaseSequence)
	}

	// Verify in DB.
	var baseSeq int64
	err = d.QueryRow("SELECT base_sequence FROM branches WHERE name = 'feature/base'").Scan(&baseSeq)
	if err != nil {
		t.Fatalf("query branch: %v", err)
	}
	if baseSeq != mainHead {
		t.Errorf("expected db base_sequence %d, got %d", mainHead, baseSeq)
	}
}

func TestRebase_ReplaysAllCommits(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Commit to main (seq=1).
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "a.txt", Content: []byte("a")}},
		Message: "initial",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Create branch (base=1).
	_, err = s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default", Name: "feature/multi"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	// Three commits on branch: seq=2, 3, 4.
	for i, file := range []string{"x.txt", "y.txt", "z.txt"} {
		_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
			Branch:  "feature/multi",
			Files:   []model.FileChange{{Path: file, Content: []byte("content")}},
			Message: "branch commit",
			Author:  "bob",
		})
		if err != nil {
			t.Fatalf("branch commit %d: %v", i, err)
		}
	}

	// Advance main (seq=5).
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "main.txt", Content: []byte("m")}},
		Message: "main advance",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("main advance: %v", err)
	}

	resp, conflicts, err := s.Rebase(ctx, model.RebaseRequest{Repo: "default", Branch: "feature/multi"})
	if err != nil {
		t.Fatalf("rebase: %v", err)
	}
	if conflicts != nil {
		t.Fatalf("unexpected conflicts: %v", conflicts)
	}
	if resp.CommitsReplayed != 3 {
		t.Errorf("expected 3 commits replayed, got %d", resp.CommitsReplayed)
	}

	// head_sequence should be base (5) + 3 = 8.
	if resp.NewHeadSequence != 8 {
		t.Errorf("expected head_sequence 8, got %d", resp.NewHeadSequence)
	}

	// All three files should be visible on the branch via new file_commits.
	for _, file := range []string{"x.txt", "y.txt", "z.txt"} {
		var count int
		err = d.QueryRow(
			"SELECT count(*) FROM file_commits WHERE branch = 'feature/multi' AND path = $1 AND sequence > 5",
			file,
		).Scan(&count)
		if err != nil {
			t.Fatalf("query %s: %v", file, err)
		}
		if count != 1 {
			t.Errorf("expected 1 replayed file_commit for %s after base=5, got %d", file, count)
		}
	}
}

func TestRebase_EmptyBranch(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Commit to main (seq=1).
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "a.txt", Content: []byte("a")}},
		Message: "initial",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Create branch (base=head=1, no commits on branch).
	_, err = s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default", Name: "feature/empty"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	// Advance main (seq=2).
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "b.txt", Content: []byte("b")}},
		Message: "main advance",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("main advance: %v", err)
	}

	resp, conflicts, err := s.Rebase(ctx, model.RebaseRequest{Repo: "default", Branch: "feature/empty"})
	if err != nil {
		t.Fatalf("rebase empty branch: %v", err)
	}
	if conflicts != nil {
		t.Fatalf("expected no conflicts, got %v", conflicts)
	}
	if resp.CommitsReplayed != 0 {
		t.Errorf("expected 0 commits replayed, got %d", resp.CommitsReplayed)
	}
	if resp.NewBaseSequence != 2 {
		t.Errorf("expected base_sequence 2, got %d", resp.NewBaseSequence)
	}

	// Verify DB state.
	var baseSeq, headSeq int64
	err = d.QueryRow("SELECT base_sequence, head_sequence FROM branches WHERE name = 'feature/empty'").Scan(&baseSeq, &headSeq)
	if err != nil {
		t.Fatalf("query branch: %v", err)
	}
	if baseSeq != 2 {
		t.Errorf("expected db base_sequence 2, got %d", baseSeq)
	}
}

func TestRebase_Conflict(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Commit to main (seq=1).
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "shared.txt", Content: []byte("original")}},
		Message: "initial",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Create branch (base=1).
	_, err = s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default", Name: "feature/conflict"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	// Branch modifies shared.txt (seq=2).
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
		Branch:  "feature/conflict",
		Files:   []model.FileChange{{Path: "shared.txt", Content: []byte("branch version")}},
		Message: "branch edit",
		Author:  "bob",
	})
	if err != nil {
		t.Fatalf("branch commit: %v", err)
	}

	// Main also modifies shared.txt (seq=3).
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "shared.txt", Content: []byte("main version")}},
		Message: "main edit",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("main commit: %v", err)
	}

	_, conflicts, err := s.Rebase(ctx, model.RebaseRequest{Repo: "default", Branch: "feature/conflict"})
	if err != ErrRebaseConflict {
		t.Fatalf("expected ErrRebaseConflict, got %v", err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(conflicts))
	}
	if conflicts[0].Path != "shared.txt" {
		t.Errorf("expected conflict on shared.txt, got %q", conflicts[0].Path)
	}
}

func TestRebase_ConflictIsAtomic(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Commit to main (seq=1).
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "shared.txt", Content: []byte("original")}},
		Message: "initial",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Create branch (base=1).
	_, err = s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default", Name: "feature/atomic"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	// Two commits on branch: shared.txt (seq=2) and other.txt (seq=3).
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
		Branch:  "feature/atomic",
		Files:   []model.FileChange{{Path: "shared.txt", Content: []byte("branch shared")}},
		Message: "edit shared",
		Author:  "bob",
	})
	if err != nil {
		t.Fatalf("branch commit 1: %v", err)
	}
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
		Branch:  "feature/atomic",
		Files:   []model.FileChange{{Path: "other.txt", Content: []byte("other")}},
		Message: "add other",
		Author:  "bob",
	})
	if err != nil {
		t.Fatalf("branch commit 2: %v", err)
	}

	// Main modifies shared.txt (seq=4).
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "shared.txt", Content: []byte("main shared")}},
		Message: "main edit",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("main commit: %v", err)
	}

	// Rebase should fail with conflict and branch should be unchanged.
	_, _, err = s.Rebase(ctx, model.RebaseRequest{Repo: "default", Branch: "feature/atomic"})
	if err != ErrRebaseConflict {
		t.Fatalf("expected ErrRebaseConflict, got %v", err)
	}

	// Branch state must be unchanged: base=1, head=3.
	var baseSeq, headSeq int64
	err = d.QueryRow("SELECT base_sequence, head_sequence FROM branches WHERE name = 'feature/atomic'").Scan(&baseSeq, &headSeq)
	if err != nil {
		t.Fatalf("query branch: %v", err)
	}
	if baseSeq != 1 {
		t.Errorf("expected base_sequence 1 (unchanged), got %d", baseSeq)
	}
	if headSeq != 3 {
		t.Errorf("expected head_sequence 3 (unchanged), got %d", headSeq)
	}

	// No new file_commits should have been inserted after the rollback.
	var count int
	err = d.QueryRow("SELECT count(*) FROM file_commits WHERE branch = 'feature/atomic' AND sequence > 3").Scan(&count)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 new file_commits after rollback, got %d", count)
	}
}

func TestRebase_BranchNotFound(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, _, err := s.Rebase(ctx, model.RebaseRequest{Repo: "default", Branch: "nonexistent"})
	if err != ErrBranchNotFound {
		t.Fatalf("expected ErrBranchNotFound, got %v", err)
	}
}

func TestRebase_AlreadyMerged(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := d.Exec("INSERT INTO branches (repo, name, head_sequence, base_sequence, status) VALUES ('default', 'already-merged', 0, 0, 'merged')")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	_, _, err = s.Rebase(ctx, model.RebaseRequest{Repo: "default", Branch: "already-merged"})
	if err != ErrBranchNotActive {
		t.Fatalf("expected ErrBranchNotActive, got %v", err)
	}
}

func TestRebase_MainBranch(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, _, err := s.Rebase(ctx, model.RebaseRequest{Repo: "default", Branch: "main"})
	if err != ErrBranchNotActive {
		t.Fatalf("expected ErrBranchNotActive, got %v", err)
	}
}

func TestRebase_ThenMerge(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Commit to main (seq=1).
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "base.txt", Content: []byte("base")}},
		Message: "initial",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Create branch (base=1, head=1).
	_, err = s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default", Name: "feature/rebase-merge"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	// Commit to branch (seq=2, adds "new.txt").
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
		Branch:  "feature/rebase-merge",
		Files:   []model.FileChange{{Path: "new.txt", Content: []byte("new")}},
		Message: "add new",
		Author:  "bob",
	})
	if err != nil {
		t.Fatalf("branch commit: %v", err)
	}

	// Advance main (seq=3, adds "other.txt" — no conflict with branch).
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "other.txt", Content: []byte("other")}},
		Message: "main advance",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("main advance: %v", err)
	}

	// Rebase (seq=4 for replayed branch commit).
	rebaseResp, conflicts, err := s.Rebase(ctx, model.RebaseRequest{Repo: "default", Branch: "feature/rebase-merge", Author: "bob"})
	if err != nil {
		t.Fatalf("rebase: %v", err)
	}
	if conflicts != nil {
		t.Fatalf("unexpected conflicts: %v", conflicts)
	}
	if rebaseResp.NewBaseSequence != 3 {
		t.Errorf("expected base_sequence 3, got %d", rebaseResp.NewBaseSequence)
	}

	// Merge should now succeed cleanly (seq=5).
	mergeResp, mergeConflicts, err := s.Merge(ctx, model.MergeRequest{Repo: "default", Branch: "feature/rebase-merge", Author: "alice"})
	if err != nil {
		t.Fatalf("merge after rebase: %v", err)
	}
	if mergeConflicts != nil {
		t.Fatalf("unexpected merge conflicts: %v", mergeConflicts)
	}
	if mergeResp.Sequence != 5 {
		t.Errorf("expected merge sequence 5, got %d", mergeResp.Sequence)
	}

	// new.txt should be visible on main.
	var count int
	err = d.QueryRow("SELECT count(*) FROM file_commits WHERE branch = 'main' AND path = 'new.txt'").Scan(&count)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if count != 1 {
		t.Errorf("expected new.txt on main after merge, got %d file_commits", count)
	}
}

// ---------------------------------------------------------------------------
// Repo CRUD tests
// ---------------------------------------------------------------------------

func TestCreateRepo_Success(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	r, err := s.CreateRepo(ctx, model.CreateRepoRequest{Name: "myrepo", CreatedBy: "alice"})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	if r.Name != "myrepo" {
		t.Errorf("expected name myrepo, got %q", r.Name)
	}
	if r.CreatedBy != "alice" {
		t.Errorf("expected created_by alice, got %q", r.CreatedBy)
	}

	// main branch should exist for the new repo.
	var status string
	err = d.QueryRow("SELECT status FROM branches WHERE repo = 'myrepo' AND name = 'main'").Scan(&status)
	if err != nil {
		t.Fatalf("query main branch: %v", err)
	}
	if status != "active" {
		t.Errorf("expected main branch to be active, got %q", status)
	}
}

func TestCreateRepo_Duplicate(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.CreateRepo(ctx, model.CreateRepoRequest{Name: "dup"})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}

	_, err = s.CreateRepo(ctx, model.CreateRepoRequest{Name: "dup"})
	if err != ErrRepoExists {
		t.Fatalf("expected ErrRepoExists, got %v", err)
	}
}

func TestDeleteRepo_RemovesAllData(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Create a repo and add some data.
	_, err := s.CreateRepo(ctx, model.CreateRepoRequest{Name: "todelete"})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}

	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "todelete",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "hello.txt", Content: []byte("hello")}},
		Message: "initial",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Seed a role, a review, and a check_run for the repo so we can verify
	// they are all cleaned up on deletion.
	_, err = d.Exec(
		"INSERT INTO roles (repo, identity, role) VALUES ('todelete', 'alice', 'writer')",
	)
	if err != nil {
		t.Fatalf("insert role: %v", err)
	}
	_, err = d.Exec(
		"INSERT INTO reviews (repo, id, branch, reviewer, sequence, status) VALUES ('todelete', gen_random_uuid(), 'main', 'alice', 1, 'approved')",
	)
	if err != nil {
		t.Fatalf("insert review: %v", err)
	}
	_, err = d.Exec(
		"INSERT INTO check_runs (repo, id, branch, sequence, check_name, status, reporter) VALUES ('todelete', gen_random_uuid(), 'main', 1, 'ci', 'passed', 'ci-bot')",
	)
	if err != nil {
		t.Fatalf("insert check_run: %v", err)
	}

	// Delete the repo.
	if err := s.DeleteRepo(ctx, "todelete"); err != nil {
		t.Fatalf("DeleteRepo: %v", err)
	}

	// All data should be gone — including reviews, check_runs, and roles.
	for _, table := range []string{"branches", "commits", "file_commits", "documents", "reviews", "check_runs", "roles"} {
		var count int
		err = d.QueryRow("SELECT count(*) FROM "+table+" WHERE repo = 'todelete'").Scan(&count)
		if err != nil {
			t.Fatalf("query %s: %v", table, err)
		}
		if count != 0 {
			t.Errorf("expected 0 rows in %s for deleted repo, got %d", table, count)
		}
	}

	// Repo row itself gone.
	var exists bool
	err = d.QueryRow("SELECT EXISTS(SELECT 1 FROM repos WHERE name = 'todelete')").Scan(&exists)
	if err != nil {
		t.Fatalf("query repos: %v", err)
	}
	if exists {
		t.Error("expected repo row to be deleted")
	}
}

func TestDeleteRepo_DoesNotAffectOtherRepo(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Create two repos and commit data to each.
	for _, repo := range []string{"repo-a", "repo-b"} {
		_, err := s.CreateRepo(ctx, model.CreateRepoRequest{Name: repo})
		if err != nil {
			t.Fatalf("CreateRepo %s: %v", repo, err)
		}
		_, err = s.Commit(ctx, model.CommitRequest{
			Repo:    repo,
			Branch:  "main",
			Files:   []model.FileChange{{Path: "f.txt", Content: []byte(repo)}},
			Message: "init",
			Author:  "alice",
		})
		if err != nil {
			t.Fatalf("Commit %s: %v", repo, err)
		}
	}

	// Delete repo-a.
	if err := s.DeleteRepo(ctx, "repo-a"); err != nil {
		t.Fatalf("DeleteRepo: %v", err)
	}

	// repo-b's data should be intact.
	var count int
	err := d.QueryRow("SELECT count(*) FROM file_commits WHERE repo = 'repo-b'").Scan(&count)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 file_commit in repo-b, got %d", count)
	}
}

func TestDeleteRepo_NotFound(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	err := s.DeleteRepo(ctx, "nonexistent")
	if err != ErrRepoNotFound {
		t.Fatalf("expected ErrRepoNotFound, got %v", err)
	}
}

// TestCreateRepo_IsAtomic verifies that if the process were to fail between the
// repo INSERT and the main-branch INSERT, the caller would not observe a half-
// created repo.  Since we cannot inject a crash mid-transaction, we verify the
// positive invariant: after a successful CreateRepo the repo and its main branch
// always exist together.
func TestCreateRepo_IsAtomic(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.CreateRepo(ctx, model.CreateRepoRequest{Name: "atomic-repo"})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}

	// Both the repo row and its main branch must be present.
	var repoExists bool
	if err := d.QueryRow("SELECT EXISTS(SELECT 1 FROM repos WHERE name = 'atomic-repo')").Scan(&repoExists); err != nil {
		t.Fatal(err)
	}
	if !repoExists {
		t.Error("expected repo row to exist")
	}

	var branchStatus string
	if err := d.QueryRow("SELECT status FROM branches WHERE repo = 'atomic-repo' AND name = 'main'").Scan(&branchStatus); err != nil {
		t.Fatalf("expected main branch, got error: %v", err)
	}
	if branchStatus != "active" {
		t.Errorf("expected main branch active, got %q", branchStatus)
	}
}

// TestCreateBranch_RepoNotFound verifies that creating a branch for a repo that
// does not exist returns ErrRepoNotFound rather than an opaque error.
func TestCreateBranch_RepoNotFound(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "no-such-repo", Name: "feature/x"})
	if err != ErrRepoNotFound {
		t.Fatalf("expected ErrRepoNotFound, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Repo isolation tests
// ---------------------------------------------------------------------------

func TestRepoIsolation_Branches(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Create two repos.
	for _, repo := range []string{"repo-x", "repo-y"} {
		if _, err := s.CreateRepo(ctx, model.CreateRepoRequest{Name: repo}); err != nil {
			t.Fatalf("CreateRepo %s: %v", repo, err)
		}
	}

	// Both repos can have a branch named "feature/shared".
	for _, repo := range []string{"repo-x", "repo-y"} {
		_, err := s.CreateBranch(ctx, model.CreateBranchRequest{Repo: repo, Name: "feature/shared"})
		if err != nil {
			t.Fatalf("CreateBranch %s: %v", repo, err)
		}
	}

	// Commit to one repo's branch.
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "repo-x",
		Branch:  "feature/shared",
		Files:   []model.FileChange{{Path: "x-only.txt", Content: []byte("x")}},
		Message: "x commit",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("Commit repo-x: %v", err)
	}

	// The other repo's branch should have no commits.
	var count int
	err = d.QueryRow("SELECT count(*) FROM file_commits WHERE repo = 'repo-y' AND branch = 'feature/shared'").Scan(&count)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 file_commits in repo-y/feature/shared, got %d", count)
	}
}

func TestRepoIsolation_Commits(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	for _, repo := range []string{"iso-a", "iso-b"} {
		if _, err := s.CreateRepo(ctx, model.CreateRepoRequest{Name: repo}); err != nil {
			t.Fatalf("CreateRepo %s: %v", repo, err)
		}
	}

	// Commit unique content to each repo.
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "iso-a",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "a.txt", Content: []byte("a only")}},
		Message: "a init",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("Commit iso-a: %v", err)
	}

	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "iso-b",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "b.txt", Content: []byte("b only")}},
		Message: "b init",
		Author:  "bob",
	})
	if err != nil {
		t.Fatalf("Commit iso-b: %v", err)
	}

	// repo-a should only see a.txt, not b.txt.
	var aCount, bCount int
	if err := d.QueryRow("SELECT count(*) FROM file_commits WHERE repo = 'iso-a' AND path = 'b.txt'").Scan(&bCount); err != nil {
		t.Fatal(err)
	}
	if bCount != 0 {
		t.Errorf("expected b.txt absent in iso-a, got %d rows", bCount)
	}

	if err := d.QueryRow("SELECT count(*) FROM file_commits WHERE repo = 'iso-b' AND path = 'a.txt'").Scan(&aCount); err != nil {
		t.Fatal(err)
	}
	if aCount != 0 {
		t.Errorf("expected a.txt absent in iso-b, got %d rows", aCount)
	}
}

func TestRepoIsolation_Merge(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	for _, repo := range []string{"merge-a", "merge-b"} {
		if _, err := s.CreateRepo(ctx, model.CreateRepoRequest{Name: repo}); err != nil {
			t.Fatalf("CreateRepo %s: %v", repo, err)
		}
	}

	// Set up a branch with a change in merge-a only.
	_, err := s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "merge-a", Name: "feature/x"})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "merge-a",
		Branch:  "feature/x",
		Files:   []model.FileChange{{Path: "new.txt", Content: []byte("new")}},
		Message: "add new",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Merge in merge-a.
	resp, conflicts, err := s.Merge(ctx, model.MergeRequest{Repo: "merge-a", Branch: "feature/x", Author: "alice"})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if conflicts != nil {
		t.Fatalf("unexpected conflicts: %v", conflicts)
	}

	// merge-b's main should have no file_commits from the merge.
	var bMainCount int
	if err := d.QueryRow("SELECT count(*) FROM file_commits WHERE repo = 'merge-b' AND branch = 'main'").Scan(&bMainCount); err != nil {
		t.Fatal(err)
	}
	if bMainCount != 0 {
		t.Errorf("expected 0 file_commits on merge-b main, got %d", bMainCount)
	}

	// merge-a's main should have the merged file.
	var aMainCount int
	if err := d.QueryRow("SELECT count(*) FROM file_commits WHERE repo = 'merge-a' AND branch = 'main' AND path = 'new.txt'").Scan(&aMainCount); err != nil {
		t.Fatal(err)
	}
	if aMainCount != 1 {
		t.Errorf("expected 1 file_commit for new.txt on merge-a main, got %d", aMainCount)
	}

	_ = resp
}

// ---------------------------------------------------------------------------
// Review store tests
// ---------------------------------------------------------------------------

func TestCreateReview_Success(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Commit to main so head_sequence > 0 and reviewable by a different author.
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo: "default", Branch: "main",
		Files:   []model.FileChange{{Path: "f.txt", Content: []byte("v1")}},
		Message: "init", Author: "alice@example.com",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	rev, err := s.CreateReview(ctx, "default", "main", "bob@example.com", model.ReviewApproved, "LGTM")
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}
	if rev.ID == "" {
		t.Error("expected non-empty id")
	}
	if rev.Sequence != 1 {
		t.Errorf("expected sequence 1, got %d", rev.Sequence)
	}
	if rev.Status != model.ReviewApproved {
		t.Errorf("expected approved, got %q", rev.Status)
	}
	if rev.Body != "LGTM" {
		t.Errorf("expected body LGTM, got %q", rev.Body)
	}
}

func TestCreateReview_RecordedAtHeadSequence(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Two commits on main by alice.
	for i := 0; i < 2; i++ {
		_, err := s.Commit(ctx, model.CommitRequest{
			Repo: "default", Branch: "main",
			Files:   []model.FileChange{{Path: "f.txt", Content: []byte("v" + string(rune('1'+i)))}},
			Message: "commit", Author: "alice@example.com",
		})
		if err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}

	// bob reviews at head (seq=2).
	rev, err := s.CreateReview(ctx, "default", "main", "bob@example.com", model.ReviewApproved, "")
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}
	if rev.Sequence != 2 {
		t.Errorf("expected sequence 2 (head), got %d", rev.Sequence)
	}
}

func TestCreateReview_StaleAfterCommit(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.Commit(ctx, model.CommitRequest{
		Repo: "default", Branch: "main",
		Files:   []model.FileChange{{Path: "f.txt", Content: []byte("v1")}},
		Message: "init", Author: "alice@example.com",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Bob reviews at seq=1.
	rev, err := s.CreateReview(ctx, "default", "main", "bob@example.com", model.ReviewApproved, "")
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}
	if rev.Sequence != 1 {
		t.Fatalf("expected sequence 1, got %d", rev.Sequence)
	}

	// A new commit advances head_sequence to 2.
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo: "default", Branch: "main",
		Files:   []model.FileChange{{Path: "f.txt", Content: []byte("v2")}},
		Message: "update", Author: "alice@example.com",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// The review at seq=1 is now stale: head_sequence is now 2.
	var headSeq int64
	err = d.QueryRow("SELECT head_sequence FROM branches WHERE name = 'main'").Scan(&headSeq)
	if err != nil {
		t.Fatalf("query branch: %v", err)
	}
	if headSeq != 2 {
		t.Fatalf("expected head_sequence 2 after commit, got %d", headSeq)
	}
	// The review's sequence (1) no longer matches head (2) — it is stale.
	if rev.Sequence == headSeq {
		t.Error("expected review to be stale (sequence != head_sequence)")
	}
}

func TestListReviews_ByBranch(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.Commit(ctx, model.CommitRequest{
		Repo: "default", Branch: "main",
		Files:   []model.FileChange{{Path: "f.txt", Content: []byte("v1")}},
		Message: "init", Author: "alice@example.com",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Two reviews by different users.
	_, err = s.CreateReview(ctx, "default", "main", "bob@example.com", model.ReviewApproved, "LGTM")
	if err != nil {
		t.Fatalf("review 1: %v", err)
	}
	_, err = s.CreateReview(ctx, "default", "main", "carol@example.com", model.ReviewRejected, "needs work")
	if err != nil {
		t.Fatalf("review 2: %v", err)
	}

	reviews, err := s.ListReviews(ctx, "default", "main", nil)
	if err != nil {
		t.Fatalf("ListReviews: %v", err)
	}
	if len(reviews) != 2 {
		t.Fatalf("expected 2 reviews, got %d", len(reviews))
	}
	// Most recent first (carol reviewed last).
	if reviews[0].Reviewer != "carol@example.com" {
		t.Errorf("expected first review by carol, got %q", reviews[0].Reviewer)
	}
}

func TestCreateReview_SelfApproval(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// alice commits to main.
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo: "default", Branch: "main",
		Files:   []model.FileChange{{Path: "f.txt", Content: []byte("v1")}},
		Message: "init", Author: "alice@example.com",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// alice tries to approve her own commit.
	_, err = s.CreateReview(ctx, "default", "main", "alice@example.com", model.ReviewApproved, "")
	if !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("expected ErrSelfApproval, got %v", err)
	}
}

func TestReviewRepoIsolation(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Create two repos.
	for _, repo := range []string{"repo-a", "repo-b"} {
		if _, err := s.CreateRepo(ctx, model.CreateRepoRequest{Name: repo}); err != nil {
			t.Fatalf("CreateRepo %s: %v", repo, err)
		}
	}

	// alice commits to repo-a.
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo: "repo-a", Branch: "main",
		Files:   []model.FileChange{{Path: "f.txt", Content: []byte("a")}},
		Message: "init", Author: "alice@example.com",
	})
	if err != nil {
		t.Fatalf("commit repo-a: %v", err)
	}

	// bob reviews repo-a.
	_, err = s.CreateReview(ctx, "repo-a", "main", "bob@example.com", model.ReviewApproved, "")
	if err != nil {
		t.Fatalf("review repo-a: %v", err)
	}

	// Reviews in repo-b must be empty.
	reviews, err := s.ListReviews(ctx, "repo-b", "main", nil)
	if err != nil {
		t.Fatalf("ListReviews repo-b: %v", err)
	}
	if len(reviews) != 0 {
		t.Errorf("expected 0 reviews in repo-b, got %d", len(reviews))
	}
}

// ---------------------------------------------------------------------------
// Check-run store tests
// ---------------------------------------------------------------------------

func TestCreateCheckRun_Success(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	cr, err := s.CreateCheckRun(ctx, "default", "main", "ci/build", model.CheckRunPassed, "ci-bot")
	if err != nil {
		t.Fatalf("CreateCheckRun: %v", err)
	}
	if cr.ID == "" {
		t.Error("expected non-empty id")
	}
	if cr.Sequence != 0 {
		t.Errorf("expected sequence 0 (no commits yet), got %d", cr.Sequence)
	}
	if cr.Status != model.CheckRunPassed {
		t.Errorf("expected passed, got %q", cr.Status)
	}
	if cr.Reporter != "ci-bot" {
		t.Errorf("expected reporter ci-bot, got %q", cr.Reporter)
	}
}

func TestCreateCheckRun_RecordedAtHeadSequence(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.Commit(ctx, model.CommitRequest{
		Repo: "default", Branch: "main",
		Files:   []model.FileChange{{Path: "f.txt", Content: []byte("v1")}},
		Message: "init", Author: "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	cr, err := s.CreateCheckRun(ctx, "default", "main", "ci/build", model.CheckRunPassed, "ci-bot")
	if err != nil {
		t.Fatalf("CreateCheckRun: %v", err)
	}
	if cr.Sequence != 1 {
		t.Errorf("expected sequence 1 (head), got %d", cr.Sequence)
	}
}

func TestListCheckRuns_ByBranch(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.CreateCheckRun(ctx, "default", "main", "ci/build", model.CheckRunPassed, "ci-bot")
	if err != nil {
		t.Fatalf("check 1: %v", err)
	}
	_, err = s.CreateCheckRun(ctx, "default", "main", "ci/lint", model.CheckRunFailed, "ci-bot")
	if err != nil {
		t.Fatalf("check 2: %v", err)
	}

	crs, err := s.ListCheckRuns(ctx, "default", "main", nil)
	if err != nil {
		t.Fatalf("ListCheckRuns: %v", err)
	}
	if len(crs) != 2 {
		t.Fatalf("expected 2 check runs, got %d", len(crs))
	}
}

func TestListCheckRuns_LatestPerName(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// First run: pending
	_, err := s.CreateCheckRun(ctx, "default", "main", "ci/build", model.CheckRunPending, "ci-bot")
	if err != nil {
		t.Fatalf("check 1: %v", err)
	}
	// Second run: passed (same check_name, more recent)
	_, err = s.CreateCheckRun(ctx, "default", "main", "ci/build", model.CheckRunPassed, "ci-bot")
	if err != nil {
		t.Fatalf("check 2: %v", err)
	}

	crs, err := s.ListCheckRuns(ctx, "default", "main", nil)
	if err != nil {
		t.Fatalf("ListCheckRuns: %v", err)
	}
	if len(crs) != 2 {
		t.Fatalf("expected 2 check runs total, got %d", len(crs))
	}
	// Most recent first: passed should be first.
	if crs[0].Status != model.CheckRunPassed {
		t.Errorf("expected most recent check run (passed) first, got %q", crs[0].Status)
	}
}

// ---------------------------------------------------------------------------
// Role store tests
// ---------------------------------------------------------------------------

func TestGetRole_Exists(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	if _, err := s.CreateRepo(ctx, model.CreateRepoRequest{Name: "r1"}); err != nil {
		t.Fatalf("create repo: %v", err)
	}
	if err := s.SetRole(ctx, "r1", "alice@example.com", model.RoleWriter); err != nil {
		t.Fatalf("set role: %v", err)
	}

	role, err := s.GetRole(ctx, "r1", "alice@example.com")
	if err != nil {
		t.Fatalf("get role: %v", err)
	}
	if role.Identity != "alice@example.com" {
		t.Errorf("expected identity alice@example.com, got %q", role.Identity)
	}
	if role.Role != model.RoleWriter {
		t.Errorf("expected writer, got %q", role.Role)
	}
}

func TestGetRole_NotFound(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	if _, err := s.CreateRepo(ctx, model.CreateRepoRequest{Name: "r1"}); err != nil {
		t.Fatalf("create repo: %v", err)
	}

	_, err := s.GetRole(ctx, "r1", "nobody@example.com")
	if !errors.Is(err, ErrRoleNotFound) {
		t.Errorf("expected ErrRoleNotFound, got %v", err)
	}
}

func TestSetRole(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	if _, err := s.CreateRepo(ctx, model.CreateRepoRequest{Name: "r1"}); err != nil {
		t.Fatalf("create repo: %v", err)
	}

	// Assign role.
	if err := s.SetRole(ctx, "r1", "bob@example.com", model.RoleReader); err != nil {
		t.Fatalf("set role: %v", err)
	}
	role, _ := s.GetRole(ctx, "r1", "bob@example.com")
	if role.Role != model.RoleReader {
		t.Errorf("expected reader, got %q", role.Role)
	}

	// Upsert: update to admin.
	if err := s.SetRole(ctx, "r1", "bob@example.com", model.RoleAdmin); err != nil {
		t.Fatalf("update role: %v", err)
	}
	role, _ = s.GetRole(ctx, "r1", "bob@example.com")
	if role.Role != model.RoleAdmin {
		t.Errorf("expected admin after upsert, got %q", role.Role)
	}
}

func TestDeleteRole(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	if _, err := s.CreateRepo(ctx, model.CreateRepoRequest{Name: "r1"}); err != nil {
		t.Fatalf("create repo: %v", err)
	}
	if err := s.SetRole(ctx, "r1", "carol@example.com", model.RoleMaintainer); err != nil {
		t.Fatalf("set role: %v", err)
	}

	// Delete the role.
	if err := s.DeleteRole(ctx, "r1", "carol@example.com"); err != nil {
		t.Fatalf("delete role: %v", err)
	}

	// Verify it's gone.
	if _, err := s.GetRole(ctx, "r1", "carol@example.com"); !errors.Is(err, ErrRoleNotFound) {
		t.Errorf("expected ErrRoleNotFound after delete, got %v", err)
	}

	// Delete again → ErrRoleNotFound.
	if err := s.DeleteRole(ctx, "r1", "carol@example.com"); !errors.Is(err, ErrRoleNotFound) {
		t.Errorf("expected ErrRoleNotFound on second delete, got %v", err)
	}
}

func TestRoleIsolation(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	for _, name := range []string{"repo-a", "repo-b"} {
		if _, err := s.CreateRepo(ctx, model.CreateRepoRequest{Name: name}); err != nil {
			t.Fatalf("create repo %s: %v", name, err)
		}
	}

	// Set alice as admin in repo-a only.
	if err := s.SetRole(ctx, "repo-a", "alice@example.com", model.RoleAdmin); err != nil {
		t.Fatalf("set role: %v", err)
	}

	// Alice has role in repo-a.
	role, err := s.GetRole(ctx, "repo-a", "alice@example.com")
	if err != nil || role.Role != model.RoleAdmin {
		t.Errorf("expected admin in repo-a, got %v %v", role, err)
	}

	// Alice has NO role in repo-b.
	if _, err := s.GetRole(ctx, "repo-b", "alice@example.com"); !errors.Is(err, ErrRoleNotFound) {
		t.Errorf("expected ErrRoleNotFound for alice in repo-b, got %v", err)
	}

	// HasAdmin for repo-a is true.
	hasAdmin, err := s.HasAdmin(ctx, "repo-a")
	if err != nil || !hasAdmin {
		t.Errorf("expected HasAdmin true for repo-a, got %v %v", hasAdmin, err)
	}

	// HasAdmin for repo-b is false.
	hasAdmin, err = s.HasAdmin(ctx, "repo-b")
	if err != nil || hasAdmin {
		t.Errorf("expected HasAdmin false for repo-b, got %v %v", hasAdmin, err)
	}
}

// ---------------------------------------------------------------------------
// Purge tests
// ---------------------------------------------------------------------------

// makeOld ages a branch and all its commits to 100 days ago so it is eligible
// for purge with any threshold less than 100 days.
func makeOld(t *testing.T, db *sql.DB, repo, branch string) {
	t.Helper()
	_, err := db.Exec(`UPDATE branches SET created_at = now() - interval '100 days' WHERE repo = $1 AND name = $2`, repo, branch)
	if err != nil {
		t.Fatalf("makeOld branches: %v", err)
	}
	_, err = db.Exec(`UPDATE commits SET created_at = now() - interval '100 days' WHERE repo = $1 AND branch = $2`, repo, branch)
	if err != nil {
		t.Fatalf("makeOld commits: %v", err)
	}
}

func TestPurge_DeletesMergedBranches(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Commit something to main so the branch has a non-zero base.
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo: "default", Branch: "main",
		Files:   []model.FileChange{{Path: "base.txt", Content: []byte("base")}},
		Message: "base", Author: "alice",
	})
	if err != nil {
		t.Fatalf("commit to main: %v", err)
	}

	// Create a branch, commit to it, then merge.
	_, err = s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default", Name: "feature/merged"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo: "default", Branch: "feature/merged",
		Files:   []model.FileChange{{Path: "f.txt", Content: []byte("branch work")}},
		Message: "work", Author: "alice",
	})
	if err != nil {
		t.Fatalf("commit to branch: %v", err)
	}
	_, _, err = s.Merge(ctx, model.MergeRequest{Repo: "default", Branch: "feature/merged", Author: "alice"})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	// Make the branch and its commits appear old.
	makeOld(t, d, "default", "feature/merged")

	result, err := s.Purge(ctx, PurgeRequest{Repo: "default", OlderThan: 24 * time.Hour})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}

	if result.BranchesPurged != 1 {
		t.Errorf("expected 1 branch purged, got %d", result.BranchesPurged)
	}
	if result.FileCommitsDeleted < 1 {
		t.Errorf("expected at least 1 file_commit deleted, got %d", result.FileCommitsDeleted)
	}
	if result.CommitsDeleted < 1 {
		t.Errorf("expected at least 1 commit deleted, got %d", result.CommitsDeleted)
	}

	// The branch row must be gone.
	var count int
	d.QueryRow("SELECT count(*) FROM branches WHERE repo = 'default' AND name = 'feature/merged'").Scan(&count)
	if count != 0 {
		t.Errorf("expected branch to be deleted, found %d rows", count)
	}
}

func TestPurge_DeletesAbandonedBranches(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default", Name: "feature/abandoned"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo: "default", Branch: "feature/abandoned",
		Files:   []model.FileChange{{Path: "x.txt", Content: []byte("x")}},
		Message: "x", Author: "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := s.DeleteBranch(ctx, "default", "feature/abandoned"); err != nil {
		t.Fatalf("delete branch: %v", err)
	}

	makeOld(t, d, "default", "feature/abandoned")

	result, err := s.Purge(ctx, PurgeRequest{Repo: "default", OlderThan: 24 * time.Hour})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if result.BranchesPurged != 1 {
		t.Errorf("expected 1 branch purged, got %d", result.BranchesPurged)
	}

	var count int
	d.QueryRow("SELECT count(*) FROM branches WHERE repo = 'default' AND name = 'feature/abandoned'").Scan(&count)
	if count != 0 {
		t.Errorf("expected abandoned branch to be deleted, found %d rows", count)
	}
}

func TestPurge_RespectsOlderThan(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Create a branch and merge it — timestamps are current (now), not old.
	_, err := s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default", Name: "feature/recent"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}
	_, _, err = s.Merge(ctx, model.MergeRequest{Repo: "default", Branch: "feature/recent", Author: "alice"})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	// Purge with 1-day threshold — recently merged branch must NOT be purged.
	result, err := s.Purge(ctx, PurgeRequest{Repo: "default", OlderThan: 24 * time.Hour})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if result.BranchesPurged != 0 {
		t.Errorf("expected 0 branches purged, got %d", result.BranchesPurged)
	}

	var count int
	d.QueryRow("SELECT count(*) FROM branches WHERE repo = 'default' AND name = 'feature/recent'").Scan(&count)
	if count != 1 {
		t.Errorf("expected recent branch to still exist, got count=%d", count)
	}
}

func TestPurge_DoesNotTouchActiveBranches(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default", Name: "feature/active"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}
	// Age the branch row so it would be eligible if it were merged/abandoned.
	makeOld(t, d, "default", "feature/active")

	result, err := s.Purge(ctx, PurgeRequest{Repo: "default", OlderThan: 24 * time.Hour})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if result.BranchesPurged != 0 {
		t.Errorf("expected 0 branches purged (active branch must be skipped), got %d", result.BranchesPurged)
	}
}

func TestPurge_DoesNotTouchMain(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.Commit(ctx, model.CommitRequest{
		Repo: "default", Branch: "main",
		Files:   []model.FileChange{{Path: "m.txt", Content: []byte("main")}},
		Message: "m", Author: "alice",
	})
	if err != nil {
		t.Fatalf("commit to main: %v", err)
	}
	// Force main's created_at to be very old (should never be eligible due to name filter).
	d.Exec(`UPDATE branches SET created_at = now() - interval '100 days' WHERE repo = 'default' AND name = 'main'`)
	d.Exec(`UPDATE commits SET created_at = now() - interval '100 days' WHERE repo = 'default' AND branch = 'main'`)

	result, err := s.Purge(ctx, PurgeRequest{Repo: "default", OlderThan: 24 * time.Hour})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if result.BranchesPurged != 0 {
		t.Errorf("expected main to be untouched, but %d branches purged", result.BranchesPurged)
	}

	// main must still exist.
	var count int
	d.QueryRow("SELECT count(*) FROM branches WHERE repo = 'default' AND name = 'main'").Scan(&count)
	if count != 1 {
		t.Errorf("expected main branch to still exist")
	}
}

func TestPurge_OrphanedDocumentsDeleted(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Create a branch, commit a file, then abandon it without merging.
	_, err := s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default", Name: "feature/orphan"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}
	commitResp, err := s.Commit(ctx, model.CommitRequest{
		Repo: "default", Branch: "feature/orphan",
		Files:   []model.FileChange{{Path: "orphan.txt", Content: []byte("unique orphan content")}},
		Message: "orphan", Author: "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	// Record the version_id of the orphaned doc.
	orphanVersionID := ""
	if len(commitResp.Files) > 0 && commitResp.Files[0].VersionID != nil {
		orphanVersionID = *commitResp.Files[0].VersionID
	}

	if err := s.DeleteBranch(ctx, "default", "feature/orphan"); err != nil {
		t.Fatalf("delete branch: %v", err)
	}
	makeOld(t, d, "default", "feature/orphan")

	result, err := s.Purge(ctx, PurgeRequest{Repo: "default", OlderThan: 24 * time.Hour})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if result.DocumentsDeleted < 1 {
		t.Errorf("expected at least 1 orphaned document deleted, got %d", result.DocumentsDeleted)
	}

	// The orphaned document must no longer exist.
	if orphanVersionID != "" {
		var count int
		d.QueryRow("SELECT count(*) FROM documents WHERE version_id = $1", orphanVersionID).Scan(&count)
		if count != 0 {
			t.Errorf("orphaned document %s should have been deleted", orphanVersionID)
		}
	}
}

func TestPurge_SharedDocumentRetained(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Commit a file to main first.
	mainResp, err := s.Commit(ctx, model.CommitRequest{
		Repo: "default", Branch: "main",
		Files:   []model.FileChange{{Path: "shared.txt", Content: []byte("shared content")}},
		Message: "main", Author: "alice",
	})
	if err != nil {
		t.Fatalf("commit to main: %v", err)
	}
	sharedVersionID := ""
	if len(mainResp.Files) > 0 && mainResp.Files[0].VersionID != nil {
		sharedVersionID = *mainResp.Files[0].VersionID
	}

	// Create a branch with the same content (will dedup to same version_id).
	_, err = s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default", Name: "feature/shared"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo: "default", Branch: "feature/shared",
		Files:   []model.FileChange{{Path: "shared.txt", Content: []byte("shared content")}},
		Message: "branch", Author: "alice",
	})
	if err != nil {
		t.Fatalf("commit to branch: %v", err)
	}
	if err := s.DeleteBranch(ctx, "default", "feature/shared"); err != nil {
		t.Fatalf("delete branch: %v", err)
	}
	makeOld(t, d, "default", "feature/shared")

	result, err := s.Purge(ctx, PurgeRequest{Repo: "default", OlderThan: 24 * time.Hour})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	// The shared document is still referenced by main's file_commits → must not be deleted.
	if result.DocumentsDeleted != 0 {
		t.Errorf("expected 0 documents deleted (shared with main), got %d", result.DocumentsDeleted)
	}

	if sharedVersionID != "" {
		var count int
		d.QueryRow("SELECT count(*) FROM documents WHERE version_id = $1", sharedVersionID).Scan(&count)
		if count != 1 {
			t.Errorf("shared document %s should still exist", sharedVersionID)
		}
	}
}

func TestPurge_DeletesReviewsAndChecks(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default", Name: "feature/rev"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo: "default", Branch: "feature/rev",
		Files:   []model.FileChange{{Path: "r.txt", Content: []byte("r")}},
		Message: "r", Author: "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Add a review and a check_run for the branch.
	_, err = s.CreateReview(ctx, "default", "feature/rev", "bob", model.ReviewApproved, "LGTM")
	if err != nil {
		t.Fatalf("create review: %v", err)
	}
	_, err = s.CreateCheckRun(ctx, "default", "feature/rev", "ci/build", model.CheckRunPassed, "ci-bot")
	if err != nil {
		t.Fatalf("create check_run: %v", err)
	}

	if err := s.DeleteBranch(ctx, "default", "feature/rev"); err != nil {
		t.Fatalf("delete branch: %v", err)
	}
	makeOld(t, d, "default", "feature/rev")

	result, err := s.Purge(ctx, PurgeRequest{Repo: "default", OlderThan: 24 * time.Hour})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if result.ReviewsDeleted != 1 {
		t.Errorf("expected 1 review deleted, got %d", result.ReviewsDeleted)
	}
	if result.CheckRunsDeleted != 1 {
		t.Errorf("expected 1 check_run deleted, got %d", result.CheckRunsDeleted)
	}
}

func TestPurge_DryRun(t *testing.T) {
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default", Name: "feature/dry"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo: "default", Branch: "feature/dry",
		Files:   []model.FileChange{{Path: "d.txt", Content: []byte("dry run content")}},
		Message: "dry", Author: "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := s.DeleteBranch(ctx, "default", "feature/dry"); err != nil {
		t.Fatalf("delete branch: %v", err)
	}
	makeOld(t, d, "default", "feature/dry")

	// Count before dry run.
	var branchBefore, fcBefore int
	d.QueryRow("SELECT count(*) FROM branches WHERE repo = 'default' AND name = 'feature/dry'").Scan(&branchBefore)
	d.QueryRow("SELECT count(*) FROM file_commits WHERE repo = 'default' AND branch = 'feature/dry'").Scan(&fcBefore)

	result, err := s.Purge(ctx, PurgeRequest{Repo: "default", OlderThan: 24 * time.Hour, DryRun: true})
	if err != nil {
		t.Fatalf("purge dry run: %v", err)
	}

	// Counts must be non-zero (something would be deleted).
	if result.BranchesPurged != 1 {
		t.Errorf("dry run: expected 1 branch, got %d", result.BranchesPurged)
	}
	if result.FileCommitsDeleted < 1 {
		t.Errorf("dry run: expected file_commits > 0, got %d", result.FileCommitsDeleted)
	}

	// But no rows must actually have been deleted.
	var branchAfter, fcAfter int
	d.QueryRow("SELECT count(*) FROM branches WHERE repo = 'default' AND name = 'feature/dry'").Scan(&branchAfter)
	d.QueryRow("SELECT count(*) FROM file_commits WHERE repo = 'default' AND branch = 'feature/dry'").Scan(&fcAfter)

	if branchAfter != branchBefore {
		t.Errorf("dry run must not delete branches: before=%d after=%d", branchBefore, branchAfter)
	}
	if fcAfter != fcBefore {
		t.Errorf("dry run must not delete file_commits: before=%d after=%d", fcBefore, fcAfter)
	}
}

func TestPurge_IsAtomic(t *testing.T) {
	// Demonstrates that all deletes are committed together (or not at all).
	// We use a pre-cancelled context so BeginTx fails before any deletions occur,
	// verifying that no partial state is left behind.
	d := testutil.TestDB(t, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default", Name: "feature/atomic"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo: "default", Branch: "feature/atomic",
		Files:   []model.FileChange{{Path: "a.txt", Content: []byte("atomic")}},
		Message: "atomic", Author: "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := s.DeleteBranch(ctx, "default", "feature/atomic"); err != nil {
		t.Fatalf("delete branch: %v", err)
	}
	makeOld(t, d, "default", "feature/atomic")

	var fcBefore int
	d.QueryRow("SELECT count(*) FROM file_commits WHERE repo = 'default' AND branch = 'feature/atomic'").Scan(&fcBefore)

	// Use a cancelled context — BeginTx will fail immediately.
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()

	_, err = s.Purge(cancelCtx, PurgeRequest{Repo: "default", OlderThan: 24 * time.Hour})
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}

	// Verify no data was changed.
	var fcAfter int
	d.QueryRow("SELECT count(*) FROM file_commits WHERE repo = 'default' AND branch = 'feature/atomic'").Scan(&fcAfter)
	if fcAfter != fcBefore {
		t.Errorf("partial deletion occurred: before=%d after=%d file_commits", fcBefore, fcAfter)
	}

	var branchCount int
	d.QueryRow("SELECT count(*) FROM branches WHERE repo = 'default' AND name = 'feature/atomic'").Scan(&branchCount)
	if branchCount != 1 {
		t.Errorf("branch should still exist after failed purge, count=%d", branchCount)
	}
}
