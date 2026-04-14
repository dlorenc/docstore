package model

import "time"

// ---------------------------------------------------------------------------
// Pagination
// ---------------------------------------------------------------------------

// PaginationParams are the common query parameters for list endpoints.
type PaginationParams struct {
	Limit int    `json:"limit,omitempty"`
	After string `json:"after,omitempty"`
}

// ---------------------------------------------------------------------------
// GET /tree
// ---------------------------------------------------------------------------

// TreeEntry is one file in a materialized tree.
type TreeEntry struct {
	Path        string `json:"path"`
	VersionID   string `json:"version_id"`
	ContentHash string `json:"content_hash"`
}

// TreeResponse is the response for GET /tree.
type TreeResponse struct {
	Entries []TreeEntry `json:"entries"`
}

// ---------------------------------------------------------------------------
// GET /file/:path/history
// ---------------------------------------------------------------------------

// FileHistoryEntry is one row in a file's history.
type FileHistoryEntry struct {
	Sequence  int64     `json:"sequence"`
	VersionID string    `json:"version_id"`
	Message   string    `json:"message"`
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"created_at"`
}

// FileHistoryResponse is the response for GET /file/:path/history.
type FileHistoryResponse struct {
	Entries []FileHistoryEntry `json:"entries"`
}

// ---------------------------------------------------------------------------
// GET /file/:path
// ---------------------------------------------------------------------------

// FileResponse is the response for GET /file/:path.
type FileResponse struct {
	Path        string `json:"path"`
	VersionID   string `json:"version_id"`
	ContentHash string `json:"content_hash"`
	Content     []byte `json:"content"`
}

// ---------------------------------------------------------------------------
// GET /diff
// ---------------------------------------------------------------------------

// DiffEntry represents a single file change in a diff.
type DiffEntry struct {
	Path      string  `json:"path"`
	VersionID *string `json:"version_id"` // nil means deleted
}

// ConflictEntry represents a file that was changed on both main and the branch.
type ConflictEntry struct {
	Path            string `json:"path"`
	MainVersionID   string `json:"main_version_id"`
	BranchVersionID string `json:"branch_version_id"`
}

// DiffResponse is the response for GET /diff.
type DiffResponse struct {
	BranchChanges []DiffEntry     `json:"branch_changes"`
	MainChanges   []DiffEntry     `json:"main_changes"`
	Conflicts     []ConflictEntry `json:"conflicts,omitempty"`
}

// ---------------------------------------------------------------------------
// GET /commit/:sequence
// ---------------------------------------------------------------------------

// CommitDetail represents one file change within a commit.
type CommitDetail struct {
	Path      string  `json:"path"`
	VersionID *string `json:"version_id"` // nil means delete
}

// GetCommitResponse is the response for GET /commit/:sequence.
type GetCommitResponse struct {
	Sequence  int64          `json:"sequence"`
	Message   string         `json:"message"`
	Author    string         `json:"author"`
	CreatedAt time.Time      `json:"created_at"`
	Files     []CommitDetail `json:"files"`
}

// ---------------------------------------------------------------------------
// GET /branches
// ---------------------------------------------------------------------------

// BranchesResponse is the response for GET /branches.
type BranchesResponse struct {
	Branches []Branch `json:"branches"`
}

// ---------------------------------------------------------------------------
// GET /branch/:name/status
// ---------------------------------------------------------------------------

// PolicyResult is one policy evaluation outcome.
type PolicyResult struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Reason string `json:"reason"`
}

// BranchStatusResponse is the response for GET /branch/:name/status.
type BranchStatusResponse struct {
	Mergeable bool           `json:"mergeable"`
	Policies  []PolicyResult `json:"policies"`
}

// ---------------------------------------------------------------------------
// POST /commit
// ---------------------------------------------------------------------------

// FileChange is one file in a commit request.
// A nil Content means delete.
type FileChange struct {
	Path    string `json:"path"`
	Content []byte `json:"content,omitempty"`
}

// CommitRequest is the body for POST /repos/:name/commit.
type CommitRequest struct {
	Repo    string       `json:"repo,omitempty"`
	Branch  string       `json:"branch"`
	Files   []FileChange `json:"files"`
	Message string       `json:"message"`
	Author  string       `json:"author"`
}

// CommitFileResult describes one file outcome in a commit response.
type CommitFileResult struct {
	Path      string  `json:"path"`
	VersionID *string `json:"version_id"` // nil for deletes
}

// CommitResponse is the response for POST /commit.
type CommitResponse struct {
	Sequence int64              `json:"sequence"`
	Files    []CommitFileResult `json:"files"`
}

// ---------------------------------------------------------------------------
// POST /branch
// ---------------------------------------------------------------------------

// CreateBranchRequest is the body for POST /repos/:name/branch.
type CreateBranchRequest struct {
	Repo string `json:"repo,omitempty"`
	Name string `json:"name"`
}

// CreateBranchResponse is the response for POST /branch.
type CreateBranchResponse struct {
	Name         string `json:"name"`
	BaseSequence int64  `json:"base_sequence"`
}

// ---------------------------------------------------------------------------
// POST /merge
// ---------------------------------------------------------------------------

// MergeRequest is the body for POST /repos/:name/merge.
type MergeRequest struct {
	Repo   string `json:"repo,omitempty"`
	Branch string `json:"branch"`
	Author string `json:"author,omitempty"`
}

// MergeResponse is the response for a successful POST /merge.
type MergeResponse struct {
	Sequence int64 `json:"sequence"`
}

// MergeConflictError is the error response when merge has conflicts.
type MergeConflictError struct {
	Conflicts []ConflictEntry `json:"conflicts"`
}

// MergePolicyError is the error response when merge policies fail.
type MergePolicyError struct {
	Policies []PolicyResult `json:"policies"`
}

// ---------------------------------------------------------------------------
// POST /rebase
// ---------------------------------------------------------------------------

// RebaseRequest is the body for POST /repos/:name/rebase.
type RebaseRequest struct {
	Repo   string `json:"repo,omitempty"`
	Branch string `json:"branch"`
	Author string `json:"author,omitempty"`
}

// RebaseResponse is the response for a successful POST /rebase.
type RebaseResponse struct {
	NewBaseSequence int64 `json:"base_sequence"`
	NewHeadSequence int64 `json:"head_sequence"`
	CommitsReplayed int64 `json:"commits_replayed"`
}

// RebaseConflictError is the error response when rebase has conflicts.
type RebaseConflictError struct {
	Conflicts []ConflictEntry `json:"conflicts"`
}

// ---------------------------------------------------------------------------
// POST /review
// ---------------------------------------------------------------------------

// CreateReviewRequest is the body for POST /review.
type CreateReviewRequest struct {
	Branch string       `json:"branch"`
	Status ReviewStatus `json:"status"`
	Body   string       `json:"body,omitempty"`
}

// CreateReviewResponse is the response for POST /review.
type CreateReviewResponse struct {
	ID       string `json:"id"`
	Sequence int64  `json:"sequence"`
}

// ---------------------------------------------------------------------------
// POST /check
// ---------------------------------------------------------------------------

// CreateCheckRunRequest is the body for POST /check.
type CreateCheckRunRequest struct {
	Branch    string         `json:"branch"`
	CheckName string         `json:"check_name"`
	Status    CheckRunStatus `json:"status"`
}

// CreateCheckRunResponse is the response for POST /check.
type CreateCheckRunResponse struct {
	ID       string `json:"id"`
	Sequence int64  `json:"sequence"`
}

// ---------------------------------------------------------------------------
// DELETE /branch/:name
// ---------------------------------------------------------------------------

// DeleteBranchResponse is the response for DELETE /branch/:name.
type DeleteBranchResponse struct {
	Name   string       `json:"name"`
	Status BranchStatus `json:"status"`
}

// ---------------------------------------------------------------------------
// POST /repos / GET /repos / GET /repos/:name / DELETE /repos/:name
// ---------------------------------------------------------------------------

// CreateRepoRequest is the body for POST /repos.
type CreateRepoRequest struct {
	Name      string `json:"name"`
	CreatedBy string `json:"created_by,omitempty"`
}

// ReposResponse is the response for GET /repos.
type ReposResponse struct {
	Repos []Repo `json:"repos"`
}

// ---------------------------------------------------------------------------
// Error
// ---------------------------------------------------------------------------

// ErrorResponse is a generic API error envelope.
type ErrorResponse struct {
	Error string `json:"error"`
}
