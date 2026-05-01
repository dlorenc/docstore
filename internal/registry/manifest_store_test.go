package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/dlorenc/docstore/internal/citoken"
)

// memManifestStore is an in-memory ManifestStore for use in tests.
type memManifestStore struct {
	mu   sync.RWMutex
	data map[string]struct {
		blob []byte
		ct   string
	}
}

func newMemManifestStore() *memManifestStore {
	return &memManifestStore{
		data: make(map[string]struct {
			blob []byte
			ct   string
		}),
	}
}

func (m *memManifestStore) storeKey(repo, ref string) string { return repo + "\x00" + ref }

func (m *memManifestStore) Get(_ context.Context, repo, ref string) ([]byte, string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.data[m.storeKey(repo, ref)]
	if !ok {
		return nil, "", errNotFound
	}
	return v.blob, v.ct, nil
}

func (m *memManifestStore) Put(_ context.Context, repo, ref string, data []byte, ct string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[m.storeKey(repo, ref)] = struct {
		blob []byte
		ct   string
	}{blob: data, ct: ct}
	return nil
}

func (m *memManifestStore) Delete(_ context.Context, repo, ref string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := m.storeKey(repo, ref)
	if _, ok := m.data[k]; !ok {
		return errNotFound
	}
	delete(m.data, k)
	return nil
}

func (m *memManifestStore) Tags(_ context.Context, repo string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	prefix := repo + "\x00"
	var tags []string
	for k := range m.data {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		ref := strings.TrimPrefix(k, prefix)
		if strings.HasPrefix(ref, "sha256:") {
			continue
		}
		tags = append(tags, ref)
	}
	sort.Strings(tags)
	return tags, nil
}

func (m *memManifestStore) Repos(_ context.Context) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	seen := make(map[string]bool)
	var repos []string
	for k := range m.data {
		parts := strings.SplitN(k, "\x00", 2)
		if len(parts) != 2 {
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

// makeRegistryWithStore creates a test registry with a given manifest store and
// a fresh LocalSigner. Returns the server, signer, and JWKS server for token issuance.
func makeRegistryWithStore(t *testing.T, store ManifestStore) (*httptest.Server, *citoken.LocalSigner) {
	t.Helper()
	signer, err := citoken.NewLocalSigner()
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	jwksSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := signer.PublicKeys(r.Context())
		w.Header().Set("Content-Type", "application/json")
		w.Write(data) //nolint:errcheck
	}))
	t.Cleanup(jwksSrv.Close)

	h := New(NewMemoryHandler(), store, jwksSrv.URL, "ci-registry", "https://oidc.test")
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, signer
}

