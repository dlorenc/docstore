// Package cli implements the ds command-line client for docstore.
package cli

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/tui"
)


// commitInfo is a local type for decoding GET /commit/:seq server responses.
type commitInfo struct {
	Sequence  int64            `json:"sequence"`
	Branch    string           `json:"branch"`
	Message   string           `json:"message"`
	Author    string           `json:"author"`
	CreatedAt time.Time        `json:"created_at"`
	Files     []commitFileInfo `json:"files"`
}

type commitFileInfo struct {
	Path      string  `json:"path"`
	VersionID *string `json:"version_id"`
}

// fileHistEntry is a local type for decoding file history server responses.
type fileHistEntry struct {
	Sequence  int64     `json:"sequence"`
	VersionID *string   `json:"version_id"`
	Message   string    `json:"message"`
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"created_at"`
}

const configDir = ".docstore"
const configFile = "config.json"
const stateFile = "state.json"

// chainEntry is the JSON shape returned by GET /-/chain.
type chainEntry struct {
	Sequence   int64       `json:"sequence"`
	Branch     string      `json:"branch"`
	Author     string      `json:"author"`
	Message    string      `json:"message"`
	CreatedAt  time.Time   `json:"created_at"`
	CommitHash *string     `json:"commit_hash"`
	Files      []chainFile `json:"files"`
}

// chainFile is one file in a chainEntry.
type chainFile struct {
	Path        string `json:"path"`
	ContentHash string `json:"content_hash"`
}

// genesisHash is the all-zeros hash used as the previous-hash for the first chain entry.
const genesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// computeChainHash recomputes the SHA256 chain hash for a commit entry.
// This must exactly match the server-side formula in internal/db/store.go.
func computeChainHash(prevHash string, seq int64, repo, branch, author, message string, createdAt time.Time, files []chainFile) string {
	h := sha256.New()
	h.Write([]byte(prevHash + "\n"))
	h.Write([]byte(fmt.Sprintf("%d\n", seq)))
	h.Write([]byte(repo + "\n"))
	h.Write([]byte(branch + "\n"))
	h.Write([]byte(author + "\n"))
	h.Write([]byte(message + "\n"))
	h.Write([]byte(createdAt.UTC().Format(time.RFC3339Nano) + "\n"))
	sorted := make([]chainFile, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	for _, f := range sorted {
		h.Write([]byte(f.Path + ":" + f.ContentHash + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// fetchChain calls GET /-/chain?from=N&to=M and returns the decoded entries.
func (a *App) fetchChain(cfg *Config, from, to int64) ([]chainEntry, error) {
	q := url.Values{}
	q.Set("from", fmt.Sprintf("%d", from))
	q.Set("to", fmt.Sprintf("%d", to))
	resp, err := a.httpGet(cfg, repoBase(cfg)+"/chain?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("fetching chain: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, a.readError(resp)
	}
	var entries []chainEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, fmt.Errorf("decoding chain: %w", err)
	}
	return entries, nil
}

// Config is the persistent CLI configuration stored in .docstore/config.json.
type Config struct {
	Remote string `json:"remote"`
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Author string `json:"author"`
}

// State tracks the last-synced tree for offline status.
type State struct {
	Branch     string            `json:"branch"`
	Sequence   int64             `json:"sequence"`
	Files      map[string]string `json:"files"`      // path -> content_hash
	CommitHash string            `json:"commit_hash"` // tip commit_hash for chain verification; empty = not yet set
}

// App is the CLI application. All fields are injectable for testing.
type App struct {
	Dir           string       // working directory
	Out           io.Writer    // output writer
	HTTP          *http.Client // HTTP client (mockable)
	DefaultRemote string       // fallback remote URL when no config file exists
}

func (a *App) configPath() string {
	return filepath.Join(a.Dir, configDir, configFile)
}

func (a *App) statePath() string {
	return filepath.Join(a.Dir, configDir, stateFile)
}

func (a *App) loadConfig() (*Config, error) {
	data, err := os.ReadFile(a.configPath())
	if err != nil {
		return nil, fmt.Errorf("not a docstore workspace (run 'ds init' first)")
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("corrupt config: %w", err)
	}
	return &cfg, nil
}

// loadRemote returns the server remote URL. It first tries to read
// .docstore/config.json; if absent it falls back to App.DefaultRemote.
func (a *App) loadRemote() (string, error) {
	data, err := os.ReadFile(a.configPath())
	if err == nil {
		var cfg Config
		if jsonErr := json.Unmarshal(data, &cfg); jsonErr == nil && cfg.Remote != "" {
			return cfg.Remote, nil
		}
	}
	if a.DefaultRemote != "" {
		return a.DefaultRemote, nil
	}
	return "", fmt.Errorf("not a docstore workspace (run 'ds init' first)")
}

func (a *App) saveConfig(cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(a.configPath(), data, 0644)
}

func (a *App) loadState() (*State, error) {
	data, err := os.ReadFile(a.statePath())
	if err != nil {
		return &State{Files: make(map[string]string)}, nil
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("corrupt state: %w", err)
	}
	if st.Files == nil {
		st.Files = make(map[string]string)
	}
	return &st, nil
}

func (a *App) saveState(st *State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(a.statePath(), data, 0644)
}

// scanLocalFiles walks the working directory and returns a map of path -> content_hash.
// It skips the .docstore directory.
func (a *App) scanLocalFiles() (map[string]string, error) {
	localFiles := make(map[string]string)
	err := filepath.Walk(a.Dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(a.Dir, path)
		if rel == "." {
			return nil
		}
		if strings.HasPrefix(rel, configDir) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			return nil
		}
		hash, err := hashFile(path)
		if err != nil {
			return err
		}
		localFiles[filepath.ToSlash(rel)] = hash
		return nil
	})
	return localFiles, err
}

// hashFile returns the hex-encoded SHA256 of a file's contents.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// HashBytes returns the hex-encoded SHA256 of the given bytes.
func HashBytes(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// detectContentType returns a MIME type string for binary files, or empty string
// for text files. Binary detection uses utf8.Valid; the MIME type is derived from
// the file extension, defaulting to "application/octet-stream".
func detectContentType(path string, content []byte) string {
	if utf8.Valid(content) {
		return ""
	}
	ct := mime.TypeByExtension(filepath.Ext(path))
	if ct == "" {
		ct = "application/octet-stream"
	}
	return ct
}

// isDirty returns true if there are local uncommitted changes.
func (a *App) isDirty() (bool, error) {
	st, err := a.loadState()
	if err != nil {
		return false, err
	}
	localFiles, err := a.scanLocalFiles()
	if err != nil {
		return false, err
	}
	for path, hash := range localFiles {
		if stateHash, ok := st.Files[path]; !ok || hash != stateHash {
			return true, nil
		}
	}
	for path := range st.Files {
		if _, ok := localFiles[path]; !ok {
			return true, nil
		}
	}
	return false, nil
}

// Init creates a new docstore workspace and fetches all files from the server.
// remote may include a /repos/:name suffix (e.g. https://host/repos/myrepo),
// or repo may be provided separately via the repo parameter.
// If repo is empty and remote doesn't contain a /repos/ segment, "default" is used.
func (a *App) Init(remote, repo, author string) error {
	dir := filepath.Join(a.Dir, configDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	if author == "" {
		u, err := user.Current()
		if err == nil {
			author = u.Username
		} else {
			author = "unknown"
		}
	}

	// Extract repo from remote URL if embedded.
	// Supported forms:
	//   https://host/repos/owner/name/-    (canonical new form with /-/)
	//   https://host/repos/owner/name      (bare repo path)
	//   https://host                       (no repo path; use default/default)
	// Strip the /repos/... suffix so that we never end up with a doubled path prefix.
	baseRemote := strings.TrimRight(remote, "/")
	// Strip optional trailing /-
	baseRemote = strings.TrimSuffix(baseRemote, "/-")
	if idx := strings.Index(baseRemote, "/repos/"); idx >= 0 {
		if repo == "" {
			repo = baseRemote[idx+len("/repos/"):]
		}
		baseRemote = baseRemote[:idx]
	} else if repo == "" {
		repo = "default/default"
	}

	cfg := &Config{
		Remote: baseRemote,
		Repo:   repo,
		Branch: "main",
		Author: author,
	}
	if err := a.saveConfig(cfg); err != nil {
		return err
	}

	// Save empty initial state.
	st := &State{
		Branch:   "main",
		Sequence: 0,
		Files:    make(map[string]string),
	}
	if err := a.saveState(st); err != nil {
		return err
	}

	// Fetch files from the server.
	if err := a.syncTree(cfg, "main", false); err != nil {
		return err
	}

	// Report result using the updated state.
	st, err := a.loadState()
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Out, "Initialized docstore workspace (%d files, sequence %d)\n", len(st.Files), st.Sequence)
	fmt.Fprintf(a.Out, "Remote: %s\n", cfg.Remote)
	fmt.Fprintf(a.Out, "Author: %s\n", author)
	return nil
}

// Status shows files that differ from the last-synced state.
func (a *App) Status() error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	st, err := a.loadState()
	if err != nil {
		return err
	}

	fmt.Fprintf(a.Out, "On branch %s\n", cfg.Branch)

	localFiles, err := a.scanLocalFiles()
	if err != nil {
		return fmt.Errorf("scanning directory: %w", err)
	}

	var newFiles, modifiedFiles, deletedFiles []string

	for path, hash := range localFiles {
		if stateHash, ok := st.Files[path]; !ok {
			newFiles = append(newFiles, path)
		} else if hash != stateHash {
			modifiedFiles = append(modifiedFiles, path)
		}
	}

	for path := range st.Files {
		if _, ok := localFiles[path]; !ok {
			deletedFiles = append(deletedFiles, path)
		}
	}

	sort.Strings(newFiles)
	sort.Strings(modifiedFiles)
	sort.Strings(deletedFiles)

	if len(newFiles) == 0 && len(modifiedFiles) == 0 && len(deletedFiles) == 0 {
		fmt.Fprintf(a.Out, "No changes\n")
		return nil
	}

	fmt.Fprintf(a.Out, "\nChanges:\n")
	for _, f := range newFiles {
		fmt.Fprintf(a.Out, "  new:      %s\n", f)
	}
	for _, f := range modifiedFiles {
		fmt.Fprintf(a.Out, "  modified: %s\n", f)
	}
	for _, f := range deletedFiles {
		fmt.Fprintf(a.Out, "  deleted:  %s\n", f)
	}

	return nil
}

// Commit creates a commit with all local changes.
func (a *App) Commit(message string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	st, err := a.loadState()
	if err != nil {
		return err
	}

	localFiles, err := a.scanLocalFiles()
	if err != nil {
		return fmt.Errorf("scanning directory: %w", err)
	}

	var files []model.FileChange

	// New and modified files.
	for path, hash := range localFiles {
		stateHash, exists := st.Files[path]
		if !exists || hash != stateHash {
			content, err := os.ReadFile(filepath.Join(a.Dir, filepath.FromSlash(path)))
			if err != nil {
				return err
			}
			ct := detectContentType(path, content)
			files = append(files, model.FileChange{Path: path, Content: content, ContentType: ct})
		}
	}

	// Deleted files (nil Content = delete).
	for path := range st.Files {
		if _, ok := localFiles[path]; !ok {
			files = append(files, model.FileChange{Path: path})
		}
	}

	if len(files) == 0 {
		return fmt.Errorf("nothing to commit")
	}

	// Sort for deterministic ordering.
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	req := model.CommitRequest{
		Branch:  cfg.Branch,
		Files:   files,
		Message: message,
		Author:  cfg.Author,
	}

	resp, err := a.postJSON(cfg, repoBase(cfg)+"/commit", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return a.readError(resp)
	}

	var commitResp model.CommitResponse
	if err := json.NewDecoder(resp.Body).Decode(&commitResp); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}

	// Update state to reflect current local tree.
	st.Files = localFiles
	st.Sequence = commitResp.Sequence
	st.Branch = cfg.Branch

	// Store the commit_hash returned directly in the response (FIX-1).
	if commitResp.CommitHash != "" {
		st.CommitHash = commitResp.CommitHash
	} else {
		fmt.Fprintf(a.Out, "warning: server did not return commit_hash, chain verification may be incomplete\n")
	}

	if err := a.saveState(st); err != nil {
		return err
	}

	fmt.Fprintf(a.Out, "Committed sequence %d (%d files)\n", commitResp.Sequence, len(files))
	return nil
}

// CheckoutNew creates a new branch and switches to it.
func (a *App) CheckoutNew(branch string, draft bool) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}

	req := model.CreateBranchRequest{Name: branch, Draft: draft}
	resp, err := a.postJSON(cfg, repoBase(cfg)+"/branch", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}

	var branchResp model.CreateBranchResponse
	if err := json.NewDecoder(resp.Body).Decode(&branchResp); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}

	cfg.Branch = branch
	if err := a.saveConfig(cfg); err != nil {
		return err
	}

	// State carries over from current branch.
	st, err := a.loadState()
	if err != nil {
		return err
	}
	st.Branch = branch
	st.Sequence = branchResp.BaseSequence
	if err := a.saveState(st); err != nil {
		return err
	}

	if draft {
		fmt.Fprintf(a.Out, "Switched to new draft branch '%s' (base sequence %d)\n", branch, branchResp.BaseSequence)
	} else {
		fmt.Fprintf(a.Out, "Switched to new branch '%s' (base sequence %d)\n", branch, branchResp.BaseSequence)
	}
	return nil
}

// Ready marks the current branch as not draft (ready to merge).
func (a *App) Ready() error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if cfg.Branch == "" || cfg.Branch == "main" {
		return fmt.Errorf("no branch checked out (or on main)")
	}

	req := model.UpdateBranchRequest{Draft: false}
	resp, err := a.patchJSON(cfg, repoBase(cfg)+"/branch/"+cfg.Branch, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}

	fmt.Fprintf(a.Out, "Branch '%s' marked as ready\n", cfg.Branch)
	return nil
}

// AutoMergeEnable enables auto-merge on the current (or specified) branch.
// When all CI checks pass and merge policies are satisfied, the branch will
// be merged automatically.
func (a *App) AutoMergeEnable(branch string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if branch == "" {
		branch = cfg.Branch
	}
	if branch == "" || branch == "main" {
		return fmt.Errorf("no branch checked out (or on main)")
	}

	resp, err := a.postJSON(cfg, repoBase(cfg)+"/branch/"+branch+"/auto-merge", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		return a.readError(resp)
	}

	fmt.Fprintf(a.Out, "Auto-merge enabled for branch '%s'\n", branch)
	return nil
}

// AutoMergeDisable disables auto-merge on the current (or specified) branch.
func (a *App) AutoMergeDisable(branch string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if branch == "" {
		branch = cfg.Branch
	}
	if branch == "" || branch == "main" {
		return fmt.Errorf("no branch checked out (or on main)")
	}

	req, err := http.NewRequest("DELETE", repoBase(cfg)+"/branch/"+branch+"/auto-merge", nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-DocStore-Identity", cfg.Author)
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		return a.readError(resp)
	}

	fmt.Fprintf(a.Out, "Auto-merge disabled for branch '%s'\n", branch)
	return nil
}

// Checkout switches to an existing branch and syncs files from the server.
func (a *App) Checkout(branch string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}

	// Refuse if there are uncommitted local changes.
	dirty, err := a.isDirty()
	if err != nil {
		return err
	}
	if dirty {
		return fmt.Errorf("uncommitted changes -- commit or discard first")
	}

	cfg.Branch = branch
	if err := a.saveConfig(cfg); err != nil {
		return err
	}

	return a.syncTree(cfg, branch, false)
}

// Pull syncs local files from the current branch on the server.
// If state has a stored CommitHash, it verifies the chain from the stored
// sequence to the new head before updating state. On first pull (no stored
// CommitHash), it uses TOFU and stores the tip's commit_hash.
// If skipVerify is true, chain verification is skipped but state is still updated normally.
func (a *App) Pull(skipVerify bool) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}

	// Refuse if there are uncommitted local changes.
	dirty, err := a.isDirty()
	if err != nil {
		return err
	}
	if dirty {
		return fmt.Errorf("uncommitted changes -- commit or discard first")
	}

	// Save old state before sync (for chain verification).
	oldState, err := a.loadState()
	if err != nil {
		return err
	}

	if err := a.syncTree(cfg, cfg.Branch, skipVerify); err != nil {
		return err
	}

	// Load updated state (syncTree saved it).
	newState, err := a.loadState()
	if err != nil {
		return err
	}

	// Chain verification / TOFU.
	if skipVerify {
		return nil
	}
	if newState.Sequence == 0 {
		// No commits yet; nothing to verify.
		return nil
	}

	if oldState.CommitHash == "" {
		// TOFU: fetch the tip commit's hash and store it.
		entries, err := a.fetchChain(cfg, newState.Sequence, newState.Sequence)
		if err != nil {
			// Non-fatal: chain endpoint may not be available on older servers.
			fmt.Fprintf(a.Out, "warning: could not fetch chain for TOFU: %v\n", err)
			return nil
		}
		if len(entries) > 0 && entries[0].CommitHash != nil {
			newState.CommitHash = *entries[0].CommitHash
			return a.saveState(newState)
		}
		// Tip has NULL commit_hash (pre-feature commit); keep CommitHash empty
		// so we retry TOFU on the next pull until a hashed tip is available.
		if len(entries) > 0 {
			fmt.Fprintf(a.Out, "note: current tip (seq %d) predates hash chain feature; chain verification will begin on next commit\n", entries[0].Sequence)
		}
		return nil
	}

	// Verification: old state has a known commit hash; verify the chain forward.
	if oldState.Sequence == newState.Sequence {
		// Nothing changed; no verification needed.
		return nil
	}

	// Fetch from oldState.Sequence (inclusive) to newState.Sequence.
	// The first entry is our anchor: its stored commit_hash must match oldState.CommitHash.
	entries, err := a.fetchChain(cfg, oldState.Sequence, newState.Sequence)
	if err != nil {
		return fmt.Errorf("chain fetch for verification: %w", err)
	}

	if len(entries) == 0 {
		return nil
	}

	// Anchor check: the server's hash for oldState.Sequence must match what we stored.
	anchor := entries[0]
	if anchor.CommitHash == nil {
		// Pre-feature commit at anchor; treat as reset.
		fmt.Fprintf(a.Out, "note: skipping pre-feature commit at sequence %d (no commit_hash)\n", anchor.Sequence)
	} else if *anchor.CommitHash != oldState.CommitHash {
		return fmt.Errorf("chain integrity error at sequence %d: anchor hash changed (stored %s, server %s)",
			anchor.Sequence, oldState.CommitHash, *anchor.CommitHash)
	}

	// Verify chain linkage for entries after the anchor.
	// Only process entries on the current branch (per-branch chain semantics).
	prevHash := oldState.CommitHash
	if anchor.CommitHash == nil {
		prevHash = genesisHash
	}
	skippedBoundary := false
	for _, e := range entries[1:] {
		if e.Branch != cfg.Branch {
			// Skip commits on other branches; each branch has its own independent chain.
			continue
		}
		if e.CommitHash == nil {
			if !skippedBoundary {
				fmt.Fprintf(a.Out, "note: skipping pre-feature commit at sequence %d (no commit_hash)\n", e.Sequence)
				skippedBoundary = true
			}
			prevHash = genesisHash
			continue
		}
		computed := computeChainHash(prevHash, e.Sequence, cfg.Repo, e.Branch, e.Author, e.Message, e.CreatedAt, e.Files)
		if computed != *e.CommitHash {
			return fmt.Errorf("chain integrity error at sequence %d: expected %s got %s", e.Sequence, computed, *e.CommitHash)
		}
		prevHash = *e.CommitHash
	}

	newState.CommitHash = prevHash
	return a.saveState(newState)
}

// Verify walks the commit chain from the first non-NULL commit_hash on the current
// branch to the server's current head and prints per-commit verification status to a.Out.
func (a *App) Verify() error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}

	// Fetch the current branch head from the server (not from local state) so
	// we don't miss commits made after the last pull (FIX-3).
	headSeq, err := a.branchHeadSequence(cfg, cfg.Branch)
	if err != nil {
		return fmt.Errorf("fetching branch head: %w", err)
	}
	if headSeq == 0 {
		fmt.Fprintf(a.Out, "No commits to verify.\n")
		return nil
	}

	// Fetch the full chain from sequence 1 to server head.
	entries, err := a.fetchChain(cfg, 1, headSeq)
	if err != nil {
		return fmt.Errorf("fetching chain: %w", err)
	}

	if len(entries) == 0 {
		fmt.Fprintf(a.Out, "No commits to verify.\n")
		return nil
	}

	prevHash := genesisHash
	foundFirst := false
	var verifyErr error
	for _, e := range entries {
		// Skip commits on other branches; each branch has its own independent chain.
		if e.Branch != cfg.Branch {
			continue
		}
		if e.CommitHash == nil {
			fmt.Fprintf(a.Out, "seq %-4d  SKIP  (no commit_hash)  %s\n", e.Sequence, e.Message)
			// Reset prevHash so the next hashed commit starts a new chain segment.
			prevHash = genesisHash
			continue
		}
		if !foundFirst {
			// The first non-NULL commit on this branch is the chain anchor; accept its hash as-is.
			foundFirst = true
			prevHash = *e.CommitHash
			fmt.Fprintf(a.Out, "seq %-4d  OK    %.16s  %s\n", e.Sequence, *e.CommitHash, e.Message)
			continue
		}
		computed := computeChainHash(prevHash, e.Sequence, cfg.Repo, e.Branch, e.Author, e.Message, e.CreatedAt, e.Files)
		if computed == *e.CommitHash {
			prevHash = *e.CommitHash
			fmt.Fprintf(a.Out, "seq %-4d  OK    %.16s  %s\n", e.Sequence, *e.CommitHash, e.Message)
		} else {
			fmt.Fprintf(a.Out, "seq %-4d  FAIL  (expected %.16s got %.16s)  %s\n",
				e.Sequence, computed, *e.CommitHash, e.Message)
			prevHash = *e.CommitHash // continue verifying remaining chain
			if verifyErr == nil {
				verifyErr = fmt.Errorf("chain integrity error at sequence %d", e.Sequence)
			}
		}
	}
	return verifyErr
}

// syncTree fetches the full tree from the server and updates local files.
// syncArchive performs an initial clone by fetching the archive endpoint and
// extracting files directly. Returns (true, nil) on success, (false, nil) if
// the server doesn't support archives (404), or (true, err) on any other error.
func (a *App) syncArchive(cfg *Config, branch string, st *State) (bool, error) {
	q := url.Values{}
	q.Set("branch", branch)
	resp, err := a.httpGet(cfg, repoBase(cfg)+"/archive?"+q.Encode())
	if err != nil {
		return true, fmt.Errorf("fetching archive: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return false, nil // server doesn't support archive endpoint
	}
	if resp.StatusCode != http.StatusOK {
		return true, fmt.Errorf("archive: unexpected status %d", resp.StatusCode)
	}

	// Extract tar entries into the working directory.
	tr := tar.NewReader(resp.Body)
	extracted := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return true, fmt.Errorf("reading archive: %w", err)
		}
		localPath := filepath.Join(a.Dir, filepath.FromSlash(hdr.Name))
		if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
			return true, err
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			return true, fmt.Errorf("reading %s from archive: %w", hdr.Name, err)
		}
		if err := os.WriteFile(localPath, content, 0644); err != nil {
			return true, err
		}
		extracted++
	}

	// Do one tree call to populate st.Files with content hashes.
	serverFiles := make(map[string]string)
	after := ""
	for {
		q := url.Values{}
		q.Set("branch", branch)
		q.Set("limit", "100")
		if after != "" {
			q.Set("after", after)
		}
		tresp, err := a.httpGet(cfg, repoBase(cfg)+"/tree?"+q.Encode())
		if err != nil {
			return true, fmt.Errorf("fetching tree after archive: %w", err)
		}
		var entries []treeEntry
		if err := json.NewDecoder(tresp.Body).Decode(&entries); err != nil {
			tresp.Body.Close()
			return true, fmt.Errorf("decoding tree: %w", err)
		}
		tresp.Body.Close()

		for _, e := range entries {
			serverFiles[e.Path] = e.ContentHash
		}
		if len(entries) < 100 {
			break
		}
		after = entries[len(entries)-1].Path
	}

	// Fetch branch head sequence.
	brResp, err := a.httpGet(cfg, repoBase(cfg)+"/branches")
	if err != nil {
		return true, fmt.Errorf("fetching branches: %w", err)
	}
	var branches []model.Branch
	if err := json.NewDecoder(brResp.Body).Decode(&branches); err != nil {
		brResp.Body.Close()
		return true, fmt.Errorf("decoding branches: %w", err)
	}
	brResp.Body.Close()
	for _, b := range branches {
		if b.Name == branch {
			st.Sequence = b.HeadSequence
			break
		}
	}

	st.Branch = branch
	st.Files = serverFiles
	if err := a.saveState(st); err != nil {
		return true, err
	}

	fmt.Fprintf(a.Out, "Synced branch '%s' (archive: %d files)\n", branch, extracted)
	return true, nil
}

// skipVerify is accepted for API consistency with Pull but is unused here;
// verification occurs in Pull after syncTree returns.
func (a *App) syncTree(cfg *Config, branch string, skipVerify bool) error {
	st, err := a.loadState()
	if err != nil {
		return err
	}

	// Fast path for initial clone: use archive endpoint if available.
	if len(st.Files) == 0 {
		ok, err := a.syncArchive(cfg, branch, st)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		// Server returned 404 for archive; fall through to tree+delta.
	}

	// Fetch full tree with pagination.
	var allEntries []treeEntry
	after := ""
	for {
		q := url.Values{}
		q.Set("branch", branch)
		q.Set("limit", "100")
		if after != "" {
			q.Set("after", after)
		}
		resp, err := a.httpGet(cfg, repoBase(cfg)+"/tree?"+q.Encode())
		if err != nil {
			return fmt.Errorf("fetching tree: %w", err)
		}

		var entries []treeEntry
		if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
			resp.Body.Close()
			return fmt.Errorf("decoding tree: %w", err)
		}
		resp.Body.Close()

		if len(entries) == 0 {
			break
		}
		allEntries = append(allEntries, entries...)
		after = entries[len(entries)-1].Path
		if len(entries) < 100 {
			break
		}
	}

	// Build server file map.
	serverFiles := make(map[string]string) // path -> content_hash
	for _, e := range allEntries {
		serverFiles[e.Path] = e.ContentHash
	}

	// Download changed/new files.
	downloaded := 0
	for _, entry := range allEntries {
		if st.Files[entry.Path] == entry.ContentHash {
			continue // already up to date
		}

		q := url.Values{}
		q.Set("branch", branch)
		resp, err := a.httpGet(cfg, repoBase(cfg)+"/file/"+entry.Path+"?"+q.Encode())
		if err != nil {
			return fmt.Errorf("fetching %s: %w", entry.Path, err)
		}

		var fileResp model.FileResponse
		if err := json.NewDecoder(resp.Body).Decode(&fileResp); err != nil {
			resp.Body.Close()
			return fmt.Errorf("decoding %s: %w", entry.Path, err)
		}
		resp.Body.Close()

		localPath := filepath.Join(a.Dir, filepath.FromSlash(entry.Path))
		if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(localPath, fileResp.Content, 0644); err != nil {
			return err
		}
		downloaded++
	}

	// Remove files that no longer exist on server.
	removed := 0
	for path := range st.Files {
		if _, ok := serverFiles[path]; !ok {
			localPath := filepath.Join(a.Dir, filepath.FromSlash(path))
			os.Remove(localPath)
			removed++
		}
	}

	// Fetch the branch's current head_sequence.
	branchesResp, err := a.httpGet(cfg, repoBase(cfg)+"/branches")
	if err != nil {
		return fmt.Errorf("fetching branches: %w", err)
	}
	var branches []model.Branch
	if err := json.NewDecoder(branchesResp.Body).Decode(&branches); err != nil {
		branchesResp.Body.Close()
		return fmt.Errorf("decoding branches: %w", err)
	}
	branchesResp.Body.Close()
	for _, b := range branches {
		if b.Name == branch {
			st.Sequence = b.HeadSequence
			break
		}
	}

	// Update state.
	st.Branch = branch
	st.Files = serverFiles
	if err := a.saveState(st); err != nil {
		return err
	}

	fmt.Fprintf(a.Out, "Synced branch '%s' (%d downloaded, %d removed)\n", branch, downloaded, removed)
	return nil
}

// Merge merges the current branch into main.
func (a *App) Merge() error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}

	if cfg.Branch == "main" {
		return fmt.Errorf("cannot merge main into itself")
	}

	req := model.MergeRequest{Branch: cfg.Branch}
	resp, err := a.postJSON(cfg, repoBase(cfg)+"/merge", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusConflict {
		var conflictErr model.MergeConflictError
		if err := json.Unmarshal(body, &conflictErr); err == nil && len(conflictErr.Conflicts) > 0 {
			fmt.Fprintf(a.Out, "Merge conflicts:\n")
			for _, c := range conflictErr.Conflicts {
				fmt.Fprintf(a.Out, "  %s (main: %s, branch: %s)\n", c.Path, c.MainVersionID, c.BranchVersionID)
			}
			return fmt.Errorf("merge aborted due to conflicts")
		}
		return fmt.Errorf("server error (status %d)", resp.StatusCode)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		var errResp model.ErrorResponse
		if err := json.Unmarshal(body, &errResp); err == nil && errResp.Error != "" {
			return fmt.Errorf("server error: %s", errResp.Error)
		}
		return fmt.Errorf("server error (status %d)", resp.StatusCode)
	}

	var mergeResp model.MergeResponse
	if err := json.Unmarshal(body, &mergeResp); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}

	fmt.Fprintf(a.Out, "Merged '%s' into main at sequence %d\n", cfg.Branch, mergeResp.Sequence)

	// Switch to main after successful merge.
	cfg.Branch = "main"
	if err := a.saveConfig(cfg); err != nil {
		return err
	}
	return a.syncTree(cfg, "main", false)
}

// Diff shows the diff for the current branch relative to its base.
func (a *App) Diff() error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}

	q := url.Values{}
	q.Set("branch", cfg.Branch)
	resp, err := a.httpGet(cfg, repoBase(cfg)+"/diff?"+q.Encode())
	if err != nil {
		return fmt.Errorf("fetching diff: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}

	var diffResp model.DiffResponse
	if err := json.NewDecoder(resp.Body).Decode(&diffResp); err != nil {
		return fmt.Errorf("decoding diff: %w", err)
	}

	if len(diffResp.BranchChanges) == 0 && len(diffResp.MainChanges) == 0 && len(diffResp.Conflicts) == 0 {
		fmt.Fprintf(a.Out, "No changes on branch '%s'\n", cfg.Branch)
		return nil
	}

	if len(diffResp.BranchChanges) > 0 {
		fmt.Fprintf(a.Out, "Changed files on '%s':\n", cfg.Branch)
		for _, d := range diffResp.BranchChanges {
			if d.VersionID == nil {
				fmt.Fprintf(a.Out, "  deleted: %s\n", d.Path)
			} else if d.Binary {
				fmt.Fprintf(a.Out, "  changed: %s [binary]\n", d.Path)
			} else {
				fmt.Fprintf(a.Out, "  changed: %s\n", d.Path)
			}
		}
	}

	if len(diffResp.MainChanges) > 0 {
		fmt.Fprintf(a.Out, "Changed files on main:\n")
		for _, d := range diffResp.MainChanges {
			if d.VersionID == nil {
				fmt.Fprintf(a.Out, "  deleted: %s\n", d.Path)
			} else if d.Binary {
				fmt.Fprintf(a.Out, "  changed: %s [binary]\n", d.Path)
			} else {
				fmt.Fprintf(a.Out, "  changed: %s\n", d.Path)
			}
		}
	}

	if len(diffResp.Conflicts) > 0 {
		fmt.Fprintf(a.Out, "\nConflicts:\n")
		for _, c := range diffResp.Conflicts {
			fmt.Fprintf(a.Out, "  %s (main: %s, branch: %s)\n", c.Path, c.MainVersionID, c.BranchVersionID)
		}
	}

	return nil
}

// repoBase returns the /repos/:name/- base URL for the configured repo.
// All repo-scoped API endpoints are appended after this base with a "/" separator,
// resulting in URLs like /repos/owner/name/-/commit.
func repoBase(cfg *Config) string {
	repo := cfg.Repo
	if repo == "" {
		repo = "default/default"
	}
	return cfg.Remote + "/repos/" + repo + "/-"
}

// httpGet sends a GET request with the X-DocStore-Identity header set.
func (a *App) httpGet(cfg *Config, urlStr string) (*http.Response, error) {
	req, err := http.NewRequest("GET", urlStr, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-DocStore-Identity", cfg.Author)
	return a.HTTP.Do(req)
}

// doGET sends a GET request without requiring a workspace config.
func (a *App) doGET(urlStr string) (*http.Response, error) {
	req, err := http.NewRequest("GET", urlStr, nil)
	if err != nil {
		return nil, err
	}
	return a.HTTP.Do(req)
}

// doDELETE sends a DELETE request without requiring a workspace config.
func (a *App) doDELETE(urlStr string) (*http.Response, error) {
	req, err := http.NewRequest("DELETE", urlStr, nil)
	if err != nil {
		return nil, err
	}
	return a.HTTP.Do(req)
}

// doPOSTJSON sends a POST request with JSON body without requiring a workspace config.
func (a *App) doPOSTJSON(urlStr string, body interface{}) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", urlStr, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return a.HTTP.Do(req)
}

// doPUTJSON sends a PUT request with JSON body without requiring a workspace config.
func (a *App) doPUTJSON(urlStr string, body interface{}) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("PUT", urlStr, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return a.HTTP.Do(req)
}

// postJSON sends a POST request with a JSON body and the X-DocStore-Identity header set.
func (a *App) postJSON(cfg *Config, urlStr string, body interface{}) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", urlStr, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-DocStore-Identity", cfg.Author)
	return a.HTTP.Do(req)
}

// patchJSON sends a PATCH request with a JSON body and the X-DocStore-Identity header set.
func (a *App) patchJSON(cfg *Config, urlStr string, body interface{}) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("PATCH", urlStr, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-DocStore-Identity", cfg.Author)
	return a.HTTP.Do(req)
}

// readError extracts an error message from an API error response.
func (a *App) readError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	var errResp model.ErrorResponse
	if err := json.Unmarshal(body, &errResp); err == nil && errResp.Error != "" {
		return fmt.Errorf("server error: %s", errResp.Error)
	}
	return fmt.Errorf("server error (status %d)", resp.StatusCode)
}

// treeEntry matches the JSON returned by GET /tree (raw array of objects).
type treeEntry struct {
	Path        string `json:"path"`
	VersionID   string `json:"version_id"`
	ContentHash string `json:"content_hash"`
}

// Log shows commit history for the current branch.
// If path is non-empty, shows file-specific history via /file/:path/history.
// If path is empty, walks backward from the branch head sequence.
// Results are newest-first, capped at limit (default 20).
func (a *App) Log(path string, limit int) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if limit <= 0 {
		limit = 20
	}
	if path != "" {
		return a.logFile(cfg, path, limit)
	}
	return a.logBranch(cfg, limit)
}

func (a *App) logBranch(cfg *Config, limit int) error {
	resp, err := a.httpGet(cfg, repoBase(cfg)+"/branches")
	if err != nil {
		return fmt.Errorf("fetching branches: %w", err)
	}
	defer resp.Body.Close()

	var branches []model.Branch
	if err := json.NewDecoder(resp.Body).Decode(&branches); err != nil {
		return fmt.Errorf("decoding branches: %w", err)
	}

	var headSeq, baseSeq int64
	found := false
	for _, b := range branches {
		if b.Name == cfg.Branch {
			headSeq = b.HeadSequence
			baseSeq = b.BaseSequence
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("branch %q not found", cfg.Branch)
	}
	if headSeq == 0 {
		fmt.Fprintf(a.Out, "No commits on branch '%s'\n", cfg.Branch)
		return nil
	}

	count := 0
	for seq := headSeq; seq > baseSeq && count < limit; seq-- {
		commitResp, err := a.httpGet(cfg, fmt.Sprintf("%s/commit/%d", repoBase(cfg), seq))
		if err != nil {
			return fmt.Errorf("fetching commit %d: %w", seq, err)
		}
		if commitResp.StatusCode == http.StatusNotFound {
			commitResp.Body.Close()
			continue
		}
		if commitResp.StatusCode != http.StatusOK {
			commitResp.Body.Close()
			continue
		}
		var ci commitInfo
		if err := json.NewDecoder(commitResp.Body).Decode(&ci); err != nil {
			commitResp.Body.Close()
			continue
		}
		commitResp.Body.Close()

		if ci.Branch != cfg.Branch {
			continue
		}
		fmt.Fprintf(a.Out, "seq %-4d  %-12s  %s  %s\n",
			ci.Sequence, ci.Author, ci.CreatedAt.Format("2006-01-02"), ci.Message)
		count++
	}

	if count == 0 {
		fmt.Fprintf(a.Out, "No commits on branch '%s'\n", cfg.Branch)
	}
	return nil
}

func (a *App) logFile(cfg *Config, path string, limit int) error {
	q := url.Values{}
	q.Set("branch", cfg.Branch)
	q.Set("limit", fmt.Sprintf("%d", limit))

	resp, err := a.httpGet(cfg, repoBase(cfg)+"/file/"+path+"/history?"+q.Encode())
	if err != nil {
		return fmt.Errorf("fetching history: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}

	var entries []fileHistEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return fmt.Errorf("decoding history: %w", err)
	}

	if len(entries) == 0 {
		fmt.Fprintf(a.Out, "No history for '%s' on branch '%s'\n", path, cfg.Branch)
		return nil
	}
	for _, e := range entries {
		fmt.Fprintf(a.Out, "seq %-4d  %-12s  %s  %s\n",
			e.Sequence, e.Author, e.CreatedAt.Format("2006-01-02"), e.Message)
	}
	return nil
}

// Show inspects a specific commit or a file's content at a given sequence.
// If path is empty, lists all files changed in that commit.
// If path is non-empty, prints the file's content at that sequence.
func (a *App) Show(sequence int64, path string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if path != "" {
		return a.showFile(cfg, sequence, path)
	}
	return a.showCommit(cfg, sequence)
}

func (a *App) showCommit(cfg *Config, sequence int64) error {
	resp, err := a.httpGet(cfg, fmt.Sprintf("%s/commit/%d", repoBase(cfg), sequence))
	if err != nil {
		return fmt.Errorf("fetching commit: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("commit %d not found", sequence)
	}
	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}

	var ci commitInfo
	if err := json.NewDecoder(resp.Body).Decode(&ci); err != nil {
		return fmt.Errorf("decoding commit: %w", err)
	}

	fmt.Fprintf(a.Out, "commit %d\n", ci.Sequence)
	fmt.Fprintf(a.Out, "branch:  %s\n", ci.Branch)
	fmt.Fprintf(a.Out, "author:  %s\n", ci.Author)
	fmt.Fprintf(a.Out, "date:    %s\n", ci.CreatedAt.Format("2006-01-02"))
	fmt.Fprintf(a.Out, "message: %s\n", ci.Message)
	if len(ci.Files) > 0 {
		fmt.Fprintf(a.Out, "\nFiles:\n")
		for _, f := range ci.Files {
			if f.VersionID == nil {
				fmt.Fprintf(a.Out, "  %s  (deleted)\n", f.Path)
			} else {
				fmt.Fprintf(a.Out, "  %s  (changed)\n", f.Path)
			}
		}
	}
	return nil
}

func (a *App) showFile(cfg *Config, sequence int64, path string) error {
	q := url.Values{}
	q.Set("branch", cfg.Branch)
	q.Set("at", fmt.Sprintf("%d", sequence))

	resp, err := a.httpGet(cfg, repoBase(cfg)+"/file/"+path+"?"+q.Encode())
	if err != nil {
		return fmt.Errorf("fetching file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("file %q not found at sequence %d", path, sequence)
	}
	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}

	var fileResp model.FileResponse
	if err := json.NewDecoder(resp.Body).Decode(&fileResp); err != nil {
		return fmt.Errorf("decoding file: %w", err)
	}
	fmt.Fprintf(a.Out, "%s", fileResp.Content)
	if len(fileResp.Content) > 0 && fileResp.Content[len(fileResp.Content)-1] != '\n' {
		fmt.Fprintln(a.Out)
	}
	return nil
}

// Rebase rebases the current branch onto main's latest head.
// On success it updates state.json with the new head sequence.
// On conflict it writes <path>.main and <path>.branch for each conflicting file
// and returns a non-nil error.
func (a *App) Rebase() error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if cfg.Branch == "main" {
		return fmt.Errorf("cannot rebase main")
	}

	req := model.RebaseRequest{Branch: cfg.Branch, Author: cfg.Author}
	resp, err := a.postJSON(cfg, repoBase(cfg)+"/rebase", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusConflict {
		var conflictErr model.RebaseConflictError
		if err := json.Unmarshal(body, &conflictErr); err == nil && len(conflictErr.Conflicts) > 0 {
			return a.writeRebaseConflictFiles(cfg, conflictErr.Conflicts)
		}
		return fmt.Errorf("server error (status %d)", resp.StatusCode)
	}

	if resp.StatusCode != http.StatusOK {
		var errResp model.ErrorResponse
		if err := json.Unmarshal(body, &errResp); err == nil && errResp.Error != "" {
			return fmt.Errorf("server error: %s", errResp.Error)
		}
		return fmt.Errorf("server error (status %d)", resp.StatusCode)
	}

	var rebaseResp model.RebaseResponse
	if err := json.Unmarshal(body, &rebaseResp); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}

	st, err := a.loadState()
	if err != nil {
		return err
	}
	st.Sequence = rebaseResp.NewHeadSequence
	if err := a.saveState(st); err != nil {
		return err
	}

	fmt.Fprintf(a.Out, "Rebased '%s' onto main (base: %d, head: %d, %d commits replayed)\n",
		cfg.Branch, rebaseResp.NewBaseSequence, rebaseResp.NewHeadSequence, rebaseResp.CommitsReplayed)
	return nil
}

// writeRebaseConflictFiles fetches both the main and branch versions of each
// conflicting file and writes them to <path>.main and <path>.branch on disk.
func (a *App) writeRebaseConflictFiles(cfg *Config, conflicts []model.ConflictEntry) error {
	for _, c := range conflicts {
		mainContent, err := a.fetchFileBytesForBranch(cfg, c.Path, "main")
		if err != nil {
			return fmt.Errorf("fetching main version of %s: %w", c.Path, err)
		}
		branchContent, err := a.fetchFileBytesForBranch(cfg, c.Path, cfg.Branch)
		if err != nil {
			return fmt.Errorf("fetching branch version of %s: %w", c.Path, err)
		}

		mainDst := filepath.Join(a.Dir, filepath.FromSlash(c.Path+".main"))
		branchDst := filepath.Join(a.Dir, filepath.FromSlash(c.Path+".branch"))
		if err := os.MkdirAll(filepath.Dir(mainDst), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(mainDst, mainContent, 0644); err != nil {
			return fmt.Errorf("writing %s: %w", mainDst, err)
		}
		if err := os.WriteFile(branchDst, branchContent, 0644); err != nil {
			return fmt.Errorf("writing %s: %w", branchDst, err)
		}
		fmt.Fprintf(a.Out, "conflict: %s (wrote %s.main, %s.branch)\n", c.Path, c.Path, c.Path)
	}
	return fmt.Errorf("rebase aborted due to conflicts")
}

// fetchFileBytesForBranch fetches a file's raw content bytes from the server.
func (a *App) fetchFileBytesForBranch(cfg *Config, path, branch string) ([]byte, error) {
	q := url.Values{}
	q.Set("branch", branch)
	resp, err := a.httpGet(cfg, repoBase(cfg)+"/file/"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return []byte{}, nil // file deleted on that side
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server error (status %d)", resp.StatusCode)
	}

	var fileResp model.FileResponse
	if err := json.NewDecoder(resp.Body).Decode(&fileResp); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return fileResp.Content, nil
}

// Branches lists branches for the current repo, filtered by status (default: active).
func (a *App) Branches(status string, onlyDraft bool, includeDraft bool) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if status == "" {
		status = "active"
	}

	q := url.Values{}
	q.Set("status", status)
	if onlyDraft {
		q.Set("draft", "true")
	}
	if includeDraft {
		q.Set("include_draft", "true")
	}
	resp, err := a.httpGet(cfg, repoBase(cfg)+"/branches?"+q.Encode())
	if err != nil {
		return fmt.Errorf("fetching branches: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}

	var branches []model.Branch
	if err := json.NewDecoder(resp.Body).Decode(&branches); err != nil {
		return fmt.Errorf("decoding branches: %w", err)
	}

	fmt.Fprintf(a.Out, "%-30s %-6s %-6s %-10s %-5s\n", "BRANCH", "HEAD", "BASE", "STATUS", "DRAFT")
	for _, b := range branches {
		draft := ""
		if b.Draft {
			draft = "yes"
		}
		fmt.Fprintf(a.Out, "%-30s %-6d %-6d %-10s %-5s\n",
			b.Name, b.HeadSequence, b.BaseSequence, string(b.Status), draft)
	}
	return nil
}

// Reviews lists reviews for a branch (defaults to current branch if empty).
// A review is marked [stale] if its sequence < the branch's head_sequence.
func (a *App) Reviews(branch string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if branch == "" {
		branch = cfg.Branch
	}

	headSeq, err := a.branchHeadSequence(cfg, branch)
	if err != nil {
		return err
	}

	resp, err := a.httpGet(cfg, repoBase(cfg)+"/branch/"+url.PathEscape(branch)+"/reviews")
	if err != nil {
		return fmt.Errorf("fetching reviews: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}

	var reviews []model.Review
	if err := json.NewDecoder(resp.Body).Decode(&reviews); err != nil {
		return fmt.Errorf("decoding reviews: %w", err)
	}

	if len(reviews) == 0 {
		fmt.Fprintf(a.Out, "No reviews for branch '%s'\n", branch)
		return nil
	}

	fmt.Fprintf(a.Out, "%-36s  %-30s  %-4s  %-10s  %s\n", "ID", "REVIEWER", "SEQ", "STATUS", "BODY")
	for _, r := range reviews {
		stale := ""
		if r.Sequence < headSeq {
			stale = "  [stale]"
		}
		fmt.Fprintf(a.Out, "%-36s  %-30s  %-4d  %-10s  %s%s\n",
			r.ID, r.Reviewer, r.Sequence, string(r.Status), r.Body, stale)
	}
	return nil
}

// Review submits a review for a branch.
func (a *App) Review(branch, status, body string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if branch == "" {
		branch = cfg.Branch
	}

	reviewStatus := model.ReviewStatus(status)
	if reviewStatus != model.ReviewApproved && reviewStatus != model.ReviewRejected {
		return fmt.Errorf("status must be 'approved' or 'rejected'")
	}

	req := model.CreateReviewRequest{
		Branch: branch,
		Status: reviewStatus,
		Body:   body,
	}
	resp, err := a.postJSON(cfg, repoBase(cfg)+"/review", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}

	var reviewResp model.CreateReviewResponse
	if err := json.NewDecoder(resp.Body).Decode(&reviewResp); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}

	fmt.Fprintf(a.Out, "Review submitted: %s (id: %s, sequence: %d)\n", status, reviewResp.ID, reviewResp.Sequence)
	return nil
}

// Comment creates an inline file annotation on a branch.
// The version_id is resolved automatically by fetching the file metadata from the server.
func (a *App) Comment(branch, path, body string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if branch == "" {
		branch = cfg.Branch
	}

	// Resolve version_id for the file on this branch.
	q := url.Values{}
	q.Set("branch", branch)
	fileResp, err := a.httpGet(cfg, repoBase(cfg)+"/file/"+url.PathEscape(path)+"?"+q.Encode())
	if err != nil {
		return fmt.Errorf("fetching file info: %w", err)
	}
	defer fileResp.Body.Close()
	if fileResp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("file %q not found on branch %q", path, branch)
	}
	if fileResp.StatusCode != http.StatusOK {
		return a.readError(fileResp)
	}
	var fileMeta struct {
		VersionID string `json:"version_id"`
	}
	if err := json.NewDecoder(fileResp.Body).Decode(&fileMeta); err != nil {
		return fmt.Errorf("decoding file info: %w", err)
	}
	if fileMeta.VersionID == "" {
		return fmt.Errorf("file %q was deleted on branch %q", path, branch)
	}

	req := model.CreateReviewCommentRequest{
		Branch:    branch,
		Path:      path,
		VersionID: fileMeta.VersionID,
		Body:      body,
	}
	resp, err := a.postJSON(cfg, repoBase(cfg)+"/comment", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return a.readError(resp)
	}

	var createResp model.CreateReviewCommentResponse
	if err := json.NewDecoder(resp.Body).Decode(&createResp); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}

	fmt.Fprintf(a.Out, "Comment created: id=%s, sequence=%d\n", createResp.ID, createResp.Sequence)
	return nil
}

// Comments lists inline file comments for a branch. If path is non-empty, only
// comments on that path are shown.
func (a *App) Comments(branch, path string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if branch == "" {
		branch = cfg.Branch
	}

	q := url.Values{}
	if path != "" {
		q.Set("path", path)
	}
	urlStr := repoBase(cfg) + "/branch/" + url.PathEscape(branch) + "/comments"
	if len(q) > 0 {
		urlStr += "?" + q.Encode()
	}

	resp, err := a.httpGet(cfg, urlStr)
	if err != nil {
		return fmt.Errorf("fetching comments: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}

	var comments []model.ReviewComment
	if err := json.NewDecoder(resp.Body).Decode(&comments); err != nil {
		return fmt.Errorf("decoding comments: %w", err)
	}

	if len(comments) == 0 {
		fmt.Fprintf(a.Out, "No comments for branch '%s'\n", branch)
		return nil
	}

	fmt.Fprintf(a.Out, "%-36s  %-40s  %-4s  %s\n", "ID", "PATH", "SEQ", "BODY")
	for _, c := range comments {
		fmt.Fprintf(a.Out, "%-36s  %-40s  %-4d  %s\n", c.ID, c.Path, c.Sequence, c.Body)
	}
	return nil
}

// Checks lists check runs for a branch (defaults to current branch if empty).
// If showAll is false, only the latest result per check_name is shown.
func (a *App) Checks(branch string, showAll bool) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if branch == "" {
		branch = cfg.Branch
	}

	resp, err := a.httpGet(cfg, repoBase(cfg)+"/branch/"+url.PathEscape(branch)+"/checks")
	if err != nil {
		return fmt.Errorf("fetching checks: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}

	var checkRuns []model.CheckRun
	if err := json.NewDecoder(resp.Body).Decode(&checkRuns); err != nil {
		return fmt.Errorf("decoding checks: %w", err)
	}

	if len(checkRuns) == 0 {
		fmt.Fprintf(a.Out, "No check runs for branch '%s'\n", branch)
		return nil
	}

	headSeq, err := a.branchHeadSequence(cfg, branch)
	if err != nil {
		return err
	}

	// Deduplicate by check_name keeping highest sequence unless --all is set.
	if !showAll {
		latest := make(map[string]model.CheckRun)
		for _, c := range checkRuns {
			prev, ok := latest[c.CheckName]
			if !ok || c.Sequence > prev.Sequence {
				latest[c.CheckName] = c
			}
		}
		checkRuns = make([]model.CheckRun, 0, len(latest))
		for _, c := range latest {
			checkRuns = append(checkRuns, c)
		}
		sort.Slice(checkRuns, func(i, j int) bool {
			return checkRuns[i].CheckName < checkRuns[j].CheckName
		})
	}

	fmt.Fprintf(a.Out, "%-8s  %-20s  %-4s  %-10s  %s\n", "ID", "CHECK NAME", "SEQ", "STATUS", "REPORTER")
	for _, c := range checkRuns {
		stale := ""
		if c.Sequence < headSeq {
			stale = "  [stale]"
		}
		id := c.ID
		if len(id) > 8 {
			id = id[:8]
		}
		fmt.Fprintf(a.Out, "%-8s  %-20s  %-4d  %-10s  %s%s\n",
			id, c.CheckName, c.Sequence, string(c.Status), c.Reporter, stale)
		if c.LogURL != nil {
			fmt.Fprintf(a.Out, "         log: %s\n", *c.LogURL)
		}
	}
	return nil
}

// Check reports a CI check result for a branch.
// logURL and sequence are optional; pass nil to omit them from the request.
func (a *App) Check(branch, name, status string, logURL *string, sequence *int64) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if branch == "" {
		branch = cfg.Branch
	}

	checkStatus := model.CheckRunStatus(status)
	if checkStatus != model.CheckRunPassed && checkStatus != model.CheckRunFailed && checkStatus != model.CheckRunPending {
		return fmt.Errorf("status must be 'passed', 'failed', or 'pending'")
	}

	req := model.CreateCheckRunRequest{
		Branch:    branch,
		CheckName: name,
		Status:    checkStatus,
		LogURL:    logURL,
		Sequence:  sequence,
	}
	resp, err := a.postJSON(cfg, repoBase(cfg)+"/check", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}

	var checkResp model.CreateCheckRunResponse
	if err := json.NewDecoder(resp.Body).Decode(&checkResp); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}

	fmt.Fprintf(a.Out, "Check run submitted: %s=%s (id: %s, sequence: %d)\n", name, status, checkResp.ID, checkResp.Sequence)
	return nil
}

// branchHeadSequence returns the head sequence of a named branch.
func (a *App) branchHeadSequence(cfg *Config, branch string) (int64, error) {
	resp, err := a.httpGet(cfg, repoBase(cfg)+"/branches")
	if err != nil {
		return 0, fmt.Errorf("fetching branches: %w", err)
	}
	defer resp.Body.Close()

	var branches []model.Branch
	if err := json.NewDecoder(resp.Body).Decode(&branches); err != nil {
		return 0, fmt.Errorf("decoding branches: %w", err)
	}

	for _, b := range branches {
		if b.Name == branch {
			return b.HeadSequence, nil
		}
	}
	return 0, fmt.Errorf("branch %q not found", branch)
}

// RetryChecks requests a retry of CI checks for a branch.
// If checks is empty, all failed checks at the branch's current head sequence are retried.
func (a *App) RetryChecks(branch string, checks []string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if branch == "" {
		branch = cfg.Branch
	}

	seq, err := a.branchHeadSequence(cfg, branch)
	if err != nil {
		return err
	}

	req := model.RetryChecksRequest{
		Branch:   branch,
		Sequence: seq,
		Checks:   checks,
	}
	resp, err := a.postJSON(cfg, repoBase(cfg)+"/checks/retry", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		return a.readError(resp)
	}

	var retryResp model.RetryChecksResponse
	if err := json.NewDecoder(resp.Body).Decode(&retryResp); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}

	if len(checks) == 0 {
		fmt.Fprintf(a.Out, "Retrying all failed checks on branch '%s' at sequence %d (attempt %d)\n", branch, seq, retryResp.Attempt)
	} else {
		fmt.Fprintf(a.Out, "Retrying checks %v on branch '%s' at sequence %d (attempt %d)\n", checks, branch, seq, retryResp.Attempt)
	}
	return nil
}

// TUI launches the terminal UI reading config from .docstore/config.json.
func (a *App) TUI() error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	return tui.Run(a.HTTP, cfg.Remote, cfg.Repo, cfg.Author)
}

// Resolve resolves a merge/rebase conflict for path.
// It expects <path>.main and <path>.branch to exist on disk (written by Rebase),
// reads the resolved content from <path> itself, commits it to the current branch,
// and removes the conflict files.
func (a *App) Resolve(path string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}

	mainConflict := filepath.Join(a.Dir, filepath.FromSlash(path+".main"))
	branchConflict := filepath.Join(a.Dir, filepath.FromSlash(path+".branch"))

	if _, err := os.Stat(mainConflict); os.IsNotExist(err) {
		return fmt.Errorf("no conflict file found: %s.main (run 'ds rebase' first)", path)
	}
	if _, err := os.Stat(branchConflict); os.IsNotExist(err) {
		return fmt.Errorf("no conflict file found: %s.branch (run 'ds rebase' first)", path)
	}

	resolvedPath := filepath.Join(a.Dir, filepath.FromSlash(path))
	content, err := os.ReadFile(resolvedPath)
	if err != nil {
		return fmt.Errorf("reading resolved file %s: %w", path, err)
	}

	req := model.CommitRequest{
		Branch:  cfg.Branch,
		Files:   []model.FileChange{{Path: path, Content: content}},
		Message: fmt.Sprintf("resolve conflict in %s", path),
		Author:  cfg.Author,
	}

	resp, err := a.postJSON(cfg, repoBase(cfg)+"/commit", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return a.readError(resp)
	}

	var commitResp model.CommitResponse
	if err := json.NewDecoder(resp.Body).Decode(&commitResp); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}

	st, err := a.loadState()
	if err != nil {
		return err
	}
	st.Sequence = commitResp.Sequence
	st.Files[path] = HashBytes(content)
	if err := a.saveState(st); err != nil {
		return err
	}

	os.Remove(mainConflict)
	os.Remove(branchConflict)

	fmt.Fprintf(a.Out, "resolved %s, committed as sequence %d\n", path, commitResp.Sequence)
	return nil
}

// ImportGit imports a local git repository's default branch into docstore main.
// mode must be "replay" (default) or "squash".
// Shells out to git via os/exec — no new library dependency.
func (a *App) ImportGit(repoPath, mode string) error {
	if mode == "" {
		mode = "replay"
	}
	if mode != "replay" && mode != "squash" {
		return fmt.Errorf("mode must be 'replay' or 'squash'")
	}

	// Verify the path is a git repository.
	if _, err := os.Stat(filepath.Join(repoPath, ".git")); os.IsNotExist(err) {
		return fmt.Errorf("%s is not a git repository (no .git directory)", repoPath)
	}

	// Verify git is available in PATH.
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git not found in PATH: %w", err)
	}

	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}

	if mode == "squash" {
		return a.importGitSquash(cfg, repoPath)
	}
	return a.importGitReplay(cfg, repoPath)
}

// importGitReplay imports each non-merge git commit as one docstore commit.
func (a *App) importGitReplay(cfg *Config, repoPath string) error {
	// %s captures only the subject line; multi-line commit bodies are not imported.
	out, err := exec.Command("git", "-C", repoPath, "log", "--reverse", "--no-merges", "--format=%H|%ae|%s", "HEAD").Output()
	if err != nil {
		return fmt.Errorf("git log failed: %w", err)
	}

	raw := strings.TrimRight(string(out), "\n")
	if raw == "" {
		fmt.Fprintf(a.Out, "No commits to import.\n")
		return nil
	}
	lines := strings.Split(raw, "\n")
	total := len(lines)
	fmt.Fprintf(a.Out, "Importing %d commits from %s...\n", total, repoPath)

	for i, line := range lines {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 {
			return fmt.Errorf("unexpected git log output: %q", line)
		}
		sha, email, subject := parts[0], parts[1], parts[2]
		shortSHA := sha
		if len(shortSHA) > 7 {
			shortSHA = shortSHA[:7]
		}
		fmt.Fprintf(a.Out, "  %d/%d %s %q (%s)\n", i+1, total, shortSHA, subject, email)

		// List files changed in this commit. --root handles the initial commit (no parent).
		filesOut, err := exec.Command("git", "-C", repoPath, "diff-tree", "--no-commit-id", "--root", "-r", "--name-status", sha).Output()
		if err != nil {
			return fmt.Errorf("git diff-tree failed for %s: %w", shortSHA, err)
		}

		var changes []model.FileChange
		for _, fline := range strings.Split(strings.TrimRight(string(filesOut), "\n"), "\n") {
			if fline == "" {
				continue
			}
			fp := strings.SplitN(fline, "\t", 2)
			if len(fp) != 2 {
				continue
			}
			status, path := fp[0], filepath.ToSlash(fp[1])
			switch status {
			case "A", "M":
				content, err := exec.Command("git", "-C", repoPath, "show", sha+":"+path).Output()
				if err != nil {
					return fmt.Errorf("git show failed for %s:%s: %w", shortSHA, path, err)
				}
				ct := detectContentType(path, content)
				changes = append(changes, model.FileChange{Path: path, Content: content, ContentType: ct})
			case "D":
				changes = append(changes, model.FileChange{Path: path}) // nil Content = delete
			}
		}

		if len(changes) == 0 {
			continue
		}

		msg := fmt.Sprintf("[git-author: %s] %s", email, subject)
		req := model.CommitRequest{
			Branch:  "main",
			Files:   changes,
			Message: msg,
			Author:  cfg.Author,
		}

		resp, err := a.postJSON(cfg, repoBase(cfg)+"/commit", req)
		if err != nil {
			return fmt.Errorf("commit failed for %s: %w", shortSHA, err)
		}
		if resp.StatusCode != http.StatusCreated {
			apiErr := a.readError(resp)
			resp.Body.Close()
			return fmt.Errorf("commit failed for %s: %w", shortSHA, apiErr)
		}
		resp.Body.Close()
	}

	fmt.Fprintf(a.Out, "Done. %d commits imported.\n", total)
	return nil
}

// importGitSquash imports the entire repo at HEAD as a single docstore commit.
func (a *App) importGitSquash(cfg *Config, repoPath string) error {
	// Detect default branch name.
	branchOut, err := exec.Command("git", "-C", repoPath, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return fmt.Errorf("git rev-parse failed: %w", err)
	}
	branch := strings.TrimSpace(string(branchOut))

	// List all files at HEAD.
	filesOut, err := exec.Command("git", "-C", repoPath, "ls-tree", "-r", "HEAD", "--name-only").Output()
	if err != nil {
		return fmt.Errorf("git ls-tree failed: %w", err)
	}

	var filePaths []string
	for _, f := range strings.Split(strings.TrimRight(string(filesOut), "\n"), "\n") {
		if f != "" {
			filePaths = append(filePaths, f)
		}
	}

	fmt.Fprintf(a.Out, "Collecting files from HEAD... %d files\n", len(filePaths))

	// Get short SHA.
	shaOut, err := exec.Command("git", "-C", repoPath, "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return fmt.Errorf("git rev-parse --short failed: %w", err)
	}
	shortSHA := strings.TrimSpace(string(shaOut))

	var changes []model.FileChange
	for _, path := range filePaths {
		content, err := exec.Command("git", "-C", repoPath, "show", "HEAD:"+path).Output()
		if err != nil {
			return fmt.Errorf("git show failed for %s: %w", path, err)
		}
		slashPath := filepath.ToSlash(path)
		ct := detectContentType(slashPath, content)
		changes = append(changes, model.FileChange{Path: slashPath, Content: content, ContentType: ct})
	}

	fmt.Fprintf(a.Out, "Importing as single commit...\n")

	msg := fmt.Sprintf("[git-import] Squashed import of %s (%d files, HEAD %s)", branch, len(filePaths), shortSHA)
	req := model.CommitRequest{
		Branch:  "main",
		Files:   changes,
		Message: msg,
		Author:  cfg.Author,
	}

	resp, err := a.postJSON(cfg, repoBase(cfg)+"/commit", req)
	if err != nil {
		return fmt.Errorf("commit failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return a.readError(resp)
	}

	fmt.Fprintf(a.Out, "Done. 1 commit imported (%d files).\n", len(filePaths))
	return nil
}

// ---------------------------------------------------------------------------
// Org management
// ---------------------------------------------------------------------------

// Orgs lists all organizations.
func (a *App) Orgs() error {
	remote, err := a.loadRemote()
	if err != nil {
		return err
	}
	resp, err := a.doGET(remote + "/orgs")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	var r model.ListOrgsResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	fmt.Fprintf(a.Out, "%-30s  %-20s  %s\n", "NAME", "CREATED BY", "CREATED AT")
	for _, org := range r.Orgs {
		fmt.Fprintf(a.Out, "%-30s  %-20s  %s\n", org.Name, org.CreatedBy, org.CreatedAt.Format("2006-01-02"))
	}
	return nil
}

// OrgsCreate creates a new organization.
func (a *App) OrgsCreate(name string) error {
	remote, err := a.loadRemote()
	if err != nil {
		return err
	}
	resp, err := a.doPOSTJSON(remote+"/orgs", model.CreateOrgRequest{Name: name})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return a.readError(resp)
	}
	var org model.Org
	if err := json.NewDecoder(resp.Body).Decode(&org); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	fmt.Fprintf(a.Out, "Created org '%s'\n", org.Name)
	return nil
}

// OrgsGet fetches and prints details for a single organization.
func (a *App) OrgsGet(name string) error {
	remote, err := a.loadRemote()
	if err != nil {
		return err
	}
	resp, err := a.doGET(remote + "/orgs/" + name)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("org '%s' not found", name)
	}
	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	var org model.Org
	if err := json.NewDecoder(resp.Body).Decode(&org); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	fmt.Fprintf(a.Out, "Name:       %s\n", org.Name)
	fmt.Fprintf(a.Out, "Created by: %s\n", org.CreatedBy)
	fmt.Fprintf(a.Out, "Created at: %s\n", org.CreatedAt.Format("2006-01-02"))
	return nil
}

// OrgsDelete deletes an organization (fails if it still has repos).
func (a *App) OrgsDelete(name string) error {
	remote, err := a.loadRemote()
	if err != nil {
		return err
	}
	resp, err := a.doDELETE(remote + "/orgs/" + name)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		return fmt.Errorf("org '%s' still has repos", name)
	}
	if resp.StatusCode != http.StatusNoContent {
		return a.readError(resp)
	}
	fmt.Fprintf(a.Out, "Deleted org '%s'\n", name)
	return nil
}

// OrgsRepos lists repositories within an organization.
func (a *App) OrgsRepos(orgName string) error {
	remote, err := a.loadRemote()
	if err != nil {
		return err
	}
	resp, err := a.doGET(remote + "/orgs/" + orgName + "/repos")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	var r model.ReposResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	fmt.Fprintf(a.Out, "%-30s  %-20s  %-20s  %s\n", "NAME", "OWNER", "CREATED BY", "CREATED AT")
	for _, repo := range r.Repos {
		fmt.Fprintf(a.Out, "%-30s  %-20s  %-20s  %s\n", repo.Name, repo.Owner, repo.CreatedBy, repo.CreatedAt.Format("2006-01-02"))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Repo management
// ---------------------------------------------------------------------------

// Repos lists all repositories.
func (a *App) Repos() error {
	remote, err := a.loadRemote()
	if err != nil {
		return err
	}
	resp, err := a.doGET(remote + "/repos")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	var r model.ReposResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	fmt.Fprintf(a.Out, "%-30s  %-20s  %-20s  %s\n", "NAME", "OWNER", "CREATED BY", "CREATED AT")
	for _, repo := range r.Repos {
		fmt.Fprintf(a.Out, "%-30s  %-20s  %-20s  %s\n", repo.Name, repo.Owner, repo.CreatedBy, repo.CreatedAt.Format("2006-01-02"))
	}
	return nil
}

// ReposCreate creates a new repository under the given owner organization.
func (a *App) ReposCreate(owner, name string) error {
	remote, err := a.loadRemote()
	if err != nil {
		return err
	}
	resp, err := a.doPOSTJSON(remote+"/repos", model.CreateRepoRequest{Owner: owner, Name: name})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("org '%s' not found", owner)
	}
	if resp.StatusCode != http.StatusCreated {
		return a.readError(resp)
	}
	var repo model.Repo
	if err := json.NewDecoder(resp.Body).Decode(&repo); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	fmt.Fprintf(a.Out, "Created repo '%s'\n", repo.Name)
	return nil
}

// ReposDelete deletes a repository by full name (e.g., "acme/myrepo").
func (a *App) ReposDelete(name string) error {
	remote, err := a.loadRemote()
	if err != nil {
		return err
	}
	resp, err := a.doDELETE(remote + "/repos/" + name + "/-/")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return a.readError(resp)
	}
	fmt.Fprintf(a.Out, "Deleted repo '%s'\n", name)
	return nil
}

// RepoGet gets a single repository by full name (e.g., "acme/myrepo").
func (a *App) RepoGet(fullName string) error {
	remote, err := a.loadRemote()
	if err != nil {
		return err
	}
	resp, err := a.doGET(remote + "/repos/" + fullName)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("repo '%s' not found", fullName)
	}
	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	var repo model.Repo
	if err := json.NewDecoder(resp.Body).Decode(&repo); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	fmt.Fprintf(a.Out, "%-30s  %-20s  %-20s  %s\n", "NAME", "OWNER", "CREATED BY", "CREATED AT")
	fmt.Fprintf(a.Out, "%-30s  %-20s  %-20s  %s\n", repo.Name, repo.Owner, repo.CreatedBy, repo.CreatedAt.Format("2006-01-02"))
	return nil
}

// ---------------------------------------------------------------------------
// Branch management (CLI-level)
// ---------------------------------------------------------------------------

// BranchDelete deletes a branch from the current repo by name.
func (a *App) BranchDelete(branch string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	resp, err := a.doDELETE(repoBase(cfg) + "/branch/" + url.PathEscape(branch))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("branch '%s' not found", branch)
	}
	if resp.StatusCode == http.StatusConflict {
		return fmt.Errorf("branch '%s' is already merged or abandoned", branch)
	}
	if resp.StatusCode != http.StatusNoContent {
		return a.readError(resp)
	}
	fmt.Fprintf(a.Out, "Deleted branch '%s'\n", branch)
	return nil
}

// ---------------------------------------------------------------------------
// Purge
// ---------------------------------------------------------------------------

// Purge removes merged/abandoned branches and their unreachable data from the
// current repo. olderThan is a duration string like "30d". If dryRun is true
// the server reports what would be deleted without deleting anything.
func (a *App) Purge(olderThan string, dryRun bool) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	req := model.PurgeRequest{OlderThan: olderThan, DryRun: dryRun}
	resp, err := a.postJSON(cfg, repoBase(cfg)+"/purge", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("repo '%s' not found", cfg.Repo)
	}
	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	var result model.PurgeResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	if dryRun {
		fmt.Fprintf(a.Out, "[dry-run] would purge:\n")
	}
	fmt.Fprintf(a.Out, "  branches purged:      %d\n", result.BranchesPurged)
	fmt.Fprintf(a.Out, "  file commits deleted: %d\n", result.FileCommitsDeleted)
	fmt.Fprintf(a.Out, "  commits deleted:      %d\n", result.CommitsDeleted)
	fmt.Fprintf(a.Out, "  documents deleted:    %d\n", result.DocumentsDeleted)
	fmt.Fprintf(a.Out, "  reviews deleted:      %d\n", result.ReviewsDeleted)
	fmt.Fprintf(a.Out, "  check runs deleted:   %d\n", result.CheckRunsDeleted)
	return nil
}

// ---------------------------------------------------------------------------
// Org membership management
// ---------------------------------------------------------------------------

// OrgMembersAdd adds or updates a member in an org with the given role.
func (a *App) OrgMembersAdd(org, identity, role string) error {
	switch model.OrgRole(role) {
	case model.OrgRoleOwner, model.OrgRoleMember:
	default:
		return fmt.Errorf("role must be 'owner' or 'member'")
	}
	remote, err := a.loadRemote()
	if err != nil {
		return err
	}
	resp, err := a.doPOSTJSON(remote+"/orgs/"+org+"/members/"+identity, model.AddOrgMemberRequest{Role: model.OrgRole(role)})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return a.readError(resp)
	}
	fmt.Fprintf(a.Out, "Added '%s' to org '%s' as '%s'\n", identity, org, role)
	return nil
}

// OrgMembersRemove removes a member from an org.
func (a *App) OrgMembersRemove(org, identity string) error {
	remote, err := a.loadRemote()
	if err != nil {
		return err
	}
	resp, err := a.doDELETE(remote + "/orgs/" + org + "/members/" + identity)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return a.readError(resp)
	}
	fmt.Fprintf(a.Out, "Removed '%s' from org '%s'\n", identity, org)
	return nil
}

// OrgMembersList lists all members of an org.
func (a *App) OrgMembersList(org string) error {
	remote, err := a.loadRemote()
	if err != nil {
		return err
	}
	resp, err := a.doGET(remote + "/orgs/" + org + "/members")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	var r model.OrgMembersResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	fmt.Fprintf(a.Out, "%-30s  %s\n", "IDENTITY", "ROLE")
	for _, m := range r.Members {
		fmt.Fprintf(a.Out, "%-30s  %s\n", m.Identity, string(m.Role))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Org invite management
// ---------------------------------------------------------------------------

// OrgInvitesCreate creates an invite for email with the given role and prints the token.
func (a *App) OrgInvitesCreate(org, email, role string) error {
	switch model.OrgRole(role) {
	case model.OrgRoleOwner, model.OrgRoleMember:
	default:
		return fmt.Errorf("role must be 'owner' or 'member'")
	}
	remote, err := a.loadRemote()
	if err != nil {
		return err
	}
	resp, err := a.doPOSTJSON(remote+"/orgs/"+org+"/invites", model.CreateInviteRequest{Email: email, Role: model.OrgRole(role)})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	var r model.CreateInviteResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	fmt.Fprintf(a.Out, "Invite created: id=%s token=%s\n", r.ID, r.Token)
	return nil
}

// OrgInvitesList lists all pending invites for an org.
func (a *App) OrgInvitesList(org string) error {
	remote, err := a.loadRemote()
	if err != nil {
		return err
	}
	resp, err := a.doGET(remote + "/orgs/" + org + "/invites")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	var r model.OrgInvitesResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	fmt.Fprintf(a.Out, "%-36s  %-30s  %s\n", "ID", "EMAIL", "ROLE")
	for _, inv := range r.Invites {
		fmt.Fprintf(a.Out, "%-36s  %-30s  %s\n", inv.ID, inv.Email, string(inv.Role))
	}
	return nil
}

// OrgInvitesAccept accepts an invite using a token.
func (a *App) OrgInvitesAccept(org, token string) error {
	remote, err := a.loadRemote()
	if err != nil {
		return err
	}
	resp, err := a.doPOSTJSON(remote+"/orgs/"+org+"/invites/"+token+"/accept", struct{}{})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return a.readError(resp)
	}
	fmt.Fprintf(a.Out, "Accepted invite for org '%s'\n", org)
	return nil
}

// OrgInvitesRevoke revokes a pending invite by ID.
func (a *App) OrgInvitesRevoke(org, inviteID string) error {
	remote, err := a.loadRemote()
	if err != nil {
		return err
	}
	resp, err := a.doDELETE(remote + "/orgs/" + org + "/invites/" + inviteID)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return a.readError(resp)
	}
	fmt.Fprintf(a.Out, "Revoked invite '%s' from org '%s'\n", inviteID, org)
	return nil
}

// ---------------------------------------------------------------------------
// Role management
// ---------------------------------------------------------------------------

// Roles lists roles for the current repository.
func (a *App) Roles() error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	resp, err := a.doGET(repoBase(cfg) + "/roles")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	var r model.RolesResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	fmt.Fprintf(a.Out, "%-30s  %s\n", "IDENTITY", "ROLE")
	for _, role := range r.Roles {
		fmt.Fprintf(a.Out, "%-30s  %s\n", role.Identity, string(role.Role))
	}
	return nil
}

// RolesSet grants or updates a role for an identity on the current repository.
func (a *App) RolesSet(identity, role string) error {
	switch model.RoleType(role) {
	case model.RoleReader, model.RoleWriter, model.RoleMaintainer, model.RoleAdmin:
	default:
		return fmt.Errorf("role must be one of: reader, writer, maintainer, admin")
	}
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	resp, err := a.doPUTJSON(repoBase(cfg)+"/roles/"+identity, model.SetRoleRequest{Role: model.RoleType(role)})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	fmt.Fprintf(a.Out, "Set role '%s' for '%s'\n", role, identity)
	return nil
}

// RolesDelete removes a role assignment for an identity on the current repository.
func (a *App) RolesDelete(identity string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	resp, err := a.doDELETE(repoBase(cfg) + "/roles/" + identity)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return a.readError(resp)
	}
	fmt.Fprintf(a.Out, "Deleted role for '%s'\n", identity)
	return nil
}

// ---------------------------------------------------------------------------
// Release management
// ---------------------------------------------------------------------------

// releaseEntry is a local type for decoding release API responses.
type releaseEntry struct {
	ID        string    `json:"id"`
	Repo      string    `json:"repo"`
	Name      string    `json:"name"`
	Sequence  int64     `json:"sequence"`
	Body      string    `json:"body,omitempty"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

type listReleasesResponse struct {
	Releases []releaseEntry `json:"releases"`
}

// ReleaseCreate creates a named release. If sequence is 0, the server defaults
// to the current main head sequence.
func (a *App) ReleaseCreate(name string, sequence int64, notes string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}

	body := map[string]interface{}{
		"name": name,
	}
	if sequence != 0 {
		body["sequence"] = sequence
	}
	if notes != "" {
		body["body"] = notes
	}

	resp, err := a.postJSON(cfg, repoBase(cfg)+"/releases", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return a.readError(resp)
	}
	var rel releaseEntry
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	fmt.Fprintf(a.Out, "Created release '%s' at sequence %d\n", rel.Name, rel.Sequence)
	return nil
}

