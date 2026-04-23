// Package hash provides the canonical commit chain hash function for docstore.
// Both the server (internal/db) and client (internal/cli) must use this function
// to ensure identical hash computation.
package hash

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
	"time"
)

// GenesisHash is the all-zeros hash used as the previous-hash for the first chain entry.
const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// File holds a path and content hash for commit hash computation.
type File struct {
	Path        string `json:"path"`
	ContentHash string `json:"content_hash"`
}

// CommitHash computes the SHA256 chain hash for a commit.
// prevHash is the hex-encoded hash of the previous commit (or GenesisHash for the first commit).
// Files are sorted by path internally, so caller order does not matter.
func CommitHash(prevHash string, seq int64, repo, branch, author, message string, createdAt time.Time, files []File) string {
	sorted := slices.Clone(files)
	slices.SortFunc(sorted, func(a, b File) int { return strings.Compare(a.Path, b.Path) })

	h := sha256.New()
	h.Write([]byte(prevHash + "\n"))
	h.Write([]byte(strconv.FormatInt(seq, 10) + "\n"))
	h.Write([]byte(repo + "\n"))
	h.Write([]byte(branch + "\n"))
	h.Write([]byte(author + "\n"))
	h.Write([]byte(message + "\n"))
	h.Write([]byte(createdAt.UTC().Format(time.RFC3339Nano) + "\n"))
	for _, f := range sorted {
		h.Write([]byte(f.Path + ":" + f.ContentHash + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}
