package secrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dlorenc/docstore/internal/db"
)

// --- fakes ------------------------------------------------------------------

// fakeEncryptor wraps plaintext with a known prefix so Reveal results are
// predictable. It is NOT cryptographically meaningful — it exists to let the
// service-layer tests exercise the call-graph without pulling AES-GCM into
// scope. The KeyName matches a Sealed's KMSKeyName so a real Encryptor's
// "did I issue this?" check would pass.
type fakeEncryptor struct {
	mu          sync.Mutex
	keyName     string
	encryptN    int
	decryptN    int
	encryptErr  error
	decryptErr  error
	failDecrypt map[string]bool // by ciphertext-as-string
}

func newFakeEncryptor() *fakeEncryptor {
	return &fakeEncryptor{
		keyName:     "fake:test-key",
		failDecrypt: map[string]bool{},
	}
}

func (f *fakeEncryptor) KeyName() string { return f.keyName }

func (f *fakeEncryptor) Encrypt(_ context.Context, pt []byte) (Sealed, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.encryptN++
	if f.encryptErr != nil {
		return Sealed{}, f.encryptErr
	}
	// Ciphertext = "ct:" || pt; Nonce/EncryptedDEK have any non-empty value
	// since the store treats them as opaque.
	ct := append([]byte("ct:"), pt...)
	return Sealed{
		Ciphertext:   ct,
		Nonce:        []byte("nonce"),
		EncryptedDEK: []byte("wrapped-dek"),
		KMSKeyName:   f.keyName,
	}, nil
}

func (f *fakeEncryptor) Decrypt(_ context.Context, s Sealed) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decryptN++
	if f.decryptErr != nil {
		return nil, f.decryptErr
	}
	if f.failDecrypt[string(s.Ciphertext)] {
		return nil, errors.New("fake: forced decrypt failure")
	}
	if !bytes.HasPrefix(s.Ciphertext, []byte("ct:")) {
		return nil, errors.New("fake: ciphertext missing prefix")
	}
	return s.Ciphertext[len("ct:"):], nil
}

// fakeStore is an in-memory SecretStore. It mirrors the upsert / preserve-
// created-fields behaviour of the real store closely enough that the service
// layer can be exercised without Postgres.
type fakeStore struct {
	mu       sync.Mutex
	rows     map[string]map[string]db.RepoSecret // repo -> name -> row
	touchN   map[string]int                      // repo|name -> count
	touchErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		rows:   map[string]map[string]db.RepoSecret{},
		touchN: map[string]int{},
	}
}

func (s *fakeStore) SetRepoSecret(_ context.Context, in db.RepoSecret) (db.RepoSecret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rows[in.Repo]; !ok {
		s.rows[in.Repo] = map[string]db.RepoSecret{}
	}
	// Mirror the real store: on conflict by (repo, name) preserve
	// id/created_by/created_at, leave last_used_at alone, bump updated_*.
	if existing, ok := s.rows[in.Repo][in.Name]; ok {
		newActor := in.CreatedBy
		now := time.Now().UTC()
		out := existing
		out.Description = in.Description
		out.Ciphertext = in.Ciphertext
		out.Nonce = in.Nonce
		out.EncryptedDEK = in.EncryptedDEK
		out.KMSKeyName = in.KMSKeyName
		out.SizeBytes = in.SizeBytes
		out.UpdatedBy = &newActor
		out.UpdatedAt = &now
		s.rows[in.Repo][in.Name] = out
		return out, nil
	}
	in.CreatedAt = in.CreatedAt.UTC()
	s.rows[in.Repo][in.Name] = in
	return in, nil
}

func (s *fakeStore) GetRepoSecret(_ context.Context, repo, name string) (db.RepoSecret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.rows[repo]; ok {
		if r, ok := m[name]; ok {
			return r, nil
		}
	}
	return db.RepoSecret{}, db.ErrSecretNotFound
}

