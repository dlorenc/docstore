package store

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/dlorenc/docstore/api"
	"github.com/dlorenc/docstore/internal/blob"
)

// Store provides read queries against the docstore database.
type Store struct {
	db        *sql.DB
	blobStore blob.BlobStore
}

// New creates a Store backed by the given database connection.
func New(db *sql.DB) *Store {
	return &Store{db: db}
}

// SetBlobStore configures the external blob store used when fetching
// files whose content is stored outside Postgres.
func (s *Store) SetBlobStore(bs blob.BlobStore) {
	s.blobStore = bs
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
	ContentType string `json:"content_type,omitempty"`
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

// MaterializeTree returns the current file tree for a branch in a repo,
// optionally at a specific sequence. Pagination uses afterPath as the cursor.
func (s *Store) MaterializeTree(ctx context.Context, repo, branch string, atSequence *int64, limit int, afterPath string) ([]TreeEntry, error) {
	if limit <= 0 {
		limit = defaultLimit
	}

	// The CTE computes the latest version of each path by combining:
	//   - commits on the requested branch (within repo)
	//   - commits on main up to the branch's base_sequence (inherited files)
	// For main itself, base_sequence=0 so the second arm matches nothing.
	const q = `
WITH branch_info AS (
    SELECT base_sequence FROM branches WHERE repo = $1 AND name = $2
),
latest AS (
    SELECT DISTINCT ON (fc.path)
        fc.path, fc.version_id
    FROM file_commits fc
    CROSS JOIN branch_info bi
    WHERE fc.repo = $1
    AND (
        fc.branch = $2
        OR (fc.branch = 'main' AND fc.sequence <= bi.base_sequence)
    )
    AND ($3::bigint IS NULL OR fc.sequence <= $3)
    ORDER BY fc.path, fc.sequence DESC
)
SELECT l.path, l.version_id::text, d.content_hash
FROM latest l
JOIN documents d ON d.version_id = l.version_id AND d.repo = $1
WHERE l.version_id IS NOT NULL
  AND ($4::text = '' OR l.path > $4)
ORDER BY l.path
LIMIT $5`

	var at interface{}
	if atSequence != nil {
		at = *atSequence
	}

	rows, err := s.db.QueryContext(ctx, q, repo, branch, at, afterPath, limit)
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

// GetFile returns a file's content for a branch in a repo, optionally at a specific sequence.
// Returns nil, nil if the file does not exist or was deleted.
func (s *Store) GetFile(ctx context.Context, repo, branch, path string, atSequence *int64) (*FileContent, error) {
	const q = `
WITH branch_info AS (
    SELECT base_sequence FROM branches WHERE repo = $1 AND name = $2
)
SELECT fc.version_id, d.content_hash, d.content, d.content_type, d.blob_key
FROM file_commits fc
CROSS JOIN branch_info bi
LEFT JOIN documents d ON d.version_id = fc.version_id AND d.repo = $1
WHERE fc.repo = $1
AND (
    fc.branch = $2
    OR (fc.branch = 'main' AND fc.sequence <= bi.base_sequence)
)
AND fc.path = $3
AND ($4::bigint IS NULL OR fc.sequence <= $4)
ORDER BY fc.sequence DESC
LIMIT 1`

	var at interface{}
	if atSequence != nil {
		at = *atSequence
	}

	var versionID sql.NullString
	var contentHash sql.NullString
	var content []byte
	var contentType sql.NullString
	var blobKey sql.NullString

	err := s.db.QueryRowContext(ctx, q, repo, branch, path, at).Scan(&versionID, &contentHash, &content, &contentType, &blobKey)
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

	// If the document is stored in an external blob store, fetch it now.
	if blobKey.Valid && blobKey.String != "" {
		if s.blobStore == nil {
			return nil, fmt.Errorf("document %s requires blob store but none is configured", versionID.String)
		}
		rc, err := s.blobStore.Get(ctx, blobKey.String)
		if err != nil {
			return nil, fmt.Errorf("fetch blob %s: %w", blobKey.String, err)
		}
		defer rc.Close()
		content, err = io.ReadAll(rc)
		if err != nil {
			return nil, fmt.Errorf("read blob %s: %w", blobKey.String, err)
		}
	}

	return &FileContent{
		Path:        path,
		VersionID:   versionID.String,
		ContentHash: contentHash.String,
		Content:     content,
		ContentType: contentType.String,
	}, nil
}

// GetFileHistory returns the change history for a file on a branch in a repo.
// Pagination: afterSequence is the cursor (exclusive); pass nil for the first page.
func (s *Store) GetFileHistory(ctx context.Context, repo, branch, path string, limit int, afterSequence *int64) ([]FileHistoryEntry, error) {
	if limit <= 0 {
		limit = defaultLimit
	}

	const q = `
WITH branch_info AS (
    SELECT base_sequence FROM branches WHERE repo = $1 AND name = $2
)
SELECT fc.sequence, fc.version_id::text, c.message, c.author, c.created_at
FROM file_commits fc
CROSS JOIN branch_info bi
JOIN commits c ON c.sequence = fc.sequence AND c.repo = $1
WHERE fc.repo = $1
AND (
    fc.branch = $2
    OR (fc.branch = 'main' AND fc.sequence <= bi.base_sequence)
)
AND fc.path = $3
AND ($4::bigint IS NULL OR fc.sequence < $4)
ORDER BY fc.sequence DESC
LIMIT $5`

	var after interface{}
	if afterSequence != nil {
		after = *afterSequence
	}

	rows, err := s.db.QueryContext(ctx, q, repo, branch, path, after, limit)
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
	Draft        bool   `json:"draft"`
	AutoMerge    bool   `json:"auto_merge"`
}

// DiffEntry represents a file changed on a branch relative to its base.
type DiffEntry struct {
	Path      string  `json:"path"`
	VersionID *string `json:"version_id"`
	Binary    bool    `json:"binary,omitempty"`
}

// ConflictEntry represents a file changed on both main and the branch.
type ConflictEntry struct {
	Path            string `json:"path"`
	MainVersionID   string `json:"main_version_id"`
	BranchVersionID string `json:"branch_version_id"`
}

// DiffResult contains the diff between a branch and main.
type DiffResult struct {
	BranchChanges []DiffEntry     `json:"branch_changes"`
	MainChanges   []DiffEntry     `json:"main_changes"`
	Conflicts     []ConflictEntry `json:"conflicts,omitempty"`
}

// GetBranch returns the BranchInfo for a single named branch in a repo.
// Returns nil, nil if the branch does not exist.
func (s *Store) GetBranch(ctx context.Context, repo, branch string) (*BranchInfo, error) {
	var b BranchInfo
	err := s.db.QueryRowContext(ctx,
		"SELECT name, head_sequence, base_sequence, status, draft, auto_merge FROM branches WHERE repo = $1 AND name = $2",
		repo, branch,
	).Scan(&b.Name, &b.HeadSequence, &b.BaseSequence, &b.Status, &b.Draft, &b.AutoMerge)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &b, nil
}

// ListBranches returns branches in a repo, optionally filtered by status.
// includeDraft=true includes draft branches; onlyDraft=true returns only draft branches.
// By default (both false), draft branches are excluded.
func (s *Store) ListBranches(ctx context.Context, repo, statusFilter string, includeDraft, onlyDraft bool) ([]BranchInfo, error) {
	q := "SELECT name, head_sequence, base_sequence, status, draft, auto_merge FROM branches WHERE repo = $1"
	args := []interface{}{repo}

	if statusFilter != "" {
		args = append(args, statusFilter)
		q += " AND status = $" + strconv.Itoa(len(args)) + "::branch_status"
	}

	if onlyDraft {
		q += " AND draft = true"
	} else if !includeDraft {
		q += " AND draft = false"
	}
	q += " ORDER BY name"

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var branches []BranchInfo
	for rows.Next() {
		var b BranchInfo
		if err := rows.Scan(&b.Name, &b.HeadSequence, &b.BaseSequence, &b.Status, &b.Draft, &b.AutoMerge); err != nil {
			return nil, err
		}
		branches = append(branches, b)
	}
	return branches, rows.Err()
}

// GetDiff returns the files changed on a branch relative to its base_sequence,
// plus any conflicting paths that were also changed on main.
func (s *Store) GetDiff(ctx context.Context, repo, branch string) (*DiffResult, error) {
	// Get the branch's base_sequence.
	var baseSeq int64
	var status string
	err := s.db.QueryRowContext(ctx,
		"SELECT base_sequence, status FROM branches WHERE repo = $1 AND name = $2",
		repo, branch,
	).Scan(&baseSeq, &status)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil // branch not found
		}
		return nil, err
	}

	// Branch changes: latest version of each path changed on branch since base.
	branchChanges, err := s.latestChanges(ctx, repo, branch, baseSeq)
	if err != nil {
		return nil, err
	}

	// Main changes: latest version of each path changed on main since base.
	mainChanges, err := s.latestChanges(ctx, repo, "main", baseSeq)
	if err != nil {
		return nil, err
	}

	result := &DiffResult{}

	for path, change := range branchChanges {
		result.BranchChanges = append(result.BranchChanges, DiffEntry{
			Path:      path,
			VersionID: change.versionID,
			Binary:    isBinaryContentType(change.contentType),
		})

		// Check for conflicts.
		if mainChange, ok := mainChanges[path]; ok {
			mainV := ""
			if mainChange.versionID != nil {
				mainV = *mainChange.versionID
			}
			branchV := ""
			if change.versionID != nil {
				branchV = *change.versionID
			}
			result.Conflicts = append(result.Conflicts, ConflictEntry{
				Path:            path,
				MainVersionID:   mainV,
				BranchVersionID: branchV,
			})
		}
	}

	for path, change := range mainChanges {
		result.MainChanges = append(result.MainChanges, DiffEntry{
			Path:      path,
			VersionID: change.versionID,
			Binary:    isBinaryContentType(change.contentType),
		})
	}

	return result, nil
}

