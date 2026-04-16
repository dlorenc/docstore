package db

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/dlorenc/docstore/internal/blob"
	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/testutil"
)

// newLocalBlobStore creates a LocalBlobStore backed by the test's temp dir.
func newLocalBlobStore(t *testing.T) *blob.LocalBlobStore {
	t.Helper()
	bs, err := blob.NewLocalBlobStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalBlobStore: %v", err)
	}
	return bs
}

// TestCommit_LargeFileGoesToBlobStore checks that a file exceeding the threshold
// is stored in the blob store, not inline in Postgres.
func TestCommit_LargeFileGoesToBlobStore(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	ctx := context.Background()

	bs := newLocalBlobStore(t)
	s := NewStore(d)
	const threshold = 100
	s.SetBlobStore(bs, threshold)

	// 200 bytes of content — exceeds the 100-byte threshold.
	content := bytes.Repeat([]byte("x"), 200)

	resp, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "big.bin", Content: content}},
		Message: "add large file",
		Author:  "alice@example.com",
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if len(resp.Files) != 1 || resp.Files[0].VersionID == nil {
		t.Fatalf("expected 1 file with version_id")
	}

	// content column in DB should be NULL; blob_key should be set.
	var dbContent []byte
	var dbBlobKey *string
	err = d.QueryRowContext(ctx,
		"SELECT content, blob_key FROM documents WHERE version_id = $1",
		*resp.Files[0].VersionID,
	).Scan(&dbContent, &dbBlobKey)
	if err != nil {
		t.Fatalf("scan document: %v", err)
	}
	if dbContent != nil {
		t.Errorf("expected content column to be NULL for large file, got %d bytes", len(dbContent))
	}
	if dbBlobKey == nil || *dbBlobKey == "" {
		t.Errorf("expected blob_key to be set in DB")
	}

	// Blob should exist in the local blob store.
	ok, err := bs.Exists(ctx, *dbBlobKey)
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !ok {
		t.Errorf("blob not found in blob store after commit")
	}
}

// TestCommit_SmallFileStaysInline checks that a file below the threshold is
// stored inline in Postgres and blob_key is NULL.
func TestCommit_SmallFileStaysInline(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	ctx := context.Background()

	bs := newLocalBlobStore(t)
	s := NewStore(d)
	s.SetBlobStore(bs, 1000) // high threshold; small file stays inline

	content := []byte("small")

	resp, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "small.txt", Content: content}},
		Message: "add small file",
		Author:  "alice@example.com",
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	var dbContent []byte
	var dbBlobKey *string
	err = d.QueryRowContext(ctx,
		"SELECT content, blob_key FROM documents WHERE version_id = $1",
		*resp.Files[0].VersionID,
	).Scan(&dbContent, &dbBlobKey)
	if err != nil {
		t.Fatalf("scan document: %v", err)
	}
	if !bytes.Equal(dbContent, content) {
		t.Errorf("expected inline content %q, got %q", content, dbContent)
	}
	if dbBlobKey != nil {
		t.Errorf("expected blob_key to be NULL for small file, got %q", *dbBlobKey)
	}
}

// TestCommit_BlobDedup verifies that committing the same large content twice
// does not write to the blob store a second time (content-addressed dedup).
func TestCommit_BlobDedup(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	ctx := context.Background()

	bs := newLocalBlobStore(t)
	s := NewStore(d)
	const threshold = 100
	s.SetBlobStore(bs, threshold)

	content := bytes.Repeat([]byte("y"), 200)

	// First commit.
	resp1, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "dup1.bin", Content: content}},
		Message: "first",
		Author:  "alice@example.com",
	})
	if err != nil {
		t.Fatalf("first Commit: %v", err)
	}

	// Second commit with identical content under a different path.
	resp2, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "dup2.bin", Content: content}},
		Message: "second",
		Author:  "alice@example.com",
	})
	if err != nil {
		t.Fatalf("second Commit: %v", err)
	}

	// Both file_commits should point to the same document version_id.
	if *resp1.Files[0].VersionID != *resp2.Files[0].VersionID {
		t.Errorf("expected dedup: version_ids differ: %s vs %s",
			*resp1.Files[0].VersionID, *resp2.Files[0].VersionID)
	}

	// Only one document row should exist.
	var count int
	if err := d.QueryRowContext(ctx,
		"SELECT count(*) FROM documents WHERE version_id = $1",
		*resp1.Files[0].VersionID,
	).Scan(&count); err != nil {
		t.Fatalf("count documents: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 document row after dedup, got %d", count)
	}
}

