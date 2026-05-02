package ciconfig

import (
	"slices"
	"testing"
)

func TestMatchesPush(t *testing.T) {
	tests := []struct {
		name   string
		cfg    CIConfig
		branch string
		want   bool
	}{
		{
			name:   "no on block always matches",
			cfg:    CIConfig{On: nil},
			branch: "feature/foo",
			want:   true,
		},
		{
			name:   "no on block matches main",
			cfg:    CIConfig{On: nil},
			branch: "main",
			want:   true,
		},
		{
			name: "on with no push never matches",
			cfg: CIConfig{
				On: &TriggerConfig{Push: nil},
			},
			branch: "main",
			want:   false,
		},
		{
			name: "on push no branches matches all",
			cfg: CIConfig{
				On: &TriggerConfig{
					Push: &PushTrigger{Branches: nil},
				},
			},
			branch: "feature/foo",
			want:   true,
		},
		{
			name: "on push empty branches matches all",
			cfg: CIConfig{
				On: &TriggerConfig{
					Push: &PushTrigger{Branches: []string{}},
				},
			},
			branch: "feature/foo",
			want:   true,
		},
		{
			name: "on push branches main matches main",
			cfg: CIConfig{
				On: &TriggerConfig{
					Push: &PushTrigger{Branches: []string{"main"}},
				},
			},
			branch: "main",
			want:   true,
		},
		{
			name: "on push branches main does not match feature branch",
			cfg: CIConfig{
				On: &TriggerConfig{
					Push: &PushTrigger{Branches: []string{"main"}},
				},
			},
			branch: "feature/foo",
			want:   false,
		},
		{
			name: "wildcard release/* matches release/1.0",
			cfg: CIConfig{
				On: &TriggerConfig{
					Push: &PushTrigger{Branches: []string{"main", "release/*"}},
				},
			},
			branch: "release/1.0",
			want:   true,
		},
		{
			name: "wildcard release/* does not match feature/foo",
			cfg: CIConfig{
				On: &TriggerConfig{
					Push: &PushTrigger{Branches: []string{"main", "release/*"}},
				},
			},
			branch: "feature/foo",
			want:   false,
		},
		{
			name: "double star matches nested branches",
			cfg: CIConfig{
				On: &TriggerConfig{
					Push: &PushTrigger{Branches: []string{"feature/**"}},
				},
			},
			branch: "feature/team/my-feature",
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.MatchesPush(tt.branch)
			if got != tt.want {
				t.Errorf("MatchesPush(%q) = %v, want %v", tt.branch, got, tt.want)
			}
		})
	}
}

func TestMatchesProposal(t *testing.T) {
	tests := []struct {
		name       string
		cfg        CIConfig
		baseBranch string
		want       bool
	}{
		{
			name:       "no on block always matches",
			cfg:        CIConfig{On: nil},
			baseBranch: "main",
			want:       true,
		},
		{
			name: "on with no proposal never matches",
			cfg: CIConfig{
				On: &TriggerConfig{Push: &PushTrigger{}},
			},
			baseBranch: "main",
			want:       false,
		},
		{
			name: "on proposal no base_branches matches all",
			cfg: CIConfig{
				On: &TriggerConfig{
					Proposal: &ProposalTrigger{BaseBranches: nil},
				},
			},
			baseBranch: "main",
			want:       true,
		},
		{
			name: "on proposal empty base_branches matches all",
			cfg: CIConfig{
				On: &TriggerConfig{
					Proposal: &ProposalTrigger{BaseBranches: []string{}},
				},
			},
			baseBranch: "develop",
			want:       true,
		},
		{
			name: "on proposal base_branches main matches main",
			cfg: CIConfig{
				On: &TriggerConfig{
					Proposal: &ProposalTrigger{BaseBranches: []string{"main"}},
				},
			},
			baseBranch: "main",
			want:       true,
		},
		{
			name: "on proposal base_branches main does not match develop",
			cfg: CIConfig{
				On: &TriggerConfig{
					Proposal: &ProposalTrigger{BaseBranches: []string{"main"}},
				},
			},
			baseBranch: "develop",
			want:       false,
		},
		{
			name: "wildcard release/* matches release/1.0",
			cfg: CIConfig{
				On: &TriggerConfig{
					Proposal: &ProposalTrigger{BaseBranches: []string{"main", "release/*"}},
				},
			},
			baseBranch: "release/1.0",
			want:       true,
		},
		{
			name: "wildcard release/* does not match feature/foo",
			cfg: CIConfig{
				On: &TriggerConfig{
					Proposal: &ProposalTrigger{BaseBranches: []string{"main", "release/*"}},
				},
			},
			baseBranch: "feature/foo",
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.MatchesProposal(tt.baseBranch)
			if got != tt.want {
				t.Errorf("MatchesProposal(%q) = %v, want %v", tt.baseBranch, got, tt.want)
			}
		})
	}
}

func TestEffectivePermissions(t *testing.T) {
	tests := []struct {
		name string
		cfg  CIConfig
		want []string
	}{
		{
			name: "no permissions block returns default checks",
			cfg:  CIConfig{Permissions: nil},
			want: []string{"checks"},
		},
		{
			name: "empty permissions block returns nothing",
			cfg:  CIConfig{Permissions: &Permissions{}},
			want: nil,
		},
		{
			name: "checks write only",
			cfg:  CIConfig{Permissions: &Permissions{Checks: "write"}},
			want: []string{"checks"},
		},
		{
			name: "contents write only",
			cfg:  CIConfig{Permissions: &Permissions{Contents: "write"}},
			want: []string{"contents"},
		},
		{
			name: "checks and contents",
			cfg:  CIConfig{Permissions: &Permissions{Checks: "write", Contents: "write"}},
			want: []string{"checks", "contents"},
		},
		{
			name: "all permissions",
			cfg: CIConfig{Permissions: &Permissions{
				Checks:    "write",
				Contents:  "write",
				Proposals: "write",
				Issues:    "write",
				Releases:  "write",
				CI:        "write",
			}},
			want: []string{"checks", "contents", "proposals", "issues", "releases", "ci"},
		},
		{
			name: "non-write values are ignored",
			cfg:  CIConfig{Permissions: &Permissions{Checks: "read", Contents: "write"}},
			want: []string{"contents"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.EffectivePermissions()
			if !slices.Equal(got, tt.want) {
				t.Errorf("EffectivePermissions() = %v, want %v", got, tt.want)
			}
		})
	}
}