// ReleaseList lists all releases for the current repository.
func (a *App) ReleaseList() error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	resp, err := a.httpGet(cfg, repoBase(cfg)+"/releases")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	var r listReleasesResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	fmt.Fprintf(a.Out, "%-20s  %-10s  %-20s  %s\n", "NAME", "SEQUENCE", "CREATED BY", "CREATED AT")
	for _, rel := range r.Releases {
		fmt.Fprintf(a.Out, "%-20s  %-10d  %-20s  %s\n", rel.Name, rel.Sequence, rel.CreatedBy, rel.CreatedAt.Format("2006-01-02"))
	}
	return nil
}

// ReleaseShow prints release metadata and then shows the tree at that release's sequence.
func (a *App) ReleaseShow(name string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	resp, err := a.httpGet(cfg, repoBase(cfg)+"/releases/"+name)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("release '%s' not found", name)
	}
	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	var rel releaseEntry
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	fmt.Fprintf(a.Out, "Name:       %s\n", rel.Name)
	fmt.Fprintf(a.Out, "Sequence:   %d\n", rel.Sequence)
	fmt.Fprintf(a.Out, "Created by: %s\n", rel.CreatedBy)
	fmt.Fprintf(a.Out, "Created at: %s\n", rel.CreatedAt.Format("2006-01-02 15:04:05"))
	if rel.Body != "" {
		fmt.Fprintf(a.Out, "Notes:\n%s\n", rel.Body)
	}

	// Show the tree at the release sequence.
	q := url.Values{}
	q.Set("at", fmt.Sprintf("%d", rel.Sequence))
	treeResp, err := a.httpGet(cfg, repoBase(cfg)+"/tree?"+q.Encode())
	if err != nil {
		return err
	}
	defer treeResp.Body.Close()
	if treeResp.StatusCode != http.StatusOK {
		return a.readError(treeResp)
	}

	type treeEntry struct {
		Path        string `json:"path"`
		VersionID   string `json:"version_id"`
		ContentHash string `json:"content_hash"`
	}
	var entries []treeEntry
	if err := json.NewDecoder(treeResp.Body).Decode(&entries); err != nil {
		return fmt.Errorf("decoding tree: %w", err)
	}
	fmt.Fprintf(a.Out, "\nTree at sequence %d:\n", rel.Sequence)
	for _, e := range entries {
		fmt.Fprintf(a.Out, "  %s\n", e.Path)
	}
	return nil
}

