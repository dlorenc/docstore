package logstore_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dlorenc/docstore/internal/logstore"
)

func TestLocalLogStore_Write(t *testing.T) {
	dir := t.TempDir()
	ls, err := logstore.NewLocalLogStore(dir)
	if err != nil {
		t.Fatalf("NewLocalLogStore: %v", err)
	}

	ctx := context.Background()
	const logs = "build output here"
	url, err := ls.Write(ctx, "myorg/myrepo", "feature/x", 42, "ci/build", logs)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// key = "myorg/myrepo/feature/x/42/ci_build.log"
	expectedPath := filepath.Join(dir, filepath.FromSlash("myorg/myrepo/feature/x/42/ci_build.log"))
	expectedURL := "file://" + expectedPath
	if url != expectedURL {
		t.Errorf("url = %q, want %q", url, expectedURL)
	}

	content, err := os.ReadFile(expectedPath)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if string(content) != logs {
		t.Errorf("content = %q, want %q", string(content), logs)
	}
}

func TestLocalLogStore_Write_SlashInCheckName(t *testing.T) {
	dir := t.TempDir()
	ls, err := logstore.NewLocalLogStore(dir)
	if err != nil {
		t.Fatalf("NewLocalLogStore: %v", err)
	}

	ctx := context.Background()
	url, err := ls.Write(ctx, "org/repo", "main", 1, "ci/lint/strict", "lint output")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Slashes in check name replaced with underscores: ci_lint_strict.log
	expectedPath := filepath.Join(dir, filepath.FromSlash("org/repo/main/1/ci_lint_strict.log"))
	expectedURL := "file://" + expectedPath
	if url != expectedURL {
		t.Errorf("url = %q, want %q", url, expectedURL)
	}

	content, err := os.ReadFile(expectedPath)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if string(content) != "lint output" {
		t.Errorf("content = %q, want %q", string(content), "lint output")
	}
}

func TestLocalLogStore_Write_OverwritesPreviousLog(t *testing.T) {
	dir := t.TempDir()
	ls, err := logstore.NewLocalLogStore(dir)
	if err != nil {
		t.Fatalf("NewLocalLogStore: %v", err)
	}

	ctx := context.Background()
	if _, err := ls.Write(ctx, "o/r", "b", 1, "ci/test", "first"); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	if _, err := ls.Write(ctx, "o/r", "b", 1, "ci/test", "second"); err != nil {
		t.Fatalf("second Write: %v", err)
	}

	path := filepath.Join(dir, filepath.FromSlash("o/r/b/1/ci_test.log"))
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(content) != "second" {
		t.Errorf("content = %q, want %q", string(content), "second")
	}
}

func TestNewFromEnv_Local(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LOG_STORE", "local")
	t.Setenv("LOG_LOCAL_DIR", dir)

	ls, err := logstore.NewFromEnv(context.Background())
	if err != nil {
		t.Fatalf("NewFromEnv: %v", err)
	}
	if ls == nil {
		t.Fatal("expected non-nil LogStore")
	}
}

func TestNewFromEnv_DefaultIsLocal(t *testing.T) {
	t.Setenv("LOG_STORE", "")
	t.Setenv("LOG_LOCAL_DIR", t.TempDir())

	ls, err := logstore.NewFromEnv(context.Background())
	if err != nil {
		t.Fatalf("NewFromEnv: %v", err)
	}
	if ls == nil {
		t.Fatal("expected non-nil LogStore")
	}
}

func TestNewFromEnv_GCS_MissingBucket(t *testing.T) {
	t.Setenv("LOG_STORE", "gcs")
	t.Setenv("LOG_BUCKET", "")

	_, err := logstore.NewFromEnv(context.Background())
	if err == nil {
		t.Fatal("expected error when LOG_BUCKET is not set")
	}
}

func TestNewFromEnv_Invalid(t *testing.T) {
	t.Setenv("LOG_STORE", "s3")

	_, err := logstore.NewFromEnv(context.Background())
	if err == nil {
		t.Fatal("expected error for unsupported LOG_STORE")
	}
}
