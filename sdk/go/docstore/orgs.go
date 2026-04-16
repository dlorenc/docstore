package docstore

import (
	"context"
	"net/url"

	"github.com/dlorenc/docstore/api"
)

// OrgsService groups organisation-level endpoints. Access it via
// Client.Orgs.
type OrgsService struct {
	client *Client
}

// Create creates a new organisation.
func (s *OrgsService) Create(ctx context.Context, req api.CreateOrgRequest) (*api.Org, error) {
	var out api.Org
	if err := s.client.doJSON(ctx, "POST", "/orgs", nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// List returns every organisation the caller can see.
func (s *OrgsService) List(ctx context.Context) (*api.ListOrgsResponse, error) {
	var out api.ListOrgsResponse
	if err := s.client.doJSON(ctx, "GET", "/orgs", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Get fetches one organisation by name.
func (s *OrgsService) Get(ctx context.Context, name string) (*api.Org, error) {
	var out api.Org
	if err := s.client.doJSON(ctx, "GET", "/orgs/"+url.PathEscape(name), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Delete removes an organisation.
func (s *OrgsService) Delete(ctx context.Context, name string) error {
	return s.client.doJSON(ctx, "DELETE", "/orgs/"+url.PathEscape(name), nil, nil, nil)
}

// Repos lists repos belonging to the organisation.
func (s *OrgsService) Repos(ctx context.Context, org string) (*api.ReposResponse, error) {
	var out api.ReposResponse
	if err := s.client.doJSON(ctx, "GET", "/orgs/"+url.PathEscape(org)+"/repos", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------------------------------------------------------------------------
// Members
// ---------------------------------------------------------------------------

// Members lists members of an organisation.
func (s *OrgsService) Members(ctx context.Context, org string) (*api.OrgMembersResponse, error) {
	var out api.OrgMembersResponse
	if err := s.client.doJSON(ctx, "GET", "/orgs/"+url.PathEscape(org)+"/members", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AddMember adds identity to an organisation with the given role. Org
// owner only.
func (s *OrgsService) AddMember(ctx context.Context, org, identity string, role api.OrgRole) (*api.OrgMember, error) {
	path := "/orgs/" + url.PathEscape(org) + "/members/" + url.PathEscape(identity)
	var out api.OrgMember
	if err := s.client.doJSON(ctx, "POST", path, nil, api.AddOrgMemberRequest{Role: role}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RemoveMember removes identity from an organisation. Org owner only.
func (s *OrgsService) RemoveMember(ctx context.Context, org, identity string) error {
	path := "/orgs/" + url.PathEscape(org) + "/members/" + url.PathEscape(identity)
	return s.client.doJSON(ctx, "DELETE", path, nil, nil, nil)
}

// ---------------------------------------------------------------------------
// Invites
// ---------------------------------------------------------------------------

// CreateInvite issues an invitation to join an organisation.
func (s *OrgsService) CreateInvite(ctx context.Context, org string, req api.CreateInviteRequest) (*api.CreateInviteResponse, error) {
	path := "/orgs/" + url.PathEscape(org) + "/invites"
	var out api.CreateInviteResponse
	if err := s.client.doJSON(ctx, "POST", path, nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListInvites returns pending invitations on an organisation.
func (s *OrgsService) ListInvites(ctx context.Context, org string) (*api.OrgInvitesResponse, error) {
	path := "/orgs/" + url.PathEscape(org) + "/invites"
	var out api.OrgInvitesResponse
	if err := s.client.doJSON(ctx, "GET", path, nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AcceptInvite accepts an invitation using its token.
func (s *OrgsService) AcceptInvite(ctx context.Context, org, token string) error {
	path := "/orgs/" + url.PathEscape(org) + "/invites/" + url.PathEscape(token) + "/accept"
	return s.client.doJSON(ctx, "POST", path, nil, nil, nil)
}

// RevokeInvite revokes a pending invitation by ID. Org owner only.
func (s *OrgsService) RevokeInvite(ctx context.Context, org, id string) error {
	path := "/orgs/" + url.PathEscape(org) + "/invites/" + url.PathEscape(id)
	return s.client.doJSON(ctx, "DELETE", path, nil, nil, nil)
}
