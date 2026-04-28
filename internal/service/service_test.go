package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/events"
	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/service"
)

// ---------------------------------------------------------------------------
// Stub store
// ---------------------------------------------------------------------------

type stubStore struct {
	issue        *model.Issue
	comment      *model.IssueComment
	proposal     *model.Proposal
	reviewComment *model.ReviewComment

	createIssueErr    error
	getIssueErr       error
	updateIssueErr    error
	closeIssueErr     error
	reopenIssueErr    error
	getCommentErr     error
	updateCommentErr  error
	deleteCommentErr  error
	getProposalErr    error
	updateProposalErr error
	closeProposalErr  error
	createReviewErr   error
	getRevCommentErr  error
	deleteRevCommentErr error

	createIssueCalled    bool
	updateIssueCalled    bool
	closeIssueCalled     bool
	reopenIssueCalled    bool
	updateCommentCalled  bool
	deleteCommentCalled  bool
	updateProposalCalled bool
	closeProposalCalled  bool
	createReviewCalled   bool
	deleteRevCommentCalled bool
}

func (s *stubStore) CreateOrg(ctx context.Context, name, createdBy string) (*model.Org, error) {
	return &model.Org{Name: name}, nil
}
func (s *stubStore) AddOrgMember(ctx context.Context, org, identity string, role model.OrgRole, invitedBy string) error {
	return nil
}
func (s *stubStore) CreateRepo(ctx context.Context, req model.CreateRepoRequest) (*model.Repo, error) {
	return &model.Repo{Name: req.Owner + "/" + req.Name, Owner: req.Owner}, nil
}
func (s *stubStore) SetRole(ctx context.Context, repo, identity string, role model.RoleType) error {
	return nil
}
func (s *stubStore) GetRole(ctx context.Context, repo, identity string) (*model.Role, error) {
	return &model.Role{Identity: identity, Role: model.RoleReader}, nil
}
func (s *stubStore) CreateIssue(ctx context.Context, repo, title, body, author string, labels []string) (*model.Issue, error) {
	s.createIssueCalled = true
	if s.createIssueErr != nil {
		return nil, s.createIssueErr
	}
	if s.issue != nil {
		return s.issue, nil
	}
	return &model.Issue{ID: "iss-1", Repo: repo, Title: title, Body: body, Author: author, Number: 1}, nil
}
func (s *stubStore) GetIssue(ctx context.Context, repo string, number int64) (*model.Issue, error) {
	if s.getIssueErr != nil {
		return nil, s.getIssueErr
	}
	if s.issue != nil {
		return s.issue, nil
	}
	return &model.Issue{ID: "iss-1", Repo: repo, Number: number, Author: "alice@example.com"}, nil
}
func (s *stubStore) UpdateIssue(ctx context.Context, repo string, number int64, title, body *string, labels *[]string) (*model.Issue, error) {
	s.updateIssueCalled = true
	if s.updateIssueErr != nil {
		return nil, s.updateIssueErr
	}
	return &model.Issue{ID: "iss-1", Repo: repo, Number: number}, nil
}
func (s *stubStore) CloseIssue(ctx context.Context, repo string, number int64, reason model.IssueCloseReason, closedBy string) (*model.Issue, error) {
	s.closeIssueCalled = true
	if s.closeIssueErr != nil {
		return nil, s.closeIssueErr
	}
	return &model.Issue{ID: "iss-1", Repo: repo, Number: number, State: model.IssueStateClosed}, nil
}
func (s *stubStore) ReopenIssue(ctx context.Context, repo string, number int64) (*model.Issue, error) {
	s.reopenIssueCalled = true
	if s.reopenIssueErr != nil {
		return nil, s.reopenIssueErr
	}
	return &model.Issue{ID: "iss-1", Repo: repo, Number: number, State: model.IssueStateOpen}, nil
}
func (s *stubStore) CreateIssueComment(ctx context.Context, repo string, number int64, body, author string) (*model.IssueComment, error) {
	return &model.IssueComment{ID: "c-1", IssueID: "iss-1", Repo: repo, Body: body, Author: author}, nil
}
func (s *stubStore) GetIssueComment(ctx context.Context, repo, id string) (*model.IssueComment, error) {
	if s.getCommentErr != nil {
		return nil, s.getCommentErr
	}
	if s.comment != nil {
		return s.comment, nil
	}
	return &model.IssueComment{ID: id, IssueID: "iss-1", Repo: repo, Author: "alice@example.com"}, nil
}
func (s *stubStore) UpdateIssueComment(ctx context.Context, repo, id, body string) (*model.IssueComment, error) {
	s.updateCommentCalled = true
	if s.updateCommentErr != nil {
		return nil, s.updateCommentErr
	}
	return &model.IssueComment{ID: id, IssueID: "iss-1", Repo: repo, Body: body, Author: "alice@example.com"}, nil
}
func (s *stubStore) DeleteIssueComment(ctx context.Context, repo, id string) error {
	s.deleteCommentCalled = true
	return s.deleteCommentErr
}
func (s *stubStore) CreateIssueRef(ctx context.Context, repo string, number int64, refType model.IssueRefType, refID string) (*model.IssueRef, error) {
	return &model.IssueRef{ID: "ref-1"}, nil
}
func (s *stubStore) CreateProposal(ctx context.Context, repo, branch, baseBranch, title, description, author string) (*model.Proposal, error) {
	return &model.Proposal{ID: "p-1", Repo: repo, Branch: branch, Author: author, Title: title}, nil
}
func (s *stubStore) GetProposal(ctx context.Context, repo, proposalID string) (*model.Proposal, error) {
	if s.getProposalErr != nil {
		return nil, s.getProposalErr
	}
	if s.proposal != nil {
		return s.proposal, nil
	}
	return &model.Proposal{ID: proposalID, Repo: repo, Branch: "feature", Author: "alice@example.com"}, nil
}
func (s *stubStore) UpdateProposal(ctx context.Context, repo, proposalID string, title, description *string) (*model.Proposal, error) {
	s.updateProposalCalled = true
	if s.updateProposalErr != nil {
		return nil, s.updateProposalErr
	}
	return &model.Proposal{ID: proposalID, Repo: repo}, nil
}
func (s *stubStore) CloseProposal(ctx context.Context, repo, proposalID string) error {
	s.closeProposalCalled = true
	return s.closeProposalErr
}
func (s *stubStore) CreateReview(ctx context.Context, repo, branch, reviewer string, status model.ReviewStatus, body string) (*model.Review, error) {
	s.createReviewCalled = true
	if s.createReviewErr != nil {
		return nil, s.createReviewErr
	}
	return &model.Review{ID: "r-1", Repo: repo, Branch: branch, Reviewer: reviewer, Status: status}, nil
}
func (s *stubStore) GetReviewComment(ctx context.Context, repo, id string) (*model.ReviewComment, error) {
	if s.getRevCommentErr != nil {
		return nil, s.getRevCommentErr
	}
	if s.reviewComment != nil {
		return s.reviewComment, nil
	}
	return &model.ReviewComment{ID: id, Branch: "feature", Author: "alice@example.com"}, nil
}
func (s *stubStore) DeleteReviewComment(ctx context.Context, repo, id string) error {
	s.deleteRevCommentCalled = true
	return s.deleteRevCommentErr
}
func (s *stubStore) Commit(ctx context.Context, req model.CommitRequest) (*model.CommitResponse, error) {
	return &model.CommitResponse{Sequence: 1}, nil
}

