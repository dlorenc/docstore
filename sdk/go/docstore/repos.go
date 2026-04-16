package docstore

import (
	"context"

	"github.com/dlorenc/docstore/api"
)

// ReposService groups repo CRUD endpoints (create/list/get/delete). For
// operations scoped inside a single repo, use Client.Repo.
type ReposService struct {
	client *Client
}

// Create creates a new repository under an existing org.
func (s *ReposService) Create(ctx context.Context, req api.CreateRepoRequest) (*api.Repo, error) {
	var out api.Repo
	if err := s.client.doJSON(ctx, "POST", "/repos", nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// List returns every repo the caller can see.
func (s *ReposService) List(ctx context.Context) (*api.ReposResponse, error) {
	var out api.ReposResponse
	if err := s.client.doJSON(ctx, "GET", "/repos", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Get fetches a repo by full path ("owner/name").
func (s *ReposService) Get(ctx context.Context, name string) (*api.Repo, error) {
	var out api.Repo
	if err := s.client.doJSON(ctx, "GET", repoBareCRUD(name), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Delete removes a repo by full path.
func (s *ReposService) Delete(ctx context.Context, name string) error {
	return s.client.doJSON(ctx, "DELETE", repoBareCRUD(name), nil, nil, nil)
}
