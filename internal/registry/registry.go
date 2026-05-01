package registry

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"

	gcrregistry "github.com/google/go-containerregistry/pkg/registry"

	"github.com/dlorenc/docstore/internal/server"
)

// New creates an OCI Distribution Spec registry http.Handler using the given
// blob handler for blob storage (manifests are stored in-memory).
//
// Every request is authenticated with a CI job OIDC token validated against
// the provided JWKS endpoint, audience, and issuer.  The token's repo claim
// is used to restrict access so that a token for org "acme" can only push/pull
// images whose name starts with "acme/".
func New(blobHandler BlobHandler, jwksURL, audience, issuer string) http.Handler {
	validate := server.NewJobTokenValidator(jwksURL, audience, issuer)

	inner := gcrregistry.New(
		gcrregistry.WithBlobHandler(blobHandler),
	)

	return authMiddleware(ociManifest404Middleware(inner), validate)
}

// responseRecorder is a minimal http.ResponseWriter that buffers the response
// status code and body so the calling middleware can inspect and optionally
// rewrite the response before flushing it to the real writer.
//
// Note: Header() calls are NOT intercepted — they operate directly on the
// underlying ResponseWriter's header map, which the caller can modify between
// the inner ServeHTTP call and the flush.
type responseRecorder struct {
	http.ResponseWriter
	code int
	body bytes.Buffer
}

func (r *responseRecorder) WriteHeader(code int) { r.code = code }
func (r *responseRecorder) Write(b []byte) (int, error) {
	return r.body.Write(b)
}

// flush writes the buffered status and body to the underlying ResponseWriter.
func (r *responseRecorder) flush() {
	code := r.code
	if code == 0 {
		code = http.StatusOK
	}
	r.ResponseWriter.WriteHeader(code)
	r.ResponseWriter.Write(r.body.Bytes()) //nolint:errcheck
}

const manifestUnknownBody = `{"errors":[{"code":"MANIFEST_UNKNOWN","message":"Unknown manifest"}]}` + "\n"

// ociManifest404Middleware ensures that any 404 response from a manifest
// endpoint carries an OCI Distribution Spec–compliant JSON error body with the
// MANIFEST_UNKNOWN error code.
//
// go-containerregistry returns NAME_UNKNOWN (not MANIFEST_UNKNOWN) when a
// repository has no manifests yet. BuildKit v0.29.x treats MANIFEST_UNKNOWN as
// "no existing cache, proceed with full push" but may not handle NAME_UNKNOWN
// gracefully, causing the cache export to abort on the first-ever push.
func ociManifest404Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/manifests/") {
			next.ServeHTTP(w, r)
			return
		}
		rec := &responseRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.code == http.StatusNotFound {
			w.Header().Set("Content-Type", "application/json")
			if r.Method != http.MethodHead {
				w.Header().Set("Content-Length", strconv.Itoa(len(manifestUnknownBody)))
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, manifestUnknownBody) //nolint:errcheck
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
			return
		}
		rec.flush()
	})
}
