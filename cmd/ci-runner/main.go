package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
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
// RunStatus tracks an in-progress or completed CI run.
// ---------------------------------------------------------------------------

// RunStatus is the state record for a single CI run.
type RunStatus struct {
	RunID     string                 `json:"run_id"`
	Repo      string                 `json:"repo"`
	Branch    string                 `json:"branch"`
	HeadSeq   int64                  `json:"head_seq,omitempty"`
	State     string                 `json:"state"` // "running", "done", "failed"
	StartedAt time.Time              `json:"started_at"`
	Checks    []executor.CheckResult `json:"checks,omitempty"`
	Error     string                 `json:"error,omitempty"`
}

// runRegistry is a thread-safe in-memory store of run statuses.
type runRegistry struct {
	mu   sync.RWMutex
	runs map[string]*RunStatus
}

func newRunRegistry() *runRegistry {
	return &runRegistry{runs: make(map[string]*RunStatus)}
}

func (r *runRegistry) start(runID, repo, branch string, headSeq int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs[runID] = &RunStatus{
		RunID:     runID,
		Repo:      repo,
		Branch:    branch,
		HeadSeq:   headSeq,
		State:     "running",
		StartedAt: time.Now(),
	}
}

func (r *runRegistry) complete(runID string, checks []executor.CheckResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.runs[runID]; ok {
		s.State = "done"
		s.Checks = append([]executor.CheckResult(nil), checks...)
	}
}

func (r *runRegistry) fail(runID, errMsg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.runs[runID]; ok {
		s.State = "failed"
		s.Error = errMsg
	}
}

func (r *runRegistry) cleanup() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, s := range r.runs {
		if s.State != "running" && time.Since(s.StartedAt) > time.Hour {
			delete(r.runs, id)
		}
	}
}

func (r *runRegistry) get(runID string) (*RunStatus, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.runs[runID]
	if !ok {
		return nil, false
	}
	// Return a shallow copy to avoid data races.
	cp := *s
	if s.Checks != nil {
		cp.Checks = append([]executor.CheckResult(nil), s.Checks...)
	}
	return &cp, true
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

// pullBranchSource materialises the given branch at headSeq into a new temp
// directory and returns its path. The caller is responsible for os.RemoveAll.
// It uses the /-/archive endpoint to fetch all files in a single request.
func pullBranchSource(ctx context.Context, client *http.Client, docstoreURL, repo, branch string, headSeq int64) (string, error) {
	tempDir, err := os.MkdirTemp("", "ci-runner-src-*")
	if err != nil {
		return "", fmt.Errorf("create temp dir: %w", err)
	}

	archiveURL := fmt.Sprintf("%s/repos/%s/-/archive?branch=%s&at=%d",
		docstoreURL, repo, url.QueryEscape(branch), headSeq)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, archiveURL, nil)
	if err != nil {
		return tempDir, fmt.Errorf("create archive request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return tempDir, fmt.Errorf("fetch archive: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return tempDir, fmt.Errorf("fetch archive: unexpected status %d", resp.StatusCode)
	}

	tr := tar.NewReader(resp.Body)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return tempDir, fmt.Errorf("read archive: %w", err)
		}
		destPath := filepath.Join(tempDir, filepath.FromSlash(hdr.Name))
		if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
			return tempDir, fmt.Errorf("create dir for %s: %w", hdr.Name, err)
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			return tempDir, fmt.Errorf("read archive entry %s: %w", hdr.Name, err)
		}
		if err := os.WriteFile(destPath, content, 0o644); err != nil {
			return tempDir, fmt.Errorf("write file %s: %w", hdr.Name, err)
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

func runAsync(ctx context.Context, client *http.Client, exec *executor.Executor, ls logstore.LogStore, docstoreURL, repo, branch string, headSeq int64, runID string, reg *runRegistry) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("runAsync panic", "repo", repo, "branch", branch, "panic", r)
			if reg != nil {
				reg.fail(runID, fmt.Sprintf("panic: %v", r))
			}
		}
	}()

	slog.Info("run started", "repo", repo, "branch", branch, "head_seq", headSeq, "run_id", runID)

	// 1. Fetch config from main branch.
	cfg, err := fetchConfig(ctx, client, docstoreURL, repo)
	if err != nil {
		slog.Error("fetch config failed", "repo", repo, "branch", branch, "error", err)
		// Post a synthetic failed check so the failure is visible in docstore.
		msg := err.Error()
		_ = postCheckRun(ctx, client, docstoreURL, repo, model.CreateCheckRunRequest{
			Branch:    branch,
			CheckName: "ci/config",
			Status:    model.CheckRunFailed,
			LogURL:    &msg,
			Sequence:  &headSeq,
		})
		if reg != nil {
			reg.fail(runID, "config fetch failed: "+err.Error())
		}
		return
	}

	// 2. Pull branch source into temp dir.
	tempDir, err := pullBranchSource(ctx, client, docstoreURL, repo, branch, headSeq)
	if tempDir != "" {
		defer os.RemoveAll(tempDir)
	}
	if err != nil {
		slog.Error("pull source failed", "repo", repo, "branch", branch, "error", err)
		if reg != nil {
			reg.fail(runID, "source pull failed: "+err.Error())
		}
		return
	}

	// 3. Mark each check as pending.
	for _, check := range cfg.Checks {
		if err := postCheckRun(ctx, client, docstoreURL, repo, model.CreateCheckRunRequest{
			Branch:    branch,
			CheckName: check.Name,
			Status:    model.CheckRunPending,
			Sequence:  &headSeq,
		}); err != nil {
			slog.Warn("mark pending failed", "check", check.Name, "error", err)
		}
	}

	// 4. Execute all checks.
	results, err := exec.Run(ctx, tempDir, *cfg)
	if err != nil {
		slog.Error("executor failed", "repo", repo, "branch", branch, "error", err)
		if reg != nil {
			reg.fail(runID, "executor failed: "+err.Error())
		}
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
			Sequence:  &headSeq,
		}); err != nil {
			slog.Warn("post result failed", "check", result.Name, "error", err)
		}
	}

	if reg != nil {
		reg.complete(runID, results)
	}
	slog.Info("run complete", "repo", repo, "branch", branch, "checks", len(results), "run_id", runID)
}

