package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dlorenc/docstore/internal/db"
	evtypes "github.com/dlorenc/docstore/internal/events/types"
	"github.com/dlorenc/docstore/internal/model"
)

// handleCreateRepo implements POST /repos
func (s *server) handleCreateRepo(w http.ResponseWriter, r *http.Request) {
	var req model.CreateRepoRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Owner == "" {
		writeError(w, http.StatusBadRequest, "owner is required")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	fullName := req.FullName()
	if strings.SplitN(fullName, "/", 2)[0] != req.Owner {
		writeError(w, http.StatusBadRequest, "owner must match first segment of repo name")
		return
	}
	if req.CreatedBy == "" {
		req.CreatedBy = IdentityFromContext(r.Context())
	}

	repo, err := s.commitStore.CreateRepo(r.Context(), req)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrRepoExists):
			writeAPIError(w, ErrCodeRepoExists, http.StatusConflict, "repo already exists")
		case errors.Is(err, db.ErrOrgNotFound):
			writeAPIError(w, ErrCodeOrgNotFound, http.StatusNotFound, "org not found")
		default:
			slog.Error("internal error", "op", "create_repo", "repo", req.Name, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	identity := IdentityFromContext(r.Context())
	s.emit(r.Context(), evtypes.RepoCreated{Repo: repo.Name, Owner: repo.Owner, CreatedBy: identity})
	slog.Info("repo created", "repo", repo.Name, "by", identity)
	writeJSON(w, http.StatusCreated, repo)
}

// handleListRepos implements GET /repos
func (s *server) handleListRepos(w http.ResponseWriter, r *http.Request) {
	repos, err := s.commitStore.ListRepos(r.Context())
	if err != nil {
		slog.Error("internal error", "op", "list_repos", "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		return
	}
	if repos == nil {
		repos = []model.Repo{}
	}
	writeJSON(w, http.StatusOK, model.ReposResponse{Repos: repos})
}

// handleGetRepo implements GET /repos/:name
func (s *server) handleGetRepo(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	repo, err := s.commitStore.GetRepo(r.Context(), name)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrRepoNotFound):
			writeAPIError(w, ErrCodeRepoNotFound, http.StatusNotFound, "repo not found")
		default:
			slog.Error("internal error", "op", "get_repo", "repo", name, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, repo)
}

// handleDeleteRepo implements DELETE /repos/:name (hard delete)
func (s *server) handleDeleteRepo(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	err := s.commitStore.DeleteRepo(r.Context(), name)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrRepoNotFound):
			writeAPIError(w, ErrCodeRepoNotFound, http.StatusNotFound, "repo not found")
		default:
			slog.Error("internal error", "op", "delete_repo", "repo", name, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	identity := IdentityFromContext(r.Context())
	s.emit(r.Context(), evtypes.RepoDeleted{Repo: name, DeletedBy: identity})
	slog.Info("repo deleted", "repo", name, "by", identity)
	w.WriteHeader(http.StatusNoContent)
}

// parseDayDuration parses a duration string of the form "Nd" (e.g. "7d", "30d").
// N must be a positive integer.
func parseDayDuration(s string) (time.Duration, error) {
	if len(s) < 2 || s[len(s)-1] != 'd' {
		return 0, fmt.Errorf("invalid duration %q: must be of the form \"Nd\" (e.g. \"7d\")", s)
	}
	n, err := strconv.ParseInt(s[:len(s)-1], 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid duration %q: must be a positive integer followed by 'd'", s)
	}
	return time.Duration(n) * 24 * time.Hour, nil
}

// handlePurge implements POST /repos/:name/purge
func (s *server) handlePurge(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	identity := IdentityFromContext(r.Context())

	var req model.PurgeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.OlderThan == "" {
		writeError(w, http.StatusBadRequest, "older_than is required")
		return
	}

	dur, err := parseDayDuration(req.OlderThan)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	result, err := s.commitStore.Purge(r.Context(), db.PurgeRequest{
		Repo:      repo,
		OlderThan: dur,
		DryRun:    req.DryRun,
	})
	if err != nil {
		switch {
		case errors.Is(err, db.ErrRepoNotFound):
			writeAPIError(w, ErrCodeRepoNotFound, http.StatusNotFound, "repo not found")
		default:
			slog.Error("internal error", "op", "purge", "repo", repo, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	slog.Info("purge complete", "repo", repo, "dry_run", req.DryRun, "branches_purged", result.BranchesPurged, "docs_deleted", result.DocumentsDeleted, "by", identity)
	writeJSON(w, http.StatusOK, model.PurgeResponse{
		BranchesPurged:     result.BranchesPurged,
		FileCommitsDeleted: result.FileCommitsDeleted,
		CommitsDeleted:     result.CommitsDeleted,
		DocumentsDeleted:   result.DocumentsDeleted,
		ReviewsDeleted:     result.ReviewsDeleted,
		CheckRunsDeleted:   result.CheckRunsDeleted,
	})
}
