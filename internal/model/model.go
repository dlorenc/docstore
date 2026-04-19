// Package model defines data types for the docstore server.
//
// Wire-facing types (anything that crosses the HTTP boundary) live in the
// public github.com/dlorenc/docstore/api package and are re-exported here as
// type aliases so existing server/CLI code that imports this package keeps
// working. Internal-only types (storage-layer rows that never appear on the
// wire) remain defined here.
package model

import (
	"time"

	"github.com/dlorenc/docstore/api"
)

// ---------------------------------------------------------------------------
// Wire-type aliases — canonical definitions in github.com/dlorenc/docstore/api.
// ---------------------------------------------------------------------------

type OrgRole = api.OrgRole

const (
	OrgRoleOwner  = api.OrgRoleOwner
	OrgRoleMember = api.OrgRoleMember
)

type OrgMember = api.OrgMember
type OrgInvite = api.OrgInvite
type Org = api.Org
type Repo = api.Repo

type BranchStatus = api.BranchStatus

const (
	BranchStatusActive    = api.BranchStatusActive
	BranchStatusMerged    = api.BranchStatusMerged
	BranchStatusAbandoned = api.BranchStatusAbandoned
)

type RoleType = api.RoleType

const (
	RoleReader     = api.RoleReader
	RoleWriter     = api.RoleWriter
	RoleMaintainer = api.RoleMaintainer
	RoleAdmin      = api.RoleAdmin
)

type ReviewStatus = api.ReviewStatus

const (
	ReviewApproved  = api.ReviewApproved
	ReviewRejected  = api.ReviewRejected
	ReviewDismissed = api.ReviewDismissed
)

type CheckRunStatus = api.CheckRunStatus

const (
	CheckRunPending = api.CheckRunPending
	CheckRunPassed  = api.CheckRunPassed
	CheckRunFailed  = api.CheckRunFailed
)

type Branch = api.Branch
type Role = api.Role
type Review = api.Review
type CheckRun = api.CheckRun
type ReviewComment = api.ReviewComment
type Release = api.Release
type EventSubscription = api.EventSubscription
type Proposal = api.Proposal

type ProposalState = api.ProposalState

const (
	ProposalOpen   = api.ProposalOpen
	ProposalClosed = api.ProposalClosed
	ProposalMerged = api.ProposalMerged
)

// ---------------------------------------------------------------------------
// Internal-only types — not part of the HTTP wire surface.
// ---------------------------------------------------------------------------

// Document stores an immutable file version. Every save creates a new row.
type Document struct {
	VersionID   string    `json:"version_id"`
	Path        string    `json:"path"`
	Content     []byte    `json:"content"`
	ContentHash string    `json:"content_hash"`
	ContentType string    `json:"content_type,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	CreatedBy   string    `json:"created_by"`
}

// CIJob tracks a CI run scheduled via the ci-scheduler webhook.
type CIJob struct {
	ID              string     `json:"id"`
	Repo            string     `json:"repo"`
	Branch          string     `json:"branch"`
	Sequence        int64      `json:"sequence"`
	Status          string     `json:"status"` // queued, claimed, passed, failed
	ClaimedAt       *time.Time `json:"claimed_at,omitempty"`
	LastHeartbeatAt *time.Time `json:"last_heartbeat_at,omitempty"`
	WorkerPod       *string    `json:"worker_pod,omitempty"`
	WorkerPodIP     *string    `json:"worker_pod_ip,omitempty"`
	LogURL          *string    `json:"log_url,omitempty"`
	ErrorMessage    *string    `json:"error_message,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	TriggerType     string     `json:"trigger_type,omitempty"`
	TriggerBranch   string     `json:"trigger_branch,omitempty"`
}

// FileCommit is the core event log. One row per file change.
// All rows sharing a sequence number form one atomic commit.
type FileCommit struct {
	CommitID  string    `json:"commit_id"`
	Sequence  int64     `json:"sequence"`
	Path      string    `json:"path"`
	VersionID *string   `json:"version_id"` // nil means delete
	Branch    string    `json:"branch"`
	Message   string    `json:"message"`
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"created_at"`
}
