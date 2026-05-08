package db

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dlorenc/docstore/internal/testutil"
	"github.com/google/uuid"
)

// newRepoSecret returns a RepoSecret pre-populated with deterministic-looking
// sealed bytes so that tests can compare round-tripped values.
func newRepoSecret(repo, name string) RepoSecret {
	return RepoSecret{
		ID:           uuid.New().String(),
		Repo:         repo,
		Name:         name,
		Description:  "test secret " + name,
		Ciphertext:   []byte("sealed:" + name),
		Nonce:        bytes.Repeat([]byte{0xab}, 12),
		EncryptedDEK: []byte("kms-wrapped-dek:" + name),
		KMSKeyName:   "projects/p/locations/l/keyRings/r/cryptoKeys/repo-secrets",
		SizeBytes:    len("plaintext-" + name),
		CreatedBy:    "alice@example.com",
		CreatedAt:    time.Now().UTC().Truncate(time.Microsecond),
	}
}

func assertSealedFieldsEqual(t *testing.T, got, want RepoSecret) {
	t.Helper()
	if got.Repo != want.Repo {
		t.Errorf("repo: got %q want %q", got.Repo, want.Repo)
	}
	if got.Name != want.Name {
		t.Errorf("name: got %q want %q", got.Name, want.Name)
	}
	if got.Description != want.Description {
		t.Errorf("description: got %q want %q", got.Description, want.Description)
	}
	if !bytes.Equal(got.Ciphertext, want.Ciphertext) {
		t.Errorf("ciphertext: got %x want %x", got.Ciphertext, want.Ciphertext)
	}
	if !bytes.Equal(got.Nonce, want.Nonce) {
		t.Errorf("nonce: got %x want %x", got.Nonce, want.Nonce)
	}
	if !bytes.Equal(got.EncryptedDEK, want.EncryptedDEK) {
		t.Errorf("encrypted_dek: got %x want %x", got.EncryptedDEK, want.EncryptedDEK)
	}
	if got.KMSKeyName != want.KMSKeyName {
		t.Errorf("kms_key_name: got %q want %q", got.KMSKeyName, want.KMSKeyName)
	}
	if got.SizeBytes != want.SizeBytes {
		t.Errorf("size_bytes: got %d want %d", got.SizeBytes, want.SizeBytes)
	}
}

func TestRepoSecret_SetGetRoundtrip(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	in := newRepoSecret("acme/widgets", "DOCKERHUB_TOKEN")

	written, err := s.SetRepoSecret(ctx, in)
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if written.ID != in.ID {
		t.Errorf("id: got %q want %q", written.ID, in.ID)
	}
	if written.CreatedBy != in.CreatedBy {
		t.Errorf("created_by: got %q want %q", written.CreatedBy, in.CreatedBy)
	}
	if !written.CreatedAt.Equal(in.CreatedAt) {
		t.Errorf("created_at: got %v want %v", written.CreatedAt, in.CreatedAt)
	}
	if written.UpdatedBy != nil {
		t.Errorf("updated_by: got %v want nil", *written.UpdatedBy)
	}
	if written.UpdatedAt != nil {
		t.Errorf("updated_at: got %v want nil", *written.UpdatedAt)
	}
	if written.LastUsedAt != nil {
		t.Errorf("last_used_at: got %v want nil", *written.LastUsedAt)
	}
	assertSealedFieldsEqual(t, written, in)

	got, err := s.GetRepoSecret(ctx, in.Repo, in.Name)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != in.ID {
		t.Errorf("id roundtrip: got %q want %q", got.ID, in.ID)
	}
	if !got.CreatedAt.Equal(in.CreatedAt) {
		t.Errorf("created_at roundtrip: got %v want %v", got.CreatedAt, in.CreatedAt)
	}
	assertSealedFieldsEqual(t, got, in)
}

func TestRepoSecret_SetTwiceUpdates(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	first := newRepoSecret("acme/widgets", "DOCKERHUB_TOKEN")
	written1, err := s.SetRepoSecret(ctx, first)
	if err != nil {
		t.Fatalf("first set: %v", err)
	}

	// Second write with same (repo, name) but a different caller-supplied id,
	// CreatedBy, sealed payload, and description. The store must preserve the
	// original id/created_by/created_at and bump updated_by/updated_at.
	second := newRepoSecret("acme/widgets", "DOCKERHUB_TOKEN")
	second.CreatedBy = "bob@example.com"
	second.Description = "rotated"
	second.Ciphertext = []byte("sealed:rotated")
	second.EncryptedDEK = []byte("kms-wrapped-dek:rotated")
	second.SizeBytes = 99

	beforeUpdate := time.Now().UTC().Add(-time.Second)
	written2, err := s.SetRepoSecret(ctx, second)
	if err != nil {
		t.Fatalf("second set: %v", err)
	}

	if written2.ID != written1.ID {
		t.Errorf("id changed on update: got %q want %q", written2.ID, written1.ID)
	}
	if written2.CreatedBy != written1.CreatedBy {
		t.Errorf("created_by changed on update: got %q want %q", written2.CreatedBy, written1.CreatedBy)
	}
	if !written2.CreatedAt.Equal(written1.CreatedAt) {
		t.Errorf("created_at changed on update: got %v want %v", written2.CreatedAt, written1.CreatedAt)
	}
	if written2.UpdatedBy == nil || *written2.UpdatedBy != second.CreatedBy {
		t.Errorf("updated_by: got %v want %q", written2.UpdatedBy, second.CreatedBy)
	}
	if written2.UpdatedAt == nil {
		t.Fatal("updated_at: got nil, want non-nil")
	}
	if written2.UpdatedAt.Before(beforeUpdate) {
		t.Errorf("updated_at: got %v, expected >= %v", *written2.UpdatedAt, beforeUpdate)
	}
	if written2.Description != "rotated" {
		t.Errorf("description not updated: %q", written2.Description)
	}
	if !bytes.Equal(written2.Ciphertext, []byte("sealed:rotated")) {
		t.Errorf("ciphertext not updated: %x", written2.Ciphertext)
	}
	if !bytes.Equal(written2.EncryptedDEK, []byte("kms-wrapped-dek:rotated")) {
		t.Errorf("encrypted_dek not updated: %x", written2.EncryptedDEK)
	}
	if written2.SizeBytes != 99 {
		t.Errorf("size_bytes not updated: %d", written2.SizeBytes)
	}

	// Confirm only one row exists for (repo, name).
	var count int
	if err := d.QueryRow(`SELECT COUNT(*) FROM repo_secrets WHERE repo = $1 AND name = $2`,
		first.Repo, first.Name).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 row, got %d", count)
	}
}

