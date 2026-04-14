package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

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
	ErrRoleNotFound    = errors.New("role not found")
	ErrSelfApproval    = errors.New("reviewer cannot approve their own commits")
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
		slog.Error("tx begin failed", "op", "create_repo", "error", err)
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			slog.Error("tx rollback failed", "op", "create_repo", "error", rollbackErr)
		}
	}()

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
		slog.Error("tx begin failed", "op", "delete_repo", "error", err)
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			slog.Error("tx rollback failed", "op", "delete_repo", "error", rollbackErr)
		}
	}()

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
		slog.Error("tx begin failed", "op", "commit", "error", err)
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			slog.Error("tx rollback failed", "op", "commit", "error", rollbackErr)
		}
	}()

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
		slog.Error("tx begin failed", "op", "create_branch", "error", err)
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			slog.Error("tx rollback failed", "op", "create_branch", "error", rollbackErr)
		}
	}()

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
	slog.Debug("merge started", "repo", req.Repo, "branch", req.Branch)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		slog.Error("tx begin failed", "op", "merge", "error", err)
		return nil, nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			slog.Error("tx rollback failed", "op", "merge", "error", rollbackErr)
		}
	}()

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
		slog.Info("merge complete", "repo", req.Repo, "branch", req.Branch, "sequence", mainHead, "files", 0)
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

	slog.Info("merge complete", "repo", req.Repo, "branch", req.Branch, "sequence", newSeq, "files", len(branchChanges))
	return &model.MergeResponse{Sequence: newSeq}, nil, nil
}

// Rebase replays a branch's file_commits onto main's current head.
// It locks main first, then the source branch. All replayed groups get new global
// sequences. On conflict, the entire transaction is rolled back.
func (s *Store) Rebase(ctx context.Context, req model.RebaseRequest) (*model.RebaseResponse, []MergeConflict, error) {
	if req.Branch == "main" {
		return nil, nil, ErrBranchNotActive
	}

	slog.Debug("rebase started", "repo", req.Repo, "branch", req.Branch)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		slog.Error("tx begin failed", "op", "rebase", "error", err)
		return nil, nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			slog.Error("tx rollback failed", "op", "rebase", "error", rollbackErr)
		}
	}()

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
		slog.Info("rebase complete", "repo", req.Repo, "branch", req.Branch, "commits_replayed", 0)
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

	slog.Info("rebase complete", "repo", req.Repo, "branch", req.Branch, "commits_replayed", len(groups))
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
// the branch does not exist, ErrBranchNotActive if it is already merged,
// abandoned, or is the protected "main" branch.
func (s *Store) DeleteBranch(ctx context.Context, repo, name string) error {
	if name == "main" {
		return ErrBranchNotActive
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		slog.Error("tx begin failed", "op", "delete_branch", "error", err)
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			slog.Error("tx rollback failed", "op", "delete_branch", "error", rollbackErr)
		}
	}()

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

