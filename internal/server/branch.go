package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/dlorenc/docstore/internal/model"
)

// handleCreateBranch handles POST /branch.
// Creates a new branch forked from main's current head.
func (s *Server) handleCreateBranch(w http.ResponseWriter, r *http.Request) {
	var req model.CreateBranchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
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

	branch, err := s.store.CreateBranch(r.Context(), req.Name)
	if err != nil {
		if errors.Is(err, model.ErrBranchExists) {
			writeError(w, http.StatusConflict, "branch already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusCreated, model.CreateBranchResponse{
		Name:         branch.Name,
		BaseSequence: branch.BaseSequence,
	})
}

// handleDeleteBranch handles DELETE /branch/{name}.
// Marks a branch as abandoned.
func (s *Server) handleDeleteBranch(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "branch name is required")
		return
	}
	if name == "main" {
		writeError(w, http.StatusBadRequest, "cannot delete main branch")
		return
	}

	err := s.store.DeleteBranch(r.Context(), name)
	if err != nil {
		if errors.Is(err, model.ErrBranchNotFound) {
			writeError(w, http.StatusNotFound, "branch not found")
			return
		}
		if errors.Is(err, model.ErrBranchNotActive) {
			writeError(w, http.StatusBadRequest, "branch is not active")
			return
		}
		if errors.Is(err, model.ErrProtectedBranch) {
			writeError(w, http.StatusBadRequest, "cannot delete protected branch")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, model.DeleteBranchResponse{
		Name:   name,
		Status: model.BranchStatusAbandoned,
	})
}

// handleListBranches handles GET /branches.
// Returns all branches, optionally filtered by ?status=active.
func (s *Server) handleListBranches(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")

	branches, err := s.store.ListBranches(r.Context(), status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, model.BranchesResponse{Branches: branches})
}
