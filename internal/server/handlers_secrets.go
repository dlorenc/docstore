package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

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
