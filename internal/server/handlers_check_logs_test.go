package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dlorenc/docstore/internal/citoken"
	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/logstore"
	"github.com/dlorenc/docstore/internal/model"
)

// mockLogStore implements logstore.LogStore for tests.
type mockLogStore struct {
	writeURL string
	writeErr error
	calls    int
}

func (m *mockLogStore) Write(_ context.Context, _, _ string, _ int64, _, _ string) (string, error) {
	m.calls++
	return m.writeURL, m.writeErr
}

var _ logstore.LogStore = (*mockLogStore)(nil)

// buildCheckLogsServer creates a server wired for testing handleCheckLogs.
func buildCheckLogsServer(jobStore jobTokenStore, ls logstore.LogStore) *server {
	return &server{
		commitStore:   &mockStore{},
		jobTokenStore: jobStore,
		logStore:      ls,
	}
}

// ---------------------------------------------------------------------------
// handleCheckLogs tests
// ---------------------------------------------------------------------------

func TestHandleCheckLogs_NotConfigured(t *testing.T) {
	// logStore is nil → 503
	s := buildCheckLogsServer(&stubJobTokenStore{
		lookupFn: func(_ context.Context, _ string) (*model.CIJob, error) {
			return nil, db.ErrTokenInvalid
		},
	}, nil)
	handler := s.buildHandler(devID, devID, s.commitStore)

	req := httptest.NewRequest(http.MethodPost, "/repos/org/myrepo/-/check/ci_build/logs", strings.NewReader("log output"))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Authorization", "Bearer sometoken")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleCheckLogs_MissingAuth(t *testing.T) {
	ls := &mockLogStore{writeURL: "gs://bucket/key"}
	s := buildCheckLogsServer(&stubJobTokenStore{
		lookupFn: func(_ context.Context, _ string) (*model.CIJob, error) {
			return nil, db.ErrTokenInvalid
		},
	}, ls)
	handler := s.buildHandler(devID, devID, s.commitStore)

	// No Authorization header.
	req := httptest.NewRequest(http.MethodPost, "/repos/org/myrepo/-/check/ci_build/logs", strings.NewReader("logs"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleCheckLogs_InvalidToken(t *testing.T) {
	ls := &mockLogStore{writeURL: "gs://bucket/key"}
	s := buildCheckLogsServer(&stubJobTokenStore{
		lookupFn: func(_ context.Context, _ string) (*model.CIJob, error) {
			return nil, db.ErrTokenInvalid
		},
	}, ls)
	handler := s.buildHandler(devID, devID, s.commitStore)

	req := httptest.NewRequest(http.MethodPost, "/repos/org/myrepo/-/check/ci_build/logs", strings.NewReader("logs"))
	req.Header.Set("Authorization", "Bearer bad-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for invalid token, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleCheckLogs_RepoMismatch(t *testing.T) {
	ls := &mockLogStore{writeURL: "gs://bucket/key"}
	// Token is valid but job.Repo doesn't match URL repo.
	store := &stubJobTokenStore{
		lookupFn: func(_ context.Context, _ string) (*model.CIJob, error) {
			return &model.CIJob{
				ID:       "job-1",
				Repo:     "org/other-repo",
				Branch:   "main",
				Sequence: 1,
			}, nil
		},
	}
	s := buildCheckLogsServer(store, ls)
	handler := s.buildHandler(devID, devID, s.commitStore)

	plaintext, _, _ := citoken.GenerateRequestToken()
	req := httptest.NewRequest(http.MethodPost, "/repos/org/myrepo/-/check/ci_build/logs", strings.NewReader("logs"))
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for repo mismatch, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleCheckLogs_ValidToken_WritesLogs(t *testing.T) {
	wantLogURL := "gs://my-bucket/org/myrepo/feature/42/ci_build.log"
	ls := &mockLogStore{writeURL: wantLogURL}

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
	s := buildCheckLogsServer(store, ls)
	handler := s.buildHandler(devID, devID, s.commitStore)

	const logBody = "=== build output ===\nok\n"
	req := httptest.NewRequest(http.MethodPost, "/repos/org/myrepo/-/check/ci_build/logs", strings.NewReader(logBody))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("expected application/json content type, got %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, wantLogURL) {
		t.Errorf("expected response to contain URL %q; got %q", wantLogURL, body)
	}
	if ls.calls != 1 {
		t.Errorf("expected 1 log store write, got %d", ls.calls)
	}
}

func TestHandleCheckLogs_CheckNameWithSlash(t *testing.T) {
	// Check names with "/" should be handled correctly.
	ls := &mockLogStore{writeURL: "gs://bucket/key"}
	plaintext, hashed, _ := citoken.GenerateRequestToken()
	store := &stubJobTokenStore{
		lookupFn: func(_ context.Context, h string) (*model.CIJob, error) {
			if h != hashed {
				return nil, db.ErrTokenInvalid
			}
			return &model.CIJob{
				ID:     "job-1",
				Repo:   "org/myrepo",
				Branch: "main",
			}, nil
		},
	}
	s := buildCheckLogsServer(store, ls)
	handler := s.buildHandler(devID, devID, s.commitStore)

	// "ci/build" as check name — encoded in URL as "ci%2Fbuild" or "ci/build"
	// The router receives endpoint = "check/ci/build/logs".
	// The handler strips "check/" prefix and "/logs" suffix → "ci/build".
	req := httptest.NewRequest(http.MethodPost, "/repos/org/myrepo/-/check/ci%2Fbuild/logs", strings.NewReader("logs"))
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Should succeed (200) or reach the logStore — either way, not 401/404.
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleCheckLogs_MethodNotAllowed(t *testing.T) {
	ls := &mockLogStore{}
	s := buildCheckLogsServer(&stubJobTokenStore{
		lookupFn: func(_ context.Context, _ string) (*model.CIJob, error) {
			return nil, db.ErrTokenInvalid
		},
	}, ls)
	handler := s.buildHandler(devID, devID, s.commitStore)

	// GET is not allowed on this endpoint.
	req := httptest.NewRequest(http.MethodGet, "/repos/org/myrepo/-/check/ci_build/logs", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// GET requests fall through to the IAP + inner mux, which won't match check/ci_build/logs.
	// We just verify it's not a 200.
	if rec.Code == http.StatusOK {
		t.Fatalf("expected non-200 for GET, got 200")
	}
}
