// Package api defines the request, response, and domain types that cross the
// HTTP wire between the docstore server and its clients.
//
// This package is intentionally dependency-free (only the standard library)
// so it can be imported by external SDK modules without pulling in server
// internals. Types here are the canonical JSON schema for the docstore API;
// the server and the in-tree CLI consume them via re-export aliases in
// internal/model, and external SDKs (e.g. sdk/go/docstore) import this
// package directly.
package api

import (
	"encoding/json"
	"time"
)

// ---------------------------------------------------------------------------
// Enumerations
// ---------------------------------------------------------------------------

// OrgRole represents the role of an org member.
type OrgRole string

const (
	OrgRoleOwner  OrgRole = "owner"
	OrgRoleMember OrgRole = "member"
)

// BranchStatus represents the lifecycle state of a branch.
type BranchStatus string

const (
	BranchStatusActive    BranchStatus = "active"
	BranchStatusMerged    BranchStatus = "merged"
	BranchStatusAbandoned BranchStatus = "abandoned"
)

// RoleType represents coarse-grained permission levels on a repo.
type RoleType string

const (
	RoleReader     RoleType = "reader"
	RoleWriter     RoleType = "writer"
	RoleMaintainer RoleType = "maintainer"
	RoleAdmin      RoleType = "admin"
)

// ReviewStatus represents the outcome of a review.
type ReviewStatus string

const (
	ReviewApproved  ReviewStatus = "approved"
	ReviewRejected  ReviewStatus = "rejected"
	ReviewDismissed ReviewStatus = "dismissed"
)

// ProposalState represents the lifecycle state of a proposal.
type ProposalState string

const (
	ProposalOpen   ProposalState = "open"
	ProposalClosed ProposalState = "closed"
	ProposalMerged ProposalState = "merged"
)

// CheckRunStatus represents the state of a CI check run.
type CheckRunStatus string

const (
	CheckRunPending CheckRunStatus = "pending"
	CheckRunPassed  CheckRunStatus = "passed"
	CheckRunFailed  CheckRunStatus = "failed"
)

// ---------------------------------------------------------------------------
// Domain entities
// ---------------------------------------------------------------------------

// Org is a top-level namespace that owns one or more repos.
type Org struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
}

// OrgMember is a member of an org with a specific role.
type OrgMember struct {
	Org       string    `json:"org"`
	Identity  string    `json:"identity"`
	Role      OrgRole   `json:"role"`
	InvitedBy string    `json:"invited_by"`
	CreatedAt time.Time `json:"created_at"`
}

// OrgInvite is a pending or accepted invitation to join an org.
type OrgInvite struct {
	ID         string     `json:"id"`
	Org        string     `json:"org"`
	Email      string     `json:"email"`
	Role       OrgRole    `json:"role"`
	InvitedBy  string     `json:"invited_by"`
	Token      string     `json:"token,omitempty"`
	ExpiresAt  time.Time  `json:"expires_at"`
	AcceptedAt *time.Time `json:"accepted_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// Repo is a named tenant that owns its own isolated set of branches and commits.
// Name is the full path (e.g. "acme/myrepo" or "acme/team/subrepo").
// Owner is the first path segment, i.e. the org name.
type Repo struct {
	Name      string    `json:"name"`
	Owner     string    `json:"owner"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
}

// Branch is a named pointer to a sequence.
type Branch struct {
	Name         string       `json:"name"`
	HeadSequence int64        `json:"head_sequence"`
	BaseSequence int64        `json:"base_sequence"`
	Status       BranchStatus `json:"status"`
	Draft        bool         `json:"draft"`
}

// Role maps an identity to a coarse-grained permission level on a repo.
type Role struct {
	Identity string   `json:"identity"`
	Role     RoleType `json:"role"`
}

// Review records an approval or rejection scoped to a branch at a specific
// head sequence.
type Review struct {
	ID        string       `json:"id"`
	Branch    string       `json:"branch"`
	Reviewer  string       `json:"reviewer"`
	Sequence  int64        `json:"sequence"`
	Status    ReviewStatus `json:"status"`
	Body      string       `json:"body,omitempty"`
	CreatedAt time.Time    `json:"created_at"`
}

