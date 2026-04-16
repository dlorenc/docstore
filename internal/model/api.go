package model

import (
	"encoding/json"
	"time"
)

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
	ContentType string `json:"content_type,omitempty"`
}

// ---------------------------------------------------------------------------
// GET /diff
// ---------------------------------------------------------------------------

// DiffEntry represents a single file change in a diff.
type DiffEntry struct {
	Path      string  `json:"path"`
	VersionID *string `json:"version_id"` // nil means deleted
	Binary    bool    `json:"binary,omitempty"`
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
	Path        string `json:"path"`
	Content     []byte `json:"content,omitempty"`
	ContentType string `json:"content_type,omitempty"`
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
	Sequence   int64              `json:"sequence"`
	Files      []CommitFileResult `json:"files"`
	CommitHash string             `json:"commit_hash,omitempty"`
}

// ---------------------------------------------------------------------------
// POST /branch
// ---------------------------------------------------------------------------

// CreateBranchRequest is the body for POST /repos/:name/branch.
type CreateBranchRequest struct {
	Repo  string `json:"repo,omitempty"`
	Name  string `json:"name"`
	Draft bool   `json:"draft,omitempty"`
}

// CreateBranchResponse is the response for POST /branch.
type CreateBranchResponse struct {
	Name         string `json:"name"`
	BaseSequence int64  `json:"base_sequence"`
	Draft        bool   `json:"draft,omitempty"`
}

// UpdateBranchRequest is the body for PATCH /repos/:name/branch/:name.
type UpdateBranchRequest struct {
	Draft bool `json:"draft"`
}

// UpdateBranchResponse is the response for PATCH /repos/:name/branch/:name.
type UpdateBranchResponse struct {
	Name  string `json:"name"`
	Draft bool   `json:"draft"`
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
	LogURL    *string        `json:"log_url,omitempty"`
}

