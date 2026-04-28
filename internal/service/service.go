// Package service implements the unified business logic layer for DocStore.
// Both the JSON API (internal/server) and the web UI (internal/ui) call through
// this layer so that authorization enforcement, role provisioning, cross-reference
// extraction, policy cache invalidation, and event emission are applied uniformly
// regardless of entry point.
//
// Service methods accept identity and role as explicit parameters; callers are
// responsible for resolving these from their respective authentication mechanisms
// (e.g. context middleware for the API, store look-up for the UI).
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"

	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/events"
	evtypes "github.com/dlorenc/docstore/internal/events/types"
	"github.com/dlorenc/docstore/internal/model"
)

// ErrForbidden is returned when an operation is rejected because the caller
// lacks sufficient authority (not the resource owner and not a maintainer+).
var ErrForbidden = errors.New("forbidden")

// ErrConflict is returned when an operation cannot proceed due to the current
// state of the resource (e.g. closing an already-closed issue).
var ErrConflict = errors.New("conflict")

// Store is the minimal subset of the database write interface that the service
// requires. *db.Store satisfies this interface.
type Store interface {
	// Org management
	CreateOrg(ctx context.Context, name, createdBy string) (*model.Org, error)
	AddOrgMember(ctx context.Context, org, identity string, role model.OrgRole, invitedBy string) error

	// Repo management
	CreateRepo(ctx context.Context, req model.CreateRepoRequest) (*model.Repo, error)
	SetRole(ctx context.Context, repo, identity string, role model.RoleType) error
	GetRole(ctx context.Context, repo, identity string) (*model.Role, error)

	// Issue management
	CreateIssue(ctx context.Context, repo, title, body, author string, labels []string) (*model.Issue, error)
	GetIssue(ctx context.Context, repo string, number int64) (*model.Issue, error)
	UpdateIssue(ctx context.Context, repo string, number int64, title, body *string, labels *[]string) (*model.Issue, error)
	CloseIssue(ctx context.Context, repo string, number int64, reason model.IssueCloseReason, closedBy string) (*model.Issue, error)
	ReopenIssue(ctx context.Context, repo string, number int64) (*model.Issue, error)

	// Issue comment management
	CreateIssueComment(ctx context.Context, repo string, number int64, body, author string) (*model.IssueComment, error)
	GetIssueComment(ctx context.Context, repo, id string) (*model.IssueComment, error)
	UpdateIssueComment(ctx context.Context, repo, id, body string) (*model.IssueComment, error)
	DeleteIssueComment(ctx context.Context, repo, id string) error

	// Issue ref management
	CreateIssueRef(ctx context.Context, repo string, number int64, refType model.IssueRefType, refID string) (*model.IssueRef, error)

	// Proposal management
	CreateProposal(ctx context.Context, repo, branch, baseBranch, title, description, author string) (*model.Proposal, error)
	GetProposal(ctx context.Context, repo, proposalID string) (*model.Proposal, error)
	UpdateProposal(ctx context.Context, repo, proposalID string, title, description *string) (*model.Proposal, error)
	CloseProposal(ctx context.Context, repo, proposalID string) error

	// Review management
	CreateReview(ctx context.Context, repo, branch, reviewer string, status model.ReviewStatus, body string) (*model.Review, error)
	GetReviewComment(ctx context.Context, repo, id string) (*model.ReviewComment, error)
	DeleteReviewComment(ctx context.Context, repo, id string) error

	// Commit
	Commit(ctx context.Context, req model.CommitRequest) (*model.CommitResponse, error)
}

// EventEmitter publishes domain events after successful mutations.
// *events.Broker satisfies this interface.
type EventEmitter interface {
	Emit(ctx context.Context, e events.Event)
}

// PolicyInvalidator invalidates the compiled OPA policy cache for a repo.
// *policy.Cache satisfies this interface.
type PolicyInvalidator interface {
	Invalidate(repo string)
}

// Service owns all write business logic: authorization enforcement, role
// provisioning, cross-reference extraction, policy cache invalidation, and
// event emission.
type Service struct {
	store   Store
	emitter EventEmitter
	policy  PolicyInvalidator
}

// New constructs a Service. emitter and policy may be nil; the service will
// skip event emission / policy invalidation when they are not configured.
func New(store Store, emitter EventEmitter, policy PolicyInvalidator) *Service {
	return &Service{store: store, emitter: emitter, policy: policy}
}

