// Package main implements the ci-scheduler binary.
//
// ci-scheduler is responsible for:
//   - Receiving webhook deliveries from docstore and inserting ci_jobs rows
//   - Serving job status and log-proxy endpoints
//   - Reaping stale (heartbeat-missed) claimed jobs back to 'queued'
//
// It does NOT execute builds — that is the ci-worker's job.
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/lib/pq"
	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/dlorenc/docstore/internal/ciconfig"
	"github.com/dlorenc/docstore/internal/citoken"
	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/k8sproof"
	"github.com/dlorenc/docstore/internal/model"
)

// ---------------------------------------------------------------------------
// ciJobStore is the minimal interface the scheduler needs from the DB layer.
// ---------------------------------------------------------------------------

type ciJobStore interface {
	InsertCIJob(ctx context.Context, repo, branch string, sequence int64, triggerType, triggerBranch, triggerBaseBranch, triggerProposalID string) (*model.CIJob, error)
	GetCIJob(ctx context.Context, id string) (*model.CIJob, error)
	ClaimCIJob(ctx context.Context, podName, podIP string) (*model.CIJob, error)
	StoreRequestToken(ctx context.Context, jobID string, hashedToken string, exp time.Time) error
	LookupRequestToken(ctx context.Context, hashedToken string) (*model.CIJob, error)
	HeartbeatCIJob(ctx context.Context, id string) error
	CompleteCIJob(ctx context.Context, id, status string, logURL, errorMessage *string) error
	ReapStaleCIJobs(ctx context.Context) ([]model.CIJob, error)
	CountQueuedCIJobs(ctx context.Context) (int64, error)
}

// ---------------------------------------------------------------------------
// HMAC helper
// ---------------------------------------------------------------------------

// computeHMACHex returns the hex-encoded HMAC-SHA256 of body using secret.
// Matches the format sent by docstore's outbox dispatcher:
// "sha256=" + hex(hmac-sha256(body, secret))
func computeHMACHex(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// ---------------------------------------------------------------------------
// Webhook subscription registration
// ---------------------------------------------------------------------------

// registerWebhookSubscription checks whether a subscription for schedulerURL
// already exists and creates one if not. Errors are logged as warnings.
func registerWebhookSubscription(ctx context.Context, client *http.Client, docstoreURL, schedulerURL, webhookSecret string) {
	webhookURL := strings.TrimRight(schedulerURL, "/") + "/webhook"
	subsURL := strings.TrimRight(docstoreURL, "/") + "/subscriptions"

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

	type sub struct {
		ID     string          `json:"id"`
		Config json.RawMessage `json:"config"`
	}
	type listResp struct {
		Subscriptions []sub `json:"subscriptions"`
	}
	if resp.StatusCode == http.StatusOK {
		var lr listResp
		if err := json.NewDecoder(resp.Body).Decode(&lr); err == nil {
			for _, s := range lr.Subscriptions {
				var cfg map[string]string
				if err := json.Unmarshal(s.Config, &cfg); err != nil {
					continue
				}
				if cfg["url"] == webhookURL {
					slog.Info("webhook subscription already registered", "id", s.ID, "url", webhookURL)
					return
				}
			}
		}
	}

	cfgJSON, _ := json.Marshal(map[string]string{"url": webhookURL, "secret": webhookSecret})
	type createReq struct {
		Backend    string          `json:"backend"`
		EventTypes []string        `json:"event_types"`
		Config     json.RawMessage `json:"config"`
	}
	createBody, _ := json.Marshal(createReq{
		Backend:    "webhook",
		EventTypes: []string{"com.docstore.commit.created", "com.docstore.proposal.opened"},
		Config:     cfgJSON,
	})
	createReq2, err := http.NewRequestWithContext(ctx, http.MethodPost, subsURL, bytes.NewReader(createBody))
	if err != nil {
		slog.Warn("webhook registration: build create request failed", "error", err)
		return
	}
	createReq2.Header.Set("Content-Type", "application/json")
	createResp, err := client.Do(createReq2)
	if err != nil {
		slog.Warn("webhook registration: create subscription failed", "error", err)
		return
	}
	defer createResp.Body.Close()

	if createResp.StatusCode == http.StatusCreated {
		slog.Info("webhook subscription registered", "url", webhookURL)
	} else {
		slog.Warn("webhook registration: unexpected status",
			"status", createResp.StatusCode, "url", webhookURL)
	}
}

// claimValidator validates a worker pod's Kubernetes service account token.
// *k8sproof.PodClaimer satisfies this interface.
type claimValidator interface {
	ValidateToken(ctx context.Context, token string) (podName, podIP string, err error)
}

// ---------------------------------------------------------------------------
// scheduler holds handler dependencies
// ---------------------------------------------------------------------------

type scheduler struct {
	store         ciJobStore
	webhookSecret string
	docstoreURL   string
	httpClient    *http.Client
	claimer       claimValidator // nil in local dev (no in-cluster K8s)
	oidcTokenURL  string
}

// fetchCIConfig fetches and parses .docstore/ci.yaml from the given repo/branch
// at the given sequence. Returns nil if the file does not exist (no ci.yaml =
// no filtering). Returns an error only for unexpected failures.
func (s *scheduler) fetchCIConfig(ctx context.Context, repo, branch string, sequence int64) (*ciconfig.CIConfig, error) {
	if s.docstoreURL == "" {
		return nil, nil
	}
	fileURL := fmt.Sprintf("%s/repos/%s/-/file/.docstore/ci.yaml?branch=%s&at=%d",
		s.docstoreURL, repo, url.QueryEscape(branch), sequence)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build config request: %w", err)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch ci config: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // no ci.yaml = no filtering
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch ci config: unexpected status %d", resp.StatusCode)
	}
	var fileResp model.FileResponse
	if err := json.NewDecoder(resp.Body).Decode(&fileResp); err != nil {
		return nil, fmt.Errorf("decode ci config response: %w", err)
	}
	var cfg ciconfig.CIConfig
	if err := yaml.Unmarshal(fileResp.Content, &cfg); err != nil {
		return nil, fmt.Errorf("parse ci.yaml: %w", err)
	}
	return &cfg, nil
}

