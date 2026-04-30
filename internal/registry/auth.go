package registry

import (
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
		tokenStr := strings.TrimPrefix(auth, "Bearer ")
		if tokenStr == "" || tokenStr == auth {
			w.Header().Set("WWW-Authenticate", `Bearer realm="ci-registry"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}

		identity, err := validate(tokenStr)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="ci-registry"`)
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
func imageNameFromPath(path string) string {
	const prefix = "/v2/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(path, prefix)
	if rest == "" || rest == "/" {
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
