package events

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/lib/pq"
)

// outboxRow is a single pending delivery row fetched from event_outbox.
type outboxRow struct {
	ID             string
	Event          []byte
	SubscriptionID string
	Attempts       int
	WebhookURL     string
	WebhookSecret  string
}

// StartDispatcher starts the outbox dispatcher goroutine. It polls for pending
// outbox rows every 5 seconds, delivers webhooks with exponential backoff, and
// cleans up delivered rows older than 7 days every hour.
// The goroutine stops when ctx is cancelled (e.g. on SIGTERM).
func StartDispatcher(ctx context.Context, db *sql.DB) {
	go func() {
		pollTicker := time.NewTicker(5 * time.Second)
		cleanupTicker := time.NewTicker(time.Hour)
		defer pollTicker.Stop()
		defer cleanupTicker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-pollTicker.C:
				processOutboxBatch(ctx, db)
			case <-cleanupTicker.C:
				if err := cleanupOutbox(ctx, db); err != nil {
					slog.Error("outbox: cleanup failed", "error", err)
				}
			}
		}
	}()
}

// processOutboxBatch claims and delivers one batch of pending outbox rows.
func processOutboxBatch(ctx context.Context, db *sql.DB) {
	rows, err := claimOutboxBatch(ctx, db, 50)
	if err != nil {
		slog.Error("outbox: claim batch failed", "error", err)
		return
	}
	for _, row := range rows {
		deliverWebhook(ctx, db, row)
	}
}

// claimOutboxBatch selects up to batchSize pending outbox rows FOR UPDATE SKIP LOCKED,
// joining with event_subscriptions for the webhook config.
func claimOutboxBatch(ctx context.Context, db *sql.DB, batchSize int) ([]outboxRow, error) {
	const q = `
		SELECT o.id, o.event, o.subscription_id, o.attempts,
		       s.config->>'url'    AS webhook_url,
		       s.config->>'secret' AS webhook_secret
		FROM event_outbox o
		JOIN event_subscriptions s ON s.id = o.subscription_id
		WHERE o.delivered_at IS NULL
		  AND o.next_attempt <= now()
		  AND o.attempts < 10
		  AND s.suspended_at IS NULL
		  AND s.backend = 'webhook'
		ORDER BY o.next_attempt
		LIMIT $1
		FOR UPDATE OF o SKIP LOCKED`

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	sqlRows, err := tx.QueryContext(ctx, q, batchSize)
	if err != nil {
		return nil, fmt.Errorf("query outbox: %w", err)
	}
	defer sqlRows.Close()

	var batch []outboxRow
	var ids []string
	for sqlRows.Next() {
		var r outboxRow
		var webhookURL, webhookSecret sql.NullString
		if err := sqlRows.Scan(&r.ID, &r.Event, &r.SubscriptionID,
			&r.Attempts, &webhookURL, &webhookSecret); err != nil {
			return nil, fmt.Errorf("scan outbox row: %w", err)
		}
		r.WebhookURL = webhookURL.String
		r.WebhookSecret = webhookSecret.String
		batch = append(batch, r)
		ids = append(ids, r.ID)
	}
	if err := sqlRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate outbox rows: %w", err)
	}

	// Mark claimed rows as in-flight within the same transaction so a slow
	// delivery cannot be double-processed by a concurrent poller.
	if len(ids) > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE event_outbox SET next_attempt = now() + interval '5 minutes'
			 WHERE id = ANY($1)`,
			pq.Array(ids),
		); err != nil {
			return nil, fmt.Errorf("mark in-flight: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return batch, nil
}

// deliverWebhook attempts to deliver one outbox row via HTTP POST.
func deliverWebhook(ctx context.Context, db *sql.DB, row outboxRow) {
	if row.WebhookURL == "" {
		slog.Warn("outbox: webhook URL is empty, skipping", "id", row.ID)
		return
	}

	client := &http.Client{Timeout: 10 * time.Second}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, row.WebhookURL,
		strings.NewReader(string(row.Event)))
	if err != nil {
		markFailed(ctx, db, row, fmt.Sprintf("build request: %v", err))
		return
	}
	req.Header.Set("Content-Type", "application/cloudevents+json")
	if row.WebhookSecret != "" {
		sig := computeHMAC(row.Event, row.WebhookSecret)
		req.Header.Set("X-DocStore-Signature", "sha256="+sig)
	}

	resp, err := client.Do(req)
	if err != nil {
		markFailed(ctx, db, row, fmt.Sprintf("http: %v", err))
		return
	}
	resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		markFailed(ctx, db, row, fmt.Sprintf("webhook returned status %d", resp.StatusCode))
		return
	}

	// Success.
	if _, err := db.ExecContext(ctx,
		`UPDATE event_outbox SET delivered_at = now() WHERE id = $1`, row.ID,
	); err != nil {
		slog.Error("outbox: mark delivered failed", "id", row.ID, "error", err)
	}
}

// markFailed increments attempts, sets next_attempt using exponential backoff,
// and suspends the subscription when attempts reach 10.
func markFailed(ctx context.Context, db *sql.DB, row outboxRow, lastErr string) {
	newAttempts := row.Attempts + 1
	backoff := time.Duration(1<<uint(row.Attempts)) * time.Second
	if backoff > time.Hour {
		backoff = time.Hour
	}
	nextAttempt := time.Now().Add(backoff)

	if _, err := db.ExecContext(ctx, `
		UPDATE event_outbox
		SET attempts = $1, next_attempt = $2, last_error = $3
		WHERE id = $4`,
		newAttempts, nextAttempt, lastErr, row.ID,
	); err != nil {
		slog.Error("outbox: mark failed update", "id", row.ID, "error", err)
	}

	if newAttempts >= 10 {
		slog.Warn("outbox: max attempts reached, suspending subscription",
			"subscription_id", row.SubscriptionID, "last_error", lastErr)
		if _, err := db.ExecContext(ctx, `
			UPDATE event_subscriptions
			SET suspended_at = now(), failure_count = failure_count + 1
			WHERE id = $1`,
			row.SubscriptionID,
		); err != nil {
			slog.Error("outbox: suspend subscription failed",
				"subscription_id", row.SubscriptionID, "error", err)
		}
	}
}

// cleanupOutbox deletes outbox rows that have been delivered more than 7 days ago.
func cleanupOutbox(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		DELETE FROM event_outbox
		WHERE delivered_at < now() - interval '7 days'`)
	return err
}

