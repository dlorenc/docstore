// Package cli implements the ds command-line client for docstore.
package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"time"

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

// Config is the persistent CLI configuration stored in .docstore/config.json.
type Config struct {
	Remote string `json:"remote"`
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Author string `json:"author"`
}

// State tracks the last-synced tree for offline status.
type State struct {
	Branch   string            `json:"branch"`
	Sequence int64             `json:"sequence"`
	Files    map[string]string `json:"files"` // path -> content_hash
}

// App is the CLI application. All fields are injectable for testing.
type App struct {
	Dir  string       // working directory
	Out  io.Writer    // output writer
	HTTP *http.Client // HTTP client (mockable)
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
	if err := a.syncTree(cfg, "main"); err != nil {
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
			files = append(files, model.FileChange{Path: path, Content: content})
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

	if err := a.saveState(st); err != nil {
		return err
	}

	fmt.Fprintf(a.Out, "Committed sequence %d (%d files)\n", commitResp.Sequence, len(files))
	return nil
}

// CheckoutNew creates a new branch and switches to it.
func (a *App) CheckoutNew(branch string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}

	req := model.CreateBranchRequest{Name: branch}
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

	fmt.Fprintf(a.Out, "Switched to new branch '%s' (base sequence %d)\n", branch, branchResp.BaseSequence)
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

	return a.syncTree(cfg, branch)
}

// Pull syncs local files from the current branch on the server.
func (a *App) Pull() error {
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

	return a.syncTree(cfg, cfg.Branch)
}

// syncTree fetches the full tree from the server and updates local files.
func (a *App) syncTree(cfg *Config, branch string) error {
	st, err := a.loadState()
	if err != nil {
		return err
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
	return a.syncTree(cfg, "main")
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
func (a *App) Branches(status string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if status == "" {
		status = "active"
	}

	q := url.Values{}
	q.Set("status", status)
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

	fmt.Fprintf(a.Out, "%-30s %-6s %-6s %-30s %-10s\n", "BRANCH", "HEAD", "BASE", "AUTHOR", "STATUS")
	for _, b := range branches {
		// Author is not directly available in Branch model; use empty placeholder.
		fmt.Fprintf(a.Out, "%-30s %-6d %-6d %-30s %-10s\n",
			b.Name, b.HeadSequence, b.BaseSequence, "", string(b.Status))
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

	resp, err := a.httpGet(cfg, repoBase(cfg)+"/branch/"+branch+"/reviews")
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

// Checks lists check runs for a branch (defaults to current branch if empty).
func (a *App) Checks(branch string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if branch == "" {
		branch = cfg.Branch
	}

	resp, err := a.httpGet(cfg, repoBase(cfg)+"/branch/"+branch+"/checks")
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
	}
	return nil
}

// Check reports a CI check result for a branch.
func (a *App) Check(branch, name, status string) error {
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
