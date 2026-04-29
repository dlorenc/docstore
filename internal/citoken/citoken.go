// Package citoken implements CI job OIDC identity tokens.
// It provides JWT issuance backed by either an in-process RSA key (LocalSigner,
// for dev/test) or Google Cloud KMS (KMSSigner, for production).
package citoken

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"sync"
	"time"

	kms "cloud.google.com/go/kms/apiv1"
	kmspb "cloud.google.com/go/kms/apiv1/kmspb"
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
	if err := pubKey.Set(jwk.AlgorithmKey, jwa.RS256()); err != nil {
		return nil, fmt.Errorf("set alg on public key: %w", err)
	}
	if err := pubKey.Set(jwk.KeyUsageKey, "sig"); err != nil {
		return nil, fmt.Errorf("set use on public key: %w", err)
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
// The key must be an asymmetric RSA-2048 or RSA-4096 signing key with
// purpose ASYMMETRIC_SIGN and algorithm RSA_SIGN_PKCS1_*_SHA256.
type KMSSigner struct {
	keyVersionName string
	client         *kms.KeyManagementClient
	// cached public key (fetched once; KMS keys rotate infrequently)
	once   sync.Once
	pubKey *rsa.PublicKey
	kid    string
	pubErr error
}

// NewKMSSigner creates a KMSSigner backed by the given KMS key version resource name.
// The format is:
//
//	projects/{project}/locations/{location}/keyRings/{keyRing}/cryptoKeys/{key}/cryptoKeyVersions/{version}
func NewKMSSigner(ctx context.Context, keyVersionName string) (*KMSSigner, error) {
	c, err := kms.NewKeyManagementClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("create kms client: %w", err)
	}
	return &KMSSigner{keyVersionName: keyVersionName, client: c}, nil
}

// fetchPublicKey retrieves and caches the RSA public key from KMS (called once via sync.Once).
func (s *KMSSigner) fetchPublicKey(ctx context.Context) {
	resp, err := s.client.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: s.keyVersionName})
	if err != nil {
		s.pubErr = fmt.Errorf("kms get public key: %w", err)
		return
	}
	block, _ := pem.Decode([]byte(resp.Pem))
	if block == nil {
		s.pubErr = fmt.Errorf("kms public key: failed to PEM-decode response")
		return
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		s.pubErr = fmt.Errorf("kms public key: parse PKIX: %w", err)
		return
	}
	rsaKey, ok := pub.(*rsa.PublicKey)
	if !ok {
		s.pubErr = fmt.Errorf("kms public key: expected *rsa.PublicKey, got %T", pub)
		return
	}
	jwkKey, err := jwk.Import(rsaKey)
	if err != nil {
		s.pubErr = fmt.Errorf("kms public key: import to jwk: %w", err)
		return
	}
	if err := jwk.AssignKeyID(jwkKey); err != nil {
		s.pubErr = fmt.Errorf("kms public key: assign kid: %w", err)
		return
	}
	kid, ok := jwkKey.KeyID()
	if !ok {
		s.pubErr = fmt.Errorf("kms public key: kid not set after AssignKeyID")
		return
	}
	s.pubKey = rsaKey
	s.kid = kid
}

// PublicKeys returns the JWKS JSON containing the KMS RSA public key.
func (s *KMSSigner) PublicKeys(ctx context.Context) ([]byte, error) {
	s.once.Do(func() { s.fetchPublicKey(ctx) })
	if s.pubErr != nil {
		return nil, s.pubErr
	}

	jwkKey, err := jwk.Import(s.pubKey)
	if err != nil {
		return nil, fmt.Errorf("kms jwks: import public key: %w", err)
	}
	if err := jwk.AssignKeyID(jwkKey); err != nil {
		return nil, fmt.Errorf("kms jwks: assign kid: %w", err)
	}
	if err := jwkKey.Set(jwk.AlgorithmKey, jwa.RS256()); err != nil {
		return nil, fmt.Errorf("kms jwks: set alg: %w", err)
	}
	if err := jwkKey.Set(jwk.KeyUsageKey, "sig"); err != nil {
		return nil, fmt.Errorf("kms jwks: set use: %w", err)
	}

	set := jwk.NewSet()
	if err := set.AddKey(jwkKey); err != nil {
		return nil, fmt.Errorf("kms jwks: add key: %w", err)
	}
	data, err := json.Marshal(set)
	if err != nil {
		return nil, fmt.Errorf("kms jwks: marshal: %w", err)
	}
	return data, nil
}

// Sign builds a JWT with the given claims and signs it via KMS AsymmetricSign.
// The JWT is assembled manually so that only the digest is sent to KMS
// (the private key never leaves KMS).
func (s *KMSSigner) Sign(ctx context.Context, claims map[string]any) (string, error) {
	s.once.Do(func() { s.fetchPublicKey(ctx) })
	if s.pubErr != nil {
		return "", s.pubErr
	}

	headerJSON, err := json.Marshal(map[string]string{
		"alg": "RS256",
		"kid": s.kid,
		"typ": "JWT",
	})
	if err != nil {
		return "", fmt.Errorf("kms sign: marshal header: %w", err)
	}
	headerB64 := base64.RawURLEncoding.EncodeToString(headerJSON)

	payloadJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("kms sign: marshal payload: %w", err)
	}
	payloadB64 := base64.RawURLEncoding.EncodeToString(payloadJSON)

	signingInput := headerB64 + "." + payloadB64
	digest := sha256.Sum256([]byte(signingInput))

	signResp, err := s.client.AsymmetricSign(ctx, &kmspb.AsymmetricSignRequest{
		Name: s.keyVersionName,
		Digest: &kmspb.Digest{
			Digest: &kmspb.Digest_Sha256{Sha256: digest[:]},
		},
	})
	if err != nil {
		return "", fmt.Errorf("kms sign: asymmetric sign: %w", err)
	}

	sigB64 := base64.RawURLEncoding.EncodeToString(signResp.Signature)
	return signingInput + "." + sigB64, nil
}
