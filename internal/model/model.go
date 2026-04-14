// Package model defines the data types for all six database tables
// described in DESIGN.md.
package model

import (
	"crypto/rand"
	"errors"
	"fmt"
	"time"
)

// Sentinel errors for store operations.
var (
	ErrBranchExists    = errors.New("branch already exists")
	ErrBranchNotFound  = errors.New("branch not found")
	ErrBranchNotActive = errors.New("branch is not active")
	ErrProtectedBranch = errors.New("cannot modify protected branch")
)

// NewUUID generates a random UUID v4.
func NewUUID() string {
	var u [16]byte
	if _, err := rand.Read(u[:]); err != nil {
		panic(err)
	}
	u[6] = (u[6] & 0x0f) | 0x40 // version 4
	u[8] = (u[8] & 0x3f) | 0x80 // variant
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// BranchStatus represents the lifecycle state of a branch.
type BranchStatus string

const (
	BranchStatusActive    BranchStatus = "active"
	BranchStatusMerged    BranchStatus = "merged"
	BranchStatusAbandoned BranchStatus = "abandoned"
)

// RoleType represents coarse-grained permission levels.
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

// CheckRunStatus represents the state of a CI check run.
type CheckRunStatus string

const (
	CheckRunPending CheckRunStatus = "pending"
	CheckRunPassed  CheckRunStatus = "passed"
	CheckRunFailed  CheckRunStatus = "failed"
)

// Document stores an immutable file version. Every save creates a new row.
type Document struct {
	VersionID   string    `json:"version_id"`
	Path        string    `json:"path"`
	Content     []byte    `json:"content"`
	ContentHash string    `json:"content_hash"`
	CreatedAt   time.Time `json:"created_at"`
	CreatedBy   string    `json:"created_by"`
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

// Branch is a named pointer to a sequence.
type Branch struct {
	Name         string       `json:"name"`
	HeadSequence int64        `json:"head_sequence"`
	BaseSequence int64        `json:"base_sequence"`
	Status       BranchStatus `json:"status"`
}

// Role maps an identity to a coarse-grained permission level.
type Role struct {
	Identity string   `json:"identity"`
	Role     RoleType `json:"role"`
}

// Review records an approval or rejection scoped to a branch at a
// specific head sequence.
type Review struct {
	ID        string       `json:"id"`
	Branch    string       `json:"branch"`
	Reviewer  string       `json:"reviewer"`
	Sequence  int64        `json:"sequence"`
	Status    ReviewStatus `json:"status"`
	CreatedAt time.Time    `json:"created_at"`
}

// CheckRun stores an external CI status report for a branch at a
// specific head sequence.
type CheckRun struct {
	ID        string         `json:"id"`
	Branch    string         `json:"branch"`
	Sequence  int64          `json:"sequence"`
	CheckName string         `json:"check_name"`
	Status    CheckRunStatus `json:"status"`
	Reporter  string         `json:"reporter"`
	CreatedAt time.Time      `json:"created_at"`
}

