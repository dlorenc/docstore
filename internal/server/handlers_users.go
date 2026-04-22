package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/dlorenc/docstore/internal/db"
	evtypes "github.com/dlorenc/docstore/internal/events/types"
	"github.com/dlorenc/docstore/internal/model"
)

// ---------------------------------------------------------------------------
// Role (user) handlers
// ---------------------------------------------------------------------------

// handleListRoles implements GET /repos/:name/roles (admin only — enforced by RBAC middleware)
func (s *server) handleListRoles(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	roles, err := s.commitStore.ListRoles(r.Context(), repo)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if roles == nil {
		roles = []model.Role{}
	}
	writeJSON(w, http.StatusOK, model.RolesResponse{Roles: roles})
}

// handleSetRole implements PUT /repos/:name/roles/:identity (admin only — enforced by RBAC middleware)
func (s *server) handleSetRole(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	identity := r.PathValue("identity")

	if identity == "" {
		writeError(w, http.StatusBadRequest, "identity is required")
		return
	}

	var req model.SetRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	switch req.Role {
	case model.RoleReader, model.RoleWriter, model.RoleMaintainer, model.RoleAdmin:
		// valid
	default:
		writeError(w, http.StatusBadRequest, "invalid role; must be reader, writer, maintainer, or admin")
		return
	}

	if err := s.commitStore.SetRole(r.Context(), repo, identity, req.Role); err != nil {
		slog.Error("internal error", "op", "set_role", "repo", repo, "identity", identity, "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	by := IdentityFromContext(r.Context())
	s.emit(r.Context(), evtypes.RoleChanged{
		Repo:      repo,
		Identity:  identity,
		Role:      string(req.Role),
		ChangedBy: by,
	})
	slog.Info("role assigned", "repo", repo, "identity", identity, "role", req.Role, "by", by)
	writeJSON(w, http.StatusOK, model.Role{Identity: identity, Role: req.Role})
}

// handleDeleteRole implements DELETE /repos/:name/roles/:identity (admin only — enforced by RBAC middleware)
func (s *server) handleDeleteRole(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	identity := r.PathValue("identity")

	if identity == "" {
		writeError(w, http.StatusBadRequest, "identity is required")
		return
	}

	if err := s.commitStore.DeleteRole(r.Context(), repo, identity); err != nil {
		switch {
		case errors.Is(err, db.ErrRoleNotFound):
			writeError(w, http.StatusNotFound, "role not found")
		default:
			slog.Error("internal error", "op", "delete_role", "repo", repo, "identity", identity, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	by := IdentityFromContext(r.Context())
	s.emit(r.Context(), evtypes.RoleChanged{
		Repo:      repo,
		Identity:  identity,
		Role:      "", // empty means removed
		ChangedBy: by,
	})
	slog.Info("role removed", "repo", repo, "identity", identity, "by", by)
	w.WriteHeader(http.StatusNoContent)
}
