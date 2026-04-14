package server

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
