package types

// RepoCreated is emitted when a new repo is created.
type RepoCreated struct {
	Repo      string `json:"repo"`
	Owner     string `json:"owner"`
	CreatedBy string `json:"created_by"`
}

func (e RepoCreated) Type() string   { return "com.docstore.repo.created" }
func (e RepoCreated) Source() string { return "/repos/" + e.Repo }
func (e RepoCreated) Data() any      { return e }

// RepoDeleted is emitted when a repo is deleted.
type RepoDeleted struct {
	Repo      string `json:"repo"`
	DeletedBy string `json:"deleted_by"`
}

func (e RepoDeleted) Type() string   { return "com.docstore.repo.deleted" }
func (e RepoDeleted) Source() string { return "/repos/" + e.Repo }
func (e RepoDeleted) Data() any      { return e }
