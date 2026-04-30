package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/dlorenc/docstore/internal/citoken"
	"github.com/dlorenc/docstore/internal/db"
)

// handleCheckLogs implements POST /repos/:repo/-/check/:checkName/logs.
// Authenticated via request_token (Authorization: Bearer <plaintext>).
// Reads the log body as text/plain and writes to GCS via the logStore.
// Returns JSON {"url": "<gs://...>"} on success.
func (s *server) handleCheckLogs(w http.ResponseWriter, r *http.Request) {
	if s.logStore == nil || s.jobTokenStore == nil {
		writeError(w, http.StatusServiceUnavailable, "log upload not configured")
		return
	}

	// 1. Extract and validate request_token from Authorization header.
	authHdr := r.Header.Get("Authorization")
	plaintext := strings.TrimPrefix(authHdr, "Bearer ")
	if plaintext == "" || plaintext == authHdr {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	hashed := citoken.HashRequestToken(plaintext)
	job, err := s.jobTokenStore.LookupRequestToken(r.Context(), hashed)
	if errors.Is(err, db.ErrTokenInvalid) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// 2. Verify repo in token matches the request path.
	repoName := r.PathValue("name")
	if job.Repo != repoName {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// 3. Extract check name from path value.
	checkName := r.PathValue("checkName")
	if checkName == "" {
		writeError(w, http.StatusBadRequest, "check name required")
		return
	}

	// 4. Read log body (limit to 10 MB).
	const maxLogSize = 10 << 20
	body, err := io.ReadAll(io.LimitReader(r.Body, maxLogSize))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// 5. Write logs to the store using job metadata from the token.
	logURL, err := s.logStore.Write(r.Context(), job.Repo, job.Branch, job.Sequence, checkName, string(body))
	if err != nil {
		slog.Error("log write failed", "repo", job.Repo, "check", checkName, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"url": logURL}) //nolint:errcheck
}