// emit publishes an event if an emitter is configured.
func (s *Service) emit(ctx context.Context, e events.Event) {
	if s.emitter != nil {
		s.emitter.Emit(ctx, e)
	}
}

// Emit publishes a domain event. Called directly by UI handlers for write
// operations not owned by the service (e.g. branch CRUD, role changes).
func (s *Service) Emit(ctx context.Context, e events.Event) {
	s.emit(ctx, e)
}

// invalidatePolicy invalidates the policy cache for repo when committing to main.
func (s *Service) invalidatePolicy(repo string) {
	if s.policy != nil {
		s.policy.Invalidate(repo)
	}
}

// isMaintainerOrAdmin returns true if the role is maintainer or admin.
func isMaintainerOrAdmin(role model.RoleType) bool {
	return role == model.RoleMaintainer || role == model.RoleAdmin
}

// ---------------------------------------------------------------------------
// Cross-reference helpers (package-local, also exported for server reuse)
// ---------------------------------------------------------------------------

var (
	reMentionProposal = regexp.MustCompile(`\bproposal:([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\b`)
	reMentionCommit   = regexp.MustCompile(`\bcommit:(\d+)\b`)
	reMentionIssue    = regexp.MustCompile(`(?:^|[^&\w])#(\d+)`)
)

// ParseMentions extracts cross-reference mentions from a text body.
// Returns proposal UUIDs, commit sequence numbers, and issue numbers.
func ParseMentions(body string) (proposalIDs []string, commitSeqs []int64, issueNums []int64) {
	for _, m := range reMentionProposal.FindAllStringSubmatch(body, -1) {
		proposalIDs = append(proposalIDs, m[1])
	}
	for _, m := range reMentionCommit.FindAllStringSubmatch(body, -1) {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			commitSeqs = append(commitSeqs, n)
		}
	}
	for _, m := range reMentionIssue.FindAllStringSubmatch(body, -1) {
		// Skip 6-digit sequences that look like CSS hex colors (#rrggbb).
		if len(m[1]) == 6 {
			continue
		}
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			issueNums = append(issueNums, n)
		}
	}
	return
}

// upsertIssueBodyRefs creates issue_refs on issueNum for proposal and commit
// mentions found in body, ignoring duplicate and not-found errors.
func (s *Service) upsertIssueBodyRefs(ctx context.Context, repo string, issueNum int64, body string) {
	proposalIDs, commitSeqs, _ := ParseMentions(body)
	for _, pid := range proposalIDs {
		_, err := s.store.CreateIssueRef(ctx, repo, issueNum, model.IssueRefTypeProposal, pid)
		if err != nil && !errors.Is(err, db.ErrIssueRefExists) && !errors.Is(err, db.ErrIssueNotFound) {
			slog.Warn("upsert issue ref", "repo", repo, "issue", issueNum, "ref_type", "proposal", "ref_id", pid, "error", err)
		}
	}
	for _, seq := range commitSeqs {
		refID := strconv.FormatInt(seq, 10)
		_, err := s.store.CreateIssueRef(ctx, repo, issueNum, model.IssueRefTypeCommit, refID)
		if err != nil && !errors.Is(err, db.ErrIssueRefExists) && !errors.Is(err, db.ErrIssueNotFound) {
			slog.Warn("upsert issue ref", "repo", repo, "issue", issueNum, "ref_type", "commit", "ref_id", refID, "error", err)
		}
	}
}

// upsertProposalMentionRefs creates issue_refs for issues mentioned in a
// proposal body. When a proposal body contains "#N", an issue_ref of type
// "proposal" pointing at proposalID is upserted on issue N.
func (s *Service) upsertProposalMentionRefs(ctx context.Context, repo, proposalID, body string) {
	_, _, issueNums := ParseMentions(body)
	for _, num := range issueNums {
		_, err := s.store.CreateIssueRef(ctx, repo, num, model.IssueRefTypeProposal, proposalID)
		if err != nil && !errors.Is(err, db.ErrIssueRefExists) && !errors.Is(err, db.ErrIssueNotFound) {
			slog.Warn("upsert proposal mention ref", "repo", repo, "issue", num, "proposal", proposalID, "error", err)
		}
	}
}

// ---------------------------------------------------------------------------
// Org operations
// ---------------------------------------------------------------------------

