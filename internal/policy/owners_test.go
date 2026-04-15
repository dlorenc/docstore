package policy_test

import (
	"testing"

	"github.com/dlorenc/docstore/internal/policy"
)

func TestParseOwners_Empty(t *testing.T) {
	owners := policy.ParseOwners(nil)
	if len(owners) != 0 {
		t.Errorf("expected empty map, got %v", owners)
	}
}

func TestParseOwners_IgnoresNonOwners(t *testing.T) {
	files := map[string][]byte{
		"docs/README.md": []byte("# README"),
		"policy.rego":   []byte("package policy"),
	}
	owners := policy.ParseOwners(files)
	if len(owners) != 0 {
		t.Errorf("expected empty map, got %v", owners)
	}
}

func TestParseOwners_Root(t *testing.T) {
	files := map[string][]byte{
		"OWNERS": []byte("alice@example.com\nbob@example.com\n"),
	}
	owners := policy.ParseOwners(files)
	root, ok := owners[""]
	if !ok {
		t.Fatal("expected root entry in owners map")
	}
	if len(root) != 2 {
		t.Fatalf("expected 2 owners, got %d: %v", len(root), root)
	}
}

func TestParseOwners_Subdirectory(t *testing.T) {
	files := map[string][]byte{
		"docs/OWNERS": []byte("carol@example.com\n"),
	}
	owners := policy.ParseOwners(files)
	docs, ok := owners["docs"]
	if !ok {
		t.Fatal("expected 'docs' entry in owners map")
	}
	if len(docs) != 1 || docs[0] != "carol@example.com" {
		t.Errorf("unexpected owners: %v", docs)
	}
}

func TestParseOwners_Comments(t *testing.T) {
	files := map[string][]byte{
		"OWNERS": []byte("# This is a comment\nalice@example.com\n\n# Another comment\nbob@example.com\n"),
	}
	owners := policy.ParseOwners(files)
	root := owners[""]
	if len(root) != 2 {
		t.Fatalf("expected 2 owners (comments stripped), got %d: %v", len(root), root)
	}
}

func TestParseOwners_MultipleDirectories(t *testing.T) {
	files := map[string][]byte{
		"OWNERS":          []byte("root@example.com"),
		"docs/OWNERS":     []byte("docs@example.com"),
		"docs/api/OWNERS": []byte("api@example.com"),
	}
	owners := policy.ParseOwners(files)
	if len(owners) != 3 {
		t.Fatalf("expected 3 entries, got %d: %v", len(owners), owners)
	}
	if _, ok := owners[""]; !ok {
		t.Error("missing root entry")
	}
	if _, ok := owners["docs"]; !ok {
		t.Error("missing docs entry")
	}
	if _, ok := owners["docs/api"]; !ok {
		t.Error("missing docs/api entry")
	}
}

func TestParseOwners_EmptyFile(t *testing.T) {
	files := map[string][]byte{
		"OWNERS": []byte("# only comments\n"),
	}
	owners := policy.ParseOwners(files)
	// Empty file should not create an entry.
	if len(owners) != 0 {
		t.Errorf("expected no entry for all-comment OWNERS file, got %v", owners)
	}
}

// ---------------------------------------------------------------------------
// ResolveOwners tests
// ---------------------------------------------------------------------------

func TestResolveOwners_ExactDirMatch(t *testing.T) {
	// Most specific match: "src/pkg" is the longest prefix of "src/pkg/foo.go"
	owners := map[string][]string{
		"":        {"root@example.com"},
		"src":     {"src@example.com"},
		"src/pkg": {"pkg@example.com"},
	}
	got := policy.ResolveOwners(owners, "src/pkg/foo.go")
	if len(got) != 1 || got[0] != "pkg@example.com" {
		t.Errorf("expected [pkg@example.com], got %v", got)
	}
}

func TestResolveOwners_PrefixInheritance(t *testing.T) {
	// "src/pkg" not in map; fall back to "src"
	owners := map[string][]string{
		"":    {"root@example.com"},
		"src": {"src@example.com"},
	}
	got := policy.ResolveOwners(owners, "src/bar.go")
	if len(got) != 1 || got[0] != "src@example.com" {
		t.Errorf("expected [src@example.com], got %v", got)
	}
}

func TestResolveOwners_RootFallback(t *testing.T) {
	// No subdirectory entry; root applies.
	owners := map[string][]string{
		"":    {"root@example.com"},
		"src": {"src@example.com"},
	}
	got := policy.ResolveOwners(owners, "README.md")
	if len(got) != 1 || got[0] != "root@example.com" {
		t.Errorf("expected [root@example.com], got %v", got)
	}
}

func TestResolveOwners_NoOwners(t *testing.T) {
	// Empty map: no owners at all.
	got := policy.ResolveOwners(map[string][]string{}, "src/foo.go")
	if got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestResolveOwners_NoMatchingPrefix(t *testing.T) {
	// Map has entries but none match the path, and no root entry.
	owners := map[string][]string{
		"docs": {"docs@example.com"},
	}
	got := policy.ResolveOwners(owners, "src/foo.go")
	if got != nil {
		t.Errorf("expected nil (no root fallback), got %v", got)
	}
}

func TestResolveOwners_DeepNesting(t *testing.T) {
	// Three-level deep: "a/b/c" matches "a/b/c/d/e.go" better than "a/b"
	owners := map[string][]string{
		"":      {"root@example.com"},
		"a/b":   {"ab@example.com"},
		"a/b/c": {"abc@example.com"},
	}
	got := policy.ResolveOwners(owners, "a/b/c/d/e.go")
	if len(got) != 1 || got[0] != "abc@example.com" {
		t.Errorf("expected [abc@example.com], got %v", got)
	}
}
