package model

import "time"

// Document stores an immutable file version.
type Document struct {
	VersionID   string    `json:"version_id"`
	Path        string    `json:"path"`
	Content     []byte    `json:"-"`
	ContentHash string    `json:"content_hash"`
	CreatedAt   time.Time `json:"created_at"`
	CreatedBy   string    `json:"created_by"`
}

// FileCommit is the core event log entry. One row per file change.
type FileCommit struct {
	CommitID  string    `json:"commit_id"`
	Sequence  int64     `json:"sequence"`
	Path      string    `json:"path"`
	VersionID *string   `json:"version_id"` // nil = delete
	Branch    string    `json:"branch"`
	Message   string    `json:"message"`
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"created_at"`
}

// Branch is a named pointer.
type Branch struct {
	Name         string `json:"name"`
	HeadSequence int64  `json:"head_sequence"`
	BaseSequence int64  `json:"base_sequence"`
	Status       string `json:"status"` // active, merged, abandoned
}

// Role maps an identity to a permission level.
type Role struct {
	Identity string `json:"identity"`
	Role     string `json:"role"` // reader, writer, maintainer, admin
}

// Review records an approval or rejection scoped to a branch at a sequence.
type Review struct {
	ID        string    `json:"id"`
	Branch    string    `json:"branch"`
	Reviewer  string    `json:"reviewer"`
	Sequence  int64     `json:"sequence"`
	Status    string    `json:"status"` // approved, rejected, dismissed
	CreatedAt time.Time `json:"created_at"`
}

// CheckRun stores external CI status for a branch at a sequence.
type CheckRun struct {
	ID        string    `json:"id"`
	Branch    string    `json:"branch"`
	Sequence  int64     `json:"sequence"`
	CheckName string    `json:"check_name"`
	Status    string    `json:"status"` // pending, passed, failed
	Reporter  string    `json:"reporter"`
	CreatedAt time.Time `json:"created_at"`
}
