package types

import "github.com/dlorenc/docstore/api"

// CheckReported is emitted when a CI check run is reported.
type CheckReported struct {
	Repo         string                    `json:"repo"`
	Branch       string                    `json:"branch"`
	Sequence     int64                     `json:"sequence"`
	CheckName    string                    `json:"check_name"`
	Status       string                    `json:"status"`
	Reporter     string                    `json:"reporter"`
	BranchStatus *api.BranchStatusResponse `json:"branch_status,omitempty"`
}

func (e CheckReported) Type() string   { return "com.docstore.check.reported" }
func (e CheckReported) Source() string { return "/repos/" + e.Repo }
func (e CheckReported) Data() any      { return e }