// ---------------------------------------------------------------------------
// Authorization tests: UpdateIssue
// ---------------------------------------------------------------------------

func TestUpdateIssue_AuthorAllowed(t *testing.T) {
	st := &stubStore{}
	svc := service.New(st, nil, nil)
	ctx := context.Background()
	// alice is the issue author and a plain reader — should be allowed.
	_, err := svc.UpdateIssue(ctx, "alice@example.com", model.RoleReader, "org/repo", 1, nil, nil, nil)
	if err != nil {
		t.Fatalf("expected author to be allowed, got: %v", err)
	}
	if !st.updateIssueCalled {
		t.Fatal("expected store UpdateIssue to be called")
	}
}

func TestUpdateIssue_MaintainerAllowed(t *testing.T) {
	st := &stubStore{}
	svc := service.New(st, nil, nil)
	ctx := context.Background()
	// bob is NOT the author but is a maintainer — should be allowed.
	_, err := svc.UpdateIssue(ctx, "bob@example.com", model.RoleMaintainer, "org/repo", 1, nil, nil, nil)
	if err != nil {
		t.Fatalf("expected maintainer to be allowed, got: %v", err)
	}
}

func TestUpdateIssue_AdminAllowed(t *testing.T) {
	st := &stubStore{}
	svc := service.New(st, nil, nil)
	ctx := context.Background()
	_, err := svc.UpdateIssue(ctx, "carol@example.com", model.RoleAdmin, "org/repo", 1, nil, nil, nil)
	if err != nil {
		t.Fatalf("expected admin to be allowed, got: %v", err)
	}
}

