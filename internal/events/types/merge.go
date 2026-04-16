package types

// MergeBlocked is emitted when a merge attempt is denied by policy.
type MergeBlocked struct {
	Repo     string   `json:"repo"`
	Branch   string   `json:"branch"`
	Actor    string   `json:"actor"`
	Policies []string `json:"policies"`
}

func (e MergeBlocked) Type() string   { return "com.docstore.merge.blocked" }
func (e MergeBlocked) Source() string { return "/repos/" + e.Repo }
func (e MergeBlocked) Data() any      { return e }
