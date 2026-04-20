package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	exec2 "os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/lib/pq"
	"gopkg.in/yaml.v3"

	"github.com/dlorenc/docstore/internal/ciconfig"
	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/executor"
	"github.com/dlorenc/docstore/internal/logstore"
	"github.com/dlorenc/docstore/internal/model"
)


// runner executes CI checks against a source directory.
// *executor.Executor satisfies this interface.
type runner interface {
	Run(ctx context.Context, sourceDir string, cfg executor.Config, triggerCtx ciconfig.TriggerContext) ([]executor.CheckResult, error)
}

// heartbeater updates a CI job's heartbeat timestamp.
// *db.Store satisfies this interface.
type heartbeater interface {
	HeartbeatCIJob(ctx context.Context, id string) error
}

// heartbeatInterval is the delay between heartbeat updates.
// Overridable in tests.
var heartbeatInterval = 30 * time.Second

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fmt.Fprintf(os.Stderr, "error: %s is required\n", key)
		os.Exit(1)
	}
	return v
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// waitServiceReady polls addr (tcp:// or unix://) until a connection succeeds.
func waitServiceReady(ctx context.Context, name, addr string) error {
	var network, address string
	switch {
	case strings.HasPrefix(addr, "tcp://"):
		network = "tcp"
		address = strings.TrimPrefix(addr, "tcp://")
	case strings.HasPrefix(addr, "unix://"):
		network = "unix"
		address = strings.TrimPrefix(addr, "unix://")
	default:
		return fmt.Errorf("unsupported address scheme for %s: %s", name, addr)
	}
	slog.Info("waiting for service", "name", name, "addr", addr)
	for {
		conn, err := net.DialTimeout(network, address, time.Second)
		if err == nil {
			conn.Close()
			slog.Info("service ready", "name", name)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// waitBuildkitReady polls the buildkitd address until a TCP/unix connection succeeds.
func waitBuildkitReady(ctx context.Context, addr string) error {
	return waitServiceReady(ctx, "buildkitd", addr)
}

// waitDockerdReady polls dockerd at tcp://localhost:2375 until ready.
func waitDockerdReady(ctx context.Context) error {
	return waitServiceReady(ctx, "dockerd", "tcp://localhost:2375")
}

// heartbeat sends periodic last_heartbeat_at updates until done is closed.
func heartbeat(ctx context.Context, store heartbeater, jobID string, done <-chan struct{}) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := store.HeartbeatCIJob(ctx, jobID); err != nil {
				slog.Warn("heartbeat failed", "job_id", jobID, "error", err)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Docstore API helpers (same as ci-runner)
// ---------------------------------------------------------------------------

func fetchConfig(ctx context.Context, client *http.Client, docstoreURL, repo, branch string, headSeq int64) (*executor.Config, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	fileURL := fmt.Sprintf("%s/repos/%s/-/file/.docstore/ci.yaml?branch=%s&at=%d", docstoreURL, repo, url.QueryEscape(branch), headSeq)
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
		// No ci.yaml on this branch — CI is not configured; skip silently.
		return nil, nil
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
// In-progress log server
// ---------------------------------------------------------------------------

// checkLogServer serves per-check log files written during job execution.
// GET /logs/{check} returns the log file for the named check.
type checkLogServer struct {
	mu     sync.RWMutex
	logDir string
}

func (s *checkLogServer) setDir(dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logDir = dir
}

func (s *checkLogServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	checkName := strings.TrimPrefix(r.URL.Path, "/logs/")
	if checkName == "" {
		http.Error(w, "check name required", http.StatusBadRequest)
		return
	}
	s.mu.RLock()
	dir := s.logDir
	s.mu.RUnlock()
	if dir == "" {
		http.Error(w, "no active job", http.StatusNotFound)
		return
	}
	safeName := strings.ReplaceAll(checkName, "/", "_")
	data, err := os.ReadFile(filepath.Join(dir, safeName+".log"))
	if err != nil {
		http.Error(w, "log not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(data) //nolint:errcheck
}

// ---------------------------------------------------------------------------
// Job execution
// ---------------------------------------------------------------------------

// runJob fetches CI config, pulls source, executes checks, uploads logs, and
// posts check run results. Returns (overallStatus, firstLogURL, errorMessage).
func runJob(
	ctx context.Context,
	httpClient *http.Client,
	exec runner,
	ls logstore.LogStore,
	docstoreURL string,
	job *model.CIJob,
	logDir string,
	triggerCtx ciconfig.TriggerContext,
) (status string, logURL *string, errMsg *string) {
	fail := func(msg string) (string, *string, *string) {
		slog.Error("job failed", "job_id", job.ID, "reason", msg)
		return "failed", nil, &msg
	}

	// 1. Fetch CI config from the branch under test at its head sequence.
	cfg, err := fetchConfig(ctx, httpClient, docstoreURL, job.Repo, job.Branch, job.Sequence)
	if err != nil {
		_ = postCheckRun(ctx, httpClient, docstoreURL, job.Repo, model.CreateCheckRunRequest{
			Branch:    job.Branch,
			CheckName: "ci/config",
			Status:    model.CheckRunFailed,
			Sequence:  &job.Sequence,
		})
		return fail(err.Error())
	}
	if cfg == nil {
		// No .docstore/ci.yaml on this branch — CI not configured; skip.
		slog.Info("no ci.yaml on branch, skipping CI", "job_id", job.ID, "branch", job.Branch)
		return "passed", nil, nil
	}

	// 2. Construct the archive URL; BuildKit fetches it directly via llb.HTTP.
	// The sequence parameter makes the URL content-addressed, so BuildKit's
	// HTTP cache deduplicates fetches across parallel checks at the same commit.
	archiveURL := fmt.Sprintf("%s/repos/%s/-/archive?branch=%s&at=%d",
		docstoreURL, job.Repo, url.QueryEscape(job.Branch), job.Sequence)

	// 3. Mark each check as pending (in parallel).
	var pendingWg sync.WaitGroup
	for _, check := range cfg.Checks {
		pendingWg.Add(1)
		go func(checkName string) {
			defer pendingWg.Done()
			if err := postCheckRun(ctx, httpClient, docstoreURL, job.Repo, model.CreateCheckRunRequest{
				Branch:    job.Branch,
				CheckName: checkName,
				Status:    model.CheckRunPending,
				Sequence:  &job.Sequence,
			}); err != nil {
				slog.Warn("mark pending failed", "check", checkName, "error", err)
			}
		}(check.Name)
	}
	pendingWg.Wait()

	// 4. Execute all checks.
	results, err := exec.Run(ctx, archiveURL, *cfg, triggerCtx)
	if err != nil {
		return fail(fmt.Sprintf("executor: %v", err))
	}

	// 5. Upload logs and post results.
	overallStatus := "passed"
	var firstLogURL *string
	for _, result := range results {
		// Write log to temp dir so the log HTTP endpoint can serve it.
		safeName := strings.ReplaceAll(result.Name, "/", "_")
		_ = os.WriteFile(filepath.Join(logDir, safeName+".log"), []byte(result.Logs), 0o644)

		// Upload to persistent log store.
		var checkLogURL *string
		if ls != nil {
			u, err := ls.Write(ctx, job.Repo, job.Branch, job.Sequence, result.Name, result.Logs)
			if err != nil {
				slog.Warn("log upload failed", "check", result.Name, "error", err)
			} else {
				checkLogURL = &u
				if firstLogURL == nil {
					firstLogURL = &u
				}
			}
		}

		// Post check run result.
		if err := postCheckRun(ctx, httpClient, docstoreURL, job.Repo, model.CreateCheckRunRequest{
			Branch:    job.Branch,
			CheckName: result.Name,
			Status:    model.CheckRunStatus(result.Status),
			LogURL:    checkLogURL,
			Sequence:  &job.Sequence,
		}); err != nil {
			slog.Warn("post result failed", "check", result.Name, "error", err)
		}

		if result.Status == "failed" {
			overallStatus = "failed"
		}
	}

	return overallStatus, firstLogURL, nil
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	buildkitAddr := envOrDefault("BUILDKIT_ADDR", "tcp://localhost:1234")
	dbURL := mustEnv("DATABASE_URL")
	docstoreURL := mustEnv("DOCSTORE_URL")
	podName := mustEnv("POD_NAME")
	podIP := mustEnv("POD_IP")

	// Structured logging.
	var logLevel slog.LevelVar
	if lvlStr := os.Getenv("LOG_LEVEL"); lvlStr != "" {
		if err := logLevel.UnmarshalText([]byte(lvlStr)); err != nil {
			logLevel.Set(slog.LevelInfo)
		}
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: &logLevel})))

	ctx := context.Background()

	// Wait for buildkitd to be ready.
	waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Minute)
	if err := waitBuildkitReady(waitCtx, buildkitAddr); err != nil {
		slog.Error("buildkitd not ready", "error", err)
		os.Exit(1)
	}
	waitCancel()

	// Wait for dockerd to be ready.
	waitCtx, waitCancel = context.WithTimeout(ctx, 5*time.Minute)
	if err := waitDockerdReady(waitCtx); err != nil {
		slog.Error("dockerd not ready", "error", err)
		os.Exit(1)
	}
	waitCancel()

	// Connect to Postgres — retry to allow cloud-sql-proxy sidecar time to start.
	sqlDB, err := sql.Open("postgres", dbURL)
	if err != nil {
		slog.Error("open db", "error", err)
		os.Exit(1)
	}
	{
		dbCtx, dbCancel := context.WithTimeout(ctx, 2*time.Minute)
		for {
			if err := sqlDB.PingContext(dbCtx); err == nil {
				break
			} else if dbCtx.Err() != nil {
				slog.Error("ping db: timed out waiting for cloud-sql-proxy", "error", err)
				os.Exit(1)
			}
			slog.Info("waiting for db", "error", err)
			time.Sleep(2 * time.Second)
		}
		dbCancel()
	}
	defer sqlDB.Close()
	store := db.NewStore(sqlDB)

	// Build executor.
	cacheBucket := os.Getenv("CACHE_REF")
	exec, err := executor.New(buildkitAddr, cacheBucket)
	if err != nil {
		slog.Error("connect to buildkitd", "error", err)
		os.Exit(1)
	}
	defer exec.Close()

	// Build log store.
	ls, err := logstore.NewFromEnv(ctx)
	if err != nil {
		slog.Error("create log store", "error", err)
		os.Exit(1)
	}

	// HTTP client for docstore requests. The 5-minute timeout covers the
	// worst-case archive download; fetchConfig wraps its context with a
	// tighter 30-second deadline for the lightweight config fetch.
	httpClient := &http.Client{Timeout: 5 * time.Minute}

	// Start log HTTP server on :8081.
	logSrv := &checkLogServer{}
	logHTTP := &http.Server{
		Addr:    ":8081",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { logSrv.ServeHTTP(w, r) }),
	}
	go func() {
		if err := logHTTP.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("log server error", "error", err)
		}
	}()

	slog.Info("ci-worker started", "pod", podName, "pod_ip", podIP, "buildkit_addr", buildkitAddr)

	// Poll for a job to claim.
	for {
		job, err := store.ClaimCIJob(ctx, podName, podIP)
		if err != nil {
			slog.Error("claim job failed", "error", err)
			time.Sleep(1 * time.Second)
			continue
		}
		if job == nil {
			time.Sleep(1 * time.Second)
			continue
		}

		slog.Info("claimed job", "job_id", job.ID, "repo", job.Repo, "branch", job.Branch, "sequence", job.Sequence)

		// Start heartbeat goroutine.
		hbDone := make(chan struct{})
		go heartbeat(ctx, store, job.ID, hbDone)

		// Create a temp dir for in-progress check logs.
		logDir, err := os.MkdirTemp("", "ci-worker-logs-*")
		if err != nil {
			close(hbDone)
			slog.Error("create log dir", "error", err)
			msg := err.Error()
			_ = store.CompleteCIJob(ctx, job.ID, "failed", nil, &msg)
			break
		}
		defer os.RemoveAll(logDir)
		logSrv.setDir(logDir)

		// Build trigger context from claimed job.
		triggerCtx := ciconfig.TriggerContext{
			Type:       job.TriggerType,
			Branch:     job.TriggerBranch,
			BaseBranch: job.TriggerBaseBranch,
		}
		if job.TriggerProposalID != nil {
			triggerCtx.ProposalID = *job.TriggerProposalID
		}

		// Execute the job.
		jobStatus, jobLogURL, jobErrMsg := runJob(ctx, httpClient, exec, ls, docstoreURL, job, logDir, triggerCtx)

		// Stop heartbeat and clear log dir.
		close(hbDone)
		logSrv.setDir("")

		// Persist final status to DB.
		if err := store.CompleteCIJob(ctx, job.ID, jobStatus, jobLogURL, jobErrMsg); err != nil {
			slog.Error("complete job failed", "job_id", job.ID, "error", err)
		}
		slog.Info("job complete", "job_id", job.ID, "status", jobStatus)

		// Exit — the Deployment controller will schedule a fresh pod for the next job.
		break
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(ctx, 5*time.Second)
	defer shutdownCancel()
	_ = logHTTP.Shutdown(shutdownCtx)

	// Signal cloud-sql-proxy sidecar to exit so the Job pod reaches Succeeded.
	// Requires shareProcessNamespace: true on the pod spec.
	if out, err := exec2.Command("pkill", "-TERM", "cloud-sql-proxy").CombinedOutput(); err != nil {
		slog.Info("cloud-sql-proxy not signalled (not running?)", "output", string(out))
	}
}
