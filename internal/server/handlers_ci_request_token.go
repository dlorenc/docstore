package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/dlorenc/docstore/internal/citoken"
	"github.com/dlorenc/docstore/internal/db"
	evtypes "github.com/dlorenc/docstore/internal/events/types"
	"github.com/dlorenc/docstore/internal/model"
)

// handleCIConfig implements GET /repos/:repo/-/ci/config.
// Authenticated via request_token (Authorization: Bearer <plaintext>).
// Returns the .docstore/ci.yaml file at the job's branch and sequence as a
// JSON FileResponse, using the same wire format as the IAP-gated file endpoint.
func (s *server) handleCIConfig(w http.ResponseWriter, r *http.Request) {
	if s.jobTokenStore == nil || s.readStore == nil {
		writeError(w, http.StatusServiceUnavailable, "ci config not available")
		return
	}

	// 1. Extract and validate request_token from Authorization header.
	authHdr := r.Header.Get("Authorization")
	plaintext := strings.TrimPrefix(authHdr, "Bearer ")
	if plaintext == "" || plaintext == authHdr {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	hashed := citoken.HashRequestToken(plaintext)
	job, err := s.jobTokenStore.LookupRequestToken(r.Context(), hashed)
	if errors.Is(err, db.ErrTokenInvalid) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// 2. Verify repo in token matches the request path.
	repoName := r.PathValue("name")
	if job.Repo != repoName {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// 3. Fetch .docstore/ci.yaml at the job's branch and sequence.
	seq := job.Sequence
	fc, err := s.readStore.GetFile(r.Context(), job.Repo, job.Branch, ".docstore/ci.yaml", &seq)
	if err != nil {
		slog.Error("ci config fetch", "repo", job.Repo, "branch", job.Branch, "seq", seq, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if fc == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(fc) //nolint:errcheck
}

// handleCICheck implements POST /repos/:repo/-/check via request_token authentication.
// This is the worker-facing check-run endpoint, authenticated via request_token.
// It mirrors handleCheck but bypasses IAP/RBAC and derives identity from the token.
func (s *server) handleCICheck(w http.ResponseWriter, r *http.Request) {
	if s.jobTokenStore == nil || s.commitStore == nil {
		writeError(w, http.StatusServiceUnavailable, "ci check not available")
		return
	}

	// 1. Extract and validate request_token from Authorization header.
	authHdr := r.Header.Get("Authorization")
	plaintext := strings.TrimPrefix(authHdr, "Bearer ")
	if plaintext == "" || plaintext == authHdr {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	hashed := citoken.HashRequestToken(plaintext)
	job, err := s.jobTokenStore.LookupRequestToken(r.Context(), hashed)
	if errors.Is(err, db.ErrTokenInvalid) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// 2. Verify repo in token matches the request path.
	repoName := r.PathValue("name")
	if job.Repo != repoName {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// 3. Decode and validate request body.
	var req model.CreateCheckRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Branch == "" {
		writeError(w, http.StatusBadRequest, "branch is required")
		return
	}
	if req.CheckName == "" {
		writeError(w, http.StatusBadRequest, "check_name is required")
		return
	}
	if req.Status == "" {
		writeError(w, http.StatusBadRequest, "status is required")
		return
	}

	reporter := "ci-job:" + job.ID

	attempt := int16(1)
	if req.Attempt != nil {
		attempt = *req.Attempt
	}
	cr, err := s.commitStore.CreateCheckRun(r.Context(), repoName, req.Branch, req.CheckName, req.Status, reporter, req.LogURL, req.Sequence, attempt, req.Metadata)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrBranchNotFound):
			writeAPIError(w, ErrCodeBranchNotFound, http.StatusNotFound, "branch not found")
		default:
			slog.Error("internal error", "op", "ci_check", "repo", repoName, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	bs, bsErr := s.computeBranchStatus(r.Context(), repoName, req.Branch, reporter)
	if bsErr != nil {
		slog.Warn("branch status computation failed", "op", "ci_check", "repo", repoName, "branch", req.Branch, "error", bsErr)
	}
	s.emit(r.Context(), evtypes.CheckReported{
		Repo:         repoName,
		Branch:       req.Branch,
		Sequence:     cr.Sequence,
		CheckName:    req.CheckName,
		Status:       string(req.Status),
		Reporter:     reporter,
		BranchStatus: bs,
	})
	writeJSON(w, http.StatusCreated, model.CreateCheckRunResponse{
		ID:       cr.ID,
		Sequence: cr.Sequence,
		LogURL:   req.LogURL,
	})
}
