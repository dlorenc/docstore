package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/store"
)

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// handleTree implements GET /tree?branch=main&at=N&limit=N&after=cursor
func (s *server) handleTree(w http.ResponseWriter, r *http.Request) {
	branch := r.URL.Query().Get("branch")
	if branch == "" {
		branch = "main"
	}

	var atSequence *int64
	if v := r.URL.Query().Get("at"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid 'at' parameter")
			return
		}
		atSequence = &n
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

	afterPath := r.URL.Query().Get("after")

	entries, err := s.readStore.MaterializeTree(r.Context(), branch, atSequence, limit, afterPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if entries == nil {
		entries = []store.TreeEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}

// handleFile implements:
//   - GET /file/{path...}          → file content
//   - GET /file/{path...}/history  → file change history
//
// Query params: branch (default "main"), at (sequence), limit, after (cursor).
func (s *server) handleFile(w http.ResponseWriter, r *http.Request) {
	fullPath := r.PathValue("path")

	// Check for /history suffix.
	if strings.HasSuffix(fullPath, "/history") {
		filePath := strings.TrimSuffix(fullPath, "/history")
		s.handleFileHistory(w, r, filePath)
		return
	}

	s.handleFileContent(w, r, fullPath)
}

func (s *server) handleFileContent(w http.ResponseWriter, r *http.Request, path string) {
	branch := r.URL.Query().Get("branch")
	if branch == "" {
		branch = "main"
	}

	var atSequence *int64
	if v := r.URL.Query().Get("at"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid 'at' parameter")
			return
		}
		atSequence = &n
	}

	fc, err := s.readStore.GetFile(r.Context(), branch, path, atSequence)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if fc == nil {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}

	writeJSON(w, http.StatusOK, fc)
}

func (s *server) handleFileHistory(w http.ResponseWriter, r *http.Request, path string) {
	branch := r.URL.Query().Get("branch")
	if branch == "" {
		branch = "main"
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

	var afterSeq *int64
	if v := r.URL.Query().Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid 'after' parameter")
			return
		}
		afterSeq = &n
	}

	entries, err := s.readStore.GetFileHistory(r.Context(), branch, path, limit, afterSeq)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if entries == nil {
		entries = []store.FileHistoryEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}

// handleGetCommit implements GET /commit/{sequence}
func (s *server) handleGetCommit(w http.ResponseWriter, r *http.Request) {
	seqStr := r.PathValue("sequence")
	seq, err := strconv.ParseInt(seqStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid sequence number")
		return
	}

	detail, err := s.readStore.GetCommit(r.Context(), seq)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if detail == nil {
		writeError(w, http.StatusNotFound, "commit not found")
		return
	}

	writeJSON(w, http.StatusOK, detail)
}

// handleBranches implements GET /branches?status=active
func (s *server) handleBranches(w http.ResponseWriter, r *http.Request) {
	statusFilter := r.URL.Query().Get("status")

	branches, err := s.readStore.ListBranches(r.Context(), statusFilter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if branches == nil {
		branches = []store.BranchInfo{}
	}
	writeJSON(w, http.StatusOK, branches)
}

// handleDiff implements GET /diff?branch=X
func (s *server) handleDiff(w http.ResponseWriter, r *http.Request) {
	branch := r.URL.Query().Get("branch")
	if branch == "" {
		writeError(w, http.StatusBadRequest, "branch parameter is required")
		return
	}

	result, err := s.readStore.GetDiff(r.Context(), branch)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if result == nil {
		writeError(w, http.StatusNotFound, "branch not found")
		return
	}

	// Convert to API response type.
	resp := model.DiffResponse{
		BranchChanges: make([]model.DiffEntry, len(result.BranchChanges)),
		MainChanges:   make([]model.DiffEntry, len(result.MainChanges)),
	}
	for i, e := range result.BranchChanges {
		resp.BranchChanges[i] = model.DiffEntry{Path: e.Path, VersionID: e.VersionID}
	}
	for i, e := range result.MainChanges {
		resp.MainChanges[i] = model.DiffEntry{Path: e.Path, VersionID: e.VersionID}
	}
	for _, c := range result.Conflicts {
		resp.Conflicts = append(resp.Conflicts, model.ConflictEntry{
			Path:            c.Path,
			MainVersionID:   c.MainVersionID,
			BranchVersionID: c.BranchVersionID,
		})
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleCreateBranch implements POST /branch
func (s *server) handleCreateBranch(w http.ResponseWriter, r *http.Request) {
	var req model.CreateBranchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Name == "main" {
		writeError(w, http.StatusBadRequest, "cannot create branch named 'main'")
		return
	}

	resp, err := s.commitStore.CreateBranch(r.Context(), req)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrBranchExists):
			writeError(w, http.StatusConflict, "branch already exists")
		default:
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	writeJSON(w, http.StatusCreated, resp)
}

// handleDeleteBranch implements DELETE /branch/{name}
func (s *server) handleDeleteBranch(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "main" {
		writeError(w, http.StatusBadRequest, "cannot delete branch 'main'")
		return
	}

	err := s.commitStore.DeleteBranch(r.Context(), name)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrBranchNotFound):
			writeError(w, http.StatusNotFound, "branch not found")
		case errors.Is(err, db.ErrBranchNotActive):
			writeError(w, http.StatusConflict, "branch is already merged or abandoned")
		default:
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleRebase implements POST /rebase
func (s *server) handleRebase(w http.ResponseWriter, r *http.Request) {
	var req model.RebaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Branch == "" {
		writeError(w, http.StatusBadRequest, "branch is required")
		return
	}
	if req.Branch == "main" {
		writeError(w, http.StatusBadRequest, "cannot rebase main")
		return
	}

	// Author always comes from the authenticated identity; body value is ignored.
	req.Author = IdentityFromContext(r.Context())

	resp, conflicts, err := s.commitStore.Rebase(r.Context(), req)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrRebaseConflict):
			apiConflicts := make([]model.ConflictEntry, len(conflicts))
			for i, c := range conflicts {
				apiConflicts[i] = model.ConflictEntry{
					Path:            c.Path,
					MainVersionID:   c.MainVersionID,
					BranchVersionID: c.BranchVersionID,
				}
			}
			writeJSON(w, http.StatusConflict, model.RebaseConflictError{Conflicts: apiConflicts})
		case errors.Is(err, db.ErrBranchNotFound):
			writeError(w, http.StatusNotFound, "branch not found")
		case errors.Is(err, db.ErrBranchNotActive):
			writeError(w, http.StatusBadRequest, "branch is not active")
		default:
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleMerge implements POST /merge
func (s *server) handleMerge(w http.ResponseWriter, r *http.Request) {
	var req model.MergeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Branch == "" {
		writeError(w, http.StatusBadRequest, "branch is required")
		return
	}
	if req.Branch == "main" {
		writeError(w, http.StatusBadRequest, "cannot merge main into itself")
		return
	}

	// Author always comes from the authenticated identity; body value is ignored.
	req.Author = IdentityFromContext(r.Context())

	resp, conflicts, err := s.commitStore.Merge(r.Context(), req)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrMergeConflict):
			// Convert conflicts to API response.
			apiConflicts := make([]model.ConflictEntry, len(conflicts))
			for i, c := range conflicts {
				apiConflicts[i] = model.ConflictEntry{
					Path:            c.Path,
					MainVersionID:   c.MainVersionID,
					BranchVersionID: c.BranchVersionID,
				}
			}
			writeJSON(w, http.StatusConflict, model.MergeConflictError{Conflicts: apiConflicts})
		case errors.Is(err, db.ErrBranchNotFound):
			writeError(w, http.StatusNotFound, "branch not found")
		case errors.Is(err, db.ErrBranchNotActive):
			writeError(w, http.StatusConflict, "branch is not active")
		default:
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	writeJSON(w, http.StatusOK, resp)
}
