package db

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dlorenc/docstore/internal/hash"
	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/testutil"
)

func TestCommit_SingleFile(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	resp, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
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
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	resp, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
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
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	content := []byte("identical content")

	// Commit the same content as two different files.
	resp, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
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
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	content := []byte("shared content")

	resp1, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "first.txt", Content: content}},
		Message: "commit 1",
		Author:  "alice@example.com",
	})
	if err != nil {
		t.Fatalf("commit 1: %v", err)
	}

	resp2, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
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
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	for i := 1; i <= 3; i++ {
		resp, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
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
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// First, create a file.
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
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
		Repo:    "default/default",
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
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
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
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Create a branch and mark it as merged.
	_, err := d.Exec(
		"INSERT INTO branches (repo, name, head_sequence, base_sequence, status) VALUES ('default/default', 'merged-br', 0, 0, 'merged')",
	)
	if err != nil {
		t.Fatalf("insert branch: %v", err)
	}

	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
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
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// First commit to main to advance head.
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "file.txt", Content: []byte("hello")}},
		Message: "initial",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	resp, err := s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default/default", Name: "feature/test"})
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
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default/default", Name: "feature/dup"})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}

	_, err = s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default/default", Name: "feature/dup"})
	if err != ErrBranchExists {
		t.Fatalf("expected ErrBranchExists, got %v", err)
	}
}

// --- Merge tests ---

func TestMerge_Success(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Commit to main.
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "base.txt", Content: []byte("base")}},
		Message: "initial",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Create branch.
	_, err = s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default/default", Name: "feature/merge"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	// Commit to branch.
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "feature/merge",
		Files:   []model.FileChange{{Path: "new.txt", Content: []byte("new file")}},
		Message: "add new file",
		Author:  "bob",
	})
	if err != nil {
		t.Fatalf("branch commit: %v", err)
	}

	// Merge.
	resp, conflicts, err := s.Merge(ctx, model.MergeRequest{Repo: "default/default", Branch: "feature/merge", Author: "carol"})
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
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Commit to main.
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "shared.txt", Content: []byte("original")}},
		Message: "initial",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Create branch.
	_, err = s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default/default", Name: "feature/conflict"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	// Change shared.txt on both main and branch.
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "shared.txt", Content: []byte("main version")}},
		Message: "main edit",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("main commit: %v", err)
	}

	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "feature/conflict",
		Files:   []model.FileChange{{Path: "shared.txt", Content: []byte("branch version")}},
		Message: "branch edit",
		Author:  "bob",
	})
	if err != nil {
		t.Fatalf("branch commit: %v", err)
	}

	// Merge should fail with conflict.
	_, conflicts, err := s.Merge(ctx, model.MergeRequest{Repo: "default/default", Branch: "feature/conflict"})
	if err != ErrMergeConflict {
		t.Fatalf("expected ErrMergeConflict, got %v", err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(conflicts))
	}
	if conflicts[0].Path != "shared.txt" {
		t.Errorf("expected conflict on shared.txt, got %q", conflicts[0].Path)
	}
	if conflicts[0].MainVersionID == "" {
		t.Error("expected non-empty MainVersionID")
	}
	if conflicts[0].BranchVersionID == "" {
		t.Error("expected non-empty BranchVersionID")
	}
	if conflicts[0].MainVersionID == conflicts[0].BranchVersionID {
		t.Error("expected MainVersionID and BranchVersionID to differ")
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
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, _, err := s.Merge(ctx, model.MergeRequest{Repo: "default/default", Branch: "nonexistent"})
	if err != ErrBranchNotFound {
		t.Fatalf("expected ErrBranchNotFound, got %v", err)
	}
}

func TestMerge_BranchNotActive(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Create and immediately mark merged.
	_, err := d.Exec("INSERT INTO branches (repo, name, head_sequence, base_sequence, status) VALUES ('default/default', 'already-merged', 0, 0, 'merged')")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	_, _, err = s.Merge(ctx, model.MergeRequest{Repo: "default/default", Branch: "already-merged"})
	if err != ErrBranchNotActive {
		t.Fatalf("expected ErrBranchNotActive, got %v", err)
	}
}

// --- DeleteBranch tests ---

func TestDeleteBranch_Success(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default/default", Name: "feature/delete-me"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	if err := s.DeleteBranch(ctx, "default/default", "feature/delete-me"); err != nil {
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
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	err := s.DeleteBranch(ctx, "default/default", "nonexistent")
	if err != ErrBranchNotFound {
		t.Fatalf("expected ErrBranchNotFound, got %v", err)
	}
}

func TestDeleteBranch_AlreadyMerged(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := d.Exec("INSERT INTO branches (repo, name, head_sequence, base_sequence, status) VALUES ('default/default', 'merged-br', 0, 0, 'merged')")
	if err != nil {
		t.Fatalf("insert branch: %v", err)
	}

	err = s.DeleteBranch(ctx, "default/default", "merged-br")
	if err != ErrBranchNotActive {
		t.Fatalf("expected ErrBranchNotActive, got %v", err)
	}
}

func TestDeleteBranch_AlreadyAbandoned(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := d.Exec("INSERT INTO branches (repo, name, head_sequence, base_sequence, status) VALUES ('default/default', 'abandoned-br', 0, 0, 'abandoned')")
	if err != nil {
		t.Fatalf("insert branch: %v", err)
	}

	err = s.DeleteBranch(ctx, "default/default", "abandoned-br")
	if err != ErrBranchNotActive {
		t.Fatalf("expected ErrBranchNotActive, got %v", err)
	}
}

func TestMerge_EmptyBranch(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Create branch with no commits.
	_, err := s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default/default", Name: "feature/empty"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	resp, conflicts, err := s.Merge(ctx, model.MergeRequest{Repo: "default/default", Branch: "feature/empty"})
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
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Commit to main (seq=1).
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "base.txt", Content: []byte("base")}},
		Message: "initial",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Create branch (base=1, head=1).
	_, err = s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default/default", Name: "feature/rebase"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	// Commit to branch (seq=2, adds "branch.txt").
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
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
		Repo:    "default/default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "main.txt", Content: []byte("main work")}},
		Message: "main advance",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("main commit: %v", err)
	}

	// Rebase.
	resp, conflicts, err := s.Rebase(ctx, model.RebaseRequest{Repo: "default/default", Branch: "feature/rebase", Author: "bob"})
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
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "a.txt", Content: []byte("a")}},
		Message: "initial",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	_, err = s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default/default", Name: "feature/base"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "feature/base",
		Files:   []model.FileChange{{Path: "b.txt", Content: []byte("b")}},
		Message: "branch commit",
		Author:  "bob",
	})
	if err != nil {
		t.Fatalf("branch commit: %v", err)
	}

	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
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

	resp, _, err := s.Rebase(ctx, model.RebaseRequest{Repo: "default/default", Branch: "feature/base"})
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
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Commit to main (seq=1).
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "a.txt", Content: []byte("a")}},
		Message: "initial",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Create branch (base=1).
	_, err = s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default/default", Name: "feature/multi"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	// Three commits on branch: seq=2, 3, 4.
	for i, file := range []string{"x.txt", "y.txt", "z.txt"} {
		_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
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
		Repo:    "default/default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "main.txt", Content: []byte("m")}},
		Message: "main advance",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("main advance: %v", err)
	}

	resp, conflicts, err := s.Rebase(ctx, model.RebaseRequest{Repo: "default/default", Branch: "feature/multi"})
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
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Commit to main (seq=1).
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "a.txt", Content: []byte("a")}},
		Message: "initial",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Create branch (base=head=1, no commits on branch).
	_, err = s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default/default", Name: "feature/empty"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	// Advance main (seq=2).
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "b.txt", Content: []byte("b")}},
		Message: "main advance",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("main advance: %v", err)
	}

	resp, conflicts, err := s.Rebase(ctx, model.RebaseRequest{Repo: "default/default", Branch: "feature/empty"})
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
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Commit to main (seq=1).
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "shared.txt", Content: []byte("original")}},
		Message: "initial",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Create branch (base=1).
	_, err = s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default/default", Name: "feature/conflict"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	// Branch modifies shared.txt (seq=2).
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
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
		Repo:    "default/default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "shared.txt", Content: []byte("main version")}},
		Message: "main edit",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("main commit: %v", err)
	}

	_, conflicts, err := s.Rebase(ctx, model.RebaseRequest{Repo: "default/default", Branch: "feature/conflict"})
	if err != ErrRebaseConflict {
		t.Fatalf("expected ErrRebaseConflict, got %v", err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(conflicts))
	}
	if conflicts[0].Path != "shared.txt" {
		t.Errorf("expected conflict on shared.txt, got %q", conflicts[0].Path)
	}
	if conflicts[0].MainVersionID == "" {
		t.Error("expected non-empty MainVersionID")
	}
	if conflicts[0].BranchVersionID == "" {
		t.Error("expected non-empty BranchVersionID")
	}
	if conflicts[0].MainVersionID == conflicts[0].BranchVersionID {
		t.Error("expected MainVersionID and BranchVersionID to differ")
	}
}

func TestRebase_ConflictIsAtomic(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Commit to main (seq=1).
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "shared.txt", Content: []byte("original")}},
		Message: "initial",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Create branch (base=1).
	_, err = s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default/default", Name: "feature/atomic"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	// Two commits on branch: shared.txt (seq=2) and other.txt (seq=3).
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "feature/atomic",
		Files:   []model.FileChange{{Path: "shared.txt", Content: []byte("branch shared")}},
		Message: "edit shared",
		Author:  "bob",
	})
	if err != nil {
		t.Fatalf("branch commit 1: %v", err)
	}
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
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
		Repo:    "default/default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "shared.txt", Content: []byte("main shared")}},
		Message: "main edit",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("main commit: %v", err)
	}

	// Rebase should fail with conflict and branch should be unchanged.
	_, _, err = s.Rebase(ctx, model.RebaseRequest{Repo: "default/default", Branch: "feature/atomic"})
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
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, _, err := s.Rebase(ctx, model.RebaseRequest{Repo: "default/default", Branch: "nonexistent"})
	if err != ErrBranchNotFound {
		t.Fatalf("expected ErrBranchNotFound, got %v", err)
	}
}

