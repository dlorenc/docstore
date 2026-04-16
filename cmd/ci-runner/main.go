package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/dlorenc/docstore/internal/executor"
	"github.com/dlorenc/docstore/internal/logstore"
	"github.com/dlorenc/docstore/internal/model"
)

// ---------------------------------------------------------------------------
// Request / response types for POST /run
// ---------------------------------------------------------------------------

type runRequest struct {
	Repo         string `json:"repo"`
	Branch       string `json:"branch"`
	HeadSequence int64  `json:"head_sequence"`
}

type runResponse struct {
	RunID string `json:"run_id"`
}

// ---------------------------------------------------------------------------
// devTransport adds the X-Goog-IAP-JWT-Assertion header to every request
// so that a docstore server running with --dev-identity can match identities.
// ---------------------------------------------------------------------------

type devTransport struct {
	base     http.RoundTripper
	identity string
}

func (t *devTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("X-Goog-IAP-JWT-Assertion", t.identity)
	return t.base.RoundTrip(clone)
}

// ---------------------------------------------------------------------------
// Docstore API helpers
// ---------------------------------------------------------------------------

// fetchConfig retrieves and parses .docstore/ci.yaml from the main branch.
func fetchConfig(ctx context.Context, client *http.Client, docstoreURL, repo string) (*executor.Config, error) {
	fileURL := fmt.Sprintf("%s/repos/%s/-/file/.docstore/ci.yaml?branch=main", docstoreURL, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create config request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch config: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("ci config not found at %s", fileURL)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch config: unexpected status %d", resp.StatusCode)
	}
	var fileResp model.FileResponse
	if err := json.NewDecoder(resp.Body).Decode(&fileResp); err != nil {
		return nil, fmt.Errorf("decode file response: %w", err)
	}
	var cfg executor.Config
	if err := yaml.Unmarshal(fileResp.Content, &cfg); err != nil {
		return nil, fmt.Errorf("parse ci.yaml: %w", err)
	}
	return &cfg, nil
}

// treeEntry is a single entry from GET /-/tree.
type treeEntry struct {
	Path      string `json:"path"`
	VersionID string `json:"version_id"`
}

// pullBranchSource materialises the given branch into a new temp directory
// and returns its path. The caller is responsible for os.RemoveAll.
func pullBranchSource(ctx context.Context, client *http.Client, docstoreURL, repo, branch string) (string, error) {
	tempDir, err := os.MkdirTemp("", "ci-runner-src-*")
	if err != nil {
		return "", fmt.Errorf("create temp dir: %w", err)
	}

	// Paginate through the full tree.
	var entries []treeEntry
	afterPath := ""
	for {
		treeURL := fmt.Sprintf("%s/repos/%s/-/tree?branch=%s&limit=100",
			docstoreURL, repo, url.QueryEscape(branch))
		if afterPath != "" {
			treeURL += "&after=" + url.QueryEscape(afterPath)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, treeURL, nil)
		if err != nil {
			return tempDir, fmt.Errorf("create tree request: %w", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			return tempDir, fmt.Errorf("fetch tree: %w", err)
		}
		var page []treeEntry
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
			resp.Body.Close()
			return tempDir, fmt.Errorf("decode tree: %w", err)
		}
		resp.Body.Close()

		entries = append(entries, page...)
		if len(page) < 100 {
			break
		}
		afterPath = page[len(page)-1].Path
	}

	// Download each file.
	for _, e := range entries {
		fileURL := fmt.Sprintf("%s/repos/%s/-/file/%s?branch=%s",
			docstoreURL, repo, e.Path, url.QueryEscape(branch))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
		if err != nil {
			return tempDir, fmt.Errorf("create file request for %s: %w", e.Path, err)
		}
		resp, err := client.Do(req)
		if err != nil {
			return tempDir, fmt.Errorf("fetch file %s: %w", e.Path, err)
		}
		var fileResp model.FileResponse
		if err := json.NewDecoder(resp.Body).Decode(&fileResp); err != nil {
			resp.Body.Close()
			return tempDir, fmt.Errorf("decode file %s: %w", e.Path, err)
		}
		resp.Body.Close()

		destPath := filepath.Join(tempDir, filepath.FromSlash(e.Path))
		if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
			return tempDir, fmt.Errorf("create dir for %s: %w", e.Path, err)
		}
		if err := os.WriteFile(destPath, fileResp.Content, 0o644); err != nil {
			return tempDir, fmt.Errorf("write file %s: %w", e.Path, err)
		}
	}

	return tempDir, nil
}