// CreateOrg creates an organisation and automatically grants the creator the
// org-owner role. Emits OrgCreated.
func (s *Service) CreateOrg(ctx context.Context, identity, name string) (*model.Org, error) {
	org, err := s.store.CreateOrg(ctx, name, identity)
	if err != nil {
		return nil, err
	}
	if addErr := s.store.AddOrgMember(ctx, org.Name, identity, model.OrgRoleOwner, identity); addErr != nil {
		slog.Error("failed to add org creator as owner", "org", org.Name, "identity", identity, "error", addErr)
	}
	s.emit(ctx, evtypes.OrgCreated{Org: org.Name, CreatedBy: identity})
	return org, nil
}

// ---------------------------------------------------------------------------
// Repo operations
// ---------------------------------------------------------------------------

// CreateRepo creates a repository and automatically grants the creator the
// admin role. Emits RepoCreated.
func (s *Service) CreateRepo(ctx context.Context, identity string, req model.CreateRepoRequest) (*model.Repo, error) {
	if req.CreatedBy == "" {
		req.CreatedBy = identity
	}
	repo, err := s.store.CreateRepo(ctx, req)
	if err != nil {
		return nil, err
	}
	if setErr := s.store.SetRole(ctx, repo.Name, identity, model.RoleAdmin); setErr != nil {
		slog.Error("failed to add repo creator as admin", "repo", repo.Name, "identity", identity, "error", setErr)
	}
	s.emit(ctx, evtypes.RepoCreated{Repo: repo.Name, Owner: repo.Owner, CreatedBy: identity})
	return repo, nil
}

// ---------------------------------------------------------------------------
// Issue operations
// ---------------------------------------------------------------------------

// CreateIssue creates an issue, extracts cross-reference mentions from the body,
// and emits IssueOpened.
func (s *Service) CreateIssue(ctx context.Context, identity, repo, title, body string, labels []string) (*model.Issue, error) {
	iss, err := s.store.CreateIssue(ctx, repo, title, body, identity, labels)
	if err != nil {
		return nil, err
	}
	s.upsertIssueBodyRefs(ctx, repo, iss.Number, body)
	s.emit(ctx, evtypes.IssueOpened{Repo: repo, IssueID: iss.ID, Number: iss.Number, Author: identity})
	slog.Info("issue opened", "repo", repo, "number", iss.Number, "author", identity)
	return iss, nil
}

// UpdateIssue updates an issue after verifying that the caller is either the
// issue author or a maintainer+. Extracts cross-reference mentions from a
// non-nil body. Emits IssueUpdated.
func (s *Service) UpdateIssue(ctx context.Context, identity string, role model.RoleType, repo string, number int64, title, body *string, labels *[]string) (*model.Issue, error) {
	existing, err := s.store.GetIssue(ctx, repo, number)
	if err != nil {
		return nil, err
	}
	if existing.Author != identity && !isMaintainerOrAdmin(role) {
		return nil, fmt.Errorf("%w: must be issue author or maintainer", ErrForbidden)
	}
	iss, err := s.store.UpdateIssue(ctx, repo, number, title, body, labels)
	if err != nil {
		return nil, err
	}
	if body != nil {
		s.upsertIssueBodyRefs(ctx, repo, number, *body)
	}
	s.emit(ctx, evtypes.IssueUpdated{Repo: repo, IssueID: iss.ID, Number: iss.Number})
	return iss, nil
}

// CloseIssue closes an issue after verifying that the caller is either the
// issue author or a maintainer+. Returns ErrConflict if already closed.
// Emits IssueClosed.
func (s *Service) CloseIssue(ctx context.Context, identity string, role model.RoleType, repo string, number int64, reason model.IssueCloseReason) (*model.Issue, error) {
	existing, err := s.store.GetIssue(ctx, repo, number)
	if err != nil {
		return nil, err
	}
	if existing.Author != identity && !isMaintainerOrAdmin(role) {
		return nil, fmt.Errorf("%w: must be issue author or maintainer", ErrForbidden)
	}
	if existing.State == model.IssueStateClosed {
		return nil, fmt.Errorf("%w: issue is already closed", ErrConflict)
	}
	iss, err := s.store.CloseIssue(ctx, repo, number, reason, identity)
	if err != nil {
		return nil, err
	}
	s.emit(ctx, evtypes.IssueClosed{Repo: repo, IssueID: iss.ID, Number: iss.Number, Reason: string(reason), ClosedBy: identity})
	slog.Info("issue closed", "repo", repo, "number", number, "by", identity)
	return iss, nil
}

