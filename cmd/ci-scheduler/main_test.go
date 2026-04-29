package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dlorenc/docstore/internal/model"
)

// ---------------------------------------------------------------------------
// stubStore is an in-memory ciJobStore for unit tests.
// ---------------------------------------------------------------------------

type stubStore struct {
	insertedJobs []*model.CIJob
	getJob       *model.CIJob
	getErr       error
	reapJobs     []model.CIJob
	reapErr      error
	queueDepth   int64
}

func (s *stubStore) InsertCIJob(_ context.Context, repo, branch string, sequence int64, triggerType, triggerBranch, triggerBaseBranch, triggerProposalID string) (*model.CIJob, error) {
	j := &model.CIJob{
		ID:                "test-uuid",
		Repo:              repo,
		Branch:            branch,
		Sequence:          sequence,
		Status:            "queued",
		TriggerType:       triggerType,
		TriggerBranch:     triggerBranch,
		TriggerBaseBranch: triggerBaseBranch,
	}
	if triggerProposalID != "" {
		j.TriggerProposalID = &triggerProposalID
	}
	s.insertedJobs = append(s.insertedJobs, j)
	return j, nil
}

func (s *stubStore) GetCIJob(_ context.Context, id string) (*model.CIJob, error) {
	return s.getJob, s.getErr
}

func (s *stubStore) ClaimCIJob(_ context.Context, podName, podIP string) (*model.CIJob, error) {
	return nil, nil
}

func (s *stubStore) StoreRequestToken(_ context.Context, jobID string, hashedToken string, exp time.Time) error {
	return nil
}

func (s *stubStore) LookupRequestToken(_ context.Context, hashedToken string) (*model.CIJob, error) {
	return nil, nil
}

func (s *stubStore) HeartbeatCIJob(_ context.Context, id string) error {
	return nil
}

func (s *stubStore) CompleteCIJob(_ context.Context, id, status string, logURL, errorMessage *string) error {
	return nil
}

func (s *stubStore) ReapStaleCIJobs(_ context.Context) ([]model.CIJob, error) {
	return s.reapJobs, s.reapErr
}

