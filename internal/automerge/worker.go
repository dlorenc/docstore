// Package automerge implements the auto-merge worker. When a branch has
// auto_merge=true, the worker listens for check.reported and review.submitted
// events and attempts a merge whenever policies are satisfied.
package automerge

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/events"
	evtypes "github.com/dlorenc/docstore/internal/events/types"
	mergeutil "github.com/dlorenc/docstore/internal/merge"
	"github.com/dlorenc/docstore/internal/model"
)

// Store is the minimal interface required by the worker for write operations.
// *db.Store satisfies this interface.
type Store interface {
	// Branch writes
	SetBranchAutoMerge(ctx context.Context, repo, name string, autoMerge bool) error
	// Merge
	Merge(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error)
	MergeProposal(ctx context.Context, repo, branch string) (*model.Proposal, error)
	// Policy input assembly (satisfies mergeutil.QueryStore)
	ListReviews(ctx context.Context, repo, branch string, atSeq *int64) ([]model.Review, error)
	ListCheckRuns(ctx context.Context, repo, branch string, atSeq *int64, history bool) ([]model.CheckRun, error)
	ListProposals(ctx context.Context, repo string, state *model.ProposalState, branch *string) ([]*model.Proposal, error)
	GetRole(ctx context.Context, repo, identity string) (*model.Role, error)
}

// Worker subscribes to check.reported and review.submitted events and
// attempts an auto-merge when a branch has auto_merge=true.
type Worker struct {
	broker      *events.Broker
	store       Store
	readStore   mergeutil.ReadStore
	policyCache mergeutil.PolicyCache
}

// New creates a new Worker. If broker, store, or readStore are nil the worker
// will do nothing (safe no-op for tests that don't wire events).
func New(broker *events.Broker, st Store, rs mergeutil.ReadStore, pc mergeutil.PolicyCache) *Worker {
	return &Worker{
		broker:      broker,
		store:       st,
		readStore:   rs,
		policyCache: pc,
	}
}

// Run starts the event loop. It polls event_log for check.reported and
// review.submitted events and attempts auto-merge when a branch qualifies.
// Blocks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	if w.broker == nil || w.store == nil || w.readStore == nil {
		return
	}

	targetTypes := []string{
		"com.docstore.check.reported",
		"com.docstore.review.submitted",
	}

	// Start from the current tail so we don't replay historical events.
	sinceSeq, err := w.broker.CurrentSeq(ctx)
	if err != nil {
		slog.Error("auto-merge: CurrentSeq failed", "error", err)
	}

	for {
		evs, pollErr := w.broker.Poll(ctx, "*", sinceSeq, targetTypes)
		if pollErr != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("auto-merge: poll failed", "error", pollErr)
		}

		for _, ev := range evs {
			sinceSeq = ev.Seq
			var envelope struct {
				Data struct {
					Branch string `json:"branch"`
				} `json:"data"`
			}
			if err := json.Unmarshal(ev.Payload, &envelope); err != nil {
				slog.Warn("auto-merge: unmarshal event payload failed",
					"type", ev.Type, "seq", ev.Seq, "error", err)
				continue
			}
			w.tryAutoMerge(ctx, ev.Repo, envelope.Data.Branch)
		}

		// Wait for a new event notification (or give up after 30 s and re-poll).
		waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		w.broker.WaitForEvent(waitCtx) //nolint:errcheck
		cancel()

		if ctx.Err() != nil {
			return
		}
	}
}

// tryAutoMerge checks whether the branch should be auto-merged and attempts
// the merge if all conditions are met.
func (w *Worker) tryAutoMerge(ctx context.Context, repo, branch string) {
	info, err := w.readStore.GetBranch(ctx, repo, branch)
	if err != nil {
		slog.Error("auto-merge: get branch failed", "repo", repo, "branch", branch, "error", err)
		return
	}
	if info == nil {
		return // branch not found
	}
	if !info.AutoMerge || info.Draft || info.Status != "active" {
		return
	}

	denied, err := mergeutil.EvaluatePolicy(ctx, w.policyCache, w.readStore, w.store, repo, branch, "auto-merge")
	if err != nil {
		slog.Error("auto-merge: policy evaluation failed", "repo", repo, "branch", branch, "error", err)
		return
	}
	if denied != nil {
		// Policies not yet satisfied — will retry on next event.
		return
	}

	resp, conflicts, err := w.store.Merge(ctx, model.MergeRequest{
		Repo:   repo,
		Branch: branch,
		Author: "auto-merge",
	})
	if err != nil {
		if errors.Is(err, db.ErrMergeConflict) {
			slog.Warn("auto-merge: conflict detected, disabling auto-merge",
				"repo", repo, "branch", branch, "conflicts", len(conflicts))
			if clearErr := w.store.SetBranchAutoMerge(ctx, repo, branch, false); clearErr != nil {
				slog.Warn("auto-merge: failed to clear auto_merge flag", "repo", repo, "branch", branch, "error", clearErr)
			}
			w.broker.Emit(ctx, evtypes.BranchAutoMergeFailed{
				Repo:   repo,
				Branch: branch,
				Reason: "merge conflict",
			})
		} else if errors.Is(err, db.ErrBranchDraft) || errors.Is(err, db.ErrBranchNotActive) || errors.Is(err, db.ErrBranchNotFound) {
			// Branch state changed between check and merge — nothing to do.
			slog.Info("auto-merge: branch no longer mergeable", "repo", repo, "branch", branch, "error", err)
		} else {
			slog.Error("auto-merge: merge failed", "repo", repo, "branch", branch, "error", err)
		}
		return
	}

	// Best-effort: transition any open proposal to merged.
	if mp, mergeProposalErr := w.store.MergeProposal(ctx, repo, branch); mergeProposalErr != nil {
		slog.Warn("auto-merge: failed to merge proposal", "repo", repo, "branch", branch, "error", mergeProposalErr)
	} else if mp != nil {
		w.broker.Emit(ctx, evtypes.ProposalMerged{
			Repo:       repo,
			Branch:     branch,
			BaseBranch: mp.BaseBranch,
			ProposalID: mp.ID,
		})
	}

	w.broker.Emit(ctx, evtypes.BranchMerged{
		Repo:     repo,
		Branch:   branch,
		Sequence: resp.Sequence,
		MergedBy: "auto-merge",
	})
	slog.Info("auto-merge: branch merged", "repo", repo, "branch", branch, "sequence", resp.Sequence)
}