// ReleaseDelete deletes a named release (admin only).
func (a *App) ReleaseDelete(name string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	resp, err := a.doDELETE(repoBase(cfg) + "/releases/" + name)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("release '%s' not found", name)
	}
	if resp.StatusCode != http.StatusNoContent {
		return a.readError(resp)
	}
	fmt.Fprintf(a.Out, "Deleted release '%s'\n", name)
	return nil
}

// ProposalOpen creates a new proposal for a branch.
// If branch is empty, the current workspace branch is used.
// BaseBranch defaults to "main" if empty.
func (a *App) ProposalOpen(branch, baseBranch, title, description string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if branch == "" {
		branch = cfg.Branch
	}
	if baseBranch == "" {
		baseBranch = "main"
	}
	if title == "" {
		return fmt.Errorf("--title is required")
	}

	req := model.CreateProposalRequest{
		Branch:      branch,
		BaseBranch:  baseBranch,
		Title:       title,
		Description: description,
	}
	resp, err := a.postJSON(cfg, repoBase(cfg)+"/proposals", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}

	var proposalResp model.CreateProposalResponse
	if err := json.NewDecoder(resp.Body).Decode(&proposalResp); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}

	fmt.Fprintf(a.Out, "Proposal opened: %s\n", proposalResp.ID)
	return nil
}

