package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ErrSecretNotFound is returned by repo-secret operations when no row matches
// the given (repo, name) tuple.
var ErrSecretNotFound = errors.New("repo secret not found")

// RepoSecret is a sealed secret keyed by (repo, name). Ciphertext, Nonce, and
// EncryptedDEK are produced by the encryptor service; the store treats them
// as opaque bytes.
type RepoSecret struct {
	ID           string
	Repo         string
	Name         string
	Description  string
	Ciphertext   []byte
	Nonce        []byte
	EncryptedDEK []byte
	KMSKeyName   string
	SizeBytes    int
	CreatedBy    string
	CreatedAt    time.Time
	UpdatedBy    *string
	UpdatedAt    *time.Time
	LastUsedAt   *time.Time
}

// repoSecretColumns is the ordered list of columns returned by SELECT * on
// repo_secrets.
const repoSecretColumns = `id, repo, name, description, ciphertext, nonce, encrypted_dek,
	kms_key_name, size_bytes, created_by, created_at, updated_by, updated_at, last_used_at`

// scanRepoSecret scans a single repo_secrets row into a RepoSecret.
func scanRepoSecret(row interface {
	Scan(...any) error
}) (RepoSecret, error) {
	var rs RepoSecret
	var updatedBy sql.NullString
	var updatedAt sql.NullTime
	var lastUsedAt sql.NullTime
	if err := row.Scan(
		&rs.ID, &rs.Repo, &rs.Name, &rs.Description,
		&rs.Ciphertext, &rs.Nonce, &rs.EncryptedDEK,
		&rs.KMSKeyName, &rs.SizeBytes,
		&rs.CreatedBy, &rs.CreatedAt,
		&updatedBy, &updatedAt, &lastUsedAt,
	); err != nil {
		return RepoSecret{}, err
	}
	if updatedBy.Valid {
		rs.UpdatedBy = &updatedBy.String
	}
	if updatedAt.Valid {
		rs.UpdatedAt = &updatedAt.Time
	}
	if lastUsedAt.Valid {
		rs.LastUsedAt = &lastUsedAt.Time
	}
	return rs, nil
}

// SetRepoSecret inserts or updates a repo secret keyed by (repo, name).
//
// On insert: id, created_by, and created_at are taken from the input (or
// generated when missing) and written verbatim. On update by (repo, name):
// the existing id, created_by, and created_at are preserved, last_used_at
// is left untouched, and updated_by/updated_at are bumped to the caller's
// identity / now(). The remaining sealed fields and metadata are replaced
// with the new values.
//
// Returns the row as written.
func (s *Store) SetRepoSecret(ctx context.Context, in RepoSecret) (RepoSecret, error) {
	if in.ID == "" {
		in.ID = uuid.New().String()
	}
	if in.CreatedAt.IsZero() {
		in.CreatedAt = time.Now().UTC()
	} else {
		in.CreatedAt = in.CreatedAt.UTC()
	}
	now := time.Now().UTC()

	row := s.db.QueryRowContext(ctx,
		`INSERT INTO repo_secrets (
			id, repo, name, description, ciphertext, nonce, encrypted_dek,
			kms_key_name, size_bytes, created_by, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (repo, name) DO UPDATE SET
			description   = EXCLUDED.description,
			ciphertext    = EXCLUDED.ciphertext,
			nonce         = EXCLUDED.nonce,
			encrypted_dek = EXCLUDED.encrypted_dek,
			kms_key_name  = EXCLUDED.kms_key_name,
			size_bytes    = EXCLUDED.size_bytes,
			updated_by    = $12,
			updated_at    = $13
		RETURNING `+repoSecretColumns,
		in.ID, in.Repo, in.Name, in.Description,
		in.Ciphertext, in.Nonce, in.EncryptedDEK,
		in.KMSKeyName, in.SizeBytes,
		in.CreatedBy, in.CreatedAt,
		in.CreatedBy, now,
	)
	rs, err := scanRepoSecret(row)
	if err != nil {
		return RepoSecret{}, fmt.Errorf("set repo secret: %w", err)
	}
	return rs, nil
}

// GetRepoSecret returns the repo secret keyed by (repo, name). Returns
// ErrSecretNotFound if no such row exists.
func (s *Store) GetRepoSecret(ctx context.Context, repo, name string) (RepoSecret, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+repoSecretColumns+` FROM repo_secrets WHERE repo = $1 AND name = $2`,
		repo, name,
	)
	rs, err := scanRepoSecret(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RepoSecret{}, ErrSecretNotFound
		}
		return RepoSecret{}, fmt.Errorf("get repo secret: %w", err)
	}
	return rs, nil
}

// ListRepoSecrets returns all repo secrets for a repo, ordered by name ASC.
// Sealed fields are populated; callers that only need metadata should
// project away ciphertext/nonce/encrypted_dek themselves.
func (s *Store) ListRepoSecrets(ctx context.Context, repo string) ([]RepoSecret, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+repoSecretColumns+` FROM repo_secrets WHERE repo = $1 ORDER BY name ASC`,
		repo,
	)
	if err != nil {
		return nil, fmt.Errorf("list repo secrets: %w", err)
	}
	defer rows.Close()

	var secrets []RepoSecret
	for rows.Next() {
		rs, err := scanRepoSecret(rows)
		if err != nil {
			return nil, fmt.Errorf("list repo secrets: scan: %w", err)
		}
		secrets = append(secrets, rs)
	}
	if secrets == nil {
		secrets = []RepoSecret{}
	}
	return secrets, rows.Err()
}

// DeleteRepoSecret removes the repo secret keyed by (repo, name). Returns
// ErrSecretNotFound if no row was deleted.
func (s *Store) DeleteRepoSecret(ctx context.Context, repo, name string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM repo_secrets WHERE repo = $1 AND name = $2`,
		repo, name,
	)
	if err != nil {
		return fmt.Errorf("delete repo secret: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete repo secret: rows affected: %w", err)
	}
	if n == 0 {
		return ErrSecretNotFound
	}
	return nil
}

// TouchRepoSecretLastUsed sets last_used_at = now() for (repo, name). It is
// idempotent and does not return an error if the row is missing — callers
// have already observed the row via GetRepoSecret before touching it.
func (s *Store) TouchRepoSecretLastUsed(ctx context.Context, repo, name string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE repo_secrets SET last_used_at = now() WHERE repo = $1 AND name = $2`,
		repo, name,
	)
	if err != nil {
		return fmt.Errorf("touch repo secret last used: %w", err)
	}
	return nil
}