// ---------------------------------------------------------------------------
// HMAC helper
// ---------------------------------------------------------------------------

// computeHMACHex returns the hex-encoded HMAC-SHA256 of body using secret.
// The format matches what docstore's outbox dispatcher sends:
// "sha256=" + hex(hmac-sha256(body, secret))
func computeHMACHex(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// ---------------------------------------------------------------------------
// Startup: auto-register webhook subscription with docstore
// ---------------------------------------------------------------------------

// registerWebhookSubscription checks whether a webhook subscription for
// runnerURL already exists and creates one if not. Errors are logged as
// warnings so the runner can still be triggered manually.
func registerWebhookSubscription(ctx context.Context, client *http.Client, docstoreURL, runnerURL, webhookSecret string) {
	webhookURL := strings.TrimRight(runnerURL, "/") + "/webhook"
	subsURL := strings.TrimRight(docstoreURL, "/") + "/subscriptions"

	// List existing subscriptions.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, subsURL, nil)
	if err != nil {
		slog.Warn("webhook registration: build list request failed", "error", err)
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("webhook registration: list subscriptions failed", "error", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		var listResp model.ListSubscriptionsResponse
		if err := json.NewDecoder(resp.Body).Decode(&listResp); err == nil {
			for _, sub := range listResp.Subscriptions {
				var cfg map[string]string
				if err := json.Unmarshal(sub.Config, &cfg); err != nil {
					slog.Warn("webhook registration: failed to parse subscription config", "id", sub.ID, "error", err)
					continue
				}
				if cfg["url"] == webhookURL {
					slog.Info("webhook subscription already registered", "id", sub.ID, "url", webhookURL)
					return
				}
			}
		}
	}

	// Create subscription.
	cfgJSON, _ := json.Marshal(map[string]string{"url": webhookURL, "secret": webhookSecret})
	createBody, _ := json.Marshal(model.CreateSubscriptionRequest{
		Backend:    "webhook",
		EventTypes: []string{"com.docstore.commit.created"},
		Config:     cfgJSON,
	})
	createReq, err := http.NewRequestWithContext(ctx, http.MethodPost, subsURL, bytes.NewReader(createBody))
	if err != nil {
		slog.Warn("webhook registration: build create request failed", "error", err)
		return
	}
	createReq.Header.Set("Content-Type", "application/json")
	createResp, err := client.Do(createReq)
	if err != nil {
		slog.Warn("webhook registration: create subscription failed", "error", err)
		return
	}
	defer createResp.Body.Close()

	if createResp.StatusCode == http.StatusCreated {
		slog.Info("webhook subscription registered", "url", webhookURL)
	} else {
		slog.Warn("webhook registration: unexpected status (runner will still accept manual POST /run)",
			"status", createResp.StatusCode, "url", webhookURL)
	}
}

// ---------------------------------------------------------------------------
// HTTP mux
// ---------------------------------------------------------------------------

func newMux(serverCtx context.Context, exec *executor.Executor, ls logstore.LogStore, docstoreURL string, client *http.Client, runTimeout time.Duration, webhookSecret string) *http.ServeMux {
	reg := newRunRegistry()

	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-serverCtx.Done():
				return
			case <-ticker.C:
				reg.cleanup()
			}
		}
	}()

	mux := http.NewServeMux()

	// POST /run — manual trigger. Runs synchronously so the HTTP connection
	// stays open for the duration of the build. Returns when all checks complete.
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
		reg.start(runID, req.Repo, req.Branch, req.HeadSequence)

		// Flush headers immediately so the load balancer doesn't timeout
		// waiting for the first byte while the build runs.
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}

		// Run synchronously using a background context so the build is not
		// cancelled if the client disconnects.
		ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
		defer cancel()
		runAsync(ctx, client, exec, ls, docstoreURL, req.Repo, req.Branch, req.HeadSequence, runID, reg)

		status, _ := reg.get(runID)
		json.NewEncoder(w).Encode(status) //nolint:errcheck
	})

	// POST /webhook — receives CloudEvents webhook deliveries from docstore.
	mux.HandleFunc("POST /webhook", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}

		// Verify HMAC signature when a secret is configured.
		if webhookSecret != "" {
			sig := r.Header.Get("X-DocStore-Signature")
			expected := "sha256=" + computeHMACHex(body, webhookSecret)
			if !hmac.Equal([]byte(sig), []byte(expected)) {
				http.Error(w, "invalid signature", http.StatusBadRequest)
				return
			}
		}

		// Parse CloudEvents envelope — only need type and data.
		var env struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			http.Error(w, "invalid cloudevent body", http.StatusBadRequest)
			return
		}

		// Unknown event types are silently acknowledged (forward-compat).
		if env.Type != "com.docstore.commit.created" {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Extract commit.created data fields.
		var data struct {
			Repo     string `json:"repo"`
			Branch   string `json:"branch"`
			Sequence int64  `json:"sequence"`
		}
		if err := json.Unmarshal(env.Data, &data); err != nil {
			http.Error(w, "invalid event data", http.StatusBadRequest)
			return
		}
		if data.Repo == "" || data.Branch == "" {
			http.Error(w, "missing repo or branch in event data", http.StatusBadRequest)
			return
		}

		runID := uuid.New().String()
		reg.start(runID, data.Repo, data.Branch, data.Sequence)
		go func() {
			ctx, cancel := context.WithTimeout(serverCtx, runTimeout)
			defer cancel()
			runAsync(ctx, client, exec, ls, docstoreURL, data.Repo, data.Branch, data.Sequence, runID, reg)
		}()

		w.WriteHeader(http.StatusOK)
	})

	// GET /run/{run_id} — polling endpoint for run status.
	mux.HandleFunc("GET /run/{run_id}", func(w http.ResponseWriter, r *http.Request) {
		runID := r.PathValue("run_id")
		status, ok := reg.get(runID)
		if !ok {
			http.Error(w, "run not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(status) //nolint:errcheck
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
	runnerURL := flag.String("runner-url", "", "Public URL of this ci-runner instance (used to register webhook subscription)")
	webhookSecret := flag.String("webhook-secret", "", "Shared HMAC secret for webhook signature verification")
	flag.Parse()

	if *docstoreURL == "" {
		fmt.Fprintln(os.Stderr, "error: --docstore-url is required")
		os.Exit(1)
	}

	// LOG_LEVEL accepts: debug, info, warn, error (default: info).
	var logLevel slog.LevelVar
	if lvlStr := os.Getenv("LOG_LEVEL"); lvlStr != "" {
		if err := logLevel.UnmarshalText([]byte(lvlStr)); err != nil {
			logLevel.Set(slog.LevelInfo)
		}
	}
	var handler slog.Handler
	if os.Getenv("LOG_FORMAT") == "text" {
		handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: &logLevel})
	} else {
		handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: &logLevel})
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

	// serverCtx is cancelled on shutdown so in-flight runAsync goroutines can
	// detect the signal and stop gracefully.
	serverCtx, serverCancel := context.WithCancel(context.Background())
	defer serverCancel()

	mux := newMux(serverCtx, exec, ls, *docstoreURL, httpClient, *runTimeout, *webhookSecret)

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

	// Auto-register webhook subscription if runner-url and webhook-secret are provided.
	if *runnerURL != "" && *webhookSecret != "" {
		registerCtx, registerCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer registerCancel()
		registerWebhookSubscription(registerCtx, httpClient, *docstoreURL, *runnerURL, *webhookSecret)
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	slog.Info("shutting down")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown error", "error", err)
		os.Exit(1)
	}
	// Cancel serverCtx after HTTP shutdown so runAsync goroutines stop.
	serverCancel()
	if err := exec.Close(); err != nil {
		slog.Error("executor close error", "error", err)
	}
	slog.Info("stopped")
}
