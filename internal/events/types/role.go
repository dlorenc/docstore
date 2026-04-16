package types

// RoleChanged is emitted when a role is set or removed.
type RoleChanged struct {
	Repo      string `json:"repo"`
	Identity  string `json:"identity"`
	Role      string `json:"role"`      // empty string means role was removed
	ChangedBy string `json:"changed_by"`
}

func (e RoleChanged) Type() string   { return "com.docstore.role.changed" }
func (e RoleChanged) Source() string { return "/repos/" + e.Repo }
func (e RoleChanged) Data() any      { return e }
