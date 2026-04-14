package server

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	dbpkg "github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/model"
)

// generateTestKey generates a 2048-bit RSA key pair for tests.
func generateTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

// makeTestJWT returns a signed RS256 JWT with the given claims.
func makeTestJWT(t *testing.T, key *rsa.PrivateKey, kid, email string, exp time.Time) string {
	t.Helper()

	headerJSON, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"})
	payloadJSON, _ := json.Marshal(map[string]interface{}{
		"email": email,
		"exp":   exp.Unix(),
		"iat":   time.Now().Unix(),
		"iss":   "https://cloud.google.com/iap",
	})

	headerB64 := base64.RawURLEncoding.EncodeToString(headerJSON)
	payloadB64 := base64.RawURLEncoding.EncodeToString(payloadJSON)
	signingInput := headerB64 + "." + payloadB64

	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// staticKeyFetcher returns a fetcher that serves a single known key.
func staticKeyFetcher(kid string, pub *rsa.PublicKey) func(string) (*rsa.PublicKey, error) {
	return func(requestedKid string) (*rsa.PublicKey, error) {
		if requestedKid != kid {
			return nil, fmt.Errorf("key %q not found", requestedKid)
		}
		return pub, nil
	}
}

// okHandler is a trivial downstream handler that records the identity it sees.
type identityCapture struct {
	identity string
}

func (c *identityCapture) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c.identity = IdentityFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}
}

