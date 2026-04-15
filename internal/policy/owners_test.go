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
