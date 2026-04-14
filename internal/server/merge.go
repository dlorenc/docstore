package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/dlorenc/docstore/internal/model"
)

// handleMerge handles POST /merge.
// Merges a branch into main per the DESIGN.md algorithm:
// 1. Lock both branches (SELECT FOR UPDATE)
// 2. Find branch changes since base_sequence
// 3. Find main changes since base_sequence
// 4. Detect conflicts (overlapping paths)
// 5. If clean, insert file_commits rows on main with new sequence
func (s *Server) handleMerge(w http.ResponseWriter, r *http.Request) {
	var req model.MergeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
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

	resp, conflicts, err := s.store.Merge(r.Context(), req.Branch)
	if err != nil {
		if errors.Is(err, model.ErrBranchNotFound) {
			writeError(w, http.StatusNotFound, "branch not found")
			return
		}
		if errors.Is(err, model.ErrBranchNotActive) {
			writeError(w, http.StatusBadRequest, "branch is not active")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if len(conflicts) > 0 {
		writeJSON(w, http.StatusConflict, model.MergeConflictError{Conflicts: conflicts})
		return
	}

	writeJSON(w, http.StatusOK, resp)
}
