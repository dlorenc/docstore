package secrets

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"path/filepath"
	"testing"

	kmspb "cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/googleapis/gax-go/v2"
)

// --- LocalEncryptor ---------------------------------------------------------

func newLocalEncryptorForTest(t *testing.T) *LocalEncryptor {
	t.Helper()
	keyPath := filepath.Join(t.TempDir(), "dev-encryption-key")
	enc, err := NewLocalEncryptor(keyPath)
	if err != nil {
		t.Fatalf("NewLocalEncryptor: %v", err)
	}
	return enc
}

func TestLocalEncryptor_Roundtrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	enc := newLocalEncryptorForTest(t)

	cases := map[string][]byte{
		"small": []byte("hunter2"),
		"empty": {},
		"32KiB": bytes.Repeat([]byte{0xab}, 32*1024),
	}

	for name, pt := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sealed, err := enc.Encrypt(ctx, pt)
			if err != nil {
				t.Fatalf("Encrypt: %v", err)
			}
			if sealed.KMSKeyName != enc.KeyName() {
				t.Errorf("KMSKeyName = %q, want %q", sealed.KMSKeyName, enc.KeyName())
			}
			got, err := enc.Decrypt(ctx, sealed)
			if err != nil {
				t.Fatalf("Decrypt: %v", err)
			}
			if !bytes.Equal(got, pt) {
				t.Errorf("plaintext mismatch: got %d bytes, want %d", len(got), len(pt))
			}
		})
	}
}

func TestLocalEncryptor_FreshDEKEachCall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	enc := newLocalEncryptorForTest(t)

	pt := []byte("same plaintext both times")
	a, err := enc.Encrypt(ctx, pt)
	if err != nil {
		t.Fatalf("Encrypt a: %v", err)
	}
	b, err := enc.Encrypt(ctx, pt)
	if err != nil {
		t.Fatalf("Encrypt b: %v", err)
	}

	if bytes.Equal(a.Ciphertext, b.Ciphertext) {
		t.Error("ciphertexts should differ across Encrypt calls (fresh nonce)")
	}
	if bytes.Equal(a.Nonce, b.Nonce) {
		t.Error("nonces should differ across Encrypt calls")
	}
	if bytes.Equal(a.EncryptedDEK, b.EncryptedDEK) {
		t.Error("encrypted DEKs should differ across Encrypt calls (fresh DEK)")
	}
}

func TestLocalEncryptor_TamperDetected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	enc := newLocalEncryptorForTest(t)

	pt := []byte("don't-mess-with-me")
	sealed, err := enc.Encrypt(ctx, pt)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Each subtest flips one byte in a different field.
	tests := []struct {
		name  string
		field func(*Sealed)
	}{
		{"ciphertext", func(s *Sealed) { s.Ciphertext[0] ^= 0x01 }},
		{"nonce", func(s *Sealed) { s.Nonce[0] ^= 0x01 }},
		{"encrypted_dek", func(s *Sealed) { s.EncryptedDEK[0] ^= 0x01 }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bad := Sealed{
				Ciphertext:   bytes.Clone(sealed.Ciphertext),
				Nonce:        bytes.Clone(sealed.Nonce),
				EncryptedDEK: bytes.Clone(sealed.EncryptedDEK),
				KMSKeyName:   sealed.KMSKeyName,
			}
			tc.field(&bad)
			if _, err := enc.Decrypt(ctx, bad); err == nil {
				t.Errorf("Decrypt of tampered %s should fail", tc.name)
			}
		})
	}
}

func TestLocalEncryptor_Persistence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	keyPath := filepath.Join(t.TempDir(), "dev-encryption-key")

	first, err := NewLocalEncryptor(keyPath)
	if err != nil {
		t.Fatalf("NewLocalEncryptor first: %v", err)
	}
	pt := []byte("persistent-plaintext")
	sealed, err := first.Encrypt(ctx, pt)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Fresh instance reading the same key file must Decrypt the prior Sealed.
	second, err := NewLocalEncryptor(keyPath)
	if err != nil {
		t.Fatalf("NewLocalEncryptor second: %v", err)
	}
	got, err := second.Decrypt(ctx, sealed)
	if err != nil {
		t.Fatalf("Decrypt across instances: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Errorf("decrypted plaintext = %q, want %q", got, pt)
	}
	if first.KeyName() != second.KeyName() {
		t.Errorf("KeyName mismatch: first=%q second=%q", first.KeyName(), second.KeyName())
	}
}

