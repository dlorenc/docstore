// Package logstore provides the LogStore interface and implementations for
// storing CI build logs. Follows the same local/GCS abstraction as internal/blob.
package logstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"cloud.google.com/go/storage"
)

// LogStore writes build logs for a check run and returns a URL reference.
type LogStore interface {
	// Write stores the logs for the named check run and returns the URL
	// (e.g. "file:///tmp/ci-logs/..." or "gs://bucket/...").
	Write(ctx context.Context, repo, branch string, seq int64, checkName, logs string) (string, error)
}

// objectKey returns the log object/file key for the given parameters.
// Check names containing "/" are replaced with "_" so they are safe as path segments.
func objectKey(repo, branch string, seq int64, checkName string) string {
	safeName := strings.ReplaceAll(checkName, "/", "_")
	return fmt.Sprintf("%s/%s/%d/%s.log", repo, branch, seq, safeName)
}

// ---------------------------------------------------------------------------
// LocalLogStore
// ---------------------------------------------------------------------------

// LocalLogStore is a LogStore backed by the local filesystem.
// Intended for development and testing.
type LocalLogStore struct {
	dir string
}

// NewLocalLogStore creates a LocalLogStore rooted at dir.
// The directory is created if it does not exist.
func NewLocalLogStore(dir string) (*LocalLogStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	return &LocalLogStore{dir: dir}, nil
}

// Write writes logs to a file under dir and returns a file:// URL.
func (s *LocalLogStore) Write(_ context.Context, repo, branch string, seq int64, checkName, logs string) (string, error) {
	key := objectKey(repo, branch, seq, checkName)
	path := filepath.Join(s.dir, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create log subdir: %w", err)
	}
	if err := os.WriteFile(path, []byte(logs), 0o644); err != nil {
		return "", fmt.Errorf("write log: %w", err)
	}
	return "file://" + path, nil
}

// ---------------------------------------------------------------------------
// GCSLogStore
// ---------------------------------------------------------------------------

// GCSLogStore is a LogStore backed by Google Cloud Storage.
// Uses Application Default Credentials (workload identity on Cloud Run).
type GCSLogStore struct {
	client *storage.Client
	bucket string
}

// NewGCSLogStore creates a GCSLogStore for the given bucket.
func NewGCSLogStore(ctx context.Context, bucket string) (*GCSLogStore, error) {
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("create gcs client: %w", err)
	}
	return &GCSLogStore{client: client, bucket: bucket}, nil
}

// Write uploads logs to GCS and returns a gs:// URI.
func (s *GCSLogStore) Write(ctx context.Context, repo, branch string, seq int64, checkName, logs string) (string, error) {
	key := objectKey(repo, branch, seq, checkName)
	w := s.client.Bucket(s.bucket).Object(key).NewWriter(ctx)
	if _, err := w.Write([]byte(logs)); err != nil {
		w.Close() //nolint:errcheck
		return "", fmt.Errorf("gcs write: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("gcs close writer: %w", err)
	}
	return fmt.Sprintf("gs://%s/%s", s.bucket, key), nil
}

// ---------------------------------------------------------------------------
// Factory
// ---------------------------------------------------------------------------

// NewFromEnv creates a LogStore from environment variables:
//
//	LOG_STORE      "local" (default) or "gcs"
//	LOG_LOCAL_DIR  directory for local store (default /tmp/ci-logs)
//	LOG_BUCKET     GCS bucket name (required when LOG_STORE=gcs)
func NewFromEnv(ctx context.Context) (LogStore, error) {
	storeType := os.Getenv("LOG_STORE")
	if storeType == "" {
		storeType = "local"
	}
	switch storeType {
	case "local":
		dir := os.Getenv("LOG_LOCAL_DIR")
		if dir == "" {
			dir = "/tmp/ci-logs"
		}
		return NewLocalLogStore(dir)
	case "gcs":
		bucket := os.Getenv("LOG_BUCKET")
		if bucket == "" {
			return nil, fmt.Errorf("LOG_BUCKET is required when LOG_STORE=gcs")
		}
		return NewGCSLogStore(ctx, bucket)
	default:
		return nil, fmt.Errorf("unsupported LOG_STORE %q: must be 'local' or 'gcs'", storeType)
	}
}
