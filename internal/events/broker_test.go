package events_test

import (
	"context"
	"testing"
	"time"

	"github.com/dlorenc/docstore/internal/events"
	evtypes "github.com/dlorenc/docstore/internal/events/types"
)

func TestBroker_PublishAndReceive(t *testing.T) {
	t.Parallel()
	b := events.NewBroker(nil)

	ch, unsub := b.Subscribe("acme/myrepo", nil)
	defer unsub()

	e := evtypes.CommitCreated{
		Repo:     "acme/myrepo",
		Branch:   "main",
		Sequence: 1,
		Author:   "alice@example.com",
	}
	b.Emit(context.Background(), e)

	select {
	case got := <-ch:
		if got.Type() != e.Type() {
			t.Errorf("expected type %q, got %q", e.Type(), got.Type())
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestBroker_ClientDisconnect(t *testing.T) {
	t.Parallel()
	b := events.NewBroker(nil)

	ch, unsub := b.Subscribe("acme/myrepo", nil)

	// Unsubscribe before any events.
	unsub()

	// Emitting after unsubscribe should not panic or block.
	b.Emit(context.Background(), evtypes.CommitCreated{
		Repo:   "acme/myrepo",
		Branch: "main",
	})

	// Channel should be closed.
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("expected channel to be closed")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out: channel not closed after unsub")
	}
}

func TestBroker_FilterByType(t *testing.T) {
	t.Parallel()
	b := events.NewBroker(nil)

	// Subscribe to only commit.created events.
	ch, unsub := b.Subscribe("acme/myrepo", []string{"com.docstore.commit.created"})
	defer unsub()

	// Emit a non-matching event first.
	b.Emit(context.Background(), evtypes.BranchCreated{
		Repo:   "acme/myrepo",
		Branch: "feature",
	})

	// Emit a matching event.
	b.Emit(context.Background(), evtypes.CommitCreated{
		Repo:     "acme/myrepo",
		Branch:   "main",
		Sequence: 42,
	})

	select {
	case got := <-ch:
		if got.Type() != "com.docstore.commit.created" {
			t.Errorf("expected commit.created, got %q", got.Type())
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for filtered event")
	}

	// Channel should have no more events.
	select {
	case unexpected := <-ch:
		t.Errorf("received unexpected event: %s", unexpected.Type())
	default:
		// Good: no extra events.
	}
}

func TestBroker_MultipleSubscribers(t *testing.T) {
	t.Parallel()
	b := events.NewBroker(nil)

	ch1, unsub1 := b.Subscribe("acme/myrepo", nil)
	defer unsub1()
	ch2, unsub2 := b.Subscribe("acme/myrepo", nil)
	defer unsub2()

	e := evtypes.CommitCreated{Repo: "acme/myrepo", Branch: "main"}
	b.Emit(context.Background(), e)

	for _, ch := range []<-chan events.Event{ch1, ch2} {
		select {
		case got := <-ch:
			if got.Type() != e.Type() {
				t.Errorf("expected %q, got %q", e.Type(), got.Type())
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for event on subscriber")
		}
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
