package ciconfig

import "testing"

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
