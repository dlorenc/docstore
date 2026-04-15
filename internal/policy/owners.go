package policy

import (
	"bufio"
	"bytes"
	"path"
	"strings"
)

// ParseOwners builds a directory → owners map from a set of OWNERS file
// contents. The files map should use repo-relative paths as keys
// (e.g. "OWNERS", "docs/OWNERS"). Only files whose base name is "OWNERS"
// are processed; all others are silently ignored.
//
// The resulting map keys are directory paths without trailing slashes
// (root is represented by the empty string "").
func ParseOwners(files map[string][]byte) map[string][]string {
	owners := make(map[string][]string)
	for filePath, content := range files {
		if path.Base(filePath) != "OWNERS" {
			continue
		}
		dir := path.Dir(filePath)
		if dir == "." {
			dir = ""
		}
		ids := parseOwnersFile(content)
		if len(ids) > 0 {
			owners[dir] = ids
		}
	}
	return owners
}

// parseOwnersFile parses a single OWNERS file.
// Lines starting with '#' are treated as comments.
// Empty or whitespace-only lines are skipped.
// Each remaining line is expected to be an owner identity string.
func parseOwnersFile(content []byte) []string {
	var owners []string
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		owners = append(owners, line)
	}
	return owners
}