func TestUpdateIssue_WriterForbidden(t *testing.T) {
	st := &stubStore{}
	svc := service.New(st, nil, nil)
	ctx := context.Background()
	// dave is a writer but not the author — should be rejected.
	_, err := svc.UpdateIssue(ctx, "dave@example.com", model.RoleWriter, "org/repo", 1, nil, nil, nil)
	if err == nil {
		t.Fatal("expected writer (non-author) to be rejected")
	}
	if !errors.Is(err, service.ErrForbidden) {
		t.Fatalf("expected ErrForbidden, got: %v", err)
	}
	if st.updateIssueCalled {
		t.Fatal("store UpdateIssue should not be called on auth failure")
	}
}

// ---------------------------------------------------------------------------
// Authorization tests: CloseIssue
// ---------------------------------------------------------------------------

func TestCloseIssue_AuthorAllowed(t *testing.T) {
	st := &stubStore{}
	svc := service.New(st, nil, nil)
	ctx := context.Background()
	_, err := svc.CloseIssue(ctx, "alice@example.com", model.RoleReader, "org/repo", 1, model.IssueCloseReasonCompleted)
	if err != nil {
		t.Fatalf("expected author to be allowed, got: %v", err)
	}
	if !st.closeIssueCalled {
		t.Fatal("expected store CloseIssue to be called")
	}
}

func TestCloseIssue_WriterForbidden(t *testing.T) {
	st := &stubStore{}
	svc := service.New(st, nil, nil)
	ctx := context.Background()
	_, err := svc.CloseIssue(ctx, "dave@example.com", model.RoleWriter, "org/repo", 1, model.IssueCloseReasonCompleted)
	if !errors.Is(err, service.ErrForbidden) {
		t.Fatalf("expected ErrForbidden, got: %v", err)
	}
}

