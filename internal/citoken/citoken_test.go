package citoken_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/dlorenc/docstore/internal/citoken"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jws"
	"github.com/lestrrat-go/jwx/v3/jwt"
)

// TestNewKMSSigner_Compile verifies that *KMSSigner satisfies the Signer interface
// at compile time. No network calls are made; the constructor would fail at runtime
// without real KMS credentials.
func TestNewKMSSigner_Compile(t *testing.T) {
	t.Parallel()
	var _ citoken.Signer = (*citoken.KMSSigner)(nil)
}

func TestIssueJWT(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	signer, err := citoken.NewLocalSigner()
	if err != nil {
		t.Fatalf("NewLocalSigner: %v", err)
	}

	claims := citoken.JobClaims{
		Issuer:      "https://docstore.dev",
		Subject:     "repo:acme/myrepo:branch:main:check:lint",
		Audience:    "https://example.com",
		Repo:        "acme/myrepo",
		Org:         "acme",
		Branch:      "main",
		CheckName:   "lint",
		RefType:     "post-submit",
		TriggeredBy: "user@example.com",
		JobID:       "job-123",
		Sequence:    42,
	}

	tokenStr, err := citoken.IssueJWT(ctx, signer, claims)
	if err != nil {
		t.Fatalf("IssueJWT: %v", err)
	}
	if tokenStr == "" {
		t.Fatal("expected non-empty token string")
	}

	// Fetch the public keys to verify the token.
	jwksJSON, err := signer.PublicKeys(ctx)
	if err != nil {
		t.Fatalf("PublicKeys: %v", err)
	}
	keySet, err := jwk.Parse(jwksJSON)
	if err != nil {
		t.Fatalf("parse JWKS: %v", err)
	}

	// Parse and verify the JWT.
	tok, err := jwt.Parse([]byte(tokenStr), jwt.WithKeySet(keySet, jws.WithInferAlgorithmFromKey(true)))
	if err != nil {
		t.Fatalf("jwt.Parse: %v", err)
	}

	// Verify standard claims.
	iss, ok := tok.Issuer()
	if !ok || iss != claims.Issuer {
		t.Errorf("iss = %q, want %q", iss, claims.Issuer)
	}
	sub, ok := tok.Subject()
	if !ok || sub != claims.Subject {
		t.Errorf("sub = %q, want %q", sub, claims.Subject)
	}
	aud, ok := tok.Audience()
	if !ok || len(aud) == 0 || aud[0] != claims.Audience {
		t.Errorf("aud = %v, want [%q]", aud, claims.Audience)
	}
	jti, ok := tok.JwtID()
	if !ok || jti == "" {
		t.Error("expected non-empty jti")
	}

	// Verify custom claims.
	assertStringClaim(t, tok, "repo", claims.Repo)
	assertStringClaim(t, tok, "org", claims.Org)
	assertStringClaim(t, tok, "branch", claims.Branch)
	assertStringClaim(t, tok, "check_name", claims.CheckName)
	assertStringClaim(t, tok, "ref_type", claims.RefType)
	assertStringClaim(t, tok, "triggered_by", claims.TriggeredBy)
	assertStringClaim(t, tok, "job_id", claims.JobID)

	var seq float64
	if err := tok.Get("sequence", &seq); err != nil {
		t.Errorf("get sequence claim: %v", err)
	} else if int64(seq) != claims.Sequence {
		t.Errorf("sequence = %v, want %d", seq, claims.Sequence)
	}
}

func assertStringClaim(t *testing.T, tok jwt.Token, key, want string) {
	t.Helper()
	var got string
	if err := tok.Get(key, &got); err != nil {
		t.Errorf("get claim %q: %v", key, err)
		return
	}
	if got != want {
		t.Errorf("claim %q = %q, want %q", key, got, want)
	}
}

