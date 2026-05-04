package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/dlorenc/docstore/internal/ciconfig"
	"github.com/dlorenc/docstore/internal/executor"
	"github.com/dlorenc/docstore/internal/model"
)

// runner executes CI checks against a source directory.
// *executor.Executor satisfies this interface.
type runner interface {
	Run(ctx context.Context, sourceDir string, cfg executor.Config, triggerCtx ciconfig.TriggerContext) ([]executor.CheckResult, error)
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
	return cmp.Or(os.Getenv(key), def)
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

// ---------------------------------------------------------------------------
// Scheduler API helpers
// ---------------------------------------------------------------------------

// claimResponse is the JSON body returned by POST /claim on success.
type claimResponse struct {
	Job           model.CIJob `json:"job"`
	RequestToken  string      `json:"request_token"`
	OIDCTokenURL  string      `json:"oidc_token_url"`
	CacheRegistry string      `json:"cache_registry,omitempty"`
}

// readSAToken reads the Kubernetes projected service account token from the
// default mount path. Returns empty string if the file does not exist
// (local dev mode without a real K8s cluster).
func readSAToken() string {
	data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// claimJob calls POST /claim on ci-scheduler using the pod's SA token.
// Returns nil if no job is available (204 response).
func claimJob(ctx context.Context, schedulerURL string) (*claimResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(schedulerURL, "/")+"/claim", nil)
	if err != nil {
		return nil, fmt.Errorf("build claim request: %w", err)
	}
	if token := readSAToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("claim request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return nil, nil // no job available
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("claim: unexpected status %d", resp.StatusCode)
	}

	var cr claimResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return nil, fmt.Errorf("decode claim response: %w", err)
	}
	return &cr, nil
}