func (s *stubStore) CountQueuedCIJobs(_ context.Context) (int64, error) {
	return s.queueDepth, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func signBody(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func proposalOpenedEvent(repo, branch, baseBranch, proposalID string) []byte {
	type data struct {
		Repo       string `json:"repo"`
		Branch     string `json:"branch"`
		BaseBranch string `json:"base_branch"`
		ProposalID string `json:"proposal_id"`
	}
	type env struct {
		Type string `json:"type"`
		Data data   `json:"data"`
	}
	b, _ := json.Marshal(env{
		Type: "com.docstore.proposal.opened",
		Data: data{Repo: repo, Branch: branch, BaseBranch: baseBranch, ProposalID: proposalID},
	})
	return b
}

func commitCreatedEvent(repo, branch string, sequence int64) []byte {
	type data struct {
		Repo     string `json:"repo"`
		Branch   string `json:"branch"`
		Sequence int64  `json:"sequence"`
	}
	type env struct {
		Type string `json:"type"`
		Data data   `json:"data"`
	}
	b, _ := json.Marshal(env{
		Type: "com.docstore.commit.created",
		Data: data{Repo: repo, Branch: branch, Sequence: sequence},
	})
	return b
}

// ---------------------------------------------------------------------------
// Tests: HMAC signature verification
// ---------------------------------------------------------------------------

func TestHandleWebhook_InvalidSignature(t *testing.T) {
	stub := &stubStore{}
	sched := &scheduler{store: stub, webhookSecret: "secret"}

	body := commitCreatedEvent("myrepo", "main", 1)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-DocStore-Signature", "sha256=badhex")
	w := httptest.NewRecorder()

	sched.handleWebhook(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if len(stub.insertedJobs) != 0 {
		t.Fatal("expected no jobs inserted on bad signature")
	}
}

func TestHandleWebhook_MissingSignature(t *testing.T) {
	stub := &stubStore{}
	sched := &scheduler{store: stub, webhookSecret: "secret"}

	body := commitCreatedEvent("myrepo", "feat", 2)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	// No X-DocStore-Signature header.
	w := httptest.NewRecorder()

	sched.handleWebhook(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleWebhook_NoSecret_AcceptsAll(t *testing.T) {
	stub := &stubStore{}
	sched := &scheduler{store: stub, webhookSecret: ""}

	body := commitCreatedEvent("myrepo", "feat", 3)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	// No signature needed when secret is empty.
	w := httptest.NewRecorder()

	sched.handleWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if len(stub.insertedJobs) != 1 {
		t.Fatalf("expected 1 inserted job, got %d", len(stub.insertedJobs))
	}
}

// ---------------------------------------------------------------------------
// Tests: happy path insert
// ---------------------------------------------------------------------------

func TestHandleWebhook_HappyPath(t *testing.T) {
	const secret = "mysecret"
	stub := &stubStore{}
	sched := &scheduler{store: stub, webhookSecret: secret}

	body := commitCreatedEvent("org/myrepo", "feat/new-feature", 42)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-DocStore-Signature", signBody(body, secret))
	w := httptest.NewRecorder()

	sched.handleWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(stub.insertedJobs) != 1 {
		t.Fatalf("expected 1 inserted job, got %d", len(stub.insertedJobs))
	}
	j := stub.insertedJobs[0]
	if j.Repo != "org/myrepo" {
		t.Errorf("repo mismatch: %q", j.Repo)
	}
	if j.Branch != "feat/new-feature" {
		t.Errorf("branch mismatch: %q", j.Branch)
	}
	if j.Sequence != 42 {
		t.Errorf("sequence mismatch: %d", j.Sequence)
	}
	if j.Status != "queued" {
		t.Errorf("status mismatch: %q", j.Status)
	}
}

func TestHandleWebhook_UnknownEventType(t *testing.T) {
	stub := &stubStore{}
	sched := &scheduler{store: stub, webhookSecret: ""}

	body, _ := json.Marshal(map[string]any{
		"type": "com.docstore.something.else",
		"data": map[string]any{},
	})
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()

	sched.handleWebhook(w, req)

	// Unknown events are silently acknowledged.
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if len(stub.insertedJobs) != 0 {
		t.Fatal("unexpected job insert for unknown event type")
	}
}

func TestHandleWebhook_MissingRepo(t *testing.T) {
	stub := &stubStore{}
	sched := &scheduler{store: stub, webhookSecret: ""}

	body, _ := json.Marshal(map[string]any{
		"type": "com.docstore.commit.created",
		"data": map[string]any{"branch": "main", "sequence": 1},
	})
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()

	sched.handleWebhook(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Tests: GET /run/{id}
// ---------------------------------------------------------------------------

func TestHandleGetRun_Found(t *testing.T) {
	errMsg := "something went wrong"
	stub := &stubStore{
		getJob: &model.CIJob{
			ID:           "abc-123",
			Status:       "failed",
			ErrorMessage: &errMsg,
		},
	}
	sched := &scheduler{store: stub}

	req := httptest.NewRequest(http.MethodGet, "/run/abc-123", nil)
	req.SetPathValue("id", "abc-123")
	w := httptest.NewRecorder()

	sched.handleGetRun(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp runStatusResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.RunID != "abc-123" {
		t.Errorf("run_id mismatch: %q", resp.RunID)
	}
	if resp.Status != "failed" {
		t.Errorf("status mismatch: %q", resp.Status)
	}
	if resp.Error != errMsg {
		t.Errorf("error mismatch: %q", resp.Error)
	}
}

// ---------------------------------------------------------------------------
// Tests: GET /run/{id}/logs/{check}
// ---------------------------------------------------------------------------

func TestHandleGetLogs_ClaimedWithWorkerIP_ProxiesToWorker(t *testing.T) {
	// The production code dials WorkerPodIP:8081 for live log proxying.
	// Bind the fake worker to port 8081 on loopback so the proxy connects correctly.
	ln, err := net.Listen("tcp", "127.0.0.1:8081")
	if err != nil {
		t.Skipf("port 8081 unavailable (likely already in use): %v", err)
	}

	fakeWorker := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("log output from worker")) //nolint:errcheck
	}))
	fakeWorker.Listener = ln
	fakeWorker.Start()
	defer fakeWorker.Close()

	stub := &stubStore{
		getJob: &model.CIJob{
			ID:          "job-1",
			Status:      "claimed",
			WorkerPodIP: new("127.0.0.1"),
		},
	}
	sched := &scheduler{store: stub}

	req := httptest.NewRequest(http.MethodGet, "/run/job-1/logs/build", nil)
	req.SetPathValue("id", "job-1")
	req.SetPathValue("check", "build")
	w := httptest.NewRecorder()

	sched.handleGetLogs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "log output from worker") {
		t.Errorf("expected proxied log body, got: %q", w.Body.String())
	}
}

func TestHandleGetLogs_CompletedWithLogURL_Redirects(t *testing.T) {
	logURL := "https://logs.example.com/job-2"
	stub := &stubStore{
		getJob: &model.CIJob{
			ID:     "job-2",
			Status: "passed",
			LogURL: &logURL,
		},
	}
	sched := &scheduler{store: stub}

	req := httptest.NewRequest(http.MethodGet, "/run/job-2/logs/build", nil)
	req.SetPathValue("id", "job-2")
	req.SetPathValue("check", "build")
	w := httptest.NewRecorder()

	sched.handleGetLogs(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", w.Code)
	}
	if got := w.Header().Get("Location"); got != logURL {
		t.Errorf("redirect location mismatch: %q", got)
	}
}

func TestHandleGetLogs_Queued_Returns404(t *testing.T) {
	stub := &stubStore{
		getJob: &model.CIJob{
			ID:     "job-3",
			Status: "queued",
		},
	}
	sched := &scheduler{store: stub}

	req := httptest.NewRequest(http.MethodGet, "/run/job-3/logs/build", nil)
	req.SetPathValue("id", "job-3")
	req.SetPathValue("check", "build")
	w := httptest.NewRecorder()

	sched.handleGetLogs(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Tests: POST /run (manual trigger)
// ---------------------------------------------------------------------------

func TestHandleRun_HappyPath(t *testing.T) {
	stub := &stubStore{}
	sched := &scheduler{store: stub}

	body, _ := json.Marshal(map[string]any{
		"repo":          "org/repo",
		"branch":        "main",
		"head_sequence": 10,
	})
	req := httptest.NewRequest(http.MethodPost, "/run", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	sched.handleRun(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["run_id"] == "" {
		t.Error("expected non-empty run_id")
	}
	if len(stub.insertedJobs) != 1 {
		t.Fatalf("expected 1 inserted job, got %d", len(stub.insertedJobs))
	}
}

func TestHandleRun_MissingRepo_Returns400(t *testing.T) {
	stub := &stubStore{}
	sched := &scheduler{store: stub}

	body, _ := json.Marshal(map[string]any{"branch": "main"})
	req := httptest.NewRequest(http.MethodPost, "/run", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	sched.handleRun(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Tests: push trigger type is recorded
// ---------------------------------------------------------------------------

func TestHandleWebhook_SetsTrigerTypePush(t *testing.T) {
	stub := &stubStore{}
	sched := &scheduler{store: stub, webhookSecret: ""}

	body := commitCreatedEvent("myrepo", "feat", 3)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()

	sched.handleWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if len(stub.insertedJobs) != 1 {
		t.Fatalf("expected 1 inserted job, got %d", len(stub.insertedJobs))
	}
	j := stub.insertedJobs[0]
	if j.TriggerType != "push" {
		t.Errorf("TriggerType = %q, want %q", j.TriggerType, "push")
	}
	if j.TriggerBranch != "feat" {
		t.Errorf("TriggerBranch = %q, want %q", j.TriggerBranch, "feat")
	}
}

func TestHandleRun_SetsTrigerTypeManual(t *testing.T) {
	stub := &stubStore{}
	sched := &scheduler{store: stub}

	body, _ := json.Marshal(map[string]any{
		"repo":          "org/repo",
		"branch":        "main",
		"head_sequence": 10,
	})
	req := httptest.NewRequest(http.MethodPost, "/run", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	sched.handleRun(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", w.Code)
	}
	if len(stub.insertedJobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(stub.insertedJobs))
	}
	j := stub.insertedJobs[0]
	if j.TriggerType != "manual" {
		t.Errorf("TriggerType = %q, want %q", j.TriggerType, "manual")
	}
}

// ---------------------------------------------------------------------------
// Tests: on: block branch filtering in webhook handler
// ---------------------------------------------------------------------------

// newCIConfigServer returns a test HTTP server that serves the given ci.yaml content.
// It validates that the request path ends with /-/file/.docstore/ci.yaml and that
// the branch and at query params are present and non-empty; returns 404 otherwise.
func newCIConfigServer(t *testing.T, ciYAML string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/-/file/.docstore/ci.yaml") ||
			r.URL.Query().Get("branch") == "" ||
			r.URL.Query().Get("at") == "" {
			http.NotFound(w, r)
			return
		}
		type fileResp struct {
			Path    string `json:"path"`
			Content []byte `json:"content"`
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(fileResp{Path: ".docstore/ci.yaml", Content: []byte(ciYAML)}) //nolint:errcheck
	}))
}

func TestHandleWebhook_OnPushBranchFilter_SkipsNonMatchingBranch(t *testing.T) {
	ciYAML := "on:\n  push:\n    branches:\n      - main\n"
	srv := newCIConfigServer(t, ciYAML)
	defer srv.Close()

	stub := &stubStore{}
	sched := &scheduler{
		store:       stub,
		docstoreURL: srv.URL,
		httpClient:  &http.Client{},
	}

	// Event for branch "feature/foo" — should be filtered out.
	body := commitCreatedEvent("myrepo", "feature/foo", 5)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()

	sched.handleWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if len(stub.insertedJobs) != 0 {
		t.Fatalf("expected 0 inserted jobs (filtered), got %d", len(stub.insertedJobs))
	}
}

func TestHandleWebhook_OnPushBranchFilter_AllowsMatchingBranch(t *testing.T) {
	ciYAML := "on:\n  push:\n    branches:\n      - main\n"
	srv := newCIConfigServer(t, ciYAML)
	defer srv.Close()

	stub := &stubStore{}
	sched := &scheduler{
		store:       stub,
		docstoreURL: srv.URL,
		httpClient:  &http.Client{},
	}

	// Event for branch "main" — should pass through.
	body := commitCreatedEvent("myrepo", "main", 6)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()

	sched.handleWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if len(stub.insertedJobs) != 1 {
		t.Fatalf("expected 1 inserted job, got %d", len(stub.insertedJobs))
	}
}

func TestHandleWebhook_NoCIYAML_AlwaysEnqueues(t *testing.T) {
	// Server returns 404 for all requests (no ci.yaml configured).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	stub := &stubStore{}
	sched := &scheduler{
		store:       stub,
		docstoreURL: srv.URL,
		httpClient:  &http.Client{},
	}

	body := commitCreatedEvent("myrepo", "feature/anything", 7)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()

	sched.handleWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if len(stub.insertedJobs) != 1 {
		t.Fatalf("expected 1 inserted job (no ci.yaml = always run), got %d", len(stub.insertedJobs))
	}
}

// newDocstoreServer builds a test server that handles both the ci.yaml file
// endpoint and the proposals endpoint. ciYAML may be empty for no config.
// proposals is a list of JSON-encodable proposal objects returned for ?state=open.
func newDocstoreServer(t *testing.T, ciYAML string, branches []map[string]any, proposals []map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/-/file/.docstore/ci.yaml"):
			if ciYAML == "" {
				http.NotFound(w, r)
				return
			}
			type fileResp struct {
				Path    string `json:"path"`
				Content []byte `json:"content"`
			}
			json.NewEncoder(w).Encode(fileResp{Path: ".docstore/ci.yaml", Content: []byte(ciYAML)}) //nolint:errcheck
		case strings.Contains(r.URL.Path, "/-/branches"):
			// Bare array — must match real server format. See TestBranchesEndpointContract.
			json.NewEncoder(w).Encode(branches) //nolint:errcheck
		case strings.Contains(r.URL.Path, "/-/proposals"):
			json.NewEncoder(w).Encode(proposals) //nolint:errcheck
		default:
			http.NotFound(w, r)
		}
	}))
}

// ---------------------------------------------------------------------------
// Tests: proposal.opened event handling
// ---------------------------------------------------------------------------

func TestHandleWebhook_ProposalOpened_EnqueuesJob(t *testing.T) {
	branches := []map[string]any{
		{"name": "feature/foo", "head_sequence": int64(10)},
	}
	srv := newDocstoreServer(t, "", branches, nil)
	defer srv.Close()

	stub := &stubStore{}
	sched := &scheduler{
		store:       stub,
		docstoreURL: srv.URL,
		httpClient:  &http.Client{},
	}

	body := proposalOpenedEvent("myrepo", "feature/foo", "main", "proposal-uuid-1")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()

	sched.handleWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(stub.insertedJobs) != 1 {
		t.Fatalf("expected 1 inserted job, got %d", len(stub.insertedJobs))
	}
	j := stub.insertedJobs[0]
	if j.TriggerType != "proposal" {
		t.Errorf("TriggerType = %q, want %q", j.TriggerType, "proposal")
	}
	if j.TriggerProposalID == nil || *j.TriggerProposalID != "proposal-uuid-1" {
		t.Errorf("TriggerProposalID = %v, want %q", j.TriggerProposalID, "proposal-uuid-1")
	}
}

func TestHandleWebhook_ProposalOpened_FilteredByOnBlock(t *testing.T) {
	ciYAML := "on:\n  proposal:\n    base_branches:\n      - release/*\n"
	branches := []map[string]any{
		{"name": "feature/foo", "head_sequence": int64(10)},
	}
	srv := newDocstoreServer(t, ciYAML, branches, nil)
	defer srv.Close()

	stub := &stubStore{}
	sched := &scheduler{
		store:       stub,
		docstoreURL: srv.URL,
		httpClient:  &http.Client{},
	}

	// base_branch is "main" — does not match "release/*", so should be filtered.
	body := proposalOpenedEvent("myrepo", "feature/foo", "main", "proposal-uuid-2")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()

	sched.handleWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if len(stub.insertedJobs) != 0 {
		t.Fatalf("expected 0 jobs (filtered), got %d", len(stub.insertedJobs))
	}
}

func TestHandleWebhook_ProposalOpened_MissingRepo_Returns400(t *testing.T) {
	stub := &stubStore{}
	sched := &scheduler{store: stub, webhookSecret: ""}

	body, _ := json.Marshal(map[string]any{
		"type": "com.docstore.proposal.opened",
		"data": map[string]any{"branch": "feature/foo", "base_branch": "main"},
	})
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()

	sched.handleWebhook(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Tests: commit.created with open proposal enqueues proposal_synchronized job
// ---------------------------------------------------------------------------

func TestHandleWebhook_CommitCreated_WithOpenProposal_EnqueuesBothJobs(t *testing.T) {
	proposals := []map[string]any{
		{"id": "prop-1", "repo": "myrepo", "branch": "feature/foo", "base_branch": "main", "state": "open"},
	}
	branches := []map[string]any{
		{"name": "feature/foo", "head_sequence": int64(5)},
	}
	srv := newDocstoreServer(t, "", branches, proposals)
	defer srv.Close()

	stub := &stubStore{}
	sched := &scheduler{
		store:       stub,
		docstoreURL: srv.URL,
		httpClient:  &http.Client{},
	}

	body := commitCreatedEvent("myrepo", "feature/foo", 5)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()

	sched.handleWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(stub.insertedJobs) != 2 {
		t.Fatalf("expected 2 inserted jobs (push + proposal_synchronized), got %d", len(stub.insertedJobs))
	}
	types := map[string]bool{}
	for _, j := range stub.insertedJobs {
		types[j.TriggerType] = true
	}
	if !types["push"] {
		t.Error("expected push job")
	}
	if !types["proposal_synchronized"] {
		t.Error("expected proposal_synchronized job")
	}
}

func TestHandleWebhook_CommitCreated_NoOpenProposal_EnqueuesOnlyPush(t *testing.T) {
	// Proposals endpoint returns empty array.
	srv := newDocstoreServer(t, "", nil, []map[string]any{})
	defer srv.Close()

	stub := &stubStore{}
	sched := &scheduler{
		store:       stub,
		docstoreURL: srv.URL,
		httpClient:  &http.Client{},
	}

	body := commitCreatedEvent("myrepo", "feature/foo", 5)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()

	sched.handleWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if len(stub.insertedJobs) != 1 {
		t.Fatalf("expected 1 inserted job (push only), got %d", len(stub.insertedJobs))
	}
	if stub.insertedJobs[0].TriggerType != "push" {
		t.Errorf("TriggerType = %q, want push", stub.insertedJobs[0].TriggerType)
	}
}

// ---------------------------------------------------------------------------
// countingStore tracks ReapStaleCIJobs call count for reaper tests.
// ---------------------------------------------------------------------------

type countingStore struct {
	mu        sync.Mutex
	reapCalls int
	reapJobs  []model.CIJob
	reapErr   error
}

func (s *countingStore) InsertCIJob(_ context.Context, repo, branch string, sequence int64, triggerType, triggerBranch, triggerBaseBranch, triggerProposalID string) (*model.CIJob, error) {
	return &model.CIJob{ID: "test-uuid", Repo: repo, Branch: branch, Sequence: sequence, Status: "queued", TriggerType: triggerType}, nil
}

func (s *countingStore) GetCIJob(_ context.Context, _ string) (*model.CIJob, error) {
	return nil, nil
}

func (s *countingStore) ReapStaleCIJobs(_ context.Context) ([]model.CIJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reapCalls++
	return s.reapJobs, s.reapErr
}

func (s *countingStore) ClaimCIJob(_ context.Context, podName, podIP string) (*model.CIJob, error) {
	return nil, nil
}

func (s *countingStore) StoreRequestToken(_ context.Context, jobID string, hashedToken string, exp time.Time) error {
	return nil
}

func (s *countingStore) LookupRequestToken(_ context.Context, hashedToken string) (*model.CIJob, error) {
	return nil, nil
}

func (s *countingStore) HeartbeatCIJob(_ context.Context, id string) error {
	return nil
}

func (s *countingStore) CompleteCIJob(_ context.Context, id, status string, logURL, errorMessage *string) error {
	return nil
}

func (s *countingStore) CountQueuedCIJobs(_ context.Context) (int64, error) {
	return 0, nil
}

func (s *countingStore) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reapCalls
}

// ---------------------------------------------------------------------------
// newScheduleDocstoreServer creates a test docstore server for schedule tests.
// It handles:
//   - GET /repos                                     → {"repos":[...]}
//   - GET /repos/{repo}/-/branches                   → {"branches":[{"name":"main",...}]}
//   - GET /repos/{repo}/-/file/.docstore/ci.yaml     → FileResponse (or 404 if ciYAML == "")
// ---------------------------------------------------------------------------

func newScheduleDocstoreServer(t *testing.T, repoNames []string, headSeq int64, ciYAML string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/repos":
			repos := make([]map[string]any, 0, len(repoNames))
			for _, name := range repoNames {
				repos = append(repos, map[string]any{"name": name})
			}
			json.NewEncoder(w).Encode(map[string]any{"repos": repos}) //nolint:errcheck
		case strings.Contains(r.URL.Path, "/-/file/.docstore/ci.yaml"):
			if ciYAML == "" {
				http.NotFound(w, r)
				return
			}
			type fileResp struct {
				Path    string `json:"path"`
				Content []byte `json:"content"`
			}
			json.NewEncoder(w).Encode(fileResp{Path: ".docstore/ci.yaml", Content: []byte(ciYAML)}) //nolint:errcheck
		case strings.Contains(r.URL.Path, "/-/branches"):
			// Bare array — must match real server format. See TestBranchesEndpointContract.
			json.NewEncoder(w).Encode([]map[string]any{ //nolint:errcheck
				{"name": "main", "head_sequence": headSeq},
			})
		default:
			http.NotFound(w, r)
		}
	}))
}

// ---------------------------------------------------------------------------
// Tests: fetchBranchHead
// ---------------------------------------------------------------------------

func TestFetchBranchHead_HappyPath(t *testing.T) {
	// NOTE: this mock returns a bare JSON array — NOT a wrapped struct like
	// {"branches":[...]}. The format must match the real server handler in
	// internal/server/handlers.go (handleBranches). TestBranchesEndpointContract
	// in contract_test.go enforces this cross-package contract automatically.
	// Regression: a previous commit decoded model.BranchesResponse here while
	// the server was returning a bare array; both sides were wrong in sync so
	// CI stayed green for months before the mismatch was discovered.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]map[string]any{ //nolint:errcheck
			{"name": "feat", "head_sequence": int64(5)},
			{"name": "main", "head_sequence": int64(99)},
		})
	}))
	defer srv.Close()

	sched := &scheduler{docstoreURL: srv.URL, httpClient: &http.Client{}}
	seq, err := sched.fetchBranchHead(context.Background(), "org/repo", "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if seq != 99 {
		t.Errorf("head sequence = %d, want 99", seq)
	}
}

