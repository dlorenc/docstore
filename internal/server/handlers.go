package server

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/events"
	evtypes "github.com/dlorenc/docstore/internal/events/types"
	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/policy"
	"github.com/dlorenc/docstore/internal/store"
)

// parseRepoPath parses a /repos/... URL path into the full repo name and the
// endpoint string that follows the "/-/" separator.
//
//	/repos/acme/myrepo/-/tree        → ("acme/myrepo", "tree", true)
//	/repos/acme/team/sub/-/commit    → ("acme/team/sub", "commit", true)
//	/repos/acme/myrepo               → ("acme/myrepo", "", true)  // bare repo
//	/something/else                  → ("", "", false)
//
// NOTE: repoAndSubPath in middleware.go parses the same "/-/" URL format for
// RBAC purposes. Both functions must be kept in sync if the URL structure changes.
func parseRepoPath(path string) (repoName, endpoint string, ok bool) {
	const prefix = "/repos/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	rest := path[len(prefix):]
	if rest == "" {
		return "", "", false
	}
	idx := strings.Index(rest, "/-/")
	if idx == -1 {
		// Bare /repos/:repopath — no endpoint
		return rest, "", true
	}
	return rest[:idx], rest[idx+3:], true
}

// handleReposPrefix is the catch-all handler for all /repos/... paths (except
// the bare GET /repos and POST /repos which are registered as exact matches).
// It parses the repo name and endpoint from the URL using the "/-/" separator,
// sets the "name" path value so existing sub-handlers work unchanged, and
// dispatches to the appropriate handler.
func (s *server) handleReposPrefix(w http.ResponseWriter, r *http.Request) {
	repoName, endpoint, ok := parseRepoPath(r.URL.Path)
	if !ok || repoName == "" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	r.SetPathValue("name", repoName)

	if endpoint == "" {
		// Bare /repos/:reponame
		switch r.Method {
		case http.MethodGet:
			s.handleGetRepo(w, r)
		case http.MethodDelete:
			s.handleDeleteRepo(w, r)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}

	switch {
	case endpoint == "tree":
		if r.Method == http.MethodGet {
			s.handleTree(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "archive":
		if r.Method == http.MethodGet {
			s.handleArchive(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "branches":
		if r.Method == http.MethodGet {
			s.handleBranches(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "diff":
		if r.Method == http.MethodGet {
			s.handleDiff(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "commit":
		if r.Method == http.MethodPost {
			s.handleCommit(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "branch":
		if r.Method == http.MethodPost {
			s.handleCreateBranch(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "merge":
		if r.Method == http.MethodPost {
			s.handleMerge(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "rebase":
		if r.Method == http.MethodPost {
			s.handleRebase(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "review":
		if r.Method == http.MethodPost {
			s.handleReview(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "check":
		if r.Method == http.MethodPost {
			s.handleCheck(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "comment":
		if r.Method == http.MethodPost {
			s.handleCreateReviewComment(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case strings.HasPrefix(endpoint, "comment/"):
		commentID := strings.TrimPrefix(endpoint, "comment/")
		r.SetPathValue("commentID", commentID)
		if r.Method == http.MethodDelete {
			s.handleDeleteReviewComment(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "purge":
		if r.Method == http.MethodPost {
			s.handlePurge(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "roles":
		if r.Method == http.MethodGet {
			s.handleListRoles(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "chain":
		if r.Method == http.MethodGet {
			s.handleChain(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "releases":
		switch r.Method {
		case http.MethodGet:
			s.handleListReleases(w, r)
		case http.MethodPost:
			s.handleCreateRelease(w, r)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case strings.HasPrefix(endpoint, "releases/"):
		releaseName := strings.TrimPrefix(endpoint, "releases/")
		r.SetPathValue("release", releaseName)
		switch r.Method {
		case http.MethodGet:
			s.handleGetRelease(w, r)
		case http.MethodDelete:
			s.handleDeleteRelease(w, r)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case strings.HasPrefix(endpoint, "commit/"):
		rest := strings.TrimPrefix(endpoint, "commit/")
		if strings.HasSuffix(rest, "/issues") {
			r.SetPathValue("sequence", strings.TrimSuffix(rest, "/issues"))
			if r.Method == http.MethodGet {
				s.handleCommitIssues(w, r)
			} else {
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			}
		} else {
			if r.Method == http.MethodGet {
				r.SetPathValue("sequence", rest)
				s.handleGetCommit(w, r)
			} else {
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			}
		}

	case strings.HasPrefix(endpoint, "file/"):
		if r.Method == http.MethodGet {
			r.SetPathValue("path", strings.TrimPrefix(endpoint, "file/"))
			s.handleFile(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case strings.HasPrefix(endpoint, "roles/"):
		identity := strings.TrimPrefix(endpoint, "roles/")
		r.SetPathValue("identity", identity)
		switch r.Method {
		case http.MethodPut:
			s.handleSetRole(w, r)
		case http.MethodDelete:
			s.handleDeleteRole(w, r)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case strings.HasPrefix(endpoint, "branch/"):
		bpath := strings.TrimPrefix(endpoint, "branch/")
		switch r.Method {
		case http.MethodDelete:
			r.SetPathValue("bname", bpath)
			s.handleDeleteBranch(w, r)
		case http.MethodPatch:
			r.SetPathValue("bname", bpath)
			s.handleUpdateBranch(w, r)
		case http.MethodGet:
			if strings.HasSuffix(bpath, "/status") {
				branchName := strings.TrimSuffix(bpath, "/status")
				s.handleBranchStatus(w, r, repoName, branchName)
			} else {
				r.SetPathValue("branch", bpath)
				s.handleBranchGet(w, r)
			}
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "events":
		if r.Method == http.MethodGet {
			s.handleSSERepoEvents(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "proposals":
		switch r.Method {
		case http.MethodPost:
			s.handleCreateProposal(w, r)
		case http.MethodGet:
			s.handleListProposals(w, r)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case strings.HasPrefix(endpoint, "proposals/"):
		rest := strings.TrimPrefix(endpoint, "proposals/")
		// Check for /proposals/:id/close
		if strings.HasSuffix(rest, "/close") {
			proposalID := strings.TrimSuffix(rest, "/close")
			r.SetPathValue("proposalID", proposalID)
			if r.Method == http.MethodPost {
				s.handleCloseProposal(w, r)
			} else {
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			}
		} else if strings.HasSuffix(rest, "/issues") {
			proposalID := strings.TrimSuffix(rest, "/issues")
			r.SetPathValue("proposalID", proposalID)
			if r.Method == http.MethodGet {
				s.handleProposalIssues(w, r)
			} else {
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			}
		} else {
			r.SetPathValue("proposalID", rest)
			switch r.Method {
			case http.MethodGet:
				s.handleGetProposal(w, r)
			case http.MethodPatch:
				s.handleUpdateProposal(w, r)
			default:
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			}
		}

	case endpoint == "issues":
		switch r.Method {
		case http.MethodGet:
			s.handleListIssues(w, r)
		case http.MethodPost:
			s.handleCreateIssue(w, r)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case strings.HasPrefix(endpoint, "issues/"):
		rest := strings.TrimPrefix(endpoint, "issues/")
		parts := strings.SplitN(rest, "/", 3)
		r.SetPathValue("number", parts[0])
		switch len(parts) {
		case 1:
			// issues/{number}
			switch r.Method {
			case http.MethodGet:
				s.handleGetIssue(w, r)
			case http.MethodPatch:
				s.handleUpdateIssue(w, r)
			default:
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			}
		case 2:
			switch parts[1] {
			case "close":
				if r.Method == http.MethodPost {
					s.handleCloseIssue(w, r)
				} else {
					writeError(w, http.StatusMethodNotAllowed, "method not allowed")
				}
			case "reopen":
				if r.Method == http.MethodPost {
					s.handleReopenIssue(w, r)
				} else {
					writeError(w, http.StatusMethodNotAllowed, "method not allowed")
				}
			case "comments":
				switch r.Method {
				case http.MethodGet:
					s.handleListIssueComments(w, r)
				case http.MethodPost:
					s.handleCreateIssueComment(w, r)
				default:
					writeError(w, http.StatusMethodNotAllowed, "method not allowed")
				}
			case "refs":
				switch r.Method {
				case http.MethodGet:
					s.handleListIssueRefs(w, r)
				case http.MethodPost:
					s.handleAddIssueRef(w, r)
				default:
					writeError(w, http.StatusMethodNotAllowed, "method not allowed")
				}
			default:
				writeError(w, http.StatusNotFound, "not found")
			}
		case 3:
			if parts[1] == "comments" {
				r.SetPathValue("commentID", parts[2])
				switch r.Method {
				case http.MethodPatch:
					s.handleUpdateIssueComment(w, r)
				case http.MethodDelete:
					s.handleDeleteIssueComment(w, r)
				default:
					writeError(w, http.StatusMethodNotAllowed, "method not allowed")
				}
			} else {
				writeError(w, http.StatusNotFound, "not found")
			}
		default:
			writeError(w, http.StatusNotFound, "not found")
		}

	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

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
	org, err := s.commitStore.CreateOrg(r.Context(), req.Name, identity)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrOrgExists):
			writeError(w, http.StatusConflict, "org already exists")
		default:
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}
	s.emit(r.Context(), evtypes.OrgCreated{Org: org.Name, CreatedBy: identity})
	writeJSON(w, http.StatusCreated, org)
}

// handleListOrgs implements GET /orgs
func (s *server) handleListOrgs(w http.ResponseWriter, r *http.Request) {
	orgs, err := s.commitStore.ListOrgs(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
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
			writeError(w, http.StatusNotFound, "org not found")
		default:
			writeError(w, http.StatusInternalServerError, "query failed")
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
			writeError(w, http.StatusNotFound, "org not found")
		case errors.Is(err, db.ErrOrgHasRepos):
			writeError(w, http.StatusConflict, "org has repos; delete them first")
		default:
			writeError(w, http.StatusInternalServerError, "internal server error")
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
			writeError(w, http.StatusNotFound, "org not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	repos, err := s.commitStore.ListOrgRepos(r.Context(), owner)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
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
			writeError(w, http.StatusForbidden, "forbidden: not an org member")
			return false
		}
		writeError(w, http.StatusInternalServerError, "query failed")
		return false
	}
	if m.Role != model.OrgRoleOwner {
		writeError(w, http.StatusForbidden, "forbidden: org owner required")
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
			writeError(w, http.StatusForbidden, "forbidden: not an org member")
			return false
		}
		writeError(w, http.StatusInternalServerError, "query failed")
		return false
	}
	return true
}

// handleListOrgMembers implements GET /orgs/{org}/members (org member+)
func (s *server) handleListOrgMembers(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	if _, err := s.commitStore.GetOrg(r.Context(), org); err != nil {
		if errors.Is(err, db.ErrOrgNotFound) {
			writeError(w, http.StatusNotFound, "org not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if !s.requireOrgMember(w, r, org) {
		return
	}
	members, err := s.commitStore.ListOrgMembers(r.Context(), org)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
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
			writeError(w, http.StatusNotFound, "org not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "query failed")
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
		writeError(w, http.StatusInternalServerError, "internal server error")
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
			writeError(w, http.StatusNotFound, "org not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if !s.requireOrgOwner(w, r, org) {
		return
	}

	if err := s.commitStore.RemoveOrgMember(r.Context(), org, identity); err != nil {
		switch {
		case errors.Is(err, db.ErrOrgMemberNotFound):
			writeError(w, http.StatusNotFound, "member not found")
		default:
			slog.Error("internal error", "op", "remove_org_member", "org", org, "identity", identity, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
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
			writeError(w, http.StatusNotFound, "org not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "query failed")
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
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	invitedBy := IdentityFromContext(r.Context())
	expiresAt := time.Now().Add(7 * 24 * time.Hour)

	inv, err := s.commitStore.CreateInvite(r.Context(), org, req.Email, req.Role, invitedBy, token, expiresAt)
	if err != nil {
		slog.Error("internal error", "op", "create_invite", "org", org, "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
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
			writeError(w, http.StatusNotFound, "org not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if !s.requireOrgOwner(w, r, org) {
		return
	}

	invites, err := s.commitStore.ListInvites(r.Context(), org)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
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
			writeError(w, http.StatusNotFound, "invite not found")
		case errors.Is(err, db.ErrInviteExpired):
			writeError(w, http.StatusGone, "invite expired")
		case errors.Is(err, db.ErrInviteAlreadyAccepted):
			writeError(w, http.StatusConflict, "invite already accepted")
		case errors.Is(err, db.ErrEmailMismatch):
			writeError(w, http.StatusForbidden, "identity does not match invite email")
		default:
			slog.Error("internal error", "op", "accept_invite", "org", org, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
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
			writeError(w, http.StatusNotFound, "org not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if !s.requireOrgOwner(w, r, org) {
		return
	}

	if err := s.commitStore.RevokeInvite(r.Context(), org, inviteID); err != nil {
		switch {
		case errors.Is(err, db.ErrInviteNotFound):
			writeError(w, http.StatusNotFound, "invite not found")
		default:
			slog.Error("internal error", "op", "revoke_invite", "org", org, "invite_id", inviteID, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	slog.Info("org invite revoked", "org", org, "invite_id", inviteID, "by", IdentityFromContext(r.Context()))
	w.WriteHeader(http.StatusNoContent)
}

// handleReview implements POST /repos/:name/review
func (s *server) handleReview(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	reviewer := IdentityFromContext(r.Context())

	var req model.CreateReviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Branch == "" {
		writeError(w, http.StatusBadRequest, "branch is required")
		return
	}
	if req.Status == "" {
		writeError(w, http.StatusBadRequest, "status is required")
		return
	}

	review, err := s.commitStore.CreateReview(r.Context(), repo, req.Branch, reviewer, req.Status, req.Body)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrBranchNotFound):
			writeError(w, http.StatusNotFound, "branch not found")
		case errors.Is(err, db.ErrSelfApproval):
			writeError(w, http.StatusForbidden, "reviewer cannot approve their own commits")
		default:
			slog.Error("internal error", "op", "review", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	s.emit(r.Context(), evtypes.ReviewSubmitted{
		Repo:     repo,
		Branch:   req.Branch,
		Sequence: review.Sequence,
		Reviewer: reviewer,
		Status:   string(req.Status),
	})
	slog.Info("review submitted", "repo", repo, "branch", req.Branch, "reviewer", reviewer, "status", req.Status)
	writeJSON(w, http.StatusCreated, model.CreateReviewResponse{
		ID:       review.ID,
		Sequence: review.Sequence,
	})
}

// handleCheck implements POST /repos/:name/check
func (s *server) handleCheck(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	reporter := IdentityFromContext(r.Context())

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

	cr, err := s.commitStore.CreateCheckRun(r.Context(), repo, req.Branch, req.CheckName, req.Status, reporter, req.LogURL, req.Sequence)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrBranchNotFound):
			writeError(w, http.StatusNotFound, "branch not found")
		default:
			slog.Error("internal error", "op", "check", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	s.emit(r.Context(), evtypes.CheckReported{
		Repo:      repo,
		Branch:    req.Branch,
		Sequence:  cr.Sequence,
		CheckName: req.CheckName,
		Status:    string(req.Status),
		Reporter:  reporter,
	})
	writeJSON(w, http.StatusCreated, model.CreateCheckRunResponse{
		ID:       cr.ID,
		Sequence: cr.Sequence,
		LogURL:   req.LogURL,
	})
}

// handleBranchGet dispatches GET /repos/:name/branch/{branch...} to the
// appropriate sub-resource handler based on the path suffix.
// Branch names may contain slashes (e.g. "feature/x"), so we use a trailing
// wildcard and strip the sub-resource suffix manually — the same technique
// used by handleFile for the "/history" suffix.
func (s *server) handleBranchGet(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	branchPath := r.PathValue("branch")

	switch {
	case strings.HasSuffix(branchPath, "/reviews"):
		branch := strings.TrimSuffix(branchPath, "/reviews")
		s.handleGetReviews(w, r, repo, branch)
	case strings.HasSuffix(branchPath, "/checks"):
		branch := strings.TrimSuffix(branchPath, "/checks")
		s.handleGetChecks(w, r, repo, branch)
	case strings.HasSuffix(branchPath, "/comments"):
		branch := strings.TrimSuffix(branchPath, "/comments")
		s.handleListReviewComments(w, r, repo, branch)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

// handleGetReviews serves GET /repos/:name/branch/:branch/reviews
func (s *server) handleGetReviews(w http.ResponseWriter, r *http.Request, repo, branch string) {
	var atSeq *int64
	if v := r.URL.Query().Get("at"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid 'at' parameter")
			return
		}
		atSeq = &n
	}

	reviews, err := s.commitStore.ListReviews(r.Context(), repo, branch, atSeq)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if reviews == nil {
		reviews = []model.Review{}
	}
	writeJSON(w, http.StatusOK, reviews)
}

// handleGetChecks serves GET /repos/:name/branch/:branch/checks
func (s *server) handleGetChecks(w http.ResponseWriter, r *http.Request, repo, branch string) {
	var atSeq *int64
	if v := r.URL.Query().Get("at"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid 'at' parameter")
			return
		}
		atSeq = &n
	}

	checkRuns, err := s.commitStore.ListCheckRuns(r.Context(), repo, branch, atSeq)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if checkRuns == nil {
		checkRuns = []model.CheckRun{}
	}
	writeJSON(w, http.StatusOK, checkRuns)
}

// handleCreateReviewComment implements POST /repos/:name/comment
func (s *server) handleCreateReviewComment(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}
	author := IdentityFromContext(r.Context())

	var req model.CreateReviewCommentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Branch == "" {
		writeError(w, http.StatusBadRequest, "branch is required")
		return
	}
	if req.Path == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	if req.VersionID == "" {
		writeError(w, http.StatusBadRequest, "version_id is required")
		return
	}
	if req.Body == "" {
		writeError(w, http.StatusBadRequest, "body is required")
		return
	}

	comment, err := s.commitStore.CreateReviewComment(r.Context(), repo, req.Branch, req.Path, req.VersionID, req.Body, author, req.ReviewID)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrBranchNotFound):
			writeError(w, http.StatusNotFound, "branch not found")
		default:
			slog.Error("internal error", "op", "create_review_comment", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	slog.Info("review comment created", "repo", repo, "branch", req.Branch, "path", req.Path, "author", author)
	writeJSON(w, http.StatusCreated, model.CreateReviewCommentResponse{
		ID:       comment.ID,
		Sequence: comment.Sequence,
	})
}

// handleListReviewComments serves GET /repos/:name/branch/:branch/comments
func (s *server) handleListReviewComments(w http.ResponseWriter, r *http.Request, repo, branch string) {
	var path *string
	if v := r.URL.Query().Get("path"); v != "" {
		path = &v
	}

	comments, err := s.commitStore.ListReviewComments(r.Context(), repo, branch, path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if comments == nil {
		comments = []model.ReviewComment{}
	}
	writeJSON(w, http.StatusOK, comments)
}

// handleDeleteReviewComment implements DELETE /repos/:name/comment/:commentID
func (s *server) handleDeleteReviewComment(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}
	commentID := r.PathValue("commentID")
	identity := IdentityFromContext(r.Context())
	role := RoleFromContext(r.Context())

	comment, err := s.commitStore.GetReviewComment(r.Context(), repo, commentID)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrCommentNotFound):
			writeError(w, http.StatusNotFound, "comment not found")
		default:
			slog.Error("internal error", "op", "get_review_comment", "repo", repo, "comment", commentID, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	// Only the comment author or a maintainer+ may delete a comment.
	if comment.Author != identity && role != model.RoleMaintainer && role != model.RoleAdmin {
		writeError(w, http.StatusForbidden, "forbidden: must be comment author or maintainer")
		return
	}

	if err := s.commitStore.DeleteReviewComment(r.Context(), repo, commentID); err != nil {
		slog.Error("internal error", "op", "delete_review_comment", "repo", repo, "comment", commentID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	slog.Info("review comment deleted", "repo", repo, "comment", commentID, "by", identity)
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
			writeError(w, http.StatusNotFound, "repo not found")
		default:
			slog.Error("internal error", "op", "purge", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
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

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// validateRepo checks that the named repo exists. It writes a 404 and returns
// false when the repo is not found, so callers can do:
//
//	if !s.validateRepo(w, r, repo) { return }
func (s *server) validateRepo(w http.ResponseWriter, r *http.Request, repo string) bool {
	_, err := s.commitStore.GetRepo(r.Context(), repo)
	if err != nil {
		if errors.Is(err, db.ErrRepoNotFound) {
			writeError(w, http.StatusNotFound, "repo not found")
		} else {
			writeError(w, http.StatusInternalServerError, "query failed")
		}
		return false
	}
	return true
}

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
			writeError(w, http.StatusConflict, "repo already exists")
		case errors.Is(err, db.ErrOrgNotFound):
			writeError(w, http.StatusNotFound, "org not found")
		default:
			slog.Error("internal error", "op", "create_repo", "repo", req.Name, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
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
		writeError(w, http.StatusInternalServerError, "query failed")
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
			writeError(w, http.StatusNotFound, "repo not found")
		default:
			writeError(w, http.StatusInternalServerError, "query failed")
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
			writeError(w, http.StatusNotFound, "repo not found")
		default:
			slog.Error("internal error", "op", "delete_repo", "repo", name, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	identity := IdentityFromContext(r.Context())
	s.emit(r.Context(), evtypes.RepoDeleted{Repo: name, DeletedBy: identity})
	slog.Info("repo deleted", "repo", name, "by", identity)
	w.WriteHeader(http.StatusNoContent)
}

// handleTree implements GET /repos/:name/tree?branch=main&at=N&limit=N&after=cursor
func (s *server) handleTree(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}
	branch := r.URL.Query().Get("branch")
	if branch == "" {
		branch = "main"
	}

	var atSequence *int64
	if v := r.URL.Query().Get("at"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			// Try as release name.
			rel, relErr := s.commitStore.GetRelease(r.Context(), repo, v)
			if relErr != nil {
				writeError(w, http.StatusBadRequest, "invalid 'at' parameter: not a sequence number or release name")
				return
			}
			atSequence = &rel.Sequence
		} else {
			atSequence = &n
		}
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

	entries, err := s.readStore.MaterializeTree(r.Context(), repo, branch, atSequence, limit, afterPath)
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
//   - GET /repos/:name/file/{path...}          → file content
//   - GET /repos/:name/file/{path...}/history  → file change history
//
// Query params: branch (default "main"), at (sequence), limit, after (cursor).
func (s *server) handleFile(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}
	fullPath := r.PathValue("path")

	// Check for /history suffix.
	if strings.HasSuffix(fullPath, "/history") {
		filePath := strings.TrimSuffix(fullPath, "/history")
		s.handleFileHistory(w, r, repo, filePath)
		return
	}

	s.handleFileContent(w, r, repo, fullPath)
}

func (s *server) handleFileContent(w http.ResponseWriter, r *http.Request, repo, path string) {
	branch := r.URL.Query().Get("branch")
	if branch == "" {
		branch = "main"
	}

	var atSequence *int64
	if v := r.URL.Query().Get("at"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			// Try as release name.
			rel, relErr := s.commitStore.GetRelease(r.Context(), repo, v)
			if relErr != nil {
				writeError(w, http.StatusBadRequest, "invalid 'at' parameter: not a sequence number or release name")
				return
			}
			atSequence = &rel.Sequence
		} else {
			atSequence = &n
		}
	}

	fc, err := s.readStore.GetFile(r.Context(), repo, branch, path, atSequence)
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

func (s *server) handleFileHistory(w http.ResponseWriter, r *http.Request, repo, path string) {
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

	entries, err := s.readStore.GetFileHistory(r.Context(), repo, branch, path, limit, afterSeq)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if entries == nil {
		entries = []store.FileHistoryEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}

// handleGetCommit implements GET /repos/:name/commit/{sequence}
func (s *server) handleGetCommit(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	seqStr := r.PathValue("sequence")
	seq, err := strconv.ParseInt(seqStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid sequence number")
		return
	}

	detail, err := s.readStore.GetCommit(r.Context(), repo, seq)
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

// handleBranches implements GET /repos/:name/branches?status=active[&include_draft=true][&draft=true]
func (s *server) handleBranches(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}
	statusFilter := r.URL.Query().Get("status")
	includeDraft := r.URL.Query().Get("include_draft") == "true"
	onlyDraft := r.URL.Query().Get("draft") == "true"

	branches, err := s.readStore.ListBranches(r.Context(), repo, statusFilter, includeDraft, onlyDraft)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if branches == nil {
		branches = []store.BranchInfo{}
	}
	writeJSON(w, http.StatusOK, branches)
}

// handleDiff implements GET /repos/:name/diff?branch=X
func (s *server) handleDiff(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}
	branch := r.URL.Query().Get("branch")
	if branch == "" {
		writeError(w, http.StatusBadRequest, "branch parameter is required")
		return
	}

	result, err := s.readStore.GetDiff(r.Context(), repo, branch)
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
		resp.BranchChanges[i] = model.DiffEntry{Path: e.Path, VersionID: e.VersionID, Binary: e.Binary}
	}
	for i, e := range result.MainChanges {
		resp.MainChanges[i] = model.DiffEntry{Path: e.Path, VersionID: e.VersionID, Binary: e.Binary}
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

// handleCreateBranch implements POST /repos/:name/branch
func (s *server) handleCreateBranch(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")

	var req model.CreateBranchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Repo = repo

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
		case errors.Is(err, db.ErrRepoNotFound):
			writeError(w, http.StatusNotFound, "repo not found")
		default:
			slog.Error("internal error", "op", "create_branch", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	identity := IdentityFromContext(r.Context())
	s.emit(r.Context(), evtypes.BranchCreated{
		Repo:         repo,
		Branch:       req.Name,
		BaseSequence: resp.BaseSequence,
		CreatedBy:    identity,
	})
	slog.Info("branch created", "repo", repo, "branch", req.Name, "by", identity)
	writeJSON(w, http.StatusCreated, resp)
}

// handleUpdateBranch implements PATCH /repos/:name/branch/:bname
func (s *server) handleUpdateBranch(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	bname := r.PathValue("bname")

	if !s.validateRepo(w, r, repo) {
		return
	}
	if bname == "main" {
		writeError(w, http.StatusBadRequest, "cannot modify branch 'main'")
		return
	}

	var req model.UpdateBranchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := s.commitStore.UpdateBranchDraft(r.Context(), repo, bname, req.Draft); err != nil {
		switch {
		case errors.Is(err, db.ErrBranchNotFound):
			writeError(w, http.StatusNotFound, "branch not found")
		default:
			slog.Error("internal error", "op", "update_branch", "repo", repo, "branch", bname, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	slog.Info("branch updated", "repo", repo, "branch", bname, "draft", req.Draft, "by", IdentityFromContext(r.Context()))
	writeJSON(w, http.StatusOK, model.UpdateBranchResponse{Name: bname, Draft: req.Draft})
}

// handleDeleteBranch implements DELETE /repos/:name/branch/{bname}
func (s *server) handleDeleteBranch(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	bname := r.PathValue("bname")
	if bname == "main" {
		writeError(w, http.StatusBadRequest, "cannot delete branch 'main'")
		return
	}

	err := s.commitStore.DeleteBranch(r.Context(), repo, bname)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrBranchNotFound):
			writeError(w, http.StatusNotFound, "branch not found")
		case errors.Is(err, db.ErrBranchNotActive):
			writeError(w, http.StatusConflict, "branch is already merged or abandoned")
		default:
			slog.Error("internal error", "op", "delete_branch", "repo", repo, "branch", bname, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	identity := IdentityFromContext(r.Context())
	s.emit(r.Context(), evtypes.BranchAbandoned{Repo: repo, Branch: bname, AbandonedBy: identity})
	slog.Info("branch deleted", "repo", repo, "branch", bname, "by", identity)
	w.WriteHeader(http.StatusNoContent)
}

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

// handleRebase implements POST /repos/:name/rebase
func (s *server) handleRebase(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	var req model.RebaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Repo = repo

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
			slog.Warn("rebase conflict", "repo", repo, "branch", req.Branch, "conflicts", len(conflicts))
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
			slog.Error("internal error", "op", "rebase", "repo", repo, "branch", req.Branch, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	s.emit(r.Context(), evtypes.BranchRebased{
		Repo:            repo,
		Branch:          req.Branch,
		NewBaseSequence: resp.NewBaseSequence,
		NewHeadSequence: resp.NewHeadSequence,
		CommitsReplayed: resp.CommitsReplayed,
		RebasedBy:       req.Author,
	})
	slog.Info("branch rebased", "repo", repo, "branch", req.Branch, "by", req.Author, "commits_replayed", resp.CommitsReplayed)
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// Proposal handlers
// ---------------------------------------------------------------------------

// handleCreateProposal implements POST /repos/:name/proposals
func (s *server) handleCreateProposal(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	author := IdentityFromContext(r.Context())

	var req model.CreateProposalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Branch == "" {
		writeError(w, http.StatusBadRequest, "branch is required")
		return
	}
	if req.Title == "" {
		writeError(w, http.StatusBadRequest, "title is required")
		return
	}
	baseBranch := req.BaseBranch
	if baseBranch == "" {
		baseBranch = "main"
	}

	p, err := s.commitStore.CreateProposal(r.Context(), repo, req.Branch, baseBranch, req.Title, req.Description, author)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrProposalExists):
			writeError(w, http.StatusConflict, "branch already has an open proposal")
		case errors.Is(err, db.ErrBranchNotFound):
			writeError(w, http.StatusNotFound, "branch not found")
		default:
			slog.Error("internal error", "op", "create_proposal", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	var headSeq int64
	if bi, err := s.readStore.GetBranch(r.Context(), repo, req.Branch); err == nil {
		headSeq = bi.HeadSequence
	} else {
		slog.Warn("could not fetch branch head for proposal event", "repo", repo, "branch", req.Branch, "error", err)
	}
	s.emit(r.Context(), evtypes.ProposalOpened{
		Repo:       repo,
		Branch:     req.Branch,
		BaseBranch: baseBranch,
		ProposalID: p.ID,
		Author:     author,
		Sequence:   headSeq,
	})
	s.upsertProposalMentionRefs(r.Context(), repo, p.ID, req.Description)
	slog.Info("proposal opened", "repo", repo, "branch", req.Branch, "proposal_id", p.ID, "author", author)
	writeJSON(w, http.StatusCreated, model.CreateProposalResponse{ID: p.ID})
}

// handleListProposals implements GET /repos/:name/proposals
func (s *server) handleListProposals(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	var state *model.ProposalState
	if sv := r.URL.Query().Get("state"); sv != "" {
		ps := model.ProposalState(sv)
		switch ps {
		case model.ProposalOpen, model.ProposalClosed, model.ProposalMerged:
			state = &ps
		default:
			writeError(w, http.StatusBadRequest, "invalid state; must be open, closed, or merged")
			return
		}
	}

	var branch *string
	if bv := r.URL.Query().Get("branch"); bv != "" {
		branch = &bv
	}

	proposals, err := s.commitStore.ListProposals(r.Context(), repo, state, branch)
	if err != nil {
		slog.Error("internal error", "op", "list_proposals", "repo", repo, "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if proposals == nil {
		proposals = []*model.Proposal{}
	}
	writeJSON(w, http.StatusOK, proposals)
}

// handleGetProposal implements GET /repos/:name/proposals/:proposalID
func (s *server) handleGetProposal(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	proposalID := r.PathValue("proposalID")
	p, err := s.commitStore.GetProposal(r.Context(), repo, proposalID)
	if err != nil {
		if errors.Is(err, db.ErrProposalNotFound) {
			writeError(w, http.StatusNotFound, "proposal not found")
		} else {
			slog.Error("internal error", "op", "get_proposal", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// handleUpdateProposal implements PATCH /repos/:name/proposals/:proposalID
func (s *server) handleUpdateProposal(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	proposalID := r.PathValue("proposalID")
	identity := IdentityFromContext(r.Context())
	role := RoleFromContext(r.Context())

	// Fetch existing proposal to check authz.
	existing, err := s.commitStore.GetProposal(r.Context(), repo, proposalID)
	if err != nil {
		if errors.Is(err, db.ErrProposalNotFound) {
			writeError(w, http.StatusNotFound, "proposal not found")
		} else {
			slog.Error("internal error", "op", "update_proposal", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	if existing.Author != identity && role != model.RoleMaintainer && role != model.RoleAdmin {
		writeError(w, http.StatusForbidden, "forbidden: must be proposal author or maintainer")
		return
	}

	var req model.UpdateProposalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	p, err := s.commitStore.UpdateProposal(r.Context(), repo, proposalID, req.Title, req.Description)
	if err != nil {
		if errors.Is(err, db.ErrProposalNotFound) {
			writeError(w, http.StatusNotFound, "proposal not found")
		} else {
			slog.Error("internal error", "op", "update_proposal", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}
	if req.Description != nil {
		s.upsertProposalMentionRefs(r.Context(), repo, proposalID, *req.Description)
	}
	writeJSON(w, http.StatusOK, p)
}

// handleCloseProposal implements POST /repos/:name/proposals/:proposalID/close
func (s *server) handleCloseProposal(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	proposalID := r.PathValue("proposalID")
	identity := IdentityFromContext(r.Context())
	role := RoleFromContext(r.Context())

	// Fetch existing proposal to check authz.
	existing, err := s.commitStore.GetProposal(r.Context(), repo, proposalID)
	if err != nil {
		if errors.Is(err, db.ErrProposalNotFound) {
			writeError(w, http.StatusNotFound, "proposal not found")
		} else {
			slog.Error("internal error", "op", "close_proposal", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	if existing.Author != identity && role != model.RoleMaintainer && role != model.RoleAdmin {
		writeError(w, http.StatusForbidden, "forbidden: must be proposal author or maintainer")
		return
	}

	if err := s.commitStore.CloseProposal(r.Context(), repo, proposalID); err != nil {
		if errors.Is(err, db.ErrProposalNotFound) {
			writeError(w, http.StatusNotFound, "proposal not found")
		} else {
			slog.Error("internal error", "op", "close_proposal", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	s.emit(r.Context(), evtypes.ProposalClosed{
		Repo:       repo,
		Branch:     existing.Branch,
		ProposalID: proposalID,
	})
	slog.Info("proposal closed", "repo", repo, "proposal_id", proposalID, "by", identity)
	w.WriteHeader(http.StatusNoContent)
}

// handleMerge implements POST /repos/:name/merge
func (s *server) handleMerge(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	var req model.MergeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Repo = repo

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

	// Policy evaluation: runs before any database transaction.
	if denied, err := s.evaluateMergePolicy(r.Context(), repo, req.Branch, req.Author); err != nil {
		slog.Error("policy evaluation error", "op", "merge", "repo", repo, "branch", req.Branch, "error", err)
		writeError(w, http.StatusInternalServerError, "policy evaluation error")
		return
	} else if denied != nil {
		slog.Warn("merge denied by policy", "repo", repo, "branch", req.Branch, "actor", req.Author)
		// Collect policy names for event.
		policyNames := make([]string, len(denied))
		for i, p := range denied {
			policyNames[i] = p.Name
		}
		s.emit(r.Context(), evtypes.MergeBlocked{
			Repo:     repo,
			Branch:   req.Branch,
			Actor:    req.Author,
			Policies: policyNames,
		})
		writeJSON(w, http.StatusForbidden, model.MergePolicyError{Policies: denied})
		return
	}

	resp, conflicts, err := s.commitStore.Merge(r.Context(), req)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrMergeConflict):
			slog.Warn("merge conflict", "repo", repo, "branch", req.Branch, "conflicts", len(conflicts))
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
		case errors.Is(err, db.ErrBranchDraft):
			writeError(w, http.StatusConflict, "branch is in draft state")
		default:
			slog.Error("internal error", "op", "merge", "repo", repo, "branch", req.Branch, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	// Invalidate the policy cache: the merge may have updated policies on main.
	if s.policyCache != nil {
		s.policyCache.Invalidate(repo)
	}

	// Best-effort: transition any open proposal for this branch to merged.
	if mp, mergeProposalErr := s.commitStore.MergeProposal(r.Context(), repo, req.Branch); mergeProposalErr != nil {
		slog.Warn("failed to merge proposal", "repo", repo, "branch", req.Branch, "error", mergeProposalErr)
	} else if mp != nil {
		s.emit(r.Context(), evtypes.ProposalMerged{
			Repo:       repo,
			Branch:     req.Branch,
			BaseBranch: mp.BaseBranch,
			ProposalID: mp.ID,
		})
	}

	s.emit(r.Context(), evtypes.BranchMerged{
		Repo:     repo,
		Branch:   req.Branch,
		Sequence: resp.Sequence,
		MergedBy: req.Author,
	})
	slog.Info("branch merged", "repo", repo, "branch", req.Branch, "by", req.Author, "sequence", resp.Sequence)
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// Policy helpers
// ---------------------------------------------------------------------------

// assembleMergeInput gathers all data needed for OPA policy evaluation and
// returns a populated Input. Returns nil, nil if the branch does not exist
// (callers should let the merge handler surface the 404). owners is the raw
// directory→owners map from the policy cache; it is resolved per changed path
// before being placed on the Input.
func (s *server) assembleMergeInput(ctx context.Context, repo, branch, actor string, owners map[string][]string) (*policy.Input, error) {
	// Collect branch info for head/base sequence.
	branchInfo, err := s.readStore.GetBranch(ctx, repo, branch)
	if err != nil {
		return nil, fmt.Errorf("get branch info: %w", err)
	}
	if branchInfo == nil {
		return nil, nil // branch not found
	}

	// Collect changed paths.
	diff, err := s.readStore.GetDiff(ctx, repo, branch)
	if err != nil {
		return nil, fmt.Errorf("get diff: %w", err)
	}
	var changedPaths []string
	if diff != nil {
		for _, e := range diff.BranchChanges {
			changedPaths = append(changedPaths, e.Path)
		}
	}

	// Collect reviews filtered to the current head sequence (stale reviews excluded).
	reviews, err := s.commitStore.ListReviews(ctx, repo, branch, &branchInfo.HeadSequence)
	if err != nil {
		return nil, fmt.Errorf("list reviews: %w", err)
	}
	checkRuns, err := s.commitStore.ListCheckRuns(ctx, repo, branch, &branchInfo.HeadSequence)
	if err != nil {
		return nil, fmt.Errorf("list check runs: %w", err)
	}

	// Fetch the open proposal for this branch (nil if none exists).
	openState := model.ProposalOpen
	proposals, err := s.commitStore.ListProposals(ctx, repo, &openState, &branch)
	if err != nil {
		return nil, fmt.Errorf("list proposals: %w", err)
	}
	var proposalInput *policy.ProposalInput
	if len(proposals) > 0 {
		p := proposals[0]
		proposalInput = &policy.ProposalInput{
			ID:         p.ID,
			BaseBranch: p.BaseBranch,
			Title:      p.Title,
			State:      string(p.State),
		}
	}

	// Look up actor roles (empty slice if not assigned).
	actorRoles := []string{}
	if r, err := s.commitStore.GetRole(ctx, repo, actor); err == nil && r != nil {
		actorRoles = []string{string(r.Role)}
	}

	// Build OPA input.
	reviewInputs := make([]policy.ReviewInput, len(reviews))
	for i, rev := range reviews {
		reviewInputs[i] = policy.ReviewInput{
			Reviewer: rev.Reviewer,
			Status:   string(rev.Status),
			Sequence: rev.Sequence,
		}
	}
	checkInputs := make([]policy.CheckRunInput, len(checkRuns))
	for i, cr := range checkRuns {
		checkInputs[i] = policy.CheckRunInput{
			CheckName: cr.CheckName,
			Status:    string(cr.Status),
			Sequence:  cr.Sequence,
		}
	}

	// Resolve owners per changed path so policies can use input.owners["path"].
	resolvedOwners := make(map[string][]string)
	for _, p := range changedPaths {
		resolvedOwners[p] = policy.ResolveOwners(owners, p)
	}

	return &policy.Input{
		Actor:        actor,
		ActorRoles:   actorRoles,
		Action:       "merge",
		Repo:         repo,
		Branch:       branch,
		Draft:        branchInfo.Draft,
		ChangedPaths: changedPaths,
		Reviews:      reviewInputs,
		CheckRuns:    checkInputs,
		Owners:       resolvedOwners,
		HeadSeq:      branchInfo.HeadSequence,
		BaseSeq:      branchInfo.BaseSequence,
		Proposal:     proposalInput,
	}, nil
}

// evaluateMergePolicy evaluates all OPA policies for a pending merge.
// Returns the failing PolicyResults (non-nil means denied), or nil if allowed.
// Returns an error only for unexpected infrastructure failures.
// When readStore or policyCache are nil, or no policies exist, the merge is allowed.
func (s *server) evaluateMergePolicy(ctx context.Context, repo, branch, actor string) ([]model.PolicyResult, error) {
	if s.policyCache == nil || s.readStore == nil {
		return nil, nil
	}

	engine, owners, err := s.policyCache.Load(ctx, repo, s.readStore)
	if err != nil {
		return nil, err
	}
	if engine == nil {
		return nil, nil // bootstrap mode
	}

	input, err := s.assembleMergeInput(ctx, repo, branch, actor, owners)
	if err != nil {
		return nil, err
	}
	if input == nil {
		return nil, nil // branch not found — let the merge handler surface the error
	}

	results, err := engine.Evaluate(ctx, *input)
	if err != nil {
		return nil, fmt.Errorf("evaluate policies: %w", err)
	}

	// Collect any failing policies.
	var denied []model.PolicyResult
	for _, r := range results {
		if !r.Pass {
			denied = append(denied, r)
		}
	}
	return denied, nil
}

// ---------------------------------------------------------------------------
// Branch status handler
// ---------------------------------------------------------------------------

// handleBranchStatus implements GET /repos/:name/branch/:branch/status.
// It evaluates merge policies without performing any write operations.
func (s *server) handleBranchStatus(w http.ResponseWriter, r *http.Request, repo, branch string) {
	if s.readStore == nil {
		writeError(w, http.StatusServiceUnavailable, "read store not available")
		return
	}

	actor := IdentityFromContext(r.Context())

	// Determine if the repo even exists.
	if !s.validateRepo(w, r, repo) {
		return
	}

	// Load policies (nil engine → bootstrap mode → mergeable=true).
	var engine *policy.Engine
	var owners map[string][]string
	if s.policyCache != nil {
		var err error
		engine, owners, err = s.policyCache.Load(r.Context(), repo, s.readStore)
		if err != nil {
			slog.Error("policy cache load error", "op", "branch_status", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "policy evaluation error")
			return
		}
	}

	if engine == nil {
		// Bootstrap mode: no policies defined.
		writeJSON(w, http.StatusOK, model.BranchStatusResponse{
			Mergeable: true,
			Policies:  []model.PolicyResult{},
		})
		return
	}

	input, err := s.assembleMergeInput(r.Context(), repo, branch, actor, owners)
	if err != nil {
		slog.Error("assemble merge input error", "op", "branch_status", "repo", repo, "branch", branch, "error", err)
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if input == nil {
		writeError(w, http.StatusNotFound, "branch not found")
		return
	}

	results, err := engine.Evaluate(r.Context(), *input)
	if err != nil {
		slog.Error("policy evaluation error", "op", "branch_status", "repo", repo, "branch", branch, "error", err)
		writeError(w, http.StatusInternalServerError, "policy evaluation error")
		return
	}
	if results == nil {
		results = []model.PolicyResult{}
	}

	mergeable := true
	for _, res := range results {
		if !res.Pass {
			mergeable = false
			break
		}
	}

	writeJSON(w, http.StatusOK, model.BranchStatusResponse{
		Mergeable: mergeable,
		Policies:  results,
	})
}

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
			writeError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		if !exists {
			writeError(w, http.StatusBadRequest, "sequence not found")
			return
		}
	} else {
		if s.readStore == nil {
			writeError(w, http.StatusServiceUnavailable, "read store not available")
			return
		}
		branchInfo, err := s.readStore.GetBranch(r.Context(), repo, "main")
		if err != nil || branchInfo == nil {
			slog.Error("internal error", "op", "create_release_head", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "could not resolve main head sequence")
			return
		}
		sequence = branchInfo.HeadSequence
	}

	createdBy := IdentityFromContext(r.Context())
	rel, err := s.commitStore.CreateRelease(r.Context(), repo, req.Name, sequence, req.Body, createdBy)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrReleaseExists):
			writeError(w, http.StatusConflict, "release already exists")
		default:
			slog.Error("internal error", "op", "create_release", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
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
		writeError(w, http.StatusInternalServerError, "query failed")
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
			writeError(w, http.StatusNotFound, "release not found")
		default:
			slog.Error("internal error", "op", "get_release", "repo", repo, "release", releaseName, "error", err)
			writeError(w, http.StatusInternalServerError, "query failed")
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
			writeError(w, http.StatusNotFound, "release not found")
		default:
			slog.Error("internal error", "op", "delete_release", "repo", repo, "release", releaseName, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	slog.Info("release deleted", "repo", repo, "release", releaseName, "by", IdentityFromContext(r.Context()))
	w.WriteHeader(http.StatusNoContent)
}

// handleArchive implements GET /repos/:name/-/archive?branch=X
// Streams all files for the given branch as a tar archive.
func (s *server) handleArchive(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}
	if s.readStore == nil {
		writeError(w, http.StatusServiceUnavailable, "read store not available")
		return
	}

	branch := r.URL.Query().Get("branch")
	if branch == "" {
		writeError(w, http.StatusBadRequest, "branch parameter is required")
		return
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

	// Verify the branch exists before we start streaming.
	bi, err := s.readStore.GetBranch(r.Context(), repo, branch)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if bi == nil {
		writeError(w, http.StatusNotFound, "branch not found")
		return
	}

	w.Header().Set("Content-Type", "application/x-tar")
	tw := tar.NewWriter(w)
	defer tw.Close()

	afterPath := ""
	for {
		entries, err := s.readStore.MaterializeTree(r.Context(), repo, branch, atSequence, 100, afterPath)
		if err != nil {
			slog.Error("archive: materialize tree error", "repo", repo, "branch", branch, "error", err)
			return
		}
		for _, entry := range entries {
			fc, err := s.readStore.GetFile(r.Context(), repo, branch, entry.Path, atSequence)
			if err != nil || fc == nil {
				slog.Error("archive: get file error", "repo", repo, "branch", branch, "path", entry.Path, "error", err)
				continue
			}
			hdr := &tar.Header{
				Name:     entry.Path,
				Size:     int64(len(fc.Content)),
				Mode:     0644,
				Typeflag: tar.TypeReg,
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return
			}
			if _, err := tw.Write(fc.Content); err != nil {
				return
			}
		}
		if len(entries) < 100 {
			break
		}
		afterPath = entries[len(entries)-1].Path
	}
}

// handleChain implements GET /repos/:name/-/chain?from=N&to=N
// Returns commit metadata for sequences in [from, to] with commit hashes and file content hashes.
func (s *server) handleChain(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}
	if s.readStore == nil {
		writeError(w, http.StatusServiceUnavailable, "read store not available")
		return
	}

	fromStr := r.URL.Query().Get("from")
	toStr := r.URL.Query().Get("to")
	if fromStr == "" || toStr == "" {
		writeError(w, http.StatusBadRequest, "from and to query parameters are required")
		return
	}
	from, err := strconv.ParseInt(fromStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid 'from' parameter")
		return
	}
	to, err := strconv.ParseInt(toStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid 'to' parameter")
		return
	}
	if from > to {
		writeError(w, http.StatusBadRequest, "'from' must be <= 'to'")
		return
	}

	entries, err := s.readStore.GetChain(r.Context(), repo, from, to)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if entries == nil {
		entries = []store.ChainEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}

// ---------------------------------------------------------------------------
// Event subscription handlers (delegated auth: admin for global scope, creator for own subscriptions)
// ---------------------------------------------------------------------------

// handleCreateSubscription implements POST /subscriptions.
// Global admin may create subscriptions of any scope. Non-admin users may create
// repo-scoped subscriptions only if they have at least reader access to that repo.
func (s *server) handleCreateSubscription(w http.ResponseWriter, r *http.Request) {
	var req model.CreateSubscriptionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Backend == "" {
		writeError(w, http.StatusBadRequest, "backend is required")
		return
	}
	if req.Backend != "webhook" {
		writeError(w, http.StatusBadRequest, "only backend='webhook' is supported")
		return
	}
	if len(req.Config) == 0 {
		writeError(w, http.StatusBadRequest, "config is required")
		return
	}

	// Validate webhook config: url must be present and an http/https URL.
	var webhookConfig map[string]string
	if err := json.Unmarshal(req.Config, &webhookConfig); err != nil {
		writeError(w, http.StatusBadRequest, "config must be a JSON object")
		return
	}
	webhookURL, ok := webhookConfig["url"]
	if !ok || webhookURL == "" {
		writeError(w, http.StatusBadRequest, "config.url is required for webhook backend")
		return
	}
	parsedURL, err := url.ParseRequestURI(webhookURL)
	if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
		writeError(w, http.StatusBadRequest, "config.url must be a valid http or https URL")
		return
	}
	if webhookConfig["secret"] == "" {
		slog.Warn("subscription created without webhook secret", "url", webhookURL)
	}

	identity := IdentityFromContext(r.Context())
	isAdmin := s.globalAdmin != "" && identity == s.globalAdmin
	if !isAdmin {
		// Non-admin may only create repo-scoped subscriptions for repos they can read.
		if req.Repo == nil || *req.Repo == "" {
			writeError(w, http.StatusForbidden, "forbidden: global admin required for global subscriptions")
			return
		}
		if !s.requireRepoReadAccess(w, r, *req.Repo) {
			return
		}
	}

	req.CreatedBy = identity

	sub, err := s.commitStore.CreateSubscription(r.Context(), req)
	if err != nil {
		slog.Error("internal error", "op", "create_subscription", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	slog.Info("subscription created", "id", sub.ID, "backend", sub.Backend, "by", req.CreatedBy)
	writeJSON(w, http.StatusCreated, sub)
}

// handleListSubscriptions implements GET /subscriptions.
// Global admin sees all subscriptions. Non-admin users see only subscriptions
// they created (queried directly by created_by at the store level).
func (s *server) handleListSubscriptions(w http.ResponseWriter, r *http.Request) {
	identity := IdentityFromContext(r.Context())
	isAdmin := s.globalAdmin != "" && identity == s.globalAdmin

	var subs []model.EventSubscription
	var err error
	if isAdmin {
		subs, err = s.commitStore.ListSubscriptions(r.Context())
	} else {
		subs, err = s.commitStore.ListSubscriptionsByCreator(r.Context(), identity)
	}
	if err != nil {
		slog.Error("internal error", "op", "list_subscriptions", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if subs == nil {
		subs = []model.EventSubscription{}
	}
	writeJSON(w, http.StatusOK, model.ListSubscriptionsResponse{Subscriptions: subs})
}

// handleDeleteSubscription implements DELETE /subscriptions/{id}.
// Global admin may delete any subscription. The subscription creator may also delete it.
func (s *server) handleDeleteSubscription(w http.ResponseWriter, r *http.Request) {
	identity := IdentityFromContext(r.Context())
	isAdmin := s.globalAdmin != "" && identity == s.globalAdmin

	id := r.PathValue("id")

	if !isAdmin {
		sub, err := s.commitStore.GetSubscription(r.Context(), id)
		if err != nil {
			switch {
			case errors.Is(err, db.ErrSubscriptionNotFound):
				writeError(w, http.StatusNotFound, "subscription not found")
			default:
				slog.Error("internal error", "op", "get_subscription", "id", id, "error", err)
				writeError(w, http.StatusInternalServerError, "internal server error")
			}
			return
		}
		if sub.CreatedBy != identity {
			writeError(w, http.StatusForbidden, "forbidden: not subscription creator")
			return
		}
	}

	if err := s.commitStore.DeleteSubscription(r.Context(), id); err != nil {
		switch {
		case errors.Is(err, db.ErrSubscriptionNotFound):
			writeError(w, http.StatusNotFound, "subscription not found")
		default:
			slog.Error("internal error", "op", "delete_subscription", "id", id, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	slog.Info("subscription deleted", "id", id, "by", identity)
	w.WriteHeader(http.StatusNoContent)
}

// handleResumeSubscription implements POST /subscriptions/{id}/resume.
// Global admin may resume any subscription. The subscription creator may also resume it.
func (s *server) handleResumeSubscription(w http.ResponseWriter, r *http.Request) {
	identity := IdentityFromContext(r.Context())
	isAdmin := s.globalAdmin != "" && identity == s.globalAdmin

	id := r.PathValue("id")

	if !isAdmin {
		sub, err := s.commitStore.GetSubscription(r.Context(), id)
		if err != nil {
			switch {
			case errors.Is(err, db.ErrSubscriptionNotFound):
				writeError(w, http.StatusNotFound, "subscription not found")
			default:
				slog.Error("internal error", "op", "get_subscription", "id", id, "error", err)
				writeError(w, http.StatusInternalServerError, "internal server error")
			}
			return
		}
		if sub.CreatedBy != identity {
			writeError(w, http.StatusForbidden, "forbidden: not subscription creator")
			return
		}
	}

	if err := s.commitStore.ResumeSubscription(r.Context(), id); err != nil {
		switch {
		case errors.Is(err, db.ErrSubscriptionNotFound):
			writeError(w, http.StatusNotFound, "subscription not found")
		default:
			slog.Error("internal error", "op", "resume_subscription", "id", id, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	slog.Info("subscription resumed", "id", id, "by", identity)
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// SSE streaming handlers
// ---------------------------------------------------------------------------

// handleSSERepoEvents implements GET /repos/{name}/-/events
// Streams CloudEvents for a specific repo. Optional ?types= comma-separated
// full event type filter (e.g. "com.docstore.commit.created").
func (s *server) handleSSERepoEvents(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}
	s.streamSSE(w, r, repo)
}

// handleSSEGlobalEvents implements GET /events (admin only)
// Streams CloudEvents for all repos or a specific repo via ?repo= filter.
func (s *server) handleSSEGlobalEvents(w http.ResponseWriter, r *http.Request) {
	if !s.requireGlobalAdmin(w, r) {
		return
	}
	// Optional repo filter.
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		repo = "*" // wildcard: receive events from all repos
	}
	s.streamSSE(w, r, repo)
}

// streamSSE is the shared SSE streaming implementation.
// repo is the repo to subscribe to (or "*" for global admin stream).
func (s *server) streamSSE(w http.ResponseWriter, r *http.Request, repo string) {
	if s.broker == nil {
		writeError(w, http.StatusServiceUnavailable, "event streaming not available")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	// Parse optional type filter.
	var types []string
	if q := r.URL.Query().Get("types"); q != "" {
		for _, t := range strings.Split(q, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				types = append(types, t)
			}
		}
	}

	ch, unsub := s.broker.Subscribe(repo, types)
	defer unsub()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	keepAlive := time.NewTicker(15 * time.Second)
	defer keepAlive.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-keepAlive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case e, ok := <-ch:
			if !ok {
				return
			}
			data, err := events.ToCloudEvent(e)
			if err != nil {
				slog.Error("SSE: serialize event failed", "error", err)
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// ---------------------------------------------------------------------------
// Issue handlers
// ---------------------------------------------------------------------------

var (
	reMentionProposal = regexp.MustCompile(`\bproposal:([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\b`)
	reMentionCommit   = regexp.MustCompile(`\bcommit:(\d+)\b`)
	reMentionIssue    = regexp.MustCompile(`(?:^|[^&\w])#(\d+)`)
)

// parseMentions extracts cross-reference mentions from a text body.
// Returns proposal UUIDs, commit sequence numbers, and issue numbers.
func parseMentions(body string) (proposalIDs []string, commitSeqs []int64, issueNums []int64) {
	for _, m := range reMentionProposal.FindAllStringSubmatch(body, -1) {
		proposalIDs = append(proposalIDs, m[1])
	}
	for _, m := range reMentionCommit.FindAllStringSubmatch(body, -1) {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			commitSeqs = append(commitSeqs, n)
		}
	}
	for _, m := range reMentionIssue.FindAllStringSubmatch(body, -1) {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			issueNums = append(issueNums, n)
		}
	}
	return
}

// upsertIssueBodyRefs creates issue_refs on issueNum for proposal and commit
// mentions found in body, ignoring duplicate and not-found errors.
func (s *server) upsertIssueBodyRefs(ctx context.Context, repo string, issueNum int64, body string) {
	proposalIDs, commitSeqs, _ := parseMentions(body)
	for _, pid := range proposalIDs {
		_, err := s.commitStore.CreateIssueRef(ctx, repo, issueNum, model.IssueRefTypeProposal, pid)
		if err != nil && !errors.Is(err, db.ErrIssueRefExists) && !errors.Is(err, db.ErrIssueNotFound) {
			slog.Warn("upsert issue ref", "repo", repo, "issue", issueNum, "ref_type", "proposal", "ref_id", pid, "error", err)
		}
	}
	for _, seq := range commitSeqs {
		refID := strconv.FormatInt(seq, 10)
		_, err := s.commitStore.CreateIssueRef(ctx, repo, issueNum, model.IssueRefTypeCommit, refID)
		if err != nil && !errors.Is(err, db.ErrIssueRefExists) && !errors.Is(err, db.ErrIssueNotFound) {
			slog.Warn("upsert issue ref", "repo", repo, "issue", issueNum, "ref_type", "commit", "ref_id", refID, "error", err)
		}
	}
}

// upsertProposalMentionRefs creates issue_refs for issues mentioned in a proposal body.
// When a proposal body contains "#N", an issue_ref of type "proposal" pointing at proposalID
// is upserted on issue N.
func (s *server) upsertProposalMentionRefs(ctx context.Context, repo, proposalID, body string) {
	_, _, issueNums := parseMentions(body)
	for _, num := range issueNums {
		_, err := s.commitStore.CreateIssueRef(ctx, repo, num, model.IssueRefTypeProposal, proposalID)
		if err != nil && !errors.Is(err, db.ErrIssueRefExists) && !errors.Is(err, db.ErrIssueNotFound) {
			slog.Warn("upsert proposal mention ref", "repo", repo, "issue", num, "proposal", proposalID, "error", err)
		}
	}
}

// parseIssueNumber parses the "number" path value into an int64.
// Writes 400 and returns false on failure.
func parseIssueNumber(w http.ResponseWriter, r *http.Request) (int64, bool) {
	n, err := strconv.ParseInt(r.PathValue("number"), 10, 64)
	if err != nil || n <= 0 {
		writeError(w, http.StatusBadRequest, "invalid issue number")
		return 0, false
	}
	return n, true
}

// handleListIssues implements GET /repos/:name/issues
func (s *server) handleListIssues(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	state := r.URL.Query().Get("state")
	if state == "" {
		state = "open"
	}
	switch state {
	case "open", "closed", "all":
	default:
		writeError(w, http.StatusBadRequest, "invalid state; must be open, closed, or all")
		return
	}
	stateFilter := state
	if state == "all" {
		stateFilter = ""
	}
	authorFilter := r.URL.Query().Get("author")

	issues, err := s.commitStore.ListIssues(r.Context(), repo, stateFilter, authorFilter)
	if err != nil {
		slog.Error("internal error", "op", "list_issues", "repo", repo, "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, model.ListIssuesResponse{Issues: issues})
}

// handleCreateIssue implements POST /repos/:name/issues
func (s *server) handleCreateIssue(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	var req model.CreateIssueRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Title == "" {
		writeError(w, http.StatusBadRequest, "title is required")
		return
	}

	author := IdentityFromContext(r.Context())
	iss, err := s.commitStore.CreateIssue(r.Context(), repo, req.Title, req.Body, author, req.Labels)
	if err != nil {
		slog.Error("internal error", "op", "create_issue", "repo", repo, "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	s.upsertIssueBodyRefs(r.Context(), repo, iss.Number, req.Body)
	s.emit(r.Context(), evtypes.IssueOpened{Repo: repo, IssueID: iss.ID, Number: iss.Number, Author: author})
	slog.Info("issue opened", "repo", repo, "number", iss.Number, "author", author)
	writeJSON(w, http.StatusCreated, model.CreateIssueResponse{ID: iss.ID, Number: iss.Number})
}

// handleGetIssue implements GET /repos/:name/issues/:number
func (s *server) handleGetIssue(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	number, ok := parseIssueNumber(w, r)
	if !ok {
		return
	}

	iss, err := s.commitStore.GetIssue(r.Context(), repo, number)
	if err != nil {
		if errors.Is(err, db.ErrIssueNotFound) {
			writeError(w, http.StatusNotFound, "issue not found")
		} else {
			slog.Error("internal error", "op", "get_issue", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}
	writeJSON(w, http.StatusOK, iss)
}

// handleUpdateIssue implements PATCH /repos/:name/issues/:number
func (s *server) handleUpdateIssue(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	number, ok := parseIssueNumber(w, r)
	if !ok {
		return
	}

	identity := IdentityFromContext(r.Context())
	role := RoleFromContext(r.Context())

	existing, err := s.commitStore.GetIssue(r.Context(), repo, number)
	if err != nil {
		if errors.Is(err, db.ErrIssueNotFound) {
			writeError(w, http.StatusNotFound, "issue not found")
		} else {
			slog.Error("internal error", "op", "update_issue", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	if existing.Author != identity && role != model.RoleMaintainer && role != model.RoleAdmin {
		writeError(w, http.StatusForbidden, "forbidden: must be issue author or maintainer")
		return
	}

	var req model.UpdateIssueRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	var labelsPtr *[]string
	if req.Labels != nil {
		labelsPtr = &req.Labels
	}
	iss, err := s.commitStore.UpdateIssue(r.Context(), repo, number, req.Title, req.Body, labelsPtr)
	if err != nil {
		if errors.Is(err, db.ErrIssueNotFound) {
			writeError(w, http.StatusNotFound, "issue not found")
		} else {
			slog.Error("internal error", "op", "update_issue", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	if req.Body != nil {
		s.upsertIssueBodyRefs(r.Context(), repo, number, *req.Body)
	}
	s.emit(r.Context(), evtypes.IssueUpdated{Repo: repo, IssueID: iss.ID, Number: iss.Number})
	writeJSON(w, http.StatusOK, iss)
}

// handleCloseIssue implements POST /repos/:name/issues/:number/close
func (s *server) handleCloseIssue(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	number, ok := parseIssueNumber(w, r)
	if !ok {
		return
	}

	identity := IdentityFromContext(r.Context())
	role := RoleFromContext(r.Context())

	existing, err := s.commitStore.GetIssue(r.Context(), repo, number)
	if err != nil {
		if errors.Is(err, db.ErrIssueNotFound) {
			writeError(w, http.StatusNotFound, "issue not found")
		} else {
			slog.Error("internal error", "op", "close_issue", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	if existing.Author != identity && role != model.RoleMaintainer && role != model.RoleAdmin {
		writeError(w, http.StatusForbidden, "forbidden: must be issue author or maintainer")
		return
	}

	if existing.State == model.IssueStateClosed {
		writeError(w, http.StatusConflict, "issue is already closed")
		return
	}

	var req model.CloseIssueRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	reason := req.Reason
	if reason == "" {
		reason = model.IssueCloseReasonCompleted
	}
	switch reason {
	case model.IssueCloseReasonCompleted, model.IssueCloseReasonNotPlanned, model.IssueCloseReasonDuplicate:
	default:
		writeError(w, http.StatusBadRequest, "invalid close reason")
		return
	}

	iss, err := s.commitStore.CloseIssue(r.Context(), repo, number, reason, identity)
	if err != nil {
		if errors.Is(err, db.ErrIssueNotFound) {
			writeError(w, http.StatusNotFound, "issue not found")
		} else {
			slog.Error("internal error", "op", "close_issue", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	s.emit(r.Context(), evtypes.IssueClosed{Repo: repo, IssueID: iss.ID, Number: iss.Number, Reason: string(reason), ClosedBy: identity})
	slog.Info("issue closed", "repo", repo, "number", number, "by", identity)
	writeJSON(w, http.StatusOK, iss)
}

// handleReopenIssue implements POST /repos/:name/issues/:number/reopen
func (s *server) handleReopenIssue(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	number, ok := parseIssueNumber(w, r)
	if !ok {
		return
	}

	identity := IdentityFromContext(r.Context())
	role := RoleFromContext(r.Context())

	existing, err := s.commitStore.GetIssue(r.Context(), repo, number)
	if err != nil {
		if errors.Is(err, db.ErrIssueNotFound) {
			writeError(w, http.StatusNotFound, "issue not found")
		} else {
			slog.Error("internal error", "op", "reopen_issue", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	if existing.Author != identity && role != model.RoleMaintainer && role != model.RoleAdmin {
		writeError(w, http.StatusForbidden, "forbidden: must be issue author or maintainer")
		return
	}

	if existing.State == model.IssueStateOpen {
		writeError(w, http.StatusConflict, "issue is already open")
		return
	}

	iss, err := s.commitStore.ReopenIssue(r.Context(), repo, number)
	if err != nil {
		if errors.Is(err, db.ErrIssueNotFound) {
			writeError(w, http.StatusNotFound, "issue not found")
		} else {
			slog.Error("internal error", "op", "reopen_issue", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	s.emit(r.Context(), evtypes.IssueReopened{Repo: repo, IssueID: iss.ID, Number: iss.Number})
	slog.Info("issue reopened", "repo", repo, "number", number)
	writeJSON(w, http.StatusOK, iss)
}

// handleListIssueComments implements GET /repos/:name/issues/:number/comments
func (s *server) handleListIssueComments(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	number, ok := parseIssueNumber(w, r)
	if !ok {
		return
	}

	comments, err := s.commitStore.ListIssueComments(r.Context(), repo, number)
	if err != nil {
		slog.Error("internal error", "op", "list_issue_comments", "repo", repo, "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, model.ListIssueCommentsResponse{Comments: comments})
}

// handleCreateIssueComment implements POST /repos/:name/issues/:number/comments
func (s *server) handleCreateIssueComment(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	number, ok := parseIssueNumber(w, r)
	if !ok {
		return
	}

	var req model.CreateIssueCommentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Body == "" {
		writeError(w, http.StatusBadRequest, "body is required")
		return
	}

	author := IdentityFromContext(r.Context())
	c, err := s.commitStore.CreateIssueComment(r.Context(), repo, number, req.Body, author)
	if err != nil {
		if errors.Is(err, db.ErrIssueNotFound) {
			writeError(w, http.StatusNotFound, "issue not found")
		} else {
			slog.Error("internal error", "op", "create_issue_comment", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	s.upsertIssueBodyRefs(r.Context(), repo, number, req.Body)
	s.emit(r.Context(), evtypes.IssueCommentCreated{Repo: repo, IssueID: c.IssueID, Number: number, CommentID: c.ID, Author: author})
	slog.Info("issue comment created", "repo", repo, "issue", number, "comment", c.ID, "author", author)
	writeJSON(w, http.StatusCreated, model.CreateIssueCommentResponse{ID: c.ID})
}

// handleUpdateIssueComment implements PATCH /repos/:name/issues/:number/comments/:commentID
func (s *server) handleUpdateIssueComment(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	number, ok := parseIssueNumber(w, r)
	if !ok {
		return
	}

	commentID := r.PathValue("commentID")
	identity := IdentityFromContext(r.Context())
	role := RoleFromContext(r.Context())

	existing, err := s.commitStore.GetIssueComment(r.Context(), repo, commentID)
	if err != nil {
		if errors.Is(err, db.ErrIssueCommentNotFound) {
			writeError(w, http.StatusNotFound, "comment not found")
		} else {
			slog.Error("internal error", "op", "update_issue_comment", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	issue, err := s.commitStore.GetIssue(r.Context(), repo, number)
	if err != nil {
		if errors.Is(err, db.ErrIssueNotFound) {
			writeError(w, http.StatusNotFound, "comment not found")
		} else {
			slog.Error("internal error", "op", "update_issue_comment", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}
	if existing.IssueID != issue.ID {
		writeError(w, http.StatusNotFound, "comment not found")
		return
	}

	if existing.Author != identity && role != model.RoleMaintainer && role != model.RoleAdmin {
		writeError(w, http.StatusForbidden, "forbidden: must be comment author or maintainer")
		return
	}

	var req model.UpdateIssueCommentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Body == "" {
		writeError(w, http.StatusBadRequest, "body is required")
		return
	}

	c, err := s.commitStore.UpdateIssueComment(r.Context(), repo, commentID, req.Body)
	if err != nil {
		if errors.Is(err, db.ErrIssueCommentNotFound) {
			writeError(w, http.StatusNotFound, "comment not found")
		} else {
			slog.Error("internal error", "op", "update_issue_comment", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	s.upsertIssueBodyRefs(r.Context(), repo, number, req.Body)
	s.emit(r.Context(), evtypes.IssueCommentUpdated{Repo: repo, IssueID: c.IssueID, Number: number, CommentID: c.ID})
	writeJSON(w, http.StatusOK, c)
}

// handleDeleteIssueComment implements DELETE /repos/:name/issues/:number/comments/:commentID
func (s *server) handleDeleteIssueComment(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	number, ok := parseIssueNumber(w, r)
	if !ok {
		return
	}

	commentID := r.PathValue("commentID")
	identity := IdentityFromContext(r.Context())
	role := RoleFromContext(r.Context())

	existing, err := s.commitStore.GetIssueComment(r.Context(), repo, commentID)
	if err != nil {
		if errors.Is(err, db.ErrIssueCommentNotFound) {
			writeError(w, http.StatusNotFound, "comment not found")
		} else {
			slog.Error("internal error", "op", "delete_issue_comment", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	issue, err := s.commitStore.GetIssue(r.Context(), repo, number)
	if err != nil {
		if errors.Is(err, db.ErrIssueNotFound) {
			writeError(w, http.StatusNotFound, "comment not found")
		} else {
			slog.Error("internal error", "op", "delete_issue_comment", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}
	if existing.IssueID != issue.ID {
		writeError(w, http.StatusNotFound, "comment not found")
		return
	}

	if existing.Author != identity && role != model.RoleMaintainer && role != model.RoleAdmin {
		writeError(w, http.StatusForbidden, "forbidden: must be comment author or maintainer")
		return
	}

	if err := s.commitStore.DeleteIssueComment(r.Context(), repo, commentID); err != nil {
		if errors.Is(err, db.ErrIssueCommentNotFound) {
			writeError(w, http.StatusNotFound, "comment not found")
		} else {
			slog.Error("internal error", "op", "delete_issue_comment", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListIssueRefs implements GET /repos/:name/issues/:number/refs
func (s *server) handleListIssueRefs(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	number, ok := parseIssueNumber(w, r)
	if !ok {
		return
	}

	refs, err := s.commitStore.ListIssueRefs(r.Context(), repo, number)
	if err != nil {
		slog.Error("internal error", "op", "list_issue_refs", "repo", repo, "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, model.ListIssueRefsResponse{Refs: refs})
}

// handleAddIssueRef implements POST /repos/:name/issues/:number/refs
func (s *server) handleAddIssueRef(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	number, ok := parseIssueNumber(w, r)
	if !ok {
		return
	}

	var req model.AddIssueRefRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	switch req.RefType {
	case model.IssueRefTypeProposal, model.IssueRefTypeCommit:
	default:
		writeError(w, http.StatusBadRequest, "invalid ref_type; must be proposal or commit")
		return
	}
	if req.RefID == "" {
		writeError(w, http.StatusBadRequest, "ref_id is required")
		return
	}

	ref, err := s.commitStore.CreateIssueRef(r.Context(), repo, number, req.RefType, req.RefID)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrIssueNotFound):
			writeError(w, http.StatusNotFound, "issue not found")
		case errors.Is(err, db.ErrIssueRefExists):
			writeError(w, http.StatusConflict, "ref already exists")
		default:
			slog.Error("internal error", "op", "add_issue_ref", "repo", repo, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}
	writeJSON(w, http.StatusCreated, model.AddIssueRefResponse{ID: ref.ID})
}

// handleProposalIssues implements GET /repos/:name/proposals/:proposalID/issues
func (s *server) handleProposalIssues(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	proposalID := r.PathValue("proposalID")
	issues, err := s.commitStore.ListIssuesByRef(r.Context(), repo, model.IssueRefTypeProposal, proposalID)
	if err != nil {
		slog.Error("internal error", "op", "proposal_issues", "repo", repo, "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, model.ListIssuesResponse{Issues: issues})
}

// handleCommitIssues implements GET /repos/:name/commit/:sequence/issues
func (s *server) handleCommitIssues(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}

	seqStr := r.PathValue("sequence")
	if _, err := strconv.ParseInt(seqStr, 10, 64); err != nil {
		writeError(w, http.StatusBadRequest, "invalid sequence number")
		return
	}
	issues, err := s.commitStore.ListIssuesByRef(r.Context(), repo, model.IssueRefTypeCommit, seqStr)
	if err != nil {
		slog.Error("internal error", "op", "commit_issues", "repo", repo, "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, model.ListIssuesResponse{Issues: issues})
}
