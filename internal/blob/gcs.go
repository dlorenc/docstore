package blob

import (
	"context"
	"errors"
	"fmt"
	"io"

	"cloud.google.com/go/storage"
)

// GCSBlobStore is a BlobStore backed by Google Cloud Storage.
type GCSBlobStore struct {
	client *storage.Client
	bucket string
}

// NewGCSBlobStore creates a GCSBlobStore for the given bucket.
// It uses Application Default Credentials for authentication.
func NewGCSBlobStore(ctx context.Context, bucket string) (*GCSBlobStore, error) {
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("create gcs client: %w", err)
	}
	return &GCSBlobStore{client: client, bucket: bucket}, nil
}

// Put writes the contents of r as object key in the bucket.
func (s *GCSBlobStore) Put(ctx context.Context, key string, r io.Reader) error {
	w := s.client.Bucket(s.bucket).Object(key).NewWriter(ctx)
	if _, err := io.Copy(w, r); err != nil {
		w.Close() //nolint:errcheck
		return fmt.Errorf("gcs upload: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("gcs close writer: %w", err)
	}
	return nil
}

// Get downloads object key from the bucket and returns it as a ReadCloser.
func (s *GCSBlobStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	r, err := s.client.Bucket(s.bucket).Object(key).NewReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcs download: %w", err)
	}
	return r, nil
}

// Exists reports whether object key exists in the bucket.
func (s *GCSBlobStore) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.client.Bucket(s.bucket).Object(key).Attrs(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("gcs attrs: %w", err)
	}
	return true, nil
}

// Delete removes object key from the bucket.
// It is not an error if the object does not exist.
func (s *GCSBlobStore) Delete(ctx context.Context, key string) error {
	err := s.client.Bucket(s.bucket).Object(key).Delete(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("gcs delete: %w", err)
	}
	return nil
}
