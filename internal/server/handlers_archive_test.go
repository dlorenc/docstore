package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dlorenc/docstore/internal/archivesign"
	"github.com/dlorenc/docstore/internal/citoken"
	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/policy"
	"github.com/dlorenc/docstore/internal/service"
	"github.com/dlorenc/docstore/internal/store"
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
// Checksum tests
// ---------------------------------------------------------------------------

func buildPresignJob(t *testing.T, secret []byte, repo, branch string, seq int64) (plaintext string, jobStore *stubJobTokenStore) {
	t.Helper()
	var hashed string
	var err error
	plaintext, hashed, err = citoken.GenerateRequestToken()
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	jobStore = &stubJobTokenStore{
		lookupFn: func(_ context.Context, h string) (*model.CIJob, error) {
			if h != hashed {
				return nil, db.ErrTokenInvalid
			}
			return &model.CIJob{ID: "job-1", Repo: repo, Branch: branch, Sequence: seq}, nil
		},
	}
	return
}

func TestHandleArchivePresign_ChecksumNilReadStore(t *testing.T) {
	secret := []byte("test-secret")
	plaintext, jobStore := buildPresignJob(t, secret, "org/myrepo", "feature", 42)

	s := buildPresignServer(secret, jobStore)
	// readStore is nil (buildPresignServer does not set it)
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
	if resp["checksum"] != "" {
		t.Fatalf("expected empty checksum when readStore is nil, got %q", resp["checksum"])
	}
}

func TestHandleArchivePresign_ChecksumWithReadStore(t *testing.T) {
	secret := []byte("test-secret")
	plaintext, jobStore := buildPresignJob(t, secret, "org/myrepo", "feature", 42)

	fileContent := []byte("hello world\n")
	rs := &mockReadStore{
		materializeTreeFn: func(_ context.Context, _, _ string, _ *int64, _ int, afterPath string) ([]store.TreeEntry, error) {
			if afterPath == "" {
				return []store.TreeEntry{{Path: "test.txt"}}, nil
			}
			return nil, nil
		},
		getFileFn: func(_ context.Context, _, _, _ string, _ *int64) (*store.FileContent, error) {
			return &store.FileContent{Content: fileContent}, nil
		},
	}

	s := buildPresignServer(secret, jobStore)
	s.readStore = rs
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
	checksum := resp["checksum"]
	if !strings.HasPrefix(checksum, "sha256:") {
		t.Fatalf("expected checksum to start with sha256:, got %q", checksum)
	}

	// Verify the checksum matches what writeArchive produces.
	h := sha256.New()
	if err := writeArchive(context.Background(), rs, h, "org/myrepo", "feature", 42); err != nil {
		t.Fatalf("writeArchive: %v", err)
	}
	expected := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if checksum != expected {
		t.Fatalf("checksum mismatch: got %q, want %q", checksum, expected)
	}
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

// ---------------------------------------------------------------------------
// Routing regression: POST /repos must not be redirected
// ---------------------------------------------------------------------------

// TestCreateRepo_NoTrailingSlashRedirect verifies that POST /repos (without a
// trailing slash) is served directly and returns 201, not a 307 redirect to
// POST /repos/. The outer mux previously used "/repos/" as its catch-all which
// caused Go's ServeMux to redirect bare POST /repos to POST /repos/.
func TestCreateRepo_NoTrailingSlashRedirect(t *testing.T) {
	ms := &mockStore{
		createRepoFn: func(_ context.Context, req model.CreateRepoRequest) (*model.Repo, error) {
			return &model.Repo{Name: req.Owner + "/" + req.Name, Owner: req.Owner}, nil
		},
		setRoleFn: func(_ context.Context, _, _ string, _ model.RoleType) error {
			return nil
		},
	}
	s := &server{
		commitStore: ms,
		svc:         service.New(ms, nil, policy.NewCache()),
	}
	handler := s.buildHandler(devID, devID, ms)

	body := strings.NewReader(`{"owner":"default","name":"myrepo"}`)
	req := httptest.NewRequest(http.MethodPost, "/repos", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusTemporaryRedirect || rec.Code == http.StatusMovedPermanently || rec.Code == http.StatusPermanentRedirect {
		t.Fatalf("POST /repos was redirected (status %d) — trailing-slash redirect bug; body: %s", rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
}
