package types

// ReviewSubmitted is emitted when a review is submitted.
type ReviewSubmitted struct {
	Repo     string `json:"repo"`
	Branch   string `json:"branch"`
	Sequence int64  `json:"sequence"`
	Reviewer string `json:"reviewer"`
	Status   string `json:"status"`
}

func (e ReviewSubmitted) Type() string   { return "com.docstore.review.submitted" }
func (e ReviewSubmitted) Source() string { return "/repos/" + e.Repo }
func (e ReviewSubmitted) Data() any      { return e }
