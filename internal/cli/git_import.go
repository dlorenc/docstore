package cli

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"github.com/dlorenc/docstore/internal/model"
)


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
		for fline := range strings.SplitSeq(strings.TrimRight(string(filesOut), "\n"), "\n") {
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
	for f := range strings.SplitSeq(strings.TrimRight(string(filesOut), "\n"), "\n") {
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