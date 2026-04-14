package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/dlorenc/docstore/internal/model"
	"github.com/google/uuid"
)

var (
	ErrBranchNotFound  = errors.New("branch not found")
	ErrBranchNotActive = errors.New("branch is not active")
)

// Store wraps a *sql.DB and provides transactional operations.
type Store struct {
	db *sql.DB
}

// NewStore returns a Store backed by the given database connection.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// Commit atomically commits one or more file changes to a branch.
// It locks the branch row with SELECT ... FOR UPDATE, allocates
// a new sequence number, deduplicates document content by hash,
// inserts file_commits rows, and advances the branch head.
func (s *Store) Commit(ctx context.Context, req model.CommitRequest) (*model.CommitResponse, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Lock the branch row and read current state.
	var headSeq int64
	var status string
	err = tx.QueryRowContext(ctx,
		"SELECT head_sequence, status FROM branches WHERE name = $1 FOR UPDATE",
		req.Branch,
	).Scan(&headSeq, &status)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrBranchNotFound
		}
		return nil, fmt.Errorf("lock branch: %w", err)
	}

	if status != "active" {
		return nil, ErrBranchNotActive
	}

	newSeq := headSeq + 1
	results := make([]model.CommitFileResult, 0, len(req.Files))

	for _, f := range req.Files {
		var versionIDPtr *string

		if f.Content != nil {
			// Hash content for dedup.
			h := sha256.Sum256(f.Content)
			contentHash := hex.EncodeToString(h[:])

			// Check for existing document with the same hash.
			var existingID string
			err = tx.QueryRowContext(ctx,
				"SELECT version_id FROM documents WHERE content_hash = $1 LIMIT 1",
				contentHash,
			).Scan(&existingID)

			if errors.Is(err, sql.ErrNoRows) {
				// Insert new document.
				existingID = uuid.New().String()
				_, err = tx.ExecContext(ctx,
					`INSERT INTO documents (version_id, path, content, content_hash, created_at, created_by)
					 VALUES ($1, $2, $3, $4, now(), $5)`,
					existingID, f.Path, f.Content, contentHash, req.Author,
				)
				if err != nil {
					return nil, fmt.Errorf("insert document: %w", err)
				}
			} else if err != nil {
				return nil, fmt.Errorf("check dedup: %w", err)
			}

			versionIDPtr = &existingID
		}
		// nil Content → delete: versionIDPtr stays nil.

		commitID := uuid.New().String()
		_, err = tx.ExecContext(ctx,
			`INSERT INTO file_commits (commit_id, sequence, path, version_id, branch, message, author, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, now())`,
			commitID, newSeq, f.Path, versionIDPtr, req.Branch, req.Message, req.Author,
		)
		if err != nil {
			return nil, fmt.Errorf("insert file_commit: %w", err)
		}

		results = append(results, model.CommitFileResult{
			Path:      f.Path,
			VersionID: versionIDPtr,
		})
	}

	// Advance branch head.
	_, err = tx.ExecContext(ctx,
		"UPDATE branches SET head_sequence = $1 WHERE name = $2",
		newSeq, req.Branch,
	)
	if err != nil {
		return nil, fmt.Errorf("update branch head: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}

	return &model.CommitResponse{
		Sequence: newSeq,
		Files:    results,
	}, nil
}