// heartbeatHTTP sends a heartbeat for the given job via the scheduler API.
func heartbeatHTTP(ctx context.Context, schedulerURL, jobID, requestToken string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/jobs/%s/heartbeat", strings.TrimRight(schedulerURL, "/"), jobID), nil)
	if err != nil {
		return fmt.Errorf("build heartbeat request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+requestToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("heartbeat request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("heartbeat: unexpected status %d", resp.StatusCode)
	}
	return nil
}

// completeJob reports job completion to the scheduler API.
func completeJob(ctx context.Context, schedulerURL, jobID, requestToken, status string, logURL, errMsg *string) error {
	body := struct {
		Status       string  `json:"status"`
		LogURL       *string `json:"log_url,omitempty"`
		ErrorMessage *string `json:"error_message,omitempty"`
	}{
		Status:       status,
		LogURL:       logURL,
		ErrorMessage: errMsg,
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal complete body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/jobs/%s/complete", strings.TrimRight(schedulerURL, "/"), jobID),
		bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("build complete request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+requestToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("complete request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("complete: unexpected status %d", resp.StatusCode)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Heartbeat goroutine
// ---------------------------------------------------------------------------

// heartbeat sends periodic last_heartbeat_at updates until done is closed.
func heartbeat(ctx context.Context, schedulerURL, jobID, requestToken string, done <-chan struct{}) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := heartbeatHTTP(ctx, schedulerURL, jobID, requestToken); err != nil {
				slog.Warn("heartbeat failed", "job_id", jobID, "error", err)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Docstore API helpers
// ---------------------------------------------------------------------------

// getPresignedArchiveURL calls POST /repos/{repo}/-/archive/presign with the
// request_token and returns the presigned URL and checksum for BuildKit to fetch
// the archive. checksum is "sha256:<hex>" when the server computed it, or ""
// if the server did not provide one. This endpoint bypasses OAuth and uses the
// request_token for authentication.
func getPresignedArchiveURL(ctx context.Context, docstoreURL, repo, requestToken string) (url, checksum string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	u := fmt.Sprintf("%s/repos/%s/-/archive/presign", docstoreURL, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return "", "", fmt.Errorf("build presign request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+requestToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("presign request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("presign: unexpected status %d", resp.StatusCode)
	}
	var body struct {
		URL      string `json:"url"`
		Checksum string `json:"checksum"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", "", fmt.Errorf("decode presign response: %w", err)
	}
	if body.URL == "" {
		return "", "", fmt.Errorf("presign: empty URL in response")
	}
	return body.URL, body.Checksum, nil
}

// postCheckLogs uploads the logs for a check run to the docstore server via
// POST /repos/{repo}/-/check/{checkName}/logs. The server writes the log to GCS
// using the job metadata bound to the request_token. Returns the log URL on success.
func postCheckLogs(ctx context.Context, docstoreURL, repo, checkName, requestToken, logs string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	u := fmt.Sprintf("%s/repos/%s/-/check/%s/logs", docstoreURL, repo, url.PathEscape(checkName))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(logs))
	if err != nil {
		return "", fmt.Errorf("build log upload request: %w", err)
	}
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Authorization", "Bearer "+requestToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("log upload request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("log upload: unexpected status %d", resp.StatusCode)
	}
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decode log upload response: %w", err)
	}
	if body.URL == "" {
		return "", fmt.Errorf("log upload: empty URL in response")
	}
	return body.URL, nil
}

// getDocstoreOIDCToken exchanges the request_token for a short-lived OIDC JWT
// with aud=docstore. This token is used to authenticate API calls to docstore.
// Returns empty string if oidcTokenURL is empty (local dev / OIDC not configured).
func getDocstoreOIDCToken(ctx context.Context, oidcTokenURL, requestToken string) (string, error) {
	if oidcTokenURL == "" {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	body, err := json.Marshal(map[string]string{"audience": "docstore"})
	if err != nil {
		return "", fmt.Errorf("marshal token request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oidcTokenURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+requestToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token request: unexpected status %d", resp.StatusCode)
	}

	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if result.Token == "" {
		return "", fmt.Errorf("token response missing token field")
	}
	return result.Token, nil
}

// getCIRegistryToken exchanges the request_token for a short-lived OIDC JWT
// with aud=ci-registry for authenticating against the CI cache registry.
// Returns empty string if oidcTokenURL is empty (local dev / OIDC not configured).
func getCIRegistryToken(ctx context.Context, oidcTokenURL, requestToken string) (string, error) {
	if oidcTokenURL == "" {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	body, err := json.Marshal(map[string]string{"audience": "ci-registry"})
	if err != nil {
		return "", fmt.Errorf("marshal token request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oidcTokenURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+requestToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token request: unexpected status %d", resp.StatusCode)
	}

	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if result.Token == "" {
		return "", fmt.Errorf("token response missing token field")
	}
	return result.Token, nil
}

// registryHost returns the host[:port] portion of a registry reference.
// Input may be "host:port/path" (no scheme) or "scheme://host:port/path".
func registryHost(registry string) string {
	if strings.Contains(registry, "://") {
		u, err := url.Parse(registry)
		if err == nil {
			return u.Host
		}
	}
	// No scheme — first path component is host[:port].
	host, _, _ := strings.Cut(registry, "/")
	return host
}

// writeCacheDockerConfig writes a temporary Docker config.json that stores
// Basic auth credentials for registryHost, using token as the password.
// BuildKit's docker auth provider reads this when authenticating against
// the cache registry. The caller is responsible for removing the directory.
func writeCacheDockerConfig(regHost, token string) (string, error) {
	dir, err := os.MkdirTemp("", "ci-docker-*")
	if err != nil {
		return "", fmt.Errorf("create docker config dir: %w", err)
	}
	// Docker auth format: base64("ci-worker:<token>").
	creds := base64.StdEncoding.EncodeToString([]byte("ci-worker:" + token))
	cfg := map[string]any{
		"auths": map[string]any{
			regHost: map[string]string{
				"auth": creds,
			},
		},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("marshal docker config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o600); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("write docker config: %w", err)
	}
	return dir, nil
}

func fetchConfig(ctx context.Context, client *http.Client, docstoreURL, repo, requestToken string) (*executor.Config, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	u := fmt.Sprintf("%s/repos/%s/-/ci/config", docstoreURL, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("create config request: %w", err)
	}
	if requestToken != "" {
		req.Header.Set("Authorization", "Bearer "+requestToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch config: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
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

func postCheckRun(ctx context.Context, client *http.Client, docstoreURL, repo string, requestToken string, req model.CreateCheckRunRequest) error {
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
	if requestToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+requestToken)
	}
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
// requestToken is used to authenticate all docstore API calls (ci/config and check).
func runJob(
	ctx context.Context,
	httpClient *http.Client,
	exec runner,
	docstoreURL string,
	job *model.CIJob,
	requestToken string,
	oidcTokenURL string,
	logDir string,
	triggerCtx ciconfig.TriggerContext,
	cacheRef string,
	dockerConfigDir string,
) (status string, logURL *string, errMsg *string) {
	fail := func(msg string) (string, *string, *string) {
		slog.Error("job failed", "job_id", job.ID, "reason", msg)
		return "failed", nil, &msg
	}

	// 1. Fetch CI config from the branch under test at its head sequence.
	cfg, err := fetchConfig(ctx, httpClient, docstoreURL, job.Repo, requestToken)
	if err != nil {
		_ = postCheckRun(ctx, httpClient, docstoreURL, job.Repo, requestToken, model.CreateCheckRunRequest{
			Branch:    job.Branch,
			CheckName: "ci/config",
			Status:    model.CheckRunFailed,
			Sequence:  &job.Sequence,
		})
		return fail(err.Error())
	}
	if cfg == nil {
		slog.Info("no ci.yaml on branch, skipping CI", "job_id", job.ID, "branch", job.Branch)
		return "passed", nil, nil
	}

	// 2. Obtain a presigned archive URL for BuildKit to fetch the source.
	archiveURL, archiveChecksum, err := getPresignedArchiveURL(ctx, docstoreURL, job.Repo, requestToken)
	if err != nil {
		return fail(fmt.Sprintf("get presigned archive URL: %v", err))
	}

	// 3. Mark each check as pending (in parallel).
	var pendingWg sync.WaitGroup
	for _, check := range cfg.Checks {
		pendingWg.Add(1)
		go func(checkName string) {
			defer pendingWg.Done()
			if err := postCheckRun(ctx, httpClient, docstoreURL, job.Repo, requestToken, model.CreateCheckRunRequest{
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
	// Set runtime fields on cfg (not from ci.yaml).
	cfg.CacheRef = cacheRef
	cfg.DockerConfigDir = dockerConfigDir
	cfg.ArchiveChecksum = archiveChecksum
	cfg.OIDCRequestToken = requestToken
	cfg.OIDCRequestURL = oidcTokenURL
	results, err := exec.Run(ctx, archiveURL, *cfg, triggerCtx)
	if err != nil {
		return fail(fmt.Sprintf("executor: %v", err))
	}

	// 5. Upload logs and post results.
	overallStatus := "passed"
	var firstLogURL *string
	for _, result := range results {
		safeName := strings.ReplaceAll(result.Name, "/", "_")
		_ = os.WriteFile(filepath.Join(logDir, safeName+".log"), []byte(result.Logs), 0o644)

		var checkLogURL *string
		u, err := postCheckLogs(ctx, docstoreURL, job.Repo, result.Name, requestToken, result.Logs)
		if err != nil {
			slog.Warn("log upload failed", "check", result.Name, "error", err)
		} else {
			checkLogURL = &u
			if firstLogURL == nil {
				firstLogURL = &u
			}
		}

		if err := postCheckRun(ctx, httpClient, docstoreURL, job.Repo, requestToken, model.CreateCheckRunRequest{
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
	schedulerURL := mustEnv("CI_SCHEDULER_URL")
	docstoreURL := mustEnv("DOCSTORE_URL")

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

	// Build executor.
	exec, err := executor.New(buildkitAddr)
	if err != nil {
		slog.Error("connect to buildkitd", "error", err)
		os.Exit(1)
	}
	defer exec.Close()

	// HTTP client for docstore requests.
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

	slog.Info("ci-worker started", "scheduler", schedulerURL, "buildkit_addr", buildkitAddr)

	// Poll for a job to claim via the scheduler API.
	for {
		cr, err := claimJob(ctx, schedulerURL)
		if err != nil {
			slog.Error("claim job failed", "error", err)
			time.Sleep(1 * time.Second)
			continue
		}
		if cr == nil {
			time.Sleep(1 * time.Second)
			continue
		}

		job := &cr.Job
		requestToken := cr.RequestToken

		slog.Info("claimed job", "job_id", job.ID, "repo", job.Repo, "branch", job.Branch, "sequence", job.Sequence)

		// Start heartbeat goroutine.
		hbDone := make(chan struct{})
		go heartbeat(ctx, schedulerURL, job.ID, requestToken, hbDone)

		// Create a temp dir for in-progress check logs.
		logDir, err := os.MkdirTemp("", "ci-worker-logs-*")
		if err != nil {
			close(hbDone)
			slog.Error("create log dir", "error", err)
			msg := err.Error()
			_ = completeJob(ctx, schedulerURL, job.ID, requestToken, "failed", nil, &msg)
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

		// Set up BuildKit registry cache if the scheduler provided a registry.
		var cacheRef, cacheDockerConfigDir string
		if cr.CacheRegistry != "" {
			org, repoName, _ := strings.Cut(job.Repo, "/")
			if org != "" && repoName != "" {
				regToken, err := getCIRegistryToken(ctx, cr.OIDCTokenURL, requestToken)
				if err != nil {
					slog.Warn("get ci-registry OIDC token failed, cache disabled", "job_id", job.ID, "error", err)
				} else if regToken != "" {
					regHost := registryHost(cr.CacheRegistry)
					dir, err := writeCacheDockerConfig(regHost, regToken)
					if err != nil {
						slog.Warn("write cache docker config failed, cache disabled", "job_id", job.ID, "error", err)
					} else {
						cacheRef = cr.CacheRegistry + "/" + org + "/" + repoName + ":buildkit"
						cacheDockerConfigDir = dir
						defer os.RemoveAll(dir)
					}
				}
				// If regToken is empty, OIDC is not configured; cache silently disabled.
			}
		}

		// Execute the job.
		jobStatus, jobLogURL, jobErrMsg := runJob(ctx, httpClient, exec, docstoreURL, job, requestToken, cr.OIDCTokenURL, logDir, triggerCtx, cacheRef, cacheDockerConfigDir)

		// Stop heartbeat and clear log dir.
		close(hbDone)
		logSrv.setDir("")

		// Report final status via scheduler API.
		if err := completeJob(ctx, schedulerURL, job.ID, requestToken, jobStatus, jobLogURL, jobErrMsg); err != nil {
			slog.Error("complete job failed", "job_id", job.ID, "error", err)
		}
		slog.Info("job complete", "job_id", job.ID, "status", jobStatus)

		// Exit — KEDA creates a fresh pod for the next job.
		break
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(ctx, 5*time.Second)
	defer shutdownCancel()
	_ = logHTTP.Shutdown(shutdownCtx)
}
