package server

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/dlorenc/docstore/internal/model"
)

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