// ReopenIssue reopens an issue after verifying that the caller is either the
// issue author or a maintainer+. Returns ErrConflict if already open.
// Emits IssueReopened.
func (s *Service) ReopenIssue(ctx context.Context, identity string, role model.RoleType, repo string, number int64) (*model.Issue, error) {
	existing, err := s.store.GetIssue(ctx, repo, number)
	if err != nil {
		return nil, err
	}
	if existing.Author != identity && !isMaintainerOrAdmin(role) {
		return nil, fmt.Errorf("%w: must be issue author or maintainer", ErrForbidden)
	}
	if existing.State == model.IssueStateOpen {
		return nil, fmt.Errorf("%w: issue is already open", ErrConflict)
	}
	iss, err := s.store.ReopenIssue(ctx, repo, number)
	if err != nil {
		return nil, err
	}
	s.emit(ctx, evtypes.IssueReopened{Repo: repo, IssueID: iss.ID, Number: iss.Number})
	slog.Info("issue reopened", "repo", repo, "number", number)
	return iss, nil
}

// CreateIssueComment creates a comment on an issue, extracts cross-reference
// mentions from the body, and emits IssueCommentCreated.
func (s *Service) CreateIssueComment(ctx context.Context, identity, repo string, number int64, body string) (*model.IssueComment, error) {
	c, err := s.store.CreateIssueComment(ctx, repo, number, body, identity)
	if err != nil {
		return nil, err
	}
	s.upsertIssueBodyRefs(ctx, repo, number, body)
	s.emit(ctx, evtypes.IssueCommentCreated{Repo: repo, IssueID: c.IssueID, Number: number, CommentID: c.ID, Author: identity})
	slog.Info("issue comment created", "repo", repo, "issue", number, "comment", c.ID, "author", identity)
	return c, nil
}

// UpdateIssueComment updates an issue comment after verifying that the caller
// is either the comment author or a maintainer+. Extracts cross-reference
// mentions from the new body. Emits IssueCommentUpdated.
func (s *Service) UpdateIssueComment(ctx context.Context, identity string, role model.RoleType, repo string, number int64, commentID, body string) (*model.IssueComment, error) {
	existing, err := s.store.GetIssueComment(ctx, repo, commentID)
	if err != nil {
		return nil, err
	}
	if existing.Author != identity && !isMaintainerOrAdmin(role) {
		return nil, fmt.Errorf("%w: must be comment author or maintainer", ErrForbidden)
	}
	c, err := s.store.UpdateIssueComment(ctx, repo, commentID, body)
	if err != nil {
		return nil, err
	}
	s.upsertIssueBodyRefs(ctx, repo, number, body)
	s.emit(ctx, evtypes.IssueCommentUpdated{Repo: repo, IssueID: c.IssueID, Number: number, CommentID: c.ID})
	return c, nil
}

// DeleteIssueComment deletes an issue comment after verifying that the caller
// is either the comment author or a maintainer+.
func (s *Service) DeleteIssueComment(ctx context.Context, identity string, role model.RoleType, repo, commentID string) error {
	existing, err := s.store.GetIssueComment(ctx, repo, commentID)
	if err != nil {
		return err
	}
	if existing.Author != identity && !isMaintainerOrAdmin(role) {
		return fmt.Errorf("%w: must be comment author or maintainer", ErrForbidden)
	}
	return s.store.DeleteIssueComment(ctx, repo, commentID)
}

// ---------------------------------------------------------------------------
// Proposal operations
// ---------------------------------------------------------------------------

// CreateProposal creates a proposal, extracts cross-reference mentions from
// the description, and emits ProposalOpened.
func (s *Service) CreateProposal(ctx context.Context, identity, repo, branch, baseBranch, title, description string) (*model.Proposal, error) {
	p, err := s.store.CreateProposal(ctx, repo, branch, baseBranch, title, description, identity)
	if err != nil {
		return nil, err
	}
	s.upsertProposalMentionRefs(ctx, repo, p.ID, description)
	s.emit(ctx, evtypes.ProposalOpened{
		Repo:       repo,
		Branch:     branch,
		BaseBranch: baseBranch,
		ProposalID: p.ID,
		Author:     identity,
	})
	slog.Info("proposal opened", "repo", repo, "branch", branch, "proposal_id", p.ID, "author", identity)
	return p, nil
}

