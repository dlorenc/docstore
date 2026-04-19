package server

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/policy"
	"github.com/dlorenc/docstore/internal/store"
)

// mockStore implements WriteStore for testing.
type mockStore struct {
	commitFn        func(ctx context.Context, req model.CommitRequest) (*model.CommitResponse, error)
	createBranchFn  func(ctx context.Context, req model.CreateBranchRequest) (*model.CreateBranchResponse, error)
	mergeFn         func(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error)
	deleteBranchFn  func(ctx context.Context, repo, name string) error
	rebaseFn        func(ctx context.Context, req model.RebaseRequest) (*model.RebaseResponse, []db.MergeConflict, error)
	createRepoFn    func(ctx context.Context, req model.CreateRepoRequest) (*model.Repo, error)
	deleteRepoFn    func(ctx context.Context, name string) error
	listReposFn     func(ctx context.Context) ([]model.Repo, error)
	getRepoFn       func(ctx context.Context, name string) (*model.Repo, error)
	createReviewFn  func(ctx context.Context, repo, branch, reviewer string, status model.ReviewStatus, body string) (*model.Review, error)
	listReviewsFn   func(ctx context.Context, repo, branch string, atSeq *int64) ([]model.Review, error)
	createCheckRunFn func(ctx context.Context, repo, branch, checkName string, status model.CheckRunStatus, reporter string, logURL *string, atSequence *int64) (*model.CheckRun, error)
	listCheckRunsFn  func(ctx context.Context, repo, branch string, atSeq *int64) ([]model.CheckRun, error)
	getRoleFn      func(ctx context.Context, repo, identity string) (*model.Role, error)
	setRoleFn      func(ctx context.Context, repo, identity string, role model.RoleType) error
	deleteRoleFn   func(ctx context.Context, repo, identity string) error
	listRolesFn    func(ctx context.Context, repo string) ([]model.Role, error)
	hasAdminFn     func(ctx context.Context, repo string) (bool, error)
	purgeFn        func(ctx context.Context, req db.PurgeRequest) (*db.PurgeResult, error)
	createOrgFn      func(ctx context.Context, name, createdBy string) (*model.Org, error)
	getOrgFn         func(ctx context.Context, name string) (*model.Org, error)
	listOrgsFn       func(ctx context.Context) ([]model.Org, error)
	deleteOrgFn      func(ctx context.Context, name string) error
	listOrgReposFn   func(ctx context.Context, owner string) ([]model.Repo, error)

	getOrgMemberFn    func(ctx context.Context, org, identity string) (*model.OrgMember, error)
	addOrgMemberFn    func(ctx context.Context, org, identity string, role model.OrgRole, invitedBy string) error
	removeOrgMemberFn func(ctx context.Context, org, identity string) error
	listOrgMembersFn  func(ctx context.Context, org string) ([]model.OrgMember, error)
	createInviteFn    func(ctx context.Context, org, email string, role model.OrgRole, invitedBy, token string, expiresAt time.Time) (*model.OrgInvite, error)
	listInvitesFn     func(ctx context.Context, org string) ([]model.OrgInvite, error)
	acceptInviteFn    func(ctx context.Context, org, token, identity string) error
	revokeInviteFn    func(ctx context.Context, org, inviteID string) error
	updateBranchDraftFn func(ctx context.Context, repo, name string, draft bool) error

	createReleaseFn        func(ctx context.Context, repo, name string, sequence int64, body, createdBy string) (*model.Release, error)
	getReleaseFn           func(ctx context.Context, repo, name string) (*model.Release, error)
	listReleasesFn         func(ctx context.Context, repo string, limit int, afterID string) ([]model.Release, error)
	deleteReleaseFn        func(ctx context.Context, repo, name string) error
	commitSequenceExistsFn func(ctx context.Context, repo string, sequence int64) (bool, error)

	createReviewCommentFn func(ctx context.Context, repo, branch, path, versionID, body, author string, reviewID *string) (*model.ReviewComment, error)
	listReviewCommentsFn  func(ctx context.Context, repo, branch string, path *string) ([]model.ReviewComment, error)
	getReviewCommentFn    func(ctx context.Context, repo, id string) (*model.ReviewComment, error)
	deleteReviewCommentFn func(ctx context.Context, repo, id string) error

	createSubscriptionFn             func(ctx context.Context, req model.CreateSubscriptionRequest) (*model.EventSubscription, error)
	getSubscriptionFn                func(ctx context.Context, id string) (*model.EventSubscription, error)
	listSubscriptionsFn              func(ctx context.Context) ([]model.EventSubscription, error)
	listSubscriptionsByCreatorFn     func(ctx context.Context, createdBy string) ([]model.EventSubscription, error)
	deleteSubscriptionFn             func(ctx context.Context, id string) error
	resumeSubscriptionFn             func(ctx context.Context, id string) error
}

func (m *mockStore) Commit(ctx context.Context, req model.CommitRequest) (*model.CommitResponse, error) {
	return m.commitFn(ctx, req)
}

func (m *mockStore) CreateBranch(ctx context.Context, req model.CreateBranchRequest) (*model.CreateBranchResponse, error) {
	if m.createBranchFn != nil {
		return m.createBranchFn(ctx, req)
	}
	return nil, errors.New("not implemented")
}

func (m *mockStore) UpdateBranchDraft(ctx context.Context, repo, name string, draft bool) error {
	if m.updateBranchDraftFn != nil {
		return m.updateBranchDraftFn(ctx, repo, name, draft)
	}
	return nil
}

func (m *mockStore) Merge(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error) {
	if m.mergeFn != nil {
		return m.mergeFn(ctx, req)
	}
	return nil, nil, errors.New("not implemented")
}

func (m *mockStore) DeleteBranch(ctx context.Context, repo, name string) error {
	if m.deleteBranchFn != nil {
		return m.deleteBranchFn(ctx, repo, name)
	}
	return errors.New("not implemented")
}

func (m *mockStore) Rebase(ctx context.Context, req model.RebaseRequest) (*model.RebaseResponse, []db.MergeConflict, error) {
	if m.rebaseFn != nil {
		return m.rebaseFn(ctx, req)
	}
	return nil, nil, errors.New("not implemented")
}

// devID is the dev identity used in handler unit tests.
const devID = "test@example.com"

func (m *mockStore) CreateRepo(ctx context.Context, req model.CreateRepoRequest) (*model.Repo, error) {
	if m.createRepoFn != nil {
		return m.createRepoFn(ctx, req)
	}
	return nil, errors.New("not implemented")
}

func (m *mockStore) DeleteRepo(ctx context.Context, name string) error {
	if m.deleteRepoFn != nil {
		return m.deleteRepoFn(ctx, name)
	}
	return errors.New("not implemented")
}

func (m *mockStore) ListRepos(ctx context.Context) ([]model.Repo, error) {
	if m.listReposFn != nil {
		return m.listReposFn(ctx)
	}
	return nil, errors.New("not implemented")
}

func (m *mockStore) GetRepo(ctx context.Context, name string) (*model.Repo, error) {
	if m.getRepoFn != nil {
		return m.getRepoFn(ctx, name)
	}
	return &model.Repo{Name: name}, nil
}

func (m *mockStore) CreateReview(ctx context.Context, repo, branch, reviewer string, status model.ReviewStatus, body string) (*model.Review, error) {
	if m.createReviewFn != nil {
		return m.createReviewFn(ctx, repo, branch, reviewer, status, body)
	}
	return nil, errors.New("not implemented")
}

func (m *mockStore) ListReviews(ctx context.Context, repo, branch string, atSeq *int64) ([]model.Review, error) {
	if m.listReviewsFn != nil {
		return m.listReviewsFn(ctx, repo, branch, atSeq)
	}
	return nil, errors.New("not implemented")
}

func (m *mockStore) CreateCheckRun(ctx context.Context, repo, branch, checkName string, status model.CheckRunStatus, reporter string, logURL *string, atSequence *int64) (*model.CheckRun, error) {
	if m.createCheckRunFn != nil {
		return m.createCheckRunFn(ctx, repo, branch, checkName, status, reporter, logURL, atSequence)
	}
	return nil, errors.New("not implemented")
}

func (m *mockStore) ListCheckRuns(ctx context.Context, repo, branch string, atSeq *int64) ([]model.CheckRun, error) {
	if m.listCheckRunsFn != nil {
		return m.listCheckRunsFn(ctx, repo, branch, atSeq)
	}
	return nil, errors.New("not implemented")
}

func (m *mockStore) GetRole(ctx context.Context, repo, identity string) (*model.Role, error) {
	if m.getRoleFn != nil {
		return m.getRoleFn(ctx, repo, identity)
	}
	return nil, db.ErrRoleNotFound
}

func (m *mockStore) SetRole(ctx context.Context, repo, identity string, role model.RoleType) error {
	if m.setRoleFn != nil {
		return m.setRoleFn(ctx, repo, identity, role)
	}
	return nil
}

func (m *mockStore) DeleteRole(ctx context.Context, repo, identity string) error {
	if m.deleteRoleFn != nil {
		return m.deleteRoleFn(ctx, repo, identity)
	}
	return db.ErrRoleNotFound
}

func (m *mockStore) ListRoles(ctx context.Context, repo string) ([]model.Role, error) {
	if m.listRolesFn != nil {
		return m.listRolesFn(ctx, repo)
	}
	return []model.Role{}, nil
}

func (m *mockStore) HasAdmin(ctx context.Context, repo string) (bool, error) {
	if m.hasAdminFn != nil {
		return m.hasAdminFn(ctx, repo)
	}
	return false, nil
}

func (m *mockStore) Purge(ctx context.Context, req db.PurgeRequest) (*db.PurgeResult, error) {
	if m.purgeFn != nil {
		return m.purgeFn(ctx, req)
	}
	return nil, errors.New("not implemented")
}

func (m *mockStore) CreateOrg(ctx context.Context, name, createdBy string) (*model.Org, error) {
	if m.createOrgFn != nil {
		return m.createOrgFn(ctx, name, createdBy)
	}
	return &model.Org{Name: name}, nil
}

func (m *mockStore) GetOrg(ctx context.Context, name string) (*model.Org, error) {
	if m.getOrgFn != nil {
		return m.getOrgFn(ctx, name)
	}
	return &model.Org{Name: name}, nil
}

func (m *mockStore) ListOrgs(ctx context.Context) ([]model.Org, error) {
	if m.listOrgsFn != nil {
		return m.listOrgsFn(ctx)
	}
	return []model.Org{}, nil
}

func (m *mockStore) DeleteOrg(ctx context.Context, name string) error {
	if m.deleteOrgFn != nil {
		return m.deleteOrgFn(ctx, name)
	}
	return nil
}

func (m *mockStore) ListOrgRepos(ctx context.Context, owner string) ([]model.Repo, error) {
	if m.listOrgReposFn != nil {
		return m.listOrgReposFn(ctx, owner)
	}
	return []model.Repo{}, nil
}

func (m *mockStore) GetOrgMember(ctx context.Context, org, identity string) (*model.OrgMember, error) {
	if m.getOrgMemberFn != nil {
		return m.getOrgMemberFn(ctx, org, identity)
	}
	return nil, db.ErrOrgMemberNotFound
}

func (m *mockStore) AddOrgMember(ctx context.Context, org, identity string, role model.OrgRole, invitedBy string) error {
	if m.addOrgMemberFn != nil {
		return m.addOrgMemberFn(ctx, org, identity, role, invitedBy)
	}
	return nil
}

func (m *mockStore) RemoveOrgMember(ctx context.Context, org, identity string) error {
	if m.removeOrgMemberFn != nil {
		return m.removeOrgMemberFn(ctx, org, identity)
	}
	return db.ErrOrgMemberNotFound
}

func (m *mockStore) ListOrgMembers(ctx context.Context, org string) ([]model.OrgMember, error) {
	if m.listOrgMembersFn != nil {
		return m.listOrgMembersFn(ctx, org)
	}
	return []model.OrgMember{}, nil
}

func (m *mockStore) CreateInvite(ctx context.Context, org, email string, role model.OrgRole, invitedBy, token string, expiresAt time.Time) (*model.OrgInvite, error) {
	if m.createInviteFn != nil {
		return m.createInviteFn(ctx, org, email, role, invitedBy, token, expiresAt)
	}
	return &model.OrgInvite{ID: "test-invite-id", Token: token}, nil
}

