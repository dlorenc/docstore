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
	Changed   []DiffEntry     `json:"changed"`
	Conflicts []ConflictEntry `json:"conflicts,omitempty"`
}

// ---------------------------------------------------------------------------
// GET /commit/:sequence
// ---------------------------------------------------------------------------

// CommitDetail represents one file change within a commit.
type CommitDetail struct {
	Path      string  `json:"path"`
	VersionID *string `json:"version_id"` // nil means delete
}

// CommitResponse is the response for GET /commit/:sequence.
type CommitResponse struct {
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

// CommitFileInput is one file in a commit request.
type CommitFileInput struct {
	Path    string `json:"path"`
	Content []byte `json:"content"`
}

// CommitRequest is the body for POST /commit.
type CommitRequest struct {
	Branch  string            `json:"branch"`
	Files   []CommitFileInput `json:"files"`
	Message string            `json:"message"`
}

// CommitResult is the response for POST /commit.
type CommitResult struct {
	Sequence int64 `json:"sequence"`
}

// ---------------------------------------------------------------------------
// POST /branch
// ---------------------------------------------------------------------------

// CreateBranchRequest is the body for POST /branch.
type CreateBranchRequest struct {
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

// MergeRequest is the body for POST /merge.
type MergeRequest struct {
	Branch string `json:"branch"`
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

// RebaseRequest is the body for POST /rebase.
type RebaseRequest struct {
	Branch string `json:"branch"`
}

// RebaseResponse is the response for a successful POST /rebase.
type RebaseResponse struct {
	NewBaseSequence int64 `json:"new_base_sequence"`
	NewHeadSequence int64 `json:"new_head_sequence"`
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
// Error
// ---------------------------------------------------------------------------

// ErrorResponse is a generic API error envelope.
type ErrorResponse struct {
	Error string `json:"error"`
}
