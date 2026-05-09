package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)


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

	body := map[string]any{
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