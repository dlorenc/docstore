package types

// ProposalOpened is emitted when a proposal is opened.
type ProposalOpened struct {
	Repo       string `json:"repo"`
	Branch     string `json:"branch"`
	BaseBranch string `json:"base_branch"`
	ProposalID string `json:"proposal_id"`
	Author     string `json:"author"`
}

func (e ProposalOpened) Type() string   { return "com.docstore.proposal.opened" }
func (e ProposalOpened) Source() string { return "/repos/" + e.Repo }
func (e ProposalOpened) Data() any      { return e }

// ProposalClosed is emitted when a proposal is closed.
type ProposalClosed struct {
	Repo       string `json:"repo"`
	Branch     string `json:"branch"`
	ProposalID string `json:"proposal_id"`
}

func (e ProposalClosed) Type() string   { return "com.docstore.proposal.closed" }
func (e ProposalClosed) Source() string { return "/repos/" + e.Repo }
func (e ProposalClosed) Data() any      { return e }

// ProposalMerged is emitted when a proposal transitions to merged state.
type ProposalMerged struct {
	Repo       string `json:"repo"`
	Branch     string `json:"branch"`
	BaseBranch string `json:"base_branch"`
	ProposalID string `json:"proposal_id"`
}

func (e ProposalMerged) Type() string   { return "com.docstore.proposal.merged" }
func (e ProposalMerged) Source() string { return "/repos/" + e.Repo }
func (e ProposalMerged) Data() any      { return e }
