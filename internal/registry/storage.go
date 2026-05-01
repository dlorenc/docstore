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
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	gcrregistry "github.com/google/go-containerregistry/pkg/registry"
	"google.golang.org/api/iterator"
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
	rc, err := m.inner.Get(ctx, repo, h)
	if err != nil {
		// Normalize go-containerregistry's unexported errNotFound to ours so
		// callers can use errors.Is(err, errNotFound) reliably.
		return nil, errNotFound
	}
	return rc, nil
}

func (m *MemoryHandler) Stat(ctx context.Context, repo string, h v1.Hash) (int64, error) {
	// go-containerregistry's memHandler implements BlobStatHandler.
	type statHandler interface {
		Stat(ctx context.Context, repo string, h v1.Hash) (int64, error)
	}
	if sh, ok := m.inner.(statHandler); ok {
		size, err := sh.Stat(ctx, repo, h)
		if err != nil {
			// Normalize go-containerregistry's unexported errNotFound to ours.
			return 0, errNotFound
		}
		return size, nil
	}
	// Fallback: read the blob to get size.
	rc, err := m.inner.Get(ctx, repo, h)
	if err != nil {
		return 0, errNotFound
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

// normalizeRepo strips a trailing "/blobs" suffix from repo when present.
//
// go-containerregistry extracts the repo name differently depending on whether
// the request is a blob HEAD/GET or a blob upload (PUT):
//   - HEAD/GET /v2/{name}/blobs/{digest}   → repo = {name}
//   - PUT /v2/{name}/blobs/uploads/{uuid}  → repo = {name}/blobs
//
// Normalizing ensures all GCS keys use the same prefix regardless of which
// operation produced the repo string.
func normalizeRepo(repo string) string {
	return strings.TrimSuffix(repo, "/blobs")
}

// gcsKey returns the GCS object key for a blob in a repo.
func gcsKey(repo string, h v1.Hash) string {
	return normalizeRepo(repo) + "/blobs/" + h.Algorithm + ":" + h.Hex
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

// ManifestStore stores and retrieves OCI manifests by repository and reference
// (tag or content digest). Implementations must be safe for concurrent use.
type ManifestStore interface {
	// Get retrieves a manifest by repo and reference (tag or digest).
	// Returns content, mediaType, and errNotFound if absent.
	Get(ctx context.Context, repo, ref string) (content []byte, mediaType string, err error)
	// Put stores a manifest. When ref is a tag, the manifest is also stored
	// under its content digest so digest-based lookups work.
	Put(ctx context.Context, repo, ref, mediaType string, content []byte) error
	// Delete removes a manifest by repo and reference.
	Delete(ctx context.Context, repo, ref string) error
	// Tags returns all tag names (not digest references) for a repository,
	// sorted lexicographically.
	Tags(ctx context.Context, repo string) ([]string, error)
	// Repos returns all repository names that have at least one manifest,
	// sorted lexicographically.
	Repos(ctx context.Context) ([]string, error)
}

// isDigestRef reports whether a manifest reference is a content digest
// (e.g. "sha256:abc..."). Tags are plain strings without a colon.
func isDigestRef(ref string) bool {
	return strings.Contains(ref, ":")
}

// manifestEntry holds a stored manifest's bytes and declared media type.
type manifestEntry struct {
	content   []byte
	mediaType string
}

// MemoryManifestStore is a thread-safe in-memory ManifestStore. It is used as
// the nil-safe fallback when no persistent store is configured.
type MemoryManifestStore struct {
	mu      sync.RWMutex
	entries map[string]manifestEntry // key: repo + "\x00" + ref
}

// NewMemoryManifestStore returns an empty MemoryManifestStore.
func NewMemoryManifestStore() *MemoryManifestStore {
	return &MemoryManifestStore{entries: make(map[string]manifestEntry)}
}

func (m *MemoryManifestStore) entryKey(repo, ref string) string {
	return repo + "\x00" + ref
}

func (m *MemoryManifestStore) Get(ctx context.Context, repo, ref string) ([]byte, string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.entries[m.entryKey(repo, ref)]
	if !ok {
		return nil, "", errNotFound
	}
	cp := make([]byte, len(e.content))
	copy(cp, e.content)
	return cp, e.mediaType, nil
}

func (m *MemoryManifestStore) Put(ctx context.Context, repo, ref, mediaType string, content []byte) error {
	d := digest.FromBytes(content)
	cp := make([]byte, len(content))
	copy(cp, content)
	entry := manifestEntry{content: cp, mediaType: mediaType}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[m.entryKey(repo, ref)] = entry
	// Also index by digest so GET-by-digest works after a tag push.
	if !isDigestRef(ref) {
		m.entries[m.entryKey(repo, d.String())] = entry
	}
	return nil
}

func (m *MemoryManifestStore) Delete(ctx context.Context, repo, ref string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, m.entryKey(repo, ref))
	return nil
}

func (m *MemoryManifestStore) Tags(ctx context.Context, repo string) ([]string, error) {
	prefix := repo + "\x00"
	m.mu.RLock()
	defer m.mu.RUnlock()
	var tags []string
	for k := range m.entries {
		if strings.HasPrefix(k, prefix) {
			ref := k[len(prefix):]
			if !isDigestRef(ref) {
				tags = append(tags, ref)
			}
		}
	}
	sort.Strings(tags)
	return tags, nil
}

func (m *MemoryManifestStore) Repos(ctx context.Context) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	repoSet := make(map[string]struct{})
	for k := range m.entries {
		repo, _, _ := strings.Cut(k, "\x00")
		repoSet[repo] = struct{}{}
	}
	repos := make([]string, 0, len(repoSet))
	for r := range repoSet {
		repos = append(repos, r)
	}
	sort.Strings(repos)
	return repos, nil
}

// GCSManifestStore implements ManifestStore backed by Google Cloud Storage.
// Manifests are stored as objects at "<repo>/manifests/<ref>". The GCS object
// ContentType field carries the OCI media type.
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

func (g *GCSManifestStore) Get(ctx context.Context, repo, ref string) ([]byte, string, error) {
	obj := g.bucket.Object(gcsManifestKey(repo, ref))
	attrs, err := obj.Attrs(ctx)
	if err != nil {
		if isGCSNotFound(err) {
			return nil, "", errNotFound
		}
		return nil, "", fmt.Errorf("gcs manifest attrs %s@%s: %w", repo, ref, err)
	}
	rc, err := obj.NewReader(ctx)
	if err != nil {
		if isGCSNotFound(err) {
			return nil, "", errNotFound
		}
		return nil, "", fmt.Errorf("gcs manifest open %s@%s: %w", repo, ref, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, "", fmt.Errorf("gcs manifest read %s@%s: %w", repo, ref, err)
	}
	return data, attrs.ContentType, nil
}

func (g *GCSManifestStore) Put(ctx context.Context, repo, ref, mediaType string, content []byte) error {
	if err := g.writeObject(ctx, gcsManifestKey(repo, ref), mediaType, content); err != nil {
		return err
	}
	// Also index by content digest when ref is a tag.
	if !isDigestRef(ref) {
		d := digest.FromBytes(content)
		if err := g.writeObject(ctx, gcsManifestKey(repo, d.String()), mediaType, content); err != nil {
			return err
		}
	}
	return nil
}

func (g *GCSManifestStore) writeObject(ctx context.Context, key, contentType string, data []byte) error {
	wc := g.bucket.Object(key).NewWriter(ctx)
	wc.ContentType = contentType
	if _, err := wc.Write(data); err != nil {
		_ = wc.Close()
		return fmt.Errorf("gcs manifest write %s: %w", key, err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("gcs manifest close %s: %w", key, err)
	}
	return nil
}

func (g *GCSManifestStore) Delete(ctx context.Context, repo, ref string) error {
	err := g.bucket.Object(gcsManifestKey(repo, ref)).Delete(ctx)
	if err != nil && !isGCSNotFound(err) {
		return fmt.Errorf("gcs manifest delete %s@%s: %w", repo, ref, err)
	}
	return nil
}

func (g *GCSManifestStore) Tags(ctx context.Context, repo string) ([]string, error) {
	prefix := repo + "/manifests/"
	it := g.bucket.Objects(ctx, &storage.Query{Prefix: prefix})
	var tags []string
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("gcs manifest tags %s: %w", repo, err)
		}
		ref := strings.TrimPrefix(attrs.Name, prefix)
		if !isDigestRef(ref) {
			tags = append(tags, ref)
		}
	}
	sort.Strings(tags)
	return tags, nil
}

func (g *GCSManifestStore) Repos(ctx context.Context) ([]string, error) {
	it := g.bucket.Objects(ctx, &storage.Query{})
	repoSet := make(map[string]struct{})
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("gcs repos list: %w", err)
		}
		// Object keys: "<repo>/manifests/<ref>" or "<repo>/blobs/<digest>".
		parts := strings.SplitN(attrs.Name, "/manifests/", 2)
		if len(parts) == 2 {
			repoSet[parts[0]] = struct{}{}
		}
	}
	repos := make([]string, 0, len(repoSet))
	for r := range repoSet {
		repos = append(repos, r)
	}
	sort.Strings(repos)
	return repos, nil
}
