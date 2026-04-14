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
	"github.com/lib/pq"
)

var (
	ErrBranchNotFound  = errors.New("branch not found")
	ErrBranchNotActive = errors.New("branch is not active")
	ErrBranchExists    = errors.New("branch already exists")
	ErrMergeConflict   = errors.New("merge conflict")
	ErrRebaseConflict  = errors.New("rebase conflict")
	ErrRepoNotFound    = errors.New("repo not found")
	ErrRepoExists      = errors.New("repo already exists")
)

// rebaseFile is one file within a replayed commit group.
type rebaseFile struct {
	path      string
	versionID *string
}

// rebaseGroup is one original commit's worth of file changes to be replayed.
type rebaseGroup struct {
	seq   int64
	files []rebaseFile
}

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

// CreateRepo creates a new repo and seeds its main branch in a single
// transaction. Returns ErrRepoExists if the name is taken.
func (s *Store) CreateRepo(ctx context.Context, req model.CreateRepoRequest) (*model.Repo, error) {
	createdBy := req.CreatedBy
	if createdBy == "" {
		createdBy = "system"
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var r model.Repo
	err = tx.QueryRowContext(ctx,
		`INSERT INTO repos (name, created_by) VALUES ($1, $2)
		 RETURNING name, created_at, created_by`,
		req.Name, createdBy,
	).Scan(&r.Name, &r.CreatedAt, &r.CreatedBy)
	if err != nil {
		if isDuplicateKeyError(err) {
			return nil, ErrRepoExists
		}
		return nil, fmt.Errorf("insert repo: %w", err)
	}

	// Seed the main branch for the new repo.
	_, err = tx.ExecContext(ctx,
		"INSERT INTO branches (repo, name, head_sequence, base_sequence, status) VALUES ($1, 'main', 0, 0, 'active')",
		req.Name,
	)
	if err != nil {
		return nil, fmt.Errorf("seed main branch: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}

	return &r, nil
}

// DeleteRepo hard-deletes a repo and all its data in a single transaction.
func (s *Store) DeleteRepo(ctx context.Context, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Verify repo exists.
	var exists bool
	err = tx.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM repos WHERE name = $1)", name,
	).Scan(&exists)
	if err != nil {
		return fmt.Errorf("check repo: %w", err)
	}
	if !exists {
		return ErrRepoNotFound
	}

	// Delete all dependent data in order (child tables first).
	for _, table := range []string{"check_runs", "reviews", "file_commits", "commits", "documents", "branches", "roles"} {
		_, err = tx.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE repo = $1", table), name)
		if err != nil {
			return fmt.Errorf("delete %s: %w", table, err)
		}
	}

	_, err = tx.ExecContext(ctx, "DELETE FROM repos WHERE name = $1", name)
	if err != nil {
		return fmt.Errorf("delete repo: %w", err)
	}

	return tx.Commit()
}

// ListRepos returns all repos ordered by name.
func (s *Store) ListRepos(ctx context.Context) ([]model.Repo, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT name, created_at, created_by FROM repos ORDER BY name",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var repos []model.Repo
	for rows.Next() {
		var r model.Repo
		if err := rows.Scan(&r.Name, &r.CreatedAt, &r.CreatedBy); err != nil {
			return nil, err
		}
		repos = append(repos, r)
	}
	return repos, rows.Err()
}

// GetRepo returns a single repo by name, or ErrRepoNotFound.
func (s *Store) GetRepo(ctx context.Context, name string) (*model.Repo, error) {
	var r model.Repo
	err := s.db.QueryRowContext(ctx,
		"SELECT name, created_at, created_by FROM repos WHERE name = $1",
		name,
	).Scan(&r.Name, &r.CreatedAt, &r.CreatedBy)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRepoNotFound
		}
		return nil, err
	}
	return &r, nil
}