func TestFetchBranchHead_BranchNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]map[string]any{ //nolint:errcheck
			{"name": "other", "head_sequence": int64(1)},
		})
	}))
	defer srv.Close()

	sched := &scheduler{docstoreURL: srv.URL, httpClient: &http.Client{}}
	_, err := sched.fetchBranchHead(context.Background(), "org/repo", "main")
	if err == nil {
		t.Fatal("expected error for missing branch, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want message containing 'not found'", err.Error())
	}
}

func TestFetchBranchHead_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	sched := &scheduler{docstoreURL: srv.URL, httpClient: &http.Client{}}
	_, err := sched.fetchBranchHead(context.Background(), "org/repo", "main")
	if err == nil {
		t.Fatal("expected error for non-200 status, got nil")
	}
}

func TestFetchBranchHead_NoDocstoreURL(t *testing.T) {
	sched := &scheduler{docstoreURL: "", httpClient: &http.Client{}}
	_, err := sched.fetchBranchHead(context.Background(), "org/repo", "main")
	if err == nil {
		t.Fatal("expected error when docstore URL is empty, got nil")
	}
}

// ---------------------------------------------------------------------------
// Tests: fetchAllRepos
// ---------------------------------------------------------------------------

func TestFetchAllRepos_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"repos": []map[string]any{
				{"name": "org/alpha"},
				{"name": "org/beta"},
			},
		})
	}))
	defer srv.Close()

	sched := &scheduler{docstoreURL: srv.URL, httpClient: &http.Client{}}
	repos, err := sched.fetchAllRepos(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("expected 2 repos, got %d", len(repos))
	}
	if repos[0] != "org/alpha" || repos[1] != "org/beta" {
		t.Errorf("repos = %v, want [org/alpha org/beta]", repos)
	}
}

