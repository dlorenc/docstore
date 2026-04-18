package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func (s *stubStore) InsertCIJob(_ context.Context, repo, branch string, sequence int64) (*model.CIJob, error) {
	j := &model.CIJob{
		ID:       "test-uuid",
		Repo:     repo,
		Branch:   branch,
		Sequence: sequence,
		Status:   "queued",
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
