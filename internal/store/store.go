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
