package ciconfig

import "testing"

func TestEvalIf(t *testing.T) {
	pushCtx := TriggerContext{
		Type:   "push",
		Branch: "main",
	}
	proposalCtx := TriggerContext{
		Type:       "proposal",
		Branch:     "feature/foo",
		BaseBranch: "main",
		ProposalID: "42",
	}

	tests := []struct {
		name    string
		expr    string
		ctx     TriggerContext
		want    bool
		wantErr bool
	}{
		// Empty expression — always true.
		{
			name: "empty expr always true",
			expr: "",
			ctx:  pushCtx,
			want: true,
		},
		{
			name: "whitespace only always true",
			expr: "   ",
			ctx:  pushCtx,
			want: true,
		},

		// Equality on event.type.
		{
			name: "event.type == push with push context",
			expr: `event.type == "push"`,
			ctx:  pushCtx,
			want: true,
		},
		{
			name: "event.type == push with proposal context",
			expr: `event.type == "push"`,
			ctx:  proposalCtx,
			want: false,
		},
		{
			name: "event.type == proposal with proposal context",
			expr: `event.type == "proposal"`,
			ctx:  proposalCtx,
			want: true,
		},

		// Single-quoted string literals.
		{
			name: "single-quoted literal push",
			expr: "event.type == 'push'",
			ctx:  pushCtx,
			want: true,
		},
		{
			name: "single-quoted literal mismatch",
			expr: "event.type == 'proposal'",
			ctx:  pushCtx,
			want: false,
		},

		// Inequality.
		{
			name: "event.type != push with push context",
			expr: `event.type != "push"`,
			ctx:  pushCtx,
			want: false,
		},
		{
			name: "event.type != push with proposal context",
			expr: `event.type != "push"`,
			ctx:  proposalCtx,
			want: true,
		},

		// event.branch matching.
		{
			name: "event.branch == main matches main",
			expr: `event.branch == "main"`,
			ctx:  pushCtx,
			want: true,
		},
		{
			name: "event.branch == main does not match feature branch",
			expr: `event.branch == "main"`,
			ctx:  proposalCtx,
			want: false,
		},

		// event.base_branch.
		{
			name: "event.base_branch == main with proposal context",
			expr: `event.base_branch == "main"`,
			ctx:  proposalCtx,
			want: true,
		},
		{
			name: "event.base_branch == main with push context (empty)",
			expr: `event.base_branch == "main"`,
			ctx:  pushCtx,
			want: false,
		},

		// event.proposal_id.
		{
			name: "event.proposal_id == 42 with proposal context",
			expr: `event.proposal_id == "42"`,
			ctx:  proposalCtx,
			want: true,
		},

		// Logical AND.
		{
			name: "push AND main branch true",
			expr: `event.type == "push" && event.branch == "main"`,
			ctx:  pushCtx,
			want: true,
		},
		{
			name: "push AND feature branch false",
			expr: `event.type == "push" && event.branch == "feature/foo"`,
			ctx:  pushCtx,
			want: false,
		},
		{
			name: "proposal type AND main base AND proposal context",
			expr: `event.type == "proposal" && event.base_branch == "main"`,
			ctx:  proposalCtx,
			want: true,
		},

		// Logical OR.
		{
			name: "proposal OR proposal_synchronized with proposal ctx",
			expr: `event.type == "proposal" || event.type == "proposal_synchronized"`,
			ctx:  proposalCtx,
			want: true,
		},
		{
			name: "proposal OR proposal_synchronized with push ctx",
			expr: `event.type == "proposal" || event.type == "proposal_synchronized"`,
			ctx:  pushCtx,
			want: false,
		},
		{
			name: "push OR proposal with push ctx",
			expr: `event.type == "push" || event.type == "proposal"`,
			ctx:  pushCtx,
			want: true,
		},

		// Parenthesized expressions.
		{
			name: "parenthesized OR then AND",
			expr: `(event.type == "proposal" || event.type == "push") && event.branch == "main"`,
			ctx:  pushCtx,
			want: true,
		},
		{
			name: "parenthesized OR then AND false branch",
			expr: `(event.type == "proposal" || event.type == "push") && event.branch == "main"`,
			ctx:  proposalCtx,
			want: false,
		},
		{
			name: "nested parens",
			expr: `(event.type == "push")`,
			ctx:  pushCtx,
			want: true,
		},

		// Real-world examples from the task description.
		{
			name: "task example push and main",
			expr: "event.type == 'push' && event.branch == 'main'",
			ctx:  pushCtx,
			want: true,
		},
		{
			name: "task example proposal types",
			expr: "event.type == 'proposal' || event.type == 'proposal_synchronized'",
			ctx:  proposalCtx,
			want: true,
		},

		// Error cases.
		{
			name:    "unknown field",
			expr:    `event.unknown == "x"`,
			ctx:     pushCtx,
			wantErr: true,
		},
		{
			name:    "unterminated string",
			expr:    `event.type == "push`,
			ctx:     pushCtx,
			wantErr: true,
		},
		{
			name:    "trailing garbage",
			expr:    `event.type == "push" extra`,
			ctx:     pushCtx,
			wantErr: true,
		},
		{
			name:    "unmatched paren",
			expr:    `(event.type == "push"`,
			ctx:     pushCtx,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := EvalIf(tt.expr, tt.ctx)
			if tt.wantErr {
				if err == nil {
					t.Errorf("EvalIf(%q) = %v, nil error; want error", tt.expr, got)
				}
				return
			}
			if err != nil {
				t.Errorf("EvalIf(%q) unexpected error: %v", tt.expr, err)
				return
			}
			if got != tt.want {
				t.Errorf("EvalIf(%q) = %v, want %v", tt.expr, got, tt.want)
			}
		})
	}
}
