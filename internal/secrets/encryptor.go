// Package secrets implements envelope encryption for repo-scoped secrets.
// It provides the Encryptor abstraction with two implementations: a KMS-backed
// encryptor for production and an in-process AES key for local development.
//
// Per docs/secrets-design.md, the storage shape is (ciphertext, nonce,
// encrypted_dek, kms_key_name): plaintext is sealed under a per-secret DEK,
// and the DEK is wrapped under a long-lived KEK (Cloud KMS in production,
// a file-backed AES key in dev). Plaintext bytes never leave this package
// alive once Encrypt returns.
package secrets

import "context"

// Sealed is the result of Encrypt — what callers persist verbatim. EncryptedDEK
// is the wrapped DEK; KMSKeyName names the KEK used so callers know which
// key/version to ask for on Decrypt and so we can rotate cleanly.
type Sealed struct {
	Ciphertext   []byte
	Nonce        []byte
	EncryptedDEK []byte
	KMSKeyName   string
}

// Encryptor wraps envelope encryption. Implementations are KMS-backed for
// production and a local in-process key for dev.
type Encryptor interface {
	// Encrypt seals plaintext. Generates a fresh DEK + nonce internally;
	// callers do NOT pass them.
	Encrypt(ctx context.Context, plaintext []byte) (Sealed, error)

	// Decrypt reverses Encrypt. Implementations must accept any KMSKeyName
	// they own (so KEK rotation is transparent). They must reject names
	// they do not own.
	Decrypt(ctx context.Context, sealed Sealed) ([]byte, error)

	// KeyName returns the canonical KMS key name new ciphertext is sealed
	// under. Diagnostic; not part of the security boundary.
	KeyName() string
}
