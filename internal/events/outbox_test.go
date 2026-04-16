package events_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	dbpkg "github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/events"
	evtypes "github.com/dlorenc/docstore/internal/events/types"
	"github.com/dlorenc/docstore/internal/testutil"
	"github.com/lib/pq"
)

// insertWebhookSub inserts a webhook subscription and returns its UUID.
func insertWebhookSub(t *testing.T, d *sql.DB, webhookURL, secret string) string {
	t.Helper()
	config, _ := json.Marshal(map[string]string{"url": webhookURL, "secret": secret})
	var subID string
	err := d.QueryRowContext(context.Background(), `
		INSERT INTO event_subscriptions (backend, config, created_by)
		VALUES ('webhook', $1, 'test')
		RETURNING id`, config,
	).Scan(&subID)
	if err != nil {
		t.Fatalf("insert subscription: %v", err)
	}
	return subID
}

func TestOutbox_InsertAndDeliver(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)

	// Start a test webhook server that counts calls.
	var callCount int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&callCount, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	subID := insertWebhookSub(t, d, srv.URL, "test-secret")

	// Insert an outbox event for this subscription.
	e := evtypes.CommitCreated{Repo: "default/default", Branch: "main", Sequence: 1, Author: "alice"}
	eventJSON, err := events.ToCloudEvent(e)
	if err != nil {
		t.Fatalf("serialize event: %v", err)
	}
	if err := events.InsertOutboxEvent(context.Background(), d, eventJSON, subID); err != nil {
		t.Fatalf("insert outbox: %v", err)
	}

	// Process one batch synchronously.
	events.ProcessOnce(context.Background(), d)

	var deliveredAt *time.Time
	d.QueryRowContext(context.Background(),
		`SELECT delivered_at FROM event_outbox WHERE subscription_id = $1`, subID,
	).Scan(&deliveredAt)
	if deliveredAt == nil {
		t.Fatal("outbox row not marked as delivered")
	}
	if atomic.LoadInt64(&callCount) == 0 {
		t.Fatal("webhook was never called")
	}
}

func TestOutbox_RetryOnFailure(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)

	// Failing webhook server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	subID := insertWebhookSub(t, d, srv.URL, "")

	e := evtypes.CommitCreated{Repo: "default/default", Branch: "main"}
	eventJSON, _ := events.ToCloudEvent(e)
	events.InsertOutboxEvent(context.Background(), d, eventJSON, subID)

	// Process one batch synchronously.
	ctx := context.Background()
	events.ProcessOnce(ctx, d)

	var attempts int
	d.QueryRowContext(context.Background(),
		`SELECT attempts FROM event_outbox WHERE subscription_id = $1`, subID,
	).Scan(&attempts)
	if attempts == 0 {
		t.Error("expected attempts > 0 after failure")
	}
}

func TestOutbox_MaxAttemptsAutoSuspends(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	subID := insertWebhookSub(t, d, srv.URL, "")

	e := evtypes.CommitCreated{Repo: "default/default", Branch: "main"}
	eventJSON, _ := events.ToCloudEvent(e)
	events.InsertOutboxEvent(context.Background(), d, eventJSON, subID)

	// Pre-set attempts=9 so next failure triggers suspension.
	d.ExecContext(context.Background(), `
		UPDATE event_outbox SET attempts = 9, next_attempt = now() - interval '1 second'
		WHERE subscription_id = $1`, subID)

	// Process one batch synchronously.
	events.ProcessOnce(context.Background(), d)

	var suspendedAt *time.Time
	d.QueryRowContext(context.Background(),
		`SELECT suspended_at FROM event_subscriptions WHERE id = $1`, subID,
	).Scan(&suspendedAt)
	if suspendedAt == nil {
		t.Error("expected subscription to be suspended after 10 failures")
	}
}

func TestOutbox_Cleanup(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)

	subID := insertWebhookSub(t, d, "http://localhost:9999", "")

	// Insert an old delivered row (> 7 days ago).
	d.ExecContext(context.Background(), `
		INSERT INTO event_outbox (event, subscription_id, delivered_at, attempts)
		VALUES ('{}', $1, now() - interval '8 days', 1)`, subID)

	// Insert a recent delivered row.
	d.ExecContext(context.Background(), `
		INSERT INTO event_outbox (event, subscription_id, delivered_at, attempts)
		VALUES ('{}', $1, now() - interval '1 day', 1)`, subID)

	// Both should exist before cleanup.
	var countBefore int
	d.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM event_outbox WHERE subscription_id = $1`, subID).Scan(&countBefore)
	if countBefore != 2 {
		t.Fatalf("expected 2 rows before cleanup, got %d", countBefore)
	}

	// Directly invoke cleanup via the exported helper.
	if err := events.RunCleanupOnce(context.Background(), d); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	// Old row should be deleted.
	var countAfter int
	d.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM event_outbox WHERE subscription_id = $1`, subID).Scan(&countAfter)
	if countAfter != 1 {
		t.Errorf("expected 1 row after cleanup, got %d", countAfter)
	}
}

// ensure pq is used
var _ = pq.Array
