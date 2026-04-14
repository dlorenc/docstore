package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/dlorenc/docstore/internal/model"
)

// handleRebase handles POST /rebase.
// Replays branch commits onto main's current head per DESIGN.md:
// 1. Lock both branches
// 2. Find branch changes grouped by original sequence
// 3. Check for conflicts with main changes since base_sequence
// 4. If clean, replay each group with new sequence numbers
// 5. Update branch's base_sequence to main's current head
func (s *Server) handleRebase(w http.ResponseWriter, r *http.Request) {
	var req model.RebaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
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

	resp, conflicts, err := s.store.Rebase(r.Context(), req.Branch)
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
		writeJSON(w, http.StatusConflict, model.RebaseConflictError{Conflicts: conflicts})
		return
	}

	writeJSON(w, http.StatusOK, resp)
}