// Commit atomically commits one or more file changes to a branch.
// It locks the branch row with SELECT ... FOR UPDATE, allocates
// a new sequence number, deduplicates document content by hash (per-repo),
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
		"SELECT head_sequence, status FROM branches WHERE repo = $1 AND name = $2 FOR UPDATE",
		req.Repo, req.Branch,
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

	// Allocate a globally monotonic sequence by inserting into commits.
	var newSeq int64
	err = tx.QueryRowContext(ctx,
		`INSERT INTO commits (repo, branch, message, author) VALUES ($1, $2, $3, $4) RETURNING sequence`,
		req.Repo, req.Branch, req.Message, req.Author,
	).Scan(&newSeq)
	if err != nil {
		return nil, fmt.Errorf("insert commit: %w", err)
	}

	results := make([]model.CommitFileResult, 0, len(req.Files))

	for _, f := range req.Files {
		var versionIDPtr *string

		if f.Content != nil {
			// Hash content for per-repo dedup.
			h := sha256.Sum256(f.Content)
			contentHash := hex.EncodeToString(h[:])

			// Check for existing document with the same hash in this repo.
			var existingID string
			err = tx.QueryRowContext(ctx,
				"SELECT version_id FROM documents WHERE repo = $1 AND content_hash = $2 LIMIT 1",
				req.Repo, contentHash,
			).Scan(&existingID)

			if errors.Is(err, sql.ErrNoRows) {
				// Insert new document.
				existingID = uuid.New().String()
				_, err = tx.ExecContext(ctx,
					`INSERT INTO documents (repo, version_id, path, content, content_hash, created_at, created_by)
					 VALUES ($1, $2, $3, $4, $5, now(), $6)`,
					req.Repo, existingID, f.Path, f.Content, contentHash, req.Author,
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
			`INSERT INTO file_commits (repo, commit_id, sequence, path, version_id, branch)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			req.Repo, commitID, newSeq, f.Path, versionIDPtr, req.Branch,
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
		"UPDATE branches SET head_sequence = $1 WHERE repo = $2 AND name = $3",
		newSeq, req.Repo, req.Branch,
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
	// If main doesn't exist the repo itself doesn't exist.
	var mainHead int64
	err = tx.QueryRowContext(ctx,
		"SELECT head_sequence FROM branches WHERE repo = $1 AND name = 'main' FOR UPDATE",
		req.Repo,
	).Scan(&mainHead)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRepoNotFound
		}
		return nil, fmt.Errorf("read main head: %w", err)
	}

	// Insert the new branch. Unique constraint on (repo, name) prevents duplicates.
	_, err = tx.ExecContext(ctx,
		"INSERT INTO branches (repo, name, head_sequence, base_sequence, status) VALUES ($1, $2, $3, $4, 'active')",
		req.Repo, req.Name, mainHead, mainHead,
	)
	if err != nil {
		if isDuplicateKeyError(err) {
			return nil, ErrBranchExists
		}
		if isForeignKeyViolation(err) {
			return nil, ErrRepoNotFound
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

	// Lock main first, then source branch — consistent ordering prevents deadlocks.
	var mainHead int64
	err = tx.QueryRowContext(ctx,
		"SELECT head_sequence FROM branches WHERE repo = $1 AND name = 'main' FOR UPDATE",
		req.Repo,
	).Scan(&mainHead)
	if err != nil {
		return nil, nil, fmt.Errorf("lock main: %w", err)
	}

	// Lock the source branch.
	var branchHead, baseSeq int64
	var branchStatus string
	err = tx.QueryRowContext(ctx,
		"SELECT head_sequence, base_sequence, status FROM branches WHERE repo = $1 AND name = $2 FOR UPDATE",
		req.Repo, req.Branch,
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

	// Step 1: Find the latest version of each path changed on the branch since base_sequence.
	branchChanges, err := latestChanges(ctx, tx, req.Repo, req.Branch, baseSeq)
	if err != nil {
		return nil, nil, fmt.Errorf("branch changes: %w", err)
	}

	if len(branchChanges) == 0 {
		// Nothing to merge — mark branch as merged anyway.
		_, err = tx.ExecContext(ctx,
			"UPDATE branches SET status = 'merged' WHERE repo = $1 AND name = $2",
			req.Repo, req.Branch,
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
	mainChanges, err := latestChanges(ctx, tx, req.Repo, "main", baseSeq)
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

	// Step 4: No conflicts — allocate a global sequence for the merge commit,
	// then insert file_commits rows on main for each branch-changed path.
	var newSeq int64
	err = tx.QueryRowContext(ctx,
		`INSERT INTO commits (repo, branch, message, author) VALUES ($1, 'main', $2, $3) RETURNING sequence`,
		req.Repo, fmt.Sprintf("merge branch '%s'", req.Branch), req.Author,
	).Scan(&newSeq)
	if err != nil {
		return nil, nil, fmt.Errorf("insert merge commit: %w", err)
	}

	for path, versionID := range branchChanges {
		commitID := uuid.New().String()
		_, err = tx.ExecContext(ctx,
			`INSERT INTO file_commits (repo, commit_id, sequence, path, version_id, branch)
			 VALUES ($1, $2, $3, $4, $5, 'main')`,
			req.Repo, commitID, newSeq, path, versionID,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("insert file_commit: %w", err)
		}
	}

	// Advance main head.
	_, err = tx.ExecContext(ctx,
		"UPDATE branches SET head_sequence = $1 WHERE repo = $2 AND name = 'main'",
		newSeq, req.Repo,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("update main head: %w", err)
	}

	// Mark the branch as merged.
	_, err = tx.ExecContext(ctx,
		"UPDATE branches SET status = 'merged' WHERE repo = $1 AND name = $2",
		req.Repo, req.Branch,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("update branch status: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit tx: %w", err)
	}

	return &model.MergeResponse{Sequence: newSeq}, nil, nil
}

// Rebase replays a branch's file_commits onto main's current head.
// It locks main first, then the source branch. All replayed groups get new global
// sequences. On conflict, the entire transaction is rolled back.
func (s *Store) Rebase(ctx context.Context, req model.RebaseRequest) (*model.RebaseResponse, []MergeConflict, error) {
	if req.Branch == "main" {
		return nil, nil, ErrBranchNotActive
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Lock main first, then source branch — consistent ordering prevents deadlocks.
	var mainHead int64
	err = tx.QueryRowContext(ctx,
		"SELECT head_sequence FROM branches WHERE repo = $1 AND name = 'main' FOR UPDATE",
		req.Repo,
	).Scan(&mainHead)
	if err != nil {
		return nil, nil, fmt.Errorf("lock main: %w", err)
	}

	// Lock the source branch.
	var branchHead, baseSeq int64
	var branchStatus string
	err = tx.QueryRowContext(ctx,
		"SELECT head_sequence, base_sequence, status FROM branches WHERE repo = $1 AND name = $2 FOR UPDATE",
		req.Repo, req.Branch,
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

	// Get all paths changed on branch since baseSeq.
	branchChanges, err := latestChanges(ctx, tx, req.Repo, req.Branch, baseSeq)
	if err != nil {
		return nil, nil, fmt.Errorf("branch changes: %w", err)
	}

	// Empty branch — just update base_sequence and head_sequence (no-op rebase).
	if len(branchChanges) == 0 {
		_, err = tx.ExecContext(ctx,
			"UPDATE branches SET base_sequence = $1, head_sequence = $2 WHERE repo = $3 AND name = $4",
			mainHead, mainHead, req.Repo, req.Branch,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("update branch: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, nil, fmt.Errorf("commit tx: %w", err)
		}
		return &model.RebaseResponse{
			NewBaseSequence: mainHead,
			NewHeadSequence: mainHead,
			CommitsReplayed: 0,
		}, nil, nil
	}

	// Get all paths changed on main since baseSeq.
	mainChanges, err := latestChanges(ctx, tx, req.Repo, "main", baseSeq)
	if err != nil {
		return nil, nil, fmt.Errorf("main changes: %w", err)
	}

	// Conflict detection — any path changed on both is a conflict.
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
		return nil, conflicts, ErrRebaseConflict
	}

	// Collect original commit groups ordered by sequence.
	groups, err := branchCommitGroups(ctx, tx, req.Repo, req.Branch, baseSeq)
	if err != nil {
		return nil, nil, fmt.Errorf("branch commit groups: %w", err)
	}

	// Replay each group as a new global sequence on the branch.
	author := req.Author
	if author == "" {
		author = "system"
	}
	var lastSeq int64
	for _, g := range groups {
		var newSeq int64
		err = tx.QueryRowContext(ctx,
			`INSERT INTO commits (repo, branch, message, author) VALUES ($1, $2, $3, $4) RETURNING sequence`,
			req.Repo, req.Branch, fmt.Sprintf("rebase: replay sequence %d", g.seq), author,
		).Scan(&newSeq)
		if err != nil {
			return nil, nil, fmt.Errorf("insert rebase commit: %w", err)
		}

		for _, f := range g.files {
			commitID := uuid.New().String()
			_, err = tx.ExecContext(ctx,
				`INSERT INTO file_commits (repo, commit_id, sequence, path, version_id, branch)
				 VALUES ($1, $2, $3, $4, $5, $6)`,
				req.Repo, commitID, newSeq, f.path, f.versionID, req.Branch,
			)
			if err != nil {
				return nil, nil, fmt.Errorf("insert file_commit: %w", err)
			}
		}
		lastSeq = newSeq
	}

	// Update branch: base_sequence = mainHead, head_sequence = lastSeq.
	_, err = tx.ExecContext(ctx,
		"UPDATE branches SET base_sequence = $1, head_sequence = $2 WHERE repo = $3 AND name = $4",
		mainHead, lastSeq, req.Repo, req.Branch,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("update branch: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit tx: %w", err)
	}

	return &model.RebaseResponse{
		NewBaseSequence: mainHead,
		NewHeadSequence: lastSeq,
		CommitsReplayed: int64(len(groups)),
	}, nil, nil
}

// branchCommitGroups returns the file_commits on a branch since baseSeq,
// grouped by original sequence in ascending order.
func branchCommitGroups(ctx context.Context, tx *sql.Tx, repo, branch string, baseSeq int64) ([]rebaseGroup, error) {
	const q = `
SELECT sequence, path, version_id::text
FROM file_commits
WHERE repo = $1 AND branch = $2 AND sequence > $3
ORDER BY sequence ASC, path ASC`

	rows, err := tx.QueryContext(ctx, q, repo, branch, baseSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups []rebaseGroup
	for rows.Next() {
		var seq int64
		var path string
		var vid sql.NullString
		if err := rows.Scan(&seq, &path, &vid); err != nil {
			return nil, err
		}

		var versionID *string
		if vid.Valid {
			v := vid.String
			versionID = &v
		}

		if len(groups) == 0 || groups[len(groups)-1].seq != seq {
			groups = append(groups, rebaseGroup{seq: seq})
		}
		groups[len(groups)-1].files = append(groups[len(groups)-1].files, rebaseFile{
			path:      path,
			versionID: versionID,
		})
	}
	return groups, rows.Err()
}

// latestChanges returns a map of path → *version_id for the latest version of
// each path changed on a branch since the given base sequence.
func latestChanges(ctx context.Context, tx *sql.Tx, repo, branch string, baseSeq int64) (map[string]*string, error) {
	const q = `
SELECT DISTINCT ON (path) path, version_id::text
FROM file_commits
WHERE repo = $1 AND branch = $2 AND sequence > $3
ORDER BY path, sequence DESC`

	rows, err := tx.QueryContext(ctx, q, repo, branch, baseSeq)
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

// DeleteBranch marks a branch as abandoned. It returns ErrBranchNotFound if
// the branch does not exist, and ErrBranchNotActive if it is already merged
// or abandoned (i.e. not active).
func (s *Store) DeleteBranch(ctx context.Context, repo, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var status string
	err = tx.QueryRowContext(ctx,
		"SELECT status FROM branches WHERE repo = $1 AND name = $2 FOR UPDATE",
		repo, name,
	).Scan(&status)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrBranchNotFound
		}
		return fmt.Errorf("lock branch: %w", err)
	}

	if status != "active" {
		return ErrBranchNotActive
	}

	_, err = tx.ExecContext(ctx,
		"UPDATE branches SET status = 'abandoned' WHERE repo = $1 AND name = $2",
		repo, name,
	)
	if err != nil {
		return fmt.Errorf("update branch status: %w", err)
	}

	return tx.Commit()
}

// isDuplicateKeyError checks if a PostgreSQL error is a unique violation (23505).
func isDuplicateKeyError(err error) bool {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return pqErr.Code == "23505"
	}
	return false
}

// isForeignKeyViolation checks if a PostgreSQL error is a FK violation (23503).
func isForeignKeyViolation(err error) bool {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return pqErr.Code == "23503"
	}
	return false
}