// fetchOpenProposalForBranch returns the open proposal for a branch, or nil if none exists.
func (s *scheduler) fetchOpenProposalForBranch(ctx context.Context, repo, branch string) (*model.Proposal, error) {
	if s.docstoreURL == "" {
		return nil, nil
	}
	proposalsURL := fmt.Sprintf("%s/repos/%s/-/proposals?state=open&branch=%s",
		s.docstoreURL, repo, url.QueryEscape(branch))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, proposalsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build proposals request: %w", err)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch proposals: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch proposals: unexpected status %d", resp.StatusCode)
	}
	var proposals []*model.Proposal
	if err := json.NewDecoder(resp.Body).Decode(&proposals); err != nil {
		return nil, fmt.Errorf("decode proposals response: %w", err)
	}
	if len(proposals) == 0 {
		return nil, nil
	}
	return proposals[0], nil
}

// handleWebhook handles POST /webhook — receives CloudEvents from docstore outbox.
func (s *scheduler) handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	// Verify HMAC signature when a secret is configured.
	if s.webhookSecret != "" {
		sig := r.Header.Get("X-DocStore-Signature")
		expected := "sha256=" + computeHMACHex(body, s.webhookSecret)
		if !hmac.Equal([]byte(sig), []byte(expected)) {
			http.Error(w, "invalid signature", http.StatusBadRequest)
			return
		}
	}

	// Parse CloudEvents envelope.
	var env struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		http.Error(w, "invalid cloudevent body", http.StatusBadRequest)
		return
	}

	switch env.Type {
	case "com.docstore.commit.created":
		s.handleCommitCreated(w, r, env.Data)
	case "com.docstore.proposal.opened":
		s.handleProposalOpened(w, r, env.Data)
	default:
		// Unknown event types are silently acknowledged (forward-compat).
		w.WriteHeader(http.StatusOK)
	}
}

