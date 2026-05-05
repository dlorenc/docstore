package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/dlorenc/docstore/internal/citoken"
	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/secrets"
)

// ---------------------------------------------------------------------------
// Wire-format types for the repo-secrets API. Plaintext is sent as a UTF-8
// string on the way in via PUT; nothing on the way out ever carries plaintext
// or sealed bytes — we project secrets.Metadata to secretMetadataDTO and rely
// on the type system to keep ciphertext/nonce/encrypted_dek out of responses.
// ---------------------------------------------------------------------------

// setSecretRequest is the PUT body. Description is optional.
type setSecretRequest struct {
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
}

// secretMetadataDTO is the public-safe shape of a secret. It mirrors
// secrets.Metadata but uses JSON tags suitable for clients and deliberately
// omits Repo (it is implicit in the URL).
type secretMetadataDTO struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	SizeBytes   int        `json:"size_bytes"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedBy   *string    `json:"updated_by,omitempty"`
	UpdatedAt   *time.Time `json:"updated_at,omitempty"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
}

// listSecretsResponse is the GET body. Always returns a non-nil slice so the
// JSON is `[]` rather than `null` when the repo has no secrets.
type listSecretsResponse struct {
	Secrets []secretMetadataDTO `json:"secrets"`
}

// secretMetadataFrom converts a secrets.Metadata to the wire DTO.
func secretMetadataFrom(m secrets.Metadata) secretMetadataDTO {
	return secretMetadataDTO{
		ID:          m.ID,
		Name:        m.Name,
		Description: m.Description,
		SizeBytes:   m.SizeBytes,
		CreatedBy:   m.CreatedBy,
		CreatedAt:   m.CreatedAt,
		UpdatedBy:   m.UpdatedBy,
		UpdatedAt:   m.UpdatedAt,
		LastUsedAt:  m.LastUsedAt,
	}
}

// ---------------------------------------------------------------------------
// Handlers. All three require an existing repo. RBAC is enforced upstream by
// roleAllows in middleware.go: GET → reader+, PUT/DELETE → admin.
// ---------------------------------------------------------------------------

// handleListSecrets implements GET /repos/{owner}/{name}/-/secrets.
// Returns metadata only — never plaintext, ciphertext, or any sealed field.
func (s *server) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}
	if s.secrets == nil {
		writeAPIError(w, ErrCodeServiceUnavailable, http.StatusServiceUnavailable, "secrets service not configured")
		return
	}

	metas, err := s.secrets.List(r.Context(), repo)
	if err != nil {
		slog.Error("internal error", "op", "list_secrets", "repo", repo, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		return
	}

	dtos := make([]secretMetadataDTO, len(metas))
	for i, m := range metas {
		dtos[i] = secretMetadataFrom(m)
	}
	writeJSON(w, http.StatusOK, listSecretsResponse{Secrets: dtos})
}