// RunCleanupOnce runs a single outbox cleanup pass. Exported for testing.
func RunCleanupOnce(ctx context.Context, db *sql.DB) error {
	return cleanupOutbox(ctx, db)
}

// ProcessOnce runs a single outbox delivery pass synchronously. Exported for testing.
func ProcessOnce(ctx context.Context, db *sql.DB) {
	processOutboxBatch(ctx, db)
}

// insertOutboxForEvent finds all webhook subscriptions matching the event and
// inserts an outbox row for each one.
func insertOutboxForEvent(ctx context.Context, db *sql.DB, e Event) error {
	// Serialize the event as a CloudEvents JSON envelope.
	eventJSON, err := ToCloudEvent(e)
	if err != nil {
		return fmt.Errorf("serialize event: %w", err)
	}

	// Extract repo name from source (nil-safe: org events have no matching repo subscription).
	repo := repoFromSource(e.Source())

	// Find matching subscriptions.
	const q = `
		SELECT id FROM event_subscriptions
		WHERE backend = 'webhook'
		  AND suspended_at IS NULL
		  AND (repo IS NULL OR repo = $1)
		  AND (event_types IS NULL OR $2 = ANY(event_types))`

	rows, err := db.QueryContext(ctx, q, repo, e.Type())
	if err != nil {
		return fmt.Errorf("query subscriptions: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan subscription id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate subscriptions: %w", err)
	}

	for _, subID := range ids {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO event_outbox (event, subscription_id)
			VALUES ($1, $2)`,
			eventJSON, subID,
		); err != nil {
			slog.Error("outbox: insert row failed", "subscription_id", subID, "error", err)
		}
	}
	return nil
}

// computeHMAC returns the hex-encoded HMAC-SHA256 of body using secret.
func computeHMAC(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// InsertOutboxEvent inserts a single outbox row. Used in tests.
func InsertOutboxEvent(ctx context.Context, db *sql.DB, eventJSON []byte, subscriptionID string) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO event_outbox (event, subscription_id)
		VALUES ($1, $2)`,
		eventJSON, subscriptionID,
	)
	return err
}

// subscriptionRow is returned by CreateSubscription for use in the store.
type subscriptionRow struct {
	ID           string
	Repo         *string
	EventTypes   []string
	Backend      string
	Config       json.RawMessage
	CreatedAt    time.Time
	CreatedBy    string
	SuspendedAt  *time.Time
	FailureCount int
}

// ensure pq is used (for TEXT[] scanning)
var _ = pq.Array
