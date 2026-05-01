package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dlorenc/docstore/internal/citoken"
	digest "github.com/opencontainers/go-digest"
)

// TestMemoryManifestStore_GetPutDelete exercises the basic CRUD operations on
// the in-memory ManifestStore.
func TestMemoryManifestStore_GetPutDelete(t *testing.T) {
	store := NewMemoryManifestStore()
	ctx := context.Background()
	repo := "testorg/myrepo"
	content := []byte(`{"schemaVersion":2}`)
	mediaType := "application/vnd.oci.image.manifest.v1+json"

	// Get non-existent → errNotFound.
	if _, _, err := store.Get(ctx, repo, "latest"); err == nil {
		t.Fatal("expected error for missing manifest, got nil")
	}

	// Put with a tag.
	if err := store.Put(ctx, repo, "latest", mediaType, content); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Get by tag.
	got, mt, err := store.Get(ctx, repo, "latest")
	if err != nil {
		t.Fatalf("Get by tag: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("content mismatch: got %q, want %q", got, content)
	}
	if mt != mediaType {
		t.Errorf("mediaType mismatch: got %q, want %q", mt, mediaType)
	}

	// Get by digest (should also work since Put indexes by digest).
	d := digest.FromBytes(content)
	got2, _, err := store.Get(ctx, repo, d.String())
	if err != nil {
		t.Fatalf("Get by digest: %v", err)
	}
	if string(got2) != string(content) {
		t.Errorf("digest lookup content mismatch: got %q, want %q", got2, content)
	}

	// Delete by tag.
	if err := store.Delete(ctx, repo, "latest"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, _, err := store.Get(ctx, repo, "latest"); err == nil {
		t.Error("expected errNotFound after Delete")
	}
}

// TestMemoryManifestStore_Tags verifies tag listing excludes digest references.
func TestMemoryManifestStore_Tags(t *testing.T) {
	store := NewMemoryManifestStore()
	ctx := context.Background()
	repo := "org/repo"
	mediaType := "application/vnd.oci.image.manifest.v1+json"

	contents := map[string][]byte{
		"v1.0": []byte(`{"v":1}`),
		"v2.0": []byte(`{"v":2}`),
		"edge": []byte(`{"v":3}`),
	}
	for tag, c := range contents {
		if err := store.Put(ctx, repo, tag, mediaType, c); err != nil {
			t.Fatalf("Put %s: %v", tag, err)
		}
	}

	tags, err := store.Tags(ctx, repo)
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if len(tags) != 3 {
		t.Errorf("expected 3 tags, got %d: %v", len(tags), tags)
	}
	for _, tag := range tags {
		if strings.Contains(tag, ":") {
			t.Errorf("Tags returned a digest ref: %q", tag)
		}
	}
	// Verify lexicographic order.
	for i := 1; i < len(tags); i++ {
		if tags[i] < tags[i-1] {
			t.Errorf("tags not sorted: %v", tags)
		}
	}
}

// TestMemoryManifestStore_Repos verifies repo listing.
func TestMemoryManifestStore_Repos(t *testing.T) {
	store := NewMemoryManifestStore()
	ctx := context.Background()
	mediaType := "application/vnd.oci.image.manifest.v1+json"

	repos := []string{"acme/frontend", "acme/backend", "other/service"}
	for _, repo := range repos {
		if err := store.Put(ctx, repo, "latest", mediaType, []byte(`{}`)); err != nil {
			t.Fatalf("Put %s: %v", repo, err)
		}
	}

	got, err := store.Repos(ctx)
	if err != nil {
		t.Fatalf("Repos: %v", err)
	}
	if len(got) != len(repos) {
		t.Errorf("expected %d repos, got %d: %v", len(repos), len(got), got)
	}
}

// TestManifestMiddleware_PutGet exercises the manifestStoreMiddleware via
// HTTP, verifying PUT stores and GET retrieves manifests through the store.
func TestManifestMiddleware_PutGet(t *testing.T) {
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

	store := NewMemoryManifestStore()
	h := New(NewMemoryHandler(), store, jwksSrv.URL, "ci-registry", "https://oidc.test")
	srv := httptest.NewServer(h)
	defer srv.Close()

	tok := makeTestToken(t, signer, "testorg/myrepo")
	manifestBody := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`)
	mediaType := "application/vnd.oci.image.manifest.v1+json"

	// PUT manifest.
	putReq, _ := http.NewRequestWithContext(context.Background(),
		http.MethodPut, srv.URL+"/v2/testorg/myrepo/manifests/v1.0",
		strings.NewReader(string(manifestBody)))
	putReq.Header.Set("Authorization", "Bearer "+tok)
	putReq.Header.Set("Content-Type", mediaType)
	resp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatalf("PUT manifest: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("PUT: expected 201, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Docker-Content-Digest") == "" {
		t.Error("PUT: missing Docker-Content-Digest header")
	}

	// GET manifest by tag.
	getReq, _ := http.NewRequestWithContext(context.Background(),
		http.MethodGet, srv.URL+"/v2/testorg/myrepo/manifests/v1.0", nil)
	getReq.Header.Set("Authorization", "Bearer "+tok)
	resp2, err := http.DefaultClient.Do(getReq)
	if err != nil {
		t.Fatalf("GET manifest: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("GET: expected 200, got %d", resp2.StatusCode)
	}
	if resp2.Header.Get("Docker-Content-Digest") == "" {
		t.Error("GET: missing Docker-Content-Digest header")
	}

	// GET by digest should also work.
	d := digest.FromBytes(manifestBody)
	getDigestReq, _ := http.NewRequestWithContext(context.Background(),
		http.MethodGet, srv.URL+"/v2/testorg/myrepo/manifests/"+d.String(), nil)
	getDigestReq.Header.Set("Authorization", "Bearer "+tok)
	resp3, err := http.DefaultClient.Do(getDigestReq)
	if err != nil {
		t.Fatalf("GET by digest: %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Errorf("GET by digest: expected 200, got %d", resp3.StatusCode)
	}
}

// TestManifestMiddleware_TagsList verifies /v2/<repo>/tags/list returns stored tags.
func TestManifestMiddleware_TagsList(t *testing.T) {
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

	store := NewMemoryManifestStore()
	ctx := context.Background()
	_ = store.Put(ctx, "testorg/myrepo", "v1", "application/vnd.oci.image.manifest.v1+json", []byte(`{"a":1}`))
	_ = store.Put(ctx, "testorg/myrepo", "v2", "application/vnd.oci.image.manifest.v1+json", []byte(`{"a":2}`))

	h := New(NewMemoryHandler(), store, jwksSrv.URL, "ci-registry", "https://oidc.test")
	srv := httptest.NewServer(h)
	defer srv.Close()

	tok := makeTestToken(t, signer, "testorg/myrepo")
	req, _ := http.NewRequestWithContext(context.Background(),
		http.MethodGet, srv.URL+"/v2/testorg/myrepo/tags/list", nil)
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("tags/list: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("tags/list: expected 200, got %d", resp.StatusCode)
	}

	var result struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode tags/list: %v", err)
	}
	if len(result.Tags) != 2 {
		t.Errorf("expected 2 tags, got %d: %v", len(result.Tags), result.Tags)
	}
}

// TestManifestMiddleware_NotFound verifies OCI-compliant 404 for unknown manifests.
func TestManifestMiddleware_NotFound(t *testing.T) {
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

	store := NewMemoryManifestStore()
	h := New(NewMemoryHandler(), store, jwksSrv.URL, "ci-registry", "https://oidc.test")
	srv := httptest.NewServer(h)
	defer srv.Close()

	tok := makeTestToken(t, signer, "testorg/myrepo")
	req, _ := http.NewRequestWithContext(context.Background(),
		http.MethodGet, srv.URL+"/v2/testorg/myrepo/manifests/nonexistent", nil)
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET missing manifest: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for missing manifest, got %d", resp.StatusCode)
	}
}

// TestManifestMiddleware_Catalog verifies /v2/_catalog returns stored repos.
func TestManifestMiddleware_Catalog(t *testing.T) {
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

	store := NewMemoryManifestStore()
	ctx := context.Background()
	_ = store.Put(ctx, "acme/app", "latest", "application/vnd.oci.image.manifest.v1+json", []byte(`{}`))
	_ = store.Put(ctx, "acme/cache", "main", "application/vnd.oci.image.manifest.v1+json", []byte(`{}`))

	h := New(NewMemoryHandler(), store, jwksSrv.URL, "ci-registry", "https://oidc.test")
	srv := httptest.NewServer(h)
	defer srv.Close()

	// _catalog endpoint uses /v2/ ping path logic, any valid token passes org check.
	tok := makeTestToken(t, signer, "acme/app")
	req, _ := http.NewRequestWithContext(context.Background(),
		http.MethodGet, srv.URL+"/v2/_catalog", nil)
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("_catalog: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("_catalog: expected 200, got %d", resp.StatusCode)
	}

	var result struct {
		Repos []string `json:"repositories"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode _catalog: %v", err)
	}
	if len(result.Repos) != 2 {
		t.Errorf("expected 2 repos, got %d: %v", len(result.Repos), result.Repos)
	}
}
