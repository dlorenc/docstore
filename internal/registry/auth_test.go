package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dlorenc/docstore/internal/citoken"
	"github.com/dlorenc/docstore/internal/server"
)

// makeTestToken issues a signed job OIDC token for the given repo using a
// LocalSigner whose public keys are served by the returned JWKS handler.
func makeTestToken(t *testing.T, signer *citoken.LocalSigner, repo string) string {
	t.Helper()
	claims := citoken.JobClaims{
		Issuer:   "https://oidc.test",
		Subject:  "repo:" + repo + ":branch:main:check:test",
		Audience: "ci-registry",
		Repo:     repo,
		Branch:   "main",
		JobID:    "test-job-1",
	}
	tok, err := citoken.IssueJWT(context.Background(), signer, claims)
	if err != nil {
		t.Fatalf("issue test token: %v", err)
	}
	return tok
}

// makeValidator builds a token validator backed by a LocalSigner, serving its
// JWKS at a test HTTP server.
func makeValidator(t *testing.T, signer *citoken.LocalSigner) func(string) (*server.JobIdentity, error) {
	t.Helper()
	jwksServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := signer.PublicKeys(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data) //nolint:errcheck
	}))
	t.Cleanup(jwksServer.Close)
	return server.NewJobTokenValidator(jwksServer.URL, "ci-registry", "https://oidc.test")
}

// okHandler records the last request it handled.
type okHandler struct {
	called bool
}

func (h *okHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.called = true
	w.WriteHeader(http.StatusOK)
}

func TestAuthMiddleware_ValidToken(t *testing.T) {
	signer, err := citoken.NewLocalSigner()
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	validate := makeValidator(t, signer)

	inner := &okHandler{}
	h := authMiddleware(inner, validate)

	tok := makeTestToken(t, signer, "acme/myrepo")
	req := httptest.NewRequest(http.MethodGet, "/v2/acme/myrepo/manifests/latest", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !inner.called {
		t.Error("expected inner handler to be called")
	}
}

func TestAuthMiddleware_WrongOrg(t *testing.T) {
	signer, err := citoken.NewLocalSigner()
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	validate := makeValidator(t, signer)

	inner := &okHandler{}
	h := authMiddleware(inner, validate)

	// Token is for org "acme"; request accesses "other/repo".
	tok := makeTestToken(t, signer, "acme/myrepo")
	req := httptest.NewRequest(http.MethodGet, "/v2/other/repo/manifests/latest", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", rec.Code)
	}
	if inner.called {
		t.Error("inner handler should not be called on forbidden request")
	}
}

func TestAuthMiddleware_SameOrgDifferentRepo(t *testing.T) {
	signer, err := citoken.NewLocalSigner()
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	validate := makeValidator(t, signer)

	inner := &okHandler{}
	h := authMiddleware(inner, validate)

	// Token is for acme/repo-a; request accesses acme/repo-b (same org, different repo).
	tok := makeTestToken(t, signer, "acme/repo-a")
	req := httptest.NewRequest(http.MethodGet, "/v2/acme/repo-b/manifests/latest", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for same-org cross-repo access, got %d", rec.Code)
	}
	if inner.called {
		t.Error("inner handler should not be called on cross-repo request")
	}
}

func TestAuthMiddleware_NoToken(t *testing.T) {
	validate := server.NewJobTokenValidator("http://unused", "ci-registry", "https://oidc.test")

	inner := &okHandler{}
	h := authMiddleware(inner, validate)

	req := httptest.NewRequest(http.MethodGet, "/v2/acme/myrepo/manifests/latest", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
	if inner.called {
		t.Error("inner handler should not be called without a token")
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Error("expected WWW-Authenticate header on 401")
	}
}

func TestAuthMiddleware_InvalidToken(t *testing.T) {
	signer, err := citoken.NewLocalSigner()
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	validate := makeValidator(t, signer)

	inner := &okHandler{}
	h := authMiddleware(inner, validate)

	req := httptest.NewRequest(http.MethodGet, "/v2/acme/myrepo/manifests/latest", nil)
	req.Header.Set("Authorization", "Bearer not.a.valid.token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestAuthMiddleware_PingEndpoint(t *testing.T) {
	// The /v2/ ping endpoint has no image name; auth still required.
	signer, err := citoken.NewLocalSigner()
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	validate := makeValidator(t, signer)

	inner := &okHandler{}
	h := authMiddleware(inner, validate)

	// No token — should still get 401.
	req := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for /v2/ without token, got %d", rec.Code)
	}
}

func TestImageNameFromPath(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/v2/acme/myrepo/blobs/sha256:abc123", "acme/myrepo"},
		{"/v2/acme/myrepo/manifests/latest", "acme/myrepo"},
		{"/v2/acme/myrepo/tags/list", "acme/myrepo"},
		{"/v2/acme/myrepo/blobs/uploads/", "acme/myrepo"},
		{"/v2/", ""},
		{"/v2", ""},
		{"/other/path", ""},
		{"/v2/_catalog", ""},
		{"/v2/_catalog?n=100", ""},
	}
	for _, tc := range tests {
		got := imageNameFromPath(tc.path)
		if got != tc.want {
			t.Errorf("imageNameFromPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}