func TestLocalEncryptor_RejectsForeignKeyName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	enc := newLocalEncryptorForTest(t)

	sealed, err := enc.Encrypt(ctx, []byte("x"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	sealed.KMSKeyName = "local:somebody-elses-key"
	if _, err := enc.Decrypt(ctx, sealed); err == nil {
		t.Error("Decrypt should reject a Sealed with a foreign key name")
	}
}

func TestLocalEncryptor_KeyNameUsesBasename(t *testing.T) {
	t.Parallel()
	keyPath := filepath.Join(t.TempDir(), "my-key-file")
	enc, err := NewLocalEncryptor(keyPath)
	if err != nil {
		t.Fatalf("NewLocalEncryptor: %v", err)
	}
	const want = "local:my-key-file"
	if got := enc.KeyName(); got != want {
		t.Errorf("KeyName = %q, want %q", got, want)
	}
}

// --- KMSEncryptor (with fake client) ---------------------------------------

// fakeKMSClient is a deterministic, in-memory stand-in for the Cloud KMS
// client. It "wraps" a DEK by AES-GCM-sealing it under a process-local KEK
// and returns nonce||ct as the wrapped bytes — close enough to the real
// thing to exercise our request/response handling without touching GCP.
type fakeKMSClient struct {
	gcm        cipher.AEAD
	expectName string
	encryptErr error
	decryptErr error
	encryptN   int
	decryptN   int
}

func newFakeKMSClient(t *testing.T, expectName string) *fakeKMSClient {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	return &fakeKMSClient{gcm: gcm, expectName: expectName}
}

func (f *fakeKMSClient) Encrypt(_ context.Context, req *kmspb.EncryptRequest, _ ...gax.CallOption) (*kmspb.EncryptResponse, error) {
	f.encryptN++
	if f.encryptErr != nil {
		return nil, f.encryptErr
	}
	if req.GetName() != f.expectName {
		return nil, errors.New("fake kms: unexpected key name")
	}
	nonce := make([]byte, f.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append([]byte{}, nonce...)
	out = f.gcm.Seal(out, nonce, req.GetPlaintext(), nil)
	return &kmspb.EncryptResponse{Name: req.GetName(), Ciphertext: out}, nil
}

func (f *fakeKMSClient) Decrypt(_ context.Context, req *kmspb.DecryptRequest, _ ...gax.CallOption) (*kmspb.DecryptResponse, error) {
	f.decryptN++
	if f.decryptErr != nil {
		return nil, f.decryptErr
	}
	if req.GetName() != f.expectName {
		return nil, errors.New("fake kms: unexpected key name")
	}
	ns := f.gcm.NonceSize()
	wrapped := req.GetCiphertext()
	if len(wrapped) < ns+f.gcm.Overhead() {
		return nil, errors.New("fake kms: ciphertext too short")
	}
	pt, err := f.gcm.Open(nil, wrapped[:ns], wrapped[ns:], nil)
	if err != nil {
		return nil, err
	}
	return &kmspb.DecryptResponse{Plaintext: pt}, nil
}

const testKMSKey = "projects/p/locations/l/keyRings/r/cryptoKeys/k"

func TestKMSEncryptor_Roundtrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newFakeKMSClient(t, testKMSKey)
	enc := newKMSEncryptorWithClient(fake, testKMSKey)

	cases := map[string][]byte{
		"small": []byte("kms-roundtrip"),
		"empty": {},
		"32KiB": bytes.Repeat([]byte{0x55}, 32*1024),
	}
	for name, pt := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sealed, err := enc.Encrypt(ctx, pt)
			if err != nil {
				t.Fatalf("Encrypt: %v", err)
			}
			if sealed.KMSKeyName != testKMSKey {
				t.Errorf("KMSKeyName = %q, want %q", sealed.KMSKeyName, testKMSKey)
			}
			got, err := enc.Decrypt(ctx, sealed)
			if err != nil {
				t.Fatalf("Decrypt: %v", err)
			}
			if !bytes.Equal(got, pt) {
				t.Errorf("roundtrip mismatch: got %d bytes, want %d", len(got), len(pt))
			}
		})
	}
}

