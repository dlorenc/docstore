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
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/dlorenc/docstore/internal/hash"
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

// configDir is the local metadata directory for the ds CLI.
// Note: previously this was ".docstore"; existing users must rename .docstore/ to .ds/.
// Note: .docstore/ci.yaml is the CI config path (separate concern) and is NOT affected.
const configDir = ".ds"
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
	Files      []hash.File `json:"files"`
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

// Config is the persistent CLI configuration stored in .ds/config.json.
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
// .ds/config.json; if absent it falls back to App.DefaultRemote.
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
// It skips the .ds directory.
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

	slices.Sort(newFiles)
	slices.Sort(modifiedFiles)
	slices.Sort(deletedFiles)

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
	slices.SortFunc(files, func(a, b model.FileChange) int { return strings.Compare(a.Path, b.Path) })

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
		prevHash = hash.GenesisHash
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
			prevHash = hash.GenesisHash
			continue
		}
		computed := hash.CommitHash(prevHash, e.Sequence, cfg.Repo, e.Branch, e.Author, e.Message, e.CreatedAt, e.Files)
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

	prevHash := hash.GenesisHash
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
			prevHash = hash.GenesisHash
			continue
		}
		if !foundFirst {
			// The first non-NULL commit on this branch is the chain anchor; accept its hash as-is.
			foundFirst = true
			prevHash = *e.CommitHash
			fmt.Fprintf(a.Out, "seq %-4d  OK    %.16s  %s\n", e.Sequence, *e.CommitHash, e.Message)
			continue
		}
		computed := hash.CommitHash(prevHash, e.Sequence, cfg.Repo, e.Branch, e.Author, e.Message, e.CreatedAt, e.Files)
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
			if err := os.Remove(localPath); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("removing %s: %w", localPath, err)
			}
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

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response body: %w", err)
	}

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

// doRequest is the single HTTP helper used by all sub-helpers below.
// method is the HTTP method, urlStr is the full URL, body is the JSON-encoded
// request body (nil for GET/DELETE), and identity is the X-DocStore-Identity
// header value (empty string to omit the header).
func (a *App) doRequest(method, urlStr string, body any, identity string) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, urlStr, bodyReader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if identity != "" {
		req.Header.Set("X-DocStore-Identity", identity)
	}
	return a.HTTP.Do(req)
}

// httpGet sends a GET request with the X-DocStore-Identity header set.
func (a *App) httpGet(cfg *Config, urlStr string) (*http.Response, error) {
	return a.doRequest("GET", urlStr, nil, cfg.Author)
}

// doGET sends a GET request without requiring a workspace config.
func (a *App) doGET(urlStr string) (*http.Response, error) {
	return a.doRequest("GET", urlStr, nil, "")
}

// doDELETE sends a DELETE request without requiring a workspace config.
func (a *App) doDELETE(urlStr string) (*http.Response, error) {
	return a.doRequest("DELETE", urlStr, nil, "")
}

// doPOSTJSON sends a POST request with JSON body without requiring a workspace config.
func (a *App) doPOSTJSON(urlStr string, body any) (*http.Response, error) {
	return a.doRequest("POST", urlStr, body, "")
}

// doPUTJSON sends a PUT request with JSON body without requiring a workspace config.
func (a *App) doPUTJSON(urlStr string, body any) (*http.Response, error) {
	return a.doRequest("PUT", urlStr, body, "")
}

// postJSON sends a POST request with a JSON body and the X-DocStore-Identity header set.
func (a *App) postJSON(cfg *Config, urlStr string, body any) (*http.Response, error) {
	return a.doRequest("POST", urlStr, body, cfg.Author)
}

// patchJSON sends a PATCH request with a JSON body and the X-DocStore-Identity header set.
func (a *App) patchJSON(cfg *Config, urlStr string, body any) (*http.Response, error) {
	return a.doRequest("PATCH", urlStr, body, cfg.Author)
}

// readError extracts an error message from an API error response.
func (a *App) readError(resp *http.Response) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("server error (status %d)", resp.StatusCode)
	}
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

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response body: %w", err)
	}

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

// TUI launches the terminal UI reading config from .ds/config.json.
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

	// Best-effort cleanup of conflict files; the commit already succeeded.
	_ = os.Remove(mainConflict)
	_ = os.Remove(branchConflict)

	fmt.Fprintf(a.Out, "resolved %s, committed as sequence %d\n", path, commitResp.Sequence)
	return nil
}






