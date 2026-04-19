package db

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/dlorenc/docstore/internal/blob"
	"github.com/dlorenc/docstore/internal/model"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// genesisHash is the all-zeros hash used as the previous hash for the first commit.
const genesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// chainFile holds a path and content_hash for commit hash computation.
type chainFile struct {
	path        string
	contentHash string
}

// computeCommitHash computes the SHA256 chain hash for a commit.
// prevHash is the hex-encoded hash of the previous commit (or genesisHash for the first commit).
// files are sorted by path internally, so caller order does not matter.
func computeCommitHash(prevHash string, seq int64, repo, branch, author, message string, createdAt time.Time, files []chainFile) string {
	// Sort a copy so the hash is always canonical regardless of input order.
	sorted := make([]chainFile, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].path < sorted[j].path })

	h := sha256.New()
	h.Write([]byte(prevHash + "\n"))
	h.Write([]byte(strconv.FormatInt(seq, 10) + "\n"))
	h.Write([]byte(repo + "\n"))
	h.Write([]byte(branch + "\n"))
	h.Write([]byte(author + "\n"))
	h.Write([]byte(message + "\n"))
	h.Write([]byte(createdAt.UTC().Format(time.RFC3339Nano) + "\n"))
	for _, f := range sorted {
		h.Write([]byte(f.path + ":" + f.contentHash + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// fetchPrevCommitHash fetches the commit_hash of the most recent commit before seq
// on the same branch. Returns genesisHash if no previous commit exists on this branch
// or if the previous commit has a NULL commit_hash (pre-feature commit).
// Using per-branch lookup ensures same-branch commits form a coherent linear chain;
// different branches have independent chains and are not subject to cross-branch races.
func fetchPrevCommitHash(ctx context.Context, tx *sql.Tx, repo, branch string, seq int64) (string, error) {
	var prevNull sql.NullString
	err := tx.QueryRowContext(ctx,
		`SELECT commit_hash FROM commits
		 WHERE repo = $1 AND branch = $2
		   AND sequence = (SELECT MAX(sequence) FROM commits WHERE repo = $1 AND branch = $2 AND sequence < $3)`,
		repo, branch, seq,
	).Scan(&prevNull)
	if errors.Is(err, sql.ErrNoRows) {
		return genesisHash, nil
	}
	if err != nil {
		return "", fmt.Errorf("fetch prev commit hash: %w", err)
	}
	if !prevNull.Valid || prevNull.String == "" {
		return genesisHash, nil
	}
	return prevNull.String, nil
}

// fetchVersionContentHash fetches the content_hash for a version_id from documents.
// Returns empty string for nil version_id (deleted files).
func fetchVersionContentHash(ctx context.Context, tx *sql.Tx, versionID *string) (string, error) {
	if versionID == nil {
		return "", nil
	}
	var hash string
	err := tx.QueryRowContext(ctx,
		"SELECT content_hash FROM documents WHERE version_id = $1",
		*versionID,
	).Scan(&hash)
	if err != nil {
		return "", fmt.Errorf("fetch content hash for version %s: %w", *versionID, err)
	}
	return hash, nil
}

// updateCommitHash stores the given hash on the commits row identified by repo+seq.
func updateCommitHash(ctx context.Context, tx *sql.Tx, repo string, seq int64, hash string) error {
	_, err := tx.ExecContext(ctx,
		"UPDATE commits SET commit_hash = $1 WHERE repo = $2 AND sequence = $3",
		hash, repo, seq,
	)
	return err
}

var (
	ErrBranchNotFound         = errors.New("branch not found")
	ErrBranchNotActive        = errors.New("branch is not active")
	ErrBranchExists           = errors.New("branch already exists")
	ErrMergeConflict          = errors.New("merge conflict")
	ErrRebaseConflict         = errors.New("rebase conflict")
	ErrRepoNotFound           = errors.New("repo not found")
	ErrRepoExists             = errors.New("repo already exists")
	ErrOrgNotFound            = errors.New("org not found")
	ErrOrgExists              = errors.New("org already exists")
	ErrOrgHasRepos            = errors.New("org has repos")
	ErrRoleNotFound           = errors.New("role not found")
	ErrSelfApproval           = errors.New("reviewer cannot approve their own commits")
	ErrOrgMemberNotFound      = errors.New("org member not found")
	ErrInviteNotFound         = errors.New("invite not found")
	ErrInviteExpired          = errors.New("invite expired")
	ErrInviteAlreadyAccepted  = errors.New("invite already accepted")
	ErrEmailMismatch          = errors.New("identity does not match invite email")
	ErrReleaseNotFound        = errors.New("release not found")
	ErrReleaseExists          = errors.New("release already exists")
	ErrInvalidCursor          = errors.New("invalid pagination cursor")
	ErrBranchDraft            = errors.New("branch is in draft state")
	ErrSubscriptionNotFound   = errors.New("subscription not found")
	ErrCommentNotFound        = errors.New("comment not found")
	ErrCIJobNotFound          = errors.New("ci job not found")
	ErrProposalNotFound       = errors.New("proposal not found")
	ErrProposalExists         = errors.New("branch already has an open proposal")
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
	db            *sql.DB
	blobStore     blob.BlobStore
	blobThreshold int64
}

// NewStore returns a Store backed by the given database connection.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// SetBlobStore configures external blob storage. Files larger than
// thresholdBytes are stored in bs instead of Postgres BYTEA.
// A threshold of 0 disables external blob storage.
func (s *Store) SetBlobStore(bs blob.BlobStore, thresholdBytes int64) {
	s.blobStore = bs
	s.blobThreshold = thresholdBytes
}

// CreateOrg creates a new org. Returns ErrOrgExists if the name is taken.
func (s *Store) CreateOrg(ctx context.Context, name, createdBy string) (*model.Org, error) {
	if createdBy == "" {
		createdBy = "system"
	}
	var o model.Org
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO orgs (name, created_by) VALUES ($1, $2)
		 RETURNING name, created_at, created_by`,
		name, createdBy,
	).Scan(&o.Name, &o.CreatedAt, &o.CreatedBy)
	if err != nil {
		if isDuplicateKeyError(err) {
			return nil, ErrOrgExists
		}
		return nil, fmt.Errorf("insert org: %w", err)
	}
	return &o, nil
}

// GetOrg returns an org by name, or ErrOrgNotFound.
func (s *Store) GetOrg(ctx context.Context, name string) (*model.Org, error) {
	var o model.Org
	err := s.db.QueryRowContext(ctx,
		"SELECT name, created_at, created_by FROM orgs WHERE name = $1",
		name,
	).Scan(&o.Name, &o.CreatedAt, &o.CreatedBy)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrOrgNotFound
		}
		return nil, err
	}
	return &o, nil
}

// ListOrgs returns all orgs ordered by name.
func (s *Store) ListOrgs(ctx context.Context) ([]model.Org, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT name, created_at, created_by FROM orgs ORDER BY name",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var orgs []model.Org
	for rows.Next() {
		var o model.Org
		if err := rows.Scan(&o.Name, &o.CreatedAt, &o.CreatedBy); err != nil {
			return nil, err
		}
		orgs = append(orgs, o)
	}
	return orgs, rows.Err()
}

// DeleteOrg hard-deletes an org. Returns ErrOrgNotFound if it doesn't exist
// and ErrOrgHasRepos if there are repos still owned by this org.
func (s *Store) DeleteOrg(ctx context.Context, name string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM orgs WHERE name = $1", name)
	if err != nil {
		if isForeignKeyViolation(err) {
			return ErrOrgHasRepos
		}
		return fmt.Errorf("delete org: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrOrgNotFound
	}
	return nil
}

// ListOrgRepos returns all repos owned by an org, ordered by name.
func (s *Store) ListOrgRepos(ctx context.Context, owner string) ([]model.Repo, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT name, owner, created_at, created_by FROM repos WHERE owner = $1 ORDER BY name",
		owner,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var repos []model.Repo
	for rows.Next() {
		var r model.Repo
		if err := rows.Scan(&r.Name, &r.Owner, &r.CreatedAt, &r.CreatedBy); err != nil {
			return nil, err
		}
		repos = append(repos, r)
	}
	return repos, rows.Err()
}

// CreateRepo creates a new repo and seeds its main branch in a single
// transaction. Returns ErrRepoExists if the name is taken.
// req.FullName() is used as the repo name; req.Owner must match the first path segment.
func (s *Store) CreateRepo(ctx context.Context, req model.CreateRepoRequest) (*model.Repo, error) {
	createdBy := req.CreatedBy
	if createdBy == "" {
		createdBy = "system"
	}

	fullName := req.FullName()

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
		`INSERT INTO repos (name, owner, created_by) VALUES ($1, $2, $3)
		 RETURNING name, owner, created_at, created_by`,
		fullName, req.Owner, createdBy,
	).Scan(&r.Name, &r.Owner, &r.CreatedAt, &r.CreatedBy)
	if err != nil {
		if isDuplicateKeyError(err) {
			return nil, ErrRepoExists
		}
		if isForeignKeyViolation(err) {
			return nil, ErrOrgNotFound
		}
		return nil, fmt.Errorf("insert repo: %w", err)
	}

	// Seed the main branch for the new repo.
	_, err = tx.ExecContext(ctx,
		"INSERT INTO branches (repo, name, head_sequence, base_sequence, status) VALUES ($1, 'main', 0, 0, 'active')",
		fullName,
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
		"SELECT name, owner, created_at, created_by FROM repos ORDER BY name",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var repos []model.Repo
	for rows.Next() {
		var r model.Repo
		if err := rows.Scan(&r.Name, &r.Owner, &r.CreatedAt, &r.CreatedBy); err != nil {
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
		"SELECT name, owner, created_at, created_by FROM repos WHERE name = $1",
		name,
	).Scan(&r.Name, &r.Owner, &r.CreatedAt, &r.CreatedBy)
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
	// RETURNING created_at so we can include it in the hash computation.
	var newSeq int64
	var commitCreatedAt time.Time
	err = tx.QueryRowContext(ctx,
		`INSERT INTO commits (repo, branch, message, author) VALUES ($1, $2, $3, $4) RETURNING sequence, created_at`,
		req.Repo, req.Branch, req.Message, req.Author,
	).Scan(&newSeq, &commitCreatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert commit: %w", err)
	}

	results := make([]model.CommitFileResult, 0, len(req.Files))
	hashFiles := make([]chainFile, 0, len(req.Files))

	for _, f := range req.Files {
		var versionIDPtr *string
		fileContentHash := ""

		if f.Content != nil {
			// Hash content for per-repo dedup.
			h := sha256.Sum256(f.Content)
			contentHash := hex.EncodeToString(h[:])
			fileContentHash = contentHash

			// Check for existing document with the same hash in this repo.
			var existingID string
			err = tx.QueryRowContext(ctx,
				"SELECT version_id FROM documents WHERE repo = $1 AND content_hash = $2 LIMIT 1",
				req.Repo, contentHash,
			).Scan(&existingID)

			if errors.Is(err, sql.ErrNoRows) {
				// Insert new document.
				existingID = uuid.New().String()
				var contentType sql.NullString
				if f.ContentType != "" {
					contentType = sql.NullString{String: f.ContentType, Valid: true}
				}

				// Decide: store inline in Postgres or in external blob store.
				var dbContent []byte
				var blobKey sql.NullString
				if s.blobStore != nil && s.blobThreshold > 0 && int64(len(f.Content)) > s.blobThreshold {
					// Upload to blob store BEFORE the DB INSERT so that if the
					// DB operation fails the blob can be re-uploaded on retry.
					if err := s.blobStore.Put(ctx, contentHash, bytes.NewReader(f.Content)); err != nil {
						return nil, fmt.Errorf("upload blob: %w", err)
					}
					blobKey = sql.NullString{String: contentHash, Valid: true}
					// dbContent stays nil — content column will be NULL in DB.
				} else {
					dbContent = f.Content
				}

				_, err = tx.ExecContext(ctx,
					`INSERT INTO documents (repo, version_id, path, content, content_hash, content_type, blob_key, created_at, created_by)
					 VALUES ($1, $2, $3, $4, $5, $6, $7, now(), $8)`,
					req.Repo, existingID, f.Path, dbContent, contentHash, contentType, blobKey, req.Author,
				)
				if err != nil {
					return nil, fmt.Errorf("insert document: %w", err)
				}
			} else if err != nil {
				return nil, fmt.Errorf("check dedup: %w", err)
			}

			versionIDPtr = &existingID
		}
		// nil Content → delete: versionIDPtr stays nil, fileContentHash stays "".

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
		hashFiles = append(hashFiles, chainFile{path: f.Path, contentHash: fileContentHash})
	}

	// Advance branch head.
	_, err = tx.ExecContext(ctx,
		"UPDATE branches SET head_sequence = $1 WHERE repo = $2 AND name = $3",
		newSeq, req.Repo, req.Branch,
	)
	if err != nil {
		return nil, fmt.Errorf("update branch head: %w", err)
	}

	// Compute and store commit_hash (per-branch chain).
	prevHash, err := fetchPrevCommitHash(ctx, tx, req.Repo, req.Branch, newSeq)
	if err != nil {
		return nil, err
	}
	// computeCommitHash sorts files internally; no pre-sort needed.
	commitHash := computeCommitHash(prevHash, newSeq, req.Repo, req.Branch, req.Author, req.Message, commitCreatedAt, hashFiles)
	if err := updateCommitHash(ctx, tx, req.Repo, newSeq, commitHash); err != nil {
		return nil, fmt.Errorf("store commit hash: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}

	return &model.CommitResponse{
		Sequence:   newSeq,
		Files:      results,
		CommitHash: commitHash,
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
		"INSERT INTO branches (repo, name, head_sequence, base_sequence, status, draft) VALUES ($1, $2, $3, $4, 'active', $5)",
		req.Repo, req.Name, mainHead, mainHead, req.Draft,
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
		Draft:        req.Draft,
	}, nil
}

// UpdateBranchDraft updates the draft flag on a branch.
// Returns ErrBranchNotFound if the branch does not exist.
func (s *Store) UpdateBranchDraft(ctx context.Context, repo, name string, draft bool) error {
	res, err := s.db.ExecContext(ctx,
		"UPDATE branches SET draft = $1 WHERE repo = $2 AND name = $3",
		draft, repo, name,
	)
	if err != nil {
		slog.Error("update branch draft failed", "repo", repo, "branch", name, "error", err)
		return fmt.Errorf("update branch draft: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrBranchNotFound
	}
	slog.Info("branch draft updated", "repo", repo, "branch", name, "draft", draft)
	return nil
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
	var branchDraft bool
	err = tx.QueryRowContext(ctx,
		"SELECT head_sequence, base_sequence, status, draft FROM branches WHERE repo = $1 AND name = $2 FOR UPDATE",
		req.Repo, req.Branch,
	).Scan(&branchHead, &baseSeq, &branchStatus, &branchDraft)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrBranchNotFound
		}
		return nil, nil, fmt.Errorf("lock branch: %w", err)
	}
	if branchStatus != "active" {
		return nil, nil, ErrBranchNotActive
	}
	if branchDraft {
		return nil, nil, ErrBranchDraft
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
	mergeMsg := fmt.Sprintf("merge branch '%s'", req.Branch)
	var newSeq int64
	var mergeCreatedAt time.Time
	err = tx.QueryRowContext(ctx,
		`INSERT INTO commits (repo, branch, message, author) VALUES ($1, 'main', $2, $3) RETURNING sequence, created_at`,
		req.Repo, mergeMsg, req.Author,
	).Scan(&newSeq, &mergeCreatedAt)
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

	// Compute and store commit_hash for the merge commit (per-branch: main chain).
	prevHash, err := fetchPrevCommitHash(ctx, tx, req.Repo, "main", newSeq)
	if err != nil {
		return nil, nil, err
	}
	hashFiles := make([]chainFile, 0, len(branchChanges))
	for path, versionID := range branchChanges {
		contentHash, err := fetchVersionContentHash(ctx, tx, versionID)
		if err != nil {
			return nil, nil, err
		}
		hashFiles = append(hashFiles, chainFile{path: path, contentHash: contentHash})
	}
	// computeCommitHash sorts files internally; no pre-sort needed.
	mergeHash := computeCommitHash(prevHash, newSeq, req.Repo, "main", req.Author, mergeMsg, mergeCreatedAt, hashFiles)
	if err := updateCommitHash(ctx, tx, req.Repo, newSeq, mergeHash); err != nil {
		return nil, nil, fmt.Errorf("store merge commit hash: %w", err)
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
		rebaseMsg := fmt.Sprintf("rebase: replay sequence %d", g.seq)
		var newSeq int64
		var rebaseCreatedAt time.Time
		err = tx.QueryRowContext(ctx,
			`INSERT INTO commits (repo, branch, message, author) VALUES ($1, $2, $3, $4) RETURNING sequence, created_at`,
			req.Repo, req.Branch, rebaseMsg, author,
		).Scan(&newSeq, &rebaseCreatedAt)
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

		// Compute and store commit_hash for this replayed commit (per-branch chain).
		prevHash, err := fetchPrevCommitHash(ctx, tx, req.Repo, req.Branch, newSeq)
		if err != nil {
			return nil, nil, err
		}
		hashFiles := make([]chainFile, 0, len(g.files))
		for _, f := range g.files {
			contentHash, err := fetchVersionContentHash(ctx, tx, f.versionID)
			if err != nil {
				return nil, nil, err
			}
			hashFiles = append(hashFiles, chainFile{path: f.path, contentHash: contentHash})
		}
		// computeCommitHash sorts files internally; no pre-sort needed.
		rebaseHash := computeCommitHash(prevHash, newSeq, req.Repo, req.Branch, author, rebaseMsg, rebaseCreatedAt, hashFiles)
		if err := updateCommitHash(ctx, tx, req.Repo, newSeq, rebaseHash); err != nil {
			return nil, nil, fmt.Errorf("store rebase commit hash: %w", err)
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

// CreateReviewComment records an inline file comment on a branch at its current
// head_sequence. Returns ErrBranchNotFound if the branch doesn't exist.
// reviewID is optional and may associate the comment with a formal review.
func (s *Store) CreateReviewComment(ctx context.Context, repo, branch, path, versionID, body, author string, reviewID *string) (*model.ReviewComment, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		slog.Error("tx begin failed", "op", "create_review_comment", "error", err)
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			slog.Error("tx rollback failed", "op", "create_review_comment", "error", rollbackErr)
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

	var nullReviewID sql.NullString
	if reviewID != nil && *reviewID != "" {
		nullReviewID = sql.NullString{String: *reviewID, Valid: true}
	}

	var id string
	var createdAt time.Time
	err = tx.QueryRowContext(ctx,
		`INSERT INTO review_comments (review_id, repo, branch, path, version_id, body, author, sequence)
		 VALUES ($1, $2, $3, $4, $5::uuid, $6, $7, $8)
		 RETURNING id::text, created_at`,
		nullReviewID, repo, branch, path, versionID, body, author, headSeq,
	).Scan(&id, &createdAt)
	if err != nil {
		return nil, fmt.Errorf("insert review_comment: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}

	rc := &model.ReviewComment{
		ID:        id,
		Branch:    branch,
		Path:      path,
		VersionID: versionID,
		Body:      body,
		Author:    author,
		Sequence:  headSeq,
		CreatedAt: createdAt,
	}
	if reviewID != nil {
		rc.ReviewID = reviewID
	}
	return rc, nil
}

// ListReviewComments returns inline file comments for a branch, ordered by
// created_at ASC. If path is non-nil, only comments on that path are returned.
func (s *Store) ListReviewComments(ctx context.Context, repo, branch string, path *string) ([]model.ReviewComment, error) {
	q := `SELECT id::text, review_id::text, path, version_id::text, body, author, sequence, created_at
	      FROM review_comments
	      WHERE repo = $1 AND branch = $2`
	args := []interface{}{repo, branch}
	if path != nil {
		q += " AND path = $3"
		args = append(args, *path)
	}
	q += " ORDER BY created_at ASC"

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var comments []model.ReviewComment
	for rows.Next() {
		var c model.ReviewComment
		c.Branch = branch
		var reviewID sql.NullString
		if err := rows.Scan(&c.ID, &reviewID, &c.Path, &c.VersionID, &c.Body, &c.Author, &c.Sequence, &c.CreatedAt); err != nil {
			return nil, err
		}
		if reviewID.Valid {
			s := reviewID.String
			c.ReviewID = &s
		}
		comments = append(comments, c)
	}
	return comments, rows.Err()
}

// GetReviewComment returns a single review comment by ID and repo.
// Returns ErrCommentNotFound if no matching comment exists.
func (s *Store) GetReviewComment(ctx context.Context, repo, id string) (*model.ReviewComment, error) {
	var c model.ReviewComment
	var reviewID sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id::text, review_id::text, branch, path, version_id::text, body, author, sequence, created_at
		 FROM review_comments
		 WHERE repo = $1 AND id = $2::uuid`,
		repo, id,
	).Scan(&c.ID, &reviewID, &c.Branch, &c.Path, &c.VersionID, &c.Body, &c.Author, &c.Sequence, &c.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrCommentNotFound
		}
		return nil, fmt.Errorf("get review_comment: %w", err)
	}
	if reviewID.Valid {
		s := reviewID.String
		c.ReviewID = &s
	}
	return &c, nil
}

// DeleteReviewComment removes a review comment by ID and repo.
// Returns ErrCommentNotFound if no matching comment exists.
func (s *Store) DeleteReviewComment(ctx context.Context, repo, id string) error {
	res, err := s.db.ExecContext(ctx,
		"DELETE FROM review_comments WHERE repo = $1 AND id = $2::uuid",
		repo, id,
	)
	if err != nil {
		return fmt.Errorf("delete review_comment: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrCommentNotFound
	}
	return nil
}

// CreateCheckRun records a CI check run for a branch. If atSequence is
// non-nil it is used as the recorded sequence; otherwise the branch's
// current head_sequence is used. Returns ErrBranchNotFound if the branch
// doesn't exist.
// logURL is optional (may be nil) and stores a GCS URI or local file path.
func (s *Store) CreateCheckRun(ctx context.Context, repo, branch, checkName string, status model.CheckRunStatus, reporter string, logURL *string, atSequence *int64) (*model.CheckRun, error) {
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
	if atSequence != nil {
		headSeq = *atSequence
	}

	id := uuid.New().String()
	var createdAt time.Time
	var nullLogURL sql.NullString
	if logURL != nil {
		nullLogURL = sql.NullString{String: *logURL, Valid: true}
	}
	err = tx.QueryRowContext(ctx,
		`INSERT INTO check_runs (id, repo, branch, sequence, check_name, status, reporter, log_url)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING created_at`,
		id, repo, branch, headSeq, checkName, string(status), reporter, nullLogURL,
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
		LogURL:    logURL,
		CreatedAt: createdAt,
	}, nil
}

// ListCheckRuns returns check runs for a branch in the given repo, ordered by
// created_at DESC. If atSeq is non-nil, only check runs at that sequence are
// returned.
func (s *Store) ListCheckRuns(ctx context.Context, repo, branch string, atSeq *int64) ([]model.CheckRun, error) {
	q := `SELECT id, sequence, check_name, status, reporter, log_url, created_at
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
		var nullLogURL sql.NullString
		if err := rows.Scan(&cr.ID, &cr.Sequence, &cr.CheckName, &statusStr, &cr.Reporter, &nullLogURL, &cr.CreatedAt); err != nil {
			return nil, err
		}
		cr.Status = model.CheckRunStatus(statusStr)
		if nullLogURL.Valid {
			cr.LogURL = &nullLogURL.String
		}
		checkRuns = append(checkRuns, cr)
	}
	return checkRuns, rows.Err()
}

// GetRole returns the role for an identity in a repo, or ErrRoleNotFound.
// It first checks the repo-scoped roles table. If no row is found, it falls
// back to org_members for the repo's owning org: org owner → admin, org member → reader.
func (s *Store) GetRole(ctx context.Context, repo, identity string) (*model.Role, error) {
	var r model.Role
	err := s.db.QueryRowContext(ctx,
		"SELECT identity, role FROM roles WHERE repo = $1 AND identity = $2",
		repo, identity,
	).Scan(&r.Identity, &r.Role)
	if err == nil {
		return &r, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	// Fall back to org membership for the repo's owning org.
	var orgRoleStr string
	err = s.db.QueryRowContext(ctx,
		`SELECT om.role FROM org_members om
		 JOIN repos r ON r.owner = om.org
		 WHERE r.name = $1 AND om.identity = $2`,
		repo, identity,
	).Scan(&orgRoleStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRoleNotFound
		}
		return nil, err
	}

	var repoRole model.RoleType
	switch orgRoleStr {
	case "owner":
		repoRole = model.RoleAdmin
	case "member":
		repoRole = model.RoleReader
	default:
		return nil, ErrRoleNotFound
	}
	return &model.Role{Identity: identity, Role: repoRole}, nil
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

	// 5. Collect blob_keys for orphaned documents before deleting them,
	// then remove the blobs from the external store, then delete the DB rows.
	orphanQuery := `
		SELECT blob_key FROM documents
		WHERE repo = $1
		  AND blob_key IS NOT NULL
		  AND version_id NOT IN (
		      SELECT DISTINCT version_id FROM file_commits
		      WHERE repo = $1 AND version_id IS NOT NULL
		  )`
	if s.blobStore != nil {
		blobRows, berr := tx.QueryContext(ctx, orphanQuery, req.Repo)
		if berr != nil {
			return nil, fmt.Errorf("collect blob keys: %w", berr)
		}
		var blobKeys []string
		for blobRows.Next() {
			var key string
			if berr := blobRows.Scan(&key); berr != nil {
				blobRows.Close()
				return nil, fmt.Errorf("scan blob key: %w", berr)
			}
			blobKeys = append(blobKeys, key)
		}
		blobRows.Close()
		if berr := blobRows.Err(); berr != nil {
			return nil, fmt.Errorf("iterate blob keys: %w", berr)
		}

		if !req.DryRun {
			for _, key := range blobKeys {
				if berr := s.blobStore.Delete(ctx, key); berr != nil {
					return nil, fmt.Errorf("delete blob %s: %w", key, berr)
				}
			}
		}
	}

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

// ---------------------------------------------------------------------------
// Org membership
// ---------------------------------------------------------------------------

// GetOrgMember returns an org member by org+identity, or ErrOrgMemberNotFound.
func (s *Store) GetOrgMember(ctx context.Context, org, identity string) (*model.OrgMember, error) {
	var m model.OrgMember
	var roleStr string
	err := s.db.QueryRowContext(ctx,
		"SELECT org, identity, role, invited_by, created_at FROM org_members WHERE org = $1 AND identity = $2",
		org, identity,
	).Scan(&m.Org, &m.Identity, &roleStr, &m.InvitedBy, &m.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrOrgMemberNotFound
		}
		return nil, err
	}
	m.Role = model.OrgRole(roleStr)
	return &m, nil
}

// AddOrgMember inserts or updates an org member (upsert by org+identity).
func (s *Store) AddOrgMember(ctx context.Context, org, identity string, role model.OrgRole, invitedBy string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO org_members (org, identity, role, invited_by)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (org, identity) DO UPDATE SET role = $3, invited_by = $4`,
		org, identity, string(role), invitedBy,
	)
	if err != nil {
		slog.Error("add org member failed", "org", org, "identity", identity, "error", err)
		return err
	}
	slog.Info("org member added", "org", org, "identity", identity, "role", role)
	return nil
}

// RemoveOrgMember removes a member from an org. Returns ErrOrgMemberNotFound if not found.
func (s *Store) RemoveOrgMember(ctx context.Context, org, identity string) error {
	result, err := s.db.ExecContext(ctx,
		"DELETE FROM org_members WHERE org = $1 AND identity = $2",
		org, identity,
	)
	if err != nil {
		slog.Error("remove org member failed", "org", org, "identity", identity, "error", err)
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrOrgMemberNotFound
	}
	slog.Info("org member removed", "org", org, "identity", identity)
	return nil
}

// ListOrgMembers returns all members of an org ordered by identity.
func (s *Store) ListOrgMembers(ctx context.Context, org string) ([]model.OrgMember, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT org, identity, role, invited_by, created_at FROM org_members WHERE org = $1 ORDER BY identity",
		org,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var members []model.OrgMember
	for rows.Next() {
		var m model.OrgMember
		var roleStr string
		if err := rows.Scan(&m.Org, &m.Identity, &roleStr, &m.InvitedBy, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.Role = model.OrgRole(roleStr)
		members = append(members, m)
	}
	return members, rows.Err()
}

// ---------------------------------------------------------------------------
// Org invitations
// ---------------------------------------------------------------------------

// CreateInvite inserts a new org invitation and returns it.
func (s *Store) CreateInvite(ctx context.Context, org, email string, role model.OrgRole, invitedBy, token string, expiresAt time.Time) (*model.OrgInvite, error) {
	id := uuid.New().String()
	var inv model.OrgInvite
	var roleStr string
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO org_invites (id, org, email, role, invited_by, token, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING id, org, email, role, invited_by, token, expires_at, created_at`,
		id, org, email, string(role), invitedBy, token, expiresAt,
	).Scan(&inv.ID, &inv.Org, &inv.Email, &roleStr, &inv.InvitedBy, &inv.Token, &inv.ExpiresAt, &inv.CreatedAt)
	if err != nil {
		slog.Error("create invite failed", "org", org, "email", email, "error", err)
		return nil, fmt.Errorf("insert invite: %w", err)
	}
	inv.Role = model.OrgRole(roleStr)
	slog.Info("invite created", "org", org, "email", email, "role", role)
	return &inv, nil
}

// ListInvites returns pending (not accepted, not expired) invites for an org, ordered by created_at.
func (s *Store) ListInvites(ctx context.Context, org string) ([]model.OrgInvite, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, org, email, role, invited_by, expires_at, created_at
		 FROM org_invites
		 WHERE org = $1 AND accepted_at IS NULL AND expires_at > now()
		 ORDER BY created_at`,
		org,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var invites []model.OrgInvite
	for rows.Next() {
		var inv model.OrgInvite
		var roleStr string
		if err := rows.Scan(&inv.ID, &inv.Org, &inv.Email, &roleStr, &inv.InvitedBy, &inv.ExpiresAt, &inv.CreatedAt); err != nil {
			return nil, err
		}
		inv.Role = model.OrgRole(roleStr)
		invites = append(invites, inv)
	}
	return invites, rows.Err()
}

// AcceptInvite accepts a pending invite: verifies the token, checks expiry and email match,
// sets accepted_at, and upserts the identity into org_members — all in one transaction.
func (s *Store) AcceptInvite(ctx context.Context, org, token, identity string) error {
	slog.Debug("accept invite started", "org", org)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		slog.Error("tx begin failed", "op", "accept_invite", "error", err)
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			slog.Error("tx rollback failed", "op", "accept_invite", "error", rollbackErr)
		}
	}()

	var invID, email, roleStr string
	var expiresAt time.Time
	var acceptedAt sql.NullTime
	err = tx.QueryRowContext(ctx,
		`SELECT id, email, role, expires_at, accepted_at
		 FROM org_invites WHERE org = $1 AND token = $2 FOR UPDATE`,
		org, token,
	).Scan(&invID, &email, &roleStr, &expiresAt, &acceptedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInviteNotFound
		}
		return fmt.Errorf("fetch invite: %w", err)
	}

	if acceptedAt.Valid {
		return ErrInviteAlreadyAccepted
	}
	if time.Now().After(expiresAt) {
		return ErrInviteExpired
	}
	if email != identity {
		return ErrEmailMismatch
	}

	// Mark accepted.
	_, err = tx.ExecContext(ctx,
		"UPDATE org_invites SET accepted_at = now() WHERE id = $1",
		invID,
	)
	if err != nil {
		return fmt.Errorf("update invite: %w", err)
	}

	// Upsert into org_members.
	_, err = tx.ExecContext(ctx,
		`INSERT INTO org_members (org, identity, role, invited_by)
		 SELECT org, $1, role, invited_by FROM org_invites WHERE id = $2
		 ON CONFLICT (org, identity) DO UPDATE SET role = EXCLUDED.role, invited_by = EXCLUDED.invited_by`,
		identity, invID,
	)
	if err != nil {
		return fmt.Errorf("add member: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	slog.Info("invite accepted", "org", org, "identity", identity)
	return nil
}

// RevokeInvite deletes a pending invite by ID. Returns ErrInviteNotFound if not found.
func (s *Store) RevokeInvite(ctx context.Context, org, inviteID string) error {
	result, err := s.db.ExecContext(ctx,
		"DELETE FROM org_invites WHERE id = $1 AND org = $2",
		inviteID, org,
	)
	if err != nil {
		slog.Error("revoke invite failed", "org", org, "invite_id", inviteID, "error", err)
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrInviteNotFound
	}
	slog.Info("invite revoked", "org", org, "invite_id", inviteID)
	return nil
}

// ---------------------------------------------------------------------------
// Release management
// ---------------------------------------------------------------------------

// CreateRelease inserts a new release row. Returns ErrReleaseExists if the
// (repo, name) pair is already taken.
func (s *Store) CreateRelease(ctx context.Context, repo, name string, sequence int64, body, createdBy string) (*model.Release, error) {
	var rel model.Release
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO releases (repo, name, sequence, body, created_by)
		 VALUES ($1, $2, $3, NULLIF($4, ''), $5)
		 RETURNING id, repo, name, sequence, COALESCE(body, ''), created_by, created_at`,
		repo, name, sequence, body, createdBy,
	).Scan(&rel.ID, &rel.Repo, &rel.Name, &rel.Sequence, &rel.Body, &rel.CreatedBy, &rel.CreatedAt)
	if err != nil {
		if isDuplicateKeyError(err) {
			return nil, ErrReleaseExists
		}
		slog.Error("create release failed", "repo", repo, "name", name, "error", err)
		return nil, fmt.Errorf("insert release: %w", err)
	}
	slog.Info("release created", "repo", repo, "name", name, "sequence", sequence, "by", createdBy)
	return &rel, nil
}

// GetRelease returns the named release for a repo, or ErrReleaseNotFound.
func (s *Store) GetRelease(ctx context.Context, repo, name string) (*model.Release, error) {
	var rel model.Release
	err := s.db.QueryRowContext(ctx,
		`SELECT id, repo, name, sequence, COALESCE(body, ''), created_by, created_at
		 FROM releases WHERE repo = $1 AND name = $2`,
		repo, name,
	).Scan(&rel.ID, &rel.Repo, &rel.Name, &rel.Sequence, &rel.Body, &rel.CreatedBy, &rel.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrReleaseNotFound
		}
		return nil, err
	}
	return &rel, nil
}

// ListReleases returns all releases for a repo ordered newest first.
// limit controls the page size (0 → no limit); afterID is a cursor (UUID) for
// keyset pagination (exclusive lower bound on created_at).
func (s *Store) ListReleases(ctx context.Context, repo string, limit int, afterID string) ([]model.Release, error) {
	var rows *sql.Rows
	var err error
	if afterID == "" {
		if limit <= 0 {
			rows, err = s.db.QueryContext(ctx,
				`SELECT id, repo, name, sequence, COALESCE(body, ''), created_by, created_at
				 FROM releases WHERE repo = $1 ORDER BY created_at DESC, id DESC`,
				repo,
			)
		} else {
			rows, err = s.db.QueryContext(ctx,
				`SELECT id, repo, name, sequence, COALESCE(body, ''), created_by, created_at
				 FROM releases WHERE repo = $1 ORDER BY created_at DESC, id DESC LIMIT $2`,
				repo, limit,
			)
		}
	} else {
		// Validate that the cursor ID actually exists before running the main query.
		// If it doesn't exist, the subquery returns no rows and the WHERE condition
		// would be vacuously false, returning all releases instead of an error.
		var exists bool
		if err := s.db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM releases WHERE id = $1)`, afterID,
		).Scan(&exists); err != nil {
			return nil, fmt.Errorf("validate cursor: %w", err)
		}
		if !exists {
			return nil, ErrInvalidCursor
		}

		// Use the row with id=afterID as the cursor.
		if limit <= 0 {
			rows, err = s.db.QueryContext(ctx,
				`SELECT id, repo, name, sequence, COALESCE(body, ''), created_by, created_at
				 FROM releases
				 WHERE repo = $1
				   AND (created_at, id) < (SELECT created_at, id FROM releases WHERE id = $2)
				 ORDER BY created_at DESC, id DESC`,
				repo, afterID,
			)
		} else {
			rows, err = s.db.QueryContext(ctx,
				`SELECT id, repo, name, sequence, COALESCE(body, ''), created_by, created_at
				 FROM releases
				 WHERE repo = $1
				   AND (created_at, id) < (SELECT created_at, id FROM releases WHERE id = $2)
				 ORDER BY created_at DESC, id DESC LIMIT $3`,
				repo, afterID, limit,
			)
		}
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var releases []model.Release
	for rows.Next() {
		var rel model.Release
		if err := rows.Scan(&rel.ID, &rel.Repo, &rel.Name, &rel.Sequence, &rel.Body, &rel.CreatedBy, &rel.CreatedAt); err != nil {
			return nil, err
		}
		releases = append(releases, rel)
	}
	return releases, rows.Err()
}

// DeleteRelease removes a release by name. Returns ErrReleaseNotFound if not found.
func (s *Store) DeleteRelease(ctx context.Context, repo, name string) error {
	result, err := s.db.ExecContext(ctx,
		"DELETE FROM releases WHERE repo = $1 AND name = $2",
		repo, name,
	)
	if err != nil {
		slog.Error("delete release failed", "repo", repo, "name", name, "error", err)
		return fmt.Errorf("delete release: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrReleaseNotFound
	}
	slog.Info("release deleted", "repo", repo, "name", name)
	return nil
}

// CommitSequenceExists reports whether the given sequence number exists in the
// commits table for the given repo.
func (s *Store) CommitSequenceExists(ctx context.Context, repo string, sequence int64) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM commits WHERE repo = $1 AND sequence = $2)`,
		repo, sequence,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check commit sequence: %w", err)
	}
	return exists, nil
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

// ---------------------------------------------------------------------------
// Event subscription store methods
// ---------------------------------------------------------------------------

// CreateSubscription inserts a new event subscription.
func (s *Store) CreateSubscription(ctx context.Context, req model.CreateSubscriptionRequest) (*model.EventSubscription, error) {
	config, err := req.Config.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}

	var id string
	var createdAt time.Time
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO event_subscriptions (repo, event_types, backend, config, created_by)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, created_at`,
		req.Repo,
		pq.Array(req.EventTypes),
		req.Backend,
		config,
		req.CreatedBy,
	).Scan(&id, &createdAt)
	if err != nil {
		return nil, fmt.Errorf("create subscription: %w", err)
	}

	sub := &model.EventSubscription{
		ID:         id,
		Repo:       req.Repo,
		EventTypes: req.EventTypes,
		Backend:    req.Backend,
		Config:     req.Config,
		CreatedAt:  createdAt,
		CreatedBy:  req.CreatedBy,
	}
	return sub, nil
}