// ProposalList lists proposals for the repo.
// state defaults to "open" if empty.
func (a *App) ProposalList(state string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if state == "" {
		state = "open"
	}

	q := url.Values{}
	q.Set("state", state)
	resp, err := a.httpGet(cfg, repoBase(cfg)+"/proposals?"+q.Encode())
	if err != nil {
		return fmt.Errorf("fetching proposals: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}

	var proposals []model.Proposal
	if err := json.NewDecoder(resp.Body).Decode(&proposals); err != nil {
		return fmt.Errorf("decoding proposals: %w", err)
	}

	if len(proposals) == 0 {
		fmt.Fprintf(a.Out, "No %s proposals\n", state)
		return nil
	}

	fmt.Fprintf(a.Out, "%-36s  %-30s  %-40s  %-30s  %-8s  %s\n",
		"ID", "BRANCH", "TITLE", "AUTHOR", "STATE", "CREATED")
	for _, p := range proposals {
		title := p.Title
		if len(title) > 38 {
			title = title[:37] + "…"
		}
		fmt.Fprintf(a.Out, "%-36s  %-30s  %-40s  %-30s  %-8s  %s\n",
			p.ID, p.Branch, title, p.Author, string(p.State),
			p.CreatedAt.Format(time.RFC3339))
	}
	return nil
}

// ProposalClose closes an open proposal.
func (a *App) ProposalClose(proposalID string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}

	resp, err := a.postJSON(cfg, repoBase(cfg)+"/proposals/"+url.PathEscape(proposalID)+"/close", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return a.readError(resp)
	}

	fmt.Fprintf(a.Out, "Proposal %s closed\n", proposalID)
	return nil
}

// ---------------------------------------------------------------------------
// Subscription management
// ---------------------------------------------------------------------------

// SubscriptionCreate creates a new webhook subscription.
func (a *App) SubscriptionCreate(webhookURL, secret string, repo *string, eventTypes []string) error {
	remote, err := a.loadRemote()
	if err != nil {
		return err
	}
	webhookConfig, err := json.Marshal(map[string]string{"url": webhookURL, "secret": secret})
	if err != nil {
		return fmt.Errorf("encoding webhook config: %w", err)
	}
	req := model.CreateSubscriptionRequest{
		Repo:       repo,
		EventTypes: eventTypes,
		Backend:    "webhook",
		Config:     json.RawMessage(webhookConfig),
	}
	resp, err := a.doPOSTJSON(remote+"/subscriptions", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	var sub model.EventSubscription
	if err := json.NewDecoder(resp.Body).Decode(&sub); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	fmt.Fprintf(a.Out, "Created subscription '%s'\n", sub.ID)
	return nil
}

// SubscriptionList lists all webhook subscriptions.
func (a *App) SubscriptionList() error {
	remote, err := a.loadRemote()
	if err != nil {
		return err
	}
	resp, err := a.doGET(remote + "/subscriptions")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	var r model.ListSubscriptionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	fmt.Fprintf(a.Out, "%-36s  %-20s  %-10s  %s\n", "ID", "REPO", "BACKEND", "SUSPENDED")
	for _, sub := range r.Subscriptions {
		repo := "(all)"
		if sub.Repo != nil {
			repo = *sub.Repo
		}
		suspended := "no"
		if sub.SuspendedAt != nil {
			suspended = sub.SuspendedAt.Format("2006-01-02")
		}
		fmt.Fprintf(a.Out, "%-36s  %-20s  %-10s  %s\n", sub.ID, repo, sub.Backend, suspended)
	}
	return nil
}

// SubscriptionDelete deletes a subscription by ID.
func (a *App) SubscriptionDelete(id string) error {
	remote, err := a.loadRemote()
	if err != nil {
		return err
	}
	resp, err := a.doDELETE(remote + "/subscriptions/" + url.PathEscape(id))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	fmt.Fprintf(a.Out, "Deleted subscription '%s'\n", id)
	return nil
}

// SubscriptionResume resumes a suspended subscription by ID.
func (a *App) SubscriptionResume(id string) error {
	remote, err := a.loadRemote()
	if err != nil {
		return err
	}
	resp, err := a.doPOSTJSON(remote+"/subscriptions/"+url.PathEscape(id)+"/resume", struct{}{})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return a.readError(resp)
	}
	fmt.Fprintf(a.Out, "Resumed subscription '%s'\n", id)
	return nil
}

// ---------------------------------------------------------------------------
// Issue management
// ---------------------------------------------------------------------------

// IssueList lists issues for the repo.
// state defaults to "open" if empty.
func (a *App) IssueList(state, author string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if state == "" {
		state = "open"
	}
	q := url.Values{}
	q.Set("state", state)
	if author != "" {
		q.Set("author", author)
	}
	resp, err := a.httpGet(cfg, repoBase(cfg)+"/issues?"+q.Encode())
	if err != nil {
		return fmt.Errorf("fetching issues: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	var r model.ListIssuesResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	if len(r.Issues) == 0 {
		fmt.Fprintf(a.Out, "No %s issues\n", state)
		return nil
	}
	fmt.Fprintf(a.Out, "%-6s  %-40s  %-30s  %-8s  %s\n", "NUMBER", "TITLE", "AUTHOR", "STATE", "CREATED")
	for _, iss := range r.Issues {
		title := iss.Title
		if len(title) > 38 {
			title = title[:37] + "…"
		}
		fmt.Fprintf(a.Out, "%-6d  %-40s  %-30s  %-8s  %s\n",
			iss.Number, title, iss.Author, string(iss.State),
			iss.CreatedAt.Format(time.RFC3339))
	}
	return nil
}

// IssueCreate creates a new issue.
func (a *App) IssueCreate(title, body string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if title == "" {
		return fmt.Errorf("--title is required")
	}
	req := model.CreateIssueRequest{
		Title: title,
		Body:  body,
	}
	resp, err := a.postJSON(cfg, repoBase(cfg)+"/issues", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	var r model.CreateIssueResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	fmt.Fprintf(a.Out, "Created issue #%d\n", r.Number)
	return nil
}

// IssueShow shows details for a single issue, including comments and refs.
func (a *App) IssueShow(number int64) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	base := repoBase(cfg) + "/issues/" + strconv.FormatInt(number, 10)

	resp, err := a.httpGet(cfg, base)
	if err != nil {
		return fmt.Errorf("fetching issue: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	var iss model.Issue
	if err := json.NewDecoder(resp.Body).Decode(&iss); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}

	fmt.Fprintf(a.Out, "Issue #%d: %s\n", iss.Number, iss.Title)
	fmt.Fprintf(a.Out, "State:   %s\n", string(iss.State))
	fmt.Fprintf(a.Out, "Author:  %s\n", iss.Author)
	fmt.Fprintf(a.Out, "Created: %s\n", iss.CreatedAt.Format(time.RFC3339))
	if iss.CloseReason != nil {
		fmt.Fprintf(a.Out, "Closed:  %s\n", string(*iss.CloseReason))
	}
	if iss.Body != "" {
		fmt.Fprintf(a.Out, "\n%s\n", iss.Body)
	}

	resp2, err := a.httpGet(cfg, base+"/comments")
	if err != nil {
		return fmt.Errorf("fetching comments: %w", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode == http.StatusOK {
		var cr model.ListIssueCommentsResponse
		if err := json.NewDecoder(resp2.Body).Decode(&cr); err == nil && len(cr.Comments) > 0 {
			fmt.Fprintf(a.Out, "\nComments (%d):\n", len(cr.Comments))
			for _, c := range cr.Comments {
				fmt.Fprintf(a.Out, "  [%s] %s: %s\n", c.CreatedAt.Format("2006-01-02"), c.Author, c.Body)
			}
		}
	}

	resp3, err := a.httpGet(cfg, base+"/refs")
	if err != nil {
		return fmt.Errorf("fetching refs: %w", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode == http.StatusOK {
		var rr model.ListIssueRefsResponse
		if err := json.NewDecoder(resp3.Body).Decode(&rr); err == nil && len(rr.Refs) > 0 {
			fmt.Fprintf(a.Out, "\nRefs (%d):\n", len(rr.Refs))
			for _, ref := range rr.Refs {
				fmt.Fprintf(a.Out, "  %s: %s\n", string(ref.RefType), ref.RefID)
			}
		}
	}

	return nil
}

// IssueClose closes an issue.
func (a *App) IssueClose(number int64, reason string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if reason == "" {
		reason = string(model.IssueCloseReasonCompleted)
	}
	req := model.CloseIssueRequest{Reason: model.IssueCloseReason(reason)}
	resp, err := a.postJSON(cfg, repoBase(cfg)+"/issues/"+strconv.FormatInt(number, 10)+"/close", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	fmt.Fprintf(a.Out, "Closed issue #%d\n", number)
	return nil
}

// IssueReopen reopens a closed issue.
func (a *App) IssueReopen(number int64) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	resp, err := a.postJSON(cfg, repoBase(cfg)+"/issues/"+strconv.FormatInt(number, 10)+"/reopen", model.ReopenIssueRequest{})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	fmt.Fprintf(a.Out, "Reopened issue #%d\n", number)
	return nil
}

// IssueCommentAdd adds a comment to an issue.
func (a *App) IssueCommentAdd(number int64, body string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if body == "" {
		return fmt.Errorf("--body is required")
	}
	req := model.CreateIssueCommentRequest{Body: body}
	resp, err := a.postJSON(cfg, repoBase(cfg)+"/issues/"+strconv.FormatInt(number, 10)+"/comments", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	var r model.CreateIssueCommentResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	fmt.Fprintf(a.Out, "Added comment %s\n", r.ID)
	return nil
}

// IssueCommentEdit edits an existing issue comment.
// issueNumber is required to construct the URL path.
func (a *App) IssueCommentEdit(issueNumber int64, commentID, body string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if body == "" {
		return fmt.Errorf("--body is required")
	}
	req := model.UpdateIssueCommentRequest{Body: body}
	urlPath := repoBase(cfg) + "/issues/" + strconv.FormatInt(issueNumber, 10) + "/comments/" + url.PathEscape(commentID)
	resp, err := a.patchJSON(cfg, urlPath, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	fmt.Fprintf(a.Out, "Updated comment %s\n", commentID)
	return nil
}

// IssueRefs lists cross-references for an issue.
func (a *App) IssueRefs(number int64) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	resp, err := a.httpGet(cfg, repoBase(cfg)+"/issues/"+strconv.FormatInt(number, 10)+"/refs")
	if err != nil {
		return fmt.Errorf("fetching refs: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	var r model.ListIssueRefsResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	if len(r.Refs) == 0 {
		fmt.Fprintf(a.Out, "No refs for issue #%d\n", number)
		return nil
	}
	fmt.Fprintf(a.Out, "%-10s  %-36s  %s\n", "TYPE", "REF_ID", "CREATED")
	for _, ref := range r.Refs {
		fmt.Fprintf(a.Out, "%-10s  %-36s  %s\n",
			string(ref.RefType), ref.RefID, ref.CreatedAt.Format(time.RFC3339))
	}
	return nil
}

// IssueTie ties a proposal or commit ref to an issue.
func (a *App) IssueTie(number int64, refType, refID string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	req := model.AddIssueRefRequest{
		RefType: model.IssueRefType(refType),
		RefID:   refID,
	}
	resp, err := a.postJSON(cfg, repoBase(cfg)+"/issues/"+strconv.FormatInt(number, 10)+"/refs", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	fmt.Fprintf(a.Out, "Tied %s %s to issue #%d\n", refType, refID, number)
	return nil
}