func TestRebase_AlreadyMerged(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := d.Exec("INSERT INTO branches (repo, name, head_sequence, base_sequence, status) VALUES ('default/default', 'already-merged', 0, 0, 'merged')")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	_, _, err = s.Rebase(ctx, model.RebaseRequest{Repo: "default/default", Branch: "already-merged"})
	if err != ErrBranchNotActive {
		t.Fatalf("expected ErrBranchNotActive, got %v", err)
	}
}

func TestRebase_MainBranch(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, _, err := s.Rebase(ctx, model.RebaseRequest{Repo: "default/default", Branch: "main"})
	if err != ErrBranchNotActive {
		t.Fatalf("expected ErrBranchNotActive, got %v", err)
	}
}

func TestRebase_ThenMerge(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Commit to main (seq=1).
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "base.txt", Content: []byte("base")}},
		Message: "initial",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Create branch (base=1, head=1).
	_, err = s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default/default", Name: "feature/rebase-merge"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	// Commit to branch (seq=2, adds "new.txt").
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
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
		Repo:    "default/default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "other.txt", Content: []byte("other")}},
		Message: "main advance",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("main advance: %v", err)
	}

	// Rebase (seq=4 for replayed branch commit).
	rebaseResp, conflicts, err := s.Rebase(ctx, model.RebaseRequest{Repo: "default/default", Branch: "feature/rebase-merge", Author: "bob"})
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
	mergeResp, mergeConflicts, err := s.Merge(ctx, model.MergeRequest{Repo: "default/default", Branch: "feature/rebase-merge", Author: "alice"})
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
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	r, err := s.CreateRepo(ctx, model.CreateRepoRequest{Owner: "default", Name: "myrepo", CreatedBy: "alice"})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	if r.Name != "default/myrepo" {
		t.Errorf("expected name myrepo, got %q", r.Name)
	}
	if r.CreatedBy != "alice" {
		t.Errorf("expected created_by alice, got %q", r.CreatedBy)
	}

	// main branch should exist for the new repo.
	var status string
	err = d.QueryRow("SELECT status FROM branches WHERE repo = 'default/myrepo' AND name = 'main'").Scan(&status)
	if err != nil {
		t.Fatalf("query main branch: %v", err)
	}
	if status != "active" {
		t.Errorf("expected main branch to be active, got %q", status)
	}
}

func TestCreateRepo_Duplicate(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.CreateRepo(ctx, model.CreateRepoRequest{Owner: "default", Name: "dup"})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}

	_, err = s.CreateRepo(ctx, model.CreateRepoRequest{Owner: "default", Name: "dup"})
	if err != ErrRepoExists {
		t.Fatalf("expected ErrRepoExists, got %v", err)
	}
}

func TestDeleteRepo_RemovesAllData(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Create a repo and add some data.
	_, err := s.CreateRepo(ctx, model.CreateRepoRequest{Owner: "default", Name: "todelete"})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}

	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default/todelete",
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
		"INSERT INTO roles (repo, identity, role) VALUES ('default/todelete', 'alice', 'writer')",
	)
	if err != nil {
		t.Fatalf("insert role: %v", err)
	}
	_, err = d.Exec(
		"INSERT INTO reviews (repo, id, branch, reviewer, sequence, status) VALUES ('default/todelete', gen_random_uuid(), 'main', 'alice', 1, 'approved')",
	)
	if err != nil {
		t.Fatalf("insert review: %v", err)
	}
	_, err = d.Exec(
		"INSERT INTO check_runs (repo, id, branch, sequence, check_name, status, reporter) VALUES ('default/todelete', gen_random_uuid(), 'main', 1, 'ci', 'passed', 'ci-bot')",
	)
	if err != nil {
		t.Fatalf("insert check_run: %v", err)
	}

	// Delete the repo.
	if err := s.DeleteRepo(ctx, "default/todelete"); err != nil {
		t.Fatalf("DeleteRepo: %v", err)
	}

	// All data should be gone — including reviews, check_runs, and roles.
	for _, table := range []string{"branches", "commits", "file_commits", "documents", "reviews", "check_runs", "roles"} {
		var count int
		err = d.QueryRow("SELECT count(*) FROM "+table+" WHERE repo = 'default/todelete'").Scan(&count)
		if err != nil {
			t.Fatalf("query %s: %v", table, err)
		}
		if count != 0 {
			t.Errorf("expected 0 rows in %s for deleted repo, got %d", table, count)
		}
	}

	// Repo row itself gone.
	var exists bool
	err = d.QueryRow("SELECT EXISTS(SELECT 1 FROM repos WHERE name = 'default/todelete')").Scan(&exists)
	if err != nil {
		t.Fatalf("query repos: %v", err)
	}
	if exists {
		t.Error("expected repo row to be deleted")
	}
}

func TestDeleteRepo_DoesNotAffectOtherRepo(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Create two repos and commit data to each.
	for _, repo := range []string{"default/repo-a", "default/repo-b"} {
		_, err := s.CreateRepo(ctx, model.CreateRepoRequest{Owner: strings.SplitN(repo, "/", 2)[0], Name: strings.SplitN(repo, "/", 2)[1]})
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
	if err := s.DeleteRepo(ctx, "default/repo-a"); err != nil {
		t.Fatalf("DeleteRepo: %v", err)
	}

	// repo-b's data should be intact.
	var count int
	err := d.QueryRow("SELECT count(*) FROM file_commits WHERE repo = 'default/repo-b'").Scan(&count)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 file_commit in repo-b, got %d", count)
	}
}

func TestDeleteRepo_NotFound(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
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
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.CreateRepo(ctx, model.CreateRepoRequest{Owner: "default", Name: "atomic-repo"})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}

	// Both the repo row and its main branch must be present.
	var repoExists bool
	if err := d.QueryRow("SELECT EXISTS(SELECT 1 FROM repos WHERE name = 'default/atomic-repo')").Scan(&repoExists); err != nil {
		t.Fatal(err)
	}
	if !repoExists {
		t.Error("expected repo row to exist")
	}

	var branchStatus string
	if err := d.QueryRow("SELECT status FROM branches WHERE repo = 'default/atomic-repo' AND name = 'main'").Scan(&branchStatus); err != nil {
		t.Fatalf("expected main branch, got error: %v", err)
	}
	if branchStatus != "active" {
		t.Errorf("expected main branch active, got %q", branchStatus)
	}
}

// TestCreateBranch_RepoNotFound verifies that creating a branch for a repo that
// does not exist returns ErrRepoNotFound rather than an opaque error.
func TestCreateBranch_RepoNotFound(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
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
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Create two repos.
	for _, repo := range []string{"default/repo-x", "default/repo-y"} {
		if _, err := s.CreateRepo(ctx, model.CreateRepoRequest{Owner: strings.SplitN(repo, "/", 2)[0], Name: strings.SplitN(repo, "/", 2)[1]}); err != nil {
			t.Fatalf("CreateRepo %s: %v", repo, err)
		}
	}

	// Both repos can have a branch named "feature/shared".
	for _, repo := range []string{"default/repo-x", "default/repo-y"} {
		_, err := s.CreateBranch(ctx, model.CreateBranchRequest{Repo: repo, Name: "feature/shared"})
		if err != nil {
			t.Fatalf("CreateBranch %s: %v", repo, err)
		}
	}

	// Commit to one repo's branch.
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default/repo-x",
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
	err = d.QueryRow("SELECT count(*) FROM file_commits WHERE repo = 'default/repo-y' AND branch = 'feature/shared'").Scan(&count)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 file_commits in repo-y/feature/shared, got %d", count)
	}
}

func TestRepoIsolation_Commits(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	for _, repo := range []string{"default/iso-a", "default/iso-b"} {
		if _, err := s.CreateRepo(ctx, model.CreateRepoRequest{Owner: strings.SplitN(repo, "/", 2)[0], Name: strings.SplitN(repo, "/", 2)[1]}); err != nil {
			t.Fatalf("CreateRepo %s: %v", repo, err)
		}
	}

	// Commit unique content to each repo.
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default/iso-a",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "a.txt", Content: []byte("a only")}},
		Message: "a init",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("Commit iso-a: %v", err)
	}

	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default/iso-b",
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
	if err := d.QueryRow("SELECT count(*) FROM file_commits WHERE repo = 'default/iso-a' AND path = 'b.txt'").Scan(&bCount); err != nil {
		t.Fatal(err)
	}
	if bCount != 0 {
		t.Errorf("expected b.txt absent in iso-a, got %d rows", bCount)
	}

	if err := d.QueryRow("SELECT count(*) FROM file_commits WHERE repo = 'default/iso-b' AND path = 'a.txt'").Scan(&aCount); err != nil {
		t.Fatal(err)
	}
	if aCount != 0 {
		t.Errorf("expected a.txt absent in iso-b, got %d rows", aCount)
	}
}

func TestRepoIsolation_Merge(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	for _, repo := range []string{"default/merge-a", "default/merge-b"} {
		if _, err := s.CreateRepo(ctx, model.CreateRepoRequest{Owner: strings.SplitN(repo, "/", 2)[0], Name: strings.SplitN(repo, "/", 2)[1]}); err != nil {
			t.Fatalf("CreateRepo %s: %v", repo, err)
		}
	}

	// Set up a branch with a change in merge-a only.
	_, err := s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default/merge-a", Name: "feature/x"})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default/merge-a",
		Branch:  "feature/x",
		Files:   []model.FileChange{{Path: "new.txt", Content: []byte("new")}},
		Message: "add new",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Merge in merge-a.
	resp, conflicts, err := s.Merge(ctx, model.MergeRequest{Repo: "default/merge-a", Branch: "feature/x", Author: "alice"})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if conflicts != nil {
		t.Fatalf("unexpected conflicts: %v", conflicts)
	}

	// merge-b's main should have no file_commits from the merge.
	var bMainCount int
	if err := d.QueryRow("SELECT count(*) FROM file_commits WHERE repo = 'default/merge-b' AND branch = 'main'").Scan(&bMainCount); err != nil {
		t.Fatal(err)
	}
	if bMainCount != 0 {
		t.Errorf("expected 0 file_commits on merge-b main, got %d", bMainCount)
	}

	// merge-a's main should have the merged file.
	var aMainCount int
	if err := d.QueryRow("SELECT count(*) FROM file_commits WHERE repo = 'default/merge-a' AND branch = 'main' AND path = 'new.txt'").Scan(&aMainCount); err != nil {
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
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Commit to main so head_sequence > 0 and reviewable by a different author.
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo: "default/default", Branch: "main",
		Files:   []model.FileChange{{Path: "f.txt", Content: []byte("v1")}},
		Message: "init", Author: "alice@example.com",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	rev, err := s.CreateReview(ctx, "default/default", "main", "bob@example.com", model.ReviewApproved, "LGTM")
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
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Two commits on main by alice.
	for i := 0; i < 2; i++ {
		_, err := s.Commit(ctx, model.CommitRequest{
			Repo: "default/default", Branch: "main",
			Files:   []model.FileChange{{Path: "f.txt", Content: []byte("v" + string(rune('1'+i)))}},
			Message: "commit", Author: "alice@example.com",
		})
		if err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}

	// bob reviews at head (seq=2).
	rev, err := s.CreateReview(ctx, "default/default", "main", "bob@example.com", model.ReviewApproved, "")
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}
	if rev.Sequence != 2 {
		t.Errorf("expected sequence 2 (head), got %d", rev.Sequence)
	}
}

