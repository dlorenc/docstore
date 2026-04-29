package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dlorenc/docstore/internal/ciconfig"
	"github.com/dlorenc/docstore/internal/executor"
	"github.com/dlorenc/docstore/internal/model"
)

// ---------------------------------------------------------------------------
// Mocks
// ---------------------------------------------------------------------------

// mockRunner implements runner for tests.
type mockRunner struct {
	results []executor.CheckResult
	err     error
}

func (m *mockRunner) Run(_ context.Context, _ string, _ executor.Config, _ ciconfig.TriggerContext, _ []string) ([]executor.CheckResult, error) {
	return m.results, m.err
}

// mockLogStore implements logstore.LogStore for tests.
type mockLogStore struct {
	writeURL string
	writeErr error
	calls    atomic.Int32
}

func (m *mockLogStore) Write(_ context.Context, _, _ string, _ int64, _, _ string) (string, error) {
	m.calls.Add(1)
	return m.writeURL, m.writeErr
}

// mockScheduler is a minimal httptest scheduler for heartbeat/complete tests.
type mockScheduler struct {
	calls    atomic.Int32
	errCode  int // if non-zero, return this HTTP status on heartbeat
}

func (m *mockScheduler) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.calls.Add(1)
		if m.errCode != 0 {
			http.Error(w, "mock error", m.errCode)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// fileResponseJSON marshals a model.FileResponse whose Content is yamlContent.
func fileResponseJSON(t *testing.T, yamlContent string) []byte {
	t.Helper()
	data, err := json.Marshal(model.FileResponse{
		Path:    ".docstore/ci.yaml",
		Content: []byte(yamlContent),
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// makeTar creates an uncompressed tar archive containing the given files.
func makeTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(content)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// testJob returns a minimal CIJob for use in runJob tests.
func testJob() *model.CIJob {
	return &model.CIJob{
		ID:       "job-1",
		Repo:     "testrepo",
		Branch:   "feature",
		Sequence: 42,
	}
}

// ---------------------------------------------------------------------------
// checkLogServer
// ---------------------------------------------------------------------------

func TestCheckLogServer_MethodNotAllowed(t *testing.T) {
	s := &checkLogServer{}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/logs/mycheck", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestCheckLogServer_EmptyCheckName(t *testing.T) {
	s := &checkLogServer{}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/logs/", nil))
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestCheckLogServer_NoActiveJob(t *testing.T) {
	s := &checkLogServer{}
	// logDir not set
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/logs/mycheck", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestCheckLogServer_LogNotFound(t *testing.T) {
	s := &checkLogServer{}
	s.setDir(t.TempDir()) // dir exists but no log file
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/logs/mycheck", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestCheckLogServer_ServesLog(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mycheck.log"), []byte("hello log"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &checkLogServer{}
	s.setDir(dir)

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/logs/mycheck", nil))

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if got := w.Body.String(); got != "hello log" {
		t.Errorf("unexpected body: %q", got)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("unexpected Content-Type: %q", ct)
	}
}

func TestCheckLogServer_SlashInCheckName(t *testing.T) {
	dir := t.TempDir()
	// The server maps "/" → "_" in check names.
	if err := os.WriteFile(filepath.Join(dir, "ci_mycheck.log"), []byte("slash log"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &checkLogServer{}
	s.setDir(dir)

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/logs/ci/mycheck", nil))

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if got := w.Body.String(); got != "slash log" {
		t.Errorf("unexpected body: %q", got)
	}
}

func TestCheckLogServer_SetDirClearsJob(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mycheck.log"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &checkLogServer{}
	s.setDir(dir)
	s.setDir("") // clear dir

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/logs/mycheck", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 after clearing dir, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// fetchConfig
// ---------------------------------------------------------------------------

func TestFetchConfig_Success(t *testing.T) {
	const ciYAML = `checks:
- name: test
  image: golang:1.22
  steps: ["go test ./..."]
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(fileResponseJSON(t, ciYAML)) //nolint:errcheck
	}))
	defer srv.Close()

	cfg, err := fetchConfig(context.Background(), srv.Client(), srv.URL, "repo", "main", 1)
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil {
		t.Fatal("expected config, got nil")
	}
	if len(cfg.Checks) != 1 || cfg.Checks[0].Name != "test" {
		t.Errorf("unexpected config: %+v", cfg)
	}
}

func TestFetchConfig_NotFound_ReturnsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	cfg, err := fetchConfig(context.Background(), srv.Client(), srv.URL, "repo", "main", 1)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if cfg != nil {
		t.Errorf("expected nil config for 404, got %+v", cfg)
	}
}

func TestFetchConfig_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := fetchConfig(context.Background(), srv.Client(), srv.URL, "repo", "main", 1)
	if err == nil {
		t.Fatal("expected error for 500, got nil")
	}
}

func TestFetchConfig_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json at all")) //nolint:errcheck
	}))
	defer srv.Close()

	_, err := fetchConfig(context.Background(), srv.Client(), srv.URL, "repo", "main", 1)
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestFetchConfig_URLContainsBranchAndSeq(t *testing.T) {
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		w.Write(fileResponseJSON(t, "checks: []")) //nolint:errcheck
	}))
	defer srv.Close()

	_, _ = fetchConfig(context.Background(), srv.Client(), srv.URL, "myrepo", "feat/x", 99)
	if !strings.Contains(gotURL, "feat%2Fx") {
		t.Errorf("branch not URL-encoded in request: %s", gotURL)
	}
	if !strings.Contains(gotURL, "at=99") {
		t.Errorf("sequence missing from request: %s", gotURL)
	}
}

// ---------------------------------------------------------------------------
// postCheckRun
// ---------------------------------------------------------------------------

func TestPostCheckRun_Success(t *testing.T) {
	var received model.CreateCheckRunRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	seq := int64(7)
	req := model.CreateCheckRunRequest{
		Branch:    "feature",
		CheckName: "lint",
		Status:    model.CheckRunPassed,
		Sequence:  &seq,
	}
	if err := postCheckRun(context.Background(), srv.Client(), srv.URL, "myrepo", req); err != nil {
		t.Fatal(err)
	}
	if received.CheckName != "lint" {
		t.Errorf("expected check_name lint, got %q", received.CheckName)
	}
	if received.Branch != "feature" {
		t.Errorf("expected branch feature, got %q", received.Branch)
	}
}

func TestPostCheckRun_UnexpectedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	seq := int64(1)
	err := postCheckRun(context.Background(), srv.Client(), srv.URL, "myrepo", model.CreateCheckRunRequest{
		Branch:    "b",
		CheckName: "c",
		Status:    model.CheckRunFailed,
		Sequence:  &seq,
	})
	if err == nil {
		t.Fatal("expected error for non-201, got nil")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("expected 502 in error, got: %v", err)
	}
}

func TestPostCheckRun_WithLogURL(t *testing.T) {
	var received model.CreateCheckRunRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&received) //nolint:errcheck
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	logURL := "file:///tmp/test.log"
	seq := int64(3)
	_ = postCheckRun(context.Background(), srv.Client(), srv.URL, "r", model.CreateCheckRunRequest{
		Branch:    "b",
		CheckName: "c",
		Status:    model.CheckRunPassed,
		LogURL:    &logURL,
		Sequence:  &seq,
	})
	if received.LogURL == nil || *received.LogURL != logURL {
		t.Errorf("expected log_url to be propagated, got %v", received.LogURL)
	}
}

// ---------------------------------------------------------------------------
// heartbeat
// ---------------------------------------------------------------------------

func TestHeartbeat_StopsOnDone(t *testing.T) {
	ms := &mockScheduler{}
	srv := httptest.NewServer(ms.handler())
	defer srv.Close()

	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		heartbeat(context.Background(), srv.URL, "job-1", "tok", done)
		close(finished)
	}()

	close(done)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("heartbeat goroutine did not stop after done closed")
	}
}

func TestHeartbeat_StopsOnContextCancel(t *testing.T) {
	ms := &mockScheduler{}
	srv := httptest.NewServer(ms.handler())
	defer srv.Close()

	done := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())

	finished := make(chan struct{})
	go func() {
		heartbeat(ctx, srv.URL, "job-1", "tok", done)
		close(finished)
	}()

	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("heartbeat goroutine did not stop after context cancelled")
	}
}

func TestHeartbeat_CallsHeartbeatPeriodically(t *testing.T) {
	orig := heartbeatInterval
	heartbeatInterval = 20 * time.Millisecond
	t.Cleanup(func() { heartbeatInterval = orig })

	ms := &mockScheduler{}
	srv := httptest.NewServer(ms.handler())
	defer srv.Close()

	done := make(chan struct{})
	go heartbeat(context.Background(), srv.URL, "job-1", "tok", done)

	time.Sleep(90 * time.Millisecond)
	close(done)

	n := ms.calls.Load()
	if n < 2 {
		t.Errorf("expected ≥2 heartbeat calls in 90ms at 20ms interval, got %d", n)
	}
}

func TestHeartbeat_ErrorDoesNotPanic(t *testing.T) {
	orig := heartbeatInterval
	heartbeatInterval = 20 * time.Millisecond
	t.Cleanup(func() { heartbeatInterval = orig })

	ms := &mockScheduler{errCode: http.StatusInternalServerError}
	srv := httptest.NewServer(ms.handler())
	defer srv.Close()

	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		heartbeat(context.Background(), srv.URL, "job-1", "tok", done)
		close(finished)
	}()

	time.Sleep(50 * time.Millisecond)
	close(done) // should not panic even when heartbeat returns an error

	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("heartbeat goroutine did not stop after done closed")
	}
}

// ---------------------------------------------------------------------------
// runJob
// ---------------------------------------------------------------------------

// docstoreHandler returns an http.HandlerFunc that dispatches on URL path
// fragments. It is reused across the runJob tests.
func docstoreHandler(t *testing.T, ciYAML string, tarData []byte, checkStatus int) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/-/file/"):
			if ciYAML == "" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(fileResponseJSON(t, ciYAML)) //nolint:errcheck
		case r.Method == http.MethodPost && strings.Contains(path, "/-/archive/presign"):
			// Return a fake presigned URL; the mock executor does not fetch it.
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"url":"http://test-archive.example.com/archive.tar"}`)) //nolint:errcheck
		case strings.Contains(path, "/-/archive"):
			w.Header().Set("Content-Type", "application/x-tar")
			w.Write(tarData) //nolint:errcheck
		case strings.Contains(path, "/-/check"):
			w.WriteHeader(checkStatus)
		default:
			http.NotFound(w, r)
		}
	}
}

func TestRunJob_NoCIYAML_ReturnsPassed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// All requests return 404 — fetchConfig interprets 404 as "no ci.yaml".
		http.NotFound(w, r)
	}))
	defer srv.Close()

	status, logURL, errMsg := runJob(
		context.Background(), srv.Client(), &mockRunner{}, nil,
		srv.URL, testJob(), "", t.TempDir(), ciconfig.TriggerContext{Type: "push"}, nil,
	)
	if status != "passed" {
		t.Errorf("expected passed, got %q", status)
	}
	if logURL != nil || errMsg != nil {
		t.Errorf("expected nil logURL/errMsg for no ci.yaml, got %v / %v", logURL, errMsg)
	}
}