// UpdateProposal updates a proposal after verifying that the caller is either
// the proposal author or a maintainer+. Extracts cross-reference mentions from
// a non-nil description.
func (s *Service) UpdateProposal(ctx context.Context, identity string, role model.RoleType, repo, proposalID string, title, description *string) (*model.Proposal, error) {
	existing, err := s.store.GetProposal(ctx, repo, proposalID)
	if err != nil {
		return nil, err
	}
	if existing.Author != identity && !isMaintainerOrAdmin(role) {
		return nil, fmt.Errorf("%w: must be proposal author or maintainer", ErrForbidden)
	}
	p, err := s.store.UpdateProposal(ctx, repo, proposalID, title, description)
	if err != nil {
		return nil, err
	}
	if description != nil {
		s.upsertProposalMentionRefs(ctx, repo, proposalID, *description)
	}
	return p, nil
}

// CloseProposal closes a proposal after verifying that the caller is either
// the proposal author or a maintainer+. Emits ProposalClosed.
func (s *Service) CloseProposal(ctx context.Context, identity string, role model.RoleType, repo, proposalID string) error {
	existing, err := s.store.GetProposal(ctx, repo, proposalID)
	if err != nil {
		return err
	}
	if existing.Author != identity && !isMaintainerOrAdmin(role) {
		return fmt.Errorf("%w: must be proposal author or maintainer", ErrForbidden)
	}
	if err := s.store.CloseProposal(ctx, repo, proposalID); err != nil {
		return err
	}
	s.emit(ctx, evtypes.ProposalClosed{
		Repo:       repo,
		Branch:     existing.Branch,
		ProposalID: proposalID,
	})
	slog.Info("proposal closed", "repo", repo, "proposal_id", proposalID, "by", identity)
	return nil
}

// ---------------------------------------------------------------------------
// Review operations
// ---------------------------------------------------------------------------

// CreateReview submits a review. Self-approval is rejected at the storage
// layer (ErrSelfApproval). Emits ReviewSubmitted.
func (s *Service) CreateReview(ctx context.Context, identity, repo, branch string, status model.ReviewStatus, body string) (*model.Review, error) {
	review, err := s.store.CreateReview(ctx, repo, branch, identity, status, body)
	if err != nil {
		return nil, err
	}
	s.emit(ctx, evtypes.ReviewSubmitted{
		Repo:     repo,
		Branch:   branch,
		Sequence: review.Sequence,
		Reviewer: identity,
		Status:   string(status),
	})
	slog.Info("review submitted", "repo", repo, "branch", branch, "reviewer", identity, "status", status)
	return review, nil
}

// DeleteReviewComment deletes a review comment after verifying that the caller
// is either the comment author or a maintainer+.
func (s *Service) DeleteReviewComment(ctx context.Context, identity string, role model.RoleType, repo, commentID string) error {
	comment, err := s.store.GetReviewComment(ctx, repo, commentID)
	if err != nil {
		return err
	}
	if comment.Author != identity && !isMaintainerOrAdmin(role) {
		return fmt.Errorf("%w: must be comment author or maintainer", ErrForbidden)
	}
	return s.store.DeleteReviewComment(ctx, repo, commentID)
}

// ---------------------------------------------------------------------------
// Commit operations
// ---------------------------------------------------------------------------

// Commit writes a commit to a branch, invalidates the policy cache when the
// commit targets the main branch, and emits CommitCreated.
func (s *Service) Commit(ctx context.Context, identity string, req model.CommitRequest) (*model.CommitResponse, error) {
	// Author always comes from the authenticated identity.
	req.Author = identity
	resp, err := s.store.Commit(ctx, req)
	if err != nil {
		return nil, err
	}
	if req.Branch == "main" {
		s.invalidatePolicy(req.Repo)
	}
	s.emit(ctx, evtypes.CommitCreated{
		Repo:      req.Repo,
		Branch:    req.Branch,
		Sequence:  resp.Sequence,
		Author:    identity,
		Message:   req.Message,
		FileCount: len(req.Files),
	})
	slog.Info("commit created", "repo", req.Repo, "branch", req.Branch, "sequence", resp.Sequence, "files", len(req.Files), "author", identity)
	return resp, nil
}

// ---------------------------------------------------------------------------
// Role lookup helpers (for use by UI handlers that need role before calling
// service methods that enforce authorization)
// ---------------------------------------------------------------------------

// GetRole returns the role for identity on repo, returning the zero value
// if the identity has no role or an error occurs.
func (s *Service) GetRole(ctx context.Context, repo, identity string) model.RoleType {
	r, err := s.store.GetRole(ctx, repo, identity)
	if err != nil || r == nil {
		return ""
	}
	return r.Role
}