func (s *fakeStore) ListRepoSecrets(_ context.Context, repo string) ([]db.RepoSecret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []db.RepoSecret{}
	if m, ok := s.rows[repo]; ok {
		// Caller of List in tests does not rely on order of the fake — the
		// real store sorts by name.
		for _, r := range m {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *fakeStore) DeleteRepoSecret(_ context.Context, repo, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.rows[repo]
	if !ok {
		return db.ErrSecretNotFound
	}
	if _, ok := m[name]; !ok {
		return db.ErrSecretNotFound
	}
	delete(m, name)
	return nil
}

func (s *fakeStore) TouchRepoSecretLastUsed(_ context.Context, repo, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touchN[repo+"|"+name]++
	if s.touchErr != nil {
		return s.touchErr
	}
	if m, ok := s.rows[repo]; ok {
		if r, ok := m[name]; ok {
			now := time.Now().UTC()
			r.LastUsedAt = &now
			m[name] = r
		}
	}
	return nil
}

// --- helpers ----------------------------------------------------------------

func newServiceForTest() (*fakeEncryptor, *fakeStore, Service) {
	enc := newFakeEncryptor()
	store := newFakeStore()
	return enc, store, NewService(enc, store)
}

// --- tests ------------------------------------------------------------------

func TestService_Set_RejectsBadName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		input   string
		wantErr error
	}{
		{"lowercase", "dockerhub_token", ErrInvalidName},
		{"leading_digit", "1TOKEN", ErrInvalidName},
		{"with_hyphen", "MY-TOKEN", ErrInvalidName},
		{"too_long", strings.Repeat("A", 65), ErrInvalidName},
		{"reserved_prefix", "DOCSTORE_OIDC_REQUEST_TOKEN", ErrReservedName},
		{"empty", "", ErrInvalidName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			enc, store, svc := newServiceForTest()
			_, err := svc.Set(t.Context(), "acme/widgets", tc.input, "", []byte("v"), "alice@x")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Set: got %v, want %v", err, tc.wantErr)
			}
			if enc.encryptN != 0 {
				t.Errorf("Encryptor was called %d times on rejection; want 0", enc.encryptN)
			}
			if got := len(store.rows); got != 0 {
				t.Errorf("store has %d repos after rejection; want 0", got)
			}
		})
	}
}

func TestService_Set_RejectsEmptyValue(t *testing.T) {
	t.Parallel()
	enc, store, svc := newServiceForTest()
	_, err := svc.Set(t.Context(), "acme/widgets", "TOKEN", "", []byte{}, "alice@x")
	if !errors.Is(err, ErrEmptyValue) {
		t.Fatalf("Set empty: got %v, want ErrEmptyValue", err)
	}
	if enc.encryptN != 0 {
		t.Error("Encryptor must not be called when value is empty")
	}
	if len(store.rows) != 0 {
		t.Error("store should be untouched on empty-value rejection")
	}
}

func TestService_Set_RejectsOversize(t *testing.T) {
	t.Parallel()
	enc, store, svc := newServiceForTest()
	big := bytes.Repeat([]byte{'a'}, MaxValueBytes+1)
	_, err := svc.Set(t.Context(), "acme/widgets", "TOKEN", "", big, "alice@x")
	if !errors.Is(err, ErrValueTooLarge) {
		t.Fatalf("Set oversize: got %v, want ErrValueTooLarge", err)
	}
	if enc.encryptN != 0 {
		t.Error("Encryptor must not be called when value is too large")
	}
	if len(store.rows) != 0 {
		t.Error("store should be untouched on oversize rejection")
	}
}

func TestService_Set_PersistsAndReturnsMetadata(t *testing.T) {
	t.Parallel()
	enc, store, svc := newServiceForTest()
	value := []byte("hunter2")

	md, err := svc.Set(t.Context(), "acme/widgets", "DOCKERHUB_TOKEN",
		"creds for docker hub", value, "alice@x")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}

	if enc.encryptN != 1 {
		t.Errorf("Encrypt called %d times; want 1", enc.encryptN)
	}
	if md.ID == "" {
		t.Error("Metadata.ID is empty")
	}
	if md.Repo != "acme/widgets" {
		t.Errorf("Repo: got %q, want %q", md.Repo, "acme/widgets")
	}
	if md.Name != "DOCKERHUB_TOKEN" {
		t.Errorf("Name: got %q", md.Name)
	}
	if md.Description != "creds for docker hub" {
		t.Errorf("Description: got %q", md.Description)
	}
	if md.SizeBytes != len(value) {
		t.Errorf("SizeBytes: got %d, want %d", md.SizeBytes, len(value))
	}
	if md.CreatedBy != "alice@x" {
		t.Errorf("CreatedBy: got %q", md.CreatedBy)
	}
	if md.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
	if md.UpdatedBy != nil || md.UpdatedAt != nil {
		t.Error("Updated* should be nil on first write")
	}

	// Exactly one row in the store.
	if got := len(store.rows["acme/widgets"]); got != 1 {
		t.Errorf("store rows: got %d, want 1", got)
	}
	row := store.rows["acme/widgets"]["DOCKERHUB_TOKEN"]
	if !bytes.Equal(row.Ciphertext, append([]byte("ct:"), value...)) {
		t.Errorf("Ciphertext was not persisted as fakeEncryptor sealed it")
	}
}

