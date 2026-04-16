package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dlorenc/docstore/internal/executor"
	"github.com/dlorenc/docstore/internal/model"
)

// ---------------------------------------------------------------------------
// fetchConfig tests
// ---------------------------------------------------------------------------

func TestFetchConfig_Success(t *testing.T) {
	yamlContent := []byte(`checks:
  - name: ci/build
    image: golang:1.25
    steps:
      - go build ./...
  - name: ci/test
    image: golang:1.25
    steps:
      - go test ./...
`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Query().Get("branch") != "main" {
			t.Errorf("expected branch=main, got %q", r.URL.Query().Get("branch"))
		}
		resp := model.FileResponse{
			Path:    ".docstore/ci.yaml",
			Content: yamlContent,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	defer srv.Close()

	cfg, err := fetchConfig(context.Background(), srv.Client(), srv.URL, "default/myrepo")
	if err != nil {
		t.Fatalf("fetchConfig: %v", err)
	}
	if len(cfg.Checks) != 2 {
		t.Fatalf("expected 2 checks, got %d", len(cfg.Checks))
	}
	if cfg.Checks[0].Name != "ci/build" {
		t.Errorf("checks[0].name = %q, want ci/build", cfg.Checks[0].Name)
	}
	if cfg.Checks[1].Name != "ci/test" {
		t.Errorf("checks[1].name = %q, want ci/test", cfg.Checks[1].Name)
	}
	if cfg.Checks[0].Image != "golang:1.25" {
		t.Errorf("checks[0].image = %q, want golang:1.25", cfg.Checks[0].Image)
	}
}

func TestFetchConfig_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := fetchConfig(context.Background(), srv.Client(), srv.URL, "default/myrepo")
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}
}

func TestFetchConfig_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := fetchConfig(context.Background(), srv.Client(), srv.URL, "default/myrepo")
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
}

// ---------------------------------------------------------------------------
// postCheckRun tests
// ---------------------------------------------------------------------------

func TestPostCheckRun_Success(t *testing.T) {
	var received model.CreateCheckRunRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(model.CreateCheckRunResponse{ID: "cr-1", Sequence: 5}) //nolint:errcheck
	}))
	defer srv.Close()

	logURL := "file:///tmp/ci-logs/ci_build.log"
	req := model.CreateCheckRunRequest{
		Branch:    "feature/x",
		CheckName: "ci/build",
		Status:    model.CheckRunPassed,
		LogURL:    &logURL,
	}
	if err := postCheckRun(context.Background(), srv.Client(), srv.URL, "default/myrepo", req); err != nil {
		t.Fatalf("postCheckRun: %v", err)
	}
	if received.Branch != "feature/x" {
		t.Errorf("branch = %q, want feature/x", received.Branch)
	}
	if received.CheckName != "ci/build" {
		t.Errorf("check_name = %q, want ci/build", received.CheckName)
	}
	if received.Status != model.CheckRunPassed {
		t.Errorf("status = %q, want passed", received.Status)
	}
	if received.LogURL == nil || *received.LogURL != logURL {
		t.Errorf("log_url = %v, want %q", received.LogURL, logURL)
	}
}

func TestPostCheckRun_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "branch not found", http.StatusNotFound)
	}))
	defer srv.Close()

	err := postCheckRun(context.Background(), srv.Client(), srv.URL, "default/myrepo", model.CreateCheckRunRequest{
		Branch:    "feature/x",
		CheckName: "ci/build",
		Status:    model.CheckRunPassed,
	})
	if err == nil {
		t.Fatal("expected error on non-201, got nil")
	}
}

// ---------------------------------------------------------------------------
// POST /run handler tests
// ---------------------------------------------------------------------------

