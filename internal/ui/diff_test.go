package ui

import (
	"strings"
	"testing"
)

func TestComputeFileDiff(t *testing.T) {
	t.Run("identical files produce no diff lines", func(t *testing.T) {
		content := []byte("line1\nline2\nline3\n")
		d := computeFileDiff(content, content, "file.txt")
		if d.Binary {
			t.Fatal("expected non-binary")
		}
		if len(d.Lines) != 0 {
			t.Fatalf("expected empty diff, got %d lines", len(d.Lines))
		}
	})

	t.Run("new file shows all additions", func(t *testing.T) {
		d := computeFileDiff(nil, []byte("a\nb\n"), "file.txt")
		if d.Binary {
			t.Fatal("expected non-binary")
		}
		adds := countKind(d.Lines, "add")
		if adds != 2 {
			t.Fatalf("expected 2 add lines, got %d", adds)
		}
	})

	t.Run("deleted file shows all deletions", func(t *testing.T) {
		d := computeFileDiff([]byte("a\nb\n"), nil, "file.txt")
		if d.Binary {
			t.Fatal("expected non-binary")
		}
		dels := countKind(d.Lines, "del")
		if dels != 2 {
			t.Fatalf("expected 2 del lines, got %d", dels)
		}
	})

	t.Run("modified line shows add and del", func(t *testing.T) {
		old := []byte("line1\nline2\nline3\n")
		new := []byte("line1\nLINE2\nline3\n")
		d := computeFileDiff(old, new, "file.txt")
		if d.Binary {
			t.Fatal("expected non-binary")
		}
		if countKind(d.Lines, "add") != 1 {
			t.Fatalf("expected 1 add line, got %d", countKind(d.Lines, "add"))
		}
		if countKind(d.Lines, "del") != 1 {
			t.Fatalf("expected 1 del line, got %d", countKind(d.Lines, "del"))
		}
		if countKind(d.Lines, "hunk") == 0 {
			t.Fatal("expected at least one hunk header")
		}
	})

	t.Run("binary content flagged as binary", func(t *testing.T) {
		bin := []byte{0x00, 0x01, 0x02}
		d := computeFileDiff(bin, bin, "file.bin")
		if !d.Binary {
			t.Fatal("expected binary")
		}
	})

	t.Run("add line starts with +", func(t *testing.T) {
		d := computeFileDiff(nil, []byte("hello\n"), "file.txt")
		for _, l := range d.Lines {
			if l.Kind == "add" && !strings.HasPrefix(l.Content, "+") {
				t.Errorf("add line missing + prefix: %q", l.Content)
			}
		}
	})

	t.Run("del line starts with -", func(t *testing.T) {
		d := computeFileDiff([]byte("hello\n"), nil, "file.txt")
		for _, l := range d.Lines {
			if l.Kind == "del" && !strings.HasPrefix(l.Content, "-") {
				t.Errorf("del line missing - prefix: %q", l.Content)
			}
		}
	})
}

func countKind(lines []commitDiffLine, kind string) int {
	n := 0
	for _, l := range lines {
		if l.Kind == kind {
			n++
		}
	}
	return n
}