func TestIAPMiddleware_ValidJWT(t *testing.T) {
	key := generateTestKey(t)
	kid := "test-key-1"
	mw := newMiddleware("", staticKeyFetcher(kid, &key.PublicKey))
	cap := &identityCapture{}
	handler := mw(cap.handler())

	token := makeTestJWT(t, key, kid, "alice@example.com", time.Now().Add(time.Hour))
	req := httptest.NewRequest(http.MethodGet, "/tree", nil)
	req.Header.Set("X-Goog-IAP-JWT-Assertion", token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestIAPMiddleware_MissingHeader(t *testing.T) {
	mw := newMiddleware("", func(kid string) (*rsa.PublicKey, error) {
		return nil, fmt.Errorf("should not be called")
	})
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/tree", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}

	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["error"] != "unauthenticated" {
		t.Errorf("expected error=unauthenticated, got %q", body["error"])
	}
}

func TestIAPMiddleware_InvalidSignature(t *testing.T) {
	signingKey := generateTestKey(t)
	verifyKey := generateTestKey(t) // different key — signature will not match
	kid := "test-key-1"

	mw := newMiddleware("", staticKeyFetcher(kid, &verifyKey.PublicKey))
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	token := makeTestJWT(t, signingKey, kid, "alice@example.com", time.Now().Add(time.Hour))
	req := httptest.NewRequest(http.MethodGet, "/tree", nil)
	req.Header.Set("X-Goog-IAP-JWT-Assertion", token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestIAPMiddleware_ExpiredToken(t *testing.T) {
	key := generateTestKey(t)
	kid := "test-key-1"
	mw := newMiddleware("", staticKeyFetcher(kid, &key.PublicKey))
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	token := makeTestJWT(t, key, kid, "alice@example.com", time.Now().Add(-time.Hour))
	req := httptest.NewRequest(http.MethodGet, "/tree", nil)
	req.Header.Set("X-Goog-IAP-JWT-Assertion", token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestIAPMiddleware_DevMode(t *testing.T) {
	// Dev mode: no key fetcher needed, no JWT header needed.
	mw := newMiddleware("alice@example.com", nil)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/tree", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestIAPMiddleware_IdentityInContext(t *testing.T) {
	key := generateTestKey(t)
	kid := "test-key-1"
	const email = "alice@example.com"

	mw := newMiddleware("", staticKeyFetcher(kid, &key.PublicKey))
	cap := &identityCapture{}
	handler := mw(cap.handler())

	token := makeTestJWT(t, key, kid, email, time.Now().Add(time.Hour))
	req := httptest.NewRequest(http.MethodGet, "/tree", nil)
	req.Header.Set("X-Goog-IAP-JWT-Assertion", token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if cap.identity != email {
		t.Errorf("expected identity %q in context, got %q", email, cap.identity)
	}
}

// ---------------------------------------------------------------------------
// RBAC middleware tests
// ---------------------------------------------------------------------------

// mockRoleStore is a test double for RoleStore.
type mockRoleStore struct {
	getRoleFn  func(ctx context.Context, repo, identity string) (*model.Role, error)
	hasAdminFn func(ctx context.Context, repo string) (bool, error)
}

func (m *mockRoleStore) GetRole(ctx context.Context, repo, identity string) (*model.Role, error) {
	if m.getRoleFn != nil {
		return m.getRoleFn(ctx, repo, identity)
	}
	return nil, dbpkg.ErrRoleNotFound
}

func (m *mockRoleStore) HasAdmin(ctx context.Context, repo string) (bool, error) {
	if m.hasAdminFn != nil {
		return m.hasAdminFn(ctx, repo)
	}
	return false, nil
}

// rbacTestServer creates a server with a fixed identity in context (simulating
// post-IAP) and the given role store + bootstrap admin. Returns the mux handler
// and a recorder factory.
func rbacTestServer(store RoleStore, bootstrapAdmin, identity string) http.Handler {
	inner := http.NewServeMux()
	inner.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// Inject identity into context (normally done by IAPMiddleware).
	identityInjector := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), identityKey, identity)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	return identityInjector(RBACMiddleware(store, bootstrapAdmin)(inner))
}

func rbacDo(t *testing.T, handler http.Handler, method, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reqBody *bytes.Reader
	if body != "" {
		reqBody = bytes.NewReader([]byte(body))
	} else {
		reqBody = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reqBody)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestRBACMiddleware_ReaderCanGet(t *testing.T) {
	store := &mockRoleStore{
		getRoleFn: func(_ context.Context, repo, identity string) (*model.Role, error) {
			return &model.Role{Identity: identity, Role: model.RoleReader}, nil
		},
	}
	h := rbacTestServer(store, "", "alice@example.com")
	rec := rbacDo(t, h, http.MethodGet, "/repos/myrepo/myrepo/-/tree", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestRBACMiddleware_ReaderCannotPost(t *testing.T) {
	store := &mockRoleStore{
		getRoleFn: func(_ context.Context, repo, identity string) (*model.Role, error) {
			return &model.Role{Identity: identity, Role: model.RoleReader}, nil
		},
	}
	h := rbacTestServer(store, "", "alice@example.com")
	rec := rbacDo(t, h, http.MethodPost, "/repos/myrepo/myrepo/-/commit", `{"branch":"feature/x","message":"m","files":[]}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestRBACMiddleware_WriterCanCommitToBranch(t *testing.T) {
	store := &mockRoleStore{
		getRoleFn: func(_ context.Context, repo, identity string) (*model.Role, error) {
			return &model.Role{Identity: identity, Role: model.RoleWriter}, nil
		},
	}
	h := rbacTestServer(store, "", "bob@example.com")
	rec := rbacDo(t, h, http.MethodPost, "/repos/myrepo/myrepo/-/commit", `{"branch":"feature/work","message":"m","files":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestRBACMiddleware_WriterCannotCommitToMain(t *testing.T) {
	store := &mockRoleStore{
		getRoleFn: func(_ context.Context, repo, identity string) (*model.Role, error) {
			return &model.Role{Identity: identity, Role: model.RoleWriter}, nil
		},
	}
	h := rbacTestServer(store, "", "bob@example.com")
	rec := rbacDo(t, h, http.MethodPost, "/repos/myrepo/myrepo/-/commit", `{"branch":"main","message":"m","files":[]}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestRBACMiddleware_WriterCannotMerge(t *testing.T) {
	store := &mockRoleStore{
		getRoleFn: func(_ context.Context, repo, identity string) (*model.Role, error) {
			return &model.Role{Identity: identity, Role: model.RoleWriter}, nil
		},
	}
	h := rbacTestServer(store, "", "bob@example.com")
	rec := rbacDo(t, h, http.MethodPost, "/repos/myrepo/myrepo/-/merge", `{"branch":"feature/x"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestRBACMiddleware_MaintainerCanMerge(t *testing.T) {
	store := &mockRoleStore{
		getRoleFn: func(_ context.Context, repo, identity string) (*model.Role, error) {
			return &model.Role{Identity: identity, Role: model.RoleMaintainer}, nil
		},
	}
	h := rbacTestServer(store, "", "carol@example.com")
	rec := rbacDo(t, h, http.MethodPost, "/repos/myrepo/myrepo/-/merge", `{"branch":"feature/x"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestRBACMiddleware_UnknownIdentity(t *testing.T) {
	store := &mockRoleStore{
		// getRoleFn nil → returns ErrRoleNotFound by default
	}
	h := rbacTestServer(store, "", "unknown@example.com")
	rec := rbacDo(t, h, http.MethodGet, "/repos/myrepo/myrepo/-/tree", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestRBACMiddleware_AdminCanManageRoles(t *testing.T) {
	store := &mockRoleStore{
		getRoleFn: func(_ context.Context, repo, identity string) (*model.Role, error) {
			return &model.Role{Identity: identity, Role: model.RoleAdmin}, nil
		},
	}
	h := rbacTestServer(store, "", "admin@example.com")

	// Admin can list roles.
	rec := rbacDo(t, h, http.MethodGet, "/repos/myrepo/myrepo/-/roles", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET roles: expected 200, got %d", rec.Code)
	}

	// Admin can PUT a role.
	rec = rbacDo(t, h, http.MethodPut, "/repos/myrepo/myrepo/-/roles/bob@example.com", `{"role":"writer"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT role: expected 200, got %d", rec.Code)
	}
}

func TestRBACMiddleware_BootstrapAdmin(t *testing.T) {
	const bootstrapAdmin = "bootstrap@example.com"
	store := &mockRoleStore{
		// No roles set up — hasAdminFn returns false by default.
	}
	h := rbacTestServer(store, bootstrapAdmin, bootstrapAdmin)

	// Bootstrap admin can access admin-only roles endpoint.
	rec := rbacDo(t, h, http.MethodGet, "/repos/newrepo/newrepo/-/roles", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("bootstrap admin GET roles: expected 200, got %d", rec.Code)
	}

	// Once an admin exists, bootstrap flag is ignored and the bootstrap
	// identity must have an explicit role.
	storeWithAdmin := &mockRoleStore{
		getRoleFn: func(_ context.Context, repo, identity string) (*model.Role, error) {
			return nil, dbpkg.ErrRoleNotFound
		},
		hasAdminFn: func(_ context.Context, repo string) (bool, error) {
			return true, nil // admin already exists
		},
	}
	h2 := rbacTestServer(storeWithAdmin, bootstrapAdmin, bootstrapAdmin)
	rec = rbacDo(t, h2, http.MethodGet, "/repos/newrepo/newrepo/-/roles", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bootstrap admin after admin exists: expected 403, got %d", rec.Code)
	}
}

func TestRBACMiddleware_RepoIsolation(t *testing.T) {
	// alice is admin in repo-a but has NO role in repo-b.
	store := &mockRoleStore{
		getRoleFn: func(_ context.Context, repo, identity string) (*model.Role, error) {
			if repo == "repo-a/repo-a" && identity == "alice@example.com" {
				return &model.Role{Identity: identity, Role: model.RoleAdmin}, nil
			}
			return nil, dbpkg.ErrRoleNotFound
		},
	}
	h := rbacTestServer(store, "", "alice@example.com")

	// Alice can access repo-a.
	rec := rbacDo(t, h, http.MethodGet, "/repos/repo-a/repo-a/-/tree", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("repo-a GET: expected 200, got %d", rec.Code)
	}

	// Alice has no access to repo-b.
	rec = rbacDo(t, h, http.MethodGet, "/repos/repo-b/repo-b/-/tree", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("repo-b GET: expected 403, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// JWT edge case tests
// ---------------------------------------------------------------------------

// buildJWT constructs a JWT with the given header and payload maps, signed
// with key. The helper exists to produce tokens with non-standard fields
// (wrong alg, missing kid, missing email, etc.) without going through makeTestJWT.
func buildJWT(t *testing.T, key *rsa.PrivateKey, header, payload map[string]interface{}) string {
	t.Helper()
	headerJSON, _ := json.Marshal(header)
	payloadJSON, _ := json.Marshal(payload)
	headerB64 := base64.RawURLEncoding.EncodeToString(headerJSON)
	payloadB64 := base64.RawURLEncoding.EncodeToString(payloadJSON)
	signingInput := headerB64 + "." + payloadB64
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestIAPMiddleware_MalformedToken(t *testing.T) {
	mw := newMiddleware("", func(kid string) (*rsa.PublicKey, error) {
		return nil, fmt.Errorf("should not be reached")
	})
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, token := range []string{"notajwt", "only.two"} {
		req := httptest.NewRequest(http.MethodGet, "/tree", nil)
		req.Header.Set("X-Goog-IAP-JWT-Assertion", token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("token=%q: expected 401, got %d", token, rec.Code)
		}
	}
}

func TestIAPMiddleware_WrongAlgorithm(t *testing.T) {
	key := generateTestKey(t)
	kid := "test-key-1"
	token := buildJWT(t, key,
		map[string]interface{}{"alg": "HS256", "kid": kid, "typ": "JWT"},
		map[string]interface{}{"email": "alice@example.com", "exp": time.Now().Add(time.Hour).Unix()},
	)

	mw := newMiddleware("", staticKeyFetcher(kid, &key.PublicKey))
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/tree", nil)
	req.Header.Set("X-Goog-IAP-JWT-Assertion", token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestIAPMiddleware_MissingKid(t *testing.T) {
	key := generateTestKey(t)
	token := buildJWT(t, key,
		map[string]interface{}{"alg": "RS256", "typ": "JWT"}, // no kid
		map[string]interface{}{"email": "alice@example.com", "exp": time.Now().Add(time.Hour).Unix()},
	)

	mw := newMiddleware("", func(kid string) (*rsa.PublicKey, error) {
		return nil, fmt.Errorf("should not be reached")
	})
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/tree", nil)
	req.Header.Set("X-Goog-IAP-JWT-Assertion", token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestIAPMiddleware_UnknownKid(t *testing.T) {
	key := generateTestKey(t)

	mw := newMiddleware("", func(kid string) (*rsa.PublicKey, error) {
		return nil, fmt.Errorf("key %q not found", kid)
	})
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	token := makeTestJWT(t, key, "unknown-kid", "alice@example.com", time.Now().Add(time.Hour))
	req := httptest.NewRequest(http.MethodGet, "/tree", nil)
	req.Header.Set("X-Goog-IAP-JWT-Assertion", token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestIAPMiddleware_MissingEmailClaim(t *testing.T) {
	key := generateTestKey(t)
	kid := "test-key-1"
	token := buildJWT(t, key,
		map[string]interface{}{"alg": "RS256", "kid": kid, "typ": "JWT"},
		map[string]interface{}{"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix()}, // no email
	)

	mw := newMiddleware("", staticKeyFetcher(kid, &key.PublicKey))
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/tree", nil)
	req.Header.Set("X-Goog-IAP-JWT-Assertion", token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestIAPMiddleware_EmptyEmailClaim(t *testing.T) {
	key := generateTestKey(t)
	kid := "test-key-1"
	token := buildJWT(t, key,
		map[string]interface{}{"alg": "RS256", "kid": kid, "typ": "JWT"},
		map[string]interface{}{"email": "", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix()},
	)

	mw := newMiddleware("", staticKeyFetcher(kid, &key.PublicKey))
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/tree", nil)
	req.Header.Set("X-Goog-IAP-JWT-Assertion", token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for empty email, got %d", rec.Code)
	}
}

func TestIAPMiddleware_MissingExpClaim(t *testing.T) {
	// A JWT with no exp defaults to zero (epoch), which is in the past.
	key := generateTestKey(t)
	kid := "test-key-1"
	token := buildJWT(t, key,
		map[string]interface{}{"alg": "RS256", "kid": kid, "typ": "JWT"},
		map[string]interface{}{"email": "alice@example.com", "iat": time.Now().Unix()}, // no exp
	)

	mw := newMiddleware("", staticKeyFetcher(kid, &key.PublicKey))
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/tree", nil)
	req.Header.Set("X-Goog-IAP-JWT-Assertion", token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// exp=0 (epoch) is in the past → token expired → 401.
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing exp, got %d", rec.Code)
	}
}

func TestIAPMiddleware_FutureIatIsAccepted(t *testing.T) {
	// A token with future iat is still valid: the implementation checks only exp.
	key := generateTestKey(t)
	kid := "test-key-1"
	token := buildJWT(t, key,
		map[string]interface{}{"alg": "RS256", "kid": kid, "typ": "JWT"},
		map[string]interface{}{
			"email": "alice@example.com",
			"exp":   time.Now().Add(time.Hour).Unix(),
			"iat":   time.Now().Add(time.Hour).Unix(), // future iat
		},
	)

	mw := newMiddleware("", staticKeyFetcher(kid, &key.PublicKey))
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/tree", nil)
	req.Header.Set("X-Goog-IAP-JWT-Assertion", token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for future iat with valid exp, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// commitTargetsMain tests
// ---------------------------------------------------------------------------

func TestCommitTargetsMain_NilBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/commit", nil)
	req.Body = nil
	if commitTargetsMain(req) {
		t.Error("nil body should return false")
	}
}

func TestCommitTargetsMain_EmptyBranchField(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/commit", bytes.NewReader([]byte(`{}`)))
	if commitTargetsMain(req) {
		t.Error("missing branch field should return false")
	}
}

func TestCommitTargetsMain_MalformedJSON(t *testing.T) {
	bodies := []string{
		`{"branch": `, // truncated
		`not json at all`,
		`{"branch":`,
	}
	for _, body := range bodies {
		req := httptest.NewRequest(http.MethodPost, "/commit", bytes.NewReader([]byte(body)))
		if commitTargetsMain(req) {
			t.Errorf("body=%q: malformed JSON should return false", body)
		}
	}
}

func TestCommitTargetsMain_NonMainBranch(t *testing.T) {
	bodies := []string{
		`{"branch":"feature/work"}`,
		`{"branch":""}`,
		`{"branch":"Main"}`, // case-sensitive
		`{}`,
	}
	for _, body := range bodies {
		req := httptest.NewRequest(http.MethodPost, "/commit", bytes.NewReader([]byte(body)))
		if commitTargetsMain(req) {
			t.Errorf("body=%q: should return false for non-main branch", body)
		}
	}
}

func TestCommitTargetsMain_MainBranch(t *testing.T) {
	body := `{"branch":"main","message":"m","files":[]}`
	req := httptest.NewRequest(http.MethodPost, "/commit", bytes.NewReader([]byte(body)))
	if !commitTargetsMain(req) {
		t.Error("expected true for branch=main")
	}
}

func TestCommitTargetsMain_BodyRestoredAfterRead(t *testing.T) {
	// Verify that commitTargetsMain restores the body so downstream handlers
	// can still read the full original content.
	originalBody := `{"branch":"main","message":"a commit","files":[]}`
	req := httptest.NewRequest(http.MethodPost, "/commit", bytes.NewReader([]byte(originalBody)))

	if !commitTargetsMain(req) {
		t.Error("expected true for branch=main")
	}

	remaining, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read body after commitTargetsMain: %v", err)
	}
	if string(remaining) != originalBody {
		t.Errorf("body not restored: got %q, want %q", string(remaining), originalBody)
	}
}

// ---------------------------------------------------------------------------
// repoAndSubPath tests
// ---------------------------------------------------------------------------

func TestRepoAndSubPath(t *testing.T) {
	tests := []struct {
		path        string
		wantRepo    string
		wantSubPath string
	}{
		{"/repos", "", ""},
		{"/repos/", "", ""},
		{"/repos/myrepo", "", ""},
		{"/repos/org/myrepo", "", ""},
		{"/repos/org/myrepo/-/branches", "org/myrepo", "branches"},
		{"/repos/org/myrepo/-/commit", "org/myrepo", "commit"},
		{"/repos/org/myrepo/-/branch/feature/with/slashes", "org/myrepo", "branch/feature/with/slashes"},
		{"/repos/acme/team/sub/-/tree", "acme/team/sub", "tree"},
		{"/healthz", "", ""},
		{"", "", ""},
	}
	for _, tc := range tests {
		repo, sub := repoAndSubPath(tc.path)
		if repo != tc.wantRepo || sub != tc.wantSubPath {
			t.Errorf("repoAndSubPath(%q) = (%q, %q), want (%q, %q)",
				tc.path, repo, sub, tc.wantRepo, tc.wantSubPath)
		}
	}
}

// ---------------------------------------------------------------------------
// Role transition tests
// ---------------------------------------------------------------------------

func TestRBACMiddleware_ReaderUpgradedToWriter(t *testing.T) {
	var currentRole model.RoleType = model.RoleReader
	store := &mockRoleStore{
		getRoleFn: func(_ context.Context, repo, identity string) (*model.Role, error) {
			return &model.Role{Identity: identity, Role: currentRole}, nil
		},
	}
	h := rbacTestServer(store, "", "bob@example.com")

	// As reader, POST /commit to a branch → 403.
	rec := rbacDo(t, h, http.MethodPost, "/repos/myrepo/myrepo/-/commit",
		`{"branch":"feature/x","message":"m","files":[]}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("reader POST commit: expected 403, got %d", rec.Code)
	}

	// Upgrade role to writer.
	currentRole = model.RoleWriter

	// Writer can now commit to a feature branch → 200.
	rec = rbacDo(t, h, http.MethodPost, "/repos/myrepo/myrepo/-/commit",
		`{"branch":"feature/x","message":"m","files":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("writer POST commit: expected 200, got %d", rec.Code)
	}
}

