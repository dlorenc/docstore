package registry

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/dlorenc/docstore/internal/citoken"
)

// hashString computes a v1.Hash for the given string content (sha256).
func hashString(s string) v1.Hash {
	sum := sha256.Sum256([]byte(s))
	return v1.Hash{
		Algorithm: "sha256",
		Hex:       fmt.Sprintf("%x", sum),
	}
}

func TestRegistry_PingRequiresAuth(t *testing.T) {
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

	h := New(NewMemoryHandler(), jwksSrv.URL, "ci-registry", "https://oidc.test")

	req := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthenticated ping, got %d", rec.Code)
	}
}

func TestRegistry_PushPullBlob(t *testing.T) {
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

	h := New(NewMemoryHandler(), jwksSrv.URL, "ci-registry", "https://oidc.test")
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Issue token for org "testorg".
	tok := makeTestToken(t, signer, "testorg/myrepo")

	// Initiate blob upload — expect 202 Accepted with Location header.
	uploadReq, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
		srv.URL+"/v2/testorg/myrepo/blobs/uploads/", nil)
	uploadReq.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(uploadReq)
	if err != nil {
		t.Fatalf("initiate upload: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("expected 202 for blob upload initiation, got %d", resp.StatusCode)
	}
}

func TestRegistry_OrgIsolation(t *testing.T) {
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

	h := New(NewMemoryHandler(), jwksSrv.URL, "ci-registry", "https://oidc.test")
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Token is for "acme/myrepo"; try to access "other/repo".
	tok := makeTestToken(t, signer, "acme/myrepo")
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
		srv.URL+"/v2/other/repo/manifests/latest", nil)
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for cross-org access, got %d", resp.StatusCode)
	}
}

// TestMemoryHandler tests basic in-memory blob storage operations.
func TestMemoryHandler(t *testing.T) {
	h := NewMemoryHandler()
	ctx := context.Background()
	repo := "testorg/repo"
	content := "hello world"
	hash := hashString(content)

	// Get a non-existent blob — should fail.
	if _, err := h.Get(ctx, repo, hash); err == nil {
		t.Error("expected error getting non-existent blob")
	}

	// Put and then Get.
	if err := h.Put(ctx, repo, hash, io.NopCloser(strings.NewReader(content))); err != nil {
		t.Fatalf("put: %v", err)
	}

	rc, err := h.Get(ctx, repo, hash)
	if err != nil {
		t.Fatalf("get after put: %v", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != content {
		t.Errorf("expected %q, got %q", content, string(data))
	}

	// Stat should return the correct size.
	size, err := h.Stat(ctx, repo, hash)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if size != int64(len(content)) {
		t.Errorf("expected size %d, got %d", len(content), size)
	}
}
