package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"github.com/dlorenc/docstore/internal/model"
)

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