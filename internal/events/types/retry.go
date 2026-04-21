package types

// CheckRunRetry is emitted when a selective check retry is requested.
type CheckRunRetry struct {
	Repo     string   `json:"repo"`
	Branch   string   `json:"branch"`
	Sequence int64    `json:"sequence"`
	Checks   []string `json:"checks"` // empty = retry all failed
	Attempt  int16    `json:"attempt"`
}

func (e CheckRunRetry) Type() string   { return "com.docstore.checkrun.retry" }
func (e CheckRunRetry) Source() string { return "/repos/" + e.Repo }
func (e CheckRunRetry) Data() any      { return e }
