//go:build e2e

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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

// TestE2EGoPipeline is a full end-to-end test that:
//  1. Starts a real docstore server backed by a testcontainers postgres
//  2. Starts a real buildkitd (with --oci-worker-no-process-sandbox, matching
//     the GKE Autopilot production config) via testcontainers
//  3. Seeds a repo with a Go module, source, and .docstore/ci.yaml using
//     golang:1.25 for build/test/vet
//  4. Registers a webhook subscription on the ci-runner
//  5. Commits a source change to trigger a commit.created event
//  6. Polls until all three checks (ci/build, ci/test, ci/vet) resolve
//  7. Asserts all three checks passed
//
// Run with: go test ./cmd/ci-runner/ -tags=e2e -v -timeout=5m
func TestE2EGoPipeline(t *testing.T) {
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

	dispatchCtx, dispatchCancel := context.WithCancel(ctx)
	t.Cleanup(dispatchCancel)
	events.StartDispatcher(dispatchCtx, db)

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

	logDir := t.TempDir()
	ls, _ := logstore.NewLocalLogStore(logDir)
	const webhookSecret = "e2e-test-secret"
	runnerMux := newMux(ctx, exec, ls, docstoreSrv.URL, adminClient, 10*time.Minute, webhookSecret)
	runnerSrv := httptest.NewServer(runnerMux)
	t.Cleanup(runnerSrv.Close)
	t.Logf("ci-runner URL: %s", runnerSrv.URL)

	// --- 3. Seed docstore: org, repo, Go source + ci.yaml on main ---
	mustPost(t, adminClient, docstoreSrv.URL+"/orgs",
		map[string]any{"name": "testci"})

	// Name is just the repo component; owner provides the org prefix.
	mustPost(t, adminClient, docstoreSrv.URL+"/repos",
		map[string]any{"owner": "testci", "name": "app"})

	ciYAML := `checks:
  - name: ci/build
    image: golang:1.25
    steps:
      - go build ./...

  - name: ci/test
    image: golang:1.25
    steps:
      - go test ./...

  - name: ci/vet
    image: golang:1.25
    steps:
      - go vet ./...
`
	goMod := `module testci/app

go 1.22
`
	mainGo := `package main

import "fmt"

func main() { fmt.Println("hello") }
`
	mainTestGo := `package main

import "testing"

func TestSmoke(t *testing.T) {}
`
	mustCommit(t, adminClient, docstoreSrv.URL, "testci/app", "main",
		"initial: add go source and ci config", []model.FileChange{
			{Path: ".docstore/ci.yaml", Content: []byte(ciYAML)},
			{Path: "go.mod", Content: []byte(goMod)},
			{Path: "main.go", Content: []byte(mainGo)},
			{Path: "main_test.go", Content: []byte(mainTestGo)},
		})

	// --- 4. Register webhook subscription pointing at the ci-runner ---
	cfgJSON, _ := json.Marshal(map[string]string{
		"url":    runnerSrv.URL + "/webhook",
		"secret": webhookSecret,
	})
	subBody, _ := json.Marshal(model.CreateSubscriptionRequest{
		Backend:    "webhook",
		EventTypes: []string{"com.docstore.commit.created"},
		Config:     cfgJSON,
	})
	subResp, err := adminClient.Post(
		docstoreSrv.URL+"/subscriptions",
		"application/json",
		bytes.NewReader(subBody),
	)
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	subResp.Body.Close()
	if subResp.StatusCode != http.StatusCreated {
		t.Fatalf("create subscription: expected 201, got %d", subResp.StatusCode)
	}
	t.Log("webhook subscription created")

	// --- 5. Commit a source change to trigger commit.created ---
	triggerResp := mustCommit(t, adminClient, docstoreSrv.URL, "testci/app", "main",
		"trigger: update smoke test", []model.FileChange{
			{Path: "main_test.go", Content: []byte(`package main

import "testing"

func TestSmoke(t *testing.T) { t.Log("smoke ok") }
`)},
		})
	t.Logf("trigger commit sequence: %d", triggerResp.Sequence)

	// --- 6. Poll until all three checks resolve (passed or failed) ---
	checksURL := fmt.Sprintf("%s/repos/testci/app/-/branch/main/checks", docstoreSrv.URL)
	deadline := time.Now().Add(4 * time.Minute)
	wantChecks := map[string]bool{"ci/build": false, "ci/test": false, "ci/vet": false}

	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)

		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, checksURL, nil)
		cr, err := adminClient.Do(req)
		if err != nil {
			t.Logf("poll checks: %v", err)
			continue
		}
		var runs []model.CheckRun
		if err := json.NewDecoder(cr.Body).Decode(&runs); err != nil {
			cr.Body.Close()
			continue
		}
		cr.Body.Close()

		for _, run := range runs {
			if run.Sequence != triggerResp.Sequence {
				continue
			}
			if run.Status == model.CheckRunPassed || run.Status == model.CheckRunFailed {
				wantChecks[run.CheckName] = true
			}
		}

		allDone := true
		for _, done := range wantChecks {
			if !done {
				allDone = false
				break
			}
		}
		if allDone {
			break
		}
		t.Logf("waiting... resolved: %v", wantChecks)
	}

	// --- 7. Assert all checks resolved and passed ---
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, checksURL, nil)
	finalResp, err := adminClient.Do(req)
	if err != nil {
		t.Fatalf("final check fetch: %v", err)
	}
	var finalRuns []model.CheckRun
	if err := json.NewDecoder(finalResp.Body).Decode(&finalRuns); err != nil {
		t.Fatalf("decode final checks: %v", err)
	}
	finalResp.Body.Close()

	byName := make(map[string]model.CheckRun)
	for _, run := range finalRuns {
		if run.Sequence == triggerResp.Sequence &&
			(run.Status == model.CheckRunPassed || run.Status == model.CheckRunFailed) {
			byName[run.CheckName] = run
		}
	}

	for _, name := range []string{"ci/build", "ci/test", "ci/vet"} {
		run, ok := byName[name]
		if !ok {
			t.Errorf("check %s: no resolved run found", name)
			continue
		}
		if run.Status != model.CheckRunPassed {
			// Print log content to help debug failures.
			logContent := ""
			if run.LogURL != nil {
				logContent = readLocalLog(t, *run.LogURL)
			}
			t.Errorf("check %s: expected passed, got %s\nlogs:\n%s", name, run.Status, logContent)
		} else {
			t.Logf("check %s: passed ✓", name)
		}
	}
	_ = logDir // keep temp dir alive until assertions complete
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
		t.Logf("POST %s: status %d", url, resp.StatusCode)
	}
}

func readLocalLog(t *testing.T, logURL string) string {
	t.Helper()
	// Local log store uses file:// URLs.
	path := strings.TrimPrefix(logURL, "file://")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(could not read log %s: %v)", path, err)
	}
	return string(data)
}

func mustCommit(t *testing.T, client *http.Client, docstoreURL, repo, branch, message string, files []model.FileChange) model.CommitResponse {
	t.Helper()
	type filePayload struct {
		Path    string `json:"path"`
		Content string `json:"content"` // base64-encoded
	}
	type commitPayload struct {
		Branch  string        `json:"branch"`
		Message string        `json:"message"`
		Files   []filePayload `json:"files"`
	}
	payload := commitPayload{Branch: branch, Message: message}
	for _, f := range files {
		payload.Files = append(payload.Files, filePayload{
			Path:    f.Path,
			Content: base64.StdEncoding.EncodeToString(f.Content),
		})
	}
	b, _ := json.Marshal(payload)
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
