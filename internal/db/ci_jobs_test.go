package db

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/dlorenc/docstore/internal/testutil"
)

func newCIJobStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	return NewStore(d), d
}

func TestInsertCIJob(t *testing.T) {
	t.Parallel()
	s, _ := newCIJobStore(t)
	ctx := context.Background()

	j, err := s.InsertCIJob(ctx, "org/repo", "main", 1, "push", "main", "", "")
	if err != nil {
		t.Fatalf("InsertCIJob: %v", err)
	}
	if j.ID == "" {
		t.Error("expected non-empty ID")
	}
	if j.Repo != "org/repo" {
		t.Errorf("Repo = %q, want %q", j.Repo, "org/repo")
	}
	if j.Branch != "main" {
		t.Errorf("Branch = %q, want %q", j.Branch, "main")
	}
	if j.Sequence != 1 {
		t.Errorf("Sequence = %d, want 1", j.Sequence)
	}
	if j.Status != "queued" {
		t.Errorf("Status = %q, want %q", j.Status, "queued")
	}
	if j.TriggerType != "push" {
		t.Errorf("TriggerType = %q, want %q", j.TriggerType, "push")
	}
	if j.TriggerBranch != "main" {
		t.Errorf("TriggerBranch = %q, want %q", j.TriggerBranch, "main")
	}
	if j.TriggerBaseBranch != "" {
		t.Errorf("TriggerBaseBranch = %q, want empty", j.TriggerBaseBranch)
	}
	if j.TriggerProposalID != nil {
		t.Errorf("TriggerProposalID = %v, want nil", j.TriggerProposalID)
	}
	if j.ClaimedAt != nil {
		t.Errorf("ClaimedAt should be nil for new job")
	}
}

func TestInsertCIJob_ProposalTrigger(t *testing.T) {
	t.Parallel()
	s, _ := newCIJobStore(t)
	ctx := context.Background()

	proposalID := "prop-42"
	j, err := s.InsertCIJob(ctx, "org/repo", "feat", 3, "proposal", "feat", "main", proposalID)
	if err != nil {
		t.Fatalf("InsertCIJob: %v", err)
	}
	if j.TriggerBaseBranch != "main" {
		t.Errorf("TriggerBaseBranch = %q, want %q", j.TriggerBaseBranch, "main")
	}
	if j.TriggerProposalID == nil || *j.TriggerProposalID != proposalID {
		t.Errorf("TriggerProposalID = %v, want %q", j.TriggerProposalID, proposalID)
	}
}

func TestGetCIJob(t *testing.T) {
	t.Parallel()
	s, _ := newCIJobStore(t)
	ctx := context.Background()

	inserted, err := s.InsertCIJob(ctx, "org/repo", "main", 1, "push", "main", "", "")
	if err != nil {
		t.Fatalf("InsertCIJob: %v", err)
	}

	got, err := s.GetCIJob(ctx, inserted.ID)
	if err != nil {
		t.Fatalf("GetCIJob: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil job")
	}
	if got.ID != inserted.ID {
		t.Errorf("ID = %q, want %q", got.ID, inserted.ID)
	}
	if got.Status != "queued" {
		t.Errorf("Status = %q, want queued", got.Status)
	}
}

func TestGetCIJob_NotFound(t *testing.T) {
	t.Parallel()
	s, _ := newCIJobStore(t)
	ctx := context.Background()

	_, err := s.GetCIJob(ctx, "00000000-0000-0000-0000-000000000000")
	if !errors.Is(err, ErrCIJobNotFound) {
		t.Fatalf("expected ErrCIJobNotFound, got %v", err)
	}
}

func TestClaimCIJob(t *testing.T) {
	t.Parallel()
	s, _ := newCIJobStore(t)
	ctx := context.Background()

	inserted, err := s.InsertCIJob(ctx, "org/repo", "main", 1, "push", "main", "", "")
	if err != nil {
		t.Fatalf("InsertCIJob: %v", err)
	}

	claimed, err := s.ClaimCIJob(ctx, "worker-pod-1", "10.0.0.1")
	if err != nil {
		t.Fatalf("ClaimCIJob: %v", err)
	}
	if claimed == nil {
		t.Fatal("expected a claimed job, got nil")
	}
	if claimed.ID != inserted.ID {
		t.Errorf("claimed ID = %q, want %q", claimed.ID, inserted.ID)
	}
	if claimed.Status != "claimed" {
		t.Errorf("Status = %q, want claimed", claimed.Status)
	}
	if claimed.ClaimedAt == nil {
		t.Error("ClaimedAt should be set after claiming")
	}
	if claimed.WorkerPod == nil || *claimed.WorkerPod != "worker-pod-1" {
		t.Errorf("WorkerPod = %v, want worker-pod-1", claimed.WorkerPod)
	}
	if claimed.WorkerPodIP == nil || *claimed.WorkerPodIP != "10.0.0.1" {
		t.Errorf("WorkerPodIP = %v, want 10.0.0.1", claimed.WorkerPodIP)
	}
}

