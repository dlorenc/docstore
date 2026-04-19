package tui

import (
	"strings"
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

// --- lcsLineDiff tests ---

func lcsKinds(hunks []diffHunkLine) string {
	var b strings.Builder
	for _, h := range hunks {
		b.WriteByte(h.kind)
	}
	return b.String()
}

func TestLcsLineDiff_IdenticalFiles(t *testing.T) {
	lines := []string{"a", "b", "c"}
	hunks := lcsLineDiff(lines, lines)
	if len(hunks) != 3 {
		t.Fatalf("want 3 hunks, got %d", len(hunks))
	}
	for _, h := range hunks {
		if h.kind != ' ' {
			t.Errorf("want all context lines, got kind %q", h.kind)
		}
	}
}

func TestLcsLineDiff_CompletelyDifferent(t *testing.T) {
	base := []string{"a", "b", "c"}
	head := []string{"x", "y", "z"}
	hunks := lcsLineDiff(base, head)
	kinds := lcsKinds(hunks)
	if strings.ContainsAny(kinds, " ") {
		t.Errorf("want no common lines, kinds: %q", kinds)
	}
	deletes := strings.Count(kinds, "-")
	adds := strings.Count(kinds, "+")
	if deletes != 3 {
		t.Errorf("want 3 deletes, got %d", deletes)
	}
	if adds != 3 {
		t.Errorf("want 3 adds, got %d", adds)
	}
}

func TestLcsLineDiff_SingleLineChangeInMiddle(t *testing.T) {
	base := []string{"a", "old", "c"}
	head := []string{"a", "new", "c"}
	hunks := lcsLineDiff(base, head)
	// LCS is ["a","c"]; middle produces one delete and one add (order depends on
	// the traceback direction, which emits '+' before '-' in this case).
	if len(hunks) != 4 {
		t.Fatalf("want 4 hunks, got %d: %v", len(hunks), hunks)
	}
	if hunks[0].kind != ' ' || hunks[0].text != "a" {
		t.Errorf("hunk[0]: want context 'a', got %q %q", hunks[0].kind, hunks[0].text)
	}
	// Middle two hunks are one delete and one add (order: '+' then '-').
	kinds12 := string([]byte{hunks[1].kind, hunks[2].kind})
	if kinds12 != "+-" && kinds12 != "-+" {
		t.Errorf("hunk[1..2]: want one '+' and one '-', got %q", kinds12)
	}
	texts := map[string]bool{hunks[1].text: true, hunks[2].text: true}
	if !texts["old"] || !texts["new"] {
		t.Errorf("hunk[1..2]: want texts 'old' and 'new', got %q %q", hunks[1].text, hunks[2].text)
	}
	if hunks[3].kind != ' ' || hunks[3].text != "c" {
		t.Errorf("hunk[3]: want context 'c', got %q %q", hunks[3].kind, hunks[3].text)
	}
}

func TestLcsLineDiff_AddedLinesOnly(t *testing.T) {
	base := []string{"a", "b"}
	head := []string{"a", "b", "c", "d"}
	hunks := lcsLineDiff(base, head)
	kinds := lcsKinds(hunks)
	if strings.Count(kinds, "+") != 2 {
		t.Errorf("want 2 adds, kinds: %q", kinds)
	}
	if strings.Count(kinds, "-") != 0 {
		t.Errorf("want 0 deletes, kinds: %q", kinds)
	}
	if strings.Count(kinds, " ") != 2 {
		t.Errorf("want 2 context lines, kinds: %q", kinds)
	}
}

func TestLcsLineDiff_DeletedLinesOnly(t *testing.T) {
	base := []string{"a", "b", "c", "d"}
	head := []string{"a", "b"}
	hunks := lcsLineDiff(base, head)
	kinds := lcsKinds(hunks)
	if strings.Count(kinds, "-") != 2 {
		t.Errorf("want 2 deletes, kinds: %q", kinds)
	}
	if strings.Count(kinds, "+") != 0 {
		t.Errorf("want 0 adds, kinds: %q", kinds)
	}
	if strings.Count(kinds, " ") != 2 {
		t.Errorf("want 2 context lines, kinds: %q", kinds)
	}
}

func TestLcsLineDiff_EmptyBase(t *testing.T) {
	head := []string{"x", "y", "z"}
	hunks := lcsLineDiff(nil, head)
	if len(hunks) != 3 {
		t.Fatalf("want 3 hunks, got %d", len(hunks))
	}
	for _, h := range hunks {
		if h.kind != '+' {
			t.Errorf("want all adds, got %q", h.kind)
		}
	}
}

func TestLcsLineDiff_EmptyHead(t *testing.T) {
	base := []string{"x", "y", "z"}
	hunks := lcsLineDiff(base, nil)
	if len(hunks) != 3 {
		t.Fatalf("want 3 hunks, got %d", len(hunks))
	}
	for _, h := range hunks {
		if h.kind != '-' {
			t.Errorf("want all deletes, got %q", h.kind)
		}
	}
}

func TestLcsLineDiff_BothEmpty(t *testing.T) {
	hunks := lcsLineDiff(nil, nil)
	if len(hunks) != 0 {
		t.Errorf("want 0 hunks for both empty, got %d", len(hunks))
	}
}

// --- computeFileDiff tests ---

func TestComputeFileDiff_AddedFile(t *testing.T) {
	head := []byte("line1\nline2\n")
	data := computeFileDiff(nil, head, "+")
	if data.err != "" {
		t.Fatalf("unexpected error: %q", data.err)
	}
	if len(data.hunks) != 2 {
		t.Fatalf("want 2 hunks, got %d", len(data.hunks))
	}
	for _, h := range data.hunks {
		if h.kind != '+' {
			t.Errorf("want all adds for added file, got %q", h.kind)
		}
	}
	if data.hunks[0].text != "line1" || data.hunks[1].text != "line2" {
		t.Errorf("unexpected texts: %v", data.hunks)
	}
}

func TestComputeFileDiff_DeletedFile(t *testing.T) {
	base := []byte("line1\nline2\n")
	data := computeFileDiff(base, nil, "-")
	if data.err != "" {
		t.Fatalf("unexpected error: %q", data.err)
	}
	if len(data.hunks) != 2 {
		t.Fatalf("want 2 hunks, got %d", len(data.hunks))
	}
	for _, h := range data.hunks {
		if h.kind != '-' {
			t.Errorf("want all deletes for deleted file, got %q", h.kind)
		}
	}
}

func TestComputeFileDiff_ModifiedFile(t *testing.T) {
	base := []byte("a\nb\nc\n")
	head := []byte("a\nB\nc\n")
	data := computeFileDiff(base, head, "~")
	if data.err != "" {
		t.Fatalf("unexpected error: %q", data.err)
	}
	// Expect: ' a', '-b', '+B', ' c'
	if len(data.hunks) != 4 {
		t.Fatalf("want 4 hunks, got %d", len(data.hunks))
	}
}

func TestComputeFileDiff_TooLarge(t *testing.T) {
	// Build slices exceeding 4000 combined lines.
	bigLine := strings.Repeat("x", 10)
	base := []byte(strings.Repeat(bigLine+"\n", 2001))
	head := []byte(strings.Repeat(bigLine+"\n", 2001))
	data := computeFileDiff(base, head, "~")
	if data.err == "" {
		t.Error("want error for too-large file, got none")
	}
	if !strings.Contains(data.err, "too large") {
		t.Errorf("want 'too large' in error, got %q", data.err)
	}
}

func TestComputeFileDiff_EmptyFiles(t *testing.T) {
	// Both empty, modified.
	data := computeFileDiff(nil, nil, "~")
	if data.err != "" {
		t.Errorf("unexpected error for empty~empty: %q", data.err)
	}
	if len(data.hunks) != 0 {
		t.Errorf("want 0 hunks for empty~empty, got %d", len(data.hunks))
	}

	// Empty head added file.
	data = computeFileDiff(nil, nil, "+")
	if data.err != "" {
		t.Errorf("unexpected error for empty+: %q", data.err)
	}
	if len(data.hunks) != 0 {
		t.Errorf("want 0 hunks for empty added file, got %d", len(data.hunks))
	}

	// Empty base deleted file.
	data = computeFileDiff(nil, nil, "-")
	if data.err != "" {
		t.Errorf("unexpected error for empty-: %q", data.err)
	}
	if len(data.hunks) != 0 {
		t.Errorf("want 0 hunks for empty deleted file, got %d", len(data.hunks))
	}
}

// --- Edge case tests ---

func TestLcsLineDiff_WhitespaceOnlyLines(t *testing.T) {
	base := []string{"   ", "\t", "   "}
	head := []string{"   ", "X", "   "}
	hunks := lcsLineDiff(base, head)
	kinds := lcsKinds(hunks)
	if strings.Count(kinds, "-") != 1 || strings.Count(kinds, "+") != 1 {
		t.Errorf("want 1 delete and 1 add, got kinds %q", kinds)
	}
	if strings.Count(kinds, " ") != 2 {
		t.Errorf("want 2 context lines, got kinds %q", kinds)
	}
}

func TestLcsLineDiff_SingleLineFiles(t *testing.T) {
	// Same single line.
	hunks := lcsLineDiff([]string{"hello"}, []string{"hello"})
	if len(hunks) != 1 || hunks[0].kind != ' ' {
		t.Errorf("want 1 context hunk, got %v", hunks)
	}

	// Different single lines.
	hunks = lcsLineDiff([]string{"hello"}, []string{"world"})
	kinds := lcsKinds(hunks)
	if strings.Count(kinds, "-") != 1 || strings.Count(kinds, "+") != 1 {
		t.Errorf("want 1 delete + 1 add, got %q", kinds)
	}
}

func TestComputeFileDiff_TrailingNewlineDifferences(t *testing.T) {
	// File with trailing newline vs without.
	withNewline := []byte("line1\nline2\n")
	withoutNewline := []byte("line1\nline2")
	data := computeFileDiff(withNewline, withoutNewline, "~")
	if data.err != "" {
		t.Fatalf("unexpected error: %q", data.err)
	}
	// Both should produce 2 lines after splitLines trims trailing empty.
	// Content is identical so all context.
	for _, h := range data.hunks {
		if h.kind != ' ' {
			t.Errorf("trailing newline diff should produce context lines, got %q", h.kind)
		}
	}
}

func TestComputeFileDiff_SingleLineFiles(t *testing.T) {
	base := []byte("only\n")
	head := []byte("only\n")
	data := computeFileDiff(base, head, "~")
	if data.err != "" {
		t.Fatalf("unexpected error: %q", data.err)
	}
	if len(data.hunks) != 1 || data.hunks[0].kind != ' ' {
		t.Errorf("want 1 context hunk for identical single-line files, got %v", data.hunks)
	}
}
