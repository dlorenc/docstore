// Package merge provides shared merge policy evaluation logic used by both
// the HTTP handler and the auto-merge worker.
package merge

import (
	"context"
	"fmt"

	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/policy"
	"github.com/dlorenc/docstore/internal/store"
)

// ReadStore is the minimal interface for reading branch and diff data.
// *store.Store satisfies this interface.
type ReadStore interface {
	GetBranch(ctx context.Context, repo, branch string) (*store.BranchInfo, error)
	GetDiff(ctx context.Context, repo, branch string) (*store.DiffResult, error)
	MaterializeTree(ctx context.Context, repo, branch string, atSeq *int64, limit int, afterPath string) ([]store.TreeEntry, error)
	GetFile(ctx context.Context, repo, branch, path string, atSeq *int64) (*store.FileContent, error)
}

// QueryStore is the minimal interface for querying reviews, checks, proposals, and roles.
// *db.Store satisfies this interface.
type QueryStore interface {
	ListReviews(ctx context.Context, repo, branch string, atSeq *int64) ([]model.Review, error)
	ListCheckRuns(ctx context.Context, repo, branch string, atSeq *int64) ([]model.CheckRun, error)
	ListProposals(ctx context.Context, repo string, state *model.ProposalState, branch *string) ([]*model.Proposal, error)
	GetRole(ctx context.Context, repo, identity string) (*model.Role, error)
}

// PolicyCache loads OPA engines for a given repo.
// *policy.Cache satisfies this interface.
type PolicyCache interface {
	Load(ctx context.Context, repo string, st policy.ReadStore) (*policy.Engine, map[string][]string, error)
}

// AssembleInput gathers all data needed for OPA policy evaluation.
// Returns nil, nil if the branch does not exist (callers should surface the 404).
// owners is the raw directory→owners map from the policy cache; it is resolved
// per changed path before being placed on the Input.
func AssembleInput(ctx context.Context, rs ReadStore, qs QueryStore, repo, branch, actor string, owners map[string][]string) (*policy.Input, error) {
	branchInfo, err := rs.GetBranch(ctx, repo, branch)
	if err != nil {
		return nil, fmt.Errorf("get branch info: %w", err)
	}
	if branchInfo == nil {
		return nil, nil // branch not found
	}

	diff, err := rs.GetDiff(ctx, repo, branch)
	if err != nil {
		return nil, fmt.Errorf("get diff: %w", err)
	}
	var changedPaths []string
	if diff != nil {
		for _, e := range diff.BranchChanges {
			changedPaths = append(changedPaths, e.Path)
		}
	}

	reviews, err := qs.ListReviews(ctx, repo, branch, &branchInfo.HeadSequence)
	if err != nil {
		return nil, fmt.Errorf("list reviews: %w", err)
	}
	checkRuns, err := qs.ListCheckRuns(ctx, repo, branch, &branchInfo.HeadSequence)
	if err != nil {
		return nil, fmt.Errorf("list check runs: %w", err)
	}

	openState := model.ProposalOpen
	proposals, err := qs.ListProposals(ctx, repo, &openState, &branch)
	if err != nil {
		return nil, fmt.Errorf("list proposals: %w", err)
	}
	var proposalInput *policy.ProposalInput
	if len(proposals) > 0 {
		p := proposals[0]
		proposalInput = &policy.ProposalInput{
			ID:         p.ID,
			BaseBranch: p.BaseBranch,
			Title:      p.Title,
			State:      string(p.State),
		}
	}

	actorRoles := []string{}
	if r, err := qs.GetRole(ctx, repo, actor); err == nil && r != nil {
		actorRoles = []string{string(r.Role)}
	}

	reviewInputs := make([]policy.ReviewInput, len(reviews))
	for i, rev := range reviews {
		reviewInputs[i] = policy.ReviewInput{
			Reviewer: rev.Reviewer,
			Status:   string(rev.Status),
			Sequence: rev.Sequence,
		}
	}
	checkInputs := make([]policy.CheckRunInput, len(checkRuns))
	for i, cr := range checkRuns {
		checkInputs[i] = policy.CheckRunInput{
			CheckName: cr.CheckName,
			Status:    string(cr.Status),
			Sequence:  cr.Sequence,
		}
	}

	resolvedOwners := make(map[string][]string)
	for _, p := range changedPaths {
		resolvedOwners[p] = policy.ResolveOwners(owners, p)
	}

	return &policy.Input{
		Actor:        actor,
		ActorRoles:   actorRoles,
		Action:       "merge",
		Repo:         repo,
		Branch:       branch,
		Draft:        branchInfo.Draft,
		ChangedPaths: changedPaths,
		Reviews:      reviewInputs,
		CheckRuns:    checkInputs,
		Owners:       resolvedOwners,
		HeadSeq:      branchInfo.HeadSequence,
		BaseSeq:      branchInfo.BaseSequence,
		Proposal:     proposalInput,
	}, nil
}

// EvaluatePolicy evaluates all OPA policies for a pending merge.
// Returns failing PolicyResults (non-nil means denied), or nil if allowed.
// Returns an error only for unexpected infrastructure failures.
// When cache or rs are nil, or no policies exist, the merge is allowed.
func EvaluatePolicy(ctx context.Context, cache PolicyCache, rs ReadStore, qs QueryStore, repo, branch, actor string) ([]model.PolicyResult, error) {
	if cache == nil || rs == nil {
		return nil, nil
	}

	engine, owners, err := cache.Load(ctx, repo, rs)
	if err != nil {
		return nil, err
	}
	if engine == nil {
		return nil, nil // bootstrap mode
	}

	input, err := AssembleInput(ctx, rs, qs, repo, branch, actor, owners)
	if err != nil {
		return nil, err
	}
	if input == nil {
		return nil, nil // branch not found — let caller surface the error
	}

	results, err := engine.Evaluate(ctx, *input)
	if err != nil {
		return nil, fmt.Errorf("evaluate policies: %w", err)
	}

	var denied []model.PolicyResult
	for _, r := range results {
		if !r.Pass {
			denied = append(denied, r)
		}
	}
	return denied, nil
}
