package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"cloud.google.com/go/storage"

	"github.com/dlorenc/docstore/internal/model"
)

// logFetcher abstracts reading a log object from a remote store.
// The real implementation reads from GCS; tests inject a mock.
type logFetcher interface {
	Fetch(ctx context.Context, bucket, key string) ([]byte, error)
}

// gcsLogFetcher implements logFetcher using the GCS storage client.
type gcsLogFetcher struct {
	client *storage.Client
}

func (f *gcsLogFetcher) Fetch(ctx context.Context, bucket, key string) ([]byte, error) {
	rc, err := f.client.Bucket(bucket).Object(key).NewReader(ctx)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// ---------------------------------------------------------------------------
// CI job handlers (read-only)
// ---------------------------------------------------------------------------

// handleListCIJobs implements GET /repos/:name/-/ci-jobs (reader+)
// Query params:
//
//	branch (optional)
//	status (optional, one of: queued/claimed/passed/failed)
//	limit  (optional, default 50, max 200)
func (s *server) handleListCIJobs(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	q := r.URL.Query()

	var branch *string
	if v := q.Get("branch"); v != "" {
		branch = &v
	}

	var status *string
	if v := q.Get("status"); v != "" {
		switch v {
		case "queued", "claimed", "passed", "failed":
		default:
			writeError(w, http.StatusBadRequest, "invalid 'status' parameter: must be one of queued, claimed, passed, failed")
			return
		}
		status = &v
	}

	limit := 50
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeError(w, http.StatusBadRequest, "invalid 'limit' parameter: must be between 1 and 200")
			return
		}
		limit = n
	}

	jobs, err := s.commitStore.ListCIJobs(r.Context(), repo, branch, status, limit)
	if err != nil {
		slog.Error("internal error", "op", "list_ci_jobs", "repo", repo, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		return
	}
	if jobs == nil {
		jobs = []model.CIJob{}
	}
	writeJSON(w, http.StatusOK, model.ListCIJobsResponse{Jobs: jobs})
}

// handleGetCIJob implements GET /ci-jobs/{id}
// The caller must have reader access to the job's repo.
func (s *server) handleGetCIJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	job, err := s.commitStore.GetCIJob(r.Context(), id)
	if err != nil {
		slog.Error("internal error", "op", "get_ci_job", "id", id, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		return
	}
	if job == nil {
		writeError(w, http.StatusNotFound, "ci job not found")
		return
	}

	// Require reader access to the job's repo.
	if !s.requireRepoReadAccess(w, r, job.Repo) {
		return
	}

	writeJSON(w, http.StatusOK, job)
}

// handleCIJobLogs implements GET /repos/:repo/-/ci-jobs/:id/logs/:check
// Streams the stored log for the named check as text/plain.
func (s *server) handleCIJobLogs(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	id := r.PathValue("id")
	check := r.PathValue("check")

	job, err := s.commitStore.GetCIJob(r.Context(), id)
	if err != nil {
		slog.Error("internal error", "op", "ci_job_logs", "id", id, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		return
	}
	if job == nil {
		writeError(w, http.StatusNotFound, "ci job not found")
		return
	}
	if job.Repo != repo {
		writeError(w, http.StatusNotFound, "ci job not found")
		return
	}
	if job.LogURL == nil || *job.LogURL == "" {
		writeError(w, http.StatusNotFound, "no logs for this job")
		return
	}

	// Extract bucket from gs:// URL.
	const gsPrefix = "gs://"
	if !strings.HasPrefix(*job.LogURL, gsPrefix) {
		writeError(w, http.StatusNotFound, "log not available")
		return
	}
	rest := strings.TrimPrefix(*job.LogURL, gsPrefix)
	bucket, _, found := strings.Cut(rest, "/")
	if !found || bucket == "" {
		writeError(w, http.StatusInternalServerError, "invalid log URL")
		return
	}

	// Construct the object key for the requested check.
	safeName := strings.ReplaceAll(check, "/", "_")
	objectKey := fmt.Sprintf("%s/%s/%d/%s.log", job.Repo, job.Branch, job.Sequence, safeName)

	lf := s.logFetcher
	if lf == nil {
		// Production: create a GCS client using Application Default Credentials.
		client, err := storage.NewClient(r.Context())
		if err != nil {
			slog.Error("create gcs client for log fetch", "error", err)
			writeError(w, http.StatusInternalServerError, "could not connect to log storage")
			return
		}
		defer client.Close()
		lf = &gcsLogFetcher{client: client}
	}

	data, err := lf.Fetch(r.Context(), bucket, objectKey)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			writeError(w, http.StatusNotFound, "log not found")
			return
		}
		slog.Error("fetch log", "bucket", bucket, "key", objectKey, "error", err)
		writeError(w, http.StatusInternalServerError, "could not read log")
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(data) //nolint:errcheck
}
