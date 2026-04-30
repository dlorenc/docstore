package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"cloud.google.com/go/storage"

	"github.com/dlorenc/docstore/internal/model"
)

// mockLogFetcher implements logFetcher for tests.
type mockLogFetcher struct {
	data []byte
	err  error
}

func (m *mockLogFetcher) Fetch(_ context.Context, _, _ string) ([]byte, error) {
	return m.data, m.err
}

// newCILogServer constructs a server with an injected logFetcher for testing.
func newCILogServer(ms *mockStore, lf logFetcher) http.Handler {
	s := &server{
		commitStore: ms,
		logFetcher:  lf,
	}
	return s.buildHandler(devID, devID, ms)
}

// ---------------------------------------------------------------------------
// handleCIJobLogs tests
// ---------------------------------------------------------------------------

func TestHandleCIJobLogs_JobNotFound(t *testing.T) {
	// getCIJobFn returns nil → 404
	ms := &mockStore{} // GetCIJob returns nil, nil by default
	srv := newCILogServer(ms, nil)

	req := httptest.NewRequest(http.MethodGet, "/repos/org/repo/-/ci-jobs/no-such-id/logs/ci_build", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleCIJobLogs_NoLogURL(t *testing.T) {
	// Job exists but has no log_url → 404
	ms := &mockStore{
		getCIJobFn: func(_ context.Context, id string) (*model.CIJob, error) {
			return &model.CIJob{
				ID:     id,
				Repo:   "org/repo",
				Branch: "main",
			}, nil
		},
	}
	srv := newCILogServer(ms, nil)

	req := httptest.NewRequest(http.MethodGet, "/repos/org/repo/-/ci-jobs/some-id/logs/ci_build", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleCIJobLogs_Found(t *testing.T) {
	// Job exists with log_url, GCS returns content → 200 text/plain
	logURL := "gs://my-bucket/org/repo/main/42/ci_build.log"
	wantContent := "=== build output ===\nok\n"
	ms := &mockStore{
		getCIJobFn: func(_ context.Context, id string) (*model.CIJob, error) {
			return &model.CIJob{
				ID:       id,
				Repo:     "org/repo",
				Branch:   "main",
				Sequence: 42,
				LogURL:   &logURL,
			}, nil
		},
	}
	lf := &mockLogFetcher{data: []byte(wantContent)}
	srv := newCILogServer(ms, lf)

	req := httptest.NewRequest(http.MethodGet, "/repos/org/repo/-/ci-jobs/some-id/logs/ci_build", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	if ct != "text/plain; charset=utf-8" {
		t.Errorf("expected text/plain; charset=utf-8, got %q", ct)
	}
	if got := rec.Body.String(); got != wantContent {
		t.Errorf("expected body %q, got %q", wantContent, got)
	}
}

func TestHandleCIJobLogs_GCSNotFound(t *testing.T) {
	// GCS returns ErrObjectNotExist → 404
	logURL := "gs://my-bucket/org/repo/main/42/ci_build.log"
	ms := &mockStore{
		getCIJobFn: func(_ context.Context, id string) (*model.CIJob, error) {
			return &model.CIJob{
				ID:       id,
				Repo:     "org/repo",
				Branch:   "main",
				Sequence: 42,
				LogURL:   &logURL,
			}, nil
		},
	}
	lf := &mockLogFetcher{err: storage.ErrObjectNotExist}
	srv := newCILogServer(ms, lf)

	req := httptest.NewRequest(http.MethodGet, "/repos/org/repo/-/ci-jobs/some-id/logs/ci_build", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing GCS object, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleCIJobLogs_WrongRepo(t *testing.T) {
	// Job exists but for a different repo → 404
	logURL := "gs://my-bucket/other/repo/main/42/ci_build.log"
	ms := &mockStore{
		getCIJobFn: func(_ context.Context, id string) (*model.CIJob, error) {
			return &model.CIJob{
				ID:     id,
				Repo:   "other/repo", // different from URL repo
				Branch: "main",
				LogURL: &logURL,
			}, nil
		},
	}
	srv := newCILogServer(ms, nil)

	req := httptest.NewRequest(http.MethodGet, "/repos/org/repo/-/ci-jobs/some-id/logs/ci_build", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for wrong repo, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleCIJobLogs_MethodNotAllowed(t *testing.T) {
	ms := &mockStore{}
	srv := newCILogServer(ms, nil)

	req := httptest.NewRequest(http.MethodPost, "/repos/org/repo/-/ci-jobs/some-id/logs/ci_build", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}
