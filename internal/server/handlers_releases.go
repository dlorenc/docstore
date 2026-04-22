package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/model"
)

// ---------------------------------------------------------------------------
// Release handlers
// ---------------------------------------------------------------------------

// handleCreateRelease implements POST /repos/:name/-/releases (maintainer+)
// Body: {name, sequence?, body?}. Sequence defaults to current main head if omitted.
func (s *server) handleCreateRelease(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	var req model.CreateReleaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	// Resolve sequence: default to main head if not provided.
	var sequence int64
	if req.Sequence != nil {
		sequence = *req.Sequence
		// Validate that the provided sequence actually exists in the commits table.
		exists, err := s.commitStore.CommitSequenceExists(r.Context(), repo, sequence)
		if err != nil {
			slog.Error("internal error", "op", "create_release_seq_check", "repo", repo, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
			return
		}
		if !exists {
			writeError(w, http.StatusBadRequest, "sequence not found")
			return
		}
	} else {
		if s.readStore == nil {
			writeAPIError(w, ErrCodeServiceUnavailable, http.StatusServiceUnavailable, "read store not available")
			return
		}
		branchInfo, err := s.readStore.GetBranch(r.Context(), repo, "main")
		if err != nil || branchInfo == nil {
			slog.Error("internal error", "op", "create_release_head", "repo", repo, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "could not resolve main head sequence")
			return
		}
		sequence = branchInfo.HeadSequence
	}

	createdBy := IdentityFromContext(r.Context())
	rel, err := s.commitStore.CreateRelease(r.Context(), repo, req.Name, sequence, req.Body, createdBy)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrReleaseExists):
			writeAPIError(w, ErrCodeConflict, http.StatusConflict, "release already exists")
		default:
			slog.Error("internal error", "op", "create_release", "repo", repo, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	slog.Info("release created", "repo", repo, "release", req.Name, "sequence", sequence, "by", createdBy)
	writeJSON(w, http.StatusCreated, rel)
}

// handleListReleases implements GET /repos/:name/-/releases (reader+)
func (s *server) handleListReleases(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "invalid 'limit' parameter")
			return
		}
		limit = n
	}
	afterID := r.URL.Query().Get("after")

	releases, err := s.commitStore.ListReleases(r.Context(), repo, limit, afterID)
	if err != nil {
		if errors.Is(err, db.ErrInvalidCursor) {
			writeError(w, http.StatusBadRequest, "invalid pagination cursor")
			return
		}
		slog.Error("internal error", "op", "list_releases", "repo", repo, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		return
	}
	if releases == nil {
		releases = []model.Release{}
	}
	writeJSON(w, http.StatusOK, model.ListReleasesResponse{Releases: releases})
}

// handleGetRelease implements GET /repos/:name/-/releases/:release (reader+)
func (s *server) handleGetRelease(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}
	releaseName := r.PathValue("release")

	rel, err := s.commitStore.GetRelease(r.Context(), repo, releaseName)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrReleaseNotFound):
			writeAPIError(w, ErrCodeReleaseNotFound, http.StatusNotFound, "release not found")
		default:
			slog.Error("internal error", "op", "get_release", "repo", repo, "release", releaseName, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, rel)
}

// handleDeleteRelease implements DELETE /repos/:name/-/releases/:release (admin only)
func (s *server) handleDeleteRelease(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}
	releaseName := r.PathValue("release")

	if err := s.commitStore.DeleteRelease(r.Context(), repo, releaseName); err != nil {
		switch {
		case errors.Is(err, db.ErrReleaseNotFound):
			writeAPIError(w, ErrCodeReleaseNotFound, http.StatusNotFound, "release not found")
		default:
			slog.Error("internal error", "op", "delete_release", "repo", repo, "release", releaseName, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	slog.Info("release deleted", "repo", repo, "release", releaseName, "by", IdentityFromContext(r.Context()))
	w.WriteHeader(http.StatusNoContent)
}
