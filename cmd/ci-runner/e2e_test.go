//go:build e2e

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	dbpkg "github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/events"
	"github.com/dlorenc/docstore/internal/executor"
	"github.com/dlorenc/docstore/internal/logstore"
	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/server"
	"github.com/dlorenc/docstore/internal/testutil"
)

// TestE2E_WebhookTriggersRun is a full end-to-end test that:
//  1. Starts a real docstore server backed by a testcontainers postgres
//  2. Starts a real ci-runner backed by a testcontainers buildkitd
//  3. Creates a repo and commits a .docstore/ci.yaml to main
//  4. Creates a branch and commits a source file to it
//  5. Registers a webhook subscription pointing at the ci-runner
//  6. Posts a commit to trigger a commit.created event via the outbox dispatcher
//  7. Polls GET /run/{run_id} until the run is done or failed
//  8. Asserts at least one check run was posted back to docstore
//
// Run with: go test ./cmd/ci-runner/ -tags=e2e -v -timeout=5m
func TestE2E_WebhookTriggersRun(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	ctx := context.Background()

	// --- 1. Start Postgres and docstore server ---
	adminDSN, dbCleanup, err := testutil.StartSharedPostgres()
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(dbCleanup)

	db := testutil.TestDBFromShared(t, adminDSN, dbpkg.RunMigrations)
	store := dbpkg.NewStore(db)
	broker := events.NewBroker(db)

	const adminIdentity = "admin@test.example.com"
	docstoreHandler := server.NewWithBroker(store, db, nil, broker, adminIdentity, adminIdentity)
	docstoreSrv := httptest.NewServer(docstoreHandler)
	t.Cleanup(docstoreSrv.Close)
	t.Logf("docstore URL: %s", docstoreSrv.URL)

	// Start the outbox dispatcher so webhook deliveries happen.
	dispatchCtx, dispatchCancel := context.WithCancel(ctx)
	t.Cleanup(dispatchCancel)
	events.StartDispatcher(dispatchCtx, db)

	// Admin HTTP client (sets IAP identity header).
	adminClient := &http.Client{
		Transport: &devTransport{base: http.DefaultTransport, identity: adminIdentity},
	}

	// --- 2. Start buildkitd and ci-runner ---
	buildkitAddr, buildkitCleanup, err := testutil.StartBuildkit()
	if err != nil {
		t.Fatalf("start buildkitd: %v", err)
	}
	t.Cleanup(buildkitCleanup)

	exec, err := executor.New(buildkitAddr)
	if err != nil {
		t.Fatalf("connect to buildkitd: %v", err)
	}
	t.Cleanup(func() { exec.Close() }) //nolint:errcheck

	ls, _ := logstore.NewLocalLogStore(t.TempDir())
	const webhookSecret = "e2e-test-secret"
	runnerMux := newMux(ctx, exec, ls, docstoreSrv.URL, adminClient, 10*time.Minute, webhookSecret)
	runnerSrv := httptest.NewServer(runnerMux)
	t.Cleanup(runnerSrv.Close)
	t.Logf("ci-runner URL: %s", runnerSrv.URL)

	// --- 3. Seed docstore: create org, repo, and commit ci.yaml to main ---
	mustPost(t, adminClient, docstoreSrv.URL+"/orgs",
		map[string]any{"name": "e2e"})

	mustPost(t, adminClient, docstoreSrv.URL+"/repos",
		map[string]any{"name": "e2e/myrepo", "owner": "e2e"})

	// Give admin role to adminIdentity on the repo.
	mustPost(t, adminClient, docstoreSrv.URL+"/repos/e2e/myrepo/-/roles",
		map[string]any{"identity": adminIdentity, "role": "admin"})

	ciYAML := `checks:
  - name: ci/hello
    image: alpine
    steps:
      - echo "hello from e2e"
`
	mustCommit(t, adminClient, docstoreSrv.URL, "e2e/myrepo", "main",
		"add ci config", []model.FileChange{
			{Path: ".docstore/ci.yaml", Content: []byte(ciYAML)},
		})

	// --- 4. Create branch and commit a source file ---
	mustPost(t, adminClient, docstoreSrv.URL+"/repos/e2e/myrepo/-/branch",
		map[string]any{"name": "feature/e2e-test", "base": "main"})

	commitResp := mustCommit(t, adminClient, docstoreSrv.URL, "e2e/myrepo", "feature/e2e-test",
		"add source file", []model.FileChange{
			{Path: "hello.txt", Content: []byte("hello world\n")},
		})

	t.Logf("branch commit sequence: %d", commitResp.Sequence)

	// --- 5. Register webhook subscription pointing at ci-runner ---
	cfgJSON, _ := json.Marshal(map[string]string{
		"url":    runnerSrv.URL + "/webhook",
		"secret": webhookSecret,
	})
	subBody, _ := json.Marshal(model.CreateSubscriptionRequest{
		Backend:    "webhook",
		EventTypes: []string{"com.docstore.commit.created"},
		Config:     cfgJSON,
	})
	resp, err := adminClient.Post(
		docstoreSrv.URL+"/subscriptions",
		"application/json",
		bytes.NewReader(subBody),
	)
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create subscription: expected 201, got %d", resp.StatusCode)
	}
	t.Log("webhook subscription created")

	// --- 6. Post another commit to trigger commit.created event ---
	// The outbox dispatcher will deliver the event to the ci-runner webhook.
	triggerResp := mustCommit(t, adminClient, docstoreSrv.URL, "e2e/myrepo", "feature/e2e-test",
		"trigger ci run", []model.FileChange{
			{Path: "trigger.txt", Content: []byte("trigger\n")},
		})
	t.Logf("trigger commit sequence: %d", triggerResp.Sequence)

	// --- 7. Poll the docstore checks endpoint until checks appear ---
	checksURL := fmt.Sprintf("%s/repos/e2e/myrepo/-/branch/feature/e2e-test/checks", docstoreSrv.URL)
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)

		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, checksURL, nil)
		cr, err := adminClient.Do(req)
		if err != nil {
			t.Logf("poll checks: %v", err)
			continue
		}
		var checksResp struct {
			Checks []struct {
				CheckName string `json:"check_name"`
				Status    string `json:"status"`
			} `json:"checks"`
		}
		_ = json.NewDecoder(cr.Body).Decode(&checksResp)
		cr.Body.Close()

		for _, c := range checksResp.Checks {
			if c.CheckName == "ci/hello" && (c.Status == "passed" || c.Status == "failed") {
				t.Logf("check ci/hello completed with status: %s", c.Status)
				return // success
			}
		}
		t.Logf("waiting for check run... current checks: %+v", checksResp.Checks)
	}

	t.Fatal("timed out waiting for check run to appear")
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func mustPost(t *testing.T, client *http.Client, url string, body any) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := client.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Logf("POST %s: status %d (may be OK for idempotent calls)", url, resp.StatusCode)
	}
}

func mustCommit(t *testing.T, client *http.Client, docstoreURL, repo, branch, message string, files []model.FileChange) model.CommitResponse {
	t.Helper()
	commitReq := model.CommitRequest{
		Branch:  branch,
		Message: message,
		Files:   files,
	}
	b, _ := json.Marshal(commitReq)
	resp, err := client.Post(
		fmt.Sprintf("%s/repos/%s/-/commit", docstoreURL, repo),
		"application/json",
		bytes.NewReader(b),
	)
	if err != nil {
		t.Fatalf("commit to %s/%s: %v", repo, branch, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("commit to %s/%s: expected 201, got %d", repo, branch, resp.StatusCode)
	}
	var cr model.CommitResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatalf("decode commit response: %v", err)
	}
	return cr
}
