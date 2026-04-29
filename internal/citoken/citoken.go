// Package citoken implements CI job OIDC identity tokens.
// It provides JWT issuance backed by either an in-process RSA key (LocalSigner,
// for dev/test) or Google Cloud KMS (KMSSigner, for production).
package citoken

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jwt"
)

// Signer issues signed JWTs and exposes its public key set.
type Signer interface {
	// Sign returns a signed JWT string for the given claims map.
	Sign(ctx context.Context, claims map[string]any) (string, error)
	// PublicKeys returns the JWKS set of public keys for verification (JSON-encoded).
	PublicKeys(ctx context.Context) ([]byte, error)
}

// JobClaims holds the fields for a CI job OIDC token.
type JobClaims struct {
	Issuer      string
	Subject     string // "repo:{repo}:branch:{branch}:check:{check_name}"
	Audience    string
	Repo        string
	Org         string
	Branch      string
	CheckName   string
	RefType     string // "post-submit", "proposal", etc.
	TriggeredBy string
	JobID       string
	Sequence    int64
}

// GenerateRequestToken returns a (plaintext, hashedForStorage) pair.
// plaintext is given to the worker; hashedForStorage is stored in the DB.
// The hashed value is SHA-256 of the plaintext, base64url-encoded (no padding).
func GenerateRequestToken() (plaintext string, hashed string, err error) {
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("generate request token: %w", err)
	}
	plaintext = base64.RawURLEncoding.EncodeToString(raw)
	hashed = HashRequestToken(plaintext)
	return plaintext, hashed, nil
}

// HashRequestToken returns the storage hash for a given plaintext request token.
// The hash is SHA-256 of the plaintext, base64url-encoded without padding.
func HashRequestToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// IssueJWT builds and signs a JWT for a CI job using the provided Signer.
// The token includes standard claims (iss, sub, aud, jti, iat, exp) plus
// all custom CI job claims as top-level fields.
func IssueJWT(ctx context.Context, signer Signer, claims JobClaims) (string, error) {
	now := time.Now()
	jti := uuid.New().String()

	c := map[string]any{
		"iss":          claims.Issuer,
		"sub":          claims.Subject,
		"aud":          claims.Audience,
		"jti":          jti,
		"iat":          now.Unix(),
		"exp":          now.Add(time.Hour).Unix(),
		"repo":         claims.Repo,
		"org":          claims.Org,
		"branch":       claims.Branch,
		"check_name":   claims.CheckName,
		"ref_type":     claims.RefType,
		"triggered_by": claims.TriggeredBy,
		"job_id":       claims.JobID,
		"sequence":     claims.Sequence,
	}

	return signer.Sign(ctx, c)
}

// LocalSigner uses an in-process RSA-2048 key pair for development and testing.
type LocalSigner struct {
	privateKey *rsa.PrivateKey
	jwkKey     jwk.Key // private JWK with kid set
	kid        string
}

// NewLocalSigner generates a fresh RSA-2048 key pair and returns a LocalSigner.
func NewLocalSigner() (*LocalSigner, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate rsa key: %w", err)
	}

	// Build the JWK from the private key.
	k, err := jwk.Import(priv)
	if err != nil {
		return nil, fmt.Errorf("import rsa key to jwk: %w", err)
	}

	// Assign a stable kid based on the public key thumbprint (SHA-256).
	if err := jwk.AssignKeyID(k); err != nil {
		return nil, fmt.Errorf("assign key id: %w", err)
	}

	kid, ok := k.KeyID()
	if !ok {
		return nil, fmt.Errorf("key id not set after AssignKeyID")
	}

	return &LocalSigner{
		privateKey: priv,
		jwkKey:     k,
		kid:        kid,
	}, nil
}

// KID returns the key ID of the LocalSigner's public key.
func (s *LocalSigner) KID() string { return s.kid }

// Sign builds and signs a JWT using the in-process RSA key.
// The claims map must include "iss", "sub", "aud", "jti", "iat", "exp" plus
// any custom claims; Sign passes them through verbatim.
// The kid from the JWK is automatically included in the JWS protected header.
func (s *LocalSigner) Sign(_ context.Context, claims map[string]any) (string, error) {
	tok := jwt.New()

	for k, v := range claims {
		if err := tok.Set(k, v); err != nil {
			return "", fmt.Errorf("set claim %q: %w", k, err)
		}
	}

	// Sign with the JWK key (not the raw private key) so the kid is included
	// in the JWS protected header.
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256(), s.jwkKey))
	if err != nil {
		return "", fmt.Errorf("sign jwt: %w", err)
	}
	return string(signed), nil
}

// PublicKeys returns the JWKS JSON containing the RSA public key.
func (s *LocalSigner) PublicKeys(_ context.Context) ([]byte, error) {
	pubKey, err := s.jwkKey.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("get public key: %w", err)
	}

	set := jwk.NewSet()
	if err := set.AddKey(pubKey); err != nil {
		return nil, fmt.Errorf("add key to set: %w", err)
	}

	data, err := json.Marshal(set)
	if err != nil {
		return nil, fmt.Errorf("marshal jwks: %w", err)
	}
	return data, nil
}

// KMSSigner uses Google Cloud KMS for production JWT signing.
// TODO: implement full KMS signing once the KMS client is wired in.
type KMSSigner struct {
	// keyName is the full KMS resource name, e.g.
	// "projects/my-project/locations/global/keyRings/my-ring/cryptoKeys/my-key/cryptoKeyVersions/1"
	keyName string
}

// NewKMSSigner creates a KMSSigner for the given KMS key resource name.
func NewKMSSigner(keyName string) *KMSSigner {
	return &KMSSigner{keyName: keyName}
}

// Sign is not yet implemented for KMSSigner.
func (s *KMSSigner) Sign(_ context.Context, _ map[string]any) (string, error) {
	return "", fmt.Errorf("KMSSigner.Sign: not yet implemented (key: %s)", s.keyName)
}

// PublicKeys is not yet implemented for KMSSigner.
func (s *KMSSigner) PublicKeys(_ context.Context) ([]byte, error) {
	return nil, fmt.Errorf("KMSSigner.PublicKeys: not yet implemented (key: %s)", s.keyName)
}
