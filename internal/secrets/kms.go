package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"

	kms "cloud.google.com/go/kms/apiv1"
	kmspb "cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/googleapis/gax-go/v2"
)

// kmsClient is the minimal subset of *kms.KeyManagementClient that
// KMSEncryptor uses. Tests inject a fake; production wires in the real client
// via NewKMSEncryptor.
type kmsClient interface {
	Encrypt(ctx context.Context, req *kmspb.EncryptRequest, opts ...gax.CallOption) (*kmspb.EncryptResponse, error)
	Decrypt(ctx context.Context, req *kmspb.DecryptRequest, opts ...gax.CallOption) (*kmspb.DecryptResponse, error)
}

// KMSEncryptor seals secrets under a Cloud KMS symmetric ENCRYPT_DECRYPT key.
// The KMS key wraps a per-secret DEK; plaintext bytes are encrypted locally
// with AES-256-GCM under the DEK so KMS only sees the (small, fixed-size) DEK.
type KMSEncryptor struct {
	client      kmsClient
	keyResource string
}

// NewKMSEncryptor constructs a KMSEncryptor by opening a default KMS client.
// keyResource is the canonical KMS key path
// (projects/.../locations/.../keyRings/.../cryptoKeys/...). Cloud KMS routes
// Decrypt requests to the appropriate key version automatically, which is
// what makes KEK rotation transparent to callers.
func NewKMSEncryptor(ctx context.Context, keyResource string) (*KMSEncryptor, error) {
	if keyResource == "" {
		return nil, errors.New("kms encryptor: empty key resource")
	}
	c, err := kms.NewKeyManagementClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("kms encryptor: new client: %w", err)
	}
	return &KMSEncryptor{client: c, keyResource: keyResource}, nil
}

// newKMSEncryptorWithClient is the test seam — exported only within the
// package so we can inject a fake kmsClient without leaking the interface
// to the rest of the codebase.
func newKMSEncryptorWithClient(client kmsClient, keyResource string) *KMSEncryptor {
	return &KMSEncryptor{client: client, keyResource: keyResource}
}

// KeyName returns the KMS key resource path. Diagnostic only.
func (e *KMSEncryptor) KeyName() string { return e.keyResource }

// Encrypt generates a fresh DEK, AES-GCM-seals the plaintext under the DEK
// locally, then asks KMS to wrap the DEK. One KMS API call per write.
//
// Any KMS error is treated as terminal — we do not retry inside the
// encryptor; the caller decides whether a transient failure deserves a retry.
func (e *KMSEncryptor) Encrypt(ctx context.Context, plaintext []byte) (Sealed, error) {
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return Sealed{}, fmt.Errorf("kms encrypt: generate dek: %w", err)
	}
	defer zero(dek)

	dekBlock, err := aes.NewCipher(dek)
	if err != nil {
		return Sealed{}, fmt.Errorf("kms encrypt: dek cipher: %w", err)
	}
	dekGCM, err := cipher.NewGCM(dekBlock)
	if err != nil {
		return Sealed{}, fmt.Errorf("kms encrypt: dek gcm: %w", err)
	}
	nonce := make([]byte, dekGCM.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return Sealed{}, fmt.Errorf("kms encrypt: nonce: %w", err)
	}
	ct := dekGCM.Seal(nil, nonce, plaintext, nil)

	resp, err := e.client.Encrypt(ctx, &kmspb.EncryptRequest{
		Name:      e.keyResource,
		Plaintext: dek,
	})
	if err != nil {
		return Sealed{}, fmt.Errorf("kms encrypt: wrap dek: %w", err)
	}

	return Sealed{
		Ciphertext:   ct,
		Nonce:        nonce,
		EncryptedDEK: resp.GetCiphertext(),
		KMSKeyName:   e.keyResource,
	}, nil
}

// Decrypt asks KMS to unwrap the DEK, then AES-GCM-Opens the ciphertext.
// We always send the configured keyResource as Name. Cloud KMS' Decrypt
// accepts the key (not key version) and figures out the version itself,
// which is how KEK rotation stays transparent.
//
// We refuse Sealed values whose KMSKeyName does not match this encryptor's
// keyResource so a misconfigured deployment fails loud rather than feeding
// the wrong ciphertext to KMS and getting a generic decryption error.
func (e *KMSEncryptor) Decrypt(ctx context.Context, sealed Sealed) ([]byte, error) {
	if sealed.KMSKeyName != e.keyResource {
		return nil, fmt.Errorf("kms decrypt: key name %q not owned by this encryptor (%q)", sealed.KMSKeyName, e.keyResource)
	}

	resp, err := e.client.Decrypt(ctx, &kmspb.DecryptRequest{
		Name:       e.keyResource,
		Ciphertext: sealed.EncryptedDEK,
	})
	if err != nil {
		return nil, fmt.Errorf("kms decrypt: unwrap dek: %w", err)
	}
	dek := resp.GetPlaintext()
	defer zero(dek)

	dekBlock, err := aes.NewCipher(dek)
	if err != nil {
		return nil, fmt.Errorf("kms decrypt: dek cipher: %w", err)
	}
	dekGCM, err := cipher.NewGCM(dekBlock)
	if err != nil {
		return nil, fmt.Errorf("kms decrypt: dek gcm: %w", err)
	}
	if len(sealed.Nonce) != dekGCM.NonceSize() {
		return nil, fmt.Errorf("kms decrypt: nonce size: want %d, got %d", dekGCM.NonceSize(), len(sealed.Nonce))
	}
	pt, err := dekGCM.Open(nil, sealed.Nonce, sealed.Ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("kms decrypt: open: %w", err)
	}
	return pt, nil
}
