package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// localKeyName is the canonical prefix returned by LocalEncryptor.KeyName.
// "local:" makes it visually impossible to confuse with a real KMS resource
// path in the DB or in logs.
const localKeyPrefix = "local:"

// LocalEncryptor is an in-process AES-256-GCM Encryptor. The KEK is persisted
// to a file on disk (mode 0600) so successive process invocations can decrypt
// data sealed by earlier ones. It mirrors the LocalSigner pattern in
// internal/citoken/citoken.go and is intended ONLY for local development —
// the server prints a banner when it starts with this implementation.
type LocalEncryptor struct {
	keyPath string
	kekGCM  cipher.AEAD
	keyName string
}

// NewLocalEncryptor returns a LocalEncryptor backed by the AES-256 key at
// keyPath. If the file does not exist a fresh 32-byte key is generated and
// written with mode 0600 (and the parent directory is created with 0700 if
// missing). If the file exists it must be exactly 32 bytes.
func NewLocalEncryptor(keyPath string) (*LocalEncryptor, error) {
	if keyPath == "" {
		return nil, errors.New("local encryptor: empty key path")
	}

	key, err := loadOrCreateKEK(keyPath)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("local encryptor: aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("local encryptor: gcm: %w", err)
	}

	return &LocalEncryptor{
		keyPath: keyPath,
		kekGCM:  gcm,
		// Use the file's basename so the stored kms_key_name is stable across
		// machines/users with different home directories. The full path would
		// leak operator-local detail into the DB.
		keyName: localKeyPrefix + filepath.Base(keyPath),
	}, nil
}

// loadOrCreateKEK reads the AES-256 key at path, or creates one if missing.
func loadOrCreateKEK(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		if len(data) != 32 {
			return nil, fmt.Errorf("local encryptor: key file %q: want 32 bytes, got %d", path, len(data))
		}
		return data, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("local encryptor: read key file: %w", err)
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if mkErr := os.MkdirAll(dir, 0o700); mkErr != nil {
			return nil, fmt.Errorf("local encryptor: create key dir: %w", mkErr)
		}
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("local encryptor: generate key: %w", err)
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, fmt.Errorf("local encryptor: write key file: %w", err)
	}
	return key, nil
}

// KeyName returns "local:<basename>" — see NewLocalEncryptor.
func (e *LocalEncryptor) KeyName() string { return e.keyName }

// Encrypt generates a fresh DEK and nonce, AES-GCM-seals the plaintext under
// the DEK, then wraps the DEK under the file-backed KEK. The 32 KiB plaintext
// cap is enforced upstream — not here, so this layer stays opinion-free about
// size.
func (e *LocalEncryptor) Encrypt(_ context.Context, plaintext []byte) (Sealed, error) {
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return Sealed{}, fmt.Errorf("local encrypt: generate dek: %w", err)
	}
	dekBlock, err := aes.NewCipher(dek)
	if err != nil {
		return Sealed{}, fmt.Errorf("local encrypt: dek cipher: %w", err)
	}
	dekGCM, err := cipher.NewGCM(dekBlock)
	if err != nil {
		return Sealed{}, fmt.Errorf("local encrypt: dek gcm: %w", err)
	}

	nonce := make([]byte, dekGCM.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return Sealed{}, fmt.Errorf("local encrypt: nonce: %w", err)
	}
	// nil dst, nil aad — the Sealed struct fields fully describe the ciphertext.
	ct := dekGCM.Seal(nil, nonce, plaintext, nil)

	wrapped, err := e.wrapDEK(dek)
	if err != nil {
		return Sealed{}, err
	}

	return Sealed{
		Ciphertext:   ct,
		Nonce:        nonce,
		EncryptedDEK: wrapped,
		KMSKeyName:   e.keyName,
	}, nil
}

// Decrypt unwraps the DEK and AES-GCM-Opens the ciphertext.
func (e *LocalEncryptor) Decrypt(_ context.Context, sealed Sealed) ([]byte, error) {
	if sealed.KMSKeyName != e.keyName {
		// Refusing names we do not own makes operator misconfiguration loud
		// instead of producing a confusing GCM-tag-mismatch error deeper down.
		return nil, fmt.Errorf("local decrypt: key name %q not owned by this encryptor (%q)", sealed.KMSKeyName, e.keyName)
	}

	dek, err := e.unwrapDEK(sealed.EncryptedDEK)
	if err != nil {
		return nil, err
	}
	defer zero(dek)

	dekBlock, err := aes.NewCipher(dek)
	if err != nil {
		return nil, fmt.Errorf("local decrypt: dek cipher: %w", err)
	}
	dekGCM, err := cipher.NewGCM(dekBlock)
	if err != nil {
		return nil, fmt.Errorf("local decrypt: dek gcm: %w", err)
	}
	if len(sealed.Nonce) != dekGCM.NonceSize() {
		return nil, fmt.Errorf("local decrypt: nonce size: want %d, got %d", dekGCM.NonceSize(), len(sealed.Nonce))
	}
	pt, err := dekGCM.Open(nil, sealed.Nonce, sealed.Ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("local decrypt: open: %w", err)
	}
	return pt, nil
}

// wrapDEK seals the DEK under the KEK with a fresh per-call nonce. The
// on-disk format is `nonce_kek (12B) || ciphertext_dek_with_tag` so the
// EncryptedDEK byte string carries everything needed for unwrap. We pick
// this self-contained format (rather than a separate "dek_nonce" column)
// to keep the schema in docs/secrets-design.md unchanged for the dev path.
func (e *LocalEncryptor) wrapDEK(dek []byte) ([]byte, error) {
	nonce := make([]byte, e.kekGCM.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("local encrypt: kek nonce: %w", err)
	}
	out := make([]byte, 0, len(nonce)+len(dek)+e.kekGCM.Overhead())
	out = append(out, nonce...)
	out = e.kekGCM.Seal(out, nonce, dek, nil)
	return out, nil
}

// unwrapDEK reverses wrapDEK.
func (e *LocalEncryptor) unwrapDEK(wrapped []byte) ([]byte, error) {
	ns := e.kekGCM.NonceSize()
	if len(wrapped) < ns+e.kekGCM.Overhead() {
		return nil, fmt.Errorf("local decrypt: encrypted dek too short: %d bytes", len(wrapped))
	}
	nonce, ct := wrapped[:ns], wrapped[ns:]
	dek, err := e.kekGCM.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("local decrypt: unwrap dek: %w", err)
	}
	return dek, nil
}

// zero overwrites the slice with zeros. Best-effort hygiene — Go's GC may
// have already copied the bytes, but we wipe what we can reach.
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
