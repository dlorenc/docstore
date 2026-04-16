package db

import (
	"context"
	"fmt"
	"testing"

	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/store"
	"github.com/dlorenc/docstore/internal/testutil"
)

// BenchmarkChainWalk_100Commits measures the end-to-end cost of fetching and
// verifying the hash chain for a repo with 100 commits on main.
// The benchmark fails if any commit_hash does not match the recomputed value.
func BenchmarkChainWalk_100Commits(b *testing.B) {
	d := testutil.TestDB(b, RunMigrations)
	s := NewStore(d)
	rs := store.New(d)
	ctx := context.Background()

	const n = 100
	for i := 1; i <= n; i++ {
		_, err := s.Commit(ctx, model.CommitRequest{
			Repo:    "default/default",
			Branch:  "main",
			Files:   []model.FileChange{{Path: fmt.Sprintf("file%03d.txt", i), Content: []byte(fmt.Sprintf("content %d", i))}},
			Message: fmt.Sprintf("commit %d", i),
			Author:  "bench@example.com",
		})
		if err != nil {
			b.Fatalf("setup commit %d: %v", i, err)
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entries, err := rs.GetChain(ctx, "default/default", 1, n)
		if err != nil {
			b.Fatalf("GetChain: %v", err)
		}

		prevHash := genesisHash
		for _, e := range entries {
			if e.CommitHash == nil {
				prevHash = genesisHash
				continue
			}
			files := make([]chainFile, len(e.Files))
			for j, f := range e.Files {
				files[j] = chainFile{path: f.Path, contentHash: f.ContentHash}
			}
			got := computeCommitHash(prevHash, e.Sequence, "default/default", e.Branch, e.Author, e.Message, e.CreatedAt, files)
			if got != *e.CommitHash {
				b.Fatalf("chain integrity error at sequence %d: got %s want %s", e.Sequence, got, *e.CommitHash)
			}
			prevHash = *e.CommitHash
		}
	}
}
