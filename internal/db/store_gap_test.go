package db

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/testutil"
)

// ---------------------------------------------------------------------------
// GetOrg / ListOrgs tests
// ---------------------------------------------------------------------------

func TestGetOrg_Found(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	if _, err := s.CreateOrg(ctx, "myorg", "alice@example.com"); err != nil {
		t.Fatalf("create org: %v", err)
	}

	org, err := s.GetOrg(ctx, "myorg")
	if err != nil {
		t.Fatalf("get org: %v", err)
	}
	if org.Name != "myorg" {
		t.Errorf("expected name myorg, got %q", org.Name)
	}
	if org.CreatedBy != "alice@example.com" {
		t.Errorf("expected created_by alice@example.com, got %q", org.CreatedBy)
	}
}

func TestGetOrg_NotFound(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.GetOrg(ctx, "nonexistent-org")
	if !errors.Is(err, ErrOrgNotFound) {
		t.Errorf("expected ErrOrgNotFound, got %v", err)
	}
}

func TestListOrgs_Empty(t *testing.T) {
	t.Parallel()
	// Fresh DB has only the 'default' org from migrations.
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	orgs, err := s.ListOrgs(ctx)
	if err != nil {
		t.Fatalf("list orgs: %v", err)
	}
	// The 'default' org is always inserted by migrations.
	for _, o := range orgs {
		if o.Name != "default" {
			t.Errorf("unexpected org: %q", o.Name)
		}
	}
}

func TestListOrgs_Multiple(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	for _, name := range []string{"alpha", "beta", "gamma"} {
		if _, err := s.CreateOrg(ctx, name, "admin@example.com"); err != nil {
			t.Fatalf("create org %q: %v", name, err)
		}
	}

	orgs, err := s.ListOrgs(ctx)
	if err != nil {
		t.Fatalf("list orgs: %v", err)
	}

	names := make(map[string]bool)
	for _, o := range orgs {
		names[o.Name] = true
	}
	for _, want := range []string{"alpha", "beta", "gamma"} {
		if !names[want] {
			t.Errorf("expected org %q in list", want)
		}
	}
}

// ---------------------------------------------------------------------------
// UpdateBranchDraft tests
// ---------------------------------------------------------------------------

func TestUpdateBranchDraft_SetAndUnset(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Create a repo and branch.
	if _, err := s.CreateRepo(ctx, model.CreateRepoRequest{Owner: "default", Name: "draftrepo"}); err != nil {
		t.Fatalf("create repo: %v", err)
	}
	if _, err := s.CreateBranch(ctx, model.CreateBranchRequest{
		Repo: "default/draftrepo",
		Name: "feature/draft-test",
	}); err != nil {
		t.Fatalf("create branch: %v", err)
	}

	// Set draft=true.
	if err := s.UpdateBranchDraft(ctx, "default/draftrepo", "feature/draft-test", true); err != nil {
		t.Fatalf("set draft=true: %v", err)
	}

	// Verify via a raw query.
	var draft bool
	if err := d.QueryRow(`SELECT draft FROM branches WHERE repo = $1 AND name = $2`,
		"default/draftrepo", "feature/draft-test").Scan(&draft); err != nil {
		t.Fatalf("query draft: %v", err)
	}
	if !draft {
		t.Error("expected draft=true after update")
	}

	// Unset draft.
	if err := s.UpdateBranchDraft(ctx, "default/draftrepo", "feature/draft-test", false); err != nil {
		t.Fatalf("set draft=false: %v", err)
	}
	if err := d.QueryRow(`SELECT draft FROM branches WHERE repo = $1 AND name = $2`,
		"default/draftrepo", "feature/draft-test").Scan(&draft); err != nil {
		t.Fatalf("query draft: %v", err)
	}
	if draft {
		t.Error("expected draft=false after unsetting")
	}
}

