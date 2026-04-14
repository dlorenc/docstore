package server

import (
	"context"

	"github.com/dlorenc/docstore/internal/model"
)

// Store defines the data access interface for the server.
type Store interface {
	// CreateBranch creates a new branch forked from main's current head.
	CreateBranch(ctx context.Context, name string) (*model.Branch, error)

	// DeleteBranch marks a branch as abandoned.
	DeleteBranch(ctx context.Context, name string) error

	// ListBranches returns all branches, optionally filtered by status.
	ListBranches(ctx context.Context, status string) ([]model.Branch, error)

	// Merge merges a branch into main using the algorithm from DESIGN.md.
	// Returns (response, nil, nil) on success, (nil, conflicts, nil) on conflict,
	// or (nil, nil, err) on error.
	Merge(ctx context.Context, branch string) (*model.MergeResponse, []model.ConflictEntry, error)

	// Rebase replays branch commits onto main's current head.
	// Returns (response, nil, nil) on success, (nil, conflicts, nil) on conflict,
	// or (nil, nil, err) on error.
	Rebase(ctx context.Context, branch string) (*model.RebaseResponse, []model.ConflictEntry, error)

	// Diff computes the diff between a branch and main since base_sequence.
	Diff(ctx context.Context, branch string) (*model.DiffResponse, error)
}
