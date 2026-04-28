package types

// ReleaseCreated is emitted when a new release is created.
type ReleaseCreated struct {
	Repo      string `json:"repo"`
	Name      string `json:"name"`
	Sequence  int64  `json:"sequence"`
	CreatedBy string `json:"created_by"`
}

func (e ReleaseCreated) Type() string   { return "com.docstore.release.created" }
func (e ReleaseCreated) Source() string { return "/repos/" + e.Repo }
func (e ReleaseCreated) Data() any      { return e }

// ReleaseDeleted is emitted when a release is deleted.
type ReleaseDeleted struct {
	Repo      string `json:"repo"`
	Name      string `json:"name"`
	DeletedBy string `json:"deleted_by"`
}

func (e ReleaseDeleted) Type() string   { return "com.docstore.release.deleted" }
func (e ReleaseDeleted) Source() string { return "/repos/" + e.Repo }
func (e ReleaseDeleted) Data() any      { return e }
