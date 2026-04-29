package server_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dbpkg "github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/events"
	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/server"
	"github.com/dlorenc/docstore/internal/testutil"
	"github.com/lib/pq"
)

// newEventTestServer creates a server with an event broker wired.
func newEventTestServer(t *testing.T, db *sql.DB) (*httptest.Server, *events.Broker) {
	t.Helper()
	store := dbpkg.NewStore(db)
	broker := events.NewBroker(db)
	handler := server.NewWithBroker(store, db, nil, broker, "test@example.com", "test@example.com", "", "")
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, broker
}

// seedForEvents inserts the minimum rows needed for SSE/webhook tests.
func seedForEvents(t *testing.T, db *sql.DB) {
	t.Helper()
	stmts := []string{
		`INSERT INTO orgs (name) VALUES ('default') ON CONFLICT DO NOTHING`,
		`INSERT INTO repos (name, owner) VALUES ('default/default', 'default') ON CONFLICT DO NOTHING`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed: %v\n%s", err, s)
		}
	}
}

func TestSSE_ReceivesEventsOnCommit(t *testing.T) {
	t.Parallel()
	db := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	seedForEvents(t, db)
	// Give default/default a role for the identity "test@example.com".
	db.Exec(`INSERT INTO roles (repo, identity, role) VALUES ('default/default', 'test@example.com', 'admin') ON CONFLICT DO NOTHING`)

	srv, _ := newEventTestServer(t, db)

	// Open SSE stream.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/repos/default/default/-/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE connect: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// In a goroutine, post a commit.
	go func() {
		time.Sleep(100 * time.Millisecond)
		commitBody, _ := json.Marshal(model.CommitRequest{
			Branch:  "main",
			Message: "test commit",
			Files:   []model.FileChange{{Path: "x.txt", Content: []byte("hello")}},
		})
		http.Post(srv.URL+"/repos/default/default/-/commit",
			"application/json", bytes.NewReader(commitBody))
	}()

	// Read the SSE stream looking for the commit.created event.
	scanner := bufio.NewScanner(resp.Body)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !scanner.Scan() {
			break
		}
		line := scanner.Text()
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			var env map[string]any
			if err := json.Unmarshal([]byte(data), &env); err != nil {
				continue
			}
			if env["type"] == "com.docstore.commit.created" {
				return // success
			}
		}
	}
	t.Fatal("did not receive commit.created event on SSE stream")
}

func TestSSE_FilterByType(t *testing.T) {
	t.Parallel()
	db := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	seedForEvents(t, db)
	db.Exec(`INSERT INTO roles (repo, identity, role) VALUES ('default/default', 'test@example.com', 'admin') ON CONFLICT DO NOTHING`)

	srv, broker := newEventTestServer(t, db)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Subscribe only to branch.created.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/repos/default/default/-/events?types=com.docstore.branch.created", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE connect: %v", err)
	}
	defer resp.Body.Close()

	// Emit a commit event (should be filtered out) then a branch event.
	go func() {
		time.Sleep(100 * time.Millisecond)
		// Emit commit (should not appear in stream).
		broker.Emit(context.Background(), &commitCreatedStub{repo: "default/default"})
		time.Sleep(50 * time.Millisecond)
		// Emit branch.created (should appear).
		broker.Emit(context.Background(), &branchCreatedStub{repo: "default/default"})
	}()

	scanner := bufio.NewScanner(resp.Body)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !scanner.Scan() {
			break
		}
		line := scanner.Text()
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			var env map[string]any
			if err := json.Unmarshal([]byte(data), &env); err != nil {
				continue
			}
			evType, _ := env["type"].(string)
			if evType == "com.docstore.commit.created" {
				t.Error("received commit.created event that should have been filtered")
			}
			if evType == "com.docstore.branch.created" {
				return // success
			}
		}
	}
	t.Fatal("did not receive branch.created event")
}

