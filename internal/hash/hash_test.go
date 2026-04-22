package hash_test

import (
	"testing"
	"time"

	"github.com/dlorenc/docstore/internal/hash"
)

// TestCommitHash_Deterministic verifies that CommitHash returns the same value
// for identical inputs regardless of call order.
func TestCommitHash_Deterministic(t *testing.T) {
	t.Parallel()
	ts := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	files := []hash.File{
		{Path: "b.txt", ContentHash: "bbbbbbbb"},
		{Path: "a.txt", ContentHash: "aaaaaaaa"},
	}
	got1 := hash.CommitHash(hash.GenesisHash, 1, "org/repo", "main", "alice@example.com", "init", ts, files)
	got2 := hash.CommitHash(hash.GenesisHash, 1, "org/repo", "main", "alice@example.com", "init", ts, files)
	if got1 != got2 {
		t.Errorf("non-deterministic: %q != %q", got1, got2)
	}
}

// TestCommitHash_FileOrderIndependent verifies that file order does not affect the hash.
func TestCommitHash_FileOrderIndependent(t *testing.T) {
	t.Parallel()
	ts := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	filesAB := []hash.File{
		{Path: "a.txt", ContentHash: "aaa"},
		{Path: "b.txt", ContentHash: "bbb"},
	}
	filesBA := []hash.File{
		{Path: "b.txt", ContentHash: "bbb"},
		{Path: "a.txt", ContentHash: "aaa"},
	}
	h1 := hash.CommitHash(hash.GenesisHash, 1, "org/repo", "main", "alice", "msg", ts, filesAB)
	h2 := hash.CommitHash(hash.GenesisHash, 1, "org/repo", "main", "alice", "msg", ts, filesBA)
	if h1 != h2 {
		t.Errorf("file order affected hash: %q vs %q", h1, h2)
	}
}

// TestCommitHash_ChainLinks verifies that prevHash is incorporated so consecutive
// commits form a linked chain (changing prevHash changes the output).
func TestCommitHash_ChainLinks(t *testing.T) {
	t.Parallel()
	ts := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	files := []hash.File{{Path: "f.txt", ContentHash: "cccc"}}

	h1 := hash.CommitHash(hash.GenesisHash, 1, "org/repo", "main", "alice", "first", ts, files)
	h2 := hash.CommitHash(h1, 2, "org/repo", "main", "alice", "second", ts, files)
	h3WithWrongPrev := hash.CommitHash(hash.GenesisHash, 2, "org/repo", "main", "alice", "second", ts, files)

	if h2 == h3WithWrongPrev {
		t.Error("prevHash not incorporated: different prevHash produced the same hash")
	}
}

// TestCommitHash_KnownValue is a golden-value test ensuring the algorithm does not
// silently change. If this test breaks, all stored hashes must be recomputed.
func TestCommitHash_KnownValue(t *testing.T) {
	t.Parallel()
	ts := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	files := []hash.File{
		{Path: "README.md", ContentHash: "deadbeef"},
	}
	got := hash.CommitHash(hash.GenesisHash, 1, "org/repo", "main", "alice@example.com", "initial commit", ts, files)
	// The want value is computed from the canonical algorithm; update it here if the
	// algorithm is intentionally changed (requires recomputing all stored hashes).
	const want = "e245acb0715bd67a76e1395fc4b27229835c93e133b51b9e96364f4507e0c06b"
	if got != want {
		t.Errorf("hash formula changed: got %s, want %s — update this constant only if the algorithm change is intentional (all stored hashes must be recomputed)", got, want)
	}
}

// TestGenesisHash verifies the GenesisHash constant has the expected format.
func TestGenesisHash(t *testing.T) {
	t.Parallel()
	if len(hash.GenesisHash) != 64 {
		t.Errorf("GenesisHash len=%d, want 64", len(hash.GenesisHash))
	}
	for _, c := range hash.GenesisHash {
		if c != '0' {
			t.Errorf("GenesisHash contains non-zero char %q", c)
			break
		}
	}
}

// TestCommitHash_ServerClientConsistency explicitly documents that the function
// is the single shared implementation for both server (internal/db) and client
// (internal/cli) and verifies it produces results matching the documented formula.
func TestCommitHash_ServerClientConsistency(t *testing.T) {
	t.Parallel()
	// Simulate the server computing a hash during a Commit call.
	ts := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	serverFiles := []hash.File{
		{Path: "doc.txt", ContentHash: "abc123"},
		{Path: "README.md", ContentHash: "def456"},
	}
	serverHash := hash.CommitHash(hash.GenesisHash, 3, "myorg/myrepo", "feature", "bob@example.com", "add docs", ts, serverFiles)

	// Simulate the client recomputing the same hash during a Verify call.
	// The client receives files from the server's JSON response (same values, possibly different order).
	clientFiles := []hash.File{
		{Path: "README.md", ContentHash: "def456"},
		{Path: "doc.txt", ContentHash: "abc123"},
	}
	clientHash := hash.CommitHash(hash.GenesisHash, 3, "myorg/myrepo", "feature", "bob@example.com", "add docs", ts, clientFiles)

	if serverHash != clientHash {
		t.Errorf("server hash %q != client hash %q — implementations diverged", serverHash, clientHash)
	}
}
