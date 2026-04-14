package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/dlorenc/docstore/internal/model"
)

// PGStore implements the server.Store interface using PostgreSQL.
type PGStore struct {
	db *sql.DB
}

// NewStore creates a new PGStore wrapping the given database connection.
func NewStore(db *sql.DB) *PGStore {
	return &PGStore{db: db}
}

// CreateBranch creates a new branch forked from main's current head.
// Uses a transaction with SELECT FOR UPDATE on main to get a consistent base_sequence.
func (s *PGStore) CreateBranch(ctx context.Context, name string) (*model.Branch, error) {
	if name == "main" {
		return nil, model.ErrProtectedBranch
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Check if branch already exists.
	var existing string
	err = tx.QueryRowContext(ctx,
		`SELECT name FROM branches WHERE name = $1`, name).Scan(&existing)
	if err == nil {
		return nil, model.ErrBranchExists
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("check branch: %w", err)
	}

	// Lock main and get its head_sequence as our base.
	var mainHead int64
	err = tx.QueryRowContext(ctx,
		`SELECT head_sequence FROM branches WHERE name = 'main' FOR UPDATE`).Scan(&mainHead)
	if err != nil {
		return nil, fmt.Errorf("lock main: %w", err)
	}

	// Insert the new branch.
	_, err = tx.ExecContext(ctx,
		`INSERT INTO branches (name, head_sequence, base_sequence, status) VALUES ($1, $2, $3, 'active')`,
		name, mainHead, mainHead)
	if err != nil {
		return nil, fmt.Errorf("insert branch: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	return &model.Branch{
		Name:         name,
		HeadSequence: mainHead,
		BaseSequence: mainHead,
		Status:       model.BranchStatusActive,
	}, nil
}

// DeleteBranch marks a branch as abandoned.
func (s *PGStore) DeleteBranch(ctx context.Context, name string) error {
	if name == "main" {
		return model.ErrProtectedBranch
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Lock and check the branch.
	var status string
	err = tx.QueryRowContext(ctx,
		`SELECT status FROM branches WHERE name = $1 FOR UPDATE`, name).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return model.ErrBranchNotFound
	}
	if err != nil {
		return fmt.Errorf("lock branch: %w", err)
	}
	if status != "active" {
		return model.ErrBranchNotActive
	}

	_, err = tx.ExecContext(ctx,
		`UPDATE branches SET status = 'abandoned' WHERE name = $1`, name)
	if err != nil {
		return fmt.Errorf("update branch: %w", err)
	}

	return tx.Commit()
}

// ListBranches returns all branches, optionally filtered by status.
func (s *PGStore) ListBranches(ctx context.Context, status string) ([]model.Branch, error) {
	var rows *sql.Rows
	var err error

	if status != "" {
		rows, err = s.db.QueryContext(ctx,
			`SELECT name, head_sequence, base_sequence, status FROM branches WHERE status = $1::branch_status ORDER BY name`,
			status)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT name, head_sequence, base_sequence, status FROM branches ORDER BY name`)
	}
	if err != nil {
		return nil, fmt.Errorf("query branches: %w", err)
	}
	defer rows.Close()

	var branches []model.Branch
	for rows.Next() {
		var b model.Branch
		if err := rows.Scan(&b.Name, &b.HeadSequence, &b.BaseSequence, &b.Status); err != nil {
			return nil, fmt.Errorf("scan branch: %w", err)
		}
		branches = append(branches, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration: %w", err)
	}

	// Return empty slice instead of nil for consistent JSON encoding.
	if branches == nil {
		branches = []model.Branch{}
	}
	return branches, nil
}

// changedFiles queries the latest version of each path changed on a branch since a given sequence.
// This implements the DISTINCT ON (path) ... ORDER BY path, sequence DESC pattern from DESIGN.md.
// Returns a map of path -> version_id (nil for deletes).
func changedFiles(ctx context.Context, tx *sql.Tx, branch string, sinceSequence int64) (map[string]*string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT DISTINCT ON (path) path, version_id
		 FROM file_commits
		 WHERE branch = $1 AND sequence > $2
		 ORDER BY path, sequence DESC`,
		branch, sinceSequence)
	if err != nil {
		return nil, fmt.Errorf("query changes for %s: %w", branch, err)
	}
	defer rows.Close()

	changes := make(map[string]*string)
	for rows.Next() {
		var path string
		var versionID *string
		if err := rows.Scan(&path, &versionID); err != nil {
			return nil, fmt.Errorf("scan change: %w", err)
		}
		changes[path] = versionID
	}
	return changes, rows.Err()
}

// derefVersionID safely dereferences a *string, returning "" for nil (deletes).
func derefVersionID(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// buildConflicts detects paths changed in both sets and returns ConflictEntry slice.
func buildConflicts(branchChanges, mainChanges map[string]*string) []model.ConflictEntry {
	var conflicts []model.ConflictEntry
	for path, branchVID := range branchChanges {
		if mainVID, ok := mainChanges[path]; ok {
			conflicts = append(conflicts, model.ConflictEntry{
				Path:            path,
				MainVersionID:   derefVersionID(mainVID),
				BranchVersionID: derefVersionID(branchVID),
			})
		}
	}
	return conflicts
}

// Merge merges a branch into main per the DESIGN.md algorithm:
// 1. Lock both branches (SELECT FOR UPDATE)
// 2. Find branch changes since base_sequence
// 3. Find main changes since base_sequence
// 4. Detect conflicts (overlapping paths)
// 5. If clean, insert file_commits rows on main with new sequence
func (s *PGStore) Merge(ctx context.Context, branchName string) (*model.MergeResponse, []model.ConflictEntry, error) {
	if branchName == "main" {
		return nil, nil, model.ErrProtectedBranch
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Step 1: Lock both branches. Lock main first for consistent ordering.
	var mainHead int64
	err = tx.QueryRowContext(ctx,
		`SELECT head_sequence FROM branches WHERE name = 'main' FOR UPDATE`).Scan(&mainHead)
	if err != nil {
		return nil, nil, fmt.Errorf("lock main: %w", err)
	}

	var baseSeq int64
	var branchStatus string
	err = tx.QueryRowContext(ctx,
		`SELECT base_sequence, status FROM branches WHERE name = $1 FOR UPDATE`,
		branchName).Scan(&baseSeq, &branchStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, model.ErrBranchNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("lock branch: %w", err)
	}
	if branchStatus != "active" {
		return nil, nil, model.ErrBranchNotActive
	}

	// Step 2: Find branch changes since base_sequence.
	branchChanges, err := changedFiles(ctx, tx, branchName, baseSeq)
	if err != nil {
		return nil, nil, err
	}

	// Step 3: Find main changes since base_sequence.
	mainChanges, err := changedFiles(ctx, tx, "main", baseSeq)
	if err != nil {
		return nil, nil, err
	}

	// Step 4: Conflict check — any path in both sets is a conflict.
	conflicts := buildConflicts(branchChanges, mainChanges)
	if len(conflicts) > 0 {
		return nil, conflicts, nil
	}

	// Step 5: Clean merge — allocate new sequence and insert file_commits on main.
	if len(branchChanges) == 0 {
		// Nothing to merge, but still mark the branch.
		_, err = tx.ExecContext(ctx,
			`UPDATE branches SET status = 'merged' WHERE name = $1`, branchName)
		if err != nil {
			return nil, nil, fmt.Errorf("mark merged: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, nil, fmt.Errorf("commit: %w", err)
		}
		return &model.MergeResponse{Sequence: mainHead}, nil, nil
	}

	var newSeq int64
	err = tx.QueryRowContext(ctx, `SELECT nextval('commit_sequence')`).Scan(&newSeq)
	if err != nil {
		return nil, nil, fmt.Errorf("allocate sequence: %w", err)
	}

	// Insert one file_commit row on main for each branch-changed path.
	for path, versionID := range branchChanges {
		commitID := model.NewUUID()
		_, err = tx.ExecContext(ctx,
			`INSERT INTO file_commits (commit_id, sequence, path, version_id, branch, message, author, created_at)
			 VALUES ($1, $2, $3, $4, 'main', $5, $6, now())`,
			commitID, newSeq, path, versionID,
			fmt.Sprintf("Merge branch '%s'", branchName), "system")
		if err != nil {
			return nil, nil, fmt.Errorf("insert merge commit for %s: %w", path, err)
		}
	}

	// Advance main's head.
	_, err = tx.ExecContext(ctx,
		`UPDATE branches SET head_sequence = $1 WHERE name = 'main'`, newSeq)
	if err != nil {
		return nil, nil, fmt.Errorf("advance main head: %w", err)
	}

	// Mark branch as merged.
	_, err = tx.ExecContext(ctx,
		`UPDATE branches SET status = 'merged' WHERE name = $1`, branchName)
	if err != nil {
		return nil, nil, fmt.Errorf("mark merged: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit: %w", err)
	}

	return &model.MergeResponse{Sequence: newSeq}, nil, nil
}

// Rebase replays branch commits onto main's current head per DESIGN.md:
// 1. Lock both branches
// 2. Find branch changes grouped by original sequence
// 3. Check for conflicts with main changes since base_sequence
// 4. If clean, replay each group with new sequence numbers
// 5. Update branch's base_sequence to main's current head
func (s *PGStore) Rebase(ctx context.Context, branchName string) (*model.RebaseResponse, []model.ConflictEntry, error) {
	if branchName == "main" {
		return nil, nil, model.ErrProtectedBranch
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Lock both branches.
	var mainHead int64
	err = tx.QueryRowContext(ctx,
		`SELECT head_sequence FROM branches WHERE name = 'main' FOR UPDATE`).Scan(&mainHead)
	if err != nil {
		return nil, nil, fmt.Errorf("lock main: %w", err)
	}

	var baseSeq int64
	var branchStatus string
	err = tx.QueryRowContext(ctx,
		`SELECT base_sequence, status FROM branches WHERE name = $1 FOR UPDATE`,
		branchName).Scan(&baseSeq, &branchStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, model.ErrBranchNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("lock branch: %w", err)
	}
	if branchStatus != "active" {
		return nil, nil, model.ErrBranchNotActive
	}

	// Find branch changes (latest version of each path) for conflict detection.
	branchChanges, err := changedFiles(ctx, tx, branchName, baseSeq)
	if err != nil {
		return nil, nil, err
	}

	// Find main changes since base_sequence for conflict detection.
	mainChanges, err := changedFiles(ctx, tx, "main", baseSeq)
	if err != nil {
		return nil, nil, err
	}

	// Conflict check.
	conflicts := buildConflicts(branchChanges, mainChanges)
	if len(conflicts) > 0 {
		return nil, conflicts, nil
	}

	// Get all branch commits grouped by original sequence for replay.
	rows, err := tx.QueryContext(ctx,
		`SELECT sequence, path, version_id, message, author
		 FROM file_commits
		 WHERE branch = $1 AND sequence > $2
		 ORDER BY sequence, path`,
		branchName, baseSeq)
	if err != nil {
		return nil, nil, fmt.Errorf("query branch commits: %w", err)
	}
	defer rows.Close()

	// Group commits by original sequence.
	type commitRow struct {
		path      string
		versionID *string
		message   string
		author    string
	}
	groups := make(map[int64][]commitRow)
	var seqOrder []int64
	seenSeq := make(map[int64]bool)

	for rows.Next() {
		var seq int64
		var cr commitRow
		if err := rows.Scan(&seq, &cr.path, &cr.versionID, &cr.message, &cr.author); err != nil {
			return nil, nil, fmt.Errorf("scan commit: %w", err)
		}
		if !seenSeq[seq] {
			seqOrder = append(seqOrder, seq)
			seenSeq[seq] = true
		}
		groups[seq] = append(groups[seq], cr)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("rows iteration: %w", err)
	}

	// Replay each group with a new sequence number.
	var newBranchHead int64 = mainHead

	for _, oldSeq := range seqOrder {
		var newSeq int64
		err = tx.QueryRowContext(ctx, `SELECT nextval('commit_sequence')`).Scan(&newSeq)
		if err != nil {
			return nil, nil, fmt.Errorf("allocate sequence: %w", err)
		}
		newBranchHead = newSeq

		for _, cr := range groups[oldSeq] {
			commitID := model.NewUUID()
			_, err = tx.ExecContext(ctx,
				`INSERT INTO file_commits (commit_id, sequence, path, version_id, branch, message, author, created_at)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, now())`,
				commitID, newSeq, cr.path, cr.versionID, branchName, cr.message, cr.author)
			if err != nil {
				return nil, nil, fmt.Errorf("insert rebased commit: %w", err)
			}
		}
	}

	// Update branch: base_sequence = main's head, head_sequence = last replayed sequence.
	_, err = tx.ExecContext(ctx,
		`UPDATE branches SET base_sequence = $1, head_sequence = $2 WHERE name = $3`,
		mainHead, newBranchHead, branchName)
	if err != nil {
		return nil, nil, fmt.Errorf("update branch: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit: %w", err)
	}

	return &model.RebaseResponse{
		NewBaseSequence: mainHead,
		NewHeadSequence: newBranchHead,
	}, nil, nil
}

// Diff computes the diff between a branch and main since base_sequence.
func (s *PGStore) Diff(ctx context.Context, branchName string) (*model.DiffResponse, error) {
	// Get branch info.
	var baseSeq int64
	var status string
	err := s.db.QueryRowContext(ctx,
		`SELECT base_sequence, status FROM branches WHERE name = $1`, branchName).Scan(&baseSeq, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, model.ErrBranchNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get branch: %w", err)
	}

	// Branch changes since base_sequence.
	branchRows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT ON (path) path, version_id
		 FROM file_commits
		 WHERE branch = $1 AND sequence > $2
		 ORDER BY path, sequence DESC`,
		branchName, baseSeq)
	if err != nil {
		return nil, fmt.Errorf("query branch changes: %w", err)
	}
	defer branchRows.Close()

	branchMap := make(map[string]*string)
	var changed []model.DiffEntry
	for branchRows.Next() {
		var de model.DiffEntry
		if err := branchRows.Scan(&de.Path, &de.VersionID); err != nil {
			return nil, fmt.Errorf("scan branch change: %w", err)
		}
		changed = append(changed, de)
		branchMap[de.Path] = de.VersionID
	}
	if err := branchRows.Err(); err != nil {
		return nil, fmt.Errorf("branch rows: %w", err)
	}

	// Main changes since base_sequence for conflict detection.
	mainRows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT ON (path) path, version_id
		 FROM file_commits
		 WHERE branch = 'main' AND sequence > $1
		 ORDER BY path, sequence DESC`,
		baseSeq)
	if err != nil {
		return nil, fmt.Errorf("query main changes: %w", err)
	}
	defer mainRows.Close()

	var conflicts []model.ConflictEntry
	for mainRows.Next() {
		var path string
		var mainVID *string
		if err := mainRows.Scan(&path, &mainVID); err != nil {
			return nil, fmt.Errorf("scan main change: %w", err)
		}
		if branchVID, ok := branchMap[path]; ok {
			conflicts = append(conflicts, model.ConflictEntry{
				Path:            path,
				MainVersionID:   derefVersionID(mainVID),
				BranchVersionID: derefVersionID(branchVID),
			})
		}
	}
	if err := mainRows.Err(); err != nil {
		return nil, fmt.Errorf("main rows: %w", err)
	}

	// Ensure non-nil slices for JSON.
	if changed == nil {
		changed = []model.DiffEntry{}
	}
	if conflicts == nil {
		conflicts = []model.ConflictEntry{}
	}

	return &model.DiffResponse{
		Changed:   changed,
		Conflicts: conflicts,
	}, nil
}