// handleCommitCreated processes com.docstore.commit.created events.
func (s *scheduler) handleCommitCreated(w http.ResponseWriter, r *http.Request, raw json.RawMessage) {
	var data struct {
		Repo     string `json:"repo"`
		Branch   string `json:"branch"`
		Sequence int64  `json:"sequence"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		http.Error(w, "invalid event data", http.StatusBadRequest)
		return
	}
	if data.Repo == "" || data.Branch == "" {
		http.Error(w, "missing repo or branch in event data", http.StatusBadRequest)
		return
	}

	// Fetch CI config and evaluate push trigger filter.
	// Fail-open: if the config cannot be fetched, proceed with enqueueing.
	cfg, err := s.fetchCIConfig(r.Context(), data.Repo, data.Branch, data.Sequence)
	if err != nil {
		slog.Warn("could not fetch ci config, proceeding with enqueue", "repo", data.Repo, "branch", data.Branch, "error", err)
		cfg = nil
	}

	if cfg == nil || cfg.MatchesPush(data.Branch) {
		job, err := s.store.InsertCIJob(r.Context(), data.Repo, data.Branch, data.Sequence, "push", data.Branch, "", "")
		if err != nil {
			slog.Error("insert ci job failed", "repo", data.Repo, "branch", data.Branch, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		slog.Info("ci job queued (push)", "id", job.ID, "repo", data.Repo, "branch", data.Branch, "sequence", data.Sequence)
	} else {
		slog.Info("push trigger filtered by on: block", "repo", data.Repo, "branch", data.Branch)
	}

	// Also check if the branch has an open proposal and enqueue a proposal_synchronized job.
	proposal, err := s.fetchOpenProposalForBranch(r.Context(), data.Repo, data.Branch)
	if err != nil {
		slog.Warn("could not check open proposals, skipping proposal_synchronized trigger", "repo", data.Repo, "branch", data.Branch, "error", err)
	} else if proposal != nil {
		// Evaluate proposal trigger filter.
		proposalCfgMatch := true
		if cfg != nil {
			proposalCfgMatch = cfg.MatchesProposal(proposal.BaseBranch)
		}
		if proposalCfgMatch {
			job, err := s.store.InsertCIJob(r.Context(), data.Repo, data.Branch, data.Sequence, "proposal_synchronized", data.Branch, proposal.BaseBranch, proposal.ID)
			if err != nil {
				slog.Error("insert proposal_synchronized ci job failed", "repo", data.Repo, "branch", data.Branch, "proposal_id", proposal.ID, "error", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			slog.Info("ci job queued (proposal_synchronized)", "id", job.ID, "repo", data.Repo, "branch", data.Branch, "proposal_id", proposal.ID)
		} else {
			slog.Info("proposal_synchronized trigger filtered by on: block", "repo", data.Repo, "branch", data.Branch)
		}
	}

	w.WriteHeader(http.StatusOK)
}

// handleProposalOpened processes com.docstore.proposal.opened events.
func (s *scheduler) handleProposalOpened(w http.ResponseWriter, r *http.Request, raw json.RawMessage) {
	var data struct {
		Repo       string `json:"repo"`
		Branch     string `json:"branch"`
		BaseBranch string `json:"base_branch"`
		ProposalID string `json:"proposal_id"`
		Author     string `json:"author"`
		Sequence   int64  `json:"sequence"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		http.Error(w, "invalid event data", http.StatusBadRequest)
		return
	}
	if data.Repo == "" || data.Branch == "" {
		http.Error(w, "missing repo or branch in event data", http.StatusBadRequest)
		return
	}

	sequence := data.Sequence

	// Fetch CI config and evaluate proposal trigger filter.
	cfg, err := s.fetchCIConfig(r.Context(), data.Repo, data.Branch, sequence)
	if err != nil {
		slog.Warn("could not fetch ci config, proceeding with enqueue", "repo", data.Repo, "branch", data.Branch, "error", err)
		cfg = nil
	}
	if cfg != nil && !cfg.MatchesProposal(data.BaseBranch) {
		slog.Info("proposal trigger filtered by on: block", "repo", data.Repo, "branch", data.Branch, "base_branch", data.BaseBranch)
		w.WriteHeader(http.StatusOK)
		return
	}

	job, err := s.store.InsertCIJob(r.Context(), data.Repo, data.Branch, sequence, "proposal", data.Branch, data.BaseBranch, data.ProposalID)
	if err != nil {
		slog.Error("insert proposal ci job failed", "repo", data.Repo, "branch", data.Branch, "proposal_id", data.ProposalID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	slog.Info("ci job queued (proposal)", "id", job.ID, "repo", data.Repo, "branch", data.Branch, "proposal_id", data.ProposalID)
	w.WriteHeader(http.StatusOK)
}

// runStatusResponse is the JSON shape returned by GET /run/{id}.
type runStatusResponse struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// handleGetRun handles GET /run/{id} — returns job status from ci_jobs.
func (s *scheduler) handleGetRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, err := s.store.GetCIJob(r.Context(), id)
	if errors.Is(err, db.ErrCIJobNotFound) {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("get ci job failed", "id", id, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	resp := runStatusResponse{
		RunID:  job.ID,
		Status: job.Status,
	}
	if job.ErrorMessage != nil {
		resp.Error = *job.ErrorMessage
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp) //nolint:errcheck
}

// handleGetLogs handles GET /run/{id}/logs/{check}.
// If the job is claimed and has a worker_pod_ip, it reverse-proxies to the
// worker. Otherwise it redirects to the job's log_url.
func (s *scheduler) handleGetLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	check := r.PathValue("check")

	job, err := s.store.GetCIJob(r.Context(), id)
	if errors.Is(err, db.ErrCIJobNotFound) {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("get ci job for logs failed", "id", id, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Live proxy to worker if job is claimed and worker IP is known.
	if job.Status == "claimed" && job.WorkerPodIP != nil {
		workerURL, err := url.Parse(fmt.Sprintf("http://%s:8081", *job.WorkerPodIP))
		if err != nil {
			http.Error(w, "invalid worker address", http.StatusInternalServerError)
			return
		}
		proxy := httputil.NewSingleHostReverseProxy(workerURL)
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/logs/" + check
		r2.URL.RawQuery = ""
		r2.Host = workerURL.Host
		proxy.ServeHTTP(w, r2)
		return
	}

	// Fall back to redirect to stored log URL.
	if job.LogURL != nil && *job.LogURL != "" {
		http.Redirect(w, r, *job.LogURL, http.StatusFound)
		return
	}

	http.Error(w, "logs not yet available", http.StatusNotFound)
}

// ---------------------------------------------------------------------------
// HTTP mux
// ---------------------------------------------------------------------------

// handleQueueDepth returns the current number of queued CI jobs as JSON.
// Used by the KEDA metrics-api scaler to drive autoscaling of ci-worker.
func (s *scheduler) handleQueueDepth(w http.ResponseWriter, r *http.Request) {
	n, err := s.store.CountQueuedCIJobs(r.Context())
	if err != nil {
		http.Error(w, "failed to count queued jobs", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"queue_depth":%d}`, n)
}

// claimResponse is the JSON body returned by POST /claim on success.
type claimResponse struct {
	Job          model.CIJob `json:"job"`
	RequestToken string      `json:"request_token"`
	OIDCTokenURL string      `json:"oidc_token_url"`
}

// handleClaim handles POST /claim.
// Workers authenticate with their K8s projected service account token.
// On success the oldest queued job is claimed and a request_token is returned.
// Returns 204 if no jobs are queued.
func (s *scheduler) handleClaim(w http.ResponseWriter, r *http.Request) {
	// Extract Bearer token from Authorization header.
	authHdr := r.Header.Get("Authorization")
	saToken := strings.TrimPrefix(authHdr, "Bearer ")

	var podName, podIP string
	if s.claimer != nil {
		var err error
		podName, podIP, err = s.claimer.ValidateToken(r.Context(), saToken)
		if err != nil {
			slog.Warn("k8s token validation failed", "error", err)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	// If claimer is nil (local dev), allow with empty podName/podIP.

	job, err := s.store.ClaimCIJob(r.Context(), podName, podIP)
	if err != nil {
		slog.Error("claim job failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if job == nil {
		// No queued jobs available.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Generate a one-time request token; store its hash in the DB.
	plaintext, hashed, err := citoken.GenerateRequestToken()
	if err != nil {
		slog.Error("generate request token failed", "job_id", job.ID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	exp := time.Now().Add(24 * time.Hour)
	if err := s.store.StoreRequestToken(r.Context(), job.ID, hashed, exp); err != nil {
		slog.Error("store request token failed", "job_id", job.ID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	slog.Info("job claimed via API", "job_id", job.ID, "pod", podName)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(claimResponse{ //nolint:errcheck
		Job:          *job,
		RequestToken: plaintext,
		OIDCTokenURL: s.oidcTokenURL,
	})
}

// handleHeartbeat handles POST /jobs/{id}/heartbeat.
// Authenticated with a request_token in the Authorization: Bearer header.
func (s *scheduler) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")
	job, err := s.lookupByToken(r)
	if errors.Is(err, db.ErrTokenInvalid) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err != nil {
		slog.Error("heartbeat token lookup failed", "job_id", jobID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if job.ID != jobID {
		http.Error(w, "token/job mismatch", http.StatusUnauthorized)
		return
	}
	if err := s.store.HeartbeatCIJob(r.Context(), job.ID); err != nil {
		slog.Error("heartbeat failed", "job_id", job.ID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleComplete handles POST /jobs/{id}/complete.
// Authenticated with a request_token in the Authorization: Bearer header.
func (s *scheduler) handleComplete(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")
	job, err := s.lookupByToken(r)
	if errors.Is(err, db.ErrTokenInvalid) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err != nil {
		slog.Error("complete token lookup failed", "job_id", jobID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if job.ID != jobID {
		http.Error(w, "token/job mismatch", http.StatusUnauthorized)
		return
	}

	var req struct {
		Status       string  `json:"status"`
		LogURL       *string `json:"log_url,omitempty"`
		ErrorMessage *string `json:"error_message,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Status == "" {
		http.Error(w, "status is required", http.StatusBadRequest)
		return
	}
	if req.Status != "passed" && req.Status != "failed" {
		http.Error(w, "status must be 'passed' or 'failed'", http.StatusBadRequest)
		return
	}

	if err := s.store.CompleteCIJob(r.Context(), job.ID, req.Status, req.LogURL, req.ErrorMessage); err != nil {
		slog.Error("complete job failed", "job_id", job.ID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	slog.Info("job completed via API", "job_id", job.ID, "status", req.Status)
	w.WriteHeader(http.StatusNoContent)
}

// lookupByToken extracts the Bearer token from the request and looks up the
// corresponding ci_job. Returns db.ErrTokenInvalid if the token is missing,
// invalid, expired, or the job is not claimed.
func (s *scheduler) lookupByToken(r *http.Request) (*model.CIJob, error) {
	authHdr := r.Header.Get("Authorization")
	plaintext := strings.TrimPrefix(authHdr, "Bearer ")
	if plaintext == "" || plaintext == authHdr {
		return nil, db.ErrTokenInvalid
	}
	hashed := citoken.HashRequestToken(plaintext)
	return s.store.LookupRequestToken(r.Context(), hashed)
}

func newMux(sched *scheduler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook", sched.handleWebhook)
	mux.HandleFunc("GET /run/{id}", sched.handleGetRun)
	mux.HandleFunc("GET /run/{id}/logs/{check}", sched.handleGetLogs)
	mux.HandleFunc("GET /queue-depth", sched.handleQueueDepth)
	mux.HandleFunc("POST /claim", sched.handleClaim)
	mux.HandleFunc("POST /jobs/{id}/heartbeat", sched.handleHeartbeat)
	mux.HandleFunc("POST /jobs/{id}/complete", sched.handleComplete)
	return mux
}

// ---------------------------------------------------------------------------
// Schedule-based cron runner
// ---------------------------------------------------------------------------

// fetchBranchHead fetches the head sequence for a named branch from docstore.
func (s *scheduler) fetchBranchHead(ctx context.Context, repo, branch string) (int64, error) {
	if s.docstoreURL == "" {
		return 0, fmt.Errorf("docstore URL not configured")
	}
	branchesURL := fmt.Sprintf("%s/repos/%s/-/branches", s.docstoreURL, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, branchesURL, nil)
	if err != nil {
		return 0, fmt.Errorf("build branches request: %w", err)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("fetch branches: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("fetch branches: unexpected status %d", resp.StatusCode)
	}
	var branches []model.Branch
	if err := json.NewDecoder(resp.Body).Decode(&branches); err != nil {
		return 0, fmt.Errorf("decode branches response: %w", err)
	}
	for _, b := range branches {
		if b.Name == branch {
			return b.HeadSequence, nil
		}
	}
	return 0, fmt.Errorf("branch %q not found in repo %q", branch, repo)
}

// fetchAllRepos fetches all repo names from docstore (GET /repos).
func (s *scheduler) fetchAllRepos(ctx context.Context) ([]string, error) {
	if s.docstoreURL == "" {
		return nil, nil
	}
	reposURL := s.docstoreURL + "/repos"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reposURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build repos request: %w", err)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch repos: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch repos: unexpected status %d", resp.StatusCode)
	}
	var result model.ReposResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode repos response: %w", err)
	}
	names := make([]string, 0, len(result.Repos))
	for _, r := range result.Repos {
		names = append(names, r.Name)
	}
	return names, nil
}

// runScheduledJobs checks all repos for schedule-triggered CI jobs that should
// fire at time t (truncated to the minute).
func (s *scheduler) runScheduledJobs(ctx context.Context, t time.Time) {
	repos, err := s.fetchAllRepos(ctx)
	if err != nil {
		slog.Error("schedule runner: fetch repos failed", "error", err)
		return
	}
	now := t.Truncate(time.Minute)
	for _, repo := range repos {
		headSeq, err := s.fetchBranchHead(ctx, repo, "main")
		if err != nil {
			slog.Warn("schedule runner: fetch branch head failed", "repo", repo, "error", err)
			continue
		}
		cfg, err := s.fetchCIConfig(ctx, repo, "main", headSeq)
		if err != nil {
			slog.Warn("schedule runner: fetch ci config failed", "repo", repo, "error", err)
			continue
		}
		if cfg == nil || cfg.On == nil || len(cfg.On.Schedule) == 0 {
			continue
		}
		for _, entry := range cfg.On.Schedule {
			sched, err := cron.ParseStandard(entry.Cron)
			if err != nil {
				slog.Warn("schedule runner: invalid cron expression", "repo", repo, "cron", entry.Cron, "error", err)
				continue
			}
			// Check if the cron fires at this minute: the next fire time after
			// (now - 1 minute) must equal now.
			if sched.Next(now.Add(-time.Minute)).Equal(now) {
				job, err := s.store.InsertCIJob(ctx, repo, "main", headSeq, "schedule", "main", "", "")
				if err != nil {
					slog.Error("schedule runner: insert ci job failed", "repo", repo, "cron", entry.Cron, "error", err)
					continue
				}
				slog.Info("ci job queued (schedule)", "id", job.ID, "repo", repo, "cron", entry.Cron)
			}
		}
	}
}

// startCronRunner starts a goroutine that fires every minute and enqueues
// schedule-triggered CI jobs for all repos whose ci.yaml schedules match.
func startCronRunner(ctx context.Context, sched *scheduler) {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case t := <-ticker.C:
				sched.runScheduledJobs(ctx, t)
			}
		}
	}()
}

// ---------------------------------------------------------------------------
// Stale job reaper
// ---------------------------------------------------------------------------

func startReaper(ctx context.Context, store ciJobStore, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				jobs, err := store.ReapStaleCIJobs(ctx)
				if err != nil {
					slog.Error("reap stale ci jobs failed", "error", err)
					continue
				}
				for _, j := range jobs {
					slog.Info("reclaimed stale ci job", "id", j.ID, "repo", j.Repo, "branch", j.Branch)
				}
			}
		}
	}()
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	port := flag.String("port", "8080", "HTTP listen port")
	docstoreURL := flag.String("docstore-url", "", "Base URL of the docstore server")
	schedulerURL := flag.String("scheduler-url", "", "Public URL of this ci-scheduler (used to register webhook subscription)")
	webhookSecret := flag.String("webhook-secret", "", "Shared HMAC secret for webhook signature verification")
	oidcTokenURL := flag.String("oidc-token-url", "", "URL of the CI OIDC token endpoint (returned to workers on /claim)")
	flag.Parse()

	// Also accept env-var overrides so the binary is container-friendly.
	if *docstoreURL == "" {
		*docstoreURL = os.Getenv("DOCSTORE_URL")
	}
	if *schedulerURL == "" {
		*schedulerURL = os.Getenv("RUNNER_URL") // backward-compat name used by ci-runner
	}
	if *webhookSecret == "" {
		*webhookSecret = os.Getenv("WEBHOOK_SECRET")
	}
	if *oidcTokenURL == "" {
		*oidcTokenURL = os.Getenv("OIDC_TOKEN_URL")
	}
	if *oidcTokenURL == "" {
		*oidcTokenURL = "https://oidc.docstore.dev/ci/token"
	}

	// Set up structured logging.
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

	// Connect to Postgres.
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		slog.Error("DATABASE_URL environment variable is required")
		os.Exit(1)
	}
	database, err := db.Open(dsn)
	if err != nil {
		slog.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer database.Close()

	store := db.NewStore(database)

	serverCtx, serverCancel := context.WithCancel(context.Background())
	defer serverCancel()

	// Start stale job reaper.
	startReaper(serverCtx, store, 30*time.Second)

	httpClient := &http.Client{Timeout: 30 * time.Second}

	// Build K8s in-cluster client for pod provenance checks.
	// Log a warning and disable provenance (allow all claims) if not in cluster.
	var claimer *k8sproof.PodClaimer
	k8sCfg, k8sErr := rest.InClusterConfig()
	if k8sErr != nil {
		slog.Warn("not running in-cluster — k8s provenance checks disabled", "error", k8sErr)
	} else {
		k8sClient, k8sClientErr := kubernetes.NewForConfig(k8sCfg)
		if k8sClientErr != nil {
			slog.Warn("failed to build k8s client — provenance checks disabled", "error", k8sClientErr)
		} else {
			claimer = k8sproof.New(k8sClient, "docstore-ci")
		}
	}

	sched := &scheduler{
		store:         store,
		webhookSecret: *webhookSecret,
		docstoreURL:   strings.TrimRight(*docstoreURL, "/"),
		httpClient:    httpClient,
		claimer:       claimer,
		oidcTokenURL:  *oidcTokenURL,
	}

	// Start schedule-based cron runner.
	startCronRunner(serverCtx, sched)

	// WriteTimeout is intentionally absent: the log proxy endpoint
	// (GET /run/{id}/logs/{check}) streams responses from the worker and
	// must not be cut off by a server-wide write deadline.
	srv := &http.Server{
		Addr:        ":" + *port,
		Handler:     newMux(sched),
		ReadTimeout: 10 * time.Second,
		IdleTimeout: 60 * time.Second,
	}

	go func() {
		slog.Info("starting ci-scheduler", "port", *port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	// Register webhook subscription after server is up.
	if *schedulerURL != "" && *webhookSecret != "" && *docstoreURL != "" {
		regCtx, regCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer regCancel()
		registerWebhookSubscription(regCtx, &http.Client{}, *docstoreURL, *schedulerURL, *webhookSecret)
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
	serverCancel()
	slog.Info("stopped")
}