// CreateCheckRunResponse is the response for POST /check.
type CreateCheckRunResponse struct {
	ID       string  `json:"id"`
	Sequence int64   `json:"sequence"`
	LogURL   *string `json:"log_url,omitempty"`
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
// POST /orgs / GET /orgs / GET /orgs/:name / DELETE /orgs/:name
// ---------------------------------------------------------------------------

// CreateOrgRequest is the body for POST /orgs.
type CreateOrgRequest struct {
	Name string `json:"name"`
}

// CreateOrgResponse is the response for POST /orgs (same fields as Org).
type CreateOrgResponse = Org

// ListOrgsResponse is the response for GET /orgs.
type ListOrgsResponse struct {
	Orgs []Org `json:"orgs"`
}

// ---------------------------------------------------------------------------
// POST /repos / GET /repos / GET /repos/:name / DELETE /repos/:name
// ---------------------------------------------------------------------------

// CreateRepoRequest is the body for POST /repos.
// Owner is the org name; Name is the repo path within the org (may contain slashes).
// The full repo identifier is Owner + "/" + Name.
type CreateRepoRequest struct {
	Owner     string `json:"owner"`
	Name      string `json:"name"`
	CreatedBy string `json:"created_by,omitempty"`
}

// FullName returns the full repo path: owner + "/" + name.
func (r CreateRepoRequest) FullName() string { return r.Owner + "/" + r.Name }

// ReposResponse is the response for GET /repos.
type ReposResponse struct {
	Repos []Repo `json:"repos"`
}

// ---------------------------------------------------------------------------
// GET /repos/:name/roles / PUT /repos/:name/roles/:identity / DELETE /repos/:name/roles/:identity
// ---------------------------------------------------------------------------

// SetRoleRequest is the body for PUT /repos/:name/roles/:identity.
type SetRoleRequest struct {
	Role RoleType `json:"role"`
}

// RolesResponse is the response for GET /repos/:name/roles.
type RolesResponse struct {
	Roles []Role `json:"roles"`
}

// ---------------------------------------------------------------------------
// POST /repos/:name/purge
// ---------------------------------------------------------------------------

// PurgeRequest is the body for POST /repos/:name/purge.
type PurgeRequest struct {
	OlderThan string `json:"older_than"`
	DryRun    bool   `json:"dry_run,omitempty"`
}

// PurgeResponse is the response for POST /repos/:name/purge.
type PurgeResponse struct {
	BranchesPurged     int64 `json:"branches_purged"`
	FileCommitsDeleted int64 `json:"file_commits_deleted"`
	CommitsDeleted     int64 `json:"commits_deleted"`
	DocumentsDeleted   int64 `json:"documents_deleted"`
	ReviewsDeleted     int64 `json:"reviews_deleted"`
	CheckRunsDeleted   int64 `json:"check_runs_deleted"`
}

// ---------------------------------------------------------------------------
// POST /orgs/{org}/members/{identity} / DELETE /orgs/{org}/members/{identity} / GET /orgs/{org}/members
// ---------------------------------------------------------------------------

// AddOrgMemberRequest is the body for POST /orgs/{org}/members/{identity}.
type AddOrgMemberRequest struct {
	Role OrgRole `json:"role"`
}

// OrgMembersResponse is the response for GET /orgs/{org}/members.
type OrgMembersResponse struct {
	Members []OrgMember `json:"members"`
}

// ---------------------------------------------------------------------------
// POST /orgs/{org}/invites / GET /orgs/{org}/invites / DELETE /orgs/{org}/invites/{id}
// ---------------------------------------------------------------------------

// CreateInviteRequest is the body for POST /orgs/{org}/invites.
type CreateInviteRequest struct {
	Email string  `json:"email"`
	Role  OrgRole `json:"role"`
}

// CreateInviteResponse is the response for POST /orgs/{org}/invites.
type CreateInviteResponse struct {
	ID    string `json:"id"`
	Token string `json:"token"`
}

// OrgInvitesResponse is the response for GET /orgs/{org}/invites.
type OrgInvitesResponse struct {
	Invites []OrgInvite `json:"invites"`
}

// ---------------------------------------------------------------------------
// POST /repos/{owner}/{name}/-/releases
// GET  /repos/{owner}/{name}/-/releases
// GET  /repos/{owner}/{name}/-/releases/{name}
// DELETE /repos/{owner}/{name}/-/releases/{name}
// ---------------------------------------------------------------------------

// CreateReleaseRequest is the body for POST /repos/:name/-/releases.
// Sequence defaults to the current main head if omitted.
type CreateReleaseRequest struct {
	Name     string `json:"name"`
	Sequence *int64 `json:"sequence,omitempty"`
	Body     string `json:"body,omitempty"`
}

// ListReleasesResponse is the response for GET /repos/:name/-/releases.
type ListReleasesResponse struct {
	Releases []Release `json:"releases"`
}

// ---------------------------------------------------------------------------
// POST /subscriptions / GET /subscriptions / DELETE /subscriptions/{id}
// POST /subscriptions/{id}/resume
// ---------------------------------------------------------------------------

// CreateSubscriptionRequest is the body for POST /subscriptions.
type CreateSubscriptionRequest struct {
	// Repo is the repo to subscribe to (optional; nil means all repos).
	Repo *string `json:"repo,omitempty"`
	// EventTypes is the list of event types to subscribe to (optional; nil means all types).
	EventTypes []string `json:"event_types,omitempty"`
	// Backend must be "webhook" for Milestone 1.
	Backend string `json:"backend"`
	// Config is backend-specific: {"url":"https://...","secret":"..."}
	Config json.RawMessage `json:"config"`
	// CreatedBy is populated by the server from the IAP identity; not set by clients.
	CreatedBy string `json:"-"`
}

// ListSubscriptionsResponse is the response for GET /subscriptions.
type ListSubscriptionsResponse struct {
	Subscriptions []EventSubscription `json:"subscriptions"`
}

// ---------------------------------------------------------------------------
// Error
// ---------------------------------------------------------------------------

// ErrorResponse is a generic API error envelope.
type ErrorResponse struct {
	Error string `json:"error"`
}