func TestCloseIssue_MaintainerAllowed(t *testing.T) {
	st := &stubStore{}
	svc := service.New(st, nil, nil)
	ctx := context.Background()
	_, err := svc.CloseIssue(ctx, "eve@example.com", model.RoleMaintainer, "org/repo", 1, model.IssueCloseReasonCompleted)
	if err != nil {
		t.Fatalf("expected maintainer to be allowed, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Authorization tests: ReopenIssue
// ---------------------------------------------------------------------------

func TestReopenIssue_AuthorAllowed(t *testing.T) {
	st := &stubStore{}
	svc := service.New(st, nil, nil)
	ctx := context.Background()
	_, err := svc.ReopenIssue(ctx, "alice@example.com", model.RoleWriter, "org/repo", 1)
	if err != nil {
		t.Fatalf("expected author to be allowed, got: %v", err)
	}
}

func TestReopenIssue_WriterForbidden(t *testing.T) {
	st := &stubStore{}
	svc := service.New(st, nil, nil)
	ctx := context.Background()
	_, err := svc.ReopenIssue(ctx, "dave@example.com", model.RoleWriter, "org/repo", 1)
	if !errors.Is(err, service.ErrForbidden) {
		t.Fatalf("expected ErrForbidden, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Authorization tests: UpdateIssueComment
// ---------------------------------------------------------------------------

func TestUpdateIssueComment_AuthorAllowed(t *testing.T) {
	st := &stubStore{}
	svc := service.New(st, nil, nil)
	ctx := context.Background()
	_, err := svc.UpdateIssueComment(ctx, "alice@example.com", model.RoleReader, "org/repo", 1, "c-1", "new body")
	if err != nil {
		t.Fatalf("expected comment author to be allowed, got: %v", err)
	}
}

func TestUpdateIssueComment_WriterForbidden(t *testing.T) {
	st := &stubStore{}
	svc := service.New(st, nil, nil)
	ctx := context.Background()
	_, err := svc.UpdateIssueComment(ctx, "dave@example.com", model.RoleWriter, "org/repo", 1, "c-1", "new body")
	if !errors.Is(err, service.ErrForbidden) {
		t.Fatalf("expected ErrForbidden, got: %v", err)
	}
}

func TestUpdateIssueComment_MaintainerAllowed(t *testing.T) {
	st := &stubStore{}
	svc := service.New(st, nil, nil)
	ctx := context.Background()
	_, err := svc.UpdateIssueComment(ctx, "bob@example.com", model.RoleMaintainer, "org/repo", 1, "c-1", "new body")
	if err != nil {
		t.Fatalf("expected maintainer to be allowed, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Authorization tests: DeleteIssueComment
// ---------------------------------------------------------------------------

func TestDeleteIssueComment_AuthorAllowed(t *testing.T) {
	st := &stubStore{}
	svc := service.New(st, nil, nil)
	ctx := context.Background()
	err := svc.DeleteIssueComment(ctx, "alice@example.com", model.RoleReader, "org/repo", "c-1")
	if err != nil {
		t.Fatalf("expected comment author to be allowed, got: %v", err)
	}
}

func TestDeleteIssueComment_WriterForbidden(t *testing.T) {
	st := &stubStore{}
	svc := service.New(st, nil, nil)
	ctx := context.Background()
	err := svc.DeleteIssueComment(ctx, "dave@example.com", model.RoleWriter, "org/repo", "c-1")
	if !errors.Is(err, service.ErrForbidden) {
		t.Fatalf("expected ErrForbidden, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Authorization tests: UpdateProposal
// ---------------------------------------------------------------------------

func TestUpdateProposal_AuthorAllowed(t *testing.T) {
	st := &stubStore{}
	svc := service.New(st, nil, nil)
	ctx := context.Background()
	title := "new title"
	_, err := svc.UpdateProposal(ctx, "alice@example.com", model.RoleWriter, "org/repo", "p-1", &title, nil)
	if err != nil {
		t.Fatalf("expected author to be allowed, got: %v", err)
	}
}

func TestUpdateProposal_WriterForbidden(t *testing.T) {
	st := &stubStore{}
	svc := service.New(st, nil, nil)
	ctx := context.Background()
	title := "new title"
	_, err := svc.UpdateProposal(ctx, "dave@example.com", model.RoleWriter, "org/repo", "p-1", &title, nil)
	if !errors.Is(err, service.ErrForbidden) {
		t.Fatalf("expected ErrForbidden, got: %v", err)
	}
}

func TestUpdateProposal_MaintainerAllowed(t *testing.T) {
	st := &stubStore{}
	svc := service.New(st, nil, nil)
	ctx := context.Background()
	title := "new title"
	_, err := svc.UpdateProposal(ctx, "bob@example.com", model.RoleMaintainer, "org/repo", "p-1", &title, nil)
	if err != nil {
		t.Fatalf("expected maintainer to be allowed, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Authorization tests: CloseProposal
// ---------------------------------------------------------------------------

func TestCloseProposal_AuthorAllowed(t *testing.T) {
	st := &stubStore{}
	svc := service.New(st, nil, nil)
	ctx := context.Background()
	err := svc.CloseProposal(ctx, "alice@example.com", model.RoleWriter, "org/repo", "p-1")
	if err != nil {
		t.Fatalf("expected author to be allowed, got: %v", err)
	}
}

func TestCloseProposal_WriterForbidden(t *testing.T) {
	st := &stubStore{}
	svc := service.New(st, nil, nil)
	ctx := context.Background()
	err := svc.CloseProposal(ctx, "dave@example.com", model.RoleWriter, "org/repo", "p-1")
	if !errors.Is(err, service.ErrForbidden) {
		t.Fatalf("expected ErrForbidden, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Authorization tests: DeleteReviewComment
// ---------------------------------------------------------------------------

func TestDeleteReviewComment_AuthorAllowed(t *testing.T) {
	st := &stubStore{}
	svc := service.New(st, nil, nil)
	ctx := context.Background()
	err := svc.DeleteReviewComment(ctx, "alice@example.com", model.RoleWriter, "org/repo", "rc-1")
	if err != nil {
		t.Fatalf("expected comment author to be allowed, got: %v", err)
	}
}

func TestDeleteReviewComment_WriterForbidden(t *testing.T) {
	st := &stubStore{}
	svc := service.New(st, nil, nil)
	ctx := context.Background()
	err := svc.DeleteReviewComment(ctx, "dave@example.com", model.RoleWriter, "org/repo", "rc-1")
	if !errors.Is(err, service.ErrForbidden) {
		t.Fatalf("expected ErrForbidden, got: %v", err)
	}
}

func TestDeleteReviewComment_MaintainerAllowed(t *testing.T) {
	st := &stubStore{}
	svc := service.New(st, nil, nil)
	ctx := context.Background()
	err := svc.DeleteReviewComment(ctx, "bob@example.com", model.RoleMaintainer, "org/repo", "rc-1")
	if err != nil {
		t.Fatalf("expected maintainer to be allowed, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Self-approval: CreateReview passes through to store
// ---------------------------------------------------------------------------

func TestCreateReview_SelfApprovalPassedToStore(t *testing.T) {
	st := &stubStore{
		createReviewErr: db.ErrSelfApproval,
	}
	svc := service.New(st, nil, nil)
	ctx := context.Background()
	_, err := svc.CreateReview(ctx, "alice@example.com", "org/repo", "feature", model.ReviewApproved, "")
	if !errors.Is(err, db.ErrSelfApproval) {
		t.Fatalf("expected ErrSelfApproval from store to propagate, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Correctness: auto-grant on CreateOrg / CreateRepo
// ---------------------------------------------------------------------------

type trackingStore struct {
	stubStore
	addedOrgMember bool
	setRoleCalled  bool
	setRoleRole    model.RoleType
}

func (s *trackingStore) AddOrgMember(ctx context.Context, org, identity string, role model.OrgRole, invitedBy string) error {
	s.addedOrgMember = true
	return nil
}
func (s *trackingStore) SetRole(ctx context.Context, repo, identity string, role model.RoleType) error {
	s.setRoleCalled = true
	s.setRoleRole = role
	return nil
}

func TestCreateOrg_AutoGrantOwner(t *testing.T) {
	st := &trackingStore{}
	svc := service.New(st, nil, nil)
	ctx := context.Background()
	_, err := svc.CreateOrg(ctx, "alice@example.com", "myorg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !st.addedOrgMember {
		t.Fatal("expected creator to be added as org member (owner)")
	}
}

func TestCreateRepo_AutoGrantAdmin(t *testing.T) {
	st := &trackingStore{}
	svc := service.New(st, nil, nil)
	ctx := context.Background()
	req := model.CreateRepoRequest{Owner: "myorg", Name: "myrepo"}
	_, err := svc.CreateRepo(ctx, "alice@example.com", req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !st.setRoleCalled {
		t.Fatal("expected creator to be granted a role on the new repo")
	}
	if st.setRoleRole != model.RoleAdmin {
		t.Fatalf("expected admin role, got: %s", st.setRoleRole)
	}
}

// ---------------------------------------------------------------------------
// Correctness: policy cache invalidation
// ---------------------------------------------------------------------------

type trackingInvalidator struct {
	invalidated []string
}

func (t *trackingInvalidator) Invalidate(repo string) {
	t.invalidated = append(t.invalidated, repo)
}

func TestCommit_InvalidatesCacheOnMain(t *testing.T) {
	st := &stubStore{}
	inv := &trackingInvalidator{}
	svc := service.New(st, nil, inv)
	ctx := context.Background()
	req := model.CommitRequest{
		Repo:    "org/repo",
		Branch:  "main",
		Message: "test",
		Files:   []model.FileChange{{Path: "README.md", Content: []byte("hi")}},
	}
	_, err := svc.Commit(ctx, "alice@example.com", req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(inv.invalidated) == 0 || inv.invalidated[0] != "org/repo" {
		t.Fatalf("expected policy cache invalidated for org/repo, got: %v", inv.invalidated)
	}
}

func TestCommit_NoInvalidationOnNonMain(t *testing.T) {
	st := &stubStore{}
	inv := &trackingInvalidator{}
	svc := service.New(st, nil, inv)
	ctx := context.Background()
	req := model.CommitRequest{
		Repo:    "org/repo",
		Branch:  "feature",
		Message: "test",
		Files:   []model.FileChange{{Path: "foo.md", Content: []byte("hi")}},
	}
	_, err := svc.Commit(ctx, "alice@example.com", req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(inv.invalidated) != 0 {
		t.Fatalf("expected no policy cache invalidation for non-main branch, got: %v", inv.invalidated)
	}
}

// ---------------------------------------------------------------------------
// Observability: event emission
// ---------------------------------------------------------------------------

type captureEmitter struct {
	events []string
}

func (c *captureEmitter) Emit(ctx context.Context, e interface{ Type() string }) {
	c.events = append(c.events, e.Type())
}

// We need to satisfy the service.EventEmitter interface.
type serviceEventEmitter struct{ *captureEmitter }

func (s serviceEventEmitter) Emit(ctx context.Context, e interface {
	Type() string
	Source() string
	Data() any
}) {
	s.captureEmitter.events = append(s.captureEmitter.events, e.Type())
}

// emitterCapture records event types for test assertions.
type emitterCapture struct {
	types []string
}

func (e *emitterCapture) Emit(ctx context.Context, ev events.Event) {
	e.types = append(e.types, ev.Type())
}

func TestCreateIssue_EmitsEvent(t *testing.T) {
	st := &stubStore{}
	em := &emitterCapture{}
	svc := service.New(st, em, nil)
	ctx := context.Background()
	_, err := svc.CreateIssue(ctx, "alice@example.com", "org/repo", "Title", "body", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(em.types) == 0 {
		t.Fatal("expected event to be emitted")
	}
	if em.types[0] != "com.docstore.issue.opened" {
		t.Fatalf("expected com.docstore.issue.opened, got: %s", em.types[0])
	}
}

func TestCloseIssue_EmitsEvent(t *testing.T) {
	st := &stubStore{}
	em := &emitterCapture{}
	svc := service.New(st, em, nil)
	ctx := context.Background()
	_, err := svc.CloseIssue(ctx, "alice@example.com", model.RoleReader, "org/repo", 1, model.IssueCloseReasonCompleted)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(em.types) == 0 || em.types[0] != "com.docstore.issue.closed" {
		t.Fatalf("expected com.docstore.issue.closed, got: %v", em.types)
	}
}