func TestService_Set_UpdateBumpsUpdatedFields(t *testing.T) {
	t.Parallel()
	_, _, svc := newServiceForTest()

	first, err := svc.Set(t.Context(), "acme/widgets", "TOKEN", "v1", []byte("v1"), "alice@x")
	if err != nil {
		t.Fatalf("first Set: %v", err)
	}
	if first.UpdatedBy != nil || first.UpdatedAt != nil {
		t.Fatal("first Set should not set Updated*")
	}

	second, err := svc.Set(t.Context(), "acme/widgets", "TOKEN", "v2", []byte("v2-rotated"), "bob@x")
	if err != nil {
		t.Fatalf("second Set: %v", err)
	}
	if second.UpdatedBy == nil || *second.UpdatedBy != "bob@x" {
		t.Errorf("UpdatedBy: got %v, want %q", second.UpdatedBy, "bob@x")
	}
	if second.UpdatedAt == nil {
		t.Fatal("UpdatedAt: got nil after second Set")
	}
	if second.ID != first.ID {
		t.Errorf("ID changed across update: %q -> %q", first.ID, second.ID)
	}
	if second.Description != "v2" {
		t.Errorf("Description not updated: %q", second.Description)
	}
	if second.SizeBytes != len("v2-rotated") {
		t.Errorf("SizeBytes not updated: %d", second.SizeBytes)
	}
}

// TestService_List_ReturnsMetadataOnly compiles a function that asks for fields
// that *aren't* on Metadata; if Metadata grew a Ciphertext field, this test
// would be the first to break.
func TestService_List_ReturnsMetadataOnly(t *testing.T) {
	t.Parallel()
	_, _, svc := newServiceForTest()
	ctx := t.Context()

	for _, n := range []string{"AAA", "BBB", "CCC"} {
		if _, err := svc.Set(ctx, "acme/widgets", n, "", []byte("v-"+n), "alice@x"); err != nil {
			t.Fatalf("Set %s: %v", n, err)
		}
	}

	got, err := svc.List(ctx, "acme/widgets")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("List: got %d entries, want 3", len(got))
	}

	names := map[string]bool{}
	for _, md := range got {
		names[md.Name] = true
		if md.SizeBytes != len("v-"+md.Name) {
			t.Errorf("%s: SizeBytes = %d, want %d", md.Name, md.SizeBytes, len("v-"+md.Name))
		}
		// Compile-time guarantee: Metadata has no Ciphertext field. The
		// inability to write a line like `md.Ciphertext` is the actual
		// assertion; this comment is documentation for the next reader.
		_ = md
	}
	for _, n := range []string{"AAA", "BBB", "CCC"} {
		if !names[n] {
			t.Errorf("missing name %q in List", n)
		}
	}
}

func TestService_Delete_NotFound(t *testing.T) {
	t.Parallel()
	_, _, svc := newServiceForTest()
	err := svc.Delete(t.Context(), "acme/widgets", "NOPE")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete missing: got %v, want ErrNotFound", err)
	}
}

