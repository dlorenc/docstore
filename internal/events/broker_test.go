package events_test

import (
	"context"
	"testing"
	"time"

	dbpkg "github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/events"
	evtypes "github.com/dlorenc/docstore/internal/events/types"
	"github.com/dlorenc/docstore/internal/testutil"
)

func TestBroker_EmitAndPoll(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	b := events.NewBroker(d)

	e := evtypes.CommitCreated{
		Repo:     "acme/myrepo",
		Branch:   "main",
		Sequence: 1,
		Author:   "alice@example.com",
	}

	seq, err := b.CurrentSeq(context.Background())
	if err != nil {
		t.Fatalf("CurrentSeq: %v", err)
	}

	b.Emit(context.Background(), e)

	evs, err := b.Poll(context.Background(), "acme/myrepo", seq, nil)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(evs) == 0 {
		t.Fatal("expected at least one event after Emit")
	}
	if evs[0].Type != e.Type() {
		t.Errorf("expected type %q, got %q", e.Type(), evs[0].Type)
	}
	if evs[0].Repo != "acme/myrepo" {
		t.Errorf("expected repo %q, got %q", "acme/myrepo", evs[0].Repo)
	}
}

func TestBroker_PollFilterByRepo(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	b := events.NewBroker(d)

	seq, _ := b.CurrentSeq(context.Background())

	b.Emit(context.Background(), evtypes.CommitCreated{Repo: "acme/repo-a", Branch: "main"})
	b.Emit(context.Background(), evtypes.CommitCreated{Repo: "acme/repo-b", Branch: "main"})

	evs, err := b.Poll(context.Background(), "acme/repo-a", seq, nil)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	for _, ev := range evs {
		if ev.Repo != "acme/repo-a" {
			t.Errorf("expected repo acme/repo-a, got %q", ev.Repo)
		}
	}
	if len(evs) == 0 {
		t.Fatal("expected at least one event for acme/repo-a")
	}
}

func TestBroker_PollWildcardRepo(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	b := events.NewBroker(d)

	seq, _ := b.CurrentSeq(context.Background())

	b.Emit(context.Background(), evtypes.CommitCreated{Repo: "acme/repo-x", Branch: "main"})
	b.Emit(context.Background(), evtypes.CommitCreated{Repo: "acme/repo-y", Branch: "main"})

	evs, err := b.Poll(context.Background(), "*", seq, nil)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(evs) < 2 {
		t.Errorf("expected at least 2 events for wildcard poll, got %d", len(evs))
	}
}

func TestBroker_PollFilterByType(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	b := events.NewBroker(d)

	seq, _ := b.CurrentSeq(context.Background())

	// Emit a non-matching event first.
	b.Emit(context.Background(), evtypes.BranchCreated{Repo: "acme/myrepo", Branch: "feature"})
	// Emit a matching event.
	b.Emit(context.Background(), evtypes.CommitCreated{Repo: "acme/myrepo", Branch: "main", Sequence: 42})

	evs, err := b.Poll(context.Background(), "acme/myrepo", seq, []string{"com.docstore.commit.created"})
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	for _, ev := range evs {
		if ev.Type != "com.docstore.commit.created" {
			t.Errorf("expected only commit.created events, got %q", ev.Type)
		}
	}
	if len(evs) == 0 {
		t.Fatal("expected at least one commit.created event")
	}
}

func TestBroker_WaitForEvent(t *testing.T) {
	t.Parallel()
	b := events.NewBroker(nil) // no DB needed for in-memory signalling

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		done <- b.WaitForEvent(ctx)
	}()

	time.Sleep(10 * time.Millisecond)
	b.Notify()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected nil error after Notify, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("WaitForEvent did not return after Notify")
	}
}

func TestBroker_WaitForEventTimeout(t *testing.T) {
	t.Parallel()
	b := events.NewBroker(nil)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := b.WaitForEvent(ctx)
	if err == nil {
		t.Error("expected timeout error, got nil")
	}
}

func TestBroker_CurrentSeq(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	b := events.NewBroker(d)

	seq0, err := b.CurrentSeq(context.Background())
	if err != nil {
		t.Fatalf("CurrentSeq: %v", err)
	}

	b.Emit(context.Background(), evtypes.CommitCreated{Repo: "acme/myrepo", Branch: "main"})

	seq1, err := b.CurrentSeq(context.Background())
	if err != nil {
		t.Fatalf("CurrentSeq after Emit: %v", err)
	}
	if seq1 <= seq0 {
		t.Errorf("expected seq to increase after Emit: before=%d after=%d", seq0, seq1)
	}
}

func TestCloudEvents_Schema(t *testing.T) {
	t.Parallel()

	eventTypes := []events.Event{
		evtypes.CommitCreated{Repo: "acme/r", Branch: "main", Sequence: 1, Author: "a", FileCount: 1},
		evtypes.BranchCreated{Repo: "acme/r", Branch: "b", BaseSequence: 0, CreatedBy: "a"},
		evtypes.BranchMerged{Repo: "acme/r", Branch: "b", Sequence: 2, MergedBy: "a"},
		evtypes.BranchRebased{Repo: "acme/r", Branch: "b"},
		evtypes.BranchAbandoned{Repo: "acme/r", Branch: "b", AbandonedBy: "a"},
		evtypes.ReviewSubmitted{Repo: "acme/r", Branch: "b", Reviewer: "a", Status: "approved"},
		evtypes.CheckReported{Repo: "acme/r", Branch: "b", CheckName: "ci", Status: "passed", Reporter: "bot"},
		evtypes.MergeBlocked{Repo: "acme/r", Branch: "b", Actor: "a"},
		evtypes.RoleChanged{Repo: "acme/r", Identity: "a@example.com", Role: "admin", ChangedBy: "b"},
		evtypes.OrgCreated{Org: "acme", CreatedBy: "a"},
		evtypes.OrgDeleted{Org: "acme", DeletedBy: "a"},
		evtypes.RepoCreated{Repo: "acme/r", Owner: "acme", CreatedBy: "a"},
		evtypes.RepoDeleted{Repo: "acme/r", DeletedBy: "a"},
	}

	for _, e := range eventTypes {
		t.Run(e.Type(), func(t *testing.T) {
			data, err := events.ToCloudEvent(e)
			if err != nil {
				t.Fatalf("ToCloudEvent failed: %v", err)
			}
			if len(data) == 0 {
				t.Fatal("empty CloudEvent JSON")
			}
			// Basic sanity: contains required fields.
			s := string(data)
			for _, field := range []string{`"specversion":"1.0"`, `"type":"` + e.Type() + `"`, `"source":"` + e.Source() + `"`} {
				if !contains(s, field) {
					t.Errorf("missing field %q in CloudEvent: %s", field, s)
				}
			}
		})
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
