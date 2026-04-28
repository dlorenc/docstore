package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/dlorenc/docstore/internal/db"
	evtypes "github.com/dlorenc/docstore/internal/events/types"
	"github.com/dlorenc/docstore/internal/model"
)

// ---------------------------------------------------------------------------
// Org handlers
// ---------------------------------------------------------------------------

// handleCreateOrg implements POST /orgs
func (s *server) handleCreateOrg(w http.ResponseWriter, r *http.Request) {
	var req model.CreateOrgRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if strings.ContainsRune(req.Name, '/') {
		writeError(w, http.StatusBadRequest, "org name may not contain '/'")
		return
	}

	identity := IdentityFromContext(r.Context())
	org, err := s.svc.CreateOrg(r.Context(), identity, req.Name)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrOrgExists):
			writeAPIError(w, ErrCodeOrgExists, http.StatusConflict, "org already exists")
		default:
			slog.Error("internal error", "op", "create_org", "org", req.Name, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}
	writeJSON(w, http.StatusCreated, org)
}

// handleListOrgs implements GET /orgs
func (s *server) handleListOrgs(w http.ResponseWriter, r *http.Request) {
	orgs, err := s.commitStore.ListOrgs(r.Context())
	if err != nil {
		slog.Error("internal error", "op", "list_orgs", "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		return
	}
	if orgs == nil {
		orgs = []model.Org{}
	}
	writeJSON(w, http.StatusOK, model.ListOrgsResponse{Orgs: orgs})
}

// handleGetOrg implements GET /orgs/{org}
func (s *server) handleGetOrg(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("org")
	org, err := s.commitStore.GetOrg(r.Context(), name)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrOrgNotFound):
			writeAPIError(w, ErrCodeOrgNotFound, http.StatusNotFound, "org not found")
		default:
			slog.Error("internal error", "op", "get_org", "org", name, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, org)
}

// handleDeleteOrg implements DELETE /orgs/{org}
func (s *server) handleDeleteOrg(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("org")
	err := s.commitStore.DeleteOrg(r.Context(), name)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrOrgNotFound):
			writeAPIError(w, ErrCodeOrgNotFound, http.StatusNotFound, "org not found")
		case errors.Is(err, db.ErrOrgHasRepos):
			writeAPIError(w, ErrCodeConflict, http.StatusConflict, "org has repos; delete them first")
		default:
			slog.Error("internal error", "op", "delete_org", "org", name, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}
	s.emit(r.Context(), evtypes.OrgDeleted{Org: name, DeletedBy: IdentityFromContext(r.Context())})
	w.WriteHeader(http.StatusNoContent)
}

