package ui

import (
	"fmt"
	"strings"
)

// commitDiffLine is one rendered line in an inline file diff.
type commitDiffLine struct {
	Kind    string // "add", "del", "ctx", "hunk"
	Content string // display content including +/- prefix
}

// commitFileDiffData is the template data for the commit file diff partial.
type commitFileDiffData struct {
	Lines  []commitDiffLine
	Binary bool
}

// computeFileDiff builds an inline diff between old and new file content.
// Returns a binary indicator if either side is not plain text.
// A size cap of 512 KB per side prevents O(n²) memory use on huge files.
func computeFileDiff(oldContent, newContent []byte, _ string) commitFileDiffData {
	const sizeLimit = 512 * 1024
	if !isProbablyText(oldContent) || !isProbablyText(newContent) {
		return commitFileDiffData{Binary: true}
	}
	if len(oldContent) > sizeLimit || len(newContent) > sizeLimit {
		return commitFileDiffData{Binary: true}
	}

	oldLines := splitIntoLines(oldContent)
	newLines := splitIntoLines(newContent)

	ops := lcsEditScript(oldLines, newLines)
	lines := opsToHunks(ops, 3)
	return commitFileDiffData{Lines: lines}
}

// splitIntoLines splits content into lines without trailing newlines.
func splitIntoLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	s := strings.TrimRight(string(b), "\n")
	return strings.Split(s, "\n")
}

type editOp struct {
	kind    string // "ctx", "add", "del"
	content string
}

// lcsEditScript produces the shortest edit script transforming a into b using
// an O(m·n) LCS dynamic-programming approach.
func lcsEditScript(a, b []string) []editOp {
	m, n := len(a), len(b)

	// dp[i][j] = length of LCS of a[:i] and b[:j]
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if a[i-1] == b[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] >= dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}

	// Backtrack
	ops := make([]editOp, 0, m+n)
	i, j := m, n
	for i > 0 || j > 0 {
		switch {
		case i > 0 && j > 0 && a[i-1] == b[j-1]:
			ops = append(ops, editOp{"ctx", a[i-1]})
			i--
			j--
		case j > 0 && (i == 0 || dp[i][j-1] >= dp[i-1][j]):
			ops = append(ops, editOp{"add", b[j-1]})
			j--
		default:
			ops = append(ops, editOp{"del", a[i-1]})
			i--
		}
	}

	// Reverse to get forward order
	for l, r := 0, len(ops)-1; l < r; l, r = l+1, r-1 {
		ops[l], ops[r] = ops[r], ops[l]
	}
	return ops
}

// opsToHunks groups edit ops into unified-diff hunks with ctx context lines.
func opsToHunks(ops []editOp, ctx int) []commitDiffLine {
	n := len(ops)
	if n == 0 {
		return nil
	}

	// Mark lines that should appear in output (within ctx of a change).
	include := make([]bool, n)
	for i, op := range ops {
		if op.kind != "ctx" {
			for j := max(0, i-ctx); j <= min(n-1, i+ctx); j++ {
				include[j] = true
			}
		}
	}

	var result []commitDiffLine
	i := 0
	for i < n {
		if !include[i] {
			i++
			continue
		}

		// Find end of this hunk.
		end := i
		for end < n && include[end] {
			end++
		}

		// Compute hunk header line numbers.
		oldStart, newStart := 1, 1
		for k := 0; k < i; k++ {
			if ops[k].kind != "add" {
				oldStart++
			}
			if ops[k].kind != "del" {
				newStart++
			}
		}
		oldCount, newCount := 0, 0
		for k := i; k < end; k++ {
			if ops[k].kind != "add" {
				oldCount++
			}
			if ops[k].kind != "del" {
				newCount++
			}
		}

		result = append(result, commitDiffLine{
			Kind:    "hunk",
			Content: fmt.Sprintf("@@ -%d,%d +%d,%d @@", oldStart, oldCount, newStart, newCount),
		})

		for k := i; k < end; k++ {
			op := ops[k]
			prefix := " "
			if op.kind == "add" {
				prefix = "+"
			} else if op.kind == "del" {
				prefix = "-"
			}
			result = append(result, commitDiffLine{Kind: op.kind, Content: prefix + op.content})
		}

		i = end
	}
	return result
}
