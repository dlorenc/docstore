package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"github.com/dlorenc/docstore/internal/model"
)

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