func TestRepoSecret_ListByRepo(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	want := []string{"AAA_TOKEN", "BBB_TOKEN", "ZZZ_TOKEN"}
	// Insert in non-sorted order to confirm List orders by name.
	for _, name := range []string{want[2], want[0], want[1]} {
		if _, err := s.SetRepoSecret(ctx, newRepoSecret("acme/widgets", name)); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}
	// Insert a row in another repo that must not appear.
	if _, err := s.SetRepoSecret(ctx, newRepoSecret("other/repo", "OTHER_TOKEN")); err != nil {
		t.Fatalf("set other repo: %v", err)
	}

	got, err := s.ListRepoSecrets(ctx, "acme/widgets")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d secrets, want %d", len(got), len(want))
	}
	for i, name := range want {
		if got[i].Name != name {
			t.Errorf("position %d: got %q, want %q", i, got[i].Name, name)
		}
		if got[i].Repo != "acme/widgets" {
			t.Errorf("position %d: cross-repo leak, repo=%q", i, got[i].Repo)
		}
		if len(got[i].Ciphertext) == 0 {
			t.Errorf("position %d: ciphertext was projected away", i)
		}
	}

	// Listing an empty repo returns an empty (non-nil) slice.
	empty, err := s.ListRepoSecrets(ctx, "no/such")
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if empty == nil {
		t.Error("list of empty repo: got nil, want []")
	}
	if len(empty) != 0 {
		t.Errorf("list of empty repo: got %d rows", len(empty))
	}
}

func TestRepoSecret_Delete(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	in := newRepoSecret("acme/widgets", "DOCKERHUB_TOKEN")
	if _, err := s.SetRepoSecret(ctx, in); err != nil {
		t.Fatalf("set: %v", err)
	}

	deleted, err := s.DeleteRepoSecret(ctx, in.Repo, in.Name)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted.ID != in.ID {
		t.Errorf("DeleteRepoSecret returned id %q, want %q", deleted.ID, in.ID)
	}
	if deleted.Repo != in.Repo || deleted.Name != in.Name {
		t.Errorf("DeleteRepoSecret returned (%q, %q), want (%q, %q)",
			deleted.Repo, deleted.Name, in.Repo, in.Name)
	}

	if _, err := s.GetRepoSecret(ctx, in.Repo, in.Name); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("get after delete: got %v, want ErrSecretNotFound", err)
	}

	if _, err := s.DeleteRepoSecret(ctx, in.Repo, in.Name); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("delete missing: got %v, want ErrSecretNotFound", err)
	}

	if _, err := s.DeleteRepoSecret(ctx, "no/such", "NOPE"); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("delete unknown repo: got %v, want ErrSecretNotFound", err)
	}
}

func TestRepoSecret_TouchLastUsed(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	in := newRepoSecret("acme/widgets", "DOCKERHUB_TOKEN")
	if _, err := s.SetRepoSecret(ctx, in); err != nil {
		t.Fatalf("set: %v", err)
	}

	beforeTouch := time.Now().UTC().Add(-time.Second)
	if err := s.TouchRepoSecretLastUsed(ctx, in.Repo, in.Name); err != nil {
		t.Fatalf("touch: %v", err)
	}

	got, err := s.GetRepoSecret(ctx, in.Repo, in.Name)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.LastUsedAt == nil {
		t.Fatal("last_used_at: got nil after touch")
	}
	if got.LastUsedAt.Before(beforeTouch) {
		t.Errorf("last_used_at: got %v, expected >= %v", *got.LastUsedAt, beforeTouch)
	}

	// Touching again advances the timestamp.
	first := *got.LastUsedAt
	time.Sleep(10 * time.Millisecond)
	if err := s.TouchRepoSecretLastUsed(ctx, in.Repo, in.Name); err != nil {
		t.Fatalf("touch second: %v", err)
	}
	got2, err := s.GetRepoSecret(ctx, in.Repo, in.Name)
	if err != nil {
		t.Fatalf("get second: %v", err)
	}
	if got2.LastUsedAt == nil || !got2.LastUsedAt.After(first) {
		t.Errorf("last_used_at not advanced: first=%v second=%v", first, got2.LastUsedAt)
	}

	// Touching a missing row is a no-op, not an error.
	if err := s.TouchRepoSecretLastUsed(ctx, "no/such", "NOPE"); err != nil {
		t.Errorf("touch missing: got %v, want nil", err)
	}
}