func TestRunJob_FetchConfigError_ReturnsFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/-/file/") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// check run post after config failure
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	status, _, errMsg := runJob(
		context.Background(), srv.Client(), &mockRunner{}, nil,
		srv.URL, testJob(), "", t.TempDir(), ciconfig.TriggerContext{}, nil,
	)
	if status != "failed" {
		t.Errorf("expected failed, got %q", status)
	}
	if errMsg == nil {
		t.Error("expected non-nil errMsg")
	}
}


func TestRunJob_ExecutorError_ReturnsFailed(t *testing.T) {
	const ciYAML = "checks:\n- name: test\n  image: golang:1.22\n  steps: [\"go test ./...\"]\n"
	tarData := makeTar(t, map[string]string{"main.go": "package main"})
	srv := httptest.NewServer(docstoreHandler(t, ciYAML, tarData, http.StatusCreated))
	defer srv.Close()

	mockExec := &mockRunner{err: fmt.Errorf("buildkit unavailable")}
	status, _, errMsg := runJob(
		context.Background(), srv.Client(), mockExec, nil,
		srv.URL, testJob(), "", t.TempDir(), ciconfig.TriggerContext{}, nil,
	)
	if status != "failed" {
		t.Errorf("expected failed, got %q", status)
	}
	if errMsg == nil || !strings.Contains(*errMsg, "buildkit") {
		t.Errorf("expected buildkit error in errMsg, got %v", errMsg)
	}
}

