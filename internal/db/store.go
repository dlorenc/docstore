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
	ErrBranchExists    = errors.New("branch already exists")
	ErrMergeConflict   = errors.New("merge conflict")
)

// MergeConflict holds details about a conflicting path during merge.
type MergeConflict struct {
	Path            string
	MainVersionID   string
	BranchVersionID string
}

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

// CreateBranch creates a new branch forked from main's current head.
// It locks the main branch row to get a consistent base_sequence.
func (s *Store) CreateBranch(ctx context.Context, req model.CreateBranchRequest) (*model.CreateBranchResponse, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Read main's head to set as the base_sequence.
	var mainHead int64
	err = tx.QueryRowContext(ctx,
		"SELECT head_sequence FROM branches WHERE name = 'main' FOR UPDATE",
	).Scan(&mainHead)
	if err != nil {
		return nil, fmt.Errorf("read main head: %w", err)
	}

	// Insert the new branch. Unique constraint on name prevents duplicates.
	_, err = tx.ExecContext(ctx,
		"INSERT INTO branches (name, head_sequence, base_sequence, status) VALUES ($1, $2, $3, 'active')",
		req.Name, mainHead, mainHead,
	)
	if err != nil {
		// Check for unique violation (branch already exists).
		if isDuplicateKeyError(err) {
			return nil, ErrBranchExists
		}
		return nil, fmt.Errorf("insert branch: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}

	return &model.CreateBranchResponse{
		Name:         req.Name,
		BaseSequence: mainHead,
	}, nil
}

// Merge merges a branch into main. It detects conflicts (paths changed on
// both the branch and main since the branch's base_sequence) and either
// aborts with conflict details or fast-forwards main.
//
// On conflict, it returns a non-nil []MergeConflict and ErrMergeConflict.
func (s *Store) Merge(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []MergeConflict, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Lock the source branch.
	var branchHead, baseSeq int64
	var branchStatus string
	err = tx.QueryRowContext(ctx,
		"SELECT head_sequence, base_sequence, status FROM branches WHERE name = $1 FOR UPDATE",
		req.Branch,
	).Scan(&branchHead, &baseSeq, &branchStatus)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrBranchNotFound
		}
		return nil, nil, fmt.Errorf("lock branch: %w", err)
	}
	if branchStatus != "active" {
		return nil, nil, ErrBranchNotActive
	}

	// Lock main.
	var mainHead int64
	err = tx.QueryRowContext(ctx,
		"SELECT head_sequence FROM branches WHERE name = 'main' FOR UPDATE",
	).Scan(&mainHead)
	if err != nil {
		return nil, nil, fmt.Errorf("lock main: %w", err)
	}

	// Step 1: Find the latest version of each path changed on the branch since base_sequence.
	branchChanges, err := latestChanges(ctx, tx, req.Branch, baseSeq)
	if err != nil {
		return nil, nil, fmt.Errorf("branch changes: %w", err)
	}

	if len(branchChanges) == 0 {
		// Nothing to merge — mark branch as merged anyway.
		_, err = tx.ExecContext(ctx,
			"UPDATE branches SET status = 'merged' WHERE name = $1",
			req.Branch,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("update branch status: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, nil, fmt.Errorf("commit tx: %w", err)
		}
		return &model.MergeResponse{Sequence: mainHead}, nil, nil
	}

	// Step 2: Find the latest version of each path changed on main since base_sequence.
	mainChanges, err := latestChanges(ctx, tx, "main", baseSeq)
	if err != nil {
		return nil, nil, fmt.Errorf("main changes: %w", err)
	}

	// Step 3: Conflict detection — any path in both sets is a conflict.
	var conflicts []MergeConflict
	for path, branchVID := range branchChanges {
		if mainVID, ok := mainChanges[path]; ok {
			conflicts = append(conflicts, MergeConflict{
				Path:            path,
				MainVersionID:   nullStr(mainVID),
				BranchVersionID: nullStr(branchVID),
			})
		}
	}
	if len(conflicts) > 0 {
		return nil, conflicts, ErrMergeConflict
	}

	// Step 4: No conflicts — insert new file_commits on main for each branch-changed path.
	newSeq := mainHead + 1

	for path, versionID := range branchChanges {
		commitID := uuid.New().String()
		_, err = tx.ExecContext(ctx,
			`INSERT INTO file_commits (commit_id, sequence, path, version_id, branch, message, author, created_at)
			 VALUES ($1, $2, $3, $4, 'main', $5, $6, now())`,
			commitID, newSeq, path, versionID,
			fmt.Sprintf("merge branch '%s'", req.Branch), "system",
		)
		if err != nil {
			return nil, nil, fmt.Errorf("insert merge commit: %w", err)
		}
	}

	// Advance main head.
	_, err = tx.ExecContext(ctx,
		"UPDATE branches SET head_sequence = $1 WHERE name = 'main'",
		newSeq,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("update main head: %w", err)
	}

	// Mark the branch as merged.
	_, err = tx.ExecContext(ctx,
		"UPDATE branches SET status = 'merged' WHERE name = $1",
		req.Branch,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("update branch status: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit tx: %w", err)
	}

	return &model.MergeResponse{Sequence: newSeq}, nil, nil
}

// latestChanges returns a map of path → *version_id for the latest version of
// each path changed on a branch since the given base sequence.
func latestChanges(ctx context.Context, tx *sql.Tx, branch string, baseSeq int64) (map[string]*string, error) {
	const q = `
SELECT DISTINCT ON (path) path, version_id::text
FROM file_commits
WHERE branch = $1 AND sequence > $2
ORDER BY path, sequence DESC`

	rows, err := tx.QueryContext(ctx, q, branch, baseSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	changes := make(map[string]*string)
	for rows.Next() {
		var path string
		var vid sql.NullString
		if err := rows.Scan(&path, &vid); err != nil {
			return nil, err
		}
		if vid.Valid {
			v := vid.String
			changes[path] = &v
		} else {
			changes[path] = nil
		}
	}
	return changes, rows.Err()
}

func nullStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// isDuplicateKeyError checks if a PostgreSQL error is a unique violation (23505).
func isDuplicateKeyError(err error) bool {
	if err == nil {
		return false
	}
	// pq library returns *pq.Error with Code "23505" for unique_violation.
	return fmt.Sprintf("%v", err) != "" && contains23505(err.Error())
}

func contains23505(s string) bool {
	return len(s) > 0 && containsStr(s, "23505")
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