// handleListOrgRepos implements GET /orgs/{org}/repos
func (s *server) handleListOrgRepos(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("org")
	if _, err := s.commitStore.GetOrg(r.Context(), owner); err != nil {
		if errors.Is(err, db.ErrOrgNotFound) {
			writeAPIError(w, ErrCodeOrgNotFound, http.StatusNotFound, "org not found")
			return
		}
		slog.Error("internal error", "op", "list_org_repos", "org", owner, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		return
	}
	repos, err := s.commitStore.ListOrgRepos(r.Context(), owner)
	if err != nil {
		slog.Error("internal error", "op", "list_org_repos", "org", owner, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		return
	}
	if repos == nil {
		repos = []model.Repo{}
	}
	writeJSON(w, http.StatusOK, model.ReposResponse{Repos: repos})
}

// ---------------------------------------------------------------------------
// Org membership handlers
// ---------------------------------------------------------------------------

// requireOrgOwner checks that the current identity is an org owner. It writes
// a 403 and returns false if not.
func (s *server) requireOrgOwner(w http.ResponseWriter, r *http.Request, org string) bool {
	identity := IdentityFromContext(r.Context())
	m, err := s.commitStore.GetOrgMember(r.Context(), org, identity)
	if err != nil {
		if errors.Is(err, db.ErrOrgMemberNotFound) {
			writeAPIError(w, ErrCodeForbidden, http.StatusForbidden, "forbidden: not an org member")
			return false
		}
		slog.Error("internal error", "op", "require_org_owner", "org", org, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		return false
	}
	if m.Role != model.OrgRoleOwner {
		writeAPIError(w, ErrCodeForbidden, http.StatusForbidden, "forbidden: org owner required")
		return false
	}
	return true
}

// requireOrgMember checks that the current identity is at least an org member.
// It writes a 403 and returns false if not.
func (s *server) requireOrgMember(w http.ResponseWriter, r *http.Request, org string) bool {
	identity := IdentityFromContext(r.Context())
	_, err := s.commitStore.GetOrgMember(r.Context(), org, identity)
	if err != nil {
		if errors.Is(err, db.ErrOrgMemberNotFound) {
			writeAPIError(w, ErrCodeForbidden, http.StatusForbidden, "forbidden: not an org member")
			return false
		}
		slog.Error("internal error", "op", "require_org_member", "org", org, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		return false
	}
	return true
}

// handleListOrgMembers implements GET /orgs/{org}/members (org member+)
func (s *server) handleListOrgMembers(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	if _, err := s.commitStore.GetOrg(r.Context(), org); err != nil {
		if errors.Is(err, db.ErrOrgNotFound) {
			writeAPIError(w, ErrCodeOrgNotFound, http.StatusNotFound, "org not found")
			return
		}
		slog.Error("internal error", "op", "list_org_members", "org", org, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		return
	}
	if !s.requireOrgMember(w, r, org) {
		return
	}
	members, err := s.commitStore.ListOrgMembers(r.Context(), org)
	if err != nil {
		slog.Error("internal error", "op", "list_org_members", "org", org, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		return
	}
	if members == nil {
		members = []model.OrgMember{}
	}
	writeJSON(w, http.StatusOK, model.OrgMembersResponse{Members: members})
}

// handleAddOrgMember implements POST /orgs/{org}/members/{identity} (org owner only)
func (s *server) handleAddOrgMember(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	identity := r.PathValue("identity")

	if _, err := s.commitStore.GetOrg(r.Context(), org); err != nil {
		if errors.Is(err, db.ErrOrgNotFound) {
			writeAPIError(w, ErrCodeOrgNotFound, http.StatusNotFound, "org not found")
			return
		}
		slog.Error("internal error", "op", "add_org_member", "org", org, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		return
	}
	if !s.requireOrgOwner(w, r, org) {
		return
	}

	var req model.AddOrgMemberRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	switch req.Role {
	case model.OrgRoleOwner, model.OrgRoleMember:
		// valid
	default:
		writeError(w, http.StatusBadRequest, "invalid role; must be 'owner' or 'member'")
		return
	}

	invitedBy := IdentityFromContext(r.Context())
	if err := s.commitStore.AddOrgMember(r.Context(), org, identity, req.Role, invitedBy); err != nil {
		slog.Error("internal error", "op", "add_org_member", "org", org, "identity", identity, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		return
	}

	slog.Info("org member added", "org", org, "identity", identity, "role", req.Role, "by", invitedBy)
	writeJSON(w, http.StatusOK, model.OrgMember{Org: org, Identity: identity, Role: req.Role, InvitedBy: invitedBy})
}

// handleRemoveOrgMember implements DELETE /orgs/{org}/members/{identity} (org owner only)
func (s *server) handleRemoveOrgMember(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	identity := r.PathValue("identity")

	if _, err := s.commitStore.GetOrg(r.Context(), org); err != nil {
		if errors.Is(err, db.ErrOrgNotFound) {
			writeAPIError(w, ErrCodeOrgNotFound, http.StatusNotFound, "org not found")
			return
		}
		slog.Error("internal error", "op", "remove_org_member", "org", org, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		return
	}
	if !s.requireOrgOwner(w, r, org) {
		return
	}

	if err := s.commitStore.RemoveOrgMember(r.Context(), org, identity); err != nil {
		switch {
		case errors.Is(err, db.ErrOrgMemberNotFound):
			writeAPIError(w, ErrCodeNotFound, http.StatusNotFound, "member not found")
		default:
			slog.Error("internal error", "op", "remove_org_member", "org", org, "identity", identity, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	slog.Info("org member removed", "org", org, "identity", identity, "by", IdentityFromContext(r.Context()))
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Org invite handlers
// ---------------------------------------------------------------------------

// generateToken returns a 32-byte random hex string for use as an invite token.
func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// handleCreateInvite implements POST /orgs/{org}/invites (org owner only)
// Body: {email, role}. Generates an opaque token, expires in 7 days.
// Returns {id, token}.
func (s *server) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")

	if _, err := s.commitStore.GetOrg(r.Context(), org); err != nil {
		if errors.Is(err, db.ErrOrgNotFound) {
			writeAPIError(w, ErrCodeOrgNotFound, http.StatusNotFound, "org not found")
			return
		}
		slog.Error("internal error", "op", "create_invite", "org", org, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		return
	}
	if !s.requireOrgOwner(w, r, org) {
		return
	}

	var req model.CreateInviteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Email == "" {
		writeError(w, http.StatusBadRequest, "email is required")
		return
	}
	switch req.Role {
	case model.OrgRoleOwner, model.OrgRoleMember:
		// valid
	default:
		writeError(w, http.StatusBadRequest, "invalid role; must be 'owner' or 'member'")
		return
	}

	token, err := generateToken()
	if err != nil {
		slog.Error("internal error", "op", "create_invite", "org", org, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		return
	}

	invitedBy := IdentityFromContext(r.Context())
	expiresAt := time.Now().Add(7 * 24 * time.Hour)

	inv, err := s.commitStore.CreateInvite(r.Context(), org, req.Email, req.Role, invitedBy, token, expiresAt)
	if err != nil {
		slog.Error("internal error", "op", "create_invite", "org", org, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		return
	}

	slog.Info("org invite created", "org", org, "email", req.Email, "role", req.Role, "by", invitedBy)
	writeJSON(w, http.StatusCreated, model.CreateInviteResponse{ID: inv.ID, Token: inv.Token})
}

// handleListInvites implements GET /orgs/{org}/invites (org owner only)
// Returns pending (not accepted, not expired) invites.
func (s *server) handleListInvites(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")

	if _, err := s.commitStore.GetOrg(r.Context(), org); err != nil {
		if errors.Is(err, db.ErrOrgNotFound) {
			writeAPIError(w, ErrCodeOrgNotFound, http.StatusNotFound, "org not found")
			return
		}
		slog.Error("internal error", "op", "list_invites", "org", org, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		return
	}
	if !s.requireOrgOwner(w, r, org) {
		return
	}

	invites, err := s.commitStore.ListInvites(r.Context(), org)
	if err != nil {
		slog.Error("internal error", "op", "list_invites", "org", org, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		return
	}
	if invites == nil {
		invites = []model.OrgInvite{}
	}
	writeJSON(w, http.StatusOK, model.OrgInvitesResponse{Invites: invites})
}

// handleAcceptInvite implements POST /orgs/{org}/invites/{token}/accept
// The IAP identity must match the invite email.
func (s *server) handleAcceptInvite(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	token := r.PathValue("token")
	identity := IdentityFromContext(r.Context())

	if err := s.commitStore.AcceptInvite(r.Context(), org, token, identity); err != nil {
		switch {
		case errors.Is(err, db.ErrInviteNotFound):
			writeAPIError(w, ErrCodeInviteNotFound, http.StatusNotFound, "invite not found")
		case errors.Is(err, db.ErrInviteExpired):
			writeAPIError(w, ErrCodeGone, http.StatusGone, "invite expired")
		case errors.Is(err, db.ErrInviteAlreadyAccepted):
			writeAPIError(w, ErrCodeConflict, http.StatusConflict, "invite already accepted")
		case errors.Is(err, db.ErrEmailMismatch):
			writeAPIError(w, ErrCodeForbidden, http.StatusForbidden, "identity does not match invite email")
		default:
			slog.Error("internal error", "op", "accept_invite", "org", org, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	slog.Info("org invite accepted", "org", org, "identity", identity)
	w.WriteHeader(http.StatusNoContent)
}

// handleRevokeInvite implements DELETE /orgs/{org}/invites/{id} (org owner only)
func (s *server) handleRevokeInvite(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	inviteID := r.PathValue("id")

	if _, err := s.commitStore.GetOrg(r.Context(), org); err != nil {
		if errors.Is(err, db.ErrOrgNotFound) {
			writeAPIError(w, ErrCodeOrgNotFound, http.StatusNotFound, "org not found")
			return
		}
		slog.Error("internal error", "op", "revoke_invite", "org", org, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		return
	}
	if !s.requireOrgOwner(w, r, org) {
		return
	}

	if err := s.commitStore.RevokeInvite(r.Context(), org, inviteID); err != nil {
		switch {
		case errors.Is(err, db.ErrInviteNotFound):
			writeAPIError(w, ErrCodeInviteNotFound, http.StatusNotFound, "invite not found")
		default:
			slog.Error("internal error", "op", "revoke_invite", "org", org, "invite_id", inviteID, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	slog.Info("org invite revoked", "org", org, "invite_id", inviteID, "by", IdentityFromContext(r.Context()))
	w.WriteHeader(http.StatusNoContent)
}

