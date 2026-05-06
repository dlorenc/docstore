package secrets

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/dlorenc/docstore/internal/db"
	"github.com/google/uuid"
)

// MaxValueBytes is the cap on plaintext values, enforced server-side. Values
// larger than this belong in object storage (see docs/secrets-design.md).
const MaxValueBytes = 32 * 1024

// reservedNamePrefix is the namespace docstore reserves for built-in secrets
// like docstore_oidc_request_token. Users cannot Set names with this prefix.
const reservedNamePrefix = "DOCSTORE_"

// nameRegexp matches the POSIX env-var shape we accept for secret names: an
// uppercase letter followed by up to 63 uppercase letters, digits, or
// underscores. Mirrors docs/secrets-design.md.
var nameRegexp = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// Sentinel errors so handlers can map to HTTP status codes without sniffing
// strings. Reveal returns missing names alongside found ones rather than
// erroring; the other operations error per-call.
var (
	ErrInvalidName   = errors.New("secret: invalid name")
	ErrReservedName  = errors.New("secret: reserved name prefix")
	ErrValueTooLarge = errors.New("secret: value exceeds maximum size")
	ErrEmptyValue    = errors.New("secret: empty value")
	ErrNotFound      = errors.New("secret: not found")
)

// Metadata is the server-facing view of a secret. Plaintext and sealed bytes
// are deliberately absent — the type cannot carry them, which makes it safe
// to log or marshal directly to a response body.
type Metadata struct {
	ID          string
	Repo        string
	Name        string
	Description string
	SizeBytes   int
	CreatedBy   string
	CreatedAt   time.Time
	UpdatedBy   *string
	UpdatedAt   *time.Time
	LastUsedAt  *time.Time
}

// Service is the public surface for repo secrets. Implementations encrypt on
// Set and decrypt on Reveal; List returns metadata only.
type Service interface {
	Set(ctx context.Context, repo, name, description string, value []byte, actor string) (Metadata, error)
	List(ctx context.Context, repo string) ([]Metadata, error)
	// Delete removes the named secret and returns the metadata of the row as
	// it existed at delete time. Returns ErrNotFound if the row did not exist.
	// The metadata is returned (rather than discarded) so callers can emit
	// audit events that include the row's id without an extra Get round-trip.
	Delete(ctx context.Context, repo, name string) (Metadata, error)
	// Reveal decrypts the named secrets for a repo and updates last_used_at.
	// Used by the scheduler at CI dispatch time. Missing names are returned in
	// missing rather than as an error so the caller can decide what to do.
	Reveal(ctx context.Context, repo string, names []string) (values map[string][]byte, missing []string, err error)
}

// SecretStore is the subset of *db.Store the service depends on. Defining it
// here (rather than importing a concrete *db.Store) lets the unit tests
// substitute an in-memory fake without spinning up Postgres.
type SecretStore interface {
	SetRepoSecret(ctx context.Context, in db.RepoSecret) (db.RepoSecret, error)
	GetRepoSecret(ctx context.Context, repo, name string) (db.RepoSecret, error)
	ListRepoSecrets(ctx context.Context, repo string) ([]db.RepoSecret, error)
	DeleteRepoSecret(ctx context.Context, repo, name string) (db.RepoSecret, error)
	TouchRepoSecretLastUsed(ctx context.Context, repo, name string) error
}

// service is the concrete Service. The now func is plumbed for tests; in
// production it is time.Now.
type service struct {
	enc   Encryptor
	store SecretStore
	now   func() time.Time
}

// NewService wires an Encryptor and a SecretStore into the Service interface.
func NewService(enc Encryptor, store SecretStore) Service {
	return &service{enc: enc, store: store, now: time.Now}
}

// Set validates inputs, seals the value, and upserts the row. The store does
// the upsert by (repo, name) and bumps updated_by/updated_at on conflict, so
// from the service's perspective Set is a single call regardless of whether
// this is a create or an update.
func (s *service) Set(ctx context.Context, repo, name, description string, value []byte, actor string) (Metadata, error) {
	if err := validateName(name); err != nil {
		return Metadata{}, err
	}
	if len(value) == 0 {
		return Metadata{}, ErrEmptyValue
	}
	if len(value) > MaxValueBytes {
		return Metadata{}, ErrValueTooLarge
	}

	sealed, err := s.enc.Encrypt(ctx, value)
	if err != nil {
		return Metadata{}, fmt.Errorf("encrypt secret: %w", err)
	}

	row := db.RepoSecret{
		ID:           uuid.New().String(),
		Repo:         repo,
		Name:         name,
		Description:  description,
		Ciphertext:   sealed.Ciphertext,
		Nonce:        sealed.Nonce,
		EncryptedDEK: sealed.EncryptedDEK,
		KMSKeyName:   sealed.KMSKeyName,
		SizeBytes:    len(value),
		CreatedBy:    actor,
		CreatedAt:    s.now().UTC(),
	}

	written, err := s.store.SetRepoSecret(ctx, row)
	if err != nil {
		return Metadata{}, fmt.Errorf("store secret: %w", err)
	}
	return toMetadata(written), nil
}

