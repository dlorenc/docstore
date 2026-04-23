package server

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dlorenc/docstore/internal/model"
)

// contextKey is the type for context keys in this package.
type contextKey string

const identityKey contextKey = "identity"
const roleKey contextKey = "role"

// IdentityFromContext returns the authenticated identity stored in the context
// by the IAP middleware. Returns empty string if not set.
func IdentityFromContext(ctx context.Context) string {
	v, _ := ctx.Value(identityKey).(string)
	return v
}

// RoleFromContext returns the RBAC role stored in context by RBACMiddleware.
// Returns empty string if not set (RBAC not active).
func RoleFromContext(ctx context.Context) model.RoleType {
	v, _ := ctx.Value(roleKey).(model.RoleType)
	return v
}

// --- Request logger ---

// requestLog is a mutable capture placed in context by RequestLogger so that
// inner middleware (IAPMiddleware) can write the authenticated identity back
// for use in the access log.
type requestLog struct {
	identity string
}

type requestLogKey struct{}

func requestLogFromContext(ctx context.Context) *requestLog {
	v, _ := ctx.Value(requestLogKey{}).(*requestLog)
	return v
}

// responseWriter wraps http.ResponseWriter to capture the status code and bytes written.
type responseWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (rw *responseWriter) WriteHeader(status int) {
	rw.status = status
	rw.ResponseWriter.WriteHeader(status)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	n, err := rw.ResponseWriter.Write(b)
	rw.bytes += n
	return n, err
}

// Flush implements http.Flusher by forwarding to the underlying writer if it
// supports flushing. This is required for SSE streaming to work through the
// RequestLogger middleware.
func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// repoFromPath extracts the repo name from any /repos/:name... URL path.
// Returns "" for non-repo paths.
func repoFromPath(path string) string {
	const prefix = "/repos/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	rest := path[len(prefix):]
	repo, _, _ := strings.Cut(rest, "/")
	return repo
}

// RequestLogger returns an HTTP middleware that logs one structured line per
// request on completion. It uses a requestLog capture in the context so that
// IAPMiddleware can write back the authenticated identity.
func RequestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rl := &requestLog{}
		r = r.WithContext(context.WithValue(r.Context(), requestLogKey{}, rl))
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rw, r)

		durationMs := time.Since(start).Milliseconds()
		repo := repoFromPath(r.URL.Path)

		args := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration_ms", durationMs,
			"identity", rl.identity,
			"repo", repo,
			"bytes", rw.bytes,
		}

		switch {
		case rw.status >= 500:
			slog.Error("request", args...)
		case rw.status >= 400:
			slog.Warn("request", args...)
		default:
			slog.Info("request", args...)
		}
	})
}

// --- RBAC middleware ---

// RoleStore is the minimal interface the RBAC middleware requires.
type RoleStore interface {
	GetRole(ctx context.Context, repo, identity string) (*model.Role, error)
	HasAdmin(ctx context.Context, repo string) (bool, error)
}