func TestPostRunHandler_ReturnsRunID(t *testing.T) {
	// Use a nil executor and logstore (the goroutine won't do anything useful
	// but the handler itself should return 200 with a run_id immediately).
	mux := newMux(context.Background(), nil, nil, "http://localhost:9999", &http.Client{}, 30*time.Minute, "")

	body := `{"repo":"default/myrepo","branch":"feature/x","head_sequence":42}`
	req := httptest.NewRequest(http.MethodPost, "/run", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp runResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.RunID == "" {
		t.Error("expected non-empty run_id")
	}
}

func TestPostRunHandler_MissingRepo(t *testing.T) {
	mux := newMux(context.Background(), nil, nil, "http://localhost:9999", &http.Client{}, 30*time.Minute, "")

	req := httptest.NewRequest(http.MethodPost, "/run", strings.NewReader(`{"branch":"feature/x"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestPostRunHandler_MissingBranch(t *testing.T) {
	mux := newMux(context.Background(), nil, nil, "http://localhost:9999", &http.Client{}, 30*time.Minute, "")

	req := httptest.NewRequest(http.MethodPost, "/run", strings.NewReader(`{"repo":"default/myrepo"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// pullBranchSource tests
// ---------------------------------------------------------------------------

// buildTestTar constructs an in-memory tar archive from a map of path→content.
func buildTestTar(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		hdr := &tar.Header{
			Name:     name,
			Size:     int64(len(content)),
			Mode:     0644,
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %s: %v", name, err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatalf("tar write %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	return buf.Bytes()
}

func TestPullBranchSource_DownloadsFiles(t *testing.T) {
	tarData := buildTestTar(t, map[string][]byte{
		"main.go":     []byte("package main"),
		"sub/util.go": []byte("package sub"),
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/default/myrepo/-/archive" {
			w.Header().Set("Content-Type", "application/x-tar")
			w.Write(tarData) //nolint:errcheck
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	tempDir, err := pullBranchSource(context.Background(), srv.Client(), srv.URL, "default/myrepo", "feature/x")
	if tempDir != "" {
		defer os.RemoveAll(tempDir)
	}
	if err != nil {
		t.Fatalf("pullBranchSource: %v", err)
	}

	mainContent, err := os.ReadFile(filepath.Join(tempDir, "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if string(mainContent) != "package main" {
		t.Errorf("main.go content = %q, want \"package main\"", string(mainContent))
	}

	utilContent, err := os.ReadFile(filepath.Join(tempDir, "sub", "util.go"))
	if err != nil {
		t.Fatalf("read sub/util.go: %v", err)
	}
	if string(utilContent) != "package sub" {
		t.Errorf("sub/util.go content = %q, want \"package sub\"", string(utilContent))
	}
}

// Ensure executor.Config is imported (used in fetchConfig).
var _ executor.Config

// ---------------------------------------------------------------------------
// POST /webhook handler tests
// ---------------------------------------------------------------------------

func buildWebhookBody(t *testing.T, eventType string, data any) []byte {
	t.Helper()
	dataJSON, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}
	env := map[string]any{
		"specversion":     "1.0",
		"type":            eventType,
		"source":          "/repos/default/myrepo",
		"id":              "test-id",
		"time":            "2024-01-01T00:00:00Z",
		"datacontenttype": "application/json",
		"data":            json.RawMessage(dataJSON),
	}
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return body
}

func signBody(body []byte, secret string) string {
	return "sha256=" + computeHMACHex(body, secret)
}

func TestWebhookHandler_ValidSignature(t *testing.T) {
	const secret = "test-secret"
	mux := newMux(context.Background(), nil, nil, "http://localhost:9999", &http.Client{}, 30*time.Minute, secret)

	body := buildWebhookBody(t, "com.docstore.commit.created", map[string]any{
		"repo":       "default/myrepo",
		"branch":     "feature/x",
		"sequence":   int64(42),
		"author":     "alice",
		"message":    "add feature",
		"file_count": 1,
	})

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/cloudevents+json")
	req.Header.Set("X-DocStore-Signature", signBody(body, secret))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestWebhookHandler_InvalidSignature(t *testing.T) {
	const secret = "test-secret"
	mux := newMux(context.Background(), nil, nil, "http://localhost:9999", &http.Client{}, 30*time.Minute, secret)

	body := buildWebhookBody(t, "com.docstore.commit.created", map[string]any{
		"repo":   "default/myrepo",
		"branch": "feature/x",
	})

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/cloudevents+json")
	req.Header.Set("X-DocStore-Signature", "sha256=badhash")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestWebhookHandler_UnknownEventType(t *testing.T) {
	mux := newMux(context.Background(), nil, nil, "http://localhost:9999", &http.Client{}, 30*time.Minute, "")

	body := buildWebhookBody(t, "com.docstore.branch.created", map[string]any{
		"repo":   "default/myrepo",
		"branch": "feature/x",
	})

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/cloudevents+json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for unknown event type, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// GET /run/{run_id} handler tests
// ---------------------------------------------------------------------------

func TestGetRunStatus_NotFound(t *testing.T) {
	mux := newMux(context.Background(), nil, nil, "http://localhost:9999", &http.Client{}, 30*time.Minute, "")

	req := httptest.NewRequest(http.MethodGet, "/run/nonexistent-id", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestRunStatus_TracksInFlight(t *testing.T) {
	reg := newRunRegistry()

	// Start a run.
	reg.start("run-1", "default/myrepo", "feature/x", 42)

	status, ok := reg.get("run-1")
	if !ok {
		t.Fatal("expected run-1 to exist in registry")
	}
	if status.State != "running" {
		t.Errorf("state = %q, want running", status.State)
	}
	if status.Repo != "default/myrepo" {
		t.Errorf("repo = %q, want default/myrepo", status.Repo)
	}
	if status.Branch != "feature/x" {
		t.Errorf("branch = %q, want feature/x", status.Branch)
	}
	if status.HeadSeq != 42 {
		t.Errorf("head_seq = %d, want 42", status.HeadSeq)
	}

	// Complete the run.
	reg.complete("run-1", []executor.CheckResult{
		{Name: "ci/build", Status: "passed", Logs: "ok"},
	})

	status, ok = reg.get("run-1")
	if !ok {
		t.Fatal("expected run-1 to still exist after completion")
	}
	if status.State != "done" {
		t.Errorf("state = %q, want done", status.State)
	}
	if len(status.Checks) != 1 {
		t.Errorf("checks count = %d, want 1", len(status.Checks))
	}
}

func TestRunStatus_ViaHTTP(t *testing.T) {
	mux := newMux(context.Background(), nil, nil, "http://localhost:9999", &http.Client{}, 30*time.Minute, "")

	// Trigger a run via POST /run to register it in the registry.
	runBody := `{"repo":"default/myrepo","branch":"feature/x","head_sequence":1}`
	postReq := httptest.NewRequest(http.MethodPost, "/run", strings.NewReader(runBody))
	postReq.Header.Set("Content-Type", "application/json")
	postRec := httptest.NewRecorder()
	mux.ServeHTTP(postRec, postReq)

	if postRec.Code != http.StatusOK {
		t.Fatalf("POST /run: expected 200, got %d", postRec.Code)
	}
	var runResp runResponse
	if err := json.NewDecoder(postRec.Body).Decode(&runResp); err != nil {
		t.Fatalf("decode run response: %v", err)
	}

	// Poll GET /run/{run_id} — run should be visible immediately (at least as "running").
	getReq := httptest.NewRequest(http.MethodGet, "/run/"+runResp.RunID, nil)
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, getReq)

	if getRec.Code != http.StatusOK {
		t.Fatalf("GET /run/{id}: expected 200, got %d; body: %s", getRec.Code, getRec.Body.String())
	}
	var status RunStatus
	if err := json.NewDecoder(getRec.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.RunID != runResp.RunID {
		t.Errorf("run_id = %q, want %q", status.RunID, runResp.RunID)
	}
	if status.State == "" {
		t.Error("expected non-empty state")
	}
}