// List returns metadata for every secret in the repo. The store sorts by name.
func (s *service) List(ctx context.Context, repo string) ([]Metadata, error) {
	rows, err := s.store.ListRepoSecrets(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}
	out := make([]Metadata, len(rows))
	for i, r := range rows {
		out[i] = toMetadata(r)
	}
	return out, nil
}

// Delete maps the store's ErrSecretNotFound onto the service-level ErrNotFound
// so handlers can switch on a single sentinel without depending on the db
// package. The deleted row's metadata is returned so callers can emit audit
// events (the id is the only stable handle on a deleted row).
func (s *service) Delete(ctx context.Context, repo, name string) (Metadata, error) {
	row, err := s.store.DeleteRepoSecret(ctx, repo, name)
	if err != nil {
		if errors.Is(err, db.ErrSecretNotFound) {
			return Metadata{}, ErrNotFound
		}
		return Metadata{}, fmt.Errorf("delete secret: %w", err)
	}
	return toMetadata(row), nil
}

// Reveal decrypts each requested name and records the access. Missing names
// are returned in missing rather than as an error — the scheduler can decide
// whether a missing user-allowlisted secret should fail the run or just be
// logged. Decryption errors abort the call: returning partial values when a
// later one failed could mask a KMS problem and is unsafe.
func (s *service) Reveal(ctx context.Context, repo string, names []string) (map[string][]byte, []string, error) {
	values := make(map[string][]byte, len(names))
	var missing []string
	var revealed []string
	for _, name := range names {
		row, err := s.store.GetRepoSecret(ctx, repo, name)
		if err != nil {
			if errors.Is(err, db.ErrSecretNotFound) {
				missing = append(missing, name)
				continue
			}
			return nil, nil, fmt.Errorf("get secret %q: %w", name, err)
		}
		pt, err := s.enc.Decrypt(ctx, Sealed{
			Ciphertext:   row.Ciphertext,
			Nonce:        row.Nonce,
			EncryptedDEK: row.EncryptedDEK,
			KMSKeyName:   row.KMSKeyName,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("decrypt secret %q: %w", name, err)
		}
		values[name] = pt
		revealed = append(revealed, name)
	}

	// Best-effort touch — last_used_at is observability, not a correctness
	// signal, so a touch failure must not deny CI access to a freshly-revealed
	// secret. Log the error and move on.
	for _, name := range revealed {
		if err := s.store.TouchRepoSecretLastUsed(ctx, repo, name); err != nil {
			slog.Warn("touch secret last_used_at failed",
				"repo", repo, "name", name, "error", err)
		}
	}

	return values, missing, nil
}

// validateName enforces the regex and the reserved-prefix rule.
func validateName(name string) error {
	if !nameRegexp.MatchString(name) {
		return ErrInvalidName
	}
	// Reserved prefix is checked after the regex so callers get the more
	// precise error when both conditions are violated (e.g. "docstore_x"
	// fails the regex first, which is the right signal).
	if len(name) >= len(reservedNamePrefix) && name[:len(reservedNamePrefix)] == reservedNamePrefix {
		return ErrReservedName
	}
	return nil
}

// toMetadata projects a db.RepoSecret onto Metadata, dropping every sealed
// field. This is the only place the conversion happens, so adding a new
// metadata column means a single edit here.
func toMetadata(r db.RepoSecret) Metadata {
	return Metadata{
		ID:          r.ID,
		Repo:        r.Repo,
		Name:        r.Name,
		Description: r.Description,
		SizeBytes:   r.SizeBytes,
		CreatedBy:   r.CreatedBy,
		CreatedAt:   r.CreatedAt,
		UpdatedBy:   r.UpdatedBy,
		UpdatedAt:   r.UpdatedAt,
		LastUsedAt:  r.LastUsedAt,
	}
}
