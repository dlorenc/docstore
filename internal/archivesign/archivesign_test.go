package archivesign

import (
	"testing"
)

func TestSign_Deterministic(t *testing.T) {
	secret := []byte("test-secret")
	repo := "org/repo"
	branch := "main"
	var sequence int64 = 42
	var expiresUnix int64 = 1700000000

	sig1 := Sign(secret, repo, branch, sequence, expiresUnix)
	sig2 := Sign(secret, repo, branch, sequence, expiresUnix)
	if sig1 != sig2 {
		t.Errorf("Sign not deterministic: %q != %q", sig1, sig2)
	}
	if sig1 == "" {
		t.Error("Sign returned empty string")
	}
}

func TestVerify_Valid(t *testing.T) {
	secret := []byte("test-secret")
	repo := "org/repo"
	branch := "feature"
	var sequence int64 = 7
	var expiresUnix int64 = 1800000000

	sig := Sign(secret, repo, branch, sequence, expiresUnix)
	if !Verify(secret, repo, branch, sequence, expiresUnix, sig) {
		t.Error("Verify returned false for a valid signature")
	}
}

func TestVerify_WrongSecret(t *testing.T) {
	secret := []byte("correct-secret")
	wrongSecret := []byte("wrong-secret")
	repo := "org/repo"
	branch := "main"
	var sequence int64 = 1
	var expiresUnix int64 = 1700000000

	sig := Sign(secret, repo, branch, sequence, expiresUnix)
	if Verify(wrongSecret, repo, branch, sequence, expiresUnix, sig) {
		t.Error("Verify returned true for wrong secret")
	}
}

func TestVerify_Tampered(t *testing.T) {
	secret := []byte("test-secret")
	repo := "org/repo"
	branch := "main"
	var sequence int64 = 42
	var expiresUnix int64 = 1700000000

	sig := Sign(secret, repo, branch, sequence, expiresUnix)
	// Verify with altered repo.
	if Verify(secret, "other/repo", branch, sequence, expiresUnix, sig) {
		t.Error("Verify returned true for tampered repo")
	}
}

func TestVerify_InvalidHex(t *testing.T) {
	secret := []byte("test-secret")
	if Verify(secret, "r", "b", 1, 1, "not-valid-hex!!!") {
		t.Error("Verify returned true for invalid hex signature")
	}
}