func TestCreateReview_StaleAfterCommit(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.Commit(ctx, model.CommitRequest{
		Repo: "default/default", Branch: "main",
		Files:   []model.FileChange{{Path: "f.txt", Content: []byte("v1")}},
		Message: "init", Author: "alice@example.com",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Bob reviews at seq=1.
	rev, err := s.CreateReview(ctx, "default/default", "main", "bob@example.com", model.ReviewApproved, "")
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}
	if rev.Sequence != 1 {
		t.Fatalf("expected sequence 1, got %d", rev.Sequence)
	}

	// A new commit advances head_sequence to 2.
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo: "default/default", Branch: "main",
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
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.Commit(ctx, model.CommitRequest{
		Repo: "default/default", Branch: "main",
		Files:   []model.FileChange{{Path: "f.txt", Content: []byte("v1")}},
		Message: "init", Author: "alice@example.com",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Two reviews by different users.
	_, err = s.CreateReview(ctx, "default/default", "main", "bob@example.com", model.ReviewApproved, "LGTM")
	if err != nil {
		t.Fatalf("review 1: %v", err)
	}
	_, err = s.CreateReview(ctx, "default/default", "main", "carol@example.com", model.ReviewRejected, "needs work")
	if err != nil {
		t.Fatalf("review 2: %v", err)
	}

	reviews, err := s.ListReviews(ctx, "default/default", "main", nil)
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
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// alice commits to main.
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo: "default/default", Branch: "main",
		Files:   []model.FileChange{{Path: "f.txt", Content: []byte("v1")}},
		Message: "init", Author: "alice@example.com",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// alice tries to approve her own commit.
	_, err = s.CreateReview(ctx, "default/default", "main", "alice@example.com", model.ReviewApproved, "")
	if !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("expected ErrSelfApproval, got %v", err)
	}
}

func TestReviewRepoIsolation(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Create two repos.
	for _, repo := range []string{"default/repo-a", "default/repo-b"} {
		if _, err := s.CreateRepo(ctx, model.CreateRepoRequest{Owner: strings.SplitN(repo, "/", 2)[0], Name: strings.SplitN(repo, "/", 2)[1]}); err != nil {
			t.Fatalf("CreateRepo %s: %v", repo, err)
		}
	}

	// alice commits to repo-a.
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo: "default/repo-a", Branch: "main",
		Files:   []model.FileChange{{Path: "f.txt", Content: []byte("a")}},
		Message: "init", Author: "alice@example.com",
	})
	if err != nil {
		t.Fatalf("commit repo-a: %v", err)
	}

	// bob reviews repo-a.
	_, err = s.CreateReview(ctx, "default/repo-a", "main", "bob@example.com", model.ReviewApproved, "")
	if err != nil {
		t.Fatalf("review repo-a: %v", err)
	}

	// Reviews in repo-b must be empty.
	reviews, err := s.ListReviews(ctx, "default/repo-b", "main", nil)
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
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	cr, err := s.CreateCheckRun(ctx, "default/default", "main", "ci/build", model.CheckRunPassed, "ci-bot", nil, nil, 1, nil)
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
	if cr.Attempt != 1 {
		t.Errorf("expected attempt 1, got %d", cr.Attempt)
	}
}

func TestCreateCheckRun_RecordedAtHeadSequence(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.Commit(ctx, model.CommitRequest{
		Repo: "default/default", Branch: "main",
		Files:   []model.FileChange{{Path: "f.txt", Content: []byte("v1")}},
		Message: "init", Author: "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	cr, err := s.CreateCheckRun(ctx, "default/default", "main", "ci/build", model.CheckRunPassed, "ci-bot", nil, nil, 1, nil)
	if err != nil {
		t.Fatalf("CreateCheckRun: %v", err)
	}
	if cr.Sequence != 1 {
		t.Errorf("expected sequence 1 (head), got %d", cr.Sequence)
	}
}

func TestCreateCheckRun_WithMetadata(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	meta := json.RawMessage(`{"key":"value"}`)
	wantMeta := `{"key": "value"}`
	cr, err := s.CreateCheckRun(ctx, "default/default", "main", "ci/build", model.CheckRunPassed, "ci-bot", nil, nil, 1, meta)
	if err != nil {
		t.Fatalf("CreateCheckRun: %v", err)
	}
	var gotBuf, wantBuf bytes.Buffer
	if err := json.Compact(&gotBuf, cr.Metadata); err != nil {
		t.Fatalf("compact returned metadata: %v", err)
	}
	if err := json.Compact(&wantBuf, []byte(wantMeta)); err != nil {
		t.Fatalf("compact wantMeta: %v", err)
	}
	if gotBuf.String() != wantBuf.String() {
		t.Errorf("returned metadata: got %s, want %s", cr.Metadata, wantMeta)
	}

	runs, err := s.ListCheckRuns(ctx, "default/default", "main", nil, false)
	if err != nil {
		t.Fatalf("ListCheckRuns: %v", err)
	}
	if len(runs) == 0 {
		t.Fatal("expected at least one check run")
	}
	gotBuf.Reset()
	wantBuf.Reset()
	if err := json.Compact(&gotBuf, runs[0].Metadata); err != nil {
		t.Fatalf("compact listed metadata: %v", err)
	}
	if err := json.Compact(&wantBuf, []byte(wantMeta)); err != nil {
		t.Fatalf("compact wantMeta: %v", err)
	}
	if gotBuf.String() != wantBuf.String() {
		t.Errorf("listed metadata: got %s, want %s", runs[0].Metadata, wantMeta)
	}
}

func TestListCheckRuns_ByBranch(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.CreateCheckRun(ctx, "default/default", "main", "ci/build", model.CheckRunPassed, "ci-bot", nil, nil, 1, nil)
	if err != nil {
		t.Fatalf("check 1: %v", err)
	}
	_, err = s.CreateCheckRun(ctx, "default/default", "main", "ci/lint", model.CheckRunFailed, "ci-bot", nil, nil, 1, nil)
	if err != nil {
		t.Fatalf("check 2: %v", err)
	}

	crs, err := s.ListCheckRuns(ctx, "default/default", "main", nil, false)
	if err != nil {
		t.Fatalf("ListCheckRuns: %v", err)
	}
	if len(crs) != 2 {
		t.Fatalf("expected 2 check runs, got %d", len(crs))
	}
}

// TestListCheckRuns_UpsertSameAttempt verifies that posting pending then final
// status for the same (check_name, attempt) updates the row in place.
func TestListCheckRuns_UpsertSameAttempt(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Post pending at attempt 1.
	_, err := s.CreateCheckRun(ctx, "default/default", "main", "ci/build", model.CheckRunPending, "ci-bot", nil, nil, 1, nil)
	if err != nil {
		t.Fatalf("check pending: %v", err)
	}
	// Post passed at same attempt — should upsert (update status).
	_, err = s.CreateCheckRun(ctx, "default/default", "main", "ci/build", model.CheckRunPassed, "ci-bot", nil, nil, 1, nil)
	if err != nil {
		t.Fatalf("check passed: %v", err)
	}

	// history=false: only 1 row (latest attempt per check_name).
	crs, err := s.ListCheckRuns(ctx, "default/default", "main", nil, false)
	if err != nil {
		t.Fatalf("ListCheckRuns: %v", err)
	}
	if len(crs) != 1 {
		t.Fatalf("expected 1 check run (upserted), got %d", len(crs))
	}
	if crs[0].Status != model.CheckRunPassed {
		t.Errorf("expected passed after upsert, got %q", crs[0].Status)
	}
}

// TestRetryChecks verifies that RetryChecks inserts pending rows at the next
// attempt number and that history=true returns all attempts.
func TestRetryChecks(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// First attempt: passed.
	_, err := s.CreateCheckRun(ctx, "default/default", "main", "ci/build", model.CheckRunPassed, "ci-bot", nil, nil, 1, nil)
	if err != nil {
		t.Fatalf("CreateCheckRun: %v", err)
	}

	// Retry all checks at sequence 0.
	attempt, err := s.RetryChecks(ctx, "default/default", "main", 0, nil)
	if err != nil {
		t.Fatalf("RetryChecks: %v", err)
	}
	if attempt != 2 {
		t.Fatalf("expected attempt 2, got %d", attempt)
	}

	// history=false: only 1 row (latest attempt=2, pending).
	crs, err := s.ListCheckRuns(ctx, "default/default", "main", nil, false)
	if err != nil {
		t.Fatalf("ListCheckRuns false: %v", err)
	}
	if len(crs) != 1 {
		t.Fatalf("expected 1 check run (latest attempt), got %d", len(crs))
	}
	if crs[0].Attempt != 2 {
		t.Errorf("expected attempt 2, got %d", crs[0].Attempt)
	}
	if crs[0].Status != model.CheckRunPending {
		t.Errorf("expected pending, got %q", crs[0].Status)
	}

	// history=true: 2 rows (attempt 1 and attempt 2).
	all, err := s.ListCheckRuns(ctx, "default/default", "main", nil, true)
	if err != nil {
		t.Fatalf("ListCheckRuns true: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 rows with history=true, got %d", len(all))
	}
}

// ---------------------------------------------------------------------------
// Role store tests
// ---------------------------------------------------------------------------

func TestGetRole_Exists(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	if _, err := s.CreateRepo(ctx, model.CreateRepoRequest{Owner: "default", Name: "r1"}); err != nil {
		t.Fatalf("create repo: %v", err)
	}
	if err := s.SetRole(ctx, "default/r1", "alice@example.com", model.RoleWriter); err != nil {
		t.Fatalf("set role: %v", err)
	}

	role, err := s.GetRole(ctx, "default/r1", "alice@example.com")
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
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	if _, err := s.CreateRepo(ctx, model.CreateRepoRequest{Owner: "default", Name: "r1"}); err != nil {
		t.Fatalf("create repo: %v", err)
	}

	_, err := s.GetRole(ctx, "default/r1", "nobody@example.com")
	if !errors.Is(err, ErrRoleNotFound) {
		t.Errorf("expected ErrRoleNotFound, got %v", err)
	}
}

func TestSetRole(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	if _, err := s.CreateRepo(ctx, model.CreateRepoRequest{Owner: "default", Name: "r1"}); err != nil {
		t.Fatalf("create repo: %v", err)
	}

	// Assign role.
	if err := s.SetRole(ctx, "default/r1", "bob@example.com", model.RoleReader); err != nil {
		t.Fatalf("set role: %v", err)
	}
	role, _ := s.GetRole(ctx, "default/r1", "bob@example.com")
	if role.Role != model.RoleReader {
		t.Errorf("expected reader, got %q", role.Role)
	}

	// Upsert: update to admin.
	if err := s.SetRole(ctx, "default/r1", "bob@example.com", model.RoleAdmin); err != nil {
		t.Fatalf("update role: %v", err)
	}
	role, _ = s.GetRole(ctx, "default/r1", "bob@example.com")
	if role.Role != model.RoleAdmin {
		t.Errorf("expected admin after upsert, got %q", role.Role)
	}
}

func TestDeleteRole(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	if _, err := s.CreateRepo(ctx, model.CreateRepoRequest{Owner: "default", Name: "r1"}); err != nil {
		t.Fatalf("create repo: %v", err)
	}
	if err := s.SetRole(ctx, "default/r1", "carol@example.com", model.RoleMaintainer); err != nil {
		t.Fatalf("set role: %v", err)
	}

	// Delete the role.
	if err := s.DeleteRole(ctx, "default/r1", "carol@example.com"); err != nil {
		t.Fatalf("delete role: %v", err)
	}

	// Verify it's gone.
	if _, err := s.GetRole(ctx, "default/r1", "carol@example.com"); !errors.Is(err, ErrRoleNotFound) {
		t.Errorf("expected ErrRoleNotFound after delete, got %v", err)
	}

	// Delete again → ErrRoleNotFound.
	if err := s.DeleteRole(ctx, "default/r1", "carol@example.com"); !errors.Is(err, ErrRoleNotFound) {
		t.Errorf("expected ErrRoleNotFound on second delete, got %v", err)
	}
}

func TestRoleIsolation(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	for _, name := range []string{"default/repo-a", "default/repo-b"} {
		if _, err := s.CreateRepo(ctx, model.CreateRepoRequest{Owner: strings.SplitN(name, "/", 2)[0], Name: strings.SplitN(name, "/", 2)[1]}); err != nil {
			t.Fatalf("create repo %s: %v", name, err)
		}
	}

	// Set alice as admin in repo-a only.
	if err := s.SetRole(ctx, "default/repo-a", "alice@example.com", model.RoleAdmin); err != nil {
		t.Fatalf("set role: %v", err)
	}

	// Alice has role in repo-a.
	role, err := s.GetRole(ctx, "default/repo-a", "alice@example.com")
	if err != nil || role.Role != model.RoleAdmin {
		t.Errorf("expected admin in repo-a, got %v %v", role, err)
	}

	// Alice has NO role in repo-b.
	if _, err := s.GetRole(ctx, "default/repo-b", "alice@example.com"); !errors.Is(err, ErrRoleNotFound) {
		t.Errorf("expected ErrRoleNotFound for alice in repo-b, got %v", err)
	}

	// HasAdmin for repo-a is true.
	hasAdmin, err := s.HasAdmin(ctx, "default/repo-a")
	if err != nil || !hasAdmin {
		t.Errorf("expected HasAdmin true for repo-a, got %v %v", hasAdmin, err)
	}

	// HasAdmin for repo-b is false.
	hasAdmin, err = s.HasAdmin(ctx, "default/repo-b")
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
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Commit something to main so the branch has a non-zero base.
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo: "default/default", Branch: "main",
		Files:   []model.FileChange{{Path: "base.txt", Content: []byte("base")}},
		Message: "base", Author: "alice",
	})
	if err != nil {
		t.Fatalf("commit to main: %v", err)
	}

	// Create a branch, commit to it, then merge.
	_, err = s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default/default", Name: "feature/merged"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo: "default/default", Branch: "feature/merged",
		Files:   []model.FileChange{{Path: "f.txt", Content: []byte("branch work")}},
		Message: "work", Author: "alice",
	})
	if err != nil {
		t.Fatalf("commit to branch: %v", err)
	}
	_, _, err = s.Merge(ctx, model.MergeRequest{Repo: "default/default", Branch: "feature/merged", Author: "alice"})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	// Make the branch and its commits appear old.
	makeOld(t, d, "default/default", "feature/merged")

	result, err := s.Purge(ctx, PurgeRequest{Repo: "default/default", OlderThan: 24 * time.Hour})
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
	d.QueryRow("SELECT count(*) FROM branches WHERE repo = 'default/default' AND name = 'feature/merged'").Scan(&count)
	if count != 0 {
		t.Errorf("expected branch to be deleted, found %d rows", count)
	}
}

