package docstore

import (
	"context"
	"net/url"
	"strconv"
	"strings"

	"github.com/dlorenc/docstore/api"
)

// RepoClient exposes the repo-scoped endpoints for a single repository. It
// is created by Client.Repo and is cheap to construct.
type RepoClient struct {
	client *Client
	repo   string // full "owner/name" path
}

// Name returns the repo path this client is bound to.
func (r *RepoClient) Name() string { return r.repo }

// filePath builds the URL suffix for a file endpoint, escaping each slash-
// separated segment so arbitrary paths ("configs/prod/app.yaml") survive.
func filePath(path string) string {
	path = strings.Trim(path, "/")
	if path == "" {
		return ""
	}
	parts := strings.Split(path, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// ---------------------------------------------------------------------------
// Files and trees
// ---------------------------------------------------------------------------

// File retrieves the contents of a file. Use AtHead, AtSequence, or AtRelease
// to control which version is returned; by default the server returns the
// head of main.
func (r *RepoClient) File(ctx context.Context, path string, opts ...FileOption) (*api.FileResponse, error) {
	params := buildParams(opts)
	suffix := "/file/" + filePath(path)
	var out api.FileResponse
	if err := r.client.doJSON(ctx, "GET", repoPath(r.repo, suffix), params.query, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// FileHistory lists commits that touched path in reverse-chronological order.
func (r *RepoClient) FileHistory(ctx context.Context, path string, opts ...HistoryOption) (*api.FileHistoryResponse, error) {
	params := buildParams(opts)
	suffix := "/file/" + filePath(path) + "/history"
	var out api.FileHistoryResponse
	if err := r.client.doJSON(ctx, "GET", repoPath(r.repo, suffix), params.query, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Tree materialises the directory tree of a branch or sequence.
func (r *RepoClient) Tree(ctx context.Context, opts ...TreeOption) (*api.TreeResponse, error) {
	params := buildParams(opts)
	var out api.TreeResponse
	if err := r.client.doJSON(ctx, "GET", repoPath(r.repo, "/tree"), params.query, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------------------------------------------------------------------------
// Commits
// ---------------------------------------------------------------------------

// Commit creates one atomic commit containing the files in req. A FileChange
// with nil Content is a delete.
func (r *RepoClient) Commit(ctx context.Context, req api.CommitRequest) (*api.CommitResponse, error) {
	var out api.CommitResponse
	if err := r.client.doJSON(ctx, "POST", repoPath(r.repo, "/commit"), nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetCommit returns metadata and the file change set for a specific commit
// sequence.
func (r *RepoClient) GetCommit(ctx context.Context, sequence int64) (*api.GetCommitResponse, error) {
	suffix := "/commit/" + strconv.FormatInt(sequence, 10)
	var out api.GetCommitResponse
	if err := r.client.doJSON(ctx, "GET", repoPath(r.repo, suffix), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------------------------------------------------------------------------
// Branches
// ---------------------------------------------------------------------------

// Branches lists branches in the repo.
func (r *RepoClient) Branches(ctx context.Context, opts ...BranchListOption) (*api.BranchesResponse, error) {
	params := buildParams(opts)
	var out api.BranchesResponse
	if err := r.client.doJSON(ctx, "GET", repoPath(r.repo, "/branches"), params.query, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateBranch creates a new branch rooted at the current head of main.
func (r *RepoClient) CreateBranch(ctx context.Context, req api.CreateBranchRequest) (*api.CreateBranchResponse, error) {
	var out api.CreateBranchResponse
	if err := r.client.doJSON(ctx, "POST", repoPath(r.repo, "/branch"), nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Branch returns the current pointer and status for one branch.
func (r *RepoClient) Branch(ctx context.Context, name string) (*api.Branch, error) {
	suffix := "/branch/" + url.PathEscape(name)
	var out api.Branch
	if err := r.client.doJSON(ctx, "GET", repoPath(r.repo, suffix), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateBranch mutates branch flags (currently only draft).
func (r *RepoClient) UpdateBranch(ctx context.Context, name string, req api.UpdateBranchRequest) (*api.UpdateBranchResponse, error) {
	suffix := "/branch/" + url.PathEscape(name)
	var out api.UpdateBranchResponse
	if err := r.client.doJSON(ctx, "PATCH", repoPath(r.repo, suffix), nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteBranch marks a branch merged or abandoned.
func (r *RepoClient) DeleteBranch(ctx context.Context, name string) (*api.DeleteBranchResponse, error) {
	suffix := "/branch/" + url.PathEscape(name)
	var out api.DeleteBranchResponse
	if err := r.client.doJSON(ctx, "DELETE", repoPath(r.repo, suffix), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// BranchStatus reports mergeability and policy evaluation for a branch.
func (r *RepoClient) BranchStatus(ctx context.Context, name string) (*api.BranchStatusResponse, error) {
	suffix := "/branch/" + url.PathEscape(name) + "/status"
	var out api.BranchStatusResponse
	if err := r.client.doJSON(ctx, "GET", repoPath(r.repo, suffix), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------------------------------------------------------------------------
// Merge, rebase, diff
// ---------------------------------------------------------------------------

// Merge fast-forwards or three-way-merges a branch into main. On conflicts
// the returned error is a *ConflictError; on policy failure a *PolicyError.
func (r *RepoClient) Merge(ctx context.Context, req api.MergeRequest) (*api.MergeResponse, error) {
	var out api.MergeResponse
	if err := r.client.doJSON(ctx, "POST", repoPath(r.repo, "/merge"), nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Rebase replays a branch onto the current head of main. On conflicts the
// returned error is a *ConflictError.
func (r *RepoClient) Rebase(ctx context.Context, req api.RebaseRequest) (*api.RebaseResponse, error) {
	var out api.RebaseResponse
	if err := r.client.doJSON(ctx, "POST", repoPath(r.repo, "/rebase"), nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Diff returns the set of file changes between a branch and main, plus any
// conflicts.
func (r *RepoClient) Diff(ctx context.Context, branch string) (*api.DiffResponse, error) {
	q := url.Values{"branch": []string{branch}}
	var out api.DiffResponse
	if err := r.client.doJSON(ctx, "GET", repoPath(r.repo, "/diff"), q, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------------------------------------------------------------------------
// Reviews and checks
// ---------------------------------------------------------------------------

// SubmitReview records a review on a branch.
func (r *RepoClient) SubmitReview(ctx context.Context, req api.CreateReviewRequest) (*api.CreateReviewResponse, error) {
	var out api.CreateReviewResponse
	if err := r.client.doJSON(ctx, "POST", repoPath(r.repo, "/review"), nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Reviews lists reviews for a branch.
func (r *RepoClient) Reviews(ctx context.Context, branch string, opts ...ReviewListOption) ([]api.Review, error) {
	params := buildParams(opts)
	suffix := "/branch/" + url.PathEscape(branch) + "/reviews"
	var out []api.Review
	if err := r.client.doJSON(ctx, "GET", repoPath(r.repo, suffix), params.query, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ReportCheck records a CI check result on a branch.
func (r *RepoClient) ReportCheck(ctx context.Context, req api.CreateCheckRunRequest) (*api.CreateCheckRunResponse, error) {
	var out api.CreateCheckRunResponse
	if err := r.client.doJSON(ctx, "POST", repoPath(r.repo, "/check"), nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Checks lists check runs for a branch.
func (r *RepoClient) Checks(ctx context.Context, branch string, opts ...CheckListOption) ([]api.CheckRun, error) {
	params := buildParams(opts)
	suffix := "/branch/" + url.PathEscape(branch) + "/checks"
	var out []api.CheckRun
	if err := r.client.doJSON(ctx, "GET", repoPath(r.repo, suffix), params.query, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Proposals
// ---------------------------------------------------------------------------

// OpenProposal creates a new proposal to merge a branch.
func (r *RepoClient) OpenProposal(ctx context.Context, req api.CreateProposalRequest) (*api.CreateProposalResponse, error) {
	var out api.CreateProposalResponse
	if err := r.client.doJSON(ctx, "POST", repoPath(r.repo, "/proposals"), nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListProposals lists proposals for the repo.
func (r *RepoClient) ListProposals(ctx context.Context, opts ...ProposalOption) ([]*api.Proposal, error) {
	params := buildParams(opts)
	var out []*api.Proposal
	if err := r.client.doJSON(ctx, "GET", repoPath(r.repo, "/proposals"), params.query, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CloseProposal closes an open proposal.
func (r *RepoClient) CloseProposal(ctx context.Context, proposalID string) error {
	suffix := "/proposals/" + url.PathEscape(proposalID) + "/close"
	return r.client.doJSON(ctx, "POST", repoPath(r.repo, suffix), nil, nil, nil)
}

// ---------------------------------------------------------------------------
// Roles
// ---------------------------------------------------------------------------

// Roles lists the role assignments on this repo.
func (r *RepoClient) Roles(ctx context.Context) (*api.RolesResponse, error) {
	var out api.RolesResponse
	if err := r.client.doJSON(ctx, "GET", repoPath(r.repo, "/roles"), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetRole assigns a role to an identity. Admin-only.
func (r *RepoClient) SetRole(ctx context.Context, identity string, role api.RoleType) error {
	suffix := "/roles/" + url.PathEscape(identity)
	return r.client.doJSON(ctx, "PUT", repoPath(r.repo, suffix), nil, api.SetRoleRequest{Role: role}, nil)
}

// DeleteRole removes an identity's role. Admin-only.
func (r *RepoClient) DeleteRole(ctx context.Context, identity string) error {
	suffix := "/roles/" + url.PathEscape(identity)
	return r.client.doJSON(ctx, "DELETE", repoPath(r.repo, suffix), nil, nil, nil)
}

// ---------------------------------------------------------------------------
// Releases
// ---------------------------------------------------------------------------

// CreateRelease pins a sequence under a named release.
func (r *RepoClient) CreateRelease(ctx context.Context, req api.CreateReleaseRequest) (*api.Release, error) {
	var out api.Release
	if err := r.client.doJSON(ctx, "POST", repoPath(r.repo, "/releases"), nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListReleases returns releases in reverse-chronological order.
func (r *RepoClient) ListReleases(ctx context.Context, opts ...ReleaseListOption) (*api.ListReleasesResponse, error) {
	params := buildParams(opts)
	var out api.ListReleasesResponse
	if err := r.client.doJSON(ctx, "GET", repoPath(r.repo, "/releases"), params.query, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetRelease returns a named release.
func (r *RepoClient) GetRelease(ctx context.Context, name string) (*api.Release, error) {
	suffix := "/releases/" + url.PathEscape(name)
	var out api.Release
	if err := r.client.doJSON(ctx, "GET", repoPath(r.repo, suffix), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteRelease removes a named release. Admin-only.
func (r *RepoClient) DeleteRelease(ctx context.Context, name string) error {
	suffix := "/releases/" + url.PathEscape(name)
	return r.client.doJSON(ctx, "DELETE", repoPath(r.repo, suffix), nil, nil, nil)
}

// ---------------------------------------------------------------------------
// Chain verification and admin
// ---------------------------------------------------------------------------

// ChainSegment returns commits in [from, to] inclusive as ordered chain
// entries, for clients that want to verify the hash chain locally.
func (r *RepoClient) ChainSegment(ctx context.Context, from, to int64) ([]api.ChainEntry, error) {
	q := url.Values{
		"from": []string{strconv.FormatInt(from, 10)},
		"to":   []string{strconv.FormatInt(to, 10)},
	}
	var out []api.ChainEntry
	if err := r.client.doJSON(ctx, "GET", repoPath(r.repo, "/chain"), q, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Purge deletes abandoned/merged branches and their commits older than the
// cutoff. Admin-only.
func (r *RepoClient) Purge(ctx context.Context, req api.PurgeRequest) (*api.PurgeResponse, error) {
	var out api.PurgeResponse
	if err := r.client.doJSON(ctx, "POST", repoPath(r.repo, "/purge"), nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
