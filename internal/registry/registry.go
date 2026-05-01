package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	gcrregistry "github.com/google/go-containerregistry/pkg/registry"

	"github.com/dlorenc/docstore/internal/server"
)

// New creates an OCI Distribution Spec registry http.Handler using the given
// blob handler for blob storage.
//
// When manifestStore is non-nil, manifests are stored and retrieved via that
// store (e.g. GCS) so that all replicas share state and pod restarts do not
// lose manifest refs.  When manifestStore is nil, go-containerregistry's
// default in-memory manifest store is used.
//
// Every request is authenticated with a CI job OIDC token validated against
// the provided JWKS endpoint, audience, and issuer.  The token's repo claim
// is used to restrict access so that a token for org "acme" can only push/pull
// images whose name starts with "acme/".
func New(blobHandler BlobHandler, manifestStore ManifestStore, jwksURL, audience, issuer string) http.Handler {
	validate := server.NewJobTokenValidator(jwksURL, audience, issuer)

	inner := gcrregistry.New(
		gcrregistry.WithBlobHandler(blobHandler),
	)

	var outer http.Handler
	if manifestStore != nil {
		outer = newManifestStoreMiddleware(manifestStore, inner)
	} else {
		outer = ociManifest404Middleware(inner)
	}

	return authMiddleware(outer, validate)
}

// writeRegError writes an OCI Distribution Spec JSON error response.
func writeRegError(w http.ResponseWriter, status int, code, message string) {
	type regErr struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	type regErrWrap struct {
		Errors []regErr `json:"errors"`
	}
	body, _ := json.Marshal(regErrWrap{Errors: []regErr{{Code: code, Message: message}}})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	w.Write(body) //nolint:errcheck
}

// newManifestStoreMiddleware returns an http.Handler that intercepts
// manifest, tags, and catalog requests and serves them from store.  All
// other requests are delegated to next.
func newManifestStoreMiddleware(store ManifestStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		if isManifest(r) {
			elem := strings.Split(r.URL.Path, "/")
			elem = elem[1:]
			ref := elem[len(elem)-1]
			repo := strings.Join(elem[1:len(elem)-2], "/")
			serveManifest(ctx, store, w, r, repo, ref)
			return
		}

		if isTags(r) && r.Method == http.MethodGet {
			elem := strings.Split(r.URL.Path, "/")
			elem = elem[1:]
			repo := strings.Join(elem[1:len(elem)-2], "/")
			serveTagsList(ctx, store, w, r, repo)
			return
		}

		if isCatalog(r) && r.Method == http.MethodGet {
			serveCatalog(ctx, store, w, r)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// isManifest is copied from go-containerregistry's unexported helper.
func isManifest(req *http.Request) bool {
	elems := strings.Split(req.URL.Path, "/")
	elems = elems[1:]
	if len(elems) < 4 {
		return false
	}
	return elems[len(elems)-2] == "manifests"
}

// isTags is copied from go-containerregistry's unexported helper.
func isTags(req *http.Request) bool {
	elems := strings.Split(req.URL.Path, "/")
	elems = elems[1:]
	if len(elems) < 4 {
		return false
	}
	return elems[len(elems)-2] == "tags"
}

// isCatalog is copied from go-containerregistry's unexported helper.
func isCatalog(req *http.Request) bool {
	elems := strings.Split(req.URL.Path, "/")
	elems = elems[1:]
	if len(elems) < 2 {
		return false
	}
	return elems[len(elems)-1] == "_catalog"
}

func serveManifest(ctx context.Context, store ManifestStore, w http.ResponseWriter, r *http.Request, repo, ref string) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		data, contentType, err := store.Get(ctx, repo, ref)
		if err != nil {
			if errors.Is(err, errNotFound) {
				writeRegError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "Unknown manifest")
				return
			}
			writeRegError(w, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", err.Error())
			return
		}
		h, _, _ := v1.SHA256(bytes.NewReader(data))
		w.Header().Set("Docker-Content-Digest", h.String())
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			w.Write(data) //nolint:errcheck
		}

	case http.MethodPut:
		data, err := io.ReadAll(r.Body)
		if err != nil {
			writeRegError(w, http.StatusBadRequest, "BLOB_UPLOAD_INVALID", "failed to read body")
			return
		}
		contentType := r.Header.Get("Content-Type")
		h, _, _ := v1.SHA256(bytes.NewReader(data))
		digest := h.String()
		if err := store.Put(ctx, repo, ref, data, contentType); err != nil {
			writeRegError(w, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", err.Error())
			return
		}
		// Also index by digest so immutable digest refs resolve.
		if ref != digest {
			if err := store.Put(ctx, repo, digest, data, contentType); err != nil {
				writeRegError(w, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", err.Error())
				return
			}
		}
		w.Header().Set("Docker-Content-Digest", digest)
		w.WriteHeader(http.StatusCreated)

	case http.MethodDelete:
		if err := store.Delete(ctx, repo, ref); err != nil {
			if errors.Is(err, errNotFound) {
				writeRegError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "Unknown manifest")
				return
			}
			writeRegError(w, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", err.Error())
			return
		}
		w.WriteHeader(http.StatusAccepted)

	default:
		writeRegError(w, http.StatusMethodNotAllowed, "METHOD_UNKNOWN", "unsupported method")
	}
}

func serveTagsList(ctx context.Context, store ManifestStore, w http.ResponseWriter, r *http.Request, repo string) {
	tags, err := store.Tags(ctx, repo)
	if err != nil {
		writeRegError(w, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", err.Error())
		return
	}

	// Apply "last" pagination offset.
	if last := r.URL.Query().Get("last"); last != "" {
		for i, t := range tags {
			if t > last {
				tags = tags[i:]
				break
			}
		}
	}
	// Apply "n" limit.
	if ns := r.URL.Query().Get("n"); ns != "" {
		if n, err := strconv.Atoi(ns); err == nil && n < len(tags) {
			tags = tags[:n]
		}
	}

	type listTags struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	body, _ := json.Marshal(listTags{Name: repo, Tags: tags})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	w.Write(body) //nolint:errcheck
}

func serveCatalog(ctx context.Context, store ManifestStore, w http.ResponseWriter, r *http.Request) {
	repos, err := store.Repos(ctx)
	if err != nil {
		writeRegError(w, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", err.Error())
		return
	}

	// Apply "n" limit.
	if ns := r.URL.Query().Get("n"); ns != "" {
		if n, err := strconv.Atoi(ns); err == nil && n < len(repos) {
			repos = repos[:n]
		}
	}

	type catalog struct {
		Repos []string `json:"repositories"`
	}
	body, _ := json.Marshal(catalog{Repos: repos})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	w.Write(body) //nolint:errcheck
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
