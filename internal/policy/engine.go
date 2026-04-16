// Package policy implements OPA-based policy evaluation for merge operations.
//
// Each .rego file in .docstore/policy/ on the main branch defines one policy.
// Policies must use 'package docstore.<policy_name>' (e.g.
// 'package docstore.require_review') and should define:
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
	"strings"
	"time"

	"github.com/dlorenc/docstore/internal/model"
	"github.com/open-policy-agent/opa/v1/ast"
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
	Actor        string              `json:"actor"`
	ActorRoles   []string            `json:"actor_roles"`
	Action       string              `json:"action"`
	Repo         string              `json:"repo"`
	Branch       string              `json:"branch"`
	Draft        bool                `json:"draft"`
	ChangedPaths []string            `json:"changed_paths"`
	Reviews      []ReviewInput       `json:"reviews"`
	CheckRuns    []CheckRunInput     `json:"check_runs"`
	Owners       map[string][]string `json:"owners"`
	HeadSeq      int64               `json:"head_sequence"`
	BaseSeq      int64               `json:"base_sequence"`
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
// Each module must declare 'package docstore.<name>'; the policy name is derived
// from the last segment of the package path.
// Returns nil, nil if modules is empty (bootstrap mode).
func NewEngine(ctx context.Context, modules map[string]string) (*Engine, error) {
	if len(modules) == 0 {
		return nil, nil
	}

	queries := make([]preparedPolicy, 0, len(modules))
	for filename, src := range modules {
		// Parse the module AST to extract the package path.
		module, err := ast.ParseModule(filename, src)
		if err != nil {
			return nil, fmt.Errorf("parse policy %q: %w", filename, err)
		}
		pkgPath := module.Package.Path.String()
		name := nameFromPackagePath(pkgPath)

		allowPQ, err := rego.New(
			rego.Query(pkgPath+".allow"),
			rego.Module(filename, src),
		).PrepareForEval(ctx)
		if err != nil {
			return nil, fmt.Errorf("compile policy %q (allow): %w", name, err)
		}

		reasonPQ, err := rego.New(
			rego.Query(pkgPath+".reason"),
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
// Wraps evaluation in a 5-second timeout to prevent infinite loops in Rego
// from blocking request goroutines. Returns errors rather than converting them
// to silent denies so broken policies surface as 500s in the handler.
func evalPrepared(ctx context.Context, p preparedPolicy, input Input) (model.PolicyResult, error) {
	// Wrap in a 5-second timeout to prevent runaway Rego evaluation.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Evaluate the allow rule.
	allowRs, err := p.allowPQ.Eval(ctx, rego.EvalInput(input))
	if err != nil {
		return model.PolicyResult{}, fmt.Errorf("allow eval: %w", err)
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
	reasonRs, err := p.reasonPQ.Eval(ctx, rego.EvalInput(input))
	if err != nil {
		return model.PolicyResult{}, fmt.Errorf("reason eval: %w", err)
	}
	if len(reasonRs) > 0 && len(reasonRs[0].Expressions) > 0 {
		if r, ok := reasonRs[0].Expressions[0].Value.(string); ok && r != "" {
			reason = r
		}
	}

	return model.PolicyResult{Name: p.name, Pass: false, Reason: reason}, nil
}

// nameFromPackagePath extracts the policy name from an OPA package path.
// E.g. "data.docstore.require_review" → "require_review".
func nameFromPackagePath(pkgPath string) string {
	idx := strings.LastIndex(pkgPath, ".")
	if idx < 0 || idx == len(pkgPath)-1 {
		return pkgPath
	}
	return pkgPath[idx+1:]
}
