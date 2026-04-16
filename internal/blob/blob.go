// Package blob provides the BlobStore interface and its implementations for
// storing large file content outside of PostgreSQL.
package blob

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

// BlobStore stores and retrieves arbitrary blobs by key.
type BlobStore interface {
	Put(ctx context.Context, key string, r io.Reader) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Exists(ctx context.Context, key string) (bool, error)
	Delete(ctx context.Context, key string) error
}

// LocalBlobStore is a BlobStore backed by the local filesystem.
// It is intended for development and testing; do not use in production.
type LocalBlobStore struct {
	dir string
}

// NewLocalBlobStore creates a LocalBlobStore rooted at dir.
// The directory is created if it does not exist.
func NewLocalBlobStore(dir string) (*LocalBlobStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create blob dir: %w", err)
	}
	return &LocalBlobStore{dir: dir}, nil
}

func (s *LocalBlobStore) keyPath(key string) string {
	return filepath.Join(s.dir, key)
}

// Put writes r to the local filesystem under key.
func (s *LocalBlobStore) Put(_ context.Context, key string, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		slog.Error("blob put failed", "key", key, "backend", "local", "error", err)
		return fmt.Errorf("read blob data: %w", err)
	}
	if err := os.WriteFile(s.keyPath(key), data, 0o644); err != nil {
		slog.Error("blob put failed", "key", key, "backend", "local", "error", err)
		return fmt.Errorf("write blob: %w", err)
	}
	slog.Debug("blob put", "key", key, "backend", "local")
	return nil
}

// Get opens the file for key and returns it as a ReadCloser.
func (s *LocalBlobStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	f, err := os.Open(s.keyPath(key))
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Error("blob get failed", "key", key, "backend", "local", "error", err)
		}
		return nil, fmt.Errorf("open blob: %w", err)
	}
	return f, nil
}

// Exists reports whether a blob with the given key exists.
func (s *LocalBlobStore) Exists(_ context.Context, key string) (bool, error) {
	_, err := os.Stat(s.keyPath(key))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Delete removes the blob for key. It is not an error if the blob does not exist.
func (s *LocalBlobStore) Delete(_ context.Context, key string) error {
	err := os.Remove(s.keyPath(key))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		slog.Error("blob delete failed", "key", key, "backend", "local", "error", err)
		return err
	}
	slog.Debug("blob delete", "key", key, "backend", "local")
	return nil
}