func TestPurge_DeletesAbandonedBranches(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default/default", Name: "feature/abandoned"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo: "default/default", Branch: "feature/abandoned",
		Files:   []model.FileChange{{Path: "x.txt", Content: []byte("x")}},
		Message: "x", Author: "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := s.DeleteBranch(ctx, "default/default", "feature/abandoned"); err != nil {
		t.Fatalf("delete branch: %v", err)
	}

	makeOld(t, d, "default/default", "feature/abandoned")

	result, err := s.Purge(ctx, PurgeRequest{Repo: "default/default", OlderThan: 24 * time.Hour})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if result.BranchesPurged != 1 {
		t.Errorf("expected 1 branch purged, got %d", result.BranchesPurged)
	}

	var count int
	d.QueryRow("SELECT count(*) FROM branches WHERE repo = 'default/default' AND name = 'feature/abandoned'").Scan(&count)
	if count != 0 {
		t.Errorf("expected abandoned branch to be deleted, found %d rows", count)
	}
}

func TestPurge_RespectsOlderThan(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Create a branch and merge it — timestamps are current (now), not old.
	_, err := s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default/default", Name: "feature/recent"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}
	_, _, err = s.Merge(ctx, model.MergeRequest{Repo: "default/default", Branch: "feature/recent", Author: "alice"})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	// Purge with 1-day threshold — recently merged branch must NOT be purged.
	result, err := s.Purge(ctx, PurgeRequest{Repo: "default/default", OlderThan: 24 * time.Hour})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if result.BranchesPurged != 0 {
		t.Errorf("expected 0 branches purged, got %d", result.BranchesPurged)
	}

	var count int
	d.QueryRow("SELECT count(*) FROM branches WHERE repo = 'default/default' AND name = 'feature/recent'").Scan(&count)
	if count != 1 {
		t.Errorf("expected recent branch to still exist, got count=%d", count)
	}
}

func TestPurge_DoesNotTouchActiveBranches(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default/default", Name: "feature/active"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}
	// Age the branch row so it would be eligible if it were merged/abandoned.
	makeOld(t, d, "default/default", "feature/active")

	result, err := s.Purge(ctx, PurgeRequest{Repo: "default/default", OlderThan: 24 * time.Hour})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if result.BranchesPurged != 0 {
		t.Errorf("expected 0 branches purged (active branch must be skipped), got %d", result.BranchesPurged)
	}
}

func TestPurge_DoesNotTouchMain(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.Commit(ctx, model.CommitRequest{
		Repo: "default/default", Branch: "main",
		Files:   []model.FileChange{{Path: "m.txt", Content: []byte("main")}},
		Message: "m", Author: "alice",
	})
	if err != nil {
		t.Fatalf("commit to main: %v", err)
	}
	// Force main's created_at to be very old (should never be eligible due to name filter).
	d.Exec(`UPDATE branches SET created_at = now() - interval '100 days' WHERE repo = 'default/default' AND name = 'main'`)
	d.Exec(`UPDATE commits SET created_at = now() - interval '100 days' WHERE repo = 'default/default' AND branch = 'main'`)

	result, err := s.Purge(ctx, PurgeRequest{Repo: "default/default", OlderThan: 24 * time.Hour})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if result.BranchesPurged != 0 {
		t.Errorf("expected main to be untouched, but %d branches purged", result.BranchesPurged)
	}

	// main must still exist.
	var count int
	d.QueryRow("SELECT count(*) FROM branches WHERE repo = 'default/default' AND name = 'main'").Scan(&count)
	if count != 1 {
		t.Errorf("expected main branch to still exist")
	}
}

