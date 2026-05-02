package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	digest "github.com/opencontainers/go-digest"
	gcrregistry "github.com/google/go-containerregistry/pkg/registry"

	"github.com/dlorenc/docstore/internal/server"
)

// New creates an OCI Distribution Spec registry http.Handler using the given
// blob handler for blob storage.
//
// When store is non-nil, manifests are persisted via the ManifestStore
// (enabling consistent reads across multiple replicas). When store is nil,
// go-containerregistry's default in-memory manifest storage is used
// (suitable for tests and single-replica deployments).
//
// Every request is authenticated with a CI job OIDC token validated against
// the provided JWKS endpoint, audience, and issuer.  The token's repo claim
// is used to restrict access so that a token for org "acme" can only push/pull
// images whose name starts with "acme/".
func New(blobHandler BlobHandler, store ManifestStore, jwksURL, audience, issuer string) http.Handler {
	validate := server.NewJobTokenValidator(jwksURL, audience, issuer)

	inner := gcrregistry.New(
		gcrregistry.WithBlobHandler(blobHandler),
	)

	var handler http.Handler = ociManifest404Middleware(inner)
	if store != nil {
		handler = manifestStoreMiddleware(store, inner)
	}
	// Intercept HEAD /v2/.../blobs/... before go-containerregistry handles it.
	// go-containerregistry checks errors.Is(err, errNotFound) against its own
	// unexported sentinel; our errNotFound is a different pointer so the check
	// fails and it returns 500 instead of 404 for missing blobs.
	handler = blobHeadMiddleware(blobHandler, handler)
	return authMiddleware(handler, validate)
}

const blobUnknownBody = `{"errors":[{"code":"BLOB_UNKNOWN","message":"Unknown blob"}]}` + "\n"

// blobHeadMiddleware intercepts HEAD and GET /v2/{repo}/blobs/{digest} requests
// and serves them directly using BlobHandler, returning the correct 404
// BLOB_UNKNOWN when a blob is absent rather than delegating to
// go-containerregistry's blob handler which returns 500 for missing blobs.
func blobHeadMiddleware(blobHandler BlobHandler, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead && r.Method != http.MethodGet {
			next.ServeHTTP(w, r)
			return
		}

		// Match /v2/{repo}/blobs/{digest}
		repo, dgst, ok := parseBlobPath(r.URL.Path)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}

		h, err := v1.NewHash(dgst)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}

		if r.Method == http.MethodHead {
			size, err := blobHandler.Stat(r.Context(), repo, h)
			if errors.Is(err, errNotFound) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, blobUnknownBody) //nolint:errcheck
				return
			}
			if err != nil {
				http.Error(w, fmt.Sprintf("stat blob: %v", err), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
			w.Header().Set("Docker-Content-Digest", dgst)
			w.WriteHeader(http.StatusOK)
			return
		}

		// GET: stat for size then stream the blob.
		size, err := blobHandler.Stat(r.Context(), repo, h)
		if errors.Is(err, errNotFound) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, blobUnknownBody) //nolint:errcheck
			return
		}
		if err != nil {
			http.Error(w, fmt.Sprintf("stat blob: %v", err), http.StatusInternalServerError)
			return
		}
		rc, err := blobHandler.Get(r.Context(), repo, h)
		if err != nil {
			http.Error(w, fmt.Sprintf("get blob: %v", err), http.StatusInternalServerError)
			return
		}
		defer rc.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.Header().Set("Docker-Content-Digest", dgst)
		w.WriteHeader(http.StatusOK)
		io.Copy(w, rc) //nolint:errcheck
	})
}

// parseBlobPath extracts the repo name and digest from a path of the form
// /v2/{repo}/blobs/{digest}.  Returns ok=false for any other path shape.
func parseBlobPath(path string) (repo, dgst string, ok bool) {
	rest, found := strings.CutPrefix(path, "/v2/")
	if !found {
		return "", "", false
	}
	idx := strings.Index(rest, "/blobs/")
	if idx < 0 {
		return "", "", false
	}
	repo = rest[:idx]
	dgst = rest[idx+len("/blobs/"):]
	if repo == "" || dgst == "" {
		return "", "", false
	}
	return repo, dgst, true
}