func TestJWKS(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	signer, err := citoken.NewLocalSigner()
	if err != nil {
		t.Fatalf("NewLocalSigner: %v", err)
	}

	jwksJSON, err := signer.PublicKeys(ctx)
	if err != nil {
		t.Fatalf("PublicKeys: %v", err)
	}

	// Parse and validate structure.
	var raw struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(jwksJSON, &raw); err != nil {
		t.Fatalf("unmarshal jwks: %v", err)
	}
	if len(raw.Keys) != 1 {
		t.Fatalf("expected 1 key in JWKS, got %d", len(raw.Keys))
	}

	// Parse as jwk.Set and check the kid.
	set, err := jwk.Parse(jwksJSON)
	if err != nil {
		t.Fatalf("jwk.Parse: %v", err)
	}
	if set.Len() != 1 {
		t.Fatalf("expected 1 key in set, got %d", set.Len())
	}

	key, ok := set.Key(0)
	if !ok {
		t.Fatal("expected to find key at index 0")
	}

	kid, ok := key.KeyID()
	if !ok || kid == "" {
		t.Error("expected non-empty kid")
	}
	if kid != signer.KID() {
		t.Errorf("JWKS kid = %q, want %q", kid, signer.KID())
	}
}

func TestGenerateRequestToken(t *testing.T) {
	t.Parallel()

	pt, hashed, err := citoken.GenerateRequestToken()
	if err != nil {
		t.Fatalf("GenerateRequestToken: %v", err)
	}
	if pt == "" {
		t.Error("expected non-empty plaintext")
	}
	if hashed == "" {
		t.Error("expected non-empty hashed token")
	}
	if pt == hashed {
		t.Error("plaintext and hashed should differ")
	}

	// Verify determinism: same plaintext → same hash (HashRequestToken is deterministic).
	rehashed := citoken.HashRequestToken(pt)
	if rehashed != hashed {
		t.Errorf("re-hashed = %q, want %q", rehashed, hashed)
	}

	// Two calls should produce different plaintexts (random).
	pt2, hashed2, err := citoken.GenerateRequestToken()
	if err != nil {
		t.Fatalf("second GenerateRequestToken: %v", err)
	}
	if pt == pt2 {
		t.Error("expected different plaintexts on second call")
	}
	if hashed == hashed2 {
		t.Error("expected different hashes for different plaintexts")
	}
}

func TestJWTExpiry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	signer, err := citoken.NewLocalSigner()
	if err != nil {
		t.Fatalf("NewLocalSigner: %v", err)
	}

	before := time.Now()

	tokenStr, err := citoken.IssueJWT(ctx, signer, citoken.JobClaims{
		Issuer:    "https://docstore.dev",
		Subject:   "repo:acme/myrepo:branch:main:check:test",
		Audience:  "https://example.com",
		Repo:      "acme/myrepo",
		Org:       "acme",
		Branch:    "main",
		CheckName: "test",
		JobID:     "job-expiry",
	})
	if err != nil {
		t.Fatalf("IssueJWT: %v", err)
	}

	jwksJSON, err := signer.PublicKeys(ctx)
	if err != nil {
		t.Fatalf("PublicKeys: %v", err)
	}
	keySet, err := jwk.Parse(jwksJSON)
	if err != nil {
		t.Fatalf("parse JWKS: %v", err)
	}

	tok, err := jwt.Parse([]byte(tokenStr), jwt.WithKeySet(keySet, jws.WithInferAlgorithmFromKey(true)))
	if err != nil {
		t.Fatalf("jwt.Parse: %v", err)
	}

	iat, ok := tok.IssuedAt()
	if !ok {
		t.Fatal("expected iat claim")
	}
	exp, ok := tok.Expiration()
	if !ok {
		t.Fatal("expected exp claim")
	}

	// iat should be very close to before.
	if iat.Before(before.Add(-2 * time.Second)) {
		t.Errorf("iat %v too old (before = %v)", iat, before)
	}

	// exp should be ~1h after iat.
	ttl := exp.Sub(iat)
	if ttl < 55*time.Minute || ttl > 65*time.Minute {
		t.Errorf("token TTL = %v, want ~1h", ttl)
	}
}