func TestPurge_OrphanedDocumentsDeleted(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Create a branch, commit a file, then abandon it without merging.
	_, err := s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default/default", Name: "feature/orphan"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}
	commitResp, err := s.Commit(ctx, model.CommitRequest{
		Repo: "default/default", Branch: "feature/orphan",
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

	if err := s.DeleteBranch(ctx, "default/default", "feature/orphan"); err != nil {
		t.Fatalf("delete branch: %v", err)
	}
	makeOld(t, d, "default/default", "feature/orphan")

	result, err := s.Purge(ctx, PurgeRequest{Repo: "default/default", OlderThan: 24 * time.Hour})
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
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Commit a file to main first.
	mainResp, err := s.Commit(ctx, model.CommitRequest{
		Repo: "default/default", Branch: "main",
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
	_, err = s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default/default", Name: "feature/shared"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo: "default/default", Branch: "feature/shared",
		Files:   []model.FileChange{{Path: "shared.txt", Content: []byte("shared content")}},
		Message: "branch", Author: "alice",
	})
	if err != nil {
		t.Fatalf("commit to branch: %v", err)
	}
	if err := s.DeleteBranch(ctx, "default/default", "feature/shared"); err != nil {
		t.Fatalf("delete branch: %v", err)
	}
	makeOld(t, d, "default/default", "feature/shared")

	result, err := s.Purge(ctx, PurgeRequest{Repo: "default/default", OlderThan: 24 * time.Hour})
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
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default/default", Name: "feature/rev"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo: "default/default", Branch: "feature/rev",
		Files:   []model.FileChange{{Path: "r.txt", Content: []byte("r")}},
		Message: "r", Author: "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Add a review and a check_run for the branch.
	_, err = s.CreateReview(ctx, "default/default", "feature/rev", "bob", model.ReviewApproved, "LGTM")
	if err != nil {
		t.Fatalf("create review: %v", err)
	}
	_, err = s.CreateCheckRun(ctx, "default/default", "feature/rev", "ci/build", model.CheckRunPassed, "ci-bot", nil, nil, 1, nil)
	if err != nil {
		t.Fatalf("create check_run: %v", err)
	}

	if err := s.DeleteBranch(ctx, "default/default", "feature/rev"); err != nil {
		t.Fatalf("delete branch: %v", err)
	}
	makeOld(t, d, "default/default", "feature/rev")

	result, err := s.Purge(ctx, PurgeRequest{Repo: "default/default", OlderThan: 24 * time.Hour})
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
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default/default", Name: "feature/dry"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo: "default/default", Branch: "feature/dry",
		Files:   []model.FileChange{{Path: "d.txt", Content: []byte("dry run content")}},
		Message: "dry", Author: "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := s.DeleteBranch(ctx, "default/default", "feature/dry"); err != nil {
		t.Fatalf("delete branch: %v", err)
	}
	makeOld(t, d, "default/default", "feature/dry")

	// Count before dry run.
	var branchBefore, fcBefore int
	d.QueryRow("SELECT count(*) FROM branches WHERE repo = 'default/default' AND name = 'feature/dry'").Scan(&branchBefore)
	d.QueryRow("SELECT count(*) FROM file_commits WHERE repo = 'default/default' AND branch = 'feature/dry'").Scan(&fcBefore)

	result, err := s.Purge(ctx, PurgeRequest{Repo: "default/default", OlderThan: 24 * time.Hour, DryRun: true})
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
	d.QueryRow("SELECT count(*) FROM branches WHERE repo = 'default/default' AND name = 'feature/dry'").Scan(&branchAfter)
	d.QueryRow("SELECT count(*) FROM file_commits WHERE repo = 'default/default' AND branch = 'feature/dry'").Scan(&fcAfter)

	if branchAfter != branchBefore {
		t.Errorf("dry run must not delete branches: before=%d after=%d", branchBefore, branchAfter)
	}
	if fcAfter != fcBefore {
		t.Errorf("dry run must not delete file_commits: before=%d after=%d", fcBefore, fcAfter)
	}
}

func TestPurge_IsAtomic(t *testing.T) {
	t.Parallel()
	// Demonstrates that all deletes are committed together (or not at all).
	// We use a pre-cancelled context so BeginTx fails before any deletions occur,
	// verifying that no partial state is left behind.
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default/default", Name: "feature/atomic"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo: "default/default", Branch: "feature/atomic",
		Files:   []model.FileChange{{Path: "a.txt", Content: []byte("atomic")}},
		Message: "atomic", Author: "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := s.DeleteBranch(ctx, "default/default", "feature/atomic"); err != nil {
		t.Fatalf("delete branch: %v", err)
	}
	makeOld(t, d, "default/default", "feature/atomic")

	var fcBefore int
	d.QueryRow("SELECT count(*) FROM file_commits WHERE repo = 'default/default' AND branch = 'feature/atomic'").Scan(&fcBefore)

	// Use a cancelled context — BeginTx will fail immediately.
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()

	_, err = s.Purge(cancelCtx, PurgeRequest{Repo: "default/default", OlderThan: 24 * time.Hour})
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}

	// Verify no data was changed.
	var fcAfter int
	d.QueryRow("SELECT count(*) FROM file_commits WHERE repo = 'default/default' AND branch = 'feature/atomic'").Scan(&fcAfter)
	if fcAfter != fcBefore {
		t.Errorf("partial deletion occurred: before=%d after=%d file_commits", fcBefore, fcAfter)
	}

	var branchCount int
	d.QueryRow("SELECT count(*) FROM branches WHERE repo = 'default/default' AND name = 'feature/atomic'").Scan(&branchCount)
	if branchCount != 1 {
		t.Errorf("branch should still exist after failed purge, count=%d", branchCount)
	}
}

// ---------------------------------------------------------------------------
// Conflict body assertions — multiple conflicts
// ---------------------------------------------------------------------------

func TestMerge_MultipleConflicts(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Commit to main (seq=1): two files that will both conflict.
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo:   "default/default",
		Branch: "main",
		Files: []model.FileChange{
			{Path: "alpha.txt", Content: []byte("alpha original")},
			{Path: "beta.txt", Content: []byte("beta original")},
		},
		Message: "initial",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Create branch (base=1).
	_, err = s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default/default", Name: "feature/multi-conflict"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	// Main modifies both files (seq=2).
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:   "default/default",
		Branch: "main",
		Files: []model.FileChange{
			{Path: "alpha.txt", Content: []byte("alpha main")},
			{Path: "beta.txt", Content: []byte("beta main")},
		},
		Message: "main edit",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("main commit: %v", err)
	}

	// Branch modifies both files (seq=3).
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:   "default/default",
		Branch: "feature/multi-conflict",
		Files: []model.FileChange{
			{Path: "alpha.txt", Content: []byte("alpha branch")},
			{Path: "beta.txt", Content: []byte("beta branch")},
		},
		Message: "branch edit",
		Author:  "bob",
	})
	if err != nil {
		t.Fatalf("branch commit: %v", err)
	}

	_, conflicts, err := s.Merge(ctx, model.MergeRequest{Repo: "default/default", Branch: "feature/multi-conflict"})
	if err != ErrMergeConflict {
		t.Fatalf("expected ErrMergeConflict, got %v", err)
	}
	if len(conflicts) != 2 {
		t.Fatalf("expected 2 conflicts, got %d", len(conflicts))
	}

	// Collect conflict paths to verify both are reported.
	paths := make(map[string]MergeConflict, 2)
	for _, c := range conflicts {
		paths[c.Path] = c
	}
	for _, path := range []string{"alpha.txt", "beta.txt"} {
		c, ok := paths[path]
		if !ok {
			t.Errorf("expected conflict on %s not found", path)
			continue
		}
		if c.MainVersionID == "" {
			t.Errorf("%s: expected non-empty MainVersionID", path)
		}
		if c.BranchVersionID == "" {
			t.Errorf("%s: expected non-empty BranchVersionID", path)
		}
		if c.MainVersionID == c.BranchVersionID {
			t.Errorf("%s: MainVersionID and BranchVersionID should differ", path)
		}
	}
}

func TestRebase_MultipleConflicts(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Commit to main (seq=1).
	_, err := s.Commit(ctx, model.CommitRequest{
		Repo:   "default/default",
		Branch: "main",
		Files: []model.FileChange{
			{Path: "alpha.txt", Content: []byte("alpha original")},
			{Path: "beta.txt", Content: []byte("beta original")},
		},
		Message: "initial",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Create branch (base=1).
	_, err = s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default/default", Name: "feature/rebase-multi"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	// Branch modifies both files (seq=2).
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:   "default/default",
		Branch: "feature/rebase-multi",
		Files: []model.FileChange{
			{Path: "alpha.txt", Content: []byte("alpha branch")},
			{Path: "beta.txt", Content: []byte("beta branch")},
		},
		Message: "branch edit",
		Author:  "bob",
	})
	if err != nil {
		t.Fatalf("branch commit: %v", err)
	}

	// Main modifies both files (seq=3).
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:   "default/default",
		Branch: "main",
		Files: []model.FileChange{
			{Path: "alpha.txt", Content: []byte("alpha main")},
			{Path: "beta.txt", Content: []byte("beta main")},
		},
		Message: "main edit",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("main commit: %v", err)
	}

	_, conflicts, err := s.Rebase(ctx, model.RebaseRequest{Repo: "default/default", Branch: "feature/rebase-multi"})
	if err != ErrRebaseConflict {
		t.Fatalf("expected ErrRebaseConflict, got %v", err)
	}
	if len(conflicts) != 2 {
		t.Fatalf("expected 2 conflicts, got %d", len(conflicts))
	}

	paths := make(map[string]MergeConflict, 2)
	for _, c := range conflicts {
		paths[c.Path] = c
	}
	for _, path := range []string{"alpha.txt", "beta.txt"} {
		c, ok := paths[path]
		if !ok {
			t.Errorf("expected conflict on %s not found", path)
			continue
		}
		if c.MainVersionID == "" {
			t.Errorf("%s: expected non-empty MainVersionID", path)
		}
		if c.BranchVersionID == "" {
			t.Errorf("%s: expected non-empty BranchVersionID", path)
		}
		if c.MainVersionID == c.BranchVersionID {
			t.Errorf("%s: MainVersionID and BranchVersionID should differ", path)
		}
	}
}

// ---------------------------------------------------------------------------
// Branch lifecycle edge cases
// ---------------------------------------------------------------------------

func TestDeleteBranch_MainBranch(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	err := s.DeleteBranch(ctx, "default/default", "main")
	if err != ErrBranchNotActive {
		t.Fatalf("expected ErrBranchNotActive when deleting main, got %v", err)
	}

	// Verify main is still active.
	var status string
	if err := d.QueryRow("SELECT status FROM branches WHERE name = 'main'").Scan(&status); err != nil {
		t.Fatalf("query branch: %v", err)
	}
	if status != "active" {
		t.Errorf("expected main to remain active, got %q", status)
	}
}

func TestMerge_AlreadyMergedBranch(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Create branch, commit, then merge it.
	_, err := s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default/default", Name: "feature/once"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "feature/once",
		Files:   []model.FileChange{{Path: "f.txt", Content: []byte("content")}},
		Message: "work",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	_, _, err = s.Merge(ctx, model.MergeRequest{Repo: "default/default", Branch: "feature/once"})
	if err != nil {
		t.Fatalf("first merge: %v", err)
	}

	// Second merge of the same branch must fail.
	_, _, err = s.Merge(ctx, model.MergeRequest{Repo: "default/default", Branch: "feature/once"})
	if err != ErrBranchNotActive {
		t.Fatalf("expected ErrBranchNotActive on second merge, got %v", err)
	}
}