func TestFetchAllRepos_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()

	sched := &scheduler{docstoreURL: srv.URL, httpClient: &http.Client{}}
	_, err := sched.fetchAllRepos(context.Background())
	if err == nil {
		t.Fatal("expected error for non-200 status, got nil")
	}
}

func TestFetchAllRepos_NoDocstoreURL(t *testing.T) {
	sched := &scheduler{docstoreURL: "", httpClient: &http.Client{}}
	repos, err := sched.fetchAllRepos(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if repos != nil {
		t.Errorf("expected nil repos for empty docstore URL, got %v", repos)
	}
}

// ---------------------------------------------------------------------------
// Tests: runScheduledJobs — cron expression parsing and job enqueuing
// ---------------------------------------------------------------------------

// TestRunScheduledJobs_CronMatchesCurrentMinute_EnqueuesJob verifies that a
// schedule entry whose cron fires at the given minute results in a queued job.
// Fixed time: 2024-01-15 14:30:00 UTC; cron "30 14 * * *" fires at 14:30 daily.
func TestRunScheduledJobs_CronMatchesCurrentMinute_EnqueuesJob(t *testing.T) {
	now := time.Date(2024, 1, 15, 14, 30, 0, 0, time.UTC)
	ciYAML := "on:\n  schedule:\n    - cron: \"30 14 * * *\"\n"

	srv := newScheduleDocstoreServer(t, []string{"org/repo"}, 10, ciYAML)
	defer srv.Close()

	stub := &stubStore{}
	sched := &scheduler{store: stub, docstoreURL: srv.URL, httpClient: &http.Client{}}

	sched.runScheduledJobs(context.Background(), now)

	if len(stub.insertedJobs) != 1 {
		t.Fatalf("expected 1 schedule job, got %d", len(stub.insertedJobs))
	}
	j := stub.insertedJobs[0]
	if j.TriggerType != "schedule" {
		t.Errorf("TriggerType = %q, want schedule", j.TriggerType)
	}
	if j.Repo != "org/repo" {
		t.Errorf("Repo = %q, want org/repo", j.Repo)
	}
	if j.Branch != "main" {
		t.Errorf("Branch = %q, want main", j.Branch)
	}
	if j.Sequence != 10 {
		t.Errorf("Sequence = %d, want 10", j.Sequence)
	}
}

// TestRunScheduledJobs_CronDoesNotMatchCurrentMinute_NoJob verifies that a
// schedule entry whose cron does NOT fire at the given minute produces no job.
// Fixed time: 2024-01-15 14:30:00 UTC; cron "0 0 * * *" fires at midnight.
func TestRunScheduledJobs_CronDoesNotMatchCurrentMinute_NoJob(t *testing.T) {
	now := time.Date(2024, 1, 15, 14, 30, 0, 0, time.UTC)
	ciYAML := "on:\n  schedule:\n    - cron: \"0 0 * * *\"\n"

	srv := newScheduleDocstoreServer(t, []string{"org/repo"}, 10, ciYAML)
	defer srv.Close()

	stub := &stubStore{}
	sched := &scheduler{store: stub, docstoreURL: srv.URL, httpClient: &http.Client{}}

	sched.runScheduledJobs(context.Background(), now)

	if len(stub.insertedJobs) != 0 {
		t.Fatalf("expected 0 jobs (cron not matching at 14:30), got %d", len(stub.insertedJobs))
	}
}

// TestRunScheduledJobs_InvalidCronExpression_NoJob verifies that an invalid
// cron expression is skipped (logged as a warning) without panicking or
// enqueuing a job.
func TestRunScheduledJobs_InvalidCronExpression_NoJob(t *testing.T) {
	now := time.Date(2024, 1, 15, 14, 30, 0, 0, time.UTC)
	ciYAML := "on:\n  schedule:\n    - cron: \"not-a-cron\"\n"

	srv := newScheduleDocstoreServer(t, []string{"org/repo"}, 10, ciYAML)
	defer srv.Close()

	stub := &stubStore{}
	sched := &scheduler{store: stub, docstoreURL: srv.URL, httpClient: &http.Client{}}

	sched.runScheduledJobs(context.Background(), now)

	if len(stub.insertedJobs) != 0 {
		t.Fatalf("expected 0 jobs (invalid cron skipped), got %d", len(stub.insertedJobs))
	}
}

// TestRunScheduledJobs_NoCIConfig_NoJob verifies that repos with no ci.yaml
// are skipped — no schedule job is enqueued.
func TestRunScheduledJobs_NoCIConfig_NoJob(t *testing.T) {
	now := time.Date(2024, 1, 15, 14, 30, 0, 0, time.UTC)
	// Pass empty ciYAML so the server returns 404 for ci.yaml requests.
	srv := newScheduleDocstoreServer(t, []string{"org/repo"}, 10, "")
	defer srv.Close()

	stub := &stubStore{}
	sched := &scheduler{store: stub, docstoreURL: srv.URL, httpClient: &http.Client{}}

	sched.runScheduledJobs(context.Background(), now)

	if len(stub.insertedJobs) != 0 {
		t.Fatalf("expected 0 jobs (no ci.yaml), got %d", len(stub.insertedJobs))
	}
}

// TestRunScheduledJobs_NoScheduleEntries_NoJob verifies that a ci.yaml with
// an on: block but no schedule entries does not enqueue any schedule jobs.
func TestRunScheduledJobs_NoScheduleEntries_NoJob(t *testing.T) {
	now := time.Date(2024, 1, 15, 14, 30, 0, 0, time.UTC)
	ciYAML := "on:\n  push:\n    branches:\n      - main\n"

	srv := newScheduleDocstoreServer(t, []string{"org/repo"}, 10, ciYAML)
	defer srv.Close()

	stub := &stubStore{}
	sched := &scheduler{store: stub, docstoreURL: srv.URL, httpClient: &http.Client{}}

	sched.runScheduledJobs(context.Background(), now)

	if len(stub.insertedJobs) != 0 {
		t.Fatalf("expected 0 jobs (no schedule entries), got %d", len(stub.insertedJobs))
	}
}

// TestRunScheduledJobs_MultipleRepos_OnlyMatchingEnqueues verifies that when
// multiple repos are returned, only the one whose cron fires at the given
// minute gets a job inserted.
func TestRunScheduledJobs_MultipleRepos_OnlyMatchingEnqueues(t *testing.T) {
	// 2024-01-15 14:30:00 UTC — "30 14 * * *" matches, "0 0 * * *" does not.
	now := time.Date(2024, 1, 15, 14, 30, 0, 0, time.UTC)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/repos":
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"repos": []map[string]any{
					{"name": "org/matching"},
					{"name": "org/nomatch"},
				},
			})
		case strings.Contains(r.URL.Path, "/-/file/.docstore/ci.yaml"):
			var ciYAML string
			if strings.Contains(r.URL.Path, "org/matching") {
				ciYAML = "on:\n  schedule:\n    - cron: \"30 14 * * *\"\n"
			} else {
				ciYAML = "on:\n  schedule:\n    - cron: \"0 0 * * *\"\n"
			}
			type fileResp struct {
				Path    string `json:"path"`
				Content []byte `json:"content"`
			}
			json.NewEncoder(w).Encode(fileResp{Path: ".docstore/ci.yaml", Content: []byte(ciYAML)}) //nolint:errcheck
		case strings.Contains(r.URL.Path, "/-/branches"):
			json.NewEncoder(w).Encode([]map[string]any{ //nolint:errcheck
				{"name": "main", "head_sequence": int64(1)},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	stub := &stubStore{}
	sched := &scheduler{store: stub, docstoreURL: srv.URL, httpClient: &http.Client{}}

	sched.runScheduledJobs(context.Background(), now)

	if len(stub.insertedJobs) != 1 {
		t.Fatalf("expected 1 job (only matching repo), got %d", len(stub.insertedJobs))
	}
	if stub.insertedJobs[0].Repo != "org/matching" {
		t.Errorf("Repo = %q, want org/matching", stub.insertedJobs[0].Repo)
	}
}

// ---------------------------------------------------------------------------
// Tests: startReaper — stale job reclamation
// ---------------------------------------------------------------------------

// TestStartReaper_CallsReapOnInterval verifies that startReaper invokes
// ReapStaleCIJobs at least once within a short window using a fast ticker.
func TestStartReaper_CallsReapOnInterval(t *testing.T) {
	store := &countingStore{
		reapJobs: []model.CIJob{
			{ID: "stale-1", Repo: "org/repo", Branch: "main", Status: "queued"},
		},
	}
	ctx := t.Context()

	startReaper(ctx, store, 10*time.Millisecond)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if store.calls() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if store.calls() == 0 {
		t.Fatal("ReapStaleCIJobs was never called within 1s")
	}
}

// TestStartReaper_ReapError_ContinuesRunning verifies that an error returned
// by ReapStaleCIJobs does not stop the reaper — it continues ticking.
func TestStartReaper_ReapError_ContinuesRunning(t *testing.T) {
	store := &countingStore{reapErr: context.DeadlineExceeded}
	ctx := t.Context()

	startReaper(ctx, store, 10*time.Millisecond)

	// Wait for multiple calls despite the persistent error.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if store.calls() >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if store.calls() < 3 {
		t.Fatalf("expected at least 3 reap calls (reaper continued despite error), got %d", store.calls())
	}
}

