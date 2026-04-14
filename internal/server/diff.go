package server

import (
	"errors"
	"net/http"

	"github.com/dlorenc/docstore/internal/model"
)

// handleDiff handles GET /diff?branch=X.
// Returns files changed on the branch relative to its base_sequence,
// plus any conflicting paths on main since that base.
func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	branch := r.URL.Query().Get("branch")
	if branch == "" {
		writeError(w, http.StatusBadRequest, "branch query parameter is required")
		return
	}

	result, err := s.store.Diff(r.Context(), branch)
	if err != nil {
		if errors.Is(err, model.ErrBranchNotFound) {
			writeError(w, http.StatusNotFound, "branch not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, result)
}
