package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dlorenc/docstore/internal/archivesign"
	"github.com/dlorenc/docstore/internal/citoken"
	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/model"
)

// stubJobTokenStore is a minimal jobTokenStore implementation for tests.
type stubJobTokenStore struct {
	lookupFn func(ctx context.Context, hashedToken string) (*model.CIJob, error)
}

func (s *stubJobTokenStore) LookupRequestToken(ctx context.Context, hashedToken string) (*model.CIJob, error) {
	return s.lookupFn(ctx, hashedToken)
}

// buildPresignServer creates a server with archiveHMACSecret and optional jobTokenStore
// for testing the presigned archive handlers.
func buildPresignServer(secret []byte, jobStore jobTokenStore) *server {
	return &server{
		commitStore:       &mockStore{},
		archiveHMACSecret: secret,
		archiveBaseURL:    "https://example.com",
		jobTokenStore:     jobStore,
	}
}

// ---------------------------------------------------------------------------
// handleArchivePresign tests
// ---------------------------------------------------------------------------

func TestHandleArchivePresign_NotConfigured(t *testing.T) {
	// archiveHMACSecret is nil → 503
	s := buildPresignServer(nil, nil)
	handler := s.buildHandler(devID, devID, s.commitStore)
	req := httptest.NewRequest(http.MethodPost, "/repos/org/myrepo/-/archive/presign", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleArchivePresign_MissingAuth(t *testing.T) {
	secret := []byte("test-secret")
	store := &stubJobTokenStore{
		lookupFn: func(_ context.Context, _ string) (*model.CIJob, error) {
			return nil, db.ErrTokenInvalid
		},
	}
	s := buildPresignServer(secret, store)
	handler := s.buildHandler(devID, devID, s.commitStore)
	// No Authorization header.
	req := httptest.NewRequest(http.MethodPost, "/repos/org/myrepo/-/archive/presign", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleArchivePresign_InvalidToken(t *testing.T) {
	secret := []byte("test-secret")
	store := &stubJobTokenStore{
		lookupFn: func(_ context.Context, _ string) (*model.CIJob, error) {
			return nil, db.ErrTokenInvalid
		},
	}
	s := buildPresignServer(secret, store)
	handler := s.buildHandler(devID, devID, s.commitStore)
	req := httptest.NewRequest(http.MethodPost, "/repos/org/myrepo/-/archive/presign", nil)
	req.Header.Set("Authorization", "Bearer invalid-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleArchivePresign_RepoMismatch(t *testing.T) {
	secret := []byte("test-secret")
	// Token is valid but job.Repo doesn't match the URL repo.
	store := &stubJobTokenStore{
		lookupFn: func(_ context.Context, _ string) (*model.CIJob, error) {
			return &model.CIJob{
				ID:       "job-1",
				Repo:     "org/other-repo", // different repo
				Branch:   "main",
				Sequence: 10,
			}, nil
		},
	}
	s := buildPresignServer(secret, store)
	handler := s.buildHandler(devID, devID, s.commitStore)
	plaintext, _, _ := citoken.GenerateRequestToken()
	req := httptest.NewRequest(http.MethodPost, "/repos/org/myrepo/-/archive/presign", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for repo mismatch, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleArchivePresign_ValidToken(t *testing.T) {
	secret := []byte("test-secret")
	plaintext, hashed, err := citoken.GenerateRequestToken()
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	store := &stubJobTokenStore{
		lookupFn: func(_ context.Context, h string) (*model.CIJob, error) {
			if h != hashed {
				return nil, db.ErrTokenInvalid
			}
			return &model.CIJob{
				ID:       "job-1",
				Repo:     "org/myrepo",
				Branch:   "feature",
				Sequence: 42,
			}, nil
		},
	}
	s := buildPresignServer(secret, store)
	handler := s.buildHandler(devID, devID, s.commitStore)
	req := httptest.NewRequest(http.MethodPost, "/repos/org/myrepo/-/archive/presign", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	u, ok := resp["url"]
	if !ok || u == "" {
		t.Fatal("expected non-empty url in response")
	}
	// URL should contain branch, at, expires, sig params.
	for _, param := range []string{"branch=", "at=42", "expires=", "sig="} {
		if !containsStr(u, param) {
			t.Errorf("expected URL to contain %q; got %q", param, u)
		}
	}
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsAt(s, substr))
}

func containsAt(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// handlePresignedArchive tests
// ---------------------------------------------------------------------------

func TestHandlePresignedArchive_MissingSig(t *testing.T) {
	// Without sig param, the request is passed through to IAP and the inner mux.
	// In the inner mux, "archive" with GET goes to handleArchive, which needs
	// a read store. Without one configured, it returns 503.
	// The key test: no sig → does NOT call handlePresignedArchive directly.
	s := buildPresignServer([]byte("secret"), nil)
	s.readStore = nil // ensure readStore is nil
	handler := s.buildHandler(devID, devID, s.commitStore)
	req := httptest.NewRequest(http.MethodGet, "/repos/org/myrepo/-/archive?branch=main", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	// Without sig, goes through IAP → inner mux → handleArchive → 503 (no read store)
	// or 404 (repo not found via validateRepo with mockStore returning ErrRepoNotFound).
	// Either way, it's not 403 (which would come from handlePresignedArchive's HMAC check).
	if rec.Code == http.StatusForbidden {
		t.Fatalf("expected non-403 (not from presigned handler) when sig is absent, got %d", rec.Code)
	}
}

func TestHandlePresignedArchive_ExpiredURL(t *testing.T) {
	secret := []byte("test-secret")
	s := buildPresignServer(secret, nil)
	handler := s.buildHandler(devID, devID, s.commitStore)

	// Sign with past expiry.
	expiresUnix := time.Now().Add(-time.Hour).Unix()
	sig := archivesign.Sign(secret, "org/myrepo", "main", 1, expiresUnix)
	url := fmt.Sprintf("/repos/org/myrepo/-/archive?branch=main&at=1&expires=%d&sig=%s", expiresUnix, sig)

	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for expired URL, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandlePresignedArchive_InvalidSig(t *testing.T) {
	secret := []byte("test-secret")
	s := buildPresignServer(secret, nil)
	handler := s.buildHandler(devID, devID, s.commitStore)

	expiresUnix := time.Now().Add(time.Hour).Unix()
	// Use a wrong secret to generate the sig.
	badSig := archivesign.Sign([]byte("wrong-secret"), "org/myrepo", "main", 1, expiresUnix)
	url := fmt.Sprintf("/repos/org/myrepo/-/archive?branch=main&at=1&expires=%d&sig=%s", expiresUnix, badSig)

	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for invalid sig, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandlePresignedArchive_ValidSig_PassesHMACCheck(t *testing.T) {
	secret := []byte("test-secret")
	s := buildPresignServer(secret, nil)
	// mockStore.getRepoFn returns ErrRepoNotFound by default (nil fn → ErrRepoNotFound).
	// So after HMAC passes, handleArchive will call validateRepo → 404.
	// This proves the request passes the HMAC check (doesn't return 403).
	handler := s.buildHandler(devID, devID, s.commitStore)

	expiresUnix := time.Now().Add(time.Hour).Unix()
	sig := archivesign.Sign(secret, "org/myrepo", "main", 1, expiresUnix)
	url := fmt.Sprintf("/repos/org/myrepo/-/archive?branch=main&at=1&expires=%d&sig=%s", expiresUnix, sig)

	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	// 403 would indicate HMAC failure; any other code means HMAC check passed.
	if rec.Code == http.StatusForbidden {
		t.Fatalf("HMAC check should have passed, got 403; body: %s", rec.Body.String())
	}
}