func (m *mockStore) ListInvites(ctx context.Context, org string) ([]model.OrgInvite, error) {
	if m.listInvitesFn != nil {
		return m.listInvitesFn(ctx, org)
	}
	return []model.OrgInvite{}, nil
}

func (m *mockStore) AcceptInvite(ctx context.Context, org, token, identity string) error {
	if m.acceptInviteFn != nil {
		return m.acceptInviteFn(ctx, org, token, identity)
	}
	return nil
}

func (m *mockStore) RevokeInvite(ctx context.Context, org, inviteID string) error {
	if m.revokeInviteFn != nil {
		return m.revokeInviteFn(ctx, org, inviteID)
	}
	return nil
}

func (m *mockStore) CreateRelease(ctx context.Context, repo, name string, sequence int64, body, createdBy string) (*model.Release, error) {
	if m.createReleaseFn != nil {
		return m.createReleaseFn(ctx, repo, name, sequence, body, createdBy)
	}
	return nil, errors.New("not implemented")
}

func (m *mockStore) GetRelease(ctx context.Context, repo, name string) (*model.Release, error) {
	if m.getReleaseFn != nil {
		return m.getReleaseFn(ctx, repo, name)
	}
	return nil, db.ErrReleaseNotFound
}

func (m *mockStore) ListReleases(ctx context.Context, repo string, limit int, afterID string) ([]model.Release, error) {
	if m.listReleasesFn != nil {
		return m.listReleasesFn(ctx, repo, limit, afterID)
	}
	return []model.Release{}, nil
}

func (m *mockStore) DeleteRelease(ctx context.Context, repo, name string) error {
	if m.deleteReleaseFn != nil {
		return m.deleteReleaseFn(ctx, repo, name)
	}
	return db.ErrReleaseNotFound
}

func (m *mockStore) CommitSequenceExists(ctx context.Context, repo string, sequence int64) (bool, error) {
	if m.commitSequenceExistsFn != nil {
		return m.commitSequenceExistsFn(ctx, repo, sequence)
	}
	return true, nil
}

func (m *mockStore) CreateSubscription(ctx context.Context, req model.CreateSubscriptionRequest) (*model.EventSubscription, error) {
	if m.createSubscriptionFn != nil {
		return m.createSubscriptionFn(ctx, req)
	}
	return &model.EventSubscription{ID: "test-sub-id", Backend: req.Backend}, nil
}

func (m *mockStore) GetSubscription(ctx context.Context, id string) (*model.EventSubscription, error) {
	if m.getSubscriptionFn != nil {
		return m.getSubscriptionFn(ctx, id)
	}
	return nil, db.ErrSubscriptionNotFound
}

func (m *mockStore) ListSubscriptions(ctx context.Context) ([]model.EventSubscription, error) {
	if m.listSubscriptionsFn != nil {
		return m.listSubscriptionsFn(ctx)
	}
	return []model.EventSubscription{}, nil
}

func (m *mockStore) ListSubscriptionsByCreator(ctx context.Context, createdBy string) ([]model.EventSubscription, error) {
	if m.listSubscriptionsByCreatorFn != nil {
		return m.listSubscriptionsByCreatorFn(ctx, createdBy)
	}
	return []model.EventSubscription{}, nil
}

func (m *mockStore) DeleteSubscription(ctx context.Context, id string) error {
	if m.deleteSubscriptionFn != nil {
		return m.deleteSubscriptionFn(ctx, id)
	}
	return nil
}

func (m *mockStore) ResumeSubscription(ctx context.Context, id string) error {
	if m.resumeSubscriptionFn != nil {
		return m.resumeSubscriptionFn(ctx, id)
	}
	return nil
}

func (m *mockStore) CreateProposal(_ context.Context, _, _, _, _, _, _ string) (*model.Proposal, error) {
	return nil, errors.New("not implemented")
}

func (m *mockStore) GetProposal(_ context.Context, _, _ string) (*model.Proposal, error) {
	return nil, db.ErrProposalNotFound
}

func (m *mockStore) ListProposals(_ context.Context, _ string, _ *model.ProposalState, _ *string) ([]*model.Proposal, error) {
	return []*model.Proposal{}, nil
}

func (m *mockStore) UpdateProposal(_ context.Context, _, _ string, _, _ *string) (*model.Proposal, error) {
	return nil, db.ErrProposalNotFound
}

func (m *mockStore) CloseProposal(_ context.Context, _, _ string) error {
	return db.ErrProposalNotFound
}

func (m *mockStore) MergeProposal(_ context.Context, _, _ string) (*model.Proposal, error) {
	return nil, nil
}

func (m *mockStore) CreateReviewComment(ctx context.Context, repo, branch, path, versionID, body, author string, reviewID *string) (*model.ReviewComment, error) {
	if m.createReviewCommentFn != nil {
		return m.createReviewCommentFn(ctx, repo, branch, path, versionID, body, author, reviewID)
	}
	return nil, errors.New("not implemented")
}

func (m *mockStore) ListReviewComments(ctx context.Context, repo, branch string, path *string) ([]model.ReviewComment, error) {
	if m.listReviewCommentsFn != nil {
		return m.listReviewCommentsFn(ctx, repo, branch, path)
	}
	return []model.ReviewComment{}, nil
}

func (m *mockStore) GetReviewComment(ctx context.Context, repo, id string) (*model.ReviewComment, error) {
	if m.getReviewCommentFn != nil {
		return m.getReviewCommentFn(ctx, repo, id)
	}
	return nil, db.ErrCommentNotFound
}

func (m *mockStore) DeleteReviewComment(ctx context.Context, repo, id string) error {
	if m.deleteReviewCommentFn != nil {
		return m.deleteReviewCommentFn(ctx, repo, id)
	}
	return db.ErrCommentNotFound
}

func (m *mockStore) CreateIssue(_ context.Context, _, _, _, _ string, _ []string) (*model.Issue, error) {
	return nil, errors.New("not implemented")
}
func (m *mockStore) GetIssue(_ context.Context, _ string, _ int64) (*model.Issue, error) {
	return nil, db.ErrIssueNotFound
}
func (m *mockStore) ListIssues(_ context.Context, _, _, _ string) ([]model.Issue, error) {
	return []model.Issue{}, nil
}
func (m *mockStore) UpdateIssue(_ context.Context, _ string, _ int64, _, _ *string) (*model.Issue, error) {
	return nil, db.ErrIssueNotFound
}
func (m *mockStore) CloseIssue(_ context.Context, _ string, _ int64, _ model.IssueCloseReason, _ string) (*model.Issue, error) {
	return nil, db.ErrIssueNotFound
}
func (m *mockStore) ReopenIssue(_ context.Context, _ string, _ int64) (*model.Issue, error) {
	return nil, db.ErrIssueNotFound
}
func (m *mockStore) CreateIssueComment(_ context.Context, _ string, _ int64, _, _ string) (*model.IssueComment, error) {
	return nil, errors.New("not implemented")
}
func (m *mockStore) GetIssueComment(_ context.Context, _, _ string) (*model.IssueComment, error) {
	return nil, db.ErrIssueCommentNotFound
}
func (m *mockStore) ListIssueComments(_ context.Context, _ string, _ int64) ([]model.IssueComment, error) {
	return []model.IssueComment{}, nil
}
func (m *mockStore) UpdateIssueComment(_ context.Context, _, _, _ string) (*model.IssueComment, error) {
	return nil, db.ErrIssueCommentNotFound
}
func (m *mockStore) DeleteIssueComment(_ context.Context, _, _ string) error {
	return db.ErrIssueCommentNotFound
}
func (m *mockStore) CreateIssueRef(_ context.Context, _ string, _ int64, _ model.IssueRefType, _ string) (*model.IssueRef, error) {
	return nil, errors.New("not implemented")
}
func (m *mockStore) ListIssueRefs(_ context.Context, _ string, _ int64) ([]model.IssueRef, error) {
	return []model.IssueRef{}, nil
}
func (m *mockStore) ListIssuesByRef(_ context.Context, _ string, _ model.IssueRefType, _ string) ([]model.Issue, error) {
	return []model.Issue{}, nil
}

func TestHealthEndpoint(t *testing.T) {
	// /healthz is exempt from IAP auth, so devIdentity="" is fine here.
	srv := New(nil, nil, "", "")

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("expected status ok, got %q", body["status"])
	}
}

// TestBranchStatusNoReadStore verifies that GET /:name/branch/:b/status returns
// 503 Service Unavailable when the server was started without a read store.
func TestBranchStatusNoReadStore(t *testing.T) {
	srv := New(nil, nil, devID, "")

	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/branch/main/status", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestHandleCommit_Success(t *testing.T) {
	vid := "abc-123"
	store := &mockStore{
		commitFn: func(ctx context.Context, req model.CommitRequest) (*model.CommitResponse, error) {
			if req.Branch != "main" {
				t.Errorf("expected branch main, got %q", req.Branch)
			}
			if len(req.Files) != 1 {
				t.Errorf("expected 1 file, got %d", len(req.Files))
			}
			if req.Author != devID {
				t.Errorf("expected author %q from context, got %q", devID, req.Author)
			}
			return &model.CommitResponse{
				Sequence: 1,
				Files:    []model.CommitFileResult{{Path: "hello.txt", VersionID: &vid}},
			}, nil
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.CommitRequest{
		Branch:  "main",
		Files:   []model.FileChange{{Path: "hello.txt", Content: []byte("hello")}},
		Message: "initial commit",
	})

	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/commit", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp model.CommitResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if resp.Sequence != 1 {
		t.Errorf("expected sequence 1, got %d", resp.Sequence)
	}
	if len(resp.Files) != 1 || resp.Files[0].Path != "hello.txt" {
		t.Errorf("unexpected files: %+v", resp.Files)
	}
}