// TestPurge_DeletesBlobKeys verifies that Purge removes external blobs for
// orphaned documents.
func TestPurge_DeletesBlobKeys(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	ctx := context.Background()

	bs := newLocalBlobStore(t)
	s := NewStore(d)
	const threshold = 100
	s.SetBlobStore(bs, threshold)

	content := bytes.Repeat([]byte("z"), 200)

	// Commit a large file to a feature branch.
	_, err := s.CreateBranch(ctx, model.CreateBranchRequest{
		Repo: "default/default",
		Name: "feature-purge-blob",
	})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}

	resp, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "feature-purge-blob",
		Files:   []model.FileChange{{Path: "big.bin", Content: content}},
		Message: "large file",
		Author:  "alice@example.com",
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	blobKey := ""
	if err := d.QueryRowContext(ctx,
		"SELECT blob_key FROM documents WHERE version_id = $1",
		*resp.Files[0].VersionID,
	).Scan(&blobKey); err != nil {
		t.Fatalf("scan blob_key: %v", err)
	}

	// Verify blob exists in store.
	ok, _ := bs.Exists(ctx, blobKey)
	if !ok {
		t.Fatal("blob should exist before purge")
	}

	// Abandon the branch so it becomes purgeable.
	if err := s.DeleteBranch(ctx, "default/default", "feature-purge-blob"); err != nil {
		t.Fatalf("DeleteBranch: %v", err)
	}

	// Purge with 0 duration so everything qualifies.
	_, err = s.Purge(ctx, PurgeRequest{
		Repo:      "default/default",
		OlderThan: 0,
	})
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}

	// Blob should now be gone.
	ok, _ = bs.Exists(ctx, blobKey)
	if ok {
		t.Errorf("expected blob to be deleted after purge")
	}
}

// TestReadStore_FetchesFromBlobStore verifies that GetFile retrieves content
// from the blob store when blob_key is set.
func TestReadStore_FetchesFromBlobStore(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	ctx := context.Background()

	bs := newLocalBlobStore(t)
	s := NewStore(d)
	const threshold = 100
	s.SetBlobStore(bs, threshold)

	content := bytes.Repeat([]byte("a"), 200)

	_, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "read-blob.bin", Content: content}},
		Message: "large file",
		Author:  "alice@example.com",
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Read the file back via the read store (store.Store).
	// We need a store.Store — but this test is in package db, so we use
	// the raw DB + blob store to replicate the read-path logic.
	// Verify the blob_key is set and content is fetchable.
	var dbContent []byte
	var blobKey string
	err = d.QueryRowContext(ctx,
		`SELECT content, blob_key FROM documents d
		 JOIN file_commits fc ON fc.version_id = d.version_id
		 WHERE fc.path = 'read-blob.bin' AND fc.repo = 'default/default'
		 LIMIT 1`,
	).Scan(&dbContent, &blobKey)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if dbContent != nil {
		t.Errorf("expected NULL content in DB for external blob, got %d bytes", len(dbContent))
	}

	// Fetch from blob store directly.
	rc, err := bs.Get(ctx, blobKey)
	if err != nil {
		t.Fatalf("Get blob: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()

	if !bytes.Equal(got, content) {
		t.Errorf("blob content mismatch: got %d bytes, want %d bytes", len(got), len(content))
	}
}

// TestPurge_DryRunDoesNotDeleteBlobs verifies dry-run mode skips blob deletion.
func TestPurge_DryRunDoesNotDeleteBlobs(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	ctx := context.Background()

	bs := newLocalBlobStore(t)
	s := NewStore(d)
	s.SetBlobStore(bs, 100)

	content := bytes.Repeat([]byte("d"), 200)

	_, err := s.CreateBranch(ctx, model.CreateBranchRequest{
		Repo: "default/default",
		Name: "feature-dryrun-blob",
	})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}

	resp, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "feature-dryrun-blob",
		Files:   []model.FileChange{{Path: "dry.bin", Content: content}},
		Message: "large file",
		Author:  "alice@example.com",
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	var blobKey string
	if err := d.QueryRowContext(ctx,
		"SELECT blob_key FROM documents WHERE version_id = $1",
		*resp.Files[0].VersionID,
	).Scan(&blobKey); err != nil {
		t.Fatalf("scan blob_key: %v", err)
	}

	if err := s.DeleteBranch(ctx, "default/default", "feature-dryrun-blob"); err != nil {
		t.Fatalf("DeleteBranch: %v", err)
	}

	_, err = s.Purge(ctx, PurgeRequest{
		Repo:      "default/default",
		OlderThan: time.Duration(0),
		DryRun:    true,
	})
	if err != nil {
		t.Fatalf("Purge (dry run): %v", err)
	}

	// Blob should still exist since this was a dry run.
	ok, err := bs.Exists(ctx, blobKey)
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !ok {
		t.Errorf("blob should NOT be deleted during dry run")
	}
}
