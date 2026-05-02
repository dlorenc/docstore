package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dlorenc/docstore/internal/archivesign"
	"github.com/dlorenc/docstore/internal/citoken"
	"github.com/dlorenc/docstore/internal/db"
)

// handleArchivePresign implements POST /repos/{repo}/-/archive/presign.
// Authenticated via request_token (Authorization: Bearer <plaintext>).
// Returns a presigned URL for BuildKit to fetch the archive.
func (s *server) handleArchivePresign(w http.ResponseWriter, r *http.Request) {
	if s.archiveHMACSecret == nil || s.jobTokenStore == nil {
		writeError(w, http.StatusServiceUnavailable, "presigned archives not configured")
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

	// 3. Generate presigned URL with 1-hour expiry.
	expiresUnix := time.Now().Add(time.Hour).Unix()
	sig := archivesign.Sign(s.archiveHMACSecret, job.Repo, job.Branch, job.Sequence, expiresUnix)

	presignedURL := fmt.Sprintf("%s/repos/%s/-/archive?branch=%s&at=%d&expires=%d&sig=%s",
		s.archiveBaseURL,
		job.Repo,
		url.QueryEscape(job.Branch),
		job.Sequence,
		expiresUnix,
		sig,
	)

	// Compute a content checksum so BuildKit can cache by content rather than
	// URL (presigned URLs include an expiry timestamp and change every run).
	// Skip if the read store is not configured.
	checksum := ""
	if s.readStore != nil {
		h := sha256.New()
		if err := writeArchive(r.Context(), s.readStore, h, job.Repo, job.Branch, job.Sequence); err != nil {
			slog.Warn("presign: checksum computation failed", "repo", job.Repo, "branch", job.Branch, "error", err)
		} else {
			checksum = "sha256:" + hex.EncodeToString(h.Sum(nil))
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"url": presignedURL, "checksum": checksum}) //nolint:errcheck
}

// handlePresignedArchive serves GET /repos/{repo}/-/archive when the sig and expires
// query params are present. Validates the HMAC signature and expiry before serving.
func (s *server) handlePresignedArchive(w http.ResponseWriter, r *http.Request) {
	if s.archiveHMACSecret == nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	repo := r.PathValue("name")
	branch := r.URL.Query().Get("branch")
	atStr := r.URL.Query().Get("at")
	expiresStr := r.URL.Query().Get("expires")
	sig := r.URL.Query().Get("sig")

	if branch == "" || atStr == "" || expiresStr == "" || sig == "" {
		writeError(w, http.StatusBadRequest, "missing required query parameters")
		return
	}

	sequence, err := strconv.ParseInt(atStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid at parameter")
		return
	}
	expiresUnix, err := strconv.ParseInt(expiresStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid expires parameter")
		return
	}

	// Check expiry.
	if time.Now().Unix() > expiresUnix {
		http.Error(w, "expired", http.StatusForbidden)
		return
	}

	// Verify HMAC using constant-time comparison.
	if !archivesign.Verify(s.archiveHMACSecret, repo, branch, sequence, expiresUnix, sig) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// Serve the archive by delegating to the existing archive handler.
	// The "at", "branch" params are already in the query string, and
	// "name" path value is already set on the request.
	s.handleArchive(w, r)
}