// diffChangeInfo holds a version ID and content_type for a single file change.
type diffChangeInfo struct {
	versionID   *string
	contentType string
}

// latestChanges returns the latest version and content_type of each path changed
// on a branch in a repo since the given base sequence.
func (s *Store) latestChanges(ctx context.Context, repo, branch string, baseSeq int64) (map[string]diffChangeInfo, error) {
	const q = `
SELECT DISTINCT ON (fc.path) fc.path, fc.version_id::text, d.content_type
FROM file_commits fc
LEFT JOIN documents d ON d.version_id = fc.version_id AND d.repo = $1
WHERE fc.repo = $1 AND fc.branch = $2 AND fc.sequence > $3
ORDER BY fc.path, fc.sequence DESC`

	rows, err := s.db.QueryContext(ctx, q, repo, branch, baseSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	changes := make(map[string]diffChangeInfo)
	for rows.Next() {
		var path string
		var vid sql.NullString
		var ct sql.NullString
		if err := rows.Scan(&path, &vid, &ct); err != nil {
			return nil, err
		}
		var versionID *string
		if vid.Valid {
			v := vid.String
			versionID = &v
		}
		changes[path] = diffChangeInfo{versionID: versionID, contentType: ct.String}
	}
	return changes, rows.Err()
}

// isBinaryContentType reports whether a content_type indicates binary content.
func isBinaryContentType(ct string) bool {
	return ct != "" && !strings.HasPrefix(ct, "text/")
}

// ChainEntry is one commit in the hash chain response.
// Aliased to api.ChainEntry so the public SDK and server agree on shape.
type ChainEntry = api.ChainEntry

// ChainFile is one file change within a ChainEntry.
type ChainFile = api.ChainFile

// GetChain returns commit metadata for sequences in [from, to] inclusive,
// ordered by sequence ascending. Each entry includes the commit_hash (if set)
// and the files changed in that commit with their content hashes.
func (s *Store) GetChain(ctx context.Context, repo string, from, to int64) ([]ChainEntry, error) {
	const q = `
SELECT c.sequence, c.branch, c.author, c.message, c.created_at, c.commit_hash,
       fc.path, d.content_hash
FROM commits c
LEFT JOIN file_commits fc ON fc.sequence = c.sequence AND fc.repo = c.repo
LEFT JOIN documents d ON d.version_id = fc.version_id AND d.repo = c.repo
WHERE c.repo = $1 AND c.sequence >= $2 AND c.sequence <= $3
ORDER BY c.sequence ASC, fc.path ASC`

	rows, err := s.db.QueryContext(ctx, q, repo, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []ChainEntry
	var cur *ChainEntry
	for rows.Next() {
		var seq int64
		var branch, author, message string
		var createdAt time.Time
		var commitHash sql.NullString
		var path sql.NullString
		var contentHash sql.NullString

		if err := rows.Scan(&seq, &branch, &author, &message, &createdAt, &commitHash, &path, &contentHash); err != nil {
			return nil, err
		}

		if cur == nil || cur.Sequence != seq {
			if cur != nil {
				entries = append(entries, *cur)
			}
			var chPtr *string
			if commitHash.Valid {
				v := commitHash.String
				chPtr = &v
			}
			cur = &ChainEntry{
				Sequence:   seq,
				Branch:     branch,
				Author:     author,
				Message:    message,
				CreatedAt:  createdAt,
				CommitHash: chPtr,
				Files:      []ChainFile{},
			}
		}

		if path.Valid {
			cur.Files = append(cur.Files, ChainFile{
				Path:        path.String,
				ContentHash: contentHash.String, // empty string for deletes (NULL content_hash)
			})
		}
	}
	if cur != nil {
		entries = append(entries, *cur)
	}
	return entries, rows.Err()
}

// GetCommit returns all file changes in a single atomic commit (by sequence) within a repo.
// Returns nil, nil if no commit exists with that sequence in the repo.
func (s *Store) GetCommit(ctx context.Context, repo string, sequence int64) (*CommitDetail, error) {
	const q = `
SELECT fc.commit_id::text, fc.path, fc.version_id::text, fc.branch, c.message, c.author, c.created_at
FROM file_commits fc
JOIN commits c ON c.sequence = fc.sequence AND c.repo = $1
WHERE fc.repo = $1 AND fc.sequence = $2
ORDER BY fc.path`

	rows, err := s.db.QueryContext(ctx, q, repo, sequence)
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
