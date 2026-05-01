// Package registry implements an OCI Distribution Spec registry backed by
// pluggable blob storage. Auth uses CI job OIDC tokens.
package registry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
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

// ManifestStore handles durable manifest storage for the OCI registry.
// Implementations must be safe for concurrent use.
type ManifestStore interface {
	// Get retrieves the manifest bytes and content-type for the given repo+ref.
	// Returns errNotFound if absent.
	Get(ctx context.Context, repo, ref string) (data []byte, contentType string, err error)
	// Put stores the manifest bytes and content-type for the given repo+ref.
	Put(ctx context.Context, repo, ref string, data []byte, contentType string) error
	// Delete removes the manifest for the given repo+ref.
	Delete(ctx context.Context, repo, ref string) error
	// Tags returns the non-digest tag names for the given repo.
	Tags(ctx context.Context, repo string) ([]string, error)
	// Repos returns the names of all repositories that have at least one manifest.
	Repos(ctx context.Context) ([]string, error)
}

// GCSManifestStore implements ManifestStore backed by Google Cloud Storage.
// Manifests are stored as GCS objects keyed by <repo>/manifests/<ref>.
// The OCI content-type is stored as the GCS object's ContentType.
type GCSManifestStore struct {
	bucket *storage.BucketHandle
}

// NewGCSManifestStore returns a GCSManifestStore that stores manifests in the
// given GCS bucket.
func NewGCSManifestStore(bucket *storage.BucketHandle) *GCSManifestStore {
	return &GCSManifestStore{bucket: bucket}
}

func gcsManifestKey(repo, ref string) string {
	return repo + "/manifests/" + ref
}

func (s *GCSManifestStore) Get(ctx context.Context, repo, ref string) ([]byte, string, error) {
	obj := s.bucket.Object(gcsManifestKey(repo, ref))
	rc, err := obj.NewReader(ctx)
	if err != nil {
		if isGCSNotFound(err) {
			return nil, "", errNotFound
		}
		return nil, "", fmt.Errorf("gcs manifest get %s@%s: %w", repo, ref, err)
	}
	defer rc.Close()
	contentType := rc.Attrs.ContentType
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, "", fmt.Errorf("gcs manifest read %s@%s: %w", repo, ref, err)
	}
	return data, contentType, nil
}

func (s *GCSManifestStore) Put(ctx context.Context, repo, ref string, data []byte, contentType string) error {
	key := gcsManifestKey(repo, ref)
	w := s.bucket.Object(key).NewWriter(ctx)
	w.ObjectAttrs.ContentType = contentType
	if _, err := io.Copy(w, bytes.NewReader(data)); err != nil {
		_ = w.Close()
		return fmt.Errorf("gcs manifest put %s@%s: write: %w", repo, ref, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("gcs manifest put %s@%s: close: %w", repo, ref, err)
	}
	return nil
}

func (s *GCSManifestStore) Delete(ctx context.Context, repo, ref string) error {
	key := gcsManifestKey(repo, ref)
	if err := s.bucket.Object(key).Delete(ctx); err != nil {
		if isGCSNotFound(err) {
			return errNotFound
		}
		return fmt.Errorf("gcs manifest delete %s@%s: %w", repo, ref, err)
	}
	return nil
}

func (s *GCSManifestStore) Tags(ctx context.Context, repo string) ([]string, error) {
	prefix := repo + "/manifests/"
	it := s.bucket.Objects(ctx, &storage.Query{Prefix: prefix})
	var tags []string
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("gcs list tags %s: %w", repo, err)
		}
		ref := strings.TrimPrefix(attrs.Name, prefix)
		if ref == "" || strings.HasPrefix(ref, "sha256:") {
			continue
		}
		tags = append(tags, ref)
	}
	sort.Strings(tags)
	return tags, nil
}

func (s *GCSManifestStore) Repos(ctx context.Context) ([]string, error) {
	it := s.bucket.Objects(ctx, &storage.Query{})
	seen := make(map[string]bool)
	var repos []string
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("gcs list repos: %w", err)
		}
		// Object key format: <repo>/manifests/<ref>
		parts := strings.SplitN(attrs.Name, "/manifests/", 2)
		if len(parts) != 2 || parts[1] == "" {
			continue
		}
		repo := parts[0]
		if !seen[repo] {
			seen[repo] = true
			repos = append(repos, repo)
		}
	}
	sort.Strings(repos)
	return repos, nil
}
