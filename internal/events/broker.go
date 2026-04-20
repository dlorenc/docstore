package events

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"sync"
)

// Broker is the in-process event fan-out hub.
// It fans out emitted events to SSE subscribers and writes outbox rows for
// webhook subscriptions. SSE delivery is in-memory and best-effort; clients
// that disconnect miss events. Webhook delivery is durable via the outbox.
//
// Horizontal scaling note: SSE fan-out is in-memory. Works correctly at
// --max-instances=1 only. See OPERATIONS.md for details.
type Broker struct {
	db  *sql.DB
	mu  sync.RWMutex
	// subs maps "repo/*" or "repo/<full-type>" to subscriber channels.
	subs map[string][]chan Event
}

// NewBroker creates a new Broker. db is used for outbox writes on Emit.
// Pass nil to disable webhook outbox writes (e.g. in tests that only need SSE).
func NewBroker(db *sql.DB) *Broker {
	return &Broker{
		db:   db,
		subs: make(map[string][]chan Event),
	}
}

// Subscribe registers a channel to receive events for the given repo.
// If types is empty, all events for the repo are received.
// Otherwise, only events whose Type() matches one of the provided types
// are received. Types must be the full CloudEvents type, e.g.
// "com.docstore.commit.created".
// Returns the channel and an unsubscribe function that must be called
// when the subscriber is done (e.g. on client disconnect).
func (b *Broker) Subscribe(repo string, types []string) (<-chan Event, func()) {
	ch := make(chan Event, 64)
	keys := subscriberKeys(repo, types)

	b.mu.Lock()
	for _, k := range keys {
		b.subs[k] = append(b.subs[k], ch)
	}
	b.mu.Unlock()

	unsub := func() {
		b.mu.Lock()
		for _, k := range keys {
			b.subs[k] = removeChannel(b.subs[k], ch)
		}
		b.mu.Unlock()
		// Drain and close so SSE goroutine can exit cleanly.
		for len(ch) > 0 {
			<-ch
		}
		close(ch)
	}
	return ch, unsub
}

// Emit publishes an event to all matching SSE subscribers and writes outbox
// rows for each matching webhook subscription. Outbox writes are best-effort;
// errors are logged but not returned.
func (b *Broker) Emit(ctx context.Context, e Event) {
	b.fanOutSSE(e)

	if b.db == nil {
		return
	}
	if err := insertOutboxForEvent(ctx, b.db, e); err != nil {
		slog.Error("events: outbox insert failed", "type", e.Type(), "source", e.Source(), "error", err)
	}
}

// fanOutSSE sends an event to all SSE subscribers whose keys match the event.
func (b *Broker) fanOutSSE(e Event) {
	// Derive the repo name from the source URI.
	repo := repoFromSource(e.Source())

	// Collect the set of matching subscriber keys.
	// Also check global wildcard keys ("*/*" and "*/<type>") so that
	// subscribers using repo="*" receive events from all repos.
	wildcardKey := repo + "/*"
	typeKey := repo + "/" + e.Type()
	globalWildcardKey := "*/*"
	globalTypeKey := "*/" + e.Type()

	b.mu.RLock()
	// Collect unique channels (a subscriber might appear under both keys theoretically,
	// but our Subscribe logic ensures each channel is under exactly one set of keys).
	seen := make(map[chan Event]struct{})
	var targets []chan Event
	for _, k := range []string{wildcardKey, typeKey, globalWildcardKey, globalTypeKey} {
		for _, ch := range b.subs[k] {
			if _, ok := seen[ch]; !ok {
				seen[ch] = struct{}{}
				targets = append(targets, ch)
			}
		}
	}
	b.mu.RUnlock()

	for _, ch := range targets {
		select {
		case ch <- e:
		default:
			// Slow consumer; drop event rather than block.
			slog.Warn("events: SSE subscriber too slow, dropping event",
				"type", e.Type(), "repo", repo)
		}
	}
}

// subscriberKeys returns the broker map keys for the given repo and type filter.
func subscriberKeys(repo string, types []string) []string {
	if len(types) == 0 {
		return []string{repo + "/*"}
	}
	keys := make([]string, len(types))
	for i, t := range types {
		keys[i] = repo + "/" + t
	}
	return keys
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

// removeChannel removes ch from slice without preserving order.
func removeChannel(slice []chan Event, ch chan Event) []chan Event {
	for i, c := range slice {
		if c == ch {
			slice[i] = slice[len(slice)-1]
			return slice[:len(slice)-1]
		}
	}
	return slice
}
