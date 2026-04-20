package types

// BranchAutoMergeEnabled is emitted when auto-merge is enabled on a branch.
type BranchAutoMergeEnabled struct {
	Repo      string `json:"repo"`
	Branch    string `json:"branch"`
	EnabledBy string `json:"enabled_by"`
}

func (e BranchAutoMergeEnabled) Type() string   { return "com.docstore.branch.automerge.enabled" }
func (e BranchAutoMergeEnabled) Source() string { return "/repos/" + e.Repo }
func (e BranchAutoMergeEnabled) Data() any      { return e }

// BranchAutoMergeDisabled is emitted when auto-merge is disabled on a branch.
type BranchAutoMergeDisabled struct {
	Repo       string `json:"repo"`
	Branch     string `json:"branch"`
	DisabledBy string `json:"disabled_by"`
}

func (e BranchAutoMergeDisabled) Type() string   { return "com.docstore.branch.automerge.disabled" }
func (e BranchAutoMergeDisabled) Source() string { return "/repos/" + e.Repo }
func (e BranchAutoMergeDisabled) Data() any      { return e }

// BranchAutoMergeFailed is emitted when auto-merge fails (e.g. due to a conflict).
// The auto_merge flag is cleared on the branch when this event is emitted.
type BranchAutoMergeFailed struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Reason string `json:"reason"`
}

func (e BranchAutoMergeFailed) Type() string   { return "com.docstore.branch.automerge.failed" }
func (e BranchAutoMergeFailed) Source() string { return "/repos/" + e.Repo }
func (e BranchAutoMergeFailed) Data() any      { return e }
