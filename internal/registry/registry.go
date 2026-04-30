package registry

import (
	"net/http"

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

	return authMiddleware(inner, validate)
}
