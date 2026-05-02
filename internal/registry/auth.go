package registry

import (
	"encoding/base64"
	"log/slog"
	"net/http"
	"strings"

	"github.com/dlorenc/docstore/internal/server"
)

// authMiddleware wraps an OCI registry http.Handler with CI job OIDC token
// authentication.  For every request:
//
//   - If no Authorization: Bearer <token> header is present, returns 401.
//   - If the token is invalid, returns 401.
//   - If the image name extracted from the URL path does not start with the
//     org derived from the token's repo claim, returns 403.
//   - Otherwise the request is forwarded to the inner registry handler.
func authMiddleware(inner http.Handler, validate func(string) (*server.JobIdentity, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")

		// Accept Bearer tokens directly (used by CI workers) and Basic auth
		// where the password is the OIDC token (used by BuildKit's docker auth
		// provider reading from ~/.docker/config.json).
		var tokenStr string
		switch {
		case strings.HasPrefix(auth, "Bearer "):
			tokenStr = strings.TrimPrefix(auth, "Bearer ")
		case strings.HasPrefix(auth, "Basic "):
			b64 := strings.TrimPrefix(auth, "Basic ")
			if decoded, err := base64.StdEncoding.DecodeString(b64); err == nil {
				// Format is "username:password"; use the password as the bearer token.
				_, pass, _ := strings.Cut(string(decoded), ":")
				tokenStr = pass
			}
		}

		if tokenStr == "" {
			// Send a Basic realm challenge. BuildKit reads Basic credentials
			// from the docker config (username "ci-worker", password = OIDC
			// token). Bearer challenges cause BuildKit to POST to the realm
			// as a token URL, which fails when the realm is not an HTTP URL.
			slog.Warn("auth: missing token", "method", r.Method, "path", r.URL.Path)
			w.Header().Set("WWW-Authenticate", `Basic realm="ci-registry"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}

		identity, err := validate(tokenStr)
		if err != nil {
			slog.Warn("auth: invalid token", "method", r.Method, "path", r.URL.Path, "error", err)
			w.Header().Set("WWW-Authenticate", `Basic realm="ci-registry"`)
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}

		// Derive the org from the repo claim: e.g. "acme/myrepo" → "acme".
		org, _, _ := strings.Cut(identity.Repo, "/")
		if org == "" {
			http.Error(w, "invalid token: missing org in repo claim", http.StatusForbidden)
			return
		}

		// Extract the image name from the request path. OCI distribution spec
		// paths are /v2/<name>/blobs/... or /v2/<name>/manifests/...
		// We take everything between "/v2/" and the next action segment.
		imageName := imageNameFromPath(r.URL.Path)
		if imageName == "" {
			// /v2/ ping endpoint or unknown path — allow through.
			inner.ServeHTTP(w, r)
			return
		}

		// Check that the image belongs to the token's org.
		if !strings.HasPrefix(imageName, org+"/") {
			slog.Warn("auth: forbidden", "method", r.Method, "path", r.URL.Path, "org", org, "image", imageName)
			http.Error(w, "forbidden: image not in token org", http.StatusForbidden)
			return
		}

		inner.ServeHTTP(w, r)
	})
}

// imageNameFromPath extracts the image repository name from an OCI
// Distribution Spec URL path, e.g.:
//
//	/v2/acme/myrepo/blobs/sha256:abc    → "acme/myrepo"
//	/v2/acme/myrepo/manifests/latest    → "acme/myrepo"
//	/v2/                                → "" (ping endpoint)
//	/v2/_catalog                        → "" (registry-level endpoint)
func imageNameFromPath(path string) string {
	const prefix = "/v2/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(path, prefix)
	if rest == "" || rest == "/" {
		return ""
	}
	// _catalog is a registry-level endpoint, not scoped to a repo.
	if rest == "_catalog" || strings.HasPrefix(rest, "_catalog?") {
		return ""
	}
	// Find the action segment: blobs, manifests, tags, referrers, _catalog, uploads.
	actions := []string{"/blobs/", "/blobs/uploads/", "/manifests/", "/tags/", "/referrers/", "/_catalog"}
	for _, action := range actions {
		if idx := strings.Index(rest, action); idx != -1 {
			return rest[:idx]
		}
	}
	// Unknown sub-path — return the full rest.
	return rest
}