// ListSubscriptions returns all event subscriptions. If repo is non-nil,
// only subscriptions matching that repo (or global ones) are returned.
func (s *Store) ListSubscriptions(ctx context.Context) ([]model.EventSubscription, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, repo, event_types, backend, config, created_at, created_by, suspended_at, failure_count
		FROM event_subscriptions
		ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list subscriptions: %w", err)
	}
	defer rows.Close()

	var subs []model.EventSubscription
	for rows.Next() {
		var sub model.EventSubscription
		var repo sql.NullString
		var eventTypes pq.StringArray
		var suspendedAt sql.NullTime
		var configBytes []byte
		if err := rows.Scan(&sub.ID, &repo, &eventTypes, &sub.Backend,
			&configBytes, &sub.CreatedAt, &sub.CreatedBy, &suspendedAt, &sub.FailureCount); err != nil {
			return nil, fmt.Errorf("scan subscription: %w", err)
		}
		if repo.Valid {
			sub.Repo = &repo.String
		}
		if len(eventTypes) > 0 {
			sub.EventTypes = []string(eventTypes)
		}
		if suspendedAt.Valid {
			sub.SuspendedAt = &suspendedAt.Time
		}
		sub.Config = json.RawMessage(configBytes)
		subs = append(subs, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate subscriptions: %w", err)
	}
	return subs, nil
}