// CreateReview records a review for a branch at its current head_sequence.
// Returns ErrBranchNotFound if the branch doesn't exist.
// Returns ErrSelfApproval if the reviewer authored any commits on the branch and
// is attempting to approve (status == ReviewApproved).
func (s *Store) CreateReview(ctx context.Context, repo, branch, reviewer string, status model.ReviewStatus, body string) (*model.Review, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		slog.Error("tx begin failed", "op", "create_review", "error", err)
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			slog.Error("tx rollback failed", "op", "create_review", "error", rollbackErr)
		}
	}()

	var headSeq, baseSeq int64
	err = tx.QueryRowContext(ctx,
		"SELECT head_sequence, base_sequence FROM branches WHERE repo = $1 AND name = $2 FOR UPDATE",
		repo, branch,
	).Scan(&headSeq, &baseSeq)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrBranchNotFound
		}
		return nil, fmt.Errorf("lock branch: %w", err)
	}

	// Self-approval: reviewer cannot approve if they authored any branch commits.
	if status == model.ReviewApproved {
		var count int
		err = tx.QueryRowContext(ctx,
			"SELECT count(*) FROM commits WHERE repo = $1 AND branch = $2 AND sequence > $3 AND author = $4",
			repo, branch, baseSeq, reviewer,
		).Scan(&count)
		if err != nil {
			return nil, fmt.Errorf("check self-approval: %w", err)
		}
		if count > 0 {
			return nil, ErrSelfApproval
		}
	}

	id := uuid.New().String()
	var createdAt time.Time
	var nullBody sql.NullString
	if body != "" {
		nullBody = sql.NullString{String: body, Valid: true}
	}
	err = tx.QueryRowContext(ctx,
		`INSERT INTO reviews (id, repo, branch, reviewer, sequence, status, body)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING created_at`,
		id, repo, branch, reviewer, headSeq, string(status), nullBody,
	).Scan(&createdAt)
	if err != nil {
		return nil, fmt.Errorf("insert review: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}

	return &model.Review{
		ID:        id,
		Branch:    branch,
		Reviewer:  reviewer,
		Sequence:  headSeq,
		Status:    status,
		Body:      body,
		CreatedAt: createdAt,
	}, nil
}

// ListReviews returns reviews for a branch in the given repo, ordered by
// created_at DESC. If atSeq is non-nil, only reviews at that sequence are
// returned.
func (s *Store) ListReviews(ctx context.Context, repo, branch string, atSeq *int64) ([]model.Review, error) {
	q := `SELECT id, reviewer, sequence, status, COALESCE(body, ''), created_at
	      FROM reviews
	      WHERE repo = $1 AND branch = $2`
	args := []interface{}{repo, branch}
	if atSeq != nil {
		q += " AND sequence = $3"
		args = append(args, *atSeq)
	}
	q += " ORDER BY created_at DESC"

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var reviews []model.Review
	for rows.Next() {
		var rev model.Review
		rev.Branch = branch
		var statusStr string
		if err := rows.Scan(&rev.ID, &rev.Reviewer, &rev.Sequence, &statusStr, &rev.Body, &rev.CreatedAt); err != nil {
			return nil, err
		}
		rev.Status = model.ReviewStatus(statusStr)
		reviews = append(reviews, rev)
	}
	return reviews, rows.Err()
}

// CreateCheckRun records a CI check run for a branch at its current
// head_sequence. Returns ErrBranchNotFound if the branch doesn't exist.
func (s *Store) CreateCheckRun(ctx context.Context, repo, branch, checkName string, status model.CheckRunStatus, reporter string) (*model.CheckRun, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		slog.Error("tx begin failed", "op", "create_check_run", "error", err)
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			slog.Error("tx rollback failed", "op", "create_check_run", "error", rollbackErr)
		}
	}()

	var headSeq int64
	err = tx.QueryRowContext(ctx,
		"SELECT head_sequence FROM branches WHERE repo = $1 AND name = $2 FOR UPDATE",
		repo, branch,
	).Scan(&headSeq)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrBranchNotFound
		}
		return nil, fmt.Errorf("lock branch: %w", err)
	}

	id := uuid.New().String()
	var createdAt time.Time
	err = tx.QueryRowContext(ctx,
		`INSERT INTO check_runs (id, repo, branch, sequence, check_name, status, reporter)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING created_at`,
		id, repo, branch, headSeq, checkName, string(status), reporter,
	).Scan(&createdAt)
	if err != nil {
		return nil, fmt.Errorf("insert check_run: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}

	return &model.CheckRun{
		ID:        id,
		Branch:    branch,
		Sequence:  headSeq,
		CheckName: checkName,
		Status:    status,
		Reporter:  reporter,
		CreatedAt: createdAt,
	}, nil
}

// ListCheckRuns returns check runs for a branch in the given repo, ordered by
// created_at DESC. If atSeq is non-nil, only check runs at that sequence are
// returned.
func (s *Store) ListCheckRuns(ctx context.Context, repo, branch string, atSeq *int64) ([]model.CheckRun, error) {
	q := `SELECT id, sequence, check_name, status, reporter, created_at
	      FROM check_runs
	      WHERE repo = $1 AND branch = $2`
	args := []interface{}{repo, branch}
	if atSeq != nil {
		q += " AND sequence = $3"
		args = append(args, *atSeq)
	}
	q += " ORDER BY created_at DESC"

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var checkRuns []model.CheckRun
	for rows.Next() {
		var cr model.CheckRun
		cr.Branch = branch
		var statusStr string
		if err := rows.Scan(&cr.ID, &cr.Sequence, &cr.CheckName, &statusStr, &cr.Reporter, &cr.CreatedAt); err != nil {
			return nil, err
		}
		cr.Status = model.CheckRunStatus(statusStr)
		checkRuns = append(checkRuns, cr)
	}
	return checkRuns, rows.Err()
}

// GetRole returns the role for an identity in a repo, or ErrRoleNotFound.
func (s *Store) GetRole(ctx context.Context, repo, identity string) (*model.Role, error) {
	var r model.Role
	err := s.db.QueryRowContext(ctx,
		"SELECT identity, role FROM roles WHERE repo = $1 AND identity = $2",
		repo, identity,
	).Scan(&r.Identity, &r.Role)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRoleNotFound
		}
		return nil, err
	}
	return &r, nil
}