func TestHandleCommit_ValidationErrors(t *testing.T) {
	srv := New(&mockStore{
		commitFn: func(ctx context.Context, req model.CommitRequest) (*model.CommitResponse, error) {
			t.Fatal("store should not be called on validation error")
			return nil, nil
		},
	}, nil, devID, devID)

	tests := []struct {
		name string
		body model.CommitRequest
	}{
		{"missing branch", model.CommitRequest{Files: []model.FileChange{{Path: "a.txt", Content: []byte("x")}}, Message: "m"}},
		{"missing files", model.CommitRequest{Branch: "main", Message: "m"}},
		{"missing message", model.CommitRequest{Branch: "main", Files: []model.FileChange{{Path: "a.txt", Content: []byte("x")}}}},
		{"empty file path", model.CommitRequest{Branch: "main", Files: []model.FileChange{{Path: "", Content: []byte("x")}}, Message: "m"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _ := json.Marshal(tt.body)
			req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/commit", bytes.NewReader(b))
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHandleCommit_BranchNotFound(t *testing.T) {
	store := &mockStore{
		commitFn: func(ctx context.Context, req model.CommitRequest) (*model.CommitResponse, error) {
			return nil, db.ErrBranchNotFound
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.CommitRequest{
		Branch:  "nonexistent",
		Files:   []model.FileChange{{Path: "a.txt", Content: []byte("x")}},
		Message: "m",
	})

	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/commit", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestHandleCommit_BranchNotActive(t *testing.T) {
	store := &mockStore{
		commitFn: func(ctx context.Context, req model.CommitRequest) (*model.CommitResponse, error) {
			return nil, db.ErrBranchNotActive
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.CommitRequest{
		Branch:  "merged-branch",
		Files:   []model.FileChange{{Path: "a.txt", Content: []byte("x")}},
		Message: "m",
	})

	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/commit", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rec.Code)
	}
}

func TestHandleCommit_InternalError(t *testing.T) {
	store := &mockStore{
		commitFn: func(ctx context.Context, req model.CommitRequest) (*model.CommitResponse, error) {
			return nil, errors.New("something went wrong")
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.CommitRequest{
		Branch:  "main",
		Files:   []model.FileChange{{Path: "a.txt", Content: []byte("x")}},
		Message: "m",
	})

	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/commit", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
}

func TestHandleCommit_InvalidJSON(t *testing.T) {
	srv := New(&mockStore{
		commitFn: func(ctx context.Context, req model.CommitRequest) (*model.CommitResponse, error) {
			t.Fatal("store should not be called on invalid JSON")
			return nil, nil
		},
	}, nil, devID, devID)

	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/commit", bytes.NewReader([]byte("not json")))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

// --- POST /repos/default/branch tests ---

func TestHandleCreateBranch_Success(t *testing.T) {
	store := &mockStore{
		createBranchFn: func(ctx context.Context, req model.CreateBranchRequest) (*model.CreateBranchResponse, error) {
			return &model.CreateBranchResponse{Name: req.Name, BaseSequence: 5}, nil
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.CreateBranchRequest{Name: "feature/test"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/branch", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp model.CreateBranchResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Name != "feature/test" {
		t.Errorf("expected name feature/test, got %q", resp.Name)
	}
	if resp.BaseSequence != 5 {
		t.Errorf("expected base_sequence 5, got %d", resp.BaseSequence)
	}
}

func TestHandleCreateBranch_MissingName(t *testing.T) {
	srv := New(&mockStore{}, nil, devID, devID)

	body, _ := json.Marshal(model.CreateBranchRequest{Name: ""})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/branch", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandleCreateBranch_CannotCreateMain(t *testing.T) {
	srv := New(&mockStore{}, nil, devID, devID)

	body, _ := json.Marshal(model.CreateBranchRequest{Name: "main"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/branch", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandleCreateBranch_AlreadyExists(t *testing.T) {
	store := &mockStore{
		createBranchFn: func(ctx context.Context, req model.CreateBranchRequest) (*model.CreateBranchResponse, error) {
			return nil, db.ErrBranchExists
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.CreateBranchRequest{Name: "feature/exists"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/branch", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rec.Code)
	}
}

func TestHandleCreateBranch_RepoNotFound(t *testing.T) {
	store := &mockStore{
		createBranchFn: func(ctx context.Context, req model.CreateBranchRequest) (*model.CreateBranchResponse, error) {
			return nil, db.ErrRepoNotFound
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.CreateBranchRequest{Name: "feature/x"})
	req := httptest.NewRequest(http.MethodPost, "/repos/nonexistent/nonexistent/-/branch", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// --- validateRepo / nonexistent repo tests ---

func TestHandleTree_RepoNotFound(t *testing.T) {
	store := &mockStore{
		getRepoFn: func(ctx context.Context, name string) (*model.Repo, error) {
			return nil, db.ErrRepoNotFound
		},
	}
	srv := New(store, nil, devID, devID)

	req := httptest.NewRequest(http.MethodGet, "/repos/ghost/ghost/-/tree?branch=main", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestHandleBranches_RepoNotFound(t *testing.T) {
	store := &mockStore{
		getRepoFn: func(ctx context.Context, name string) (*model.Repo, error) {
			return nil, db.ErrRepoNotFound
		},
	}
	srv := New(store, nil, devID, devID)

	req := httptest.NewRequest(http.MethodGet, "/repos/ghost/ghost/-/branches", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestHandleDiff_RepoNotFound(t *testing.T) {
	store := &mockStore{
		getRepoFn: func(ctx context.Context, name string) (*model.Repo, error) {
			return nil, db.ErrRepoNotFound
		},
	}
	srv := New(store, nil, devID, devID)

	req := httptest.NewRequest(http.MethodGet, "/repos/ghost/ghost/-/diff?branch=main", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestHandleFile_RepoNotFound(t *testing.T) {
	store := &mockStore{
		getRepoFn: func(ctx context.Context, name string) (*model.Repo, error) {
			return nil, db.ErrRepoNotFound
		},
	}
	srv := New(store, nil, devID, devID)

	req := httptest.NewRequest(http.MethodGet, "/repos/ghost/ghost/-/file/foo.txt", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// --- POST /repos/default/merge tests ---

func TestHandleMerge_Success(t *testing.T) {
	store := &mockStore{
		mergeFn: func(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error) {
			return &model.MergeResponse{Sequence: 10}, nil, nil
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.MergeRequest{Branch: "feature/test"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/merge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp model.MergeResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Sequence != 10 {
		t.Errorf("expected sequence 10, got %d", resp.Sequence)
	}
}

func TestHandleMerge_MissingBranch(t *testing.T) {
	srv := New(&mockStore{}, nil, devID, devID)

	body, _ := json.Marshal(model.MergeRequest{Branch: ""})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/merge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandleMerge_Conflict(t *testing.T) {
	store := &mockStore{
		mergeFn: func(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error) {
			return nil, []db.MergeConflict{
				{Path: "conflict.txt", MainVersionID: "v1", BranchVersionID: "v2"},
			}, db.ErrMergeConflict
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.MergeRequest{Branch: "feature/conflict"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/merge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var errResp model.MergeConflictError
	if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(errResp.Conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(errResp.Conflicts))
	}
	if errResp.Conflicts[0].Path != "conflict.txt" {
		t.Errorf("expected conflict.txt, got %q", errResp.Conflicts[0].Path)
	}
	if errResp.Conflicts[0].MainVersionID != "v1" {
		t.Errorf("expected main_version_id v1, got %q", errResp.Conflicts[0].MainVersionID)
	}
	if errResp.Conflicts[0].BranchVersionID != "v2" {
		t.Errorf("expected branch_version_id v2, got %q", errResp.Conflicts[0].BranchVersionID)
	}
}

func TestHandleMerge_BranchNotFound(t *testing.T) {
	store := &mockStore{
		mergeFn: func(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error) {
			return nil, nil, db.ErrBranchNotFound
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.MergeRequest{Branch: "nonexistent"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/merge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestHandleMerge_BranchNotActive(t *testing.T) {
	store := &mockStore{
		mergeFn: func(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error) {
			return nil, nil, db.ErrBranchNotActive
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.MergeRequest{Branch: "merged-branch"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/merge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleMerge_DraftBranch(t *testing.T) {
	store := &mockStore{
		mergeFn: func(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error) {
			return nil, nil, db.ErrBranchDraft
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.MergeRequest{Branch: "draft/wip"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/merge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var errResp model.ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if errResp.Error != "branch is in draft state" {
		t.Errorf("expected 'branch is in draft state' error, got %q", errResp.Error)
	}
}

func TestHandleCreateBranch_DraftFlag(t *testing.T) {
	var gotDraft bool
	store := &mockStore{
		createBranchFn: func(ctx context.Context, req model.CreateBranchRequest) (*model.CreateBranchResponse, error) {
			gotDraft = req.Draft
			return &model.CreateBranchResponse{Name: req.Name, BaseSequence: 5, Draft: req.Draft}, nil
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.CreateBranchRequest{Name: "draft/wip", Draft: true})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/branch", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if !gotDraft {
		t.Error("expected draft=true to be passed to store")
	}
}

func TestHandleUpdateBranch_MarkReady(t *testing.T) {
	var gotRepo, gotName string
	var gotDraft bool
	store := &mockStore{
		updateBranchDraftFn: func(ctx context.Context, repo, name string, draft bool) error {
			gotRepo = repo
			gotName = name
			gotDraft = draft
			return nil
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.UpdateBranchRequest{Draft: false})
	req := httptest.NewRequest(http.MethodPatch, "/repos/default/default/-/branch/draft/wip", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if gotRepo != "default/default" {
		t.Errorf("expected repo default/default, got %q", gotRepo)
	}
	if gotName != "draft/wip" {
		t.Errorf("expected branch draft/wip, got %q", gotName)
	}
	if gotDraft != false {
		t.Errorf("expected draft=false, got true")
	}
}

func TestHandleUpdateBranch_NotFound(t *testing.T) {
	store := &mockStore{
		updateBranchDraftFn: func(ctx context.Context, repo, name string, draft bool) error {
			return db.ErrBranchNotFound
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.UpdateBranchRequest{Draft: false})
	req := httptest.NewRequest(http.MethodPatch, "/repos/default/default/-/branch/nonexistent", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleUpdateBranch_RepoNotFound(t *testing.T) {
	store := &mockStore{
		getRepoFn: func(ctx context.Context, name string) (*model.Repo, error) {
			return nil, db.ErrRepoNotFound
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.UpdateBranchRequest{Draft: false})
	req := httptest.NewRequest(http.MethodPatch, "/repos/default/default/-/branch/feature/x", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleUpdateBranch_CannotPatchMain(t *testing.T) {
	srv := New(&mockStore{}, nil, devID, devID)

	body, _ := json.Marshal(model.UpdateBranchRequest{Draft: false})
	req := httptest.NewRequest(http.MethodPatch, "/repos/default/default/-/branch/main", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleUpdateBranch_ReaderForbidden(t *testing.T) {
	store := &mockStore{
		getRoleFn: func(ctx context.Context, repo, identity string) (*model.Role, error) {
			return &model.Role{Identity: identity, Role: model.RoleReader}, nil
		},
	}
	srv := New(store, nil, "reader@example.com", "")

	body, _ := json.Marshal(model.UpdateBranchRequest{Draft: false})
	req := httptest.NewRequest(http.MethodPatch, "/repos/default/default/-/branch/feature/x", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleBranches_DraftFilter(t *testing.T) {
	var gotIncludeDraft, gotOnlyDraft bool
	rs := &mockReadStore{
		getBranchFn: func(ctx context.Context, repo, branch string) (*store.BranchInfo, error) {
			return &store.BranchInfo{Name: "main", Status: "active"}, nil
		},
		listBranchesFn: func(ctx context.Context, repo, statusFilter string, includeDraft, onlyDraft bool) ([]store.BranchInfo, error) {
			gotIncludeDraft = includeDraft
			gotOnlyDraft = onlyDraft
			return []store.BranchInfo{{Name: "draft/wip", Status: "active", Draft: true}}, nil
		},
	}
	srv := newTestHandler(&mockStore{}, rs, nil)

	// Test ?draft=true
	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/branches?draft=true", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if !gotOnlyDraft {
		t.Error("expected onlyDraft=true to be passed to store")
	}

	// Test ?include_draft=true
	gotIncludeDraft = false
	gotOnlyDraft = false
	req = httptest.NewRequest(http.MethodGet, "/repos/default/default/-/branches?include_draft=true", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if !gotIncludeDraft {
		t.Error("expected includeDraft=true to be passed to store")
	}
	if gotOnlyDraft {
		t.Error("expected onlyDraft=false when only include_draft is set")
	}
}

func TestHandleMerge_CannotMergeMain(t *testing.T) {
	srv := New(&mockStore{}, nil, devID, devID)

	body, _ := json.Marshal(model.MergeRequest{Branch: "main"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/merge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleMerge_AuthorFromIdentity verifies that the authenticated identity
// is always used as the merge author, regardless of what is in the request body.
func TestHandleMerge_AuthorFromIdentity(t *testing.T) {
	const identity = "alice@example.com"
	var capturedAuthor string
	store := &mockStore{
		mergeFn: func(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error) {
			capturedAuthor = req.Author
			return &model.MergeResponse{Sequence: 1}, nil, nil
		},
	}
	srv := New(store, nil, identity, identity)

	// Send a different author in the body — it must be overridden by the identity.
	body, _ := json.Marshal(model.MergeRequest{Branch: "feature/test", Author: "ignored@example.com"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/merge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if capturedAuthor != identity {
		t.Errorf("expected author %q from identity, got %q", identity, capturedAuthor)
	}
}

// --- DELETE /repos/default/branch/:name tests ---

func TestHandleDeleteBranch_Success(t *testing.T) {
	store := &mockStore{
		deleteBranchFn: func(ctx context.Context, repo, name string) error {
			if name != "feature/delete-me" {
				t.Errorf("expected name feature/delete-me, got %q", name)
			}
			return nil
		},
	}
	srv := New(store, nil, devID, devID)

	req := httptest.NewRequest(http.MethodDelete, "/repos/default/default/-/branch/feature/delete-me", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleDeleteBranch_CannotDeleteMain(t *testing.T) {
	srv := New(&mockStore{}, nil, devID, devID)

	req := httptest.NewRequest(http.MethodDelete, "/repos/default/default/-/branch/main", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandleDeleteBranch_NotFound(t *testing.T) {
	store := &mockStore{
		deleteBranchFn: func(ctx context.Context, repo, name string) error {
			return db.ErrBranchNotFound
		},
	}
	srv := New(store, nil, devID, devID)

	req := httptest.NewRequest(http.MethodDelete, "/repos/default/default/-/branch/nonexistent", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestHandleDeleteBranch_AlreadyMerged(t *testing.T) {
	store := &mockStore{
		deleteBranchFn: func(ctx context.Context, repo, name string) error {
			return db.ErrBranchNotActive
		},
	}
	srv := New(store, nil, devID, devID)

	req := httptest.NewRequest(http.MethodDelete, "/repos/default/default/-/branch/merged-branch", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rec.Code)
	}
}

func TestHandleDeleteBranch_AlreadyAbandoned(t *testing.T) {
	store := &mockStore{
		deleteBranchFn: func(ctx context.Context, repo, name string) error {
			return db.ErrBranchNotActive
		},
	}
	srv := New(store, nil, devID, devID)

	req := httptest.NewRequest(http.MethodDelete, "/repos/default/default/-/branch/abandoned-branch", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rec.Code)
	}
}

// --- POST /repos/default/rebase tests ---

func TestHandleRebase_Success(t *testing.T) {
	store := &mockStore{
		rebaseFn: func(ctx context.Context, req model.RebaseRequest) (*model.RebaseResponse, []db.MergeConflict, error) {
			if req.Branch != "feature/test" {
				t.Errorf("expected branch feature/test, got %q", req.Branch)
			}
			return &model.RebaseResponse{
				NewBaseSequence: 5,
				NewHeadSequence: 7,
				CommitsReplayed: 2,
			}, nil, nil
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.RebaseRequest{Branch: "feature/test"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/rebase", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp model.RebaseResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.NewBaseSequence != 5 {
		t.Errorf("expected base_sequence 5, got %d", resp.NewBaseSequence)
	}
	if resp.NewHeadSequence != 7 {
		t.Errorf("expected head_sequence 7, got %d", resp.NewHeadSequence)
	}
	if resp.CommitsReplayed != 2 {
		t.Errorf("expected commits_replayed 2, got %d", resp.CommitsReplayed)
	}
}

func TestHandleRebase_Conflict(t *testing.T) {
	store := &mockStore{
		rebaseFn: func(ctx context.Context, req model.RebaseRequest) (*model.RebaseResponse, []db.MergeConflict, error) {
			return nil, []db.MergeConflict{
				{Path: "conflict.txt", MainVersionID: "v1", BranchVersionID: "v2"},
			}, db.ErrRebaseConflict
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.RebaseRequest{Branch: "feature/conflict"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/rebase", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var errResp model.RebaseConflictError
	if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(errResp.Conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(errResp.Conflicts))
	}
	if errResp.Conflicts[0].Path != "conflict.txt" {
		t.Errorf("expected conflict.txt, got %q", errResp.Conflicts[0].Path)
	}
	if errResp.Conflicts[0].MainVersionID != "v1" {
		t.Errorf("expected main_version_id v1, got %q", errResp.Conflicts[0].MainVersionID)
	}
	if errResp.Conflicts[0].BranchVersionID != "v2" {
		t.Errorf("expected branch_version_id v2, got %q", errResp.Conflicts[0].BranchVersionID)
	}
}

func TestHandleRebase_BranchNotFound(t *testing.T) {
	store := &mockStore{
		rebaseFn: func(ctx context.Context, req model.RebaseRequest) (*model.RebaseResponse, []db.MergeConflict, error) {
			return nil, nil, db.ErrBranchNotFound
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.RebaseRequest{Branch: "nonexistent"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/rebase", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestHandleRebase_CannotRebaseMain(t *testing.T) {
	srv := New(&mockStore{}, nil, devID, devID)

	body, _ := json.Marshal(model.RebaseRequest{Branch: "main"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/rebase", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleRebase_BranchNotActive(t *testing.T) {
	store := &mockStore{
		rebaseFn: func(ctx context.Context, req model.RebaseRequest) (*model.RebaseResponse, []db.MergeConflict, error) {
			return nil, nil, db.ErrBranchNotActive
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.RebaseRequest{Branch: "merged-branch"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/rebase", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandleCommit_RepoNotFound(t *testing.T) {
	store := &mockStore{
		getRepoFn: func(ctx context.Context, name string) (*model.Repo, error) {
			return nil, db.ErrRepoNotFound
		},
		commitFn: func(ctx context.Context, req model.CommitRequest) (*model.CommitResponse, error) {
			t.Fatal("store.Commit should not be called when repo does not exist")
			return nil, nil
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.CommitRequest{
		Branch:  "main",
		Files:   []model.FileChange{{Path: "a.txt", Content: []byte("x")}},
		Message: "m",
	})
	req := httptest.NewRequest(http.MethodPost, "/repos/ghost/ghost/-/commit", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleMerge_RepoNotFound(t *testing.T) {
	store := &mockStore{
		getRepoFn: func(ctx context.Context, name string) (*model.Repo, error) {
			return nil, db.ErrRepoNotFound
		},
		mergeFn: func(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error) {
			t.Fatal("store.Merge should not be called when repo does not exist")
			return nil, nil, nil
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.MergeRequest{Branch: "feature/test"})
	req := httptest.NewRequest(http.MethodPost, "/repos/ghost/ghost/-/merge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleRebase_RepoNotFound(t *testing.T) {
	store := &mockStore{
		getRepoFn: func(ctx context.Context, name string) (*model.Repo, error) {
			return nil, db.ErrRepoNotFound
		},
		rebaseFn: func(ctx context.Context, req model.RebaseRequest) (*model.RebaseResponse, []db.MergeConflict, error) {
			t.Fatal("store.Rebase should not be called when repo does not exist")
			return nil, nil, nil
		},
	}
	srv := New(store, nil, devID, devID)

	body, _ := json.Marshal(model.RebaseRequest{Branch: "feature/test"})
	req := httptest.NewRequest(http.MethodPost, "/repos/ghost/ghost/-/rebase", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Review handler tests
// ---------------------------------------------------------------------------

func TestHandleReview_Success(t *testing.T) {
	const identity = "reviewer@example.com"
	store := &mockStore{
		createReviewFn: func(ctx context.Context, repo, branch, reviewer string, status model.ReviewStatus, body string) (*model.Review, error) {
			if repo != "default/default" {
				t.Errorf("expected repo default, got %q", repo)
			}
			if branch != "feature/x" {
				t.Errorf("expected branch feature/x, got %q", branch)
			}
			if reviewer != identity {
				t.Errorf("expected reviewer %q from context, got %q", identity, reviewer)
			}
			if status != model.ReviewApproved {
				t.Errorf("expected status approved, got %q", status)
			}
			if body != "LGTM" {
				t.Errorf("expected body LGTM, got %q", body)
			}
			return &model.Review{ID: "review-uuid", Sequence: 5}, nil
		},
	}
	srv := New(store, nil, identity, identity)

	b, _ := json.Marshal(model.CreateReviewRequest{Branch: "feature/x", Status: model.ReviewApproved, Body: "LGTM"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/review", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp model.CreateReviewResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != "review-uuid" {
		t.Errorf("expected id review-uuid, got %q", resp.ID)
	}
	if resp.Sequence != 5 {
		t.Errorf("expected sequence 5, got %d", resp.Sequence)
	}
}

func TestHandleReview_SelfApproval(t *testing.T) {
	store := &mockStore{
		createReviewFn: func(ctx context.Context, repo, branch, reviewer string, status model.ReviewStatus, body string) (*model.Review, error) {
			return nil, db.ErrSelfApproval
		},
	}
	srv := New(store, nil, devID, devID)

	b, _ := json.Marshal(model.CreateReviewRequest{Branch: "feature/x", Status: model.ReviewApproved})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/review", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestHandleReview_BranchNotFound(t *testing.T) {
	store := &mockStore{
		createReviewFn: func(ctx context.Context, repo, branch, reviewer string, status model.ReviewStatus, body string) (*model.Review, error) {
			return nil, db.ErrBranchNotFound
		},
	}
	srv := New(store, nil, devID, devID)

	b, _ := json.Marshal(model.CreateReviewRequest{Branch: "nonexistent", Status: model.ReviewApproved})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/review", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Check-run handler tests
// ---------------------------------------------------------------------------

func TestHandleCheck_Success(t *testing.T) {
	const identity = "ci-bot@example.com"
	store := &mockStore{
		createCheckRunFn: func(ctx context.Context, repo, branch, checkName string, status model.CheckRunStatus, reporter string, logURL *string, atSequence *int64) (*model.CheckRun, error) {
			if repo != "default/default" {
				t.Errorf("expected repo default, got %q", repo)
			}
			if branch != "feature/x" {
				t.Errorf("expected branch feature/x, got %q", branch)
			}
			if checkName != "ci/build" {
				t.Errorf("expected check_name ci/build, got %q", checkName)
			}
			if status != model.CheckRunPassed {
				t.Errorf("expected status passed, got %q", status)
			}
			if reporter != identity {
				t.Errorf("expected reporter %q from context, got %q", identity, reporter)
			}
			return &model.CheckRun{ID: "check-uuid", Sequence: 3}, nil
		},
	}
	srv := New(store, nil, identity, identity)

	b, _ := json.Marshal(model.CreateCheckRunRequest{Branch: "feature/x", CheckName: "ci/build", Status: model.CheckRunPassed})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/check", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp model.CreateCheckRunResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != "check-uuid" {
		t.Errorf("expected id check-uuid, got %q", resp.ID)
	}
	if resp.Sequence != 3 {
		t.Errorf("expected sequence 3, got %d", resp.Sequence)
	}
}

func TestHandleCheck_ExplicitSequence(t *testing.T) {
	const identity = "ci-bot@example.com"
	const wantSeq int64 = 15
	var gotSeq *int64
	store := &mockStore{
		createCheckRunFn: func(ctx context.Context, repo, branch, checkName string, status model.CheckRunStatus, reporter string, logURL *string, atSequence *int64) (*model.CheckRun, error) {
			gotSeq = atSequence
			seq := int64(0)
			if atSequence != nil {
				seq = *atSequence
			}
			return &model.CheckRun{ID: "chk", Sequence: seq}, nil
		},
	}
	srv := New(store, nil, identity, identity)

	seq := wantSeq
	b, _ := json.Marshal(model.CreateCheckRunRequest{Branch: "main", CheckName: "ci/build", Status: model.CheckRunPassed, Sequence: &seq})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/check", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if gotSeq == nil {
		t.Fatal("expected atSequence to be forwarded, got nil")
	}
	if *gotSeq != wantSeq {
		t.Errorf("expected atSequence=%d, got %d", wantSeq, *gotSeq)
	}
}

func TestHandleCheck_BranchNotFound(t *testing.T) {
	store := &mockStore{
		createCheckRunFn: func(ctx context.Context, repo, branch, checkName string, status model.CheckRunStatus, reporter string, logURL *string, atSequence *int64) (*model.CheckRun, error) {
			return nil, db.ErrBranchNotFound
		},
	}
	srv := New(store, nil, devID, devID)

	b, _ := json.Marshal(model.CreateCheckRunRequest{Branch: "nonexistent", CheckName: "ci/build", Status: model.CheckRunPassed})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/check", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// GET reviews/checks handler tests
// ---------------------------------------------------------------------------

func TestHandleGetReviews(t *testing.T) {
	store := &mockStore{
		listReviewsFn: func(ctx context.Context, repo, branch string, atSeq *int64) ([]model.Review, error) {
			if repo != "default/default" {
				t.Errorf("expected repo default, got %q", repo)
			}
			if branch != "feature/x" {
				t.Errorf("expected branch feature/x, got %q", branch)
			}
			return []model.Review{
				{ID: "r1", Branch: "feature/x", Reviewer: "alice", Sequence: 2, Status: model.ReviewApproved},
			}, nil
		},
	}
	srv := New(store, nil, devID, devID)

	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/branch/feature/x/reviews", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var reviews []model.Review
	if err := json.NewDecoder(rec.Body).Decode(&reviews); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(reviews) != 1 {
		t.Fatalf("expected 1 review, got %d", len(reviews))
	}
	if reviews[0].ID != "r1" {
		t.Errorf("expected id r1, got %q", reviews[0].ID)
	}
}

func TestHandleGetChecks(t *testing.T) {
	store := &mockStore{
		listCheckRunsFn: func(ctx context.Context, repo, branch string, atSeq *int64) ([]model.CheckRun, error) {
			if repo != "default/default" {
				t.Errorf("expected repo default, got %q", repo)
			}
			if branch != "feature/x" {
				t.Errorf("expected branch feature/x, got %q", branch)
			}
			return []model.CheckRun{
				{ID: "cr1", Branch: "feature/x", CheckName: "ci/build", Sequence: 2, Status: model.CheckRunPassed, Reporter: "ci-bot"},
			}, nil
		},
	}
	srv := New(store, nil, devID, devID)

	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/branch/feature/x/checks", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var checkRuns []model.CheckRun
	if err := json.NewDecoder(rec.Body).Decode(&checkRuns); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(checkRuns) != 1 {
		t.Fatalf("expected 1 check run, got %d", len(checkRuns))
	}
	if checkRuns[0].ID != "cr1" {
		t.Errorf("expected id cr1, got %q", checkRuns[0].ID)
	}
}

func TestHandlePurge_Success(t *testing.T) {
	ms := &mockStore{
		purgeFn: func(ctx context.Context, req db.PurgeRequest) (*db.PurgeResult, error) {
			if req.Repo != "default/default" {
				t.Errorf("expected repo 'default', got %q", req.Repo)
			}
			if req.OlderThan != 30*24*time.Hour {
				t.Errorf("expected 30d duration, got %v", req.OlderThan)
			}
			if req.DryRun {
				t.Error("expected dry_run=false")
			}
			return &db.PurgeResult{
				BranchesPurged:     3,
				FileCommitsDeleted: 12,
				CommitsDeleted:     4,
				DocumentsDeleted:   2,
				ReviewsDeleted:     1,
				CheckRunsDeleted:   1,
			}, nil
		},
	}
	srv := New(ms, nil, devID, devID)

	body, _ := json.Marshal(map[string]interface{}{"older_than": "30d"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/purge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp model.PurgeResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.BranchesPurged != 3 {
		t.Errorf("expected 3 branches_purged, got %d", resp.BranchesPurged)
	}
	if resp.FileCommitsDeleted != 12 {
		t.Errorf("expected 12 file_commits_deleted, got %d", resp.FileCommitsDeleted)
	}
}

func TestHandlePurge_DryRun(t *testing.T) {
	ms := &mockStore{
		purgeFn: func(ctx context.Context, req db.PurgeRequest) (*db.PurgeResult, error) {
			if !req.DryRun {
				t.Error("expected dry_run=true")
			}
			return &db.PurgeResult{BranchesPurged: 2, CommitsDeleted: 5}, nil
		},
	}
	srv := New(ms, nil, devID, devID)

	body, _ := json.Marshal(map[string]interface{}{"older_than": "7d", "dry_run": true})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/purge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp model.PurgeResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.BranchesPurged != 2 {
		t.Errorf("expected 2 branches_purged, got %d", resp.BranchesPurged)
	}
}

func TestHandlePurge_InvalidDuration(t *testing.T) {
	srv := New(&mockStore{}, nil, devID, devID)

	tests := []struct {
		name    string
		payload string
	}{
		{"missing older_than", `{}`},
		{"not a day format", `{"older_than":"90h"}`},
		{"zero days", `{"older_than":"0d"}`},
		{"negative days", `{"older_than":"-1d"}`},
		{"non-numeric", `{"older_than":"abcd"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/purge", bytes.NewReader([]byte(tt.payload)))
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHandlePurge_RepoNotFound(t *testing.T) {
	ms := &mockStore{
		purgeFn: func(ctx context.Context, req db.PurgeRequest) (*db.PurgeResult, error) {
			return nil, db.ErrRepoNotFound
		},
	}
	srv := New(ms, nil, devID, devID)

	body, _ := json.Marshal(map[string]interface{}{"older_than": "30d"})
	req := httptest.NewRequest(http.MethodPost, "/repos/noexist/noexist/-/purge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestParseDayDuration(t *testing.T) {
	tests := []struct {
		input   string
		want    time.Duration
		wantErr bool
	}{
		{"1d", 24 * time.Hour, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"30d", 30 * 24 * time.Hour, false},
		{"365d", 365 * 24 * time.Hour, false},
		// invalid inputs
		{"0d", 0, true},
		{"-1d", 0, true},
		{"7h", 0, true},
		{"7", 0, true},
		{"d", 0, true},
		{"", 0, true},
		{"abcd", 0, true},
		{"1.5d", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseDayDuration(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseDayDuration(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("parseDayDuration(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Org handler tests
// ---------------------------------------------------------------------------

func TestHandleDeleteOrg_OrgHasRepos(t *testing.T) {
	ms := &mockStore{
		deleteOrgFn: func(ctx context.Context, name string) error {
			return db.ErrOrgHasRepos
		},
	}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodDelete, "/orgs/acme", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleDeleteOrg_NotFound(t *testing.T) {
	ms := &mockStore{
		deleteOrgFn: func(ctx context.Context, name string) error {
			return db.ErrOrgNotFound
		},
	}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodDelete, "/orgs/noexist", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleListOrgRepos_OrgNotFound(t *testing.T) {
	ms := &mockStore{
		getOrgFn: func(ctx context.Context, name string) (*model.Org, error) {
			return nil, db.ErrOrgNotFound
		},
	}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodGet, "/orgs/noexist/repos", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleCreateOrg_SlashInName(t *testing.T) {
	ms := &mockStore{}
	srv := New(ms, nil, devID, devID)
	body, _ := json.Marshal(map[string]string{"name": "bad/name"})
	req := httptest.NewRequest(http.MethodPost, "/orgs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleCreateRepo_OwnerSlashMismatch(t *testing.T) {
	ms := &mockStore{}
	srv := New(ms, nil, devID, devID)
	// Owner contains a slash — first segment "acme" != "acme/team"
	body, _ := json.Marshal(map[string]string{"owner": "acme/team", "name": "myrepo"})
	req := httptest.NewRequest(http.MethodPost, "/repos", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Role management handler tests
// ---------------------------------------------------------------------------

func TestHandleListRoles_Success(t *testing.T) {
	ms := &mockStore{
		listRolesFn: func(ctx context.Context, repo string) ([]model.Role, error) {
			return []model.Role{
				{Identity: "alice@example.com", Role: model.RoleAdmin},
				{Identity: "bob@example.com", Role: model.RoleReader},
			}, nil
		},
	}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/roles", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp model.RolesResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Roles) != 2 {
		t.Errorf("expected 2 roles, got %d", len(resp.Roles))
	}
}

func TestHandleListRoles_Empty(t *testing.T) {
	ms := &mockStore{
		listRolesFn: func(ctx context.Context, repo string) ([]model.Role, error) {
			return nil, nil
		},
	}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/roles", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp model.RolesResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Roles) != 0 {
		t.Errorf("expected 0 roles, got %d", len(resp.Roles))
	}
}

func TestHandleListRoles_InternalError(t *testing.T) {
	ms := &mockStore{
		listRolesFn: func(ctx context.Context, repo string) ([]model.Role, error) {
			return nil, errors.New("db error")
		},
	}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/roles", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
}

func TestHandleSetRole_Success(t *testing.T) {
	var capturedIdentity string
	var capturedRole model.RoleType
	ms := &mockStore{
		setRoleFn: func(ctx context.Context, repo, identity string, role model.RoleType) error {
			capturedIdentity = identity
			capturedRole = role
			return nil
		},
	}
	srv := New(ms, nil, devID, devID)
	body, _ := json.Marshal(model.SetRoleRequest{Role: model.RoleWriter})
	req := httptest.NewRequest(http.MethodPut, "/repos/default/default/-/roles/alice@example.com", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if capturedIdentity != "alice@example.com" {
		t.Errorf("expected identity alice@example.com, got %q", capturedIdentity)
	}
	if capturedRole != model.RoleWriter {
		t.Errorf("expected role writer, got %q", capturedRole)
	}
}

func TestHandleSetRole_InvalidRole(t *testing.T) {
	ms := &mockStore{}
	srv := New(ms, nil, devID, devID)
	body, _ := json.Marshal(model.SetRoleRequest{Role: "superuser"})
	req := httptest.NewRequest(http.MethodPut, "/repos/default/default/-/roles/alice@example.com", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleSetRole_InvalidJSON(t *testing.T) {
	ms := &mockStore{}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodPut, "/repos/default/default/-/roles/alice@example.com", bytes.NewReader([]byte("not json")))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandleSetRole_InternalError(t *testing.T) {
	ms := &mockStore{
		setRoleFn: func(ctx context.Context, repo, identity string, role model.RoleType) error {
			return errors.New("db down")
		},
	}
	srv := New(ms, nil, devID, devID)
	body, _ := json.Marshal(model.SetRoleRequest{Role: model.RoleAdmin})
	req := httptest.NewRequest(http.MethodPut, "/repos/default/default/-/roles/alice@example.com", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
}

func TestHandleDeleteRole_Success(t *testing.T) {
	ms := &mockStore{
		deleteRoleFn: func(ctx context.Context, repo, identity string) error {
			return nil
		},
	}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodDelete, "/repos/default/default/-/roles/alice@example.com", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleDeleteRole_NotFound(t *testing.T) {
	ms := &mockStore{
		deleteRoleFn: func(ctx context.Context, repo, identity string) error {
			return db.ErrRoleNotFound
		},
	}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodDelete, "/repos/default/default/-/roles/nobody@example.com", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestHandleDeleteRole_InternalError(t *testing.T) {
	ms := &mockStore{
		deleteRoleFn: func(ctx context.Context, repo, identity string) error {
			return errors.New("db error")
		},
	}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodDelete, "/repos/default/default/-/roles/alice@example.com", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// handleGetReviews additional paths
// ---------------------------------------------------------------------------

func TestHandleGetReviews_WithAtParam(t *testing.T) {
	var capturedSeq *int64
	ms := &mockStore{
		listReviewsFn: func(ctx context.Context, repo, branch string, atSeq *int64) ([]model.Review, error) {
			capturedSeq = atSeq
			return []model.Review{}, nil
		},
	}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/branch/main/reviews?at=5", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if capturedSeq == nil || *capturedSeq != 5 {
		t.Errorf("expected at=5, got %v", capturedSeq)
	}
}

func TestHandleGetReviews_InvalidAt(t *testing.T) {
	ms := &mockStore{}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/branch/main/reviews?at=notanumber", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandleGetReviews_InternalError(t *testing.T) {
	ms := &mockStore{
		listReviewsFn: func(ctx context.Context, repo, branch string, atSeq *int64) ([]model.Review, error) {
			return nil, errors.New("db error")
		},
	}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/branch/main/reviews", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// handleGetChecks additional paths
// ---------------------------------------------------------------------------

func TestHandleGetChecks_WithAtParam(t *testing.T) {
	var capturedSeq *int64
	ms := &mockStore{
		listCheckRunsFn: func(ctx context.Context, repo, branch string, atSeq *int64) ([]model.CheckRun, error) {
			capturedSeq = atSeq
			return []model.CheckRun{}, nil
		},
	}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/branch/main/checks?at=3", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if capturedSeq == nil || *capturedSeq != 3 {
		t.Errorf("expected at=3, got %v", capturedSeq)
	}
}

func TestHandleGetChecks_InvalidAt(t *testing.T) {
	ms := &mockStore{}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/branch/main/checks?at=bad", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandleGetChecks_InternalError(t *testing.T) {
	ms := &mockStore{
		listCheckRunsFn: func(ctx context.Context, repo, branch string, atSeq *int64) ([]model.CheckRun, error) {
			return nil, errors.New("db error")
		},
	}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/branch/main/checks", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// handleFileHistory additional paths
// ---------------------------------------------------------------------------

func TestHandleFileHistory_InvalidLimit(t *testing.T) {
	ms := &mockStore{}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/file/hello.txt/history?limit=notanumber", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleFileHistory_ZeroLimit(t *testing.T) {
	ms := &mockStore{}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/file/hello.txt/history?limit=0", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleFileHistory_InvalidAfter(t *testing.T) {
	ms := &mockStore{}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/file/hello.txt/history?after=notanumber", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Mock readStore and policyCache for policy handler tests
// ---------------------------------------------------------------------------

// mockReadStore implements the readStore interface for testing.
type mockReadStore struct {
	getBranchFn       func(ctx context.Context, repo, branch string) (*store.BranchInfo, error)
	getDiffFn         func(ctx context.Context, repo, branch string) (*store.DiffResult, error)
	materializeTreeFn func(ctx context.Context, repo, branch string, atSeq *int64, limit int, afterPath string) ([]store.TreeEntry, error)
	getFileFn         func(ctx context.Context, repo, branch, path string, atSeq *int64) (*store.FileContent, error)
	getFileHistoryFn  func(ctx context.Context, repo, branch, path string, limit int, afterSeq *int64) ([]store.FileHistoryEntry, error)
	listBranchesFn    func(ctx context.Context, repo, statusFilter string, includeDraft, onlyDraft bool) ([]store.BranchInfo, error)
	getCommitFn       func(ctx context.Context, repo string, seq int64) (*store.CommitDetail, error)
	getChainFn        func(ctx context.Context, repo string, from, to int64) ([]store.ChainEntry, error)
}

func (m *mockReadStore) GetBranch(ctx context.Context, repo, branch string) (*store.BranchInfo, error) {
	if m.getBranchFn != nil {
		return m.getBranchFn(ctx, repo, branch)
	}
	return &store.BranchInfo{Name: branch, HeadSequence: 1, BaseSequence: 0, Status: "active"}, nil
}

func (m *mockReadStore) GetDiff(ctx context.Context, repo, branch string) (*store.DiffResult, error) {
	if m.getDiffFn != nil {
		return m.getDiffFn(ctx, repo, branch)
	}
	return &store.DiffResult{}, nil
}

func (m *mockReadStore) MaterializeTree(ctx context.Context, repo, branch string, atSeq *int64, limit int, afterPath string) ([]store.TreeEntry, error) {
	if m.materializeTreeFn != nil {
		return m.materializeTreeFn(ctx, repo, branch, atSeq, limit, afterPath)
	}
	return nil, nil
}

func (m *mockReadStore) GetFile(ctx context.Context, repo, branch, path string, atSeq *int64) (*store.FileContent, error) {
	if m.getFileFn != nil {
		return m.getFileFn(ctx, repo, branch, path, atSeq)
	}
	return nil, nil
}

func (m *mockReadStore) GetFileHistory(ctx context.Context, repo, branch, path string, limit int, afterSeq *int64) ([]store.FileHistoryEntry, error) {
	if m.getFileHistoryFn != nil {
		return m.getFileHistoryFn(ctx, repo, branch, path, limit, afterSeq)
	}
	return nil, nil
}

func (m *mockReadStore) ListBranches(ctx context.Context, repo, statusFilter string, includeDraft, onlyDraft bool) ([]store.BranchInfo, error) {
	if m.listBranchesFn != nil {
		return m.listBranchesFn(ctx, repo, statusFilter, includeDraft, onlyDraft)
	}
	return nil, nil
}

func (m *mockReadStore) GetCommit(ctx context.Context, repo string, seq int64) (*store.CommitDetail, error) {
	if m.getCommitFn != nil {
		return m.getCommitFn(ctx, repo, seq)
	}
	return nil, nil
}

func (m *mockReadStore) GetChain(ctx context.Context, repo string, from, to int64) ([]store.ChainEntry, error) {
	if m.getChainFn != nil {
		return m.getChainFn(ctx, repo, from, to)
	}
	return []store.ChainEntry{}, nil
}

// mockPolicyCache implements the policyCache interface for testing.
type mockPolicyCache struct {
	loadFn       func(ctx context.Context, repo string, st policy.ReadStore) (*policy.Engine, map[string][]string, error)
	invalidateFn func(repo string)
	invalidated  []string
}

func (m *mockPolicyCache) Load(ctx context.Context, repo string, st policy.ReadStore) (*policy.Engine, map[string][]string, error) {
	if m.loadFn != nil {
		return m.loadFn(ctx, repo, st)
	}
	return nil, nil, nil
}

func (m *mockPolicyCache) Invalidate(repo string) {
	m.invalidated = append(m.invalidated, repo)
	if m.invalidateFn != nil {
		m.invalidateFn(repo)
	}
}

// newTestHandler creates an http.Handler using injected stores and policy cache.
// Used for handler-level policy tests that need custom readStore/policyCache.
func newTestHandler(ws WriteStore, rs readStore, pc policyCache) http.Handler {
	s := &server{
		commitStore: ws,
		readStore:   rs,
		policyCache: pc,
	}
	return s.buildHandler(devID, devID, ws)
}

// mustBuildEngine compiles a test OPA policy and panics on error.
func mustBuildEngine(t *testing.T, name, src string) *policy.Engine {
	t.Helper()
	engine, err := policy.NewEngine(context.Background(), map[string]string{
		".docstore/policy/" + name + ".rego": src,
	})
	if err != nil {
		t.Fatalf("compile policy: %v", err)
	}
	return engine
}

// ---------------------------------------------------------------------------
// Handler policy tests
// ---------------------------------------------------------------------------

// TestHandleMerge_PolicyDeny verifies that a deny policy results in a 403.
func TestHandleMerge_PolicyDeny(t *testing.T) {
	engine := mustBuildEngine(t, "require_review", `
package docstore.require_review
default allow = false
default reason = "a review is required"
`)

	ms := &mockStore{
		mergeFn: func(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error) {
			t.Fatal("merge should not be called when policy denies")
			return nil, nil, nil
		},
		listReviewsFn:   func(ctx context.Context, repo, branch string, atSeq *int64) ([]model.Review, error) { return nil, nil },
		listCheckRunsFn: func(ctx context.Context, repo, branch string, atSeq *int64) ([]model.CheckRun, error) { return nil, nil },
	}
	pc := &mockPolicyCache{
		loadFn: func(ctx context.Context, repo string, _ policy.ReadStore) (*policy.Engine, map[string][]string, error) {
			return engine, nil, nil
		},
	}
	rs := &mockReadStore{}

	srv := newTestHandler(ms, rs, pc)

	body, _ := json.Marshal(model.MergeRequest{Branch: "feature/test"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/merge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp model.MergePolicyError
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Policies) != 1 {
		t.Fatalf("expected 1 policy result, got %d", len(resp.Policies))
	}
	if resp.Policies[0].Pass {
		t.Error("expected policy to fail")
	}
}

// TestHandleMerge_PolicyAllow verifies that an allowing policy lets the merge proceed.
func TestHandleMerge_PolicyAllow(t *testing.T) {
	engine := mustBuildEngine(t, "always_allow", `
package docstore.always_allow
default allow = true
`)

	ms := &mockStore{
		mergeFn: func(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error) {
			return &model.MergeResponse{Sequence: 5}, nil, nil
		},
		listReviewsFn:   func(ctx context.Context, repo, branch string, atSeq *int64) ([]model.Review, error) { return nil, nil },
		listCheckRunsFn: func(ctx context.Context, repo, branch string, atSeq *int64) ([]model.CheckRun, error) { return nil, nil },
	}
	pc := &mockPolicyCache{
		loadFn: func(ctx context.Context, repo string, _ policy.ReadStore) (*policy.Engine, map[string][]string, error) {
			return engine, nil, nil
		},
	}
	rs := &mockReadStore{}

	srv := newTestHandler(ms, rs, pc)

	body, _ := json.Marshal(model.MergeRequest{Branch: "feature/test"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/merge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if len(pc.invalidated) == 0 || pc.invalidated[0] != "default/default" {
		t.Errorf("expected policyCache.Invalidate called for repo, got: %v", pc.invalidated)
	}
}

// TestHandleMerge_Bootstrap verifies that with no policies (nil engine) the merge proceeds normally.
func TestHandleMerge_Bootstrap(t *testing.T) {
	merged := false
	ms := &mockStore{
		mergeFn: func(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error) {
			merged = true
			return &model.MergeResponse{Sequence: 1}, nil, nil
		},
	}
	// policyCache returns nil engine (bootstrap mode — no .rego files)
	pc := &mockPolicyCache{
		loadFn: func(ctx context.Context, repo string, _ policy.ReadStore) (*policy.Engine, map[string][]string, error) {
			return nil, nil, nil
		},
	}
	rs := &mockReadStore{}

	srv := newTestHandler(ms, rs, pc)

	body, _ := json.Marshal(model.MergeRequest{Branch: "feature/test"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/merge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if !merged {
		t.Error("expected merge to be called in bootstrap mode")
	}
}

// TestHandleBranchStatus_NotMergeable verifies that a denying policy gives {mergeable:false}.
func TestHandleBranchStatus_NotMergeable(t *testing.T) {
	engine := mustBuildEngine(t, "require_review", `
package docstore.require_review
default allow = false
default reason = "a review is required"
`)

	ms := &mockStore{
		listReviewsFn:   func(ctx context.Context, repo, branch string, atSeq *int64) ([]model.Review, error) { return nil, nil },
		listCheckRunsFn: func(ctx context.Context, repo, branch string, atSeq *int64) ([]model.CheckRun, error) { return nil, nil },
	}
	pc := &mockPolicyCache{
		loadFn: func(ctx context.Context, repo string, _ policy.ReadStore) (*policy.Engine, map[string][]string, error) {
			return engine, nil, nil
		},
	}
	rs := &mockReadStore{}

	srv := newTestHandler(ms, rs, pc)

	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/branch/feature/x/status", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp model.BranchStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Mergeable {
		t.Error("expected mergeable=false")
	}
	if len(resp.Policies) != 1 || resp.Policies[0].Pass {
		t.Errorf("expected 1 failing policy, got: %+v", resp.Policies)
	}
}

// TestHandleBranchStatus_Mergeable verifies that an allowing policy gives {mergeable:true}.
func TestHandleBranchStatus_Mergeable(t *testing.T) {
	engine := mustBuildEngine(t, "always_allow", `
package docstore.always_allow
default allow = true
`)

	ms := &mockStore{
		listReviewsFn:   func(ctx context.Context, repo, branch string, atSeq *int64) ([]model.Review, error) { return nil, nil },
		listCheckRunsFn: func(ctx context.Context, repo, branch string, atSeq *int64) ([]model.CheckRun, error) { return nil, nil },
	}
	pc := &mockPolicyCache{
		loadFn: func(ctx context.Context, repo string, _ policy.ReadStore) (*policy.Engine, map[string][]string, error) {
			return engine, nil, nil
		},
	}
	rs := &mockReadStore{}

	srv := newTestHandler(ms, rs, pc)

	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/branch/feature/x/status", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp model.BranchStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Mergeable {
		t.Errorf("expected mergeable=true, policies: %+v", resp.Policies)
	}
}

// TestHandleMerge_StaleReview_Excluded verifies that when the handler calls
// ListReviews with headSeq, only reviews at that sequence are passed to policy.
// A stale review (seq < headSeq) results in no eligible reviews, so a
// require-review policy denies the merge.
func TestHandleMerge_StaleReview_Excluded(t *testing.T) {
	engine := mustBuildEngine(t, "require_review", `
package docstore.require_review
default allow = false
default reason = "at least one approval required"

allow if {
    count(input.reviews) > 0
    input.reviews[_].status == "approved"
}
`)

	const headSeq int64 = 5
	ms := &mockStore{
		mergeFn: func(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error) {
			t.Fatal("merge should not be called when policy denies")
			return nil, nil, nil
		},
		// ListReviews is called with atSeq=headSeq; the stale review at seq=3
		// is filtered out by the DB (simulated here by returning empty).
		listReviewsFn: func(ctx context.Context, repo, branch string, atSeq *int64) ([]model.Review, error) {
			if atSeq == nil || *atSeq != headSeq {
				t.Errorf("expected ListReviews called with atSeq=%d, got %v", headSeq, atSeq)
			}
			// No review at head sequence — the stale review (seq=3) is not returned.
			return []model.Review{}, nil
		},
		listCheckRunsFn: func(ctx context.Context, repo, branch string, atSeq *int64) ([]model.CheckRun, error) {
			return nil, nil
		},
	}
	pc := &mockPolicyCache{
		loadFn: func(ctx context.Context, repo string, _ policy.ReadStore) (*policy.Engine, map[string][]string, error) {
			return engine, nil, nil
		},
	}
	rs := &mockReadStore{
		getBranchFn: func(ctx context.Context, repo, branch string) (*store.BranchInfo, error) {
			return &store.BranchInfo{Name: branch, HeadSequence: headSeq, BaseSequence: 0, Status: "active"}, nil
		},
	}

	srv := newTestHandler(ms, rs, pc)

	body, _ := json.Marshal(model.MergeRequest{Branch: "feature/x"})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/merge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 (policy deny, no eligible reviews), got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp model.MergePolicyError
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Policies) == 0 || resp.Policies[0].Pass {
		t.Errorf("expected policy deny, got: %+v", resp.Policies)
	}
}

// TestHandleBranchStatus_NoSideEffects verifies that GET .../status does not
// write to any DB table.
func TestHandleBranchStatus_NoSideEffects(t *testing.T) {
	engine := mustBuildEngine(t, "always_allow", `
package docstore.always_allow
default allow = true
`)

	writeCallCount := 0
	writeDetector := func() {
		writeCallCount++
		t.Errorf("unexpected write operation called during branch status")
	}

	ms := &mockStore{
		// Write operations that must NOT be called:
		commitFn: func(ctx context.Context, req model.CommitRequest) (*model.CommitResponse, error) {
			writeDetector()
			return nil, nil
		},
		createBranchFn: func(ctx context.Context, req model.CreateBranchRequest) (*model.CreateBranchResponse, error) {
			writeDetector()
			return nil, nil
		},
		mergeFn: func(ctx context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error) {
			writeDetector()
			return nil, nil, nil
		},
		deleteBranchFn: func(ctx context.Context, repo, name string) error {
			writeDetector()
			return nil
		},
		rebaseFn: func(ctx context.Context, req model.RebaseRequest) (*model.RebaseResponse, []db.MergeConflict, error) {
			writeDetector()
			return nil, nil, nil
		},
		createReviewFn: func(ctx context.Context, repo, branch, reviewer string, status model.ReviewStatus, body string) (*model.Review, error) {
			writeDetector()
			return nil, nil
		},
		createCheckRunFn: func(ctx context.Context, repo, branch, checkName string, status model.CheckRunStatus, reporter string, logURL *string, atSequence *int64) (*model.CheckRun, error) {
			writeDetector()
			return nil, nil
		},
		setRoleFn: func(ctx context.Context, repo, identity string, role model.RoleType) error {
			writeDetector()
			return nil
		},
		deleteRoleFn: func(ctx context.Context, repo, identity string) error {
			writeDetector()
			return nil
		},
		purgeFn: func(ctx context.Context, req db.PurgeRequest) (*db.PurgeResult, error) {
			writeDetector()
			return nil, nil
		},
		// Read operations that may be called:
		listReviewsFn:   func(ctx context.Context, repo, branch string, atSeq *int64) ([]model.Review, error) { return nil, nil },
		listCheckRunsFn: func(ctx context.Context, repo, branch string, atSeq *int64) ([]model.CheckRun, error) { return nil, nil },
	}
	pc := &mockPolicyCache{
		loadFn: func(ctx context.Context, repo string, _ policy.ReadStore) (*policy.Engine, map[string][]string, error) {
			return engine, nil, nil
		},
	}
	rs := &mockReadStore{}

	srv := newTestHandler(ms, rs, pc)

	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/branch/feature/x/status", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if writeCallCount != 0 {
		t.Errorf("branch status triggered %d write operation(s)", writeCallCount)
	}
	if len(pc.invalidated) != 0 {
		t.Errorf("expected no Invalidate calls from branch status, got: %v", pc.invalidated)
	}
}

// ---------------------------------------------------------------------------
// Chain endpoint tests
// ---------------------------------------------------------------------------

func TestHandleChain_Success(t *testing.T) {
	hash := "abc123"
	rs := &mockReadStore{
		getChainFn: func(ctx context.Context, repo string, from, to int64) ([]store.ChainEntry, error) {
			if repo != "default/default" || from != 1 || to != 2 {
				t.Errorf("unexpected args: repo=%s from=%d to=%d", repo, from, to)
			}
			return []store.ChainEntry{
				{
					Sequence:   1,
					Branch:     "main",
					Author:     "alice",
					Message:    "first",
					CreatedAt:  time.Now(),
					CommitHash: &hash,
					Files:      []store.ChainFile{{Path: "a.txt", ContentHash: "aaa"}},
				},
			}, nil
		},
	}
	h := newTestHandler(&mockStore{}, rs, nil)
	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/chain?from=1&to=2", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var entries []store.ChainEntry
	if err := json.NewDecoder(w.Body).Decode(&entries); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Sequence != 1 {
		t.Errorf("expected sequence 1, got %d", entries[0].Sequence)
	}
	if entries[0].CommitHash == nil || *entries[0].CommitHash != "abc123" {
		t.Errorf("unexpected commit_hash: %v", entries[0].CommitHash)
	}
}

func TestHandleChain_MissingParams(t *testing.T) {
	rs := &mockReadStore{}
	h := newTestHandler(&mockStore{}, rs, nil)

	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/chain", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing params, got %d", w.Code)
	}
}

func TestHandleChain_InvalidFrom(t *testing.T) {
	rs := &mockReadStore{}
	h := newTestHandler(&mockStore{}, rs, nil)

	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/chain?from=abc&to=5", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid from, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Org membership handler tests
// ---------------------------------------------------------------------------

func TestHandleListOrgMembers_Forbidden(t *testing.T) {
	ms := &mockStore{
		getOrgFn: func(_ context.Context, name string) (*model.Org, error) {
			return &model.Org{Name: name}, nil
		},
		// getOrgMemberFn not set → returns ErrOrgMemberNotFound → 403
	}
	h := newTestHandler(ms, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/orgs/acme/members", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestHandleListOrgMembers_Success(t *testing.T) {
	members := []model.OrgMember{
		{Org: "acme", Identity: devID, Role: model.OrgRoleOwner, InvitedBy: "system"},
	}
	ms := &mockStore{
		getOrgFn: func(_ context.Context, name string) (*model.Org, error) {
			return &model.Org{Name: name}, nil
		},
		getOrgMemberFn: func(_ context.Context, org, identity string) (*model.OrgMember, error) {
			return &model.OrgMember{Org: org, Identity: identity, Role: model.OrgRoleOwner}, nil
		},
		listOrgMembersFn: func(_ context.Context, org string) ([]model.OrgMember, error) {
			return members, nil
		},
	}
	h := newTestHandler(ms, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/orgs/acme/members", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp model.OrgMembersResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Members) != 1 {
		t.Errorf("expected 1 member, got %d", len(resp.Members))
	}
}

func TestHandleAddOrgMember_OwnerRequired(t *testing.T) {
	ms := &mockStore{
		getOrgFn: func(_ context.Context, name string) (*model.Org, error) {
			return &model.Org{Name: name}, nil
		},
		getOrgMemberFn: func(_ context.Context, org, identity string) (*model.OrgMember, error) {
			// Identity is a member, not an owner.
			return &model.OrgMember{Org: org, Identity: identity, Role: model.OrgRoleMember}, nil
		},
	}
	h := newTestHandler(ms, nil, nil)

	body := bytes.NewBufferString(`{"role":"member"}`)
	req := httptest.NewRequest(http.MethodPost, "/orgs/acme/members/bob@example.com", body)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestHandleAddOrgMember_Success(t *testing.T) {
	var captured struct{ identity, role string }
	ms := &mockStore{
		getOrgFn: func(_ context.Context, name string) (*model.Org, error) {
			return &model.Org{Name: name}, nil
		},
		getOrgMemberFn: func(_ context.Context, org, identity string) (*model.OrgMember, error) {
			return &model.OrgMember{Org: org, Identity: identity, Role: model.OrgRoleOwner}, nil
		},
		addOrgMemberFn: func(_ context.Context, org, identity string, role model.OrgRole, invitedBy string) error {
			captured.identity = identity
			captured.role = string(role)
			return nil
		},
	}
	h := newTestHandler(ms, nil, nil)

	body := bytes.NewBufferString(`{"role":"member"}`)
	req := httptest.NewRequest(http.MethodPost, "/orgs/acme/members/bob@example.com", body)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if captured.identity != "bob@example.com" {
		t.Errorf("unexpected identity: %q", captured.identity)
	}
	if captured.role != "member" {
		t.Errorf("unexpected role: %q", captured.role)
	}
}

func TestHandleRemoveOrgMember_Success(t *testing.T) {
	var removed string
	ms := &mockStore{
		getOrgFn: func(_ context.Context, name string) (*model.Org, error) {
			return &model.Org{Name: name}, nil
		},
		getOrgMemberFn: func(_ context.Context, org, identity string) (*model.OrgMember, error) {
			return &model.OrgMember{Org: org, Identity: identity, Role: model.OrgRoleOwner}, nil
		},
		removeOrgMemberFn: func(_ context.Context, org, identity string) error {
			removed = identity
			return nil
		},
	}
	h := newTestHandler(ms, nil, nil)

	req := httptest.NewRequest(http.MethodDelete, "/orgs/acme/members/bob@example.com", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d: %s", w.Code, w.Body.String())
	}
	if removed != "bob@example.com" {
		t.Errorf("unexpected removed identity: %q", removed)
	}
}

func TestHandleRemoveOrgMember_NotFound(t *testing.T) {
	ms := &mockStore{
		getOrgFn: func(_ context.Context, name string) (*model.Org, error) {
			return &model.Org{Name: name}, nil
		},
		getOrgMemberFn: func(_ context.Context, org, identity string) (*model.OrgMember, error) {
			return &model.OrgMember{Org: org, Identity: identity, Role: model.OrgRoleOwner}, nil
		},
		removeOrgMemberFn: func(_ context.Context, org, identity string) error {
			return db.ErrOrgMemberNotFound
		},
	}
	h := newTestHandler(ms, nil, nil)

	req := httptest.NewRequest(http.MethodDelete, "/orgs/acme/members/nobody@example.com", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Org invite handler tests
// ---------------------------------------------------------------------------

func TestHandleCreateInvite_Success(t *testing.T) {
	ms := &mockStore{
		getOrgFn: func(_ context.Context, name string) (*model.Org, error) {
			return &model.Org{Name: name}, nil
		},
		getOrgMemberFn: func(_ context.Context, org, identity string) (*model.OrgMember, error) {
			return &model.OrgMember{Org: org, Identity: identity, Role: model.OrgRoleOwner}, nil
		},
		createInviteFn: func(_ context.Context, org, email string, role model.OrgRole, invitedBy, token string, expiresAt time.Time) (*model.OrgInvite, error) {
			return &model.OrgInvite{ID: "inv-1", Token: token}, nil
		},
	}
	h := newTestHandler(ms, nil, nil)

	body := bytes.NewBufferString(`{"email":"newuser@example.com","role":"member"}`)
	req := httptest.NewRequest(http.MethodPost, "/orgs/acme/invites", body)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var resp model.CreateInviteResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != "inv-1" {
		t.Errorf("unexpected id: %q", resp.ID)
	}
	if resp.Token == "" {
		t.Error("expected non-empty token")
	}
}

func TestHandleCreateInvite_InvalidRole(t *testing.T) {
	ms := &mockStore{
		getOrgFn: func(_ context.Context, name string) (*model.Org, error) {
			return &model.Org{Name: name}, nil
		},
		getOrgMemberFn: func(_ context.Context, org, identity string) (*model.OrgMember, error) {
			return &model.OrgMember{Org: org, Identity: identity, Role: model.OrgRoleOwner}, nil
		},
	}
	h := newTestHandler(ms, nil, nil)

	body := bytes.NewBufferString(`{"email":"newuser@example.com","role":"admin"}`)
	req := httptest.NewRequest(http.MethodPost, "/orgs/acme/invites", body)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleListInvites_Success(t *testing.T) {
	invites := []model.OrgInvite{
		{ID: "inv-1", Org: "acme", Email: "x@example.com", Role: model.OrgRoleMember, ExpiresAt: time.Now().Add(24 * time.Hour)},
	}
	ms := &mockStore{
		getOrgFn: func(_ context.Context, name string) (*model.Org, error) {
			return &model.Org{Name: name}, nil
		},
		getOrgMemberFn: func(_ context.Context, org, identity string) (*model.OrgMember, error) {
			return &model.OrgMember{Org: org, Identity: identity, Role: model.OrgRoleOwner}, nil
		},
		listInvitesFn: func(_ context.Context, org string) ([]model.OrgInvite, error) {
			return invites, nil
		},
	}
	h := newTestHandler(ms, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/orgs/acme/invites", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp model.OrgInvitesResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Invites) != 1 {
		t.Errorf("expected 1 invite, got %d", len(resp.Invites))
	}
}

func TestHandleAcceptInvite_Success(t *testing.T) {
	ms := &mockStore{
		acceptInviteFn: func(_ context.Context, org, token, identity string) error {
			return nil
		},
	}
	h := newTestHandler(ms, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/orgs/acme/invites/mytoken123/accept", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleAcceptInvite_EmailMismatch(t *testing.T) {
	ms := &mockStore{
		acceptInviteFn: func(_ context.Context, org, token, identity string) error {
			return db.ErrEmailMismatch
		},
	}
	h := newTestHandler(ms, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/orgs/acme/invites/mytoken123/accept", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestHandleAcceptInvite_Expired(t *testing.T) {
	ms := &mockStore{
		acceptInviteFn: func(_ context.Context, org, token, identity string) error {
			return db.ErrInviteExpired
		},
	}
	h := newTestHandler(ms, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/orgs/acme/invites/mytoken123/accept", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusGone {
		t.Errorf("expected 410, got %d", w.Code)
	}
}

func TestHandleRevokeInvite_Success(t *testing.T) {
	var revokedID string
	ms := &mockStore{
		getOrgFn: func(_ context.Context, name string) (*model.Org, error) {
			return &model.Org{Name: name}, nil
		},
		getOrgMemberFn: func(_ context.Context, org, identity string) (*model.OrgMember, error) {
			return &model.OrgMember{Org: org, Identity: identity, Role: model.OrgRoleOwner}, nil
		},
		revokeInviteFn: func(_ context.Context, org, inviteID string) error {
			revokedID = inviteID
			return nil
		},
	}
	h := newTestHandler(ms, nil, nil)

	req := httptest.NewRequest(http.MethodDelete, "/orgs/acme/invites/inv-abc", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d: %s", w.Code, w.Body.String())
	}
	if revokedID != "inv-abc" {
		t.Errorf("unexpected revoked id: %q", revokedID)
	}
}

func TestHandleRevokeInvite_NotFound(t *testing.T) {
	ms := &mockStore{
		getOrgFn: func(_ context.Context, name string) (*model.Org, error) {
			return &model.Org{Name: name}, nil
		},
		getOrgMemberFn: func(_ context.Context, org, identity string) (*model.OrgMember, error) {
			return &model.OrgMember{Org: org, Identity: identity, Role: model.OrgRoleOwner}, nil
		},
		revokeInviteFn: func(_ context.Context, org, inviteID string) error {
			return db.ErrInviteNotFound
		},
	}
	h := newTestHandler(ms, nil, nil)

	req := httptest.NewRequest(http.MethodDelete, "/orgs/acme/invites/no-such-id", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Release handler unit tests
// ---------------------------------------------------------------------------

func TestHandleCreateRelease(t *testing.T) {
	ms := &mockStore{
		getRepoFn: func(_ context.Context, name string) (*model.Repo, error) {
			return &model.Repo{Name: name}, nil
		},
		createReleaseFn: func(_ context.Context, repo, name string, sequence int64, body, createdBy string) (*model.Release, error) {
			return &model.Release{ID: "test-id", Repo: repo, Name: name, Sequence: sequence, Body: body, CreatedBy: createdBy}, nil
		},
	}
	h := newTestHandler(ms, nil, nil)

	body := `{"name":"v1.0","sequence":5,"body":"initial"}`
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/releases", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp model.Release
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Name != "v1.0" || resp.Sequence != 5 {
		t.Errorf("unexpected response: %+v", resp)
	}
}

func TestHandleCreateRelease_MissingName(t *testing.T) {
	ms := &mockStore{
		getRepoFn: func(_ context.Context, name string) (*model.Repo, error) {
			return &model.Repo{Name: name}, nil
		},
	}
	h := newTestHandler(ms, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/releases", bytes.NewBufferString(`{"sequence":1}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleCreateRelease_Conflict(t *testing.T) {
	ms := &mockStore{
		getRepoFn: func(_ context.Context, name string) (*model.Repo, error) {
			return &model.Repo{Name: name}, nil
		},
		createReleaseFn: func(_ context.Context, _, _ string, _ int64, _, _ string) (*model.Release, error) {
			return nil, db.ErrReleaseExists
		},
	}
	h := newTestHandler(ms, nil, nil)

	body := `{"name":"v1.0","sequence":1}`
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/releases", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Errorf("expected 409, got %d", w.Code)
	}
}

func TestHandleListReleases(t *testing.T) {
	ms := &mockStore{
		getRepoFn: func(_ context.Context, name string) (*model.Repo, error) {
			return &model.Repo{Name: name}, nil
		},
		listReleasesFn: func(_ context.Context, repo string, limit int, afterID string) ([]model.Release, error) {
			return []model.Release{
				{ID: "id1", Repo: repo, Name: "v2.0", Sequence: 10},
				{ID: "id2", Repo: repo, Name: "v1.0", Sequence: 5},
			}, nil
		},
	}
	h := newTestHandler(ms, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/releases", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp model.ListReleasesResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Releases) != 2 {
		t.Fatalf("expected 2 releases, got %d", len(resp.Releases))
	}
	if resp.Releases[0].Name != "v2.0" {
		t.Errorf("expected v2.0 first, got %s", resp.Releases[0].Name)
	}
}

func TestHandleGetRelease(t *testing.T) {
	ms := &mockStore{
		getReleaseFn: func(_ context.Context, repo, name string) (*model.Release, error) {
			return &model.Release{ID: "id1", Repo: repo, Name: name, Sequence: 7}, nil
		},
	}
	h := newTestHandler(ms, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/releases/v1.0", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp model.Release
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Sequence != 7 {
		t.Errorf("expected sequence=7, got %d", resp.Sequence)
	}
}

func TestHandleGetRelease_NotFound(t *testing.T) {
	ms := &mockStore{}
	h := newTestHandler(ms, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/releases/nope", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleDeleteRelease(t *testing.T) {
	var deleted string
	ms := &mockStore{
		deleteReleaseFn: func(_ context.Context, repo, name string) error {
			deleted = name
			return nil
		},
	}
	h := newTestHandler(ms, nil, nil)

	req := httptest.NewRequest(http.MethodDelete, "/repos/default/default/-/releases/v1.0", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}
	if deleted != "v1.0" {
		t.Errorf("expected v1.0 to be deleted, got %q", deleted)
	}
}

func TestHandleDeleteRelease_NotFound(t *testing.T) {
	ms := &mockStore{}
	h := newTestHandler(ms, nil, nil)

	req := httptest.NewRequest(http.MethodDelete, "/repos/default/default/-/releases/missing", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleGetRelease_RepoNotFound(t *testing.T) {
	ms := &mockStore{
		getRepoFn: func(_ context.Context, name string) (*model.Repo, error) {
			return nil, db.ErrRepoNotFound
		},
	}
	h := newTestHandler(ms, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/repos/ghost/ghost/-/releases/v1.0", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleDeleteRelease_RepoNotFound(t *testing.T) {
	ms := &mockStore{
		getRepoFn: func(_ context.Context, name string) (*model.Repo, error) {
			return nil, db.ErrRepoNotFound
		},
	}
	h := newTestHandler(ms, nil, nil)

	req := httptest.NewRequest(http.MethodDelete, "/repos/ghost/ghost/-/releases/v1.0", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleListReleases_InvalidCursor(t *testing.T) {
	ms := &mockStore{
		getRepoFn: func(_ context.Context, name string) (*model.Repo, error) {
			return &model.Repo{Name: name}, nil
		},
		listReleasesFn: func(_ context.Context, repo string, limit int, afterID string) ([]model.Release, error) {
			return nil, db.ErrInvalidCursor
		},
	}
	h := newTestHandler(ms, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/releases?after=does-not-exist", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleCreateRelease_InvalidSequence(t *testing.T) {
	ms := &mockStore{
		getRepoFn: func(_ context.Context, name string) (*model.Repo, error) {
			return &model.Repo{Name: name}, nil
		},
		commitSequenceExistsFn: func(_ context.Context, repo string, sequence int64) (bool, error) {
			return false, nil
		},
	}
	h := newTestHandler(ms, nil, nil)

	body := `{"name":"v1.0","sequence":9999}`
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/releases", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Archive endpoint tests
// ---------------------------------------------------------------------------

func TestHandleArchive_Success(t *testing.T) {
	vid := "v1"
	rs := &mockReadStore{
		getBranchFn: func(_ context.Context, repo, branch string) (*store.BranchInfo, error) {
			return &store.BranchInfo{Name: branch, HeadSequence: 1, BaseSequence: 0, Status: "active"}, nil
		},
		materializeTreeFn: func(_ context.Context, repo, branch string, atSeq *int64, limit int, afterPath string) ([]store.TreeEntry, error) {
			if afterPath != "" {
				return nil, nil // only one page
			}
			return []store.TreeEntry{
				{Path: "main.go", VersionID: vid, ContentHash: "hash1"},
				{Path: "sub/util.go", VersionID: "v2", ContentHash: "hash2"},
			}, nil
		},
		getFileFn: func(_ context.Context, repo, branch, path string, atSeq *int64) (*store.FileContent, error) {
			switch path {
			case "main.go":
				return &store.FileContent{Path: path, Content: []byte("package main")}, nil
			case "sub/util.go":
				return &store.FileContent{Path: path, Content: []byte("package sub")}, nil
			}
			return nil, nil
		},
	}
	h := newTestHandler(&mockStore{}, rs, nil)

	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/archive?branch=main", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/x-tar" {
		t.Errorf("expected Content-Type application/x-tar, got %q", ct)
	}

	// Read the tar and verify entries.
	tr := tar.NewReader(w.Body)
	files := make(map[string]string)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read tar entry %s: %v", hdr.Name, err)
		}
		files[hdr.Name] = string(content)
	}

	if len(files) != 2 {
		t.Fatalf("expected 2 files in archive, got %d", len(files))
	}
	if files["main.go"] != "package main" {
		t.Errorf("main.go = %q, want \"package main\"", files["main.go"])
	}
	if files["sub/util.go"] != "package sub" {
		t.Errorf("sub/util.go = %q, want \"package sub\"", files["sub/util.go"])
	}
}

func TestHandleArchive_AtSequence(t *testing.T) {
	const wantSeq int64 = 7
	var gotSeq *int64
	rs := &mockReadStore{
		getBranchFn: func(_ context.Context, repo, branch string) (*store.BranchInfo, error) {
			return &store.BranchInfo{Name: branch, HeadSequence: 99, BaseSequence: 0, Status: "active"}, nil
		},
		materializeTreeFn: func(_ context.Context, repo, branch string, atSeq *int64, limit int, afterPath string) ([]store.TreeEntry, error) {
			gotSeq = atSeq
			return nil, nil // empty archive is fine
		},
		getFileFn: func(_ context.Context, repo, branch, path string, atSeq *int64) (*store.FileContent, error) {
			return nil, nil
		},
	}
	h := newTestHandler(&mockStore{}, rs, nil)

	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/archive?branch=main&at=7", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotSeq == nil {
		t.Fatal("expected atSequence to be passed to MaterializeTree, got nil")
	}
	if *gotSeq != wantSeq {
		t.Errorf("expected atSequence=%d, got %d", wantSeq, *gotSeq)
	}
}

func TestHandleArchive_AtInvalid(t *testing.T) {
	rs := &mockReadStore{}
	h := newTestHandler(&mockStore{}, rs, nil)

	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/archive?branch=main&at=notanumber", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleArchive_MissingBranch(t *testing.T) {
	rs := &mockReadStore{}
	h := newTestHandler(&mockStore{}, rs, nil)

	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/archive", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleArchive_BranchNotFound(t *testing.T) {
	rs := &mockReadStore{
		getBranchFn: func(_ context.Context, repo, branch string) (*store.BranchInfo, error) {
			return nil, nil // branch not found
		},
	}
	h := newTestHandler(&mockStore{}, rs, nil)

	req := httptest.NewRequest(http.MethodGet, "/repos/default/default/-/archive?branch=nonexistent", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}