// ---------------------------------------------------------------------------
// Repo-level secrets
// ---------------------------------------------------------------------------

// SecretMetadata is the JSON shape returned by the secrets list/set endpoints.
// Plaintext is never returned by any read path; this struct intentionally has
// no Value field.
type SecretMetadata struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	SizeBytes   int        `json:"size_bytes"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedBy   *string    `json:"updated_by,omitempty"`
	UpdatedAt   *time.Time `json:"updated_at,omitempty"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
}

// secretNamePattern enforces the POSIX env-var shape from the design doc:
// uppercase letter followed by up to 63 uppercase letters, digits, or
// underscores.
var secretNamePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// maxSecretValueBytes caps secret payloads at 32 KiB. Larger values belong in
// object storage, not in the secrets table.
const maxSecretValueBytes = 32 * 1024

// reservedSecretPrefix is forbidden at write time; reserved for built-in
// secrets injected by docstore (e.g. docstore_oidc_request_token in its
// uppercased form).
const reservedSecretPrefix = "DOCSTORE_"

// validateSecretName checks the name pattern and reserved-prefix rule. It
// never includes the value in any error message.
func validateSecretName(name string) error {
	if !secretNamePattern.MatchString(name) {
		return fmt.Errorf("invalid secret name %q: must match [A-Z][A-Z0-9_]{0,63}", name)
	}
	if strings.HasPrefix(name, reservedSecretPrefix) {
		return fmt.Errorf("invalid secret name %q: %s prefix is reserved", name, reservedSecretPrefix)
	}
	return nil
}

// SecretsList prints repo secret metadata (never plaintext values) as a
// tab-separated table.
func (a *App) SecretsList() error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	resp, err := a.httpGet(cfg, repoBase(cfg)+"/secrets")
	if err != nil {
		return fmt.Errorf("listing secrets: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return a.readError(resp)
	}
	var secrets []SecretMetadata
	if err := json.NewDecoder(resp.Body).Decode(&secrets); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}

	tw := tabwriter.NewWriter(a.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSIZE\tUPDATED\tLAST_USED\tDESCRIPTION")
	for _, s := range secrets {
		updated := s.CreatedAt.Format(time.RFC3339)
		if s.UpdatedAt != nil {
			updated = s.UpdatedAt.Format(time.RFC3339)
		}
		lastUsed := "-"
		if s.LastUsedAt != nil {
			lastUsed = s.LastUsedAt.Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n",
			s.Name, s.SizeBytes, updated, lastUsed, s.Description)
	}
	return tw.Flush()
}

// SecretsSet sets the named secret to the contents of valueReader. The caller
// is responsible for sourcing valueReader (stdin or a file). description may
// be empty. Errors never include the secret value.
func (a *App) SecretsSet(name string, valueReader io.Reader, description string) error {
	if err := validateSecretName(name); err != nil {
		return err
	}
	if valueReader == nil {
		return fmt.Errorf("secret value reader is required")
	}
	// Read at most maxSecretValueBytes+1 so we can detect oversized values
	// without buffering arbitrarily large input.
	limited := io.LimitReader(valueReader, int64(maxSecretValueBytes)+1)
	value, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("reading secret value: %w", err)
	}
	if len(value) == 0 {
		return fmt.Errorf("secret value is empty")
	}
	if len(value) > maxSecretValueBytes {
		return fmt.Errorf("secret value exceeds maximum size of %d bytes", maxSecretValueBytes)
	}

	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}

	body := struct {
		Value       string `json:"value"`
		Description string `json:"description,omitempty"`
	}{
		Value:       string(value),
		Description: description,
	}
	resp, err := a.doPUTJSON(repoBase(cfg)+"/secrets/"+name, body)
	if err != nil {
		// Wrap network-level errors generically; never echo the value.
		return fmt.Errorf("setting secret %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return a.readError(resp)
	}
	fmt.Fprintf(a.Out, "Set %s (%d bytes)\n", name, len(value))
	return nil
}

// SecretsUnset deletes the named secret.
func (a *App) SecretsUnset(name string) error {
	if err := validateSecretName(name); err != nil {
		return err
	}
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	resp, err := a.doDELETE(repoBase(cfg) + "/secrets/" + name)
	if err != nil {
		return fmt.Errorf("unsetting secret %s: %w", name, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		fmt.Fprintf(a.Out, "Unset %s\n", name)
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("secret %s not found", name)
	default:
		return a.readError(resp)
	}
}
