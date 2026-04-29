package db

import (
	"context"
	"testing"
	"time"
)

func TestStoreAndLookupRequestToken(t *testing.T) {
	t.Parallel()
	s, _ := newCIJobStore(t)
	ctx := context.Background()

	// Insert a job and claim it so its status becomes 'claimed'.
	inserted, err := s.InsertCIJob(ctx, "org/repo", "feat", 5, "push", "feat", "", "")
	if err != nil {
		t.Fatalf("InsertCIJob: %v", err)
	}
	claimed, err := s.ClaimCIJob(ctx, "pod-a", "1.1.1.1")
	if err != nil {
		t.Fatalf("ClaimCIJob: %v", err)
	}
	if claimed == nil {
		t.Fatal("expected job to be claimed")
	}

	hashedToken := "sha256-abc123"
	exp := time.Now().Add(10 * time.Minute)

	if err := s.StoreRequestToken(ctx, inserted.ID, hashedToken, exp); err != nil {
		t.Fatalf("StoreRequestToken: %v", err)
	}

	// LookupRequestToken should return the job.
	got, err := s.LookupRequestToken(ctx, hashedToken)
	if err != nil {
		t.Fatalf("LookupRequestToken: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil job")
	}
	if got.ID != inserted.ID {
		t.Errorf("job ID = %q, want %q", got.ID, inserted.ID)
	}
}

func TestLookupRequestToken_NotFound(t *testing.T) {
	t.Parallel()
	s, _ := newCIJobStore(t)
	ctx := context.Background()

	_, err := s.LookupRequestToken(ctx, "no-such-token")
	if err != ErrTokenInvalid {
		t.Errorf("LookupRequestToken error = %v, want ErrTokenInvalid", err)
	}
}

func TestLookupRequestToken_Expired(t *testing.T) {
	t.Parallel()
	s, _ := newCIJobStore(t)
	ctx := context.Background()

	inserted, err := s.InsertCIJob(ctx, "org/repo", "feat", 5, "push", "feat", "", "")
	if err != nil {
		t.Fatalf("InsertCIJob: %v", err)
	}
	if _, err := s.ClaimCIJob(ctx, "pod-a", "1.1.1.1"); err != nil {
		t.Fatalf("ClaimCIJob: %v", err)
	}

	// Store a token that has already expired.
	exp := time.Now().Add(-1 * time.Minute)
	if err := s.StoreRequestToken(ctx, inserted.ID, "expired-token", exp); err != nil {
		t.Fatalf("StoreRequestToken: %v", err)
	}

	_, err = s.LookupRequestToken(ctx, "expired-token")
	if err != ErrTokenInvalid {
		t.Errorf("LookupRequestToken error = %v, want ErrTokenInvalid", err)
	}
}

func TestLookupRequestToken_WrongStatus(t *testing.T) {
	t.Parallel()
	s, _ := newCIJobStore(t)
	ctx := context.Background()

	// Insert a job but do NOT claim it — it stays 'queued'.
	inserted, err := s.InsertCIJob(ctx, "org/repo", "feat", 5, "push", "feat", "", "")
	if err != nil {
		t.Fatalf("InsertCIJob: %v", err)
	}

	exp := time.Now().Add(10 * time.Minute)
	if err := s.StoreRequestToken(ctx, inserted.ID, "queued-job-token", exp); err != nil {
		t.Fatalf("StoreRequestToken: %v", err)
	}

	_, err = s.LookupRequestToken(ctx, "queued-job-token")
	if err != ErrTokenInvalid {
		t.Errorf("LookupRequestToken on queued job = %v, want ErrTokenInvalid", err)
	}
}
