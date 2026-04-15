package tui

import (
	"testing"

	"github.com/dlorenc/docstore/internal/model"
)

func strPtr(s string) *string { return &s }

func TestDiffFiles_Classification(t *testing.T) {
	vID := strPtr("v1")
	diff := &model.DiffResponse{
		BranchChanges: []model.DiffEntry{
			{Path: "existing.go", VersionID: vID},  // in base tree → modified
			{Path: "newfile.go", VersionID: vID},   // not in base tree → new
			{Path: "deleted.go", VersionID: nil},   // nil VersionID → deleted
		},
	}
	baseTreePaths := map[string]bool{
		"existing.go": true,
	}

	files := diffFiles(diff, baseTreePaths)
	if len(files) != 3 {
		t.Fatalf("expected 3 files, got %d", len(files))
	}

	byPath := make(map[string]string)
	for _, f := range files {
		byPath[f.path] = f.changeType
	}

	if byPath["existing.go"] != "~" {
		t.Errorf("existing.go: want '~', got %q", byPath["existing.go"])
	}
	if byPath["newfile.go"] != "+" {
		t.Errorf("newfile.go: want '+', got %q", byPath["newfile.go"])
	}
	if byPath["deleted.go"] != "-" {
		t.Errorf("deleted.go: want '-', got %q", byPath["deleted.go"])
	}
}

func TestDiffFiles_EmptyBaseTree(t *testing.T) {
	// When base tree is empty, all non-deleted files should be "~" (unchanged behaviour).
	vID := strPtr("v1")
	diff := &model.DiffResponse{
		BranchChanges: []model.DiffEntry{
			{Path: "file.go", VersionID: vID},
		},
	}

	files := diffFiles(diff, nil)
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	if files[0].changeType != "~" {
		t.Errorf("want '~' when base tree is nil, got %q", files[0].changeType)
	}

	files = diffFiles(diff, map[string]bool{})
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	if files[0].changeType != "~" {
		t.Errorf("want '~' when base tree is empty, got %q", files[0].changeType)
	}
}