// postCheckRun posts a single check run result to the docstore server.
func postCheckRun(ctx context.Context, client *http.Client, docstoreURL, repo string, req model.CreateCheckRunRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal check run: %w", err)
	}
	checkURL := fmt.Sprintf("%s/repos/%s/-/check", docstoreURL, repo)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, checkURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create check run request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("post check run: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("post check run: unexpected status %d", resp.StatusCode)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Async run goroutine
// ---------------------------------------------------------------------------

func runAsync(ctx context.Context, client *http.Client, exec *executor.Executor, ls logstore.LogStore, docstoreURL, repo, branch string, headSeq int64) {
	slog.Info("run started", "repo", repo, "branch", branch, "head_seq", headSeq)

	// 1. Fetch config from main branch.
	cfg, err := fetchConfig(ctx, client, docstoreURL, repo)
	if err != nil {
		slog.Error("fetch config failed", "repo", repo, "branch", branch, "error", err)
		return
	}

	// 2. Pull branch source into temp dir.
	tempDir, err := pullBranchSource(ctx, client, docstoreURL, repo, branch)
	if tempDir != "" {
		defer os.RemoveAll(tempDir)
	}
	if err != nil {
		slog.Error("pull source failed", "repo", repo, "branch", branch, "error", err)
		return
	}

	// 3. Mark each check as pending.
	for _, check := range cfg.Checks {
		if err := postCheckRun(ctx, client, docstoreURL, repo, model.CreateCheckRunRequest{
			Branch:    branch,
			CheckName: check.Name,
			Status:    model.CheckRunPending,
		}); err != nil {
			slog.Warn("mark pending failed", "check", check.Name, "error", err)
		}
	}

	// 4. Execute all checks.
	results, err := exec.Run(ctx, tempDir, *cfg)
	if err != nil {
		slog.Error("executor failed", "repo", repo, "branch", branch, "error", err)
		return
	}

	// 5. Upload logs and post results.
	for _, result := range results {
		var logURL *string
		if ls != nil {
			u, err := ls.Write(ctx, repo, branch, headSeq, result.Name, result.Logs)
			if err != nil {
				slog.Warn("log upload failed", "check", result.Name, "error", err)
			} else {
				logURL = &u
			}
		}

		if err := postCheckRun(ctx, client, docstoreURL, repo, model.CreateCheckRunRequest{
			Branch:    branch,
			CheckName: result.Name,
			Status:    model.CheckRunStatus(result.Status),
			LogURL:    logURL,
		}); err != nil {
			slog.Warn("post result failed", "check", result.Name, "error", err)
		}
	}

	slog.Info("run complete", "repo", repo, "branch", branch, "checks", len(results))
}

// ---------------------------------------------------------------------------
// HTTP mux
// ---------------------------------------------------------------------------

func newMux(exec *executor.Executor, ls logstore.LogStore, docstoreURL string, client *http.Client, runTimeout time.Duration) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /run", func(w http.ResponseWriter, r *http.Request) {
		var req runRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.Repo == "" {
			http.Error(w, "repo is required", http.StatusBadRequest)
			return
		}
		if req.Branch == "" {
			http.Error(w, "branch is required", http.StatusBadRequest)
			return
		}

		runID := uuid.New().String()
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
			defer cancel()
			runAsync(ctx, client, exec, ls, docstoreURL, req.Repo, req.Branch, req.HeadSequence)
		}()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(runResponse{RunID: runID}) //nolint:errcheck
	})
	return mux
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	buildkitAddr := flag.String("buildkit-addr", "unix:///run/buildkit/buildkitd.sock", "buildkitd socket address")
	port := flag.String("port", "8080", "HTTP listen port")
	docstoreURL := flag.String("docstore-url", "", "Base URL of the docstore server (required)")
	devIdentity := flag.String("dev-identity", "", "Identity header to send to docstore (local dev only)")
	runTimeout := flag.Duration("run-timeout", 30*time.Minute, "Maximum duration for a single CI run")
	flag.Parse()

	if *docstoreURL == "" {
		fmt.Fprintln(os.Stderr, "error: --docstore-url is required")
		os.Exit(1)
	}

	var handler slog.Handler
	if os.Getenv("LOG_FORMAT") == "text" {
		handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	} else {
		handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	}
	slog.SetDefault(slog.New(handler))

	// Build HTTP client; optionally add dev identity header.
	httpClient := &http.Client{}
	if *devIdentity != "" {
		httpClient.Transport = &devTransport{
			base:     http.DefaultTransport,
			identity: *devIdentity,
		}
	}

	// Build log store from environment.
	ls, err := logstore.NewFromEnv(context.Background())
	if err != nil {
		slog.Error("failed to create log store", "error", err)
		os.Exit(1)
	}

	// Build executor.
	exec, err := executor.New(*buildkitAddr)
	if err != nil {
		slog.Error("failed to connect to buildkitd", "addr", *buildkitAddr, "error", err)
		os.Exit(1)
	}

	mux := newMux(exec, ls, *docstoreURL, httpClient, *runTimeout)

	srv := &http.Server{
		Addr:        ":" + *port,
		Handler:     mux,
		ReadTimeout: 10 * time.Second,
		// WriteTimeout intentionally omitted: async handler returns immediately.
		IdleTimeout: 60 * time.Second,
	}

	go func() {
		slog.Info("starting ci-runner", "port", *port, "buildkit_addr", *buildkitAddr, "docstore_url", *docstoreURL)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	slog.Info("shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("shutdown error", "error", err)
		os.Exit(1)
	}
	if err := exec.Close(); err != nil {
		slog.Error("executor close error", "error", err)
	}
	slog.Info("stopped")
}