func TestCommit_ToMergedBranch(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Create branch, commit, merge it, then try to commit again.
	_, err := s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default/default", Name: "feature/merged"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "feature/merged",
		Files:   []model.FileChange{{Path: "x.txt", Content: []byte("x")}},
		Message: "initial",
		Author:  "alice",
	})
	if err != nil {
		t.Fatalf("initial commit: %v", err)
	}
	_, _, err = s.Merge(ctx, model.MergeRequest{Repo: "default/default", Branch: "feature/merged"})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	// Commit to merged branch must fail.
	_, err = s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "feature/merged",
		Files:   []model.FileChange{{Path: "x.txt", Content: []byte("updated")}},
		Message: "post-merge commit",
		Author:  "alice",
	})
	if err != ErrBranchNotActive {
		t.Fatalf("expected ErrBranchNotActive for commit to merged branch, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Org CRUD tests
// ---------------------------------------------------------------------------

func TestCreateOrg(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	org, err := s.CreateOrg(ctx, "myorg", "alice@example.com")
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if org.Name != "myorg" {
		t.Errorf("expected name myorg, got %q", org.Name)
	}
	if org.CreatedBy != "alice@example.com" {
		t.Errorf("expected created_by alice@example.com, got %q", org.CreatedBy)
	}

	// Duplicate name returns ErrOrgExists.
	_, err = s.CreateOrg(ctx, "myorg", "bob@example.com")
	if !errors.Is(err, ErrOrgExists) {
		t.Errorf("expected ErrOrgExists on duplicate, got %v", err)
	}
}

func TestCreateOrg_DefaultCreatedBy(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	org, err := s.CreateOrg(ctx, "auto-org", "")
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if org.CreatedBy != "system" {
		t.Errorf("expected created_by system, got %q", org.CreatedBy)
	}
}

func TestDeleteOrg(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	if _, err := s.CreateOrg(ctx, "todelete", "alice"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	// Delete succeeds.
	if err := s.DeleteOrg(ctx, "todelete"); err != nil {
		t.Fatalf("DeleteOrg: %v", err)
	}

	// Second delete returns ErrOrgNotFound.
	if err := s.DeleteOrg(ctx, "todelete"); !errors.Is(err, ErrOrgNotFound) {
		t.Errorf("expected ErrOrgNotFound on second delete, got %v", err)
	}
}

func TestDeleteOrg_NotFound(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	err := s.DeleteOrg(ctx, "nonexistent")
	if !errors.Is(err, ErrOrgNotFound) {
		t.Errorf("expected ErrOrgNotFound, got %v", err)
	}
}

func TestDeleteOrg_HasRepos(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	if _, err := s.CreateOrg(ctx, "orgwithrepos", "alice"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := s.CreateRepo(ctx, model.CreateRepoRequest{Owner: "orgwithrepos", Name: "myrepo"}); err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}

	err := s.DeleteOrg(ctx, "orgwithrepos")
	if !errors.Is(err, ErrOrgHasRepos) {
		t.Errorf("expected ErrOrgHasRepos, got %v", err)
	}
}

func TestListOrgRepos(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	if _, err := s.CreateOrg(ctx, "listorg", "alice"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	// No repos yet.
	repos, err := s.ListOrgRepos(ctx, "listorg")
	if err != nil {
		t.Fatalf("ListOrgRepos (empty): %v", err)
	}
	if len(repos) != 0 {
		t.Errorf("expected 0 repos, got %d", len(repos))
	}

	// Create two repos.
	for _, name := range []string{"alpha", "beta"} {
		if _, err := s.CreateRepo(ctx, model.CreateRepoRequest{Owner: "listorg", Name: name}); err != nil {
			t.Fatalf("CreateRepo %s: %v", name, err)
		}
	}

	repos, err = s.ListOrgRepos(ctx, "listorg")
	if err != nil {
		t.Fatalf("ListOrgRepos: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("expected 2 repos, got %d", len(repos))
	}
	if repos[0].Name != "listorg/alpha" || repos[1].Name != "listorg/beta" {
		t.Errorf("unexpected repo names: %v, %v", repos[0].Name, repos[1].Name)
	}
}

func TestListRepos(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// default org already has default/default from migration.
	repos, err := s.ListRepos(ctx)
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	initialCount := len(repos)

	// Create an extra org and repo.
	if _, err := s.CreateOrg(ctx, "extra", "alice"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := s.CreateRepo(ctx, model.CreateRepoRequest{Owner: "extra", Name: "newrepo"}); err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}

	repos, err = s.ListRepos(ctx)
	if err != nil {
		t.Fatalf("ListRepos after create: %v", err)
	}
	if len(repos) != initialCount+1 {
		t.Errorf("expected %d repos, got %d", initialCount+1, len(repos))
	}
}

func TestGetRepo(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// default/default exists from migration seed.
	repo, err := s.GetRepo(ctx, "default/default")
	if err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	if repo.Name != "default/default" {
		t.Errorf("expected name default/default, got %q", repo.Name)
	}

	// Non-existent repo.
	_, err = s.GetRepo(ctx, "default/noexist")
	if !errors.Is(err, ErrRepoNotFound) {
		t.Errorf("expected ErrRepoNotFound, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// ListRoles DB test
// ---------------------------------------------------------------------------

func TestListRoles(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Empty initially.
	roles, err := s.ListRoles(ctx, "default/default")
	if err != nil {
		t.Fatalf("ListRoles (empty): %v", err)
	}
	if len(roles) != 0 {
		t.Errorf("expected 0 roles, got %d", len(roles))
	}

	// Add a couple of roles.
	if err := s.SetRole(ctx, "default/default", "alice@example.com", model.RoleAdmin); err != nil {
		t.Fatalf("SetRole alice: %v", err)
	}
	if err := s.SetRole(ctx, "default/default", "bob@example.com", model.RoleReader); err != nil {
		t.Fatalf("SetRole bob: %v", err)
	}

	roles, err = s.ListRoles(ctx, "default/default")
	if err != nil {
		t.Fatalf("ListRoles: %v", err)
	}
	if len(roles) != 2 {
		t.Fatalf("expected 2 roles, got %d", len(roles))
	}
	// Results should be ordered by identity.
	if roles[0].Identity != "alice@example.com" || roles[0].Role != model.RoleAdmin {
		t.Errorf("unexpected first role: %+v", roles[0])
	}
	if roles[1].Identity != "bob@example.com" || roles[1].Role != model.RoleReader {
		t.Errorf("unexpected second role: %+v", roles[1])
	}
}

// ---------------------------------------------------------------------------
// isForeignKeyViolation / isDuplicateKeyError helper tests
// ---------------------------------------------------------------------------

func TestIsForeignKeyViolation_NonPQError(t *testing.T) {
	t.Parallel()
	// A plain error should return false.
	if isForeignKeyViolation(errors.New("some error")) {
		t.Error("expected false for non-pq error")
	}
}

func TestIsDuplicateKeyError_NonPQError(t *testing.T) {
	t.Parallel()
	if isDuplicateKeyError(errors.New("some error")) {
		t.Error("expected false for non-pq error")
	}
}

func TestIsForeignKeyViolation_FromDB(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Trying to create a repo for a non-existent org triggers a FK violation.
	_, err := s.CreateRepo(ctx, model.CreateRepoRequest{Owner: "nosuchorg", Name: "somerepo"})
	if !errors.Is(err, ErrOrgNotFound) {
		t.Errorf("expected ErrOrgNotFound (wrapping FK violation), got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Hash chain tests
// ---------------------------------------------------------------------------

// expectedCommitHash is a regression sentinel: the known SHA256 for the fixed
// test inputs used in TestComputeCommitHash. If the hash formula changes, this
// constant must be updated deliberately (it catches accidental formula drift).
// Inputs: prevHash=hash.GenesisHash, seq=1, repo="myorg/repo", branch="main",
// author="alice", message="first commit", ts=2024-01-01T00:00:00Z,
// files=[{a.txt:aaa},{b.txt:bbb}] (sorted alphabetically by path).
const expectedCommitHash = "54662b84dcaca30b108d9b779bec9ad8727f35db9e6742401aaa5d09a5a5b987"

// TestComputeCommitHash verifies that computeCommitHash produces a stable SHA256 output.
func TestComputeCommitHash(t *testing.T) {
	t.Parallel()
	ts := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	files := []hash.File{
		{Path: "b.txt", ContentHash: "bbb"},
		{Path: "a.txt", ContentHash: "aaa"},
	}
	// Files are sorted inside computeCommitHash, so order of input shouldn't matter.
	got := hash.CommitHash(hash.GenesisHash, 1, "myorg/repo", "main", "alice", "first commit", ts, files)
	if len(got) != 64 {
		t.Fatalf("expected 64-char hex string, got %q (len=%d)", got, len(got))
	}

	// Regression sentinel: any formula change will break this even if both sides are updated.
	if got != expectedCommitHash {
		t.Errorf("hash formula changed: got %s, want %s", got, expectedCommitHash)
	}

	// Recomputing with the same inputs must produce the same hash.
	got2 := hash.CommitHash(hash.GenesisHash, 1, "myorg/repo", "main", "alice", "first commit", ts, files)
	if got != got2 {
		t.Errorf("hash not deterministic: got %q vs %q", got, got2)
	}

	// Changing any field must produce a different hash.
	for _, tc := range []struct {
		name string
		fn   func() string
	}{
		{"author", func() string {
			return hash.CommitHash(hash.GenesisHash, 1, "myorg/repo", "main", "bob", "first commit", ts, files)
		}},
		{"message", func() string {
			return hash.CommitHash(hash.GenesisHash, 1, "myorg/repo", "main", "alice", "other msg", ts, files)
		}},
		{"sequence", func() string {
			return hash.CommitHash(hash.GenesisHash, 2, "myorg/repo", "main", "alice", "first commit", ts, files)
		}},
		{"repo", func() string {
			return hash.CommitHash(hash.GenesisHash, 1, "other/repo", "main", "alice", "first commit", ts, files)
		}},
		{"prevHash", func() string {
			return hash.CommitHash("aaaa"+hash.GenesisHash[4:], 1, "myorg/repo", "main", "alice", "first commit", ts, files)
		}},
	} {
		if diff := tc.fn(); diff == got {
			t.Errorf("changing %s should change the hash", tc.name)
		}
	}

	// Verify expected value by building the same hash inline.
	// computeCommitHash sorts files internally, so feed them sorted.
	h := sha256.New()
	h.Write([]byte(hash.GenesisHash + "\n"))
	h.Write([]byte("1\n"))
	h.Write([]byte("myorg/repo\n"))
	h.Write([]byte("main\n"))
	h.Write([]byte("alice\n"))
	h.Write([]byte("first commit\n"))
	// Use the same format string as the production code.
	h.Write([]byte(ts.UTC().Format(time.RFC3339Nano) + "\n"))
	// Sorted alphabetically by path: a.txt then b.txt.
	h.Write([]byte("a.txt:aaa\n"))
	h.Write([]byte("b.txt:bbb\n"))
	expected := hex.EncodeToString(h.Sum(nil))
	if got != expected {
		t.Errorf("hash mismatch: got %s, want %s", got, expected)
	}
}

// TestCommit_HashIsSet verifies that Commit stores a non-empty commit_hash.
func TestCommit_HashIsSet(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	resp, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "hello.txt", Content: []byte("hello")}},
		Message: "first commit",
		Author:  "alice@example.com",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	var commitHash sql.NullString
	err = d.QueryRowContext(ctx,
		"SELECT commit_hash FROM commits WHERE sequence = $1",
		resp.Sequence,
	).Scan(&commitHash)
	if err != nil {
		t.Fatalf("query commit_hash: %v", err)
	}
	if !commitHash.Valid || commitHash.String == "" {
		t.Fatal("expected commit_hash to be set, got NULL or empty")
	}
	if len(commitHash.String) != 64 {
		t.Errorf("expected 64-char hex commit_hash, got %q", commitHash.String)
	}
}

// TestCommit_ChainLinks verifies that the second commit's hash links to the first.
func TestCommit_ChainLinks(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	resp1, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "a.txt", Content: []byte("alpha")}},
		Message: "first",
		Author:  "alice@example.com",
	})
	if err != nil {
		t.Fatalf("commit 1: %v", err)
	}

	resp2, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "b.txt", Content: []byte("beta")}},
		Message: "second",
		Author:  "alice@example.com",
	})
	if err != nil {
		t.Fatalf("commit 2: %v", err)
	}

	var hash1, hash2 sql.NullString
	if err := d.QueryRowContext(ctx, "SELECT commit_hash FROM commits WHERE sequence = $1", resp1.Sequence).Scan(&hash1); err != nil {
		t.Fatalf("query hash1: %v", err)
	}
	if err := d.QueryRowContext(ctx, "SELECT commit_hash FROM commits WHERE sequence = $1", resp2.Sequence).Scan(&hash2); err != nil {
		t.Fatalf("query hash2: %v", err)
	}
	if !hash1.Valid || !hash2.Valid {
		t.Fatal("expected both hashes to be set")
	}
	if hash1.String == hash2.String {
		t.Error("expected different hashes for different commits")
	}

	// Verify actual chain linkage: recompute hash2 using hash1 as prevHash and
	// compare to the stored value. This proves the chain links correctly, not just
	// that the two hashes differ.
	var author2, message2 string
	var createdAt2 time.Time
	if err := d.QueryRowContext(ctx,
		"SELECT author, message, created_at FROM commits WHERE sequence = $1", resp2.Sequence,
	).Scan(&author2, &message2, &createdAt2); err != nil {
		t.Fatalf("query commit2 metadata: %v", err)
	}
	// Get content hash for a.txt at seq2 (b.txt is the new file, but we need the files committed in resp2).
	// resp2 committed b.txt; get its content hash from documents.
	var contentHash2 sql.NullString
	if err := d.QueryRowContext(ctx,
		`SELECT d.content_hash FROM file_commits fc
		 JOIN documents d ON d.version_id = fc.version_id AND d.repo = fc.repo
		 WHERE fc.sequence = $1 AND fc.path = 'b.txt'`, resp2.Sequence,
	).Scan(&contentHash2); err != nil {
		t.Fatalf("query content hash for b.txt: %v", err)
	}
	hashFiles2 := []hash.File{{Path: "b.txt", ContentHash: contentHash2.String}}
	recomputed2 := hash.CommitHash(hash1.String, resp2.Sequence, "default/default", "main", author2, message2, createdAt2, hashFiles2)
	if recomputed2 != hash2.String {
		t.Errorf("chain linkage broken: recomputed hash2=%s, stored hash2=%s", recomputed2, hash2.String)
	}
}

// TestCommit_PerBranchChain verifies that commits on different branches each
// start their own chain from hash.GenesisHash, independently of each other.
func TestCommit_PerBranchChain(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Commit to main.
	mainResp, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "base.txt", Content: []byte("base")}},
		Message: "main first",
		Author:  "alice@example.com",
	})
	if err != nil {
		t.Fatalf("main commit: %v", err)
	}

	// Create a feature branch and commit to it.
	if _, err = s.CreateBranch(ctx, model.CreateBranchRequest{Repo: "default/default", Name: "feature/x"}); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	featureResp, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "feature/x",
		Files:   []model.FileChange{{Path: "feat.txt", Content: []byte("feat")}},
		Message: "feature first",
		Author:  "bob@example.com",
	})
	if err != nil {
		t.Fatalf("feature commit: %v", err)
	}

	// Retrieve both commit hashes from the DB.
	var mainHash, featureHash sql.NullString
	if err := d.QueryRowContext(ctx, "SELECT commit_hash FROM commits WHERE sequence = $1", mainResp.Sequence).Scan(&mainHash); err != nil {
		t.Fatalf("query main hash: %v", err)
	}
	if err := d.QueryRowContext(ctx, "SELECT commit_hash FROM commits WHERE sequence = $1", featureResp.Sequence).Scan(&featureHash); err != nil {
		t.Fatalf("query feature hash: %v", err)
	}
	if !mainHash.Valid || !featureHash.Valid {
		t.Fatal("expected both hashes to be set")
	}

	// Both commits should be the first on their respective branches, so both
	// prevHash values should be hash.GenesisHash. Retrieve metadata to recompute.
	type commitMeta struct {
		author, message string
		createdAt       time.Time
	}
	getMeta := func(seq int64) commitMeta {
		t.Helper()
		var m commitMeta
		if err := d.QueryRowContext(ctx,
			"SELECT author, message, created_at FROM commits WHERE sequence = $1", seq,
		).Scan(&m.author, &m.message, &m.createdAt); err != nil {
			t.Fatalf("query meta seq=%d: %v", seq, err)
		}
		return m
	}
	getContentHash := func(seq int64, path string) string {
		t.Helper()
		var h sql.NullString
		if err := d.QueryRowContext(ctx,
			`SELECT d.content_hash FROM file_commits fc
			 JOIN documents d ON d.version_id = fc.version_id AND d.repo = fc.repo
			 WHERE fc.sequence = $1 AND fc.path = $2`, seq, path,
		).Scan(&h); err != nil {
			t.Fatalf("query content hash seq=%d path=%s: %v", seq, path, err)
		}
		return h.String
	}

	mainMeta := getMeta(mainResp.Sequence)
	featureMeta := getMeta(featureResp.Sequence)

	// Main commit: prevHash must be hash.GenesisHash (first on main).
	expectedMain := hash.CommitHash(hash.GenesisHash, mainResp.Sequence, "default/default", "main",
		mainMeta.author, mainMeta.message, mainMeta.createdAt,
		[]hash.File{{Path: "base.txt", ContentHash: getContentHash(mainResp.Sequence, "base.txt")}})
	if expectedMain != mainHash.String {
		t.Errorf("main chain: expected %s, got %s", expectedMain, mainHash.String)
	}

	// Feature commit: prevHash must be hash.GenesisHash (first on feature/x, independent of main).
	expectedFeature := hash.CommitHash(hash.GenesisHash, featureResp.Sequence, "default/default", "feature/x",
		featureMeta.author, featureMeta.message, featureMeta.createdAt,
		[]hash.File{{Path: "feat.txt", ContentHash: getContentHash(featureResp.Sequence, "feat.txt")}})
	if expectedFeature != featureHash.String {
		t.Errorf("feature chain: expected %s, got %s", expectedFeature, featureHash.String)
	}

	// The two branch chains are independent — their hashes should differ.
	if mainHash.String == featureHash.String {
		t.Error("expected different hashes for commits on different branches")
	}
}