func TestSSE_SinceSeqReplay(t *testing.T) {
	t.Parallel()
	db := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	seedForEvents(t, db)
	db.Exec(`INSERT INTO roles (repo, identity, role) VALUES ('default/default', 'test@example.com', 'admin') ON CONFLICT DO NOTHING`)

	srv, broker := newEventTestServer(t, db)

	// Record the current tail so we know where to start replaying from.
	seq0, err := broker.CurrentSeq(context.Background())
	if err != nil {
		t.Fatalf("CurrentSeq: %v", err)
	}

	// Emit three events before the client connects.
	broker.Emit(context.Background(), &branchCreatedStub{repo: "default/default"})
	broker.Emit(context.Background(), &commitCreatedStub{repo: "default/default"})
	broker.Emit(context.Background(), &branchCreatedStub{repo: "default/default"})

	// Connect with ?since_seq pointing to the position before the emits.
	// The handler must replay all three events immediately.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/repos/default/default/-/events?since_seq=%d", srv.URL, seq0), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE connect: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Count data lines; expect at least 3 replayed events.
	received := 0
	scanner := bufio.NewScanner(resp.Body)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !scanner.Scan() {
			break
		}
		if strings.HasPrefix(scanner.Text(), "data: ") {
			received++
			if received >= 3 {
				return // success
			}
		}
	}
	t.Fatalf("expected at least 3 replayed events via since_seq, got %d", received)
}

// Stub event types for testing.
type commitCreatedStub struct{ repo string }

func (e *commitCreatedStub) Type() string   { return "com.docstore.commit.created" }
func (e *commitCreatedStub) Source() string { return "/repos/" + e.repo }
func (e *commitCreatedStub) Data() any      { return e }

type branchCreatedStub struct{ repo string }

func (e *branchCreatedStub) Type() string   { return "com.docstore.branch.created" }
func (e *branchCreatedStub) Source() string { return "/repos/" + e.repo }
func (e *branchCreatedStub) Data() any      { return e }

func TestWebhook_DeliveredAfterMutation(t *testing.T) {
	t.Parallel()
	db := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	seedForEvents(t, db)
	db.Exec(`INSERT INTO roles (repo, identity, role) VALUES ('default/default', 'test@example.com', 'admin') ON CONFLICT DO NOTHING`)

	// Webhook target.
	var receivedCount atomic.Int64
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer webhook.Close()

	// Insert subscription.
	config, _ := json.Marshal(map[string]string{"url": webhook.URL, "secret": ""})
	db.Exec(`
		INSERT INTO event_subscriptions (backend, config, created_by, event_types)
		VALUES ('webhook', $1, 'test', $2)`,
		config, pq.Array([]string{"com.docstore.commit.created"}))

	srv, _ := newEventTestServer(t, db)

	// Start the outbox dispatcher (no DSN/broker needed for webhook-only test).
	events.StartDispatcher(t.Context(), db, "", nil)

	// Post a commit.
	commitBody, _ := json.Marshal(model.CommitRequest{
		Branch:  "main",
		Message: "test",
		Files:   []model.FileChange{{Path: "x.txt", Content: []byte("x")}},
	})
	resp, err := http.Post(srv.URL+"/repos/default/default/-/commit",
		"application/json", bytes.NewReader(commitBody))
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	// Wait for webhook delivery.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if receivedCount.Load() > 0 {
			return // success
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("webhook was not delivered within timeout; received count = %d", receivedCount.Load())
}

