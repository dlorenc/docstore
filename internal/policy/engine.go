// Package policy implements OPA-based policy evaluation for merge operations.
//
// Each .rego file in .docstore/policy/ on the main branch defines one policy.
// Policies must be in `package policy` and should define:
//
//	default allow = false
//	allow { ... }         // conditions under which the merge is permitted
//	reason = "..."        // optional human-readable denial reason
//
// When no .rego files exist the engine is nil and all merges are allowed
// (bootstrap mode).
package policy

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/dlorenc/docstore/internal/model"
	"github.com/open-policy-agent/opa/v1/rego"
)

// ReviewInput is the review data exposed to OPA policies.
type ReviewInput struct {
	Reviewer string `json:"reviewer"`
	Status   string `json:"status"`
	Sequence int64  `json:"sequence"`
}

// CheckRunInput is the check-run data exposed to OPA policies.
type CheckRunInput struct {
	CheckName string `json:"check_name"`
	Status    string `json:"status"`
	Sequence  int64  `json:"sequence"`
}

// Input is the structured context passed to every policy evaluation.
type Input struct {
	Actor     string              `json:"actor"`
	Role      string              `json:"role"`
	Paths     []string            `json:"paths"`
	Reviews   []ReviewInput       `json:"reviews"`
	CheckRuns []CheckRunInput     `json:"check_runs"`
	Owners    map[string][]string `json:"owners"`
	Branch    string              `json:"branch"`
	HeadSeq   int64               `json:"head_sequence"`
	BaseSeq   int64               `json:"base_sequence"`
}

// preparedPolicy holds compiled OPA queries for a single .rego file.
type preparedPolicy struct {
	name     string
	allowPQ  rego.PreparedEvalQuery
	reasonPQ rego.PreparedEvalQuery
}

// Engine evaluates a set of compiled OPA policies.
// A nil Engine means no policies are defined (bootstrap mode); all merges are allowed.
type Engine struct {
	queries []preparedPolicy
}

// NewEngine compiles the given modules into an Engine.
// modules maps filename (e.g. ".docstore/policy/require_review.rego") to rego source.
// Returns nil, nil if modules is empty (bootstrap mode).
func NewEngine(ctx context.Context, modules map[string]string) (*Engine, error) {
	if len(modules) == 0 {
		return nil, nil
	}

	queries := make([]preparedPolicy, 0, len(modules))
	for filename, src := range modules {
		name := policyName(filename)

		allowPQ, err := rego.New(
			rego.Query("data.policy.allow"),
			rego.Module(filename, src),
		).PrepareForEval(ctx)
		if err != nil {
			return nil, fmt.Errorf("compile policy %q (allow): %w", name, err)
		}

		reasonPQ, err := rego.New(
			rego.Query("data.policy.reason"),
			rego.Module(filename, src),
		).PrepareForEval(ctx)
		if err != nil {
			return nil, fmt.Errorf("compile policy %q (reason): %w", name, err)
		}

		queries = append(queries, preparedPolicy{
			name:     name,
			allowPQ:  allowPQ,
			reasonPQ: reasonPQ,
		})
	}

	return &Engine{queries: queries}, nil
}

// Evaluate runs all policy modules against the given input.
// Returns one model.PolicyResult per module.
// If the Engine is nil (bootstrap mode), returns nil, nil.
func (e *Engine) Evaluate(ctx context.Context, input Input) ([]model.PolicyResult, error) {
	if e == nil {
		return nil, nil
	}
	results := make([]model.PolicyResult, 0, len(e.queries))
	for _, p := range e.queries {
		r, err := evalPrepared(ctx, p, input)
		if err != nil {
			return nil, fmt.Errorf("evaluate policy %q: %w", p.name, err)
		}
		results = append(results, r)
	}
	return results, nil
}

// evalPrepared evaluates one compiled policy against the given input.
func evalPrepared(ctx context.Context, p preparedPolicy, input Input) (model.PolicyResult, error) {
	// Evaluate the allow rule.
	allowRs, err := p.allowPQ.Eval(ctx, rego.EvalInput(input))
	if err != nil {
		// Treat evaluation errors as denials with an error reason.
		return model.PolicyResult{
			Name:   p.name,
			Pass:   false,
			Reason: fmt.Sprintf("evaluation error: %v", err),
		}, nil
	}

	allow := false
	if len(allowRs) > 0 && len(allowRs[0].Expressions) > 0 {
		allow, _ = allowRs[0].Expressions[0].Value.(bool)
	}

	if allow {
		return model.PolicyResult{Name: p.name, Pass: true}, nil
	}

	// Denied — try to get the reason string.
	reason := "policy denied"
	reasonRs, _ := p.reasonPQ.Eval(ctx, rego.EvalInput(input))
	if len(reasonRs) > 0 && len(reasonRs[0].Expressions) > 0 {
		if r, ok := reasonRs[0].Expressions[0].Value.(string); ok && r != "" {
			reason = r
		}
	}

	return model.PolicyResult{Name: p.name, Pass: false, Reason: reason}, nil
}

// policyName extracts a human-readable policy name from its filename.
func policyName(filename string) string {
	base := path.Base(filename)
	return strings.TrimSuffix(base, ".rego")
}