func TestUpdateBranchDraft_BranchNotFound(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	err := s.UpdateBranchDraft(ctx, "default/default", "no-such-branch", true)
	if !errors.Is(err, ErrBranchNotFound) {
		t.Errorf("expected ErrBranchNotFound, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// CommitSequenceExists tests
// ---------------------------------------------------------------------------

func TestCommitSequenceExists_Exists(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Create a commit to get a real sequence number.
	resp, err := s.Commit(ctx, model.CommitRequest{
		Repo:    "default/default",
		Branch:  "main",
		Files:   []model.FileChange{{Path: "seq-check.txt", Content: []byte("hello")}},
		Message: "seq check commit",
		Author:  "alice@example.com",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	exists, err := s.CommitSequenceExists(ctx, "default/default", resp.Sequence)
	if err != nil {
		t.Fatalf("CommitSequenceExists: %v", err)
	}
	if !exists {
		t.Errorf("expected sequence %d to exist", resp.Sequence)
	}
}

func TestCommitSequenceExists_NotExists(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	exists, err := s.CommitSequenceExists(ctx, "default/default", 999999)
	if err != nil {
		t.Fatalf("CommitSequenceExists: %v", err)
	}
	if exists {
		t.Error("expected sequence 999999 to not exist")
	}
}

// ---------------------------------------------------------------------------
// Event subscription store tests
// ---------------------------------------------------------------------------

func TestCreateSubscription_Success(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	config, _ := json.Marshal(map[string]string{"url": "https://example.com/hook"})
	req := model.CreateSubscriptionRequest{
		Backend:   "webhook",
		Config:    config,
		CreatedBy: "admin@example.com",
	}

	sub, err := s.CreateSubscription(ctx, req)
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	if sub.ID == "" {
		t.Error("expected non-empty ID")
	}
	if sub.Backend != "webhook" {
		t.Errorf("expected backend webhook, got %q", sub.Backend)
	}
	if sub.CreatedBy != "admin@example.com" {
		t.Errorf("expected created_by admin@example.com, got %q", sub.CreatedBy)
	}
}

func TestListSubscriptions_EmptyAndPopulated(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	// Initially empty (no subscriptions in this test's DB).
	subs, err := s.ListSubscriptions(ctx)
	if err != nil {
		t.Fatalf("list subscriptions (empty): %v", err)
	}
	initial := len(subs)

	// Create one.
	config, _ := json.Marshal(map[string]string{"url": "https://example.com/hook"})
	if _, err := s.CreateSubscription(ctx, model.CreateSubscriptionRequest{
		Backend:   "webhook",
		Config:    config,
		CreatedBy: "admin@example.com",
	}); err != nil {
		t.Fatalf("create subscription: %v", err)
	}

	subs, err = s.ListSubscriptions(ctx)
	if err != nil {
		t.Fatalf("list subscriptions: %v", err)
	}
	if len(subs) != initial+1 {
		t.Errorf("expected %d subscriptions, got %d", initial+1, len(subs))
	}
}

func TestDeleteSubscription_Success(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	config, _ := json.Marshal(map[string]string{"url": "https://example.com/hook"})
	sub, err := s.CreateSubscription(ctx, model.CreateSubscriptionRequest{
		Backend:   "webhook",
		Config:    config,
		CreatedBy: "admin@example.com",
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}

	if err := s.DeleteSubscription(ctx, sub.ID); err != nil {
		t.Fatalf("delete subscription: %v", err)
	}

	// Verify it's gone.
	subs, err := s.ListSubscriptions(ctx)
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	for _, existing := range subs {
		if existing.ID == sub.ID {
			t.Errorf("subscription %q still present after delete", sub.ID)
		}
	}
}

func TestDeleteSubscription_NotFound(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	err := s.DeleteSubscription(ctx, "00000000-0000-0000-0000-000000000000")
	if !errors.Is(err, ErrSubscriptionNotFound) {
		t.Errorf("expected ErrSubscriptionNotFound, got %v", err)
	}
}

func TestResumeSubscription_Success(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	config, _ := json.Marshal(map[string]string{"url": "https://example.com/hook"})
	sub, err := s.CreateSubscription(ctx, model.CreateSubscriptionRequest{
		Backend:   "webhook",
		Config:    config,
		CreatedBy: "admin@example.com",
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}

	// Manually suspend it.
	if _, err := d.ExecContext(ctx,
		`UPDATE event_subscriptions SET suspended_at = NOW(), failure_count = 5 WHERE id = $1`, sub.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	// Resume it.
	if err := s.ResumeSubscription(ctx, sub.ID); err != nil {
		t.Fatalf("resume subscription: %v", err)
	}

	// Verify suspended_at cleared.
	var failureCount int
	var suspendedAtNull bool
	if err := d.QueryRow(`SELECT failure_count, suspended_at IS NULL FROM event_subscriptions WHERE id = $1`, sub.ID).
		Scan(&failureCount, &suspendedAtNull); err != nil {
		t.Fatalf("query: %v", err)
	}
	if !suspendedAtNull {
		t.Error("expected suspended_at to be NULL after resume")
	}
	if failureCount != 0 {
		t.Errorf("expected failure_count 0 after resume, got %d", failureCount)
	}
}

func TestResumeSubscription_NotFound(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	err := s.ResumeSubscription(ctx, "00000000-0000-0000-0000-000000000000")
	if !errors.Is(err, ErrSubscriptionNotFound) {
		t.Errorf("expected ErrSubscriptionNotFound, got %v", err)
	}
}

func TestGetSubscription_Found(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	config, _ := json.Marshal(map[string]string{"url": "https://example.com/hook"})
	created, err := s.CreateSubscription(ctx, model.CreateSubscriptionRequest{
		Backend:   "webhook",
		Config:    config,
		CreatedBy: "admin@example.com",
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}

	got, err := s.GetSubscription(ctx, created.ID)
	if err != nil {
		t.Fatalf("get subscription: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("expected id %q, got %q", created.ID, got.ID)
	}
	if got.Backend != "webhook" {
		t.Errorf("expected backend webhook, got %q", got.Backend)
	}
	if got.CreatedBy != "admin@example.com" {
		t.Errorf("expected created_by admin@example.com, got %q", got.CreatedBy)
	}
}

func TestGetSubscription_NotFound(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	s := NewStore(d)
	ctx := context.Background()

	_, err := s.GetSubscription(ctx, "00000000-0000-0000-0000-000000000000")
	if !errors.Is(err, ErrSubscriptionNotFound) {
		t.Errorf("expected ErrSubscriptionNotFound, got %v", err)
	}
}