// TestStartReaper_StopsOnContextCancellation verifies that cancelling the
// context causes the reaper goroutine to stop accepting new ticks.
func TestStartReaper_StopsOnContextCancellation(t *testing.T) {
	store := &countingStore{}
	ctx, cancel := context.WithCancel(t.Context())

	startReaper(ctx, store, 10*time.Millisecond)

	// Let it run briefly, then cancel.
	time.Sleep(30 * time.Millisecond)
	cancel()

	// Record call count shortly after cancellation.
	time.Sleep(20 * time.Millisecond)
	countAfterCancel := store.calls()

	// Wait another window and verify count did not grow.
	time.Sleep(50 * time.Millisecond)
	if store.calls() > countAfterCancel {
		t.Errorf("reaper continued after context cancellation: calls before=%d, after=%d",
			countAfterCancel, store.calls())
	}
}

// ---------------------------------------------------------------------------
// Tests: startCronRunner — goroutine lifecycle
// ---------------------------------------------------------------------------

// TestStartCronRunner_StopsOnContextCancellation verifies that startCronRunner
// launches without panicking and its goroutine exits when the context is cancelled.
func TestStartCronRunner_StopsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	stub := &stubStore{}
	sched := &scheduler{store: stub, docstoreURL: "", httpClient: &http.Client{}}

	startCronRunner(ctx, sched)
	// Cancel before the 1-minute ticker ever fires — goroutine should drain ctx.Done().
	cancel()
	// Brief sleep to allow the goroutine to exit gracefully.
	time.Sleep(10 * time.Millisecond)
	// No assertion beyond "did not panic or deadlock".
}

// ---------------------------------------------------------------------------
// Tests: GET /queue-depth — KEDA metrics-api scaler endpoint
// ---------------------------------------------------------------------------

// TestHandleQueueDepth verifies that GET /queue-depth returns HTTP 200 and the
// exact JSON body {"queue_depth":N}. This locks the contract that KEDA depends
// on via valueLocation: "queue_depth" in the ScaledObject.
func TestHandleQueueDepth(t *testing.T) {
	stub := &stubStore{queueDepth: 7}
	sched := &scheduler{store: stub}
	mux := newMux(sched)

	req := httptest.NewRequest(http.MethodGet, "/queue-depth", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	want := `{"queue_depth":7}`
	if got := strings.TrimSpace(w.Body.String()); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// TestHandleQueueDepth_ZeroDepth verifies the response when no jobs are queued.
func TestHandleQueueDepth_ZeroDepth(t *testing.T) {
	stub := &stubStore{queueDepth: 0}
	sched := &scheduler{store: stub}
	mux := newMux(sched)

	req := httptest.NewRequest(http.MethodGet, "/queue-depth", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	want := `{"queue_depth":0}`
	if got := strings.TrimSpace(w.Body.String()); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}