// CheckRun stores an external CI status report for a branch at a specific
// head sequence.
type CheckRun struct {
	ID        string         `json:"id"`
	Branch    string         `json:"branch"`
	Sequence  int64          `json:"sequence"`
	CheckName string         `json:"check_name"`
	Status    CheckRunStatus `json:"status"`
	Reporter  string         `json:"reporter"`
	LogURL    *string        `json:"log_url,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

// Proposal is a request to merge a branch into a base branch.
type Proposal struct {
	ID          string        `json:"id"`
	Repo        string        `json:"repo"`
	Branch      string        `json:"branch"`
	BaseBranch  string        `json:"base_branch"`
	Title       string        `json:"title"`
	Description string        `json:"description,omitempty"`
	Author      string        `json:"author"`
	State       ProposalState `json:"state"`
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
}

// EventSubscription is a webhook or Pub/Sub delivery target for events.
type EventSubscription struct {
	ID           string          `json:"id"`
	Repo         *string         `json:"repo,omitempty"`
	EventTypes   []string        `json:"event_types,omitempty"`
	Backend      string          `json:"backend"`
	Config       json.RawMessage `json:"config"`
	CreatedAt    time.Time       `json:"created_at"`
	CreatedBy    string          `json:"created_by"`
	SuspendedAt  *time.Time      `json:"suspended_at,omitempty"`
	FailureCount int             `json:"failure_count"`
}

// Release is a named immutable snapshot tied to a commit sequence.
type Release struct {
	ID        string    `json:"id"`
	Repo      string    `json:"repo"`
	Name      string    `json:"name"`
	Sequence  int64     `json:"sequence"`
	Body      string    `json:"body,omitempty"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

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

// FileChange is one file in a commit request. A nil Content means delete.
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
// POST /branch, PATCH /branch/:name, DELETE /branch/:name
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

// DeleteBranchResponse is the response for DELETE /branch/:name.
type DeleteBranchResponse struct {
	Name   string       `json:"name"`
	Status BranchStatus `json:"status"`
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
// POST /comment, GET /branch/:branch/comments, DELETE /comment/:id
// ---------------------------------------------------------------------------

// ReviewComment is an inline file annotation on a branch.
// version_id is required; comments on deleted files are not supported.
// review_id is optional; comments may exist independently of a formal review.
type ReviewComment struct {
	ID        string    `json:"id"`
	ReviewID  *string   `json:"review_id,omitempty"`
	Branch    string    `json:"branch"`
	Path      string    `json:"path"`
	VersionID string    `json:"version_id"`
	Body      string    `json:"body"`
	Author    string    `json:"author"`
	Sequence  int64     `json:"sequence"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateReviewCommentRequest is the body for POST /comment.
type CreateReviewCommentRequest struct {
	Branch    string  `json:"branch"`
	Path      string  `json:"path"`
	VersionID string  `json:"version_id"`
	Body      string  `json:"body"`
	ReviewID  *string `json:"review_id,omitempty"`
}

// CreateReviewCommentResponse is the response for POST /comment.
type CreateReviewCommentResponse struct {
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
	Sequence  *int64         `json:"sequence,omitempty"`
}

// CreateCheckRunResponse is the response for POST /check.
type CreateCheckRunResponse struct {
	ID       string  `json:"id"`
	Sequence int64   `json:"sequence"`
	LogURL   *string `json:"log_url,omitempty"`
}

// ---------------------------------------------------------------------------
// Orgs
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

// AddOrgMemberRequest is the body for POST /orgs/{org}/members/{identity}.
type AddOrgMemberRequest struct {
	Role OrgRole `json:"role"`
}

// OrgMembersResponse is the response for GET /orgs/{org}/members.
type OrgMembersResponse struct {
	Members []OrgMember `json:"members"`
}

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
// Repos
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
// Roles
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
// Proposals
// ---------------------------------------------------------------------------

// CreateProposalRequest is the body for POST /repos/:name/proposals.
type CreateProposalRequest struct {
	Branch      string `json:"branch"`
	BaseBranch  string `json:"base_branch"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
}

// CreateProposalResponse is the response for POST /repos/:name/proposals.
type CreateProposalResponse struct {
	ID string `json:"id"`
}

// UpdateProposalRequest is the body for PATCH /repos/:name/proposals/:proposalID.
type UpdateProposalRequest struct {
	Title       *string `json:"title,omitempty"`
	Description *string `json:"description,omitempty"`
}

// ---------------------------------------------------------------------------
// Purge
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
// Releases
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
// Commit chain
// ---------------------------------------------------------------------------

// ChainEntry is one commit in the hash-chain response from GET /chain.
type ChainEntry struct {
	Sequence   int64       `json:"sequence"`
	Branch     string      `json:"branch"`
	Author     string      `json:"author"`
	Message    string      `json:"message"`
	CreatedAt  time.Time   `json:"created_at"`
	CommitHash *string     `json:"commit_hash"`
	Files      []ChainFile `json:"files"`
}

// ChainFile is one file change within a ChainEntry.
type ChainFile struct {
	Path        string `json:"path"`
	ContentHash string `json:"content_hash"`
}

// ---------------------------------------------------------------------------
// Event subscriptions
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
// Issues
// ---------------------------------------------------------------------------

// IssueState represents the lifecycle state of an issue.
type IssueState string

const (
	IssueStateOpen   IssueState = "open"
	IssueStateClosed IssueState = "closed"
)

// IssueCloseReason represents why an issue was closed.
type IssueCloseReason string

const (
	IssueCloseReasonCompleted  IssueCloseReason = "completed"
	IssueCloseReasonNotPlanned IssueCloseReason = "not_planned"
	IssueCloseReasonDuplicate  IssueCloseReason = "duplicate"
)

// IssueRefType represents the kind of cross-reference attached to an issue.
type IssueRefType string

const (
	IssueRefTypeProposal IssueRefType = "proposal"
	IssueRefTypeCommit   IssueRefType = "commit"
)

// Issue is a repo-scoped bug report or feature request.
type Issue struct {
	ID          string            `json:"id"`
	Repo        string            `json:"repo"`
	Number      int64             `json:"number"`
	Title       string            `json:"title"`
	Body        string            `json:"body,omitempty"`
	Author      string            `json:"author"`
	State       IssueState        `json:"state"`
	CloseReason *IssueCloseReason `json:"close_reason,omitempty"`
	ClosedBy    *string           `json:"closed_by,omitempty"`
	Labels      []string          `json:"labels"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// IssueComment is a comment on an issue.
type IssueComment struct {
	ID        string     `json:"id"`
	IssueID   string     `json:"issue_id"`
	Repo      string     `json:"repo"`
	Body      string     `json:"body"`
	Author    string     `json:"author"`
	CreatedAt time.Time  `json:"created_at"`
	EditedAt  *time.Time `json:"edited_at,omitempty"`
}

// IssueRef is a cross-reference from an issue to a proposal or commit.
type IssueRef struct {
	ID        string       `json:"id"`
	IssueID   string       `json:"issue_id"`
	Repo      string       `json:"repo"`
	RefType   IssueRefType `json:"ref_type"`
	RefID     string       `json:"ref_id"`
	CreatedAt time.Time    `json:"created_at"`
}

// CreateIssueRequest is the body for POST /repos/:name/issues.
type CreateIssueRequest struct {
	Title  string   `json:"title"`
	Body   string   `json:"body,omitempty"`
	Labels []string `json:"labels,omitempty"`
}

// CreateIssueResponse is the response for POST /repos/:name/issues.
type CreateIssueResponse struct {
	ID     string `json:"id"`
	Number int64  `json:"number"`
}

// UpdateIssueRequest is the body for PATCH /repos/:name/issues/:number.
type UpdateIssueRequest struct {
	Title  *string  `json:"title,omitempty"`
	Body   *string  `json:"body,omitempty"`
	Labels []string `json:"labels,omitempty"`
}

// CloseIssueRequest is the body for POST /repos/:name/issues/:number/close.
type CloseIssueRequest struct {
	Reason IssueCloseReason `json:"reason"`
}

// ReopenIssueRequest is the body for POST /repos/:name/issues/:number/reopen.
// Currently empty but defined for forward compatibility.
type ReopenIssueRequest struct{}

// ListIssuesResponse is the response for GET /repos/:name/issues.
type ListIssuesResponse struct {
	Issues []Issue `json:"issues"`
}

// IssueResponse is the response for GET /repos/:name/issues/:number.
type IssueResponse = Issue

// CreateIssueCommentRequest is the body for POST /repos/:name/issues/:number/comments.
type CreateIssueCommentRequest struct {
	Body string `json:"body"`
}

// CreateIssueCommentResponse is the response for POST /repos/:name/issues/:number/comments.
type CreateIssueCommentResponse struct {
	ID string `json:"id"`
}

// UpdateIssueCommentRequest is the body for PATCH /repos/:name/issues/comments/:id.
type UpdateIssueCommentRequest struct {
	Body string `json:"body"`
}

// ListIssueCommentsResponse is the response for GET /repos/:name/issues/:number/comments.
type ListIssueCommentsResponse struct {
	Comments []IssueComment `json:"comments"`
}

// AddIssueRefRequest is the body for POST /repos/:name/issues/:number/refs.
type AddIssueRefRequest struct {
	RefType IssueRefType `json:"ref_type"`
	RefID   string       `json:"ref_id"`
}

// AddIssueRefResponse is the response for POST /repos/:name/issues/:number/refs.
type AddIssueRefResponse struct {
	ID string `json:"id"`
}

// ListIssueRefsResponse is the response for GET /repos/:name/issues/:number/refs.
type ListIssueRefsResponse struct {
	Refs []IssueRef `json:"refs"`
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// ErrorResponse is the generic API error envelope.
type ErrorResponse struct {
	Error string `json:"error"`
}
