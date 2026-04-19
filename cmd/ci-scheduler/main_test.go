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
	"testing"

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
}

func (s *stubStore) InsertCIJob(_ context.Context, repo, branch string, sequence int64, triggerType, triggerBranch, triggerProposalID string) (*model.CIJob, error) {
	j := &model.CIJob{
		ID:            "test-uuid",
		Repo:          repo,
		Branch:        branch,
		Sequence:      sequence,
		Status:        "queued",
		TriggerType:   triggerType,
		TriggerBranch: triggerBranch,
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

func (s *stubStore) ReapStaleCIJobs(_ context.Context) ([]model.CIJob, error) {
	return s.reapJobs, s.reapErr
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

	workerIP := "127.0.0.1"
	stub := &stubStore{
		getJob: &model.CIJob{
			ID:          "job-1",
			Status:      "claimed",
			WorkerPodIP: &workerIP,
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