// ---------------------------------------------------------------------------
// Org membership store tests
// ---------------------------------------------------------------------------

func TestOrgMember_AddListRemove(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Create a test org.
	org, err := s.CreateOrg(ctx, "testmemberorg", "system")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	// Add a member.
	if err := s.AddOrgMember(ctx, org.Name, "alice@example.com", "owner", "system"); err != nil {
		t.Fatalf("add member: %v", err)
	}

	// List members.
	members, err := s.ListOrgMembers(ctx, org.Name)
	if err != nil {
		t.Fatalf("list members: %v", err)
	}
	if len(members) != 1 {
		t.Fatalf("expected 1 member, got %d", len(members))
	}
	if members[0].Identity != "alice@example.com" {
		t.Errorf("unexpected identity: %s", members[0].Identity)
	}
	if members[0].Role != "owner" {
		t.Errorf("unexpected role: %s", members[0].Role)
	}

	// Get member.
	m, err := s.GetOrgMember(ctx, org.Name, "alice@example.com")
	if err != nil {
		t.Fatalf("get member: %v", err)
	}
	if m.Role != "owner" {
		t.Errorf("unexpected role: %s", m.Role)
	}

	// Upsert (role change).
	if err := s.AddOrgMember(ctx, org.Name, "alice@example.com", "member", "system"); err != nil {
		t.Fatalf("upsert member: %v", err)
	}
	m, err = s.GetOrgMember(ctx, org.Name, "alice@example.com")
	if err != nil {
		t.Fatalf("get member after upsert: %v", err)
	}
	if m.Role != "member" {
		t.Errorf("expected role=member after upsert, got %s", m.Role)
	}

	// Remove member.
	if err := s.RemoveOrgMember(ctx, org.Name, "alice@example.com"); err != nil {
		t.Fatalf("remove member: %v", err)
	}

	// Second remove → ErrOrgMemberNotFound.
	if err := s.RemoveOrgMember(ctx, org.Name, "alice@example.com"); !errors.Is(err, ErrOrgMemberNotFound) {
		t.Errorf("expected ErrOrgMemberNotFound, got %v", err)
	}
}