func TestService_Delete_HappyPath(t *testing.T) {
	t.Parallel()
	_, _, svc := newServiceForTest()
	ctx := t.Context()

	if _, err := svc.Set(ctx, "acme/widgets", "TOKEN", "", []byte("v"), "alice@x"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := svc.Delete(ctx, "acme/widgets", "TOKEN"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, err := svc.List(ctx, "acme/widgets")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List after delete: got %d, want 0", len(got))
	}
}

func TestService_Reveal_HappyPath(t *testing.T) {
	t.Parallel()
	_, store, svc := newServiceForTest()
	ctx := t.Context()

	plaintexts := map[string][]byte{
		"DOCKERHUB_TOKEN": []byte("docker-creds"),
		"SLACK_WEBHOOK":   []byte("https://slack/x"),
	}
	for name, pt := range plaintexts {
		if _, err := svc.Set(ctx, "acme/widgets", name, "", pt, "alice@x"); err != nil {
			t.Fatalf("Set %s: %v", name, err)
		}
	}

	values, missing, err := svc.Reveal(ctx, "acme/widgets",
		[]string{"DOCKERHUB_TOKEN", "SLACK_WEBHOOK"})
	if err != nil {
		t.Fatalf("Reveal: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("missing: got %v, want empty", missing)
	}
	for name, want := range plaintexts {
		if got, ok := values[name]; !ok {
			t.Errorf("missing value for %q", name)
		} else if !bytes.Equal(got, want) {
			t.Errorf("%s: got %q, want %q", name, got, want)
		}
	}

	// last_used_at touched once per revealed name.
	for name := range plaintexts {
		key := "acme/widgets|" + name
		if store.touchN[key] != 1 {
			t.Errorf("touchN[%s] = %d, want 1", key, store.touchN[key])
		}
		row := store.rows["acme/widgets"][name]
		if row.LastUsedAt == nil {
			t.Errorf("%s: LastUsedAt nil after Reveal", name)
		}
	}
}

func TestService_Reveal_PartialMissing(t *testing.T) {
	t.Parallel()
	_, _, svc := newServiceForTest()
	ctx := t.Context()

	if _, err := svc.Set(ctx, "acme/widgets", "AAA", "", []byte("a"), "alice@x"); err != nil {
		t.Fatalf("Set AAA: %v", err)
	}
	if _, err := svc.Set(ctx, "acme/widgets", "BBB", "", []byte("b"), "alice@x"); err != nil {
		t.Fatalf("Set BBB: %v", err)
	}

	values, missing, err := svc.Reveal(ctx, "acme/widgets",
		[]string{"AAA", "MISSING", "BBB"})
	if err != nil {
		t.Fatalf("Reveal: %v", err)
	}
	if len(values) != 2 {
		t.Errorf("values: got %d, want 2", len(values))
	}
	if !bytes.Equal(values["AAA"], []byte("a")) || !bytes.Equal(values["BBB"], []byte("b")) {
		t.Errorf("wrong plaintexts: %v", values)
	}
	if len(missing) != 1 || missing[0] != "MISSING" {
		t.Errorf("missing: got %v, want [MISSING]", missing)
	}
}

func TestService_Reveal_DecryptError(t *testing.T) {
	t.Parallel()
	enc, _, svc := newServiceForTest()
	ctx := t.Context()

	if _, err := svc.Set(ctx, "acme/widgets", "TOKEN", "", []byte("v"), "alice@x"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	wantErr := errors.New("kms is angry")
	enc.decryptErr = wantErr
	values, missing, err := svc.Reveal(ctx, "acme/widgets", []string{"TOKEN"})
	if err == nil {
		t.Fatal("Reveal: expected error, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("error chain: got %v, want chain containing %v", err, wantErr)
	}
	if values != nil {
		t.Errorf("values: got %v, want nil", values)
	}
	if missing != nil {
		t.Errorf("missing: got %v, want nil", missing)
	}
}

func TestService_Reveal_TouchFailureDoesNotPropagate(t *testing.T) {
	t.Parallel()
	_, store, svc := newServiceForTest()
	ctx := t.Context()

	if _, err := svc.Set(ctx, "acme/widgets", "TOKEN", "", []byte("v"), "alice@x"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	store.touchErr = fmt.Errorf("transient db blip")
	values, missing, err := svc.Reveal(ctx, "acme/widgets", []string{"TOKEN"})
	if err != nil {
		t.Fatalf("Reveal: got error %v, want nil (touch failures must be best-effort)", err)
	}
	if len(missing) != 0 {
		t.Errorf("missing: %v", missing)
	}
	if !bytes.Equal(values["TOKEN"], []byte("v")) {
		t.Errorf("TOKEN: got %q, want %q", values["TOKEN"], "v")
	}
	if store.touchN["acme/widgets|TOKEN"] != 1 {
		t.Errorf("touch was attempted %d times; want 1", store.touchN["acme/widgets|TOKEN"])
	}
}

// Compile-time assertion: *service satisfies Service.
var _ Service = (*service)(nil)
