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

// OrgMemberAdded is emitted when a member is added to an org.
type OrgMemberAdded struct {
	Org      string `json:"org"`
	Identity string `json:"identity"`
	Role     string `json:"role"`
	AddedBy  string `json:"added_by"`
}

func (e OrgMemberAdded) Type() string   { return "com.docstore.org.member.added" }
func (e OrgMemberAdded) Source() string { return "/orgs/" + e.Org }
func (e OrgMemberAdded) Data() any      { return e }

// OrgMemberRemoved is emitted when a member is removed from an org.
type OrgMemberRemoved struct {
	Org       string `json:"org"`
	Identity  string `json:"identity"`
	RemovedBy string `json:"removed_by"`
}

func (e OrgMemberRemoved) Type() string   { return "com.docstore.org.member.removed" }
func (e OrgMemberRemoved) Source() string { return "/orgs/" + e.Org }
func (e OrgMemberRemoved) Data() any      { return e }
