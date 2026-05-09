package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"github.com/dlorenc/docstore/internal/model"
)

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