// manifestStoreMiddleware intercepts OCI manifest, tags/list, and _catalog
// HTTP routes and serves them using store.  All other routes (blobs, uploads,
// ping) are passed through to next unchanged.
//
// This allows manifests to be stored in a shared durable backend (e.g. GCS)
// so that multiple registry replicas always serve the same data.
func manifestStoreMiddleware(store ManifestStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// /v2/<name>/manifests/<ref>
		if repo, ref, ok := parseManifestPath(path); ok {
			serveManifest(store, repo, ref, w, r)
			return
		}

		// /v2/<name>/tags/list
		if repo, ok := parseTagsListPath(path); ok {
			serveTagsList(store, repo, w, r)
			return
		}

		// /v2/_catalog
		if path == "/v2/_catalog" {
			serveCatalog(store, w, r)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// parseManifestPath parses paths of the form /v2/<name>/manifests/<ref>.
func parseManifestPath(path string) (repo, ref string, ok bool) {
	rest, found := strings.CutPrefix(path, "/v2/")
	if !found {
		return "", "", false
	}
	idx := strings.Index(rest, "/manifests/")
	if idx < 0 {
		return "", "", false
	}
	repo = rest[:idx]
	ref = rest[idx+len("/manifests/"):]
	if repo == "" || ref == "" {
		return "", "", false
	}
	return repo, ref, true
}

// parseTagsListPath parses paths of the form /v2/<name>/tags/list.
func parseTagsListPath(path string) (repo string, ok bool) {
	rest, found := strings.CutPrefix(path, "/v2/")
	if !found {
		return "", false
	}
	repo, found = strings.CutSuffix(rest, "/tags/list")
	if !found || repo == "" {
		return "", false
	}
	return repo, true
}

// serveManifest handles GET, HEAD, PUT, and DELETE for a single manifest.
func serveManifest(store ManifestStore, repo, ref string, w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		content, mediaType, err := store.Get(r.Context(), repo, ref)
		if err != nil {
			if errors.Is(err, errNotFound) {
				slog.Info("manifest not found", "method", r.Method, "repo", repo, "ref", ref)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				if r.Method == http.MethodGet {
					io.WriteString(w, manifestUnknownBody) //nolint:errcheck
				}
				return
			}
			http.Error(w, "manifest store error", http.StatusInternalServerError)
			return
		}
		d := digest.FromBytes(content)
		slog.Info("manifest served", "method", r.Method, "repo", repo, "ref", ref, "digest", d.String(), "media_type", mediaType)
		w.Header().Set("Content-Type", mediaType)
		w.Header().Set("Docker-Content-Digest", d.String())
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			w.Write(content) //nolint:errcheck
		}

	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		mediaType := r.Header.Get("Content-Type")
		if err := store.Put(r.Context(), repo, ref, mediaType, body); err != nil {
			http.Error(w, "manifest store error", http.StatusInternalServerError)
			return
		}
		d := digest.FromBytes(body)
		slog.Info("manifest stored", "method", r.Method, "repo", repo, "ref", ref, "digest", d.String(), "media_type", mediaType)
		w.Header().Set("Docker-Content-Digest", d.String())
		w.Header().Set("Location", "/v2/"+repo+"/manifests/"+d.String())
		w.WriteHeader(http.StatusCreated)

	case http.MethodDelete:
		if err := store.Delete(r.Context(), repo, ref); err != nil {
			http.Error(w, "manifest store error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// serveTagsList handles GET /v2/<repo>/tags/list.
func serveTagsList(store ManifestStore, repo string, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tags, err := store.Tags(r.Context(), repo)
	if err != nil {
		http.Error(w, "manifest store error", http.StatusInternalServerError)
		return
	}
	resp := struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}{Name: repo, Tags: tags}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp) //nolint:errcheck
}

// serveCatalog handles GET /v2/_catalog.
func serveCatalog(store ManifestStore, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	repos, err := store.Repos(r.Context())
	if err != nil {
		http.Error(w, "manifest store error", http.StatusInternalServerError)
		return
	}
	resp := struct {
		Repos []string `json:"repositories"`
	}{Repos: repos}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp) //nolint:errcheck
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