// SetRole assigns or updates the role for an identity in a repo (upsert).
func (s *Store) SetRole(ctx context.Context, repo, identity string, role model.RoleType) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO roles (repo, identity, role) VALUES ($1, $2, $3)
		 ON CONFLICT (repo, identity) DO UPDATE SET role = $3`,
		repo, identity, string(role),
	)
	return err
}

// DeleteRole removes the role for an identity in a repo. Returns ErrRoleNotFound if not found.
func (s *Store) DeleteRole(ctx context.Context, repo, identity string) error {
	result, err := s.db.ExecContext(ctx,
		"DELETE FROM roles WHERE repo = $1 AND identity = $2",
		repo, identity,
	)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrRoleNotFound
	}
	return nil
}

// ListRoles returns all roles for a repo ordered by identity.
func (s *Store) ListRoles(ctx context.Context, repo string) ([]model.Role, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT identity, role FROM roles WHERE repo = $1 ORDER BY identity",
		repo,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var roles []model.Role
	for rows.Next() {
		var r model.Role
		if err := rows.Scan(&r.Identity, &r.Role); err != nil {
			return nil, err
		}
		roles = append(roles, r)
	}
	return roles, rows.Err()
}

// HasAdmin returns true if any admin role exists for the given repo.
func (s *Store) HasAdmin(ctx context.Context, repo string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM roles WHERE repo = $1 AND role = 'admin')",
		repo,
	).Scan(&exists)
	return exists, err
}


// PurgeRequest contains the parameters for a purge operation.
type PurgeRequest struct {
	Repo      string
	OlderThan time.Duration
	DryRun    bool
}

// PurgeResult contains the counts of rows affected (or that would be affected) by a purge.
type PurgeResult struct {
	BranchesPurged     int64
	FileCommitsDeleted int64
	CommitsDeleted     int64
	DocumentsDeleted   int64
	ReviewsDeleted     int64
	CheckRunsDeleted   int64
}

// Purge deletes file_commits, commits, orphaned documents, reviews, check_runs,
// and branch rows for merged/abandoned branches whose last activity is older than
// req.OlderThan. If req.DryRun is true, counts are returned without any rows being
// deleted. All deletes happen within a single transaction.
func (s *Store) Purge(ctx context.Context, req PurgeRequest) (*PurgeResult, error) {
	slog.Debug("purge started", "repo", req.Repo, "older_than", req.OlderThan, "dry_run", req.DryRun)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		slog.Error("tx begin failed", "op", "purge", "error", err)
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			slog.Error("tx rollback failed", "op", "purge", "error", rollbackErr)
		}
	}()

	// Verify repo exists.
	var exists bool
	if err := tx.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM repos WHERE name = $1)", req.Repo,
	).Scan(&exists); err != nil {
		return nil, fmt.Errorf("check repo: %w", err)
	}
	if !exists {
		return nil, ErrRepoNotFound
	}

	threshold := time.Now().Add(-req.OlderThan)

	// Find eligible branches: merged or abandoned, last activity older than threshold, not main.
	rows, err := tx.QueryContext(ctx, `
		SELECT b.name
		FROM branches b
		LEFT JOIN commits c ON c.repo = b.repo AND c.sequence = b.head_sequence
		WHERE b.repo = $1
		  AND b.status IN ('merged', 'abandoned')
		  AND b.name != 'main'
		  AND COALESCE(c.created_at, b.created_at) < $2
	`, req.Repo, threshold)
	if err != nil {
		return nil, fmt.Errorf("find eligible branches: %w", err)
	}
	var branches []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan branch: %w", err)
		}
		branches = append(branches, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate branches: %w", err)
	}

	// Nothing to purge — return early (defer will rollback the empty transaction).
	if len(branches) == 0 {
		return &PurgeResult{}, nil
	}

	var result PurgeResult
	result.BranchesPurged = int64(len(branches))

	// 1. Delete file_commits for the eligible branches.
	res, err := tx.ExecContext(ctx,
		`DELETE FROM file_commits WHERE repo = $1 AND branch = ANY($2)`,
		req.Repo, pq.Array(branches),
	)
	if err != nil {
		return nil, fmt.Errorf("delete file_commits: %w", err)
	}
	result.FileCommitsDeleted, _ = res.RowsAffected()

	// 2. Delete commits for those branches not referenced by any remaining file_commits.
	res, err = tx.ExecContext(ctx,
		`DELETE FROM commits
		 WHERE repo = $1 AND branch = ANY($2)
		   AND sequence NOT IN (
		       SELECT DISTINCT sequence FROM file_commits WHERE repo = $1
		   )`,
		req.Repo, pq.Array(branches),
	)
	if err != nil {
		return nil, fmt.Errorf("delete commits: %w", err)
	}
	result.CommitsDeleted, _ = res.RowsAffected()

	// 3. Delete reviews for the eligible branches.
	res, err = tx.ExecContext(ctx,
		`DELETE FROM reviews WHERE repo = $1 AND branch = ANY($2)`,
		req.Repo, pq.Array(branches),
	)
	if err != nil {
		return nil, fmt.Errorf("delete reviews: %w", err)
	}
	result.ReviewsDeleted, _ = res.RowsAffected()

	// 4. Delete check_runs for the eligible branches.
	res, err = tx.ExecContext(ctx,
		`DELETE FROM check_runs WHERE repo = $1 AND branch = ANY($2)`,
		req.Repo, pq.Array(branches),
	)
	if err != nil {
		return nil, fmt.Errorf("delete check_runs: %w", err)
	}
	result.CheckRunsDeleted, _ = res.RowsAffected()

	// 5. Delete orphaned documents (not referenced by any remaining file_commits in the repo).
	res, err = tx.ExecContext(ctx,
		`DELETE FROM documents
		 WHERE repo = $1
		   AND version_id NOT IN (
		       SELECT DISTINCT version_id FROM file_commits
		       WHERE repo = $1 AND version_id IS NOT NULL
		   )`,
		req.Repo,
	)
	if err != nil {
		return nil, fmt.Errorf("delete documents: %w", err)
	}
	result.DocumentsDeleted, _ = res.RowsAffected()

	// 6. Delete the branch rows themselves.
	_, err = tx.ExecContext(ctx,
		`DELETE FROM branches WHERE repo = $1 AND name = ANY($2)`,
		req.Repo, pq.Array(branches),
	)
	if err != nil {
		return nil, fmt.Errorf("delete branches: %w", err)
	}

	// For dry_run, return counts without committing (defer will rollback).
	if req.DryRun {
		return &result, nil
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}

	slog.Info("purge complete", "repo", req.Repo, "branches_purged", result.BranchesPurged, "docs_deleted", result.DocumentsDeleted)
	return &result, nil
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
