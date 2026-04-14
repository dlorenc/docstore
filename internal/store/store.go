package store

import (
	"context"
	"database/sql"
	"time"
)

// Store provides read queries against the docstore database.
type Store struct {
	db *sql.DB
}

// New creates a Store backed by the given database connection.
func New(db *sql.DB) *Store {
	return &Store{db: db}
}

// TreeEntry is a single file in a materialized tree.
type TreeEntry struct {
	Path        string `json:"path"`
	VersionID   string `json:"version_id"`
	ContentHash string `json:"content_hash"`
}

// FileContent is the content of a file at a point in time.
type FileContent struct {
	Path        string `json:"path"`
	VersionID   string `json:"version_id"`
	ContentHash string `json:"content_hash"`
	Content     []byte `json:"content"`
}

// FileHistoryEntry is one change to a file.
type FileHistoryEntry struct {
	Sequence  int64     `json:"sequence"`
	VersionID *string   `json:"version_id"`
	Message   string    `json:"message"`
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"created_at"`
}

// CommitDetail describes a single atomic commit.
type CommitDetail struct {
	Sequence  int64        `json:"sequence"`
	Branch    string       `json:"branch"`
	Message   string       `json:"message"`
	Author    string       `json:"author"`
	CreatedAt time.Time    `json:"created_at"`
	Files     []CommitFile `json:"files"`
}

// CommitFile is one file changed in a commit.
type CommitFile struct {
	CommitID  string  `json:"commit_id"`
	Path      string  `json:"path"`
	VersionID *string `json:"version_id"`
}

const defaultLimit = 100

// MaterializeTree returns the current file tree for a branch, optionally at a
// specific sequence. Pagination uses afterPath as the cursor.
func (s *Store) MaterializeTree(ctx context.Context, branch string, atSequence *int64, limit int, afterPath string) ([]TreeEntry, error) {
	if limit <= 0 {
		limit = defaultLimit
	}

	// The CTE computes the latest version of each path by combining:
	//   - commits on the requested branch
	//   - commits on main up to the branch's base_sequence (inherited files)
	// For main itself, base_sequence=0 so the second arm matches nothing.
	const q = `
WITH branch_info AS (
    SELECT base_sequence FROM branches WHERE name = $1
),
latest AS (
    SELECT DISTINCT ON (fc.path)
        fc.path, fc.version_id
    FROM file_commits fc
    CROSS JOIN branch_info bi
    WHERE (
        fc.branch = $1
        OR (fc.branch = 'main' AND fc.sequence <= bi.base_sequence)
    )
    AND ($2::bigint IS NULL OR fc.sequence <= $2)
    ORDER BY fc.path, fc.sequence DESC
)
SELECT l.path, l.version_id::text, d.content_hash
FROM latest l
JOIN documents d ON d.version_id = l.version_id
WHERE l.version_id IS NOT NULL
  AND ($3::text = '' OR l.path > $3)
ORDER BY l.path
LIMIT $4`

	var at interface{}
	if atSequence != nil {
		at = *atSequence
	}

	rows, err := s.db.QueryContext(ctx, q, branch, at, afterPath, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []TreeEntry
	for rows.Next() {
		var e TreeEntry
		if err := rows.Scan(&e.Path, &e.VersionID, &e.ContentHash); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// GetFile returns a file's content for a branch, optionally at a specific sequence.
// Returns nil, nil if the file does not exist or was deleted.
func (s *Store) GetFile(ctx context.Context, branch, path string, atSequence *int64) (*FileContent, error) {
	const q = `
WITH branch_info AS (
    SELECT base_sequence FROM branches WHERE name = $1
)
SELECT fc.version_id, d.content_hash, d.content
FROM file_commits fc
CROSS JOIN branch_info bi
LEFT JOIN documents d ON d.version_id = fc.version_id
WHERE (
    fc.branch = $1
    OR (fc.branch = 'main' AND fc.sequence <= bi.base_sequence)
)
AND fc.path = $2
AND ($3::bigint IS NULL OR fc.sequence <= $3)
ORDER BY fc.sequence DESC
LIMIT 1`

	var at interface{}
	if atSequence != nil {
		at = *atSequence
	}

	var versionID sql.NullString
	var contentHash sql.NullString
	var content []byte

	err := s.db.QueryRowContext(ctx, q, branch, path, at).Scan(&versionID, &contentHash, &content)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	// version_id NULL means the file was deleted at this point.
	if !versionID.Valid {
		return nil, nil
	}

	return &FileContent{
		Path:        path,
		VersionID:   versionID.String,
		ContentHash: contentHash.String,
		Content:     content,
	}, nil
}

// GetFileHistory returns the change history for a file on a branch.
// Pagination: afterSequence is the cursor (exclusive); pass nil for the first page.
func (s *Store) GetFileHistory(ctx context.Context, branch, path string, limit int, afterSequence *int64) ([]FileHistoryEntry, error) {
	if limit <= 0 {
		limit = defaultLimit
	}

	const q = `
WITH branch_info AS (
    SELECT base_sequence FROM branches WHERE name = $1
)
SELECT fc.sequence, fc.version_id::text, fc.message, fc.author, fc.created_at
FROM file_commits fc
CROSS JOIN branch_info bi
WHERE (
    fc.branch = $1
    OR (fc.branch = 'main' AND fc.sequence <= bi.base_sequence)
)
AND fc.path = $2
AND ($3::bigint IS NULL OR fc.sequence < $3)
ORDER BY fc.sequence DESC
LIMIT $4`

	var after interface{}
	if afterSequence != nil {
		after = *afterSequence
	}

	rows, err := s.db.QueryContext(ctx, q, branch, path, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []FileHistoryEntry
	for rows.Next() {
		var e FileHistoryEntry
		var vid sql.NullString
		if err := rows.Scan(&e.Sequence, &vid, &e.Message, &e.Author, &e.CreatedAt); err != nil {
			return nil, err
		}
		if vid.Valid {
			e.VersionID = &vid.String
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// BranchInfo describes a branch for listing.
type BranchInfo struct {
	Name         string `json:"name"`
	HeadSequence int64  `json:"head_sequence"`
	BaseSequence int64  `json:"base_sequence"`
	Status       string `json:"status"`
}

// DiffEntry represents a file changed on a branch relative to its base.
type DiffEntry struct {
	Path      string  `json:"path"`
	VersionID *string `json:"version_id"`
}

// ConflictEntry represents a file changed on both main and the branch.
type ConflictEntry struct {
	Path            string `json:"path"`
	MainVersionID   string `json:"main_version_id"`
	BranchVersionID string `json:"branch_version_id"`
}

// DiffResult contains the diff between a branch and main.
type DiffResult struct {
	Changed   []DiffEntry     `json:"changed"`
	Conflicts []ConflictEntry `json:"conflicts,omitempty"`
}

// ListBranches returns all branches, optionally filtered by status.
func (s *Store) ListBranches(ctx context.Context, statusFilter string) ([]BranchInfo, error) {
	var rows *sql.Rows
	var err error

	if statusFilter != "" {
		rows, err = s.db.QueryContext(ctx,
			"SELECT name, head_sequence, base_sequence, status FROM branches WHERE status = $1::branch_status ORDER BY name",
			statusFilter,
		)
	} else {
		rows, err = s.db.QueryContext(ctx,
			"SELECT name, head_sequence, base_sequence, status FROM branches ORDER BY name",
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var branches []BranchInfo
	for rows.Next() {
		var b BranchInfo
		if err := rows.Scan(&b.Name, &b.HeadSequence, &b.BaseSequence, &b.Status); err != nil {
			return nil, err
		}
		branches = append(branches, b)
	}
	return branches, rows.Err()
}

// GetDiff returns the files changed on a branch relative to its base_sequence,
// plus any conflicting paths that were also changed on main.
func (s *Store) GetDiff(ctx context.Context, branch string) (*DiffResult, error) {
	// Get the branch's base_sequence.
	var baseSeq int64
	var status string
	err := s.db.QueryRowContext(ctx,
		"SELECT base_sequence, status FROM branches WHERE name = $1",
		branch,
	).Scan(&baseSeq, &status)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil // branch not found
		}
		return nil, err
	}

	// Branch changes: latest version of each path changed on branch since base.
	branchChanges, err := s.latestChanges(ctx, branch, baseSeq)
	if err != nil {
		return nil, err
	}

	// Main changes: latest version of each path changed on main since base.
	mainChanges, err := s.latestChanges(ctx, "main", baseSeq)
	if err != nil {
		return nil, err
	}

	result := &DiffResult{}

	for path, vid := range branchChanges {
		result.Changed = append(result.Changed, DiffEntry{
			Path:      path,
			VersionID: vid,
		})

		// Check for conflicts.
		if mainVID, ok := mainChanges[path]; ok {
			mainV := ""
			if mainVID != nil {
				mainV = *mainVID
			}
			branchV := ""
			if vid != nil {
				branchV = *vid
			}
			result.Conflicts = append(result.Conflicts, ConflictEntry{
				Path:            path,
				MainVersionID:   mainV,
				BranchVersionID: branchV,
			})
		}
	}

	return result, nil
}

// latestChanges returns the latest version of each path changed on a branch
// since the given base sequence.
func (s *Store) latestChanges(ctx context.Context, branch string, baseSeq int64) (map[string]*string, error) {
	const q = `
SELECT DISTINCT ON (path) path, version_id::text
FROM file_commits
WHERE branch = $1 AND sequence > $2
ORDER BY path, sequence DESC`

	rows, err := s.db.QueryContext(ctx, q, branch, baseSeq)
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

// GetCommit returns all file changes in a single atomic commit (by sequence).
// Returns nil, nil if no commit exists with that sequence.
func (s *Store) GetCommit(ctx context.Context, sequence int64) (*CommitDetail, error) {
	const q = `
SELECT fc.commit_id::text, fc.path, fc.version_id::text, fc.branch, fc.message, fc.author, fc.created_at
FROM file_commits fc
WHERE fc.sequence = $1
ORDER BY fc.path`

	rows, err := s.db.QueryContext(ctx, q, sequence)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var detail *CommitDetail
	for rows.Next() {
		var cid string
		var path string
		var vid sql.NullString
		var branch, message, author string
		var createdAt time.Time

		if err := rows.Scan(&cid, &path, &vid, &branch, &message, &author, &createdAt); err != nil {
			return nil, err
		}

		if detail == nil {
			detail = &CommitDetail{
				Sequence:  sequence,
				Branch:    branch,
				Message:   message,
				Author:    author,
				CreatedAt: createdAt,
			}
		}

		cf := CommitFile{CommitID: cid, Path: path}
		if vid.Valid {
			v := vid.String
			cf.VersionID = &v
		}
		detail.Files = append(detail.Files, cf)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return detail, nil
}