// DeleteSubscription deletes a subscription by ID.
func (s *Store) DeleteSubscription(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM event_subscriptions WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete subscription: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrSubscriptionNotFound
	}
	return nil
}

// ResumeSubscription clears the suspended_at on a subscription.
func (s *Store) ResumeSubscription(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE event_subscriptions SET suspended_at = NULL, failure_count = 0 WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("resume subscription: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrSubscriptionNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// Proposal store methods
// ---------------------------------------------------------------------------

// CreateProposal opens a new proposal for a branch. Returns ErrProposalExists if
// the branch already has an open proposal (enforced by unique partial index).
func (s *Store) CreateProposal(ctx context.Context, repo, branch, baseBranch, title, description, author string) (*model.Proposal, error) {
	id := uuid.New().String()
	var p model.Proposal
	p.ID = id
	p.Repo = repo
	p.Branch = branch
	p.BaseBranch = baseBranch
	p.Title = title
	p.Description = description
	p.Author = author
	p.State = model.ProposalOpen

	var nullDesc sql.NullString
	if description != "" {
		nullDesc = sql.NullString{String: description, Valid: true}
	}

	err := s.db.QueryRowContext(ctx,
		`INSERT INTO proposals (id, repo, branch, base_branch, title, description, author, state)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, 'open')
		 RETURNING created_at, updated_at`,
		id, repo, branch, baseBranch, title, nullDesc, author,
	).Scan(&p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		if isDuplicateKeyError(err) {
			return nil, ErrProposalExists
		}
		if isForeignKeyViolation(err) {
			return nil, ErrBranchNotFound
		}
		return nil, fmt.Errorf("insert proposal: %w", err)
	}
	return &p, nil
}

// GetProposal returns a single proposal by ID within a repo.
// Returns ErrProposalNotFound if no matching proposal exists.
func (s *Store) GetProposal(ctx context.Context, repo, proposalID string) (*model.Proposal, error) {
	var p model.Proposal
	var nullDesc sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id::text, repo, branch, base_branch, title, description, author, state, created_at, updated_at
		 FROM proposals WHERE repo = $1 AND id = $2::uuid`,
		repo, proposalID,
	).Scan(&p.ID, &p.Repo, &p.Branch, &p.BaseBranch, &p.Title, &nullDesc, &p.Author, &p.State, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrProposalNotFound
		}
		return nil, fmt.Errorf("get proposal: %w", err)
	}
	if nullDesc.Valid {
		p.Description = nullDesc.String
	}
	return &p, nil
}

// ListProposals returns proposals for a repo ordered by created_at DESC.
// If state is non-nil, only proposals with that state are returned.
// If branch is non-nil, only proposals for that branch are returned.
func (s *Store) ListProposals(ctx context.Context, repo string, state *model.ProposalState, branch *string) ([]*model.Proposal, error) {
	q := `SELECT id::text, repo, branch, base_branch, title, description, author, state, created_at, updated_at
	      FROM proposals WHERE repo = $1`
	args := []interface{}{repo}
	if state != nil {
		args = append(args, string(*state))
		q += fmt.Sprintf(" AND state = $%d", len(args))
	}
	if branch != nil {
		args = append(args, *branch)
		q += fmt.Sprintf(" AND branch = $%d", len(args))
	}
	q += " ORDER BY created_at DESC"

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list proposals: %w", err)
	}
	defer rows.Close()

	var proposals []*model.Proposal
	for rows.Next() {
		var p model.Proposal
		var nullDesc sql.NullString
		if err := rows.Scan(&p.ID, &p.Repo, &p.Branch, &p.BaseBranch, &p.Title, &nullDesc, &p.Author, &p.State, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan proposal: %w", err)
		}
		if nullDesc.Valid {
			p.Description = nullDesc.String
		}
		proposals = append(proposals, &p)
	}
	return proposals, rows.Err()
}

// UpdateProposal updates the title and/or description of a proposal.
// Returns ErrProposalNotFound if no matching proposal exists.
func (s *Store) UpdateProposal(ctx context.Context, repo, proposalID string, title, description *string) (*model.Proposal, error) {
	if title == nil && description == nil {
		return s.GetProposal(ctx, repo, proposalID)
	}

	setClauses := []string{"updated_at = now()"}
	args := []interface{}{}
	argIdx := 1

	if title != nil {
		setClauses = append(setClauses, fmt.Sprintf("title = $%d", argIdx))
		args = append(args, *title)
		argIdx++
	}
	if description != nil {
		setClauses = append(setClauses, fmt.Sprintf("description = $%d", argIdx))
		args = append(args, *description)
		argIdx++
	}

	setSQL := ""
	for i, c := range setClauses {
		if i > 0 {
			setSQL += ", "
		}
		setSQL += c
	}

	args = append(args, repo, proposalID)
	q := fmt.Sprintf(
		`UPDATE proposals SET %s WHERE repo = $%d AND id = $%d::uuid
		 RETURNING id::text, repo, branch, base_branch, title, description, author, state, created_at, updated_at`,
		setSQL, argIdx, argIdx+1,
	)

	var p model.Proposal
	var nullDesc sql.NullString
	err := s.db.QueryRowContext(ctx, q, args...).Scan(
		&p.ID, &p.Repo, &p.Branch, &p.BaseBranch, &p.Title, &nullDesc, &p.Author, &p.State, &p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrProposalNotFound
		}
		return nil, fmt.Errorf("update proposal: %w", err)
	}
	if nullDesc.Valid {
		p.Description = nullDesc.String
	}
	return &p, nil
}

// CloseProposal sets a proposal's state to closed.
// Returns ErrProposalNotFound if no matching proposal exists.
func (s *Store) CloseProposal(ctx context.Context, repo, proposalID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE proposals SET state = 'closed', updated_at = now()
		 WHERE repo = $1 AND id = $2::uuid`,
		repo, proposalID,
	)
	if err != nil {
		return fmt.Errorf("close proposal: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrProposalNotFound
	}
	return nil
}

// MergeProposal finds the open proposal for the given branch and sets its state
// to merged. It is a no-op (returns nil, nil) if no open proposal exists.
// Returns the merged proposal so the caller can emit an event.
func (s *Store) MergeProposal(ctx context.Context, repo, branch string) (*model.Proposal, error) {
	var p model.Proposal
	var nullDesc sql.NullString
	err := s.db.QueryRowContext(ctx,
		`UPDATE proposals SET state = 'merged', updated_at = now()
		 WHERE repo = $1 AND branch = $2 AND state = 'open'
		 RETURNING id::text, repo, branch, base_branch, title, description, author, state, created_at, updated_at`,
		repo, branch,
	).Scan(&p.ID, &p.Repo, &p.Branch, &p.BaseBranch, &p.Title, &nullDesc, &p.Author, &p.State, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil // no-op: no open proposal for this branch
		}
		return nil, fmt.Errorf("merge proposal: %w", err)
	}
	if nullDesc.Valid {
		p.Description = nullDesc.String
	}
	return &p, nil
}
