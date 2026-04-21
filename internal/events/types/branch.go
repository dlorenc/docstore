package types

import "github.com/dlorenc/docstore/api"

// BranchCreated is emitted when a new branch is created.
type BranchCreated struct {
	Repo         string `json:"repo"`
	Branch       string `json:"branch"`
	BaseSequence int64  `json:"base_sequence"`
	CreatedBy    string `json:"created_by"`
}

func (e BranchCreated) Type() string   { return "com.docstore.branch.created" }
func (e BranchCreated) Source() string { return "/repos/" + e.Repo }
func (e BranchCreated) Data() any      { return e }

// BranchMerged is emitted when a branch is merged into main.
type BranchMerged struct {
	Repo         string                    `json:"repo"`
	Branch       string                    `json:"branch"`
	Sequence     int64                     `json:"sequence"`
	MergedBy     string                    `json:"merged_by"`
	BranchStatus *api.BranchStatusResponse `json:"branch_status,omitempty"`
}

func (e BranchMerged) Type() string   { return "com.docstore.branch.merged" }
func (e BranchMerged) Source() string { return "/repos/" + e.Repo }
func (e BranchMerged) Data() any      { return e }

// BranchRebased is emitted when a branch is rebased onto main.
type BranchRebased struct {
	Repo            string                    `json:"repo"`
	Branch          string                    `json:"branch"`
	NewBaseSequence int64                     `json:"new_base_sequence"`
	NewHeadSequence int64                     `json:"new_head_sequence"`
	CommitsReplayed int64                     `json:"commits_replayed"`
	RebasedBy       string                    `json:"rebased_by"`
	BranchStatus    *api.BranchStatusResponse `json:"branch_status,omitempty"`
}

func (e BranchRebased) Type() string   { return "com.docstore.branch.rebased" }
func (e BranchRebased) Source() string { return "/repos/" + e.Repo }
func (e BranchRebased) Data() any      { return e }

// BranchAbandoned is emitted when a branch is deleted (abandoned).
type BranchAbandoned struct {
	Repo        string `json:"repo"`
	Branch      string `json:"branch"`
	AbandonedBy string `json:"abandoned_by"`
}

func (e BranchAbandoned) Type() string   { return "com.docstore.branch.abandoned" }
func (e BranchAbandoned) Source() string { return "/repos/" + e.Repo }
func (e BranchAbandoned) Data() any      { return e }