func TestClaimCIJob_NoQueued(t *testing.T) {
	t.Parallel()
	s, _ := newCIJobStore(t)
	ctx := context.Background()

	// No jobs inserted — claim should return nil, nil.
	got, err := s.ClaimCIJob(ctx, "worker-pod-1", "10.0.0.1")
	if err != nil {
		t.Fatalf("ClaimCIJob: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil when no queued jobs, got %+v", got)
	}
}

// TestClaimCIJob_SkipLocked verifies the FOR UPDATE SKIP LOCKED behaviour: a
// second concurrent caller should not claim the same job.
func TestClaimCIJob_SkipLocked(t *testing.T) {
	t.Parallel()
	s, _ := newCIJobStore(t)
	ctx := context.Background()

	if _, err := s.InsertCIJob(ctx, "org/repo", "main", 1, "push", "main", "", ""); err != nil {
		t.Fatalf("InsertCIJob: %v", err)
	}

	first, err := s.ClaimCIJob(ctx, "pod-a", "1.1.1.1")
	if err != nil {
		t.Fatalf("first ClaimCIJob: %v", err)
	}
	if first == nil {
		t.Fatal("expected first claim to succeed")
	}

	// A second claim on the same store should find no more queued jobs.
	second, err := s.ClaimCIJob(ctx, "pod-b", "2.2.2.2")
	if err != nil {
		t.Fatalf("second ClaimCIJob: %v", err)
	}
	if second != nil {
		t.Errorf("expected second claim to return nil, got job %q with status %q", second.ID, second.Status)
	}
}

func TestHeartbeatCIJob(t *testing.T) {
	t.Parallel()
	s, d := newCIJobStore(t)
	ctx := context.Background()

	inserted, err := s.InsertCIJob(ctx, "org/repo", "main", 1, "push", "main", "", "")
	if err != nil {
		t.Fatalf("InsertCIJob: %v", err)
	}

	if err := s.HeartbeatCIJob(ctx, inserted.ID); err != nil {
		t.Fatalf("HeartbeatCIJob: %v", err)
	}

	// Verify last_heartbeat_at is now set in the DB.
	var hb sql.NullTime
	err = d.QueryRowContext(ctx,
		"SELECT last_heartbeat_at FROM ci_jobs WHERE id = $1", inserted.ID,
	).Scan(&hb)
	if err != nil {
		t.Fatalf("query heartbeat: %v", err)
	}
	if !hb.Valid {
		t.Error("last_heartbeat_at should be non-null after heartbeat")
	}
	if time.Since(hb.Time) > 10*time.Second {
		t.Errorf("last_heartbeat_at looks stale: %v", hb.Time)
	}
}

func TestCompleteCIJob_Passed(t *testing.T) {
	t.Parallel()
	s, _ := newCIJobStore(t)
	ctx := context.Background()

	inserted, err := s.InsertCIJob(ctx, "org/repo", "main", 1, "push", "main", "", "")
	if err != nil {
		t.Fatalf("InsertCIJob: %v", err)
	}

	logURL := "https://logs.example.com/build/1"
	if err := s.CompleteCIJob(ctx, inserted.ID, "passed", &logURL, nil); err != nil {
		t.Fatalf("CompleteCIJob: %v", err)
	}

	got, err := s.GetCIJob(ctx, inserted.ID)
	if err != nil {
		t.Fatalf("GetCIJob: %v", err)
	}
	if got.Status != "passed" {
		t.Errorf("Status = %q, want passed", got.Status)
	}
	if got.LogURL == nil || *got.LogURL != logURL {
		t.Errorf("LogURL = %v, want %q", got.LogURL, logURL)
	}
	if got.ErrorMessage != nil {
		t.Errorf("ErrorMessage = %v, want nil", got.ErrorMessage)
	}
}

func TestCompleteCIJob_Failed(t *testing.T) {
	t.Parallel()
	s, _ := newCIJobStore(t)
	ctx := context.Background()

	inserted, err := s.InsertCIJob(ctx, "org/repo", "main", 1, "push", "main", "", "")
	if err != nil {
		t.Fatalf("InsertCIJob: %v", err)
	}

	logURL := "https://logs.example.com/build/2"
	errMsg := "exit status 1"
	if err := s.CompleteCIJob(ctx, inserted.ID, "failed", &logURL, &errMsg); err != nil {
		t.Fatalf("CompleteCIJob: %v", err)
	}

	got, err := s.GetCIJob(ctx, inserted.ID)
	if err != nil {
		t.Fatalf("GetCIJob: %v", err)
	}
	if got.Status != "failed" {
		t.Errorf("Status = %q, want failed", got.Status)
	}
	if got.ErrorMessage == nil || *got.ErrorMessage != errMsg {
		t.Errorf("ErrorMessage = %v, want %q", got.ErrorMessage, errMsg)
	}
}