func TestKMSEncryptor_FreshDEK(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newFakeKMSClient(t, testKMSKey)
	enc := newKMSEncryptorWithClient(fake, testKMSKey)

	pt := []byte("same plaintext")
	a, err := enc.Encrypt(ctx, pt)
	if err != nil {
		t.Fatalf("Encrypt a: %v", err)
	}
	b, err := enc.Encrypt(ctx, pt)
	if err != nil {
		t.Fatalf("Encrypt b: %v", err)
	}
	if bytes.Equal(a.Ciphertext, b.Ciphertext) {
		t.Error("ciphertexts should differ")
	}
	if bytes.Equal(a.EncryptedDEK, b.EncryptedDEK) {
		t.Error("encrypted DEKs should differ")
	}
	if fake.encryptN != 2 {
		t.Errorf("expected 2 KMS Encrypt calls, got %d", fake.encryptN)
	}
}

func TestKMSEncryptor_EncryptErrorPropagates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newFakeKMSClient(t, testKMSKey)
	wantErr := errors.New("kms unavailable")
	fake.encryptErr = wantErr

	enc := newKMSEncryptorWithClient(fake, testKMSKey)
	if _, err := enc.Encrypt(ctx, []byte("x")); err == nil {
		t.Fatal("Encrypt: expected error, got nil")
	} else if !errors.Is(err, wantErr) {
		t.Errorf("Encrypt error = %v, want chain containing %v", err, wantErr)
	}
}

func TestKMSEncryptor_DecryptErrorPropagates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newFakeKMSClient(t, testKMSKey)
	enc := newKMSEncryptorWithClient(fake, testKMSKey)

	sealed, err := enc.Encrypt(ctx, []byte("x"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	wantErr := errors.New("kms decrypt boom")
	fake.decryptErr = wantErr
	if _, err := enc.Decrypt(ctx, sealed); err == nil {
		t.Fatal("Decrypt: expected error, got nil")
	} else if !errors.Is(err, wantErr) {
		t.Errorf("Decrypt error = %v, want chain containing %v", err, wantErr)
	}
}

func TestKMSEncryptor_RejectsForeignKeyName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newFakeKMSClient(t, testKMSKey)
	enc := newKMSEncryptorWithClient(fake, testKMSKey)

	sealed, err := enc.Encrypt(ctx, []byte("x"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	sealed.KMSKeyName = "projects/other/locations/l/keyRings/r/cryptoKeys/other"
	if _, err := enc.Decrypt(ctx, sealed); err == nil {
		t.Error("Decrypt should reject foreign KMSKeyName before calling KMS")
	}
	if fake.decryptN != 0 {
		t.Errorf("KMS Decrypt should not have been called; got %d calls", fake.decryptN)
	}
}

func TestKMSEncryptor_TamperDetected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newFakeKMSClient(t, testKMSKey)
	enc := newKMSEncryptorWithClient(fake, testKMSKey)

	sealed, err := enc.Encrypt(ctx, []byte("don't-mess"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	tests := []struct {
		name  string
		field func(*Sealed)
	}{
		{"ciphertext", func(s *Sealed) { s.Ciphertext[0] ^= 0x01 }},
		{"nonce", func(s *Sealed) { s.Nonce[0] ^= 0x01 }},
		{"encrypted_dek", func(s *Sealed) { s.EncryptedDEK[0] ^= 0x01 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bad := Sealed{
				Ciphertext:   bytes.Clone(sealed.Ciphertext),
				Nonce:        bytes.Clone(sealed.Nonce),
				EncryptedDEK: bytes.Clone(sealed.EncryptedDEK),
				KMSKeyName:   sealed.KMSKeyName,
			}
			tc.field(&bad)
			if _, err := enc.Decrypt(ctx, bad); err == nil {
				t.Errorf("Decrypt of tampered %s should fail", tc.name)
			}
		})
	}
}

// Compile-time assertions that both implementations satisfy the interface.
var (
	_ Encryptor = (*LocalEncryptor)(nil)
	_ Encryptor = (*KMSEncryptor)(nil)
)
