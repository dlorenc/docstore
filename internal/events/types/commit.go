package types

// CommitCreated is emitted after a successful commit.
type CommitCreated struct {
	Repo      string `json:"repo"`
	Branch    string `json:"branch"`
	Sequence  int64  `json:"sequence"`
	Author    string `json:"author"`
	Message   string `json:"message"`
	FileCount int    `json:"file_count"`
}

func (e CommitCreated) Type() string   { return "com.docstore.commit.created" }
func (e CommitCreated) Source() string { return "/repos/" + e.Repo }
func (e CommitCreated) Data() any      { return e }