// handleSetSecret implements PUT /repos/{owner}/{name}/-/secrets/{secname}.
// Body: {"value": "<plaintext>", "description": "..."}. Response echoes
// metadata only. Validation errors map to 400; everything else is 500. The
// plaintext value is NEVER logged, even on error.
func (s *server) handleSetSecret(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	secname := r.PathValue("secname")
	if !s.validateRepo(w, r, repo) {
		return
	}
	if s.secrets == nil {
		writeAPIError(w, ErrCodeServiceUnavailable, http.StatusServiceUnavailable, "secrets service not configured")
		return
	}

	var req setSecretRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	actor := IdentityFromContext(r.Context())
	meta, err := s.secrets.Set(r.Context(), repo, secname, req.Description, []byte(req.Value), actor)
	if err != nil {
		switch {
		case errors.Is(err, secrets.ErrInvalidName):
			writeError(w, http.StatusBadRequest, "invalid secret name")
		case errors.Is(err, secrets.ErrReservedName):
			writeError(w, http.StatusBadRequest, "secret name uses reserved prefix")
		case errors.Is(err, secrets.ErrValueTooLarge):
			writeError(w, http.StatusBadRequest, "secret value exceeds maximum size")
		case errors.Is(err, secrets.ErrEmptyValue):
			writeError(w, http.StatusBadRequest, "secret value is empty")
		default:
			// IMPORTANT: log the error wrapper but never the value.
			slog.Error("internal error", "op", "set_secret", "repo", repo, "name", secname, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	slog.Info("secret set", "repo", repo, "name", secname, "by", actor, "size_bytes", meta.SizeBytes)
	writeJSON(w, http.StatusOK, secretMetadataFrom(meta))
}

// handleDeleteSecret implements DELETE /repos/{owner}/{name}/-/secrets/{secname}.
// 204 on success, 404 if no such secret. Hard delete — there is no soft state.
func (s *server) handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	secname := r.PathValue("secname")
	if !s.validateRepo(w, r, repo) {
		return
	}
	if s.secrets == nil {
		writeAPIError(w, ErrCodeServiceUnavailable, http.StatusServiceUnavailable, "secrets service not configured")
		return
	}

	err := s.secrets.Delete(r.Context(), repo, secname)
	if err != nil {
		switch {
		case errors.Is(err, secrets.ErrNotFound):
			writeAPIError(w, ErrCodeNotFound, http.StatusNotFound, "secret not found")
		default:
			slog.Error("internal error", "op", "delete_secret", "repo", repo, "name", secname, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	slog.Info("secret deleted", "repo", repo, "name", secname, "by", IdentityFromContext(r.Context()))
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Reveal — POST /repos/{owner}/{name}/-/secrets/reveal
//
// Worker-facing endpoint that decrypts repo secrets at CI dispatch time. The
// caller authenticates with the CI request token (Authorization: Bearer ...),
// not with a Google ID token: this is the same auth pattern as handleCIConfig.
// The handler is registered on the outer mux *before* Google auth and RBAC, so
// the regular roleAllows path is bypassed entirely and the bearer token is the
// sole credential.
// ---------------------------------------------------------------------------

// revealRequest is the POST body. Names must be a non-empty list of valid
// secret names (matching the same regex used at write time).
type revealRequest struct {
	Names []string `json:"names"`
}

// revealResponse is the success body. Values are base64 (standard, padded) so
// the JSON is binary-safe even though v1 only stores text. Missing names are
// reported alongside found ones — the worker decides what to do.
type revealResponse struct {
	Values  map[string]string `json:"values"`
	Missing []string          `json:"missing,omitempty"`
}

// revealNameRegexp mirrors the validation in internal/secrets.validateName.
// Duplicated here so the handler can reject malformed names with a 400 before
// hitting the service (which would otherwise bubble such names back via the
// "missing" list, masking client bugs).
var revealNameRegexp = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// handleRevealSecrets implements POST /repos/{owner}/{name}/-/secrets/reveal.
// Authenticated via request_token (Authorization: Bearer <plaintext>). The
// gating policy in docs/secrets-design.md is enforced here: post-submit and
// manual triggers always allow; proposal triggers allow only if the proposal
// author is an org member of the repo's org.
func (s *server) handleRevealSecrets(w http.ResponseWriter, r *http.Request) {
	if s.jobTokenStore == nil || s.commitStore == nil || s.secrets == nil {
		writeAPIError(w, ErrCodeServiceUnavailable, http.StatusServiceUnavailable, "secrets reveal not available")
		return
	}

	// 1. Extract and validate request_token from Authorization header.
	authHdr := r.Header.Get("Authorization")
	plaintext := strings.TrimPrefix(authHdr, "Bearer ")
	if plaintext == "" || plaintext == authHdr {
		writeAPIError(w, ErrCodeUnauthorized, http.StatusUnauthorized, "unauthorized")
		return
	}
	hashed := citoken.HashRequestToken(plaintext)
	job, err := s.jobTokenStore.LookupRequestToken(r.Context(), hashed)
	if errors.Is(err, db.ErrTokenInvalid) {
		writeAPIError(w, ErrCodeUnauthorized, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err != nil {
		slog.Error("internal error", "op", "reveal_secrets", "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		return
	}

	// 2. Verify repo in token matches the request path. Mismatch → 404 per the
	// agreed wire format (the URL repo "doesn't match the token's job", and we
	// do not want to disclose that the token is otherwise valid).
	repoName := r.PathValue("name")
	if job.Repo != repoName {
		writeAPIError(w, ErrCodeNotFound, http.StatusNotFound, "not found")
		return
	}

	// 3. Decode and validate the request body.
	var req revealRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, ErrCodeBadRequest, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Names) == 0 {
		writeAPIError(w, ErrCodeBadRequest, http.StatusBadRequest, "names is required")
		return
	}
	for _, n := range req.Names {
		if !revealNameRegexp.MatchString(n) {
			writeAPIError(w, ErrCodeBadRequest, http.StatusBadRequest, "invalid secret name")
			return
		}
	}

	// 4. Apply the gating policy. We never log requested names or values.
	if reason, ok := s.allowSecretsForJob(r.Context(), job); !ok {
		slog.Info("secrets reveal denied",
			"op", "reveal_secrets",
			"repo", repoName,
			"job_id", job.ID,
			"trigger_type", job.TriggerType,
			"reason", reason,
		)
		writeAPIError(w, ErrCodeForbidden, http.StatusForbidden, "secrets_blocked: "+reason)
		return
	}

	// 5. Decrypt. Service errors are mapped to 500 — the body never carries
	// the value or the underlying error string (which could include a name).
	values, missing, err := s.secrets.Reveal(r.Context(), repoName, req.Names)
	if err != nil {
		slog.Error("internal error", "op", "reveal_secrets", "repo", repoName, "job_id", job.ID, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		return
	}

	// 6. Encode plaintext as base64 to be binary-safe and write the response.
	encoded := make(map[string]string, len(values))
	for name, v := range values {
		encoded[name] = base64.StdEncoding.EncodeToString(v)
	}
	if missing == nil {
		missing = []string{}
	}
	writeJSON(w, http.StatusOK, revealResponse{Values: encoded, Missing: missing})
}

// allowSecretsForJob applies the gating policy from docs/secrets-design.md.
// Returns (reason, false) when access is denied; reason is included verbatim
// in the 403 body so operators can diagnose policy decisions, but never
// includes a secret name or value.
func (s *server) allowSecretsForJob(ctx context.Context, job *model.CIJob) (string, bool) {
	switch job.TriggerType {
	case "push", "manual", "schedule":
		return "", true
	case "proposal", "proposal_synchronized":
		// Allow iff proposal.Author is an org member of the repo's org.
		org, _, ok := splitRepoOrg(job.Repo)
		if !ok {
			return "malformed_repo", false
		}
		if job.TriggerProposalID == nil || *job.TriggerProposalID == "" {
			return "missing_proposal", false
		}
		prop, err := s.commitStore.GetProposal(ctx, job.Repo, *job.TriggerProposalID)
		if err != nil {
			if errors.Is(err, db.ErrProposalNotFound) {
				return "proposal_not_found", false
			}
			slog.Error("internal error", "op", "reveal_secrets", "repo", job.Repo, "job_id", job.ID, "error", err)
			return "lookup_failed", false
		}
		members, err := s.commitStore.ListOrgMembers(ctx, org)
		if err != nil {
			slog.Error("internal error", "op", "reveal_secrets", "repo", job.Repo, "job_id", job.ID, "error", err)
			return "lookup_failed", false
		}
		for _, m := range members {
			if m.Identity == prop.Author {
				return "", true
			}
		}
		return "non_member_proposal", false
	default:
		return "trigger_not_allowed", false
	}
}

// splitRepoOrg splits "org/name" → ("org", "name", true). Splits on the LAST
// "/" so nested org names like "acme/team" survive. Returns ok=false for
// inputs that don't contain a "/" at all.
func splitRepoOrg(repo string) (org, name string, ok bool) {
	i := strings.LastIndex(repo, "/")
	if i <= 0 || i == len(repo)-1 {
		return "", "", false
	}
	return repo[:i], repo[i+1:], true
}
