package types

// OrgCreated is emitted when a new org is created.
type OrgCreated struct {
	Org       string `json:"org"`
	CreatedBy string `json:"created_by"`
}

func (e OrgCreated) Type() string   { return "com.docstore.org.created" }
func (e OrgCreated) Source() string { return "/orgs/" + e.Org }
func (e OrgCreated) Data() any      { return e }

// OrgDeleted is emitted when an org is deleted.
type OrgDeleted struct {
	Org       string `json:"org"`
	DeletedBy string `json:"deleted_by"`
}

func (e OrgDeleted) Type() string   { return "com.docstore.org.deleted" }
func (e OrgDeleted) Source() string { return "/orgs/" + e.Org }
func (e OrgDeleted) Data() any      { return e }