func TestReapStaleCIJobs(t *testing.T) {
	t.Parallel()
	s, d := newCIJobStore(t)
	ctx := context.Background()

	// Insert a job and claim it.
	inserted, err := s.InsertCIJob(ctx, "org/repo", "main", 1, "push", "main", "", "")
	if err != nil {
		t.Fatalf("InsertCIJob: %v", err)
	}
	if _, err := s.ClaimCIJob(ctx, "pod-a", "1.1.1.1"); err != nil {
		t.Fatalf("ClaimCIJob: %v", err)
	}

	// Artificially age claimed_at so the reaper considers it stale.
	_, err = d.ExecContext(ctx,
		`UPDATE ci_jobs SET claimed_at = now() - interval '5 minutes', last_heartbeat_at = NULL WHERE id = $1`,
		inserted.ID,
	)
	if err != nil {
		t.Fatalf("age claimed_at: %v", err)
	}

	reaped, err := s.ReapStaleCIJobs(ctx)
	if err != nil {
		t.Fatalf("ReapStaleCIJobs: %v", err)
	}
	if len(reaped) != 1 {
		t.Fatalf("expected 1 reaped job, got %d", len(reaped))
	}
	if reaped[0].ID != inserted.ID {
		t.Errorf("reaped ID = %q, want %q", reaped[0].ID, inserted.ID)
	}
	if reaped[0].Status != "queued" {
		t.Errorf("reaped Status = %q, want queued", reaped[0].Status)
	}
	if reaped[0].ClaimedAt != nil {
		t.Error("ClaimedAt should be nil after reaping")
	}
	if reaped[0].WorkerPod != nil {
		t.Error("WorkerPod should be nil after reaping")
	}

	// Verify the job is now claimable again.
	reclaimed, err := s.ClaimCIJob(ctx, "pod-b", "2.2.2.2")
	if err != nil {
		t.Fatalf("re-ClaimCIJob: %v", err)
	}
	if reclaimed == nil {
		t.Error("expected job to be re-claimable after reaping")
	}
}

func TestReapStaleCIJobs_FreshJobNotReaped(t *testing.T) {
	t.Parallel()
	s, _ := newCIJobStore(t)
	ctx := context.Background()

	// Claim a fresh job (heartbeat just now via claimed_at).
	if _, err := s.InsertCIJob(ctx, "org/repo", "main", 1, "push", "main", "", ""); err != nil {
		t.Fatalf("InsertCIJob: %v", err)
	}
	if _, err := s.ClaimCIJob(ctx, "pod-a", "1.1.1.1"); err != nil {
		t.Fatalf("ClaimCIJob: %v", err)
	}

	reaped, err := s.ReapStaleCIJobs(ctx)
	if err != nil {
		t.Fatalf("ReapStaleCIJobs: %v", err)
	}
	if len(reaped) != 0 {
		t.Errorf("expected 0 reaped jobs for fresh claim, got %d", len(reaped))
	}
}

func TestReapStaleCIJobs_HeartbeatKeepsJobAlive(t *testing.T) {
	t.Parallel()
	s, d := newCIJobStore(t)
	ctx := context.Background()

	inserted, err := s.InsertCIJob(ctx, "org/repo", "main", 1, "push", "main", "", "")
	if err != nil {
		t.Fatalf("InsertCIJob: %v", err)
	}
	if _, err := s.ClaimCIJob(ctx, "pod-a", "1.1.1.1"); err != nil {
		t.Fatalf("ClaimCIJob: %v", err)
	}

	// Age claimed_at so reaper would normally pick it up...
	if _, err := d.ExecContext(ctx,
		`UPDATE ci_jobs SET claimed_at = now() - interval '5 minutes' WHERE id = $1`,
		inserted.ID,
	); err != nil {
		t.Fatalf("age claimed_at: %v", err)
	}

	// ...but send a fresh heartbeat.
	if err := s.HeartbeatCIJob(ctx, inserted.ID); err != nil {
		t.Fatalf("HeartbeatCIJob: %v", err)
	}

	reaped, err := s.ReapStaleCIJobs(ctx)
	if err != nil {
		t.Fatalf("ReapStaleCIJobs: %v", err)
	}
	if len(reaped) != 0 {
		t.Errorf("expected 0 reaped jobs when heartbeat is fresh, got %d", len(reaped))
	}
}