func TestManifestMiddleware_PutGet(t *testing.T) {
	store := newMemManifestStore()
	srv, signer := makeRegistryWithStore(t, store)
	tok := makeTestToken(t, signer, "acme/cache")

	manifestBody := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[]}`

	// PUT manifest.
	req, _ := http.NewRequestWithContext(context.Background(),
		http.MethodPut,
		srv.URL+"/v2/acme/cache/manifests/latest",
		strings.NewReader(manifestBody))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("put manifest: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 on PUT, got %d", resp.StatusCode)
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	if digest == "" {
		t.Fatal("expected Docker-Content-Digest header")
	}

	// GET manifest by tag.
	req, _ = http.NewRequestWithContext(context.Background(),
		http.MethodGet,
		srv.URL+"/v2/acme/cache/manifests/latest", nil)
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get manifest: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on GET, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != manifestBody {
		t.Errorf("body mismatch: got %q want %q", body, manifestBody)
	}
	if resp.Header.Get("Docker-Content-Digest") != digest {
		t.Errorf("digest mismatch: got %q want %q", resp.Header.Get("Docker-Content-Digest"), digest)
	}

	// GET manifest by digest.
	req, _ = http.NewRequestWithContext(context.Background(),
		http.MethodGet,
		srv.URL+"/v2/acme/cache/manifests/"+digest, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get by digest: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 GET by digest, got %d", resp.StatusCode)
	}
}

func TestManifestMiddleware_HeadNotFound(t *testing.T) {
	store := newMemManifestStore()
	srv, signer := makeRegistryWithStore(t, store)
	tok := makeTestToken(t, signer, "acme/cache")

	req, _ := http.NewRequestWithContext(context.Background(),
		http.MethodHead,
		srv.URL+"/v2/acme/cache/manifests/latest", nil)
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestManifestMiddleware_Delete(t *testing.T) {
	store := newMemManifestStore()
	srv, signer := makeRegistryWithStore(t, store)
	tok := makeTestToken(t, signer, "acme/cache")

	// Seed a manifest directly in store.
	store.Put(context.Background(), "acme/cache", "v1", []byte(`{"test":1}`), "application/json") //nolint:errcheck

	// DELETE it.
	req, _ := http.NewRequestWithContext(context.Background(),
		http.MethodDelete,
		srv.URL+"/v2/acme/cache/manifests/v1", nil)
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("expected 202, got %d", resp.StatusCode)
	}

	// Verify it's gone.
	_, _, err = store.Get(context.Background(), "acme/cache", "v1")
	if !errors.Is(err, errNotFound) {
		t.Errorf("expected errNotFound after delete, got %v", err)
	}
}

func TestManifestMiddleware_TagsList(t *testing.T) {
	store := newMemManifestStore()
	srv, signer := makeRegistryWithStore(t, store)
	tok := makeTestToken(t, signer, "acme/cache")

	// Seed some tags.
	ctx := context.Background()
	for _, tag := range []string{"v1", "v2", "latest"} {
		store.Put(ctx, "acme/cache", tag, []byte(fmt.Sprintf(`{"tag":%q}`, tag)), "application/json") //nolint:errcheck
	}
	// Also add a digest key — should not appear in tags list.
	store.Put(ctx, "acme/cache", "sha256:abc123", []byte(`{}`), "application/json") //nolint:errcheck

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/v2/acme/cache/tags/list", nil)
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("tags list: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}

	wantTags := []string{"latest", "v1", "v2"}
	sort.Strings(result.Tags)
	if fmt.Sprint(result.Tags) != fmt.Sprint(wantTags) {
		t.Errorf("tags = %v, want %v", result.Tags, wantTags)
	}
}

// TestManifestMiddleware_SharedState verifies that two handler instances backed
// by the same ManifestStore share manifest state — simulating the multi-replica
// scenario where a manifest pushed to one pod must be visible from another.
func TestManifestMiddleware_SharedState(t *testing.T) {
	signer, err := citoken.NewLocalSigner()
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	jwksSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := signer.PublicKeys(r.Context())
		w.Header().Set("Content-Type", "application/json")
		w.Write(data) //nolint:errcheck
	}))
	defer jwksSrv.Close()

	sharedStore := newMemManifestStore()

	h1 := New(NewMemoryHandler(), sharedStore, jwksSrv.URL, "ci-registry", "https://oidc.test")
	h2 := New(NewMemoryHandler(), sharedStore, jwksSrv.URL, "ci-registry", "https://oidc.test")
	srv1 := httptest.NewServer(h1)
	srv2 := httptest.NewServer(h2)
	defer srv1.Close()
	defer srv2.Close()

	tok := makeTestToken(t, signer, "acme/cache")
	manifestBody := `{"schemaVersion":2}`

	// Push to replica 1.
	req, _ := http.NewRequestWithContext(context.Background(),
		http.MethodPut,
		srv1.URL+"/v2/acme/cache/manifests/latest",
		strings.NewReader(manifestBody))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	// Pull from replica 2.
	req, _ = http.NewRequestWithContext(context.Background(),
		http.MethodGet,
		srv2.URL+"/v2/acme/cache/manifests/latest", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replica 2 cannot see manifest from replica 1: got %d", resp.StatusCode)
	}
}
