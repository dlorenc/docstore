package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/lib/pq"
)

// LoggedEvent is a single row from the event_log table.
type LoggedEvent struct {
	Seq     int64
	Repo    string
	Type    string
	Payload json.RawMessage
}

// Broker is the event hub. It persists events to the event_log table,
// sends pg_notify for real-time wake-up, and exposes Poll/WaitForEvent
// for SSE clients and background workers.
// Because delivery is DB-backed, multiple server instances all see
// every event — no --max-instances=1 constraint.
type Broker struct {
	db *sql.DB
	mu sync.Mutex
	// waitCh is closed whenever Notify is called, waking all WaitForEvent
	// callers. A new channel is created after each close.
	waitCh chan struct{}
}

// NewBroker creates a new Broker. db is used for event_log and outbox writes.
// Pass nil to disable DB writes (e.g. unit tests that do not need persistence).
func NewBroker(db *sql.DB) *Broker {
	return &Broker{
		db:     db,
		waitCh: make(chan struct{}),
	}
}

// Notify wakes all WaitForEvent callers. It is called by the pg_notify
// listener goroutine in StartDispatcher whenever a new event_log row appears.
func (b *Broker) Notify() {
	b.mu.Lock()
	old := b.waitCh
	b.waitCh = make(chan struct{})
	b.mu.Unlock()
	close(old)
}

// WaitForEvent blocks until Notify is called or ctx is cancelled.
// Returns nil when woken by a notification, or ctx.Err() on cancellation.
func (b *Broker) WaitForEvent(ctx context.Context) error {
	b.mu.Lock()
	ch := b.waitCh
	b.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// CurrentSeq returns the current maximum sequence number in event_log.
// Returns 0 if the table is empty or db is nil.
func (b *Broker) CurrentSeq(ctx context.Context) (int64, error) {
	if b.db == nil {
		return 0, nil
	}
	var seq int64
	err := b.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM event_log`,
	).Scan(&seq)
	return seq, err
}

// Poll returns logged events with seq > sinceSeq.
// If repo is "*", events from all repos are returned.
// If types is empty, all event types are returned.
// Results are ordered by seq ASC, capped at 200 rows per call.
func (b *Broker) Poll(ctx context.Context, repo string, sinceSeq int64, types []string) ([]LoggedEvent, error) {
	if b.db == nil {
		return nil, nil
	}

	var (
		sqlRows *sql.Rows
		err     error
	)

	switch {
	case repo == "*" && len(types) == 0:
		sqlRows, err = b.db.QueryContext(ctx,
			`SELECT seq, repo, type, payload FROM event_log
			 WHERE seq > $1 ORDER BY seq LIMIT 200`,
			sinceSeq)
	case repo == "*":
		sqlRows, err = b.db.QueryContext(ctx,
			`SELECT seq, repo, type, payload FROM event_log
			 WHERE seq > $1 AND type = ANY($2) ORDER BY seq LIMIT 200`,
			sinceSeq, pq.Array(types))
	case len(types) == 0:
		sqlRows, err = b.db.QueryContext(ctx,
			`SELECT seq, repo, type, payload FROM event_log
			 WHERE seq > $1 AND repo = $2 ORDER BY seq LIMIT 200`,
			sinceSeq, repo)
	default:
		sqlRows, err = b.db.QueryContext(ctx,
			`SELECT seq, repo, type, payload FROM event_log
			 WHERE seq > $1 AND repo = $2 AND type = ANY($3) ORDER BY seq LIMIT 200`,
			sinceSeq, repo, pq.Array(types))
	}
	if err != nil {
		return nil, fmt.Errorf("poll event_log: %w", err)
	}
	defer sqlRows.Close()

	var evs []LoggedEvent
	for sqlRows.Next() {
		var ev LoggedEvent
		if err := sqlRows.Scan(&ev.Seq, &ev.Repo, &ev.Type, &ev.Payload); err != nil {
			return nil, fmt.Errorf("scan event_log row: %w", err)
		}
		evs = append(evs, ev)
	}
	return evs, sqlRows.Err()
}

// Emit persists an event to event_log, sends a pg_notify wake-up, and writes
// outbox rows for matching webhook subscriptions.
// All DB operations are best-effort; errors are logged but not returned.
func (b *Broker) Emit(ctx context.Context, e Event) {
	if b.db == nil {
		return
	}

	payload, err := ToCloudEvent(e)
	if err != nil {
		slog.Error("events: serialize event failed", "type", e.Type(), "error", err)
		return
	}

	repo := repoFromSource(e.Source())

	if _, err := b.db.ExecContext(ctx,
		`INSERT INTO event_log (repo, type, payload) VALUES ($1, $2, $3)`,
		repo, e.Type(), payload,
	); err != nil {
		slog.Error("events: event_log insert failed", "type", e.Type(), "error", err)
	}

	// Wake any pq.Listener goroutines on all instances.
	if _, err := b.db.ExecContext(ctx,
		`SELECT pg_notify('event_log', $1)`, repo,
	); err != nil {
		slog.Warn("events: pg_notify failed", "type", e.Type(), "error", err)
	}

	if err := insertOutboxForEvent(ctx, b.db, e); err != nil {
		slog.Error("events: outbox insert failed", "type", e.Type(), "source", e.Source(), "error", err)
	}

	// Wake local waiters immediately. Cross-instance waiters are woken via
	// the pg_notify → pq.Listener → Notify() path in StartDispatcher.
	b.Notify()
}

// repoFromSource extracts the repo name from a CloudEvents source URI.
// "/repos/acme/myrepo" → "acme/myrepo"
// Other sources (e.g. "/orgs/acme") return the source as-is.
func repoFromSource(source string) string {
	if strings.HasPrefix(source, "/repos/") {
		return source[len("/repos/"):]
	}
	return source
}
