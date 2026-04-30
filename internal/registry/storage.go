// Package registry implements an OCI Distribution Spec registry backed by
// pluggable blob storage. Auth uses CI job OIDC tokens.
package registry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"cloud.google.com/go/storage"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	gcrregistry "github.com/google/go-containerregistry/pkg/registry"
)

// BlobHandler handles blob storage for the OCI registry. It is a superset of
// go-containerregistry's BlobHandler, BlobStatHandler, BlobPutHandler and
// BlobDeleteHandler interfaces, giving full control over blob persistence.
type BlobHandler interface {
	// Get retrieves the blob contents, or errNotFound if absent.
	Get(ctx context.Context, repo string, h v1.Hash) (io.ReadCloser, error)
	// Stat returns the size of the blob, or errNotFound if absent.
	Stat(ctx context.Context, repo string, h v1.Hash) (int64, error)
	// Put stores the blob contents.
	Put(ctx context.Context, repo string, h v1.Hash, rc io.ReadCloser) error
	// Delete removes the blob.
	Delete(ctx context.Context, repo string, h v1.Hash) error
}

// errNotFound is returned when a blob does not exist in storage.
var errNotFound = errors.New("not found")

// MemoryHandler is an in-memory BlobHandler backed by
// go-containerregistry's default in-memory implementation.
type MemoryHandler struct {
	inner gcrregistry.BlobHandler
}

// NewMemoryHandler returns a MemoryHandler wrapping go-containerregistry's
// default in-memory blob storage.
func NewMemoryHandler() *MemoryHandler {
	return &MemoryHandler{inner: gcrregistry.NewInMemoryBlobHandler()}
}

func (m *MemoryHandler) Get(ctx context.Context, repo string, h v1.Hash) (io.ReadCloser, error) {
	return m.inner.Get(ctx, repo, h)
}

func (m *MemoryHandler) Stat(ctx context.Context, repo string, h v1.Hash) (int64, error) {
	// go-containerregistry's memHandler implements BlobStatHandler.
	type statHandler interface {
		Stat(ctx context.Context, repo string, h v1.Hash) (int64, error)
	}
	if sh, ok := m.inner.(statHandler); ok {
		return sh.Stat(ctx, repo, h)
	}
	// Fallback: read the blob to get size.
	rc, err := m.inner.Get(ctx, repo, h)
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	n, err := io.Copy(io.Discard, rc)
	return n, err
}

func (m *MemoryHandler) Put(ctx context.Context, repo string, h v1.Hash, rc io.ReadCloser) error {
	type putHandler interface {
		Put(ctx context.Context, repo string, h v1.Hash, rc io.ReadCloser) error
	}
	if ph, ok := m.inner.(putHandler); ok {
		return ph.Put(ctx, repo, h, rc)
	}
	return fmt.Errorf("in-memory handler does not support Put")
}

func (m *MemoryHandler) Delete(ctx context.Context, repo string, h v1.Hash) error {
	type deleteHandler interface {
		Delete(ctx context.Context, repo string, h v1.Hash) error
	}
	if dh, ok := m.inner.(deleteHandler); ok {
		return dh.Delete(ctx, repo, h)
	}
	return nil // no-op for in-memory
}

// GCSHandler implements BlobHandler backed by Google Cloud Storage.
// Blobs are stored as objects keyed by <org>/<repo>/blobs/<digest>.
// Note: manifest storage uses go-containerregistry's default in-memory store;
// blob storage is durable in GCS.
type GCSHandler struct {
	bucket *storage.BucketHandle
	mu     sync.Mutex
	cache  map[string]int64 // digest → cached size from GCS
}

// NewGCSHandler returns a GCSHandler that stores blobs in the given GCS bucket.
func NewGCSHandler(bucket *storage.BucketHandle) *GCSHandler {
	return &GCSHandler{
		bucket: bucket,
		cache:  make(map[string]int64),
	}
}

// gcsKey returns the GCS object key for a blob in a repo.
func gcsKey(repo string, h v1.Hash) string {
	return repo + "/blobs/" + h.Algorithm + ":" + h.Hex
}

func (g *GCSHandler) Get(ctx context.Context, repo string, h v1.Hash) (io.ReadCloser, error) {
	obj := g.bucket.Object(gcsKey(repo, h))
	rc, err := obj.NewReader(ctx)
	if err != nil {
		if isGCSNotFound(err) {
			return nil, errNotFound
		}
		return nil, fmt.Errorf("gcs get %s: %w", h, err)
	}
	return rc, nil
}

func (g *GCSHandler) Stat(ctx context.Context, repo string, h v1.Hash) (int64, error) {
	key := gcsKey(repo, h)

	g.mu.Lock()
	if sz, ok := g.cache[key]; ok {
		g.mu.Unlock()
		return sz, nil
	}
	g.mu.Unlock()

	attrs, err := g.bucket.Object(key).Attrs(ctx)
	if err != nil {
		if isGCSNotFound(err) {
			return 0, errNotFound
		}
		return 0, fmt.Errorf("gcs stat %s: %w", h, err)
	}

	g.mu.Lock()
	g.cache[key] = attrs.Size
	g.mu.Unlock()

	return attrs.Size, nil
}

func (g *GCSHandler) Put(ctx context.Context, repo string, h v1.Hash, rc io.ReadCloser) error {
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return fmt.Errorf("read blob: %w", err)
	}

	key := gcsKey(repo, h)
	w := g.bucket.Object(key).NewWriter(ctx)
	if _, err := io.Copy(w, bytes.NewReader(data)); err != nil {
		_ = w.Close()
		return fmt.Errorf("gcs put %s: write: %w", h, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("gcs put %s: close: %w", h, err)
	}

	g.mu.Lock()
	g.cache[key] = int64(len(data))
	g.mu.Unlock()

	return nil
}

func (g *GCSHandler) Delete(ctx context.Context, repo string, h v1.Hash) error {
	key := gcsKey(repo, h)
	if err := g.bucket.Object(key).Delete(ctx); err != nil {
		if isGCSNotFound(err) {
			return nil
		}
		return fmt.Errorf("gcs delete %s: %w", h, err)
	}

	g.mu.Lock()
	delete(g.cache, key)
	g.mu.Unlock()

	return nil
}

// isGCSNotFound reports whether err is a GCS "not found" error.
func isGCSNotFound(err error) bool {
	return errors.Is(err, storage.ErrObjectNotExist)
}