func TestOrgMember_GetNotFound(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	org, _ := s.CreateOrg(ctx, "testgetmemberorg", "system")
	_, err := s.GetOrgMember(ctx, org.Name, "nobody@example.com")
	if !errors.Is(err, ErrOrgMemberNotFound) {
		t.Errorf("expected ErrOrgMemberNotFound, got %v", err)
	}
}

func TestOrgInvite_CreateListRevoke(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	org, err := s.CreateOrg(ctx, "testinviteorg", "system")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	expiresAt := time.Now().Add(7 * 24 * time.Hour)
	inv, err := s.CreateInvite(ctx, org.Name, "bob@example.com", "member", "admin@example.com", "tok-abc123", expiresAt)
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	if inv.ID == "" {
		t.Fatal("expected non-empty invite ID")
	}
	if inv.Token != "tok-abc123" {
		t.Errorf("unexpected token: %q", inv.Token)
	}

	// List invites — should have 1.
	invites, err := s.ListInvites(ctx, org.Name)
	if err != nil {
		t.Fatalf("list invites: %v", err)
	}
	if len(invites) != 1 {
		t.Fatalf("expected 1 invite, got %d", len(invites))
	}

	// Revoke.
	if err := s.RevokeInvite(ctx, org.Name, inv.ID); err != nil {
		t.Fatalf("revoke invite: %v", err)
	}

	// List again — should be empty.
	invites, err = s.ListInvites(ctx, org.Name)
	if err != nil {
		t.Fatalf("list invites after revoke: %v", err)
	}
	if len(invites) != 0 {
		t.Errorf("expected 0 invites after revoke, got %d", len(invites))
	}

	// Second revoke → ErrInviteNotFound.
	if err := s.RevokeInvite(ctx, org.Name, inv.ID); !errors.Is(err, ErrInviteNotFound) {
		t.Errorf("expected ErrInviteNotFound, got %v", err)
	}
}

func TestOrgInvite_AcceptInvite(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	org, _ := s.CreateOrg(ctx, "testacceptorg", "system")

	expiresAt := time.Now().Add(7 * 24 * time.Hour)
	inv, err := s.CreateInvite(ctx, org.Name, "carol@example.com", "member", "admin@example.com", "tok-accept", expiresAt)
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}

	// Accept with wrong identity → ErrEmailMismatch.
	if err := s.AcceptInvite(ctx, org.Name, inv.Token, "other@example.com"); !errors.Is(err, ErrEmailMismatch) {
		t.Errorf("expected ErrEmailMismatch, got %v", err)
	}

	// Accept with correct identity.
	if err := s.AcceptInvite(ctx, org.Name, inv.Token, "carol@example.com"); err != nil {
		t.Fatalf("accept invite: %v", err)
	}

	// carol should now be a member.
	m, err := s.GetOrgMember(ctx, org.Name, "carol@example.com")
	if err != nil {
		t.Fatalf("get member after accept: %v", err)
	}
	if m.Role != "member" {
		t.Errorf("expected role=member, got %s", m.Role)
	}

	// Accept again → ErrInviteAlreadyAccepted.
	if err := s.AcceptInvite(ctx, org.Name, inv.Token, "carol@example.com"); !errors.Is(err, ErrInviteAlreadyAccepted) {
		t.Errorf("expected ErrInviteAlreadyAccepted, got %v", err)
	}

	// The accepted invite should not appear in ListInvites.
	invites, err := s.ListInvites(ctx, org.Name)
	if err != nil {
		t.Fatalf("list invites: %v", err)
	}
	if len(invites) != 0 {
		t.Errorf("expected 0 pending invites after accept, got %d", len(invites))
	}
}

func TestOrgInvite_ExpiredInvite(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	org, _ := s.CreateOrg(ctx, "testexpiredorg", "system")

	// Expires in the past.
	expiresAt := time.Now().Add(-1 * time.Hour)
	inv, err := s.CreateInvite(ctx, org.Name, "dave@example.com", "member", "admin@example.com", "tok-expired", expiresAt)
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}

	// Accept → ErrInviteExpired.
	if err := s.AcceptInvite(ctx, org.Name, inv.Token, "dave@example.com"); !errors.Is(err, ErrInviteExpired) {
		t.Errorf("expected ErrInviteExpired, got %v", err)
	}

	// Expired invites don't appear in ListInvites.
	invites, err := s.ListInvites(ctx, org.Name)
	if err != nil {
		t.Fatalf("list invites: %v", err)
	}
	if len(invites) != 0 {
		t.Errorf("expected 0 invites (expired excluded), got %d", len(invites))
	}
}

func TestGetRole_OrgFallback(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Create org and repo.
	org, _ := s.CreateOrg(ctx, "testroleorg", "system")
	_, err := s.CreateRepo(ctx, model.CreateRepoRequest{Owner: org.Name, Name: "myrepo", CreatedBy: "system"})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	repoName := org.Name + "/myrepo"

	// Add an org owner.
	_ = s.AddOrgMember(ctx, org.Name, "owner@example.com", "owner", "system")
	// Add an org member.
	_ = s.AddOrgMember(ctx, org.Name, "member@example.com", "member", "system")

	// org owner → repo admin via fallback.
	role, err := s.GetRole(ctx, repoName, "owner@example.com")
	if err != nil {
		t.Fatalf("get role for org owner: %v", err)
	}
	if role.Role != model.RoleAdmin {
		t.Errorf("expected admin for org owner, got %s", role.Role)
	}

	// org member → repo reader via fallback.
	role, err = s.GetRole(ctx, repoName, "member@example.com")
	if err != nil {
		t.Fatalf("get role for org member: %v", err)
	}
	if role.Role != model.RoleReader {
		t.Errorf("expected reader for org member, got %s", role.Role)
	}

	// Not a member → ErrRoleNotFound.
	_, err = s.GetRole(ctx, repoName, "stranger@example.com")
	if !errors.Is(err, ErrRoleNotFound) {
		t.Errorf("expected ErrRoleNotFound for non-member, got %v", err)
	}

	// Explicit repo role should take precedence over org fallback.
	_ = s.SetRole(ctx, repoName, "member@example.com", model.RoleAdmin)
	role, err = s.GetRole(ctx, repoName, "member@example.com")
	if err != nil {
		t.Fatalf("get role after explicit set: %v", err)
	}
	if role.Role != model.RoleAdmin {
		t.Errorf("expected explicit admin role to win, got %s", role.Role)
	}
}

// ---------------------------------------------------------------------------
// Release store tests
// ---------------------------------------------------------------------------

func TestRelease_CreateListGetDelete(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Create a release.
	rel, err := s.CreateRelease(ctx, "default/default", "v1.0", 5, "initial release", "alice@example.com")
	if err != nil {
		t.Fatalf("CreateRelease: %v", err)
	}
	if rel.Name != "v1.0" || rel.Sequence != 5 || rel.Body != "initial release" || rel.CreatedBy != "alice@example.com" {
		t.Errorf("unexpected release: %+v", rel)
	}
	if rel.ID == "" {
		t.Error("expected non-empty ID")
	}

	// Create a second release (different name).
	rel2, err := s.CreateRelease(ctx, "default/default", "v2.0", 10, "", "bob@example.com")
	if err != nil {
		t.Fatalf("CreateRelease v2.0: %v", err)
	}
	if rel2.Name != "v2.0" || rel2.Sequence != 10 {
		t.Errorf("unexpected v2.0: %+v", rel2)
	}

	// Duplicate name returns ErrReleaseExists.
	_, err = s.CreateRelease(ctx, "default/default", "v1.0", 3, "", "carol@example.com")
	if !errors.Is(err, ErrReleaseExists) {
		t.Errorf("expected ErrReleaseExists, got %v", err)
	}

	// GetRelease returns the correct entry.
	got, err := s.GetRelease(ctx, "default/default", "v1.0")
	if err != nil {
		t.Fatalf("GetRelease: %v", err)
	}
	if got.Sequence != 5 || got.Body != "initial release" {
		t.Errorf("unexpected GetRelease result: %+v", got)
	}

	// GetRelease for unknown name returns ErrReleaseNotFound.
	_, err = s.GetRelease(ctx, "default/default", "nonexistent")
	if !errors.Is(err, ErrReleaseNotFound) {
		t.Errorf("expected ErrReleaseNotFound, got %v", err)
	}

	// ListReleases returns both, newest first.
	list, err := s.ListReleases(ctx, "default/default", 0, "")
	if err != nil {
		t.Fatalf("ListReleases: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 releases, got %d", len(list))
	}
	// v2.0 was created after v1.0 so should appear first.
	if list[0].Name != "v2.0" {
		t.Errorf("expected v2.0 first (newest), got %s", list[0].Name)
	}

	// ListReleases with limit=1.
	limited, err := s.ListReleases(ctx, "default/default", 1, "")
	if err != nil {
		t.Fatalf("ListReleases limit 1: %v", err)
	}
	if len(limited) != 1 {
		t.Fatalf("expected 1 release, got %d", len(limited))
	}
	if limited[0].Name != "v2.0" {
		t.Errorf("expected v2.0, got %s", limited[0].Name)
	}

	// ListReleases with afterID cursor (page 2).
	page2, err := s.ListReleases(ctx, "default/default", 0, limited[0].ID)
	if err != nil {
		t.Fatalf("ListReleases page2: %v", err)
	}
	if len(page2) != 1 || page2[0].Name != "v1.0" {
		t.Errorf("expected page2=[v1.0], got %+v", page2)
	}

	// DeleteRelease removes it.
	if err := s.DeleteRelease(ctx, "default/default", "v1.0"); err != nil {
		t.Fatalf("DeleteRelease: %v", err)
	}

	// Now GetRelease should return ErrReleaseNotFound.
	_, err = s.GetRelease(ctx, "default/default", "v1.0")
	if !errors.Is(err, ErrReleaseNotFound) {
		t.Errorf("expected ErrReleaseNotFound after delete, got %v", err)
	}

	// DeleteRelease for unknown name returns ErrReleaseNotFound.
	if err := s.DeleteRelease(ctx, "default/default", "nonexistent"); !errors.Is(err, ErrReleaseNotFound) {
		t.Errorf("expected ErrReleaseNotFound, got %v", err)
	}
}

func TestListReleases_InvalidCursor(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.ListReleases(ctx, "default/default", 10, "00000000-0000-0000-0000-000000000000")
	if !errors.Is(err, ErrInvalidCursor) {
		t.Errorf("expected ErrInvalidCursor, got %v", err)
	}
}