func TestRunJob_AllChecksPassed(t *testing.T) {
	const ciYAML = `checks:
- name: test
  image: golang:1.22
  steps: ["go test ./..."]
- name: lint
  image: golangci-lint:latest
  steps: ["golangci-lint run"]
`
	tarData := makeTar(t, map[string]string{"main.go": "package main"})
	srv := httptest.NewServer(docstoreHandler(t, ciYAML, tarData, http.StatusCreated))
	defer srv.Close()

	mockExec := &mockRunner{results: []executor.CheckResult{
		{Name: "test", Status: "passed", Logs: "ok"},
		{Name: "lint", Status: "passed", Logs: "clean"},
	}}
	ls := &mockLogStore{writeURL: "file:///tmp/ci-logs/test.log"}

	status, logURL, errMsg := runJob(
		context.Background(), srv.Client(), mockExec, ls,
		srv.URL, testJob(), "", t.TempDir(), ciconfig.TriggerContext{Type: "push"}, nil,
	)
	if status != "passed" {
		t.Errorf("expected passed, got %q", status)
	}
	if logURL == nil {
		t.Error("expected logURL to be set from log store")
	}
	if errMsg != nil {
		t.Errorf("expected nil errMsg, got %q", *errMsg)
	}
	if n := ls.calls.Load(); n != 2 {
		t.Errorf("expected 2 log store writes, got %d", n)
	}
}

