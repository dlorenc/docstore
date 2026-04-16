package main

import (
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
	mux := newMux(nil, nil, "http://localhost:9999", &http.Client{}, 30*time.Minute)

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
	mux := newMux(nil, nil, "http://localhost:9999", &http.Client{}, 30*time.Minute)

	req := httptest.NewRequest(http.MethodPost, "/run", strings.NewReader(`{"branch":"feature/x"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestPostRunHandler_MissingBranch(t *testing.T) {
	mux := newMux(nil, nil, "http://localhost:9999", &http.Client{}, 30*time.Minute)

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

func TestPullBranchSource_DownloadsFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/default/myrepo/-/tree":
			entries := []treeEntry{
				{Path: "main.go", VersionID: "v1"},
				{Path: "sub/util.go", VersionID: "v2"},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(entries) //nolint:errcheck

		case r.URL.Path == "/repos/default/myrepo/-/file/main.go":
			resp := model.FileResponse{Path: "main.go", Content: []byte("package main")}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp) //nolint:errcheck

		case r.URL.Path == "/repos/default/myrepo/-/file/sub/util.go":
			resp := model.FileResponse{Path: "sub/util.go", Content: []byte("package sub")}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp) //nolint:errcheck

		default:
			http.NotFound(w, r)
		}
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
