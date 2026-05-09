package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
	"github.com/dlorenc/docstore/internal/model"
)


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