func TestRunJob_OneCheckFailed_OverallFailed(t *testing.T) {
	const ciYAML = `checks:
- name: test
  image: golang:1.22
  steps: ["go test ./..."]
- name: lint
  image: golangci-lint:latest
  steps: ["golangci-lint run"]
`
	tarData := makeTar(t, map[string]string{"main.go": "package main"})
	srv := httptest.NewServer(docstoreHandler(t, ciYAML, tarData, http.StatusCreated))
	defer srv.Close()

	mockExec := &mockRunner{results: []executor.CheckResult{
		{Name: "test", Status: "passed", Logs: "ok"},
		{Name: "lint", Status: "failed", Logs: "FAIL: 3 issues"},
	}}

	status, _, errMsg := runJob(
		context.Background(), srv.Client(), mockExec, nil,
		srv.URL, testJob(), "", t.TempDir(), ciconfig.TriggerContext{}, nil,
	)
	if status != "failed" {
		t.Errorf("expected failed, got %q", status)
	}
	if errMsg != nil {
		t.Errorf("expected nil errMsg for check failure (not infra error), got %q", *errMsg)
	}
}

func TestRunJob_NilLogStore_StillPosts(t *testing.T) {
	const ciYAML = "checks:\n- name: build\n  image: golang:1.22\n  steps: [\"go build\"]\n"
	tarData := makeTar(t, map[string]string{"main.go": "package main"})

	var checkPostCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/-/file/"):
			w.Header().Set("Content-Type", "application/json")
			w.Write(fileResponseJSON(t, ciYAML)) //nolint:errcheck
		case r.Method == http.MethodPost && strings.Contains(path, "/-/archive/presign"):
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"url":"http://test-archive.example.com/archive.tar"}`)) //nolint:errcheck
		case strings.Contains(path, "/-/archive"):
			w.Write(tarData) //nolint:errcheck
		case strings.Contains(path, "/-/check"):
			checkPostCount++
			w.WriteHeader(http.StatusCreated)
		}
	}))
	defer srv.Close()

	mockExec := &mockRunner{results: []executor.CheckResult{
		{Name: "build", Status: "passed", Logs: "ok"},
	}}

	status, logURL, _ := runJob(
		context.Background(), srv.Client(), mockExec, nil, // nil log store
		srv.URL, testJob(), "", t.TempDir(), ciconfig.TriggerContext{}, nil,
	)
	if status != "passed" {
		t.Errorf("expected passed, got %q", status)
	}
	if logURL != nil {
		t.Errorf("expected nil logURL with nil log store, got %v", logURL)
	}
	// Expect: 1 pending post + 1 result post = 2
	if checkPostCount != 2 {
		t.Errorf("expected 2 check run posts, got %d", checkPostCount)
	}
}

func TestRunJob_LogsWrittenToDir(t *testing.T) {
	const ciYAML = "checks:\n- name: test\n  image: golang:1.22\n  steps: [\"go test\"]\n"
	tarData := makeTar(t, map[string]string{"main.go": "package main"})
	srv := httptest.NewServer(docstoreHandler(t, ciYAML, tarData, http.StatusCreated))
	defer srv.Close()

	logOutput := "=== RUN   TestFoo\n--- PASS: TestFoo\n"
	mockExec := &mockRunner{results: []executor.CheckResult{
		{Name: "test", Status: "passed", Logs: logOutput},
	}}

	logDir := t.TempDir()
	runJob(context.Background(), srv.Client(), mockExec, nil, srv.URL, testJob(), "", logDir, ciconfig.TriggerContext{}, nil) //nolint:errcheck

	data, err := os.ReadFile(filepath.Join(logDir, "test.log"))
	if err != nil {
		t.Fatalf("expected log file to be written: %v", err)
	}
	if string(data) != logOutput {
		t.Errorf("unexpected log content: %q", string(data))
	}
}
