package policy_test

import (
	"context"
	"testing"

	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/policy"
)

func TestNewEngine_Empty(t *testing.T) {
	ctx := context.Background()
	engine, err := policy.NewEngine(ctx, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if engine != nil {
		t.Fatal("expected nil engine for empty modules (bootstrap mode)")
	}
}

func TestEngine_NilEvaluate(t *testing.T) {
	ctx := context.Background()
	var engine *policy.Engine
	results, err := engine.Evaluate(ctx, policy.Input{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if results != nil {
		t.Fatal("expected nil results from nil engine")
	}
}

func TestEngine_Allow(t *testing.T) {
	ctx := context.Background()
	src := `
package docstore.require_review

default allow = false

allow if {
    count(input.reviews) > 0
    input.reviews[_].status == "approved"
}
`
	engine, err := policy.NewEngine(ctx, map[string]string{
		".docstore/policy/require_review.rego": src,
	})
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	// Should deny: no reviews.
	results, err := engine.Evaluate(ctx, policy.Input{
		Actor:  "alice@example.com",
		Branch: "feature/x",
	})
	if err != nil {
		t.Fatalf("evaluate error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Pass {
		t.Error("expected deny when no reviews")
	}

	// Should allow: approved review present.
	results, err = engine.Evaluate(ctx, policy.Input{
		Actor:  "alice@example.com",
		Branch: "feature/x",
		Reviews: []policy.ReviewInput{
			{Reviewer: "bob@example.com", Status: "approved", Sequence: 1},
		},
	})
	if err != nil {
		t.Fatalf("evaluate error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !results[0].Pass {
		t.Errorf("expected pass, got deny: %s", results[0].Reason)
	}
}

func TestEngine_DenyReason(t *testing.T) {
	ctx := context.Background()
	src := `
package docstore.require_check

default allow = false
default reason = "at least one passing check is required"

allow if {
    input.check_runs[_].status == "passed"
}
`
	engine, err := policy.NewEngine(ctx, map[string]string{
		".docstore/policy/require_check.rego": src,
	})
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	results, err := engine.Evaluate(ctx, policy.Input{
		Actor:     "alice@example.com",
		Branch:    "feature/x",
		CheckRuns: []policy.CheckRunInput{},
	})
	if err != nil {
		t.Fatalf("evaluate error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if r.Pass {
		t.Error("expected deny")
	}
	if r.Reason != "at least one passing check is required" {
		t.Errorf("unexpected reason: %q", r.Reason)
	}
	if r.Name != "require_check" {
		t.Errorf("unexpected policy name: %q", r.Name)
	}
}

func TestEngine_MultipleModules(t *testing.T) {
	ctx := context.Background()
	reviewPolicy := `
package docstore.review

default allow = false

allow if {
    input.reviews[_].status == "approved"
}
`
	checkPolicy := `
package docstore.check

default allow = false

allow if {
    input.check_runs[_].status == "passed"
}
`
	engine, err := policy.NewEngine(ctx, map[string]string{
		".docstore/policy/review.rego": reviewPolicy,
		".docstore/policy/check.rego":  checkPolicy,
	})
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	// Both should deny when nothing is provided.
	results, err := engine.Evaluate(ctx, policy.Input{})
	if err != nil {
		t.Fatalf("evaluate error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	allFail := true
	for _, r := range results {
		if r.Pass {
			allFail = false
		}
	}
	if !allFail {
		t.Error("expected all policies to deny when no reviews/checks")
	}
}

func TestEngine_PolicyName(t *testing.T) {
	ctx := context.Background()
	src := `
package docstore.my_policy
default allow = true
`
	engine, err := policy.NewEngine(ctx, map[string]string{
		".docstore/policy/my_policy.rego": src,
	})
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}
	results, err := engine.Evaluate(ctx, policy.Input{})
	if err != nil {
		t.Fatalf("evaluate error: %v", err)
	}
	if len(results) != 1 || results[0].Name != "my_policy" {
		t.Errorf("unexpected policy name: %+v", results)
	}
}

func TestEngine_PolicyResultTypes(t *testing.T) {
	// Verify that PolicyResult from policy package matches model.PolicyResult fields.
	var _ = model.PolicyResult{Name: "x", Pass: true, Reason: "y"}
}

func TestEngine_InputFieldNames(t *testing.T) {
	// Verify that the corrected field names are accessible and used by policies.
	ctx := context.Background()
	src := `
package docstore.role_check

default allow = false

allow if {
    input.actor_roles[_] == "maintainer"
}
`
	engine, err := policy.NewEngine(ctx, map[string]string{
		".docstore/policy/role_check.rego": src,
	})
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	// Should deny: no roles.
	results, err := engine.Evaluate(ctx, policy.Input{
		Actor:      "alice@example.com",
		ActorRoles: []string{},
	})
	if err != nil {
		t.Fatalf("evaluate error: %v", err)
	}
	if results[0].Pass {
		t.Error("expected deny with no roles")
	}

	// Should allow: maintainer role present.
	results, err = engine.Evaluate(ctx, policy.Input{
		Actor:      "alice@example.com",
		ActorRoles: []string{"maintainer"},
	})
	if err != nil {
		t.Fatalf("evaluate error: %v", err)
	}
	if !results[0].Pass {
		t.Errorf("expected pass with maintainer role, got deny: %s", results[0].Reason)
	}
}

func TestEngine_ChangedPathsField(t *testing.T) {
	// Verify changed_paths field name is accessible to policies.
	ctx := context.Background()
	src := `
package docstore.path_check

default allow = false

allow if {
    count(input.changed_paths) == 0
}
`
	engine, err := policy.NewEngine(ctx, map[string]string{
		".docstore/policy/path_check.rego": src,
	})
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	// Should allow: no changed paths.
	results, err := engine.Evaluate(ctx, policy.Input{ChangedPaths: []string{}})
	if err != nil {
		t.Fatalf("evaluate error: %v", err)
	}
	if !results[0].Pass {
		t.Errorf("expected pass with no changed paths")
	}

	// Should deny: has changed paths.
	results, err = engine.Evaluate(ctx, policy.Input{ChangedPaths: []string{"foo.go"}})
	if err != nil {
		t.Fatalf("evaluate error: %v", err)
	}
	if results[0].Pass {
		t.Error("expected deny with changed paths")
	}
}
