package server

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// contextKey is the type for context keys in this package.
type contextKey string

const identityKey contextKey = "identity"

// IdentityFromContext returns the authenticated identity stored in the context
// by the IAP middleware. Returns empty string if not set.
func IdentityFromContext(ctx context.Context) string {
	v, _ := ctx.Value(identityKey).(string)
	return v
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
				ctx := context.WithValue(r.Context(), identityKey, devIdentity)
				next.ServeHTTP(w, r.WithContext(ctx))
			})
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := r.Header.Get("X-Goog-IAP-JWT-Assertion")
			if token == "" {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
				return
			}
			email, err := validateIAPJWT(token, fetchKey)
			if err != nil {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
				return
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

