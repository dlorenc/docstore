package ciconfig

// TriggerContext holds runtime information about what triggered a CI run.
// It is used to evaluate job-level if: expressions.
type TriggerContext struct {
	Type       string // "push", "proposal", "proposal_synchronized", "manual", "schedule"
	Branch     string // branch being tested
	BaseBranch string // base branch (proposals only; empty for push/manual)
	ProposalID string // proposal ID (proposals only; empty otherwise)
}