// RBACMiddleware returns an HTTP middleware that enforces role-based access
// control for repo-scoped routes. It must run after IAPMiddleware so that
// the identity is already in context.
//
// bootstrapAdmin, if non-empty, is granted full admin access for any repo
// that has no admin assigned yet. Once a repo has an admin, the bootstrap
// flag is ignored for that repo.
func RBACMiddleware(roles RoleStore, bootstrapAdmin string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			repo, subPath := repoAndSubPath(r.URL.Path)
			// Non-repo-scoped paths (e.g. /repos, /healthz) skip RBAC.
			if repo == "" || subPath == "" {
				next.ServeHTTP(w, r)
				return
			}

			identity := IdentityFromContext(r.Context())

			// Bootstrap admin: bypass when no admin exists for this repo.
			if bootstrapAdmin != "" && identity == bootstrapAdmin {
				hasAdmin, err := roles.HasAdmin(r.Context(), repo)
				if err == nil && !hasAdmin {
					slog.Info("bootstrap admin granted", "identity", identity, "repo", repo)
					ctx := context.WithValue(r.Context(), roleKey, model.RoleAdmin)
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
				// An admin already exists — fall through to normal role check.
			}

			role, err := roles.GetRole(r.Context(), repo, identity)
			if err != nil {
				slog.Warn("access denied", "identity", identity, "repo", repo, "reason", "no_role", "path", r.URL.Path)
				writeAPIError(w, ErrCodeForbidden, http.StatusForbidden, "forbidden")
				return
			}

			if !roleAllows(role.Role, r.Method, subPath, r) {
				slog.Warn("access denied", "identity", identity, "repo", repo, "role", role.Role, "method", r.Method, "sub_path", subPath, "path", r.URL.Path)
				writeAPIError(w, ErrCodeForbidden, http.StatusForbidden, "forbidden")
				return
			}

			ctx := context.WithValue(r.Context(), roleKey, role.Role)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// repoAndSubPath extracts the repo name and sub-path from a /repos/.../-/subpath URL.
// Returns ("", "") for non-repo paths or bare /repos/:repopath paths with no /-/ separator.
//
// Examples:
//
//	/repos/acme/myrepo/-/commit  →  ("acme/myrepo", "commit")
//	/repos/acme/team/sub/-/tree  →  ("acme/team/sub", "tree")
//	/repos/acme/myrepo            →  ("", "")  — bare repo, no RBAC check needed
//
// NOTE: parseRepoPath in handlers.go parses the same "/-/" URL format for routing.
// Both functions must be kept in sync if the URL structure changes.
func repoAndSubPath(path string) (repo, subPath string) {
	const prefix = "/repos/"
	if !strings.HasPrefix(path, prefix) {
		return "", ""
	}
	rest := path[len(prefix):]
	repo, subPath, found := strings.Cut(rest, "/-/")
	if !found {
		// /repos/:repopath with no /-/ — no sub-path, no RBAC check needed.
		return "", ""
	}
	return repo, subPath
}

// roleAllows checks whether the given role is permitted to execute the HTTP
// method on subPath. For writer+POST /commit, it peeks at the request body
// to enforce the "no direct commits to main" rule.
func roleAllows(role model.RoleType, method, subPath string, r *http.Request) bool {
	// Roles management endpoints — admin only.
	if subPath == "roles" || strings.HasPrefix(subPath, "roles/") {
		return role == model.RoleAdmin
	}

	// DELETE /releases/<name> — admin only.
	if method == http.MethodDelete && strings.HasPrefix(subPath, "releases/") {
		return role == model.RoleAdmin
	}

	// POST /releases — maintainer+.
	if method == http.MethodPost && subPath == "releases" {
		return role == model.RoleMaintainer || role == model.RoleAdmin
	}

	// All other GET endpoints — reader+.
	if method == http.MethodGet {
		return true
	}

	// POST /commit — writer+.
	if method == http.MethodPost && subPath == "commit" {
		if role != model.RoleWriter && role != model.RoleMaintainer && role != model.RoleAdmin {
			return false
		}
		// Writers may not commit directly to main.
		if role == model.RoleWriter && commitTargetsMain(r) {
			return false
		}
		return true
	}

	// POST /comment — writer+.
	if method == http.MethodPost && subPath == "comment" {
		return role == model.RoleWriter || role == model.RoleMaintainer || role == model.RoleAdmin
	}

	// DELETE /comment/* — writer+ minimum; handler additionally checks author identity or maintainer role.
	if method == http.MethodDelete && strings.HasPrefix(subPath, "comment/") {
		return role == model.RoleWriter || role == model.RoleMaintainer || role == model.RoleAdmin
	}

	// POST /proposals — writer+.
	if method == http.MethodPost && subPath == "proposals" {
		return role == model.RoleWriter || role == model.RoleMaintainer || role == model.RoleAdmin
	}

	// PATCH /proposals/* — writer+ minimum; handler additionally checks author or maintainer.
	if method == http.MethodPatch && strings.HasPrefix(subPath, "proposals/") {
		return role == model.RoleWriter || role == model.RoleMaintainer || role == model.RoleAdmin
	}

	// POST /proposals/*/close — writer+ minimum; handler additionally checks author or maintainer.
	if method == http.MethodPost && strings.HasPrefix(subPath, "proposals/") && strings.HasSuffix(subPath, "/close") {
		return role == model.RoleWriter || role == model.RoleMaintainer || role == model.RoleAdmin
	}

	// Issue endpoints.
	// POST /issues — writer+.
	if method == http.MethodPost && subPath == "issues" {
		return role == model.RoleWriter || role == model.RoleMaintainer || role == model.RoleAdmin
	}
	// POST /issues/* (close, reopen, comments, refs) — writer+.
	if method == http.MethodPost && strings.HasPrefix(subPath, "issues/") {
		return role == model.RoleWriter || role == model.RoleMaintainer || role == model.RoleAdmin
	}
	// PATCH /issues/* — writer+ minimum; handler additionally checks author or maintainer.
	if method == http.MethodPatch && strings.HasPrefix(subPath, "issues/") {
		return role == model.RoleWriter || role == model.RoleMaintainer || role == model.RoleAdmin
	}
	// DELETE /issues/* — maintainer+.
	if method == http.MethodDelete && strings.HasPrefix(subPath, "issues/") {
		return role == model.RoleMaintainer || role == model.RoleAdmin
	}

	// POST /branch, /merge, /rebase — maintainer+.
	if method == http.MethodPost && (subPath == "branch" || subPath == "merge" || subPath == "rebase") {
		return role == model.RoleMaintainer || role == model.RoleAdmin
	}

	// POST/DELETE /branch/*/auto-merge — writer+.
	if strings.HasPrefix(subPath, "branch/") && strings.HasSuffix(subPath, "/auto-merge") {
		return role == model.RoleWriter || role == model.RoleMaintainer || role == model.RoleAdmin
	}

	// DELETE /branch/* — maintainer+.
	if method == http.MethodDelete && strings.HasPrefix(subPath, "branch/") {
		return role == model.RoleMaintainer || role == model.RoleAdmin
	}

	// PATCH /branch/* (draft promotion) — writer+.
	if method == http.MethodPatch && strings.HasPrefix(subPath, "branch/") {
		return role == model.RoleWriter || role == model.RoleMaintainer || role == model.RoleAdmin
	}

	return false
}

// commitTargetsMain peeks at the JSON request body to see whether the commit
// is targeting the "main" branch. It replaces r.Body so the handler can still
// read it.
func commitTargetsMain(r *http.Request) bool {
	if r.Body == nil {
		return false
	}
	raw, err := io.ReadAll(r.Body)
	r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(raw))
	if err != nil {
		return false
	}
	var req struct {
		Branch string `json:"branch"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return false
	}
	return req.Branch == "main"
}

// IAPMiddleware returns an HTTP middleware that validates GCP IAP JWTs from the
// X-Goog-IAP-JWT-Assertion header. If devIdentity is non-empty, JWT validation is
// skipped and devIdentity is used directly (for local dev/testing).
func IAPMiddleware(devIdentity string) func(http.Handler) http.Handler {
	cache := newKeyCache()
	return newMiddleware(devIdentity, cache.get)
}

// newMiddleware is the testable core of IAPMiddleware. fetchKey is called with a
// key ID and returns the corresponding RSA public key.
func newMiddleware(devIdentity string, fetchKey func(kid string) (*rsa.PublicKey, error)) func(http.Handler) http.Handler {
	if devIdentity != "" {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if rl := requestLogFromContext(r.Context()); rl != nil {
					rl.identity = devIdentity
				}
				ctx := context.WithValue(r.Context(), identityKey, devIdentity)
				next.ServeHTTP(w, r.WithContext(ctx))
			})
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := r.Header.Get("X-Goog-IAP-JWT-Assertion")
			if token == "" {
				slog.Warn("auth failed", "reason", "missing_token", "path", r.URL.Path)
				writeAPIError(w, ErrCodeUnauthorized, http.StatusUnauthorized, "unauthenticated")
				return
			}
			email, err := validateIAPJWT(token, fetchKey)
			if err != nil {
				slog.Warn("auth failed", "reason", "invalid_jwt", "path", r.URL.Path, "error", err)
				writeAPIError(w, ErrCodeUnauthorized, http.StatusUnauthorized, "unauthenticated")
				return
			}
			if rl := requestLogFromContext(r.Context()); rl != nil {
				rl.identity = email
			}
			ctx := context.WithValue(r.Context(), identityKey, email)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// validateIAPJWT parses and validates an IAP RS256 JWT, returning the email claim.
func validateIAPJWT(tokenString string, fetchKey func(kid string) (*rsa.PublicKey, error)) (string, error) {
	parts := strings.SplitN(tokenString, ".", 3)
	if len(parts) != 3 {
		return "", fmt.Errorf("invalid JWT format")
	}

	// Parse header to get algorithm and key ID.
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", fmt.Errorf("decode header: %w", err)
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return "", fmt.Errorf("parse header: %w", err)
	}
	if header.Alg != "RS256" {
		return "", fmt.Errorf("unsupported algorithm: %s", header.Alg)
	}
	if header.Kid == "" {
		return "", fmt.Errorf("missing kid")
	}

	// Fetch the public key for this key ID.
	key, err := fetchKey(header.Kid)
	if err != nil {
		return "", fmt.Errorf("fetch key: %w", err)
	}

	// Verify the RS256 signature over header.payload.
	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", fmt.Errorf("decode signature: %w", err)
	}
	signingInput := parts[0] + "." + parts[1]
	digest := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sigBytes); err != nil {
		return "", fmt.Errorf("invalid signature: %w", err)
	}

	// Parse payload claims.
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode payload: %w", err)
	}
	var claims struct {
		Email string  `json:"email"`
		Exp   float64 `json:"exp"`
	}
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		return "", fmt.Errorf("parse payload: %w", err)
	}

	// Check expiry.
	if time.Now().Unix() > int64(claims.Exp) {
		return "", fmt.Errorf("token expired")
	}

	if claims.Email == "" {
		return "", fmt.Errorf("missing email claim")
	}
	return claims.Email, nil
}

// --- JWK key cache ---

const iapJWKURL = "https://www.gstatic.com/iap/verify/public_key-jwk"

type keyCache struct {
	mu        sync.RWMutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
	ttl       time.Duration
}

func newKeyCache() *keyCache {
	return &keyCache{
		ttl: time.Hour,
	}
}

func (c *keyCache) get(kid string) (*rsa.PublicKey, error) {
	c.mu.RLock()
	if c.keys != nil && time.Since(c.fetchedAt) < c.ttl {
		key, ok := c.keys[kid]
		c.mu.RUnlock()
		if ok {
			return key, nil
		}
		return nil, fmt.Errorf("key %q not found", kid)
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	// Re-check under write lock.
	if c.keys != nil && time.Since(c.fetchedAt) < c.ttl {
		if key, ok := c.keys[kid]; ok {
			return key, nil
		}
		return nil, fmt.Errorf("key %q not found", kid)
	}

	if err := c.refresh(); err != nil {
		return nil, fmt.Errorf("refresh keys: %w", err)
	}
	key, ok := c.keys[kid]
	if !ok {
		return nil, fmt.Errorf("key %q not found", kid)
	}
	return key, nil
}

func (c *keyCache) refresh() error {
	resp, err := http.Get(iapJWKURL) //nolint:noctx
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var jwks struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return err
	}

	keys := make(map[string]*rsa.PublicKey, len(jwks.Keys))
	for _, k := range jwks.Keys {
		if k.Kty != "RSA" {
			continue
		}
		pub, err := jwkToRSA(k.N, k.E)
		if err != nil {
			continue
		}
		keys[k.Kid] = pub
	}
	c.keys = keys
	c.fetchedAt = time.Now()
	return nil
}

// jwkToRSA converts base64url-encoded JWK n and e values to an *rsa.PublicKey.
func jwkToRSA(nB64, eB64 string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nB64)
	if err != nil {
		return nil, fmt.Errorf("decode n: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eB64)
	if err != nil {
		return nil, fmt.Errorf("decode e: %w", err)
	}
	n := new(big.Int).SetBytes(nBytes)
	var e int
	for _, b := range eBytes {
		e = e<<8 | int(b)
	}
	return &rsa.PublicKey{N: n, E: e}, nil
}