func TestWebhook_HMACSignatureValid(t *testing.T) {
	t.Parallel()
	db := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	seedForEvents(t, db)
	db.Exec(`INSERT INTO roles (repo, identity, role) VALUES ('default/default', 'test@example.com', 'admin') ON CONFLICT DO NOTHING`)

	const secret = "my-hmac-secret"
	type capturedRequest struct {
		sig  string
		body []byte
	}
	captured := make(chan capturedRequest, 1)

	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		r.Body.Read(b)
		select {
		case captured <- capturedRequest{sig: r.Header.Get("X-DocStore-Signature"), body: b}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer webhook.Close()

	config, _ := json.Marshal(map[string]string{"url": webhook.URL, "secret": secret})
	db.Exec(`
		INSERT INTO event_subscriptions (backend, config, created_by)
		VALUES ('webhook', $1, 'test')`, config)

	srv, _ := newEventTestServer(t, db)

	events.StartDispatcher(t.Context(), db, "", nil)

	commitBody, _ := json.Marshal(model.CommitRequest{
		Branch:  "main",
		Message: "test",
		Files:   []model.FileChange{{Path: "x.txt", Content: []byte("x")}},
	})
	resp, _ := http.Post(srv.URL+"/repos/default/default/-/commit",
		"application/json", bytes.NewReader(commitBody))
	if resp != nil {
		resp.Body.Close()
	}

	// Wait for webhook.
	var cap capturedRequest
	select {
	case cap = <-captured:
	case <-time.After(15 * time.Second):
		t.Fatal("webhook not received within timeout")
	}

	// Verify HMAC.
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(cap.body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if cap.sig != expected {
		t.Errorf("HMAC mismatch: got %q, expected %q", cap.sig, expected)
	}
}

func TestWebhook_RetriedOnFailure(t *testing.T) {
	t.Parallel()
	db := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	seedForEvents(t, db)
	db.Exec(`INSERT INTO roles (repo, identity, role) VALUES ('default/default', 'test@example.com', 'admin') ON CONFLICT DO NOTHING`)

	var callCount atomic.Int64
	var fail atomic.Bool
	fail.Store(true)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer webhook.Close()

	config, _ := json.Marshal(map[string]string{"url": webhook.URL, "secret": ""})
	db.Exec(`
		INSERT INTO event_subscriptions (backend, config, created_by)
		VALUES ('webhook', $1, 'test')`, config)

	srv, _ := newEventTestServer(t, db)

	events.StartDispatcher(t.Context(), db, "", nil)

	commitBody, _ := json.Marshal(model.CommitRequest{
		Branch:  "main",
		Message: "test",
		Files:   []model.FileChange{{Path: "x.txt", Content: []byte("x")}},
	})
	resp, _ := http.Post(srv.URL+"/repos/default/default/-/commit",
		"application/json", bytes.NewReader(commitBody))
	if resp != nil {
		resp.Body.Close()
	}

	// Wait for first failure.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && callCount.Load() == 0 {
		time.Sleep(100 * time.Millisecond)
	}

	// Now succeed.
	fail.Store(false)

	// Reset next_attempt so the dispatcher retries immediately.
	db.ExecContext(context.Background(), `UPDATE event_outbox SET next_attempt = now() - interval '1 second'`)

	// Wait for successful delivery.
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var deliveredCount int
		db.QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM event_outbox WHERE delivered_at IS NOT NULL`).Scan(&deliveredCount)
		if deliveredCount > 0 {
			return // success
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("webhook was never delivered after retry; call count = %d", callCount.Load())
}

func TestSSE_CurrentSeqFailure_Returns503(t *testing.T) {
	t.Parallel()
	db := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	seedForEvents(t, db)
	db.Exec(`INSERT INTO roles (repo, identity, role) VALUES ('default/default', 'test@example.com', 'admin') ON CONFLICT DO NOTHING`)

	// Give the broker a closed *sql.DB so CurrentSeq always fails.  The
	// server's write/read store still uses the working db, so validateRepo
	// passes, but streamSSE must return 503 instead of silently using seq=0.
	brokenDB, err := sql.Open("postgres", sharedAdminDSN)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	brokenDB.Close()

	st := dbpkg.NewStore(db)
	broker := events.NewBroker(brokenDB)
	handler := server.NewWithBroker(st, db, nil, broker, "test@example.com", "test@example.com", "", "")
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/repos/default/default/-/events")
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", resp.StatusCode)
	}
}

func TestSubscription_AdminOnly(t *testing.T) {
	t.Parallel()
	db := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	seedForEvents(t, db)

	// Create server with no global admin (empty bootstrapAdmin).
	store := dbpkg.NewStore(db)
	broker := events.NewBroker(db)
	handler := server.NewWithBroker(store, db, nil, broker, "test@example.com", "", "", "")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	config, _ := json.Marshal(map[string]string{"url": "https://example.com", "secret": ""})
	body, _ := json.Marshal(map[string]any{
		"backend": "webhook",
		"config":  json.RawMessage(config),
	})

	resp, err := http.Post(srv.URL+"/subscriptions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /subscriptions: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403, got %d", resp.StatusCode)
	}
}

// Use pq to silence the import.
var _ = pq.Array
