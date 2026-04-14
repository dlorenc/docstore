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

	"github.com/dlorenc/docstore/internal/model"
)

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

	// Extract repo from remote URL if embedded (e.g. https://host/repos/myrepo).
	// Strip the /repos/:name suffix from the base URL whether or not --repo was
	// provided explicitly, so that we never end up with a doubled path prefix.
	baseRemote := strings.TrimRight(remote, "/")
	if idx := strings.Index(baseRemote, "/repos/"); idx >= 0 {
		if repo == "" {
			parts := strings.SplitN(baseRemote[idx+len("/repos/"):], "/", 2)
			repo = parts[0]
		}
		baseRemote = baseRemote[:idx]
	} else if repo == "" {
		repo = "default"
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

// repoBase returns the /repos/:name base URL for the configured repo.
func repoBase(cfg *Config) string {
	repo := cfg.Repo
	if repo == "" {
		repo = "default"
	}
	return cfg.Remote + "/repos/" + repo
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
