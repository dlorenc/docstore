package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dlorenc/docstore/internal/citoken"
	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/secrets"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// fakeSecretsService implements secrets.Service for handler tests. Each
// method is overrideable per-test via the *Fn fields.
type fakeSecretsService struct {
	setFn    func(ctx context.Context, repo, name, description string, value []byte, actor string) (secrets.Metadata, error)
	listFn   func(ctx context.Context, repo string) ([]secrets.Metadata, error)
	deleteFn func(ctx context.Context, repo, name string) error
	revealFn func(ctx context.Context, repo string, names []string) (map[string][]byte, []string, error)
}

func (f *fakeSecretsService) Set(ctx context.Context, repo, name, description string, value []byte, actor string) (secrets.Metadata, error) {
	if f.setFn != nil {
		return f.setFn(ctx, repo, name, description, value, actor)
	}
	return secrets.Metadata{}, errors.New("setFn not set")
}

func (f *fakeSecretsService) List(ctx context.Context, repo string) ([]secrets.Metadata, error) {
	if f.listFn != nil {
		return f.listFn(ctx, repo)
	}
	return nil, nil
}

func (f *fakeSecretsService) Delete(ctx context.Context, repo, name string) error {
	if f.deleteFn != nil {
		return f.deleteFn(ctx, repo, name)
	}
	return errors.New("deleteFn not set")
}

func (f *fakeSecretsService) Reveal(ctx context.Context, repo string, names []string) (map[string][]byte, []string, error) {
	if f.revealFn != nil {
		return f.revealFn(ctx, repo, names)
	}
	return nil, nil, errors.New("revealFn not set")
}

// buildSecretsServer builds a *server wired with a mockStore + fake secrets
// service. Setting roleFn lets tests simulate an arbitrary RBAC role; if nil,
// the bootstrap admin (devID) gets admin via HasAdmin=false.
func buildSecretsServer(t *testing.T, fake *fakeSecretsService, roleFn func(ctx context.Context, repo, identity string) (*model.Role, error)) (http.Handler, *mockStore) {
	t.Helper()
	ms := &mockStore{getRoleFn: roleFn}
	s := &server{commitStore: ms, secrets: fake}
	return s.buildHandler(devID, devID, ms), ms
}

// doSecretsRequest issues a request against the secrets handler and returns
// the recorder.
func doSecretsRequest(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	} else {
		rdr = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// readerRole returns RoleReader for any identity. Used to test the non-admin
// GET path.
func readerRole(_ context.Context, _, identity string) (*model.Role, error) {
	return &model.Role{Identity: identity, Role: model.RoleReader}, nil
}

// maintainerRole returns RoleMaintainer — high enough to access the repo but
// below admin, which is what the secrets write/delete endpoints require.
func maintainerRole(_ context.Context, _, identity string) (*model.Role, error) {
	return &model.Role{Identity: identity, Role: model.RoleMaintainer}, nil
}

// ---------------------------------------------------------------------------
// List
// ---------------------------------------------------------------------------

func TestSecretsList_OK(t *testing.T) {
	now := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	updatedBy := "alice@example.com"
	updatedAt := now.Add(time.Hour)
	fake := &fakeSecretsService{
		listFn: func(_ context.Context, repo string) ([]secrets.Metadata, error) {
			if repo != "org/myrepo" {
				return nil, fmt.Errorf("unexpected repo: %q", repo)
			}
			return []secrets.Metadata{
				{ID: "id-1", Repo: repo, Name: "DOCKERHUB_TOKEN", Description: "docker", SizeBytes: 12, CreatedBy: "creator", CreatedAt: now, UpdatedBy: &updatedBy, UpdatedAt: &updatedAt},
				{ID: "id-2", Repo: repo, Name: "SLACK_WEBHOOK_URL", Description: "", SizeBytes: 50, CreatedBy: "creator", CreatedAt: now},
			}, nil
		},
	}
	h, _ := buildSecretsServer(t, fake, nil)
	_ = t.Context()

	rec := doSecretsRequest(t, h, http.MethodGet, "/repos/org/myrepo/-/secrets", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp listSecretsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Secrets) != 2 {
		t.Fatalf("expected 2 secrets, got %d", len(resp.Secrets))
	}
	if resp.Secrets[0].Name != "DOCKERHUB_TOKEN" || resp.Secrets[0].SizeBytes != 12 {
		t.Errorf("unexpected first secret: %+v", resp.Secrets[0])
	}
	// Defence-in-depth: ensure no sealed/encrypted fields leak in the JSON.
	body := rec.Body.String()
	for _, banned := range []string{"ciphertext", "nonce", "encrypted_dek", "Ciphertext", "Nonce", "EncryptedDEK"} {
		if strings.Contains(body, banned) {
			t.Errorf("response contains forbidden field %q: %s", banned, body)
		}
	}
}

func TestSecretsList_EmptyArrayNotNull(t *testing.T) {
	fake := &fakeSecretsService{
		listFn: func(_ context.Context, _ string) ([]secrets.Metadata, error) {
			return nil, nil
		},
	}
	h, _ := buildSecretsServer(t, fake, nil)
	_ = t.Context()

	rec := doSecretsRequest(t, h, http.MethodGet, "/repos/org/myrepo/-/secrets", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	// The body must contain "secrets":[] not "secrets":null.
	body := strings.TrimSpace(rec.Body.String())
	if !strings.Contains(body, `"secrets":[]`) {
		t.Errorf("expected empty array, got body: %s", body)
	}
}

func TestSecretsList_ReaderAllowed(t *testing.T) {
	fake := &fakeSecretsService{
		listFn: func(_ context.Context, _ string) ([]secrets.Metadata, error) {
			return []secrets.Metadata{}, nil
		},
	}
	// Use a non-bootstrap identity with reader role so RBAC actually evaluates
	// the GET branch.
	ms := &mockStore{getRoleFn: readerRole}
	s := &server{commitStore: ms, secrets: fake}
	h := s.buildHandler("alice@example.com", "", ms)
	_ = t.Context()

	rec := doSecretsRequest(t, h, http.MethodGet, "/repos/org/myrepo/-/secrets", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("reader should be allowed to list; got %d body: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Set
// ---------------------------------------------------------------------------

func TestSecretsSet_OK(t *testing.T) {
	now := time.Date(2025, 6, 7, 8, 9, 10, 0, time.UTC)
	const repo = "org/myrepo"
	const name = "DOCKERHUB_TOKEN"
	const description = "docker creds"
	const plaintext = "super-secret-value"

	var capturedValue []byte
	fake := &fakeSecretsService{
		setFn: func(_ context.Context, gotRepo, gotName, gotDesc string, value []byte, actor string) (secrets.Metadata, error) {
			if gotRepo != repo || gotName != name || gotDesc != description {
				t.Errorf("unexpected args: repo=%q name=%q desc=%q", gotRepo, gotName, gotDesc)
			}
			capturedValue = append(capturedValue, value...)
			if actor != devID {
				t.Errorf("expected actor %q, got %q", devID, actor)
			}
			return secrets.Metadata{
				ID:        "secret-1",
				Repo:      gotRepo,
				Name:      gotName,
				Description: gotDesc,
				SizeBytes: len(value),
				CreatedBy: actor,
				CreatedAt: now,
			}, nil
		},
	}
	h, _ := buildSecretsServer(t, fake, nil)
	_ = t.Context()

	body := fmt.Sprintf(`{"value":%q,"description":%q}`, plaintext, description)
	rec := doSecretsRequest(t, h, http.MethodPut, "/repos/"+repo+"/-/secrets/"+name, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if string(capturedValue) != plaintext {
		t.Errorf("service did not receive plaintext: got %q", capturedValue)
	}

	// The response is metadata only — never echo the value.
	respBody := rec.Body.String()
	if strings.Contains(respBody, plaintext) {
		t.Fatalf("response echoes plaintext value: %s", respBody)
	}

	var dto secretMetadataDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dto.Name != name || dto.SizeBytes != len(plaintext) || dto.ID != "secret-1" {
		t.Errorf("unexpected DTO: %+v", dto)
	}
}

func TestSecretsSet_ValidationErrors(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		wantMsg string
	}{
		{"invalid name", secrets.ErrInvalidName, "invalid secret name"},
		{"reserved", secrets.ErrReservedName, "reserved prefix"},
		{"too large", secrets.ErrValueTooLarge, "exceeds maximum size"},
		{"empty", secrets.ErrEmptyValue, "is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeSecretsService{
				setFn: func(_ context.Context, _, _, _ string, _ []byte, _ string) (secrets.Metadata, error) {
					return secrets.Metadata{}, tc.err
				},
			}
			h, _ := buildSecretsServer(t, fake, nil)
			_ = t.Context()

			rec := doSecretsRequest(t, h, http.MethodPut, "/repos/org/myrepo/-/secrets/MY_SECRET", `{"value":"x"}`)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for %v, got %d body: %s", tc.err, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantMsg) {
				t.Errorf("expected message containing %q, got %s", tc.wantMsg, rec.Body.String())
			}
			// Structured error must have a code.
			var apiErr APIError
			if err := json.Unmarshal(rec.Body.Bytes(), &apiErr); err != nil {
				t.Fatalf("decode error: %v", err)
			}
			if apiErr.Code != ErrCodeBadRequest {
				t.Errorf("expected BAD_REQUEST code, got %q", apiErr.Code)
			}
		})
	}
}

func TestSecretsSet_BadJSONBody(t *testing.T) {
	fake := &fakeSecretsService{
		setFn: func(_ context.Context, _, _, _ string, _ []byte, _ string) (secrets.Metadata, error) {
			t.Fatal("set must not be called for malformed body")
			return secrets.Metadata{}, nil
		},
	}
	h, _ := buildSecretsServer(t, fake, nil)
	_ = t.Context()

	rec := doSecretsRequest(t, h, http.MethodPut, "/repos/org/myrepo/-/secrets/MY", "not-json")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestSecretsSet_ServiceError_500_NoValueInLogs(t *testing.T) {
	const plaintext = "PLEASE_DO_NOT_LOG_ME"

	fake := &fakeSecretsService{
		setFn: func(_ context.Context, _, _, _ string, _ []byte, _ string) (secrets.Metadata, error) {
			return secrets.Metadata{}, errors.New("kms down")
		},
	}
	h, _ := buildSecretsServer(t, fake, nil)

	// Capture the default logger output.
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	var buf threadSafeBuffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	_ = t.Context()
	body := fmt.Sprintf(`{"value":%q}`, plaintext)
	rec := doSecretsRequest(t, h, http.MethodPut, "/repos/org/myrepo/-/secrets/MY_SECRET", body)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d; body: %s", rec.Code, rec.Body.String())
	}

	logs := buf.String()
	if strings.Contains(logs, plaintext) {
		t.Fatalf("logs leak the secret value:\n%s", logs)
	}
	if !strings.Contains(logs, "set_secret") {
		t.Errorf("expected op=set_secret in logs, got:\n%s", logs)
	}
	// And the response body must not echo the value either.
	if strings.Contains(rec.Body.String(), plaintext) {
		t.Errorf("response echoes plaintext: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Delete
// ---------------------------------------------------------------------------

func TestSecretsDelete_OK(t *testing.T) {
	called := false
	fake := &fakeSecretsService{
		deleteFn: func(_ context.Context, repo, name string) error {
			called = true
			if repo != "org/myrepo" || name != "MY_SECRET" {
				t.Errorf("unexpected args: repo=%q name=%q", repo, name)
			}
			return nil
		},
	}
	h, _ := buildSecretsServer(t, fake, nil)
	_ = t.Context()

	rec := doSecretsRequest(t, h, http.MethodDelete, "/repos/org/myrepo/-/secrets/MY_SECRET", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if !called {
		t.Error("service Delete was not called")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("expected empty body, got %q", rec.Body.String())
	}
}

func TestSecretsDelete_NotFound(t *testing.T) {
	fake := &fakeSecretsService{
		deleteFn: func(_ context.Context, _, _ string) error {
			return secrets.ErrNotFound
		},
	}
	h, _ := buildSecretsServer(t, fake, nil)
	_ = t.Context()

	rec := doSecretsRequest(t, h, http.MethodDelete, "/repos/org/myrepo/-/secrets/MISSING", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// RBAC
// ---------------------------------------------------------------------------

func TestSecretsSet_NonAdminForbidden(t *testing.T) {
	fake := &fakeSecretsService{
		setFn: func(_ context.Context, _, _, _ string, _ []byte, _ string) (secrets.Metadata, error) {
			t.Fatal("Set must not be reached when RBAC denies the request")
			return secrets.Metadata{}, nil
		},
	}
	// Use a non-bootstrap identity so the bootstrap-admin path doesn't grant
	// admin. RoleMaintainer is high but still below admin.
	ms := &mockStore{getRoleFn: maintainerRole}
	s := &server{commitStore: ms, secrets: fake}
	h := s.buildHandler("bob@example.com", "", ms)
	_ = t.Context()

	rec := doSecretsRequest(t, h, http.MethodPut, "/repos/org/myrepo/-/secrets/MY", `{"value":"x"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-admin Set, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestSecretsDelete_NonAdminForbidden(t *testing.T) {
	fake := &fakeSecretsService{
		deleteFn: func(_ context.Context, _, _ string) error {
			t.Fatal("Delete must not be reached when RBAC denies the request")
			return nil
		},
	}
	ms := &mockStore{getRoleFn: maintainerRole}
	s := &server{commitStore: ms, secrets: fake}
	h := s.buildHandler("bob@example.com", "", ms)
	_ = t.Context()

	rec := doSecretsRequest(t, h, http.MethodDelete, "/repos/org/myrepo/-/secrets/MY", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-admin Delete, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// threadSafeBuffer wraps bytes.Buffer with a mutex so it is safe to use as
// a slog Handler destination, which may be written to concurrently.
type threadSafeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *threadSafeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *threadSafeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ---------------------------------------------------------------------------
// Reveal — POST /repos/{owner}/{name}/-/secrets/reveal
// ---------------------------------------------------------------------------

// buildRevealServer wires a *server with a fake secrets.Service, a stub
// jobTokenStore, and a mockStore (for GetProposal/ListOrgMembers). The
// returned handler is the full http chain so the outer interception fires.
func buildRevealServer(t *testing.T, fake *fakeSecretsService, jobStore jobTokenStore, ms *mockStore) http.Handler {
	t.Helper()
	if ms == nil {
		ms = &mockStore{}
	}
	s := &server{
		commitStore:   ms,
		secrets:       fake,
		jobTokenStore: jobStore,
	}
	return s.buildHandler(devID, devID, ms)
}

// revealJobLookup returns a stubJobTokenStore that resolves the given
// plaintext token to job and rejects everything else as ErrTokenInvalid.
func revealJobLookup(t *testing.T, plaintext string, job *model.CIJob) *stubJobTokenStore {
	t.Helper()
	wantHashed := citoken.HashRequestToken(plaintext)
	return &stubJobTokenStore{
		lookupFn: func(_ context.Context, h string) (*model.CIJob, error) {
			if h != wantHashed {
				return nil, db.ErrTokenInvalid
			}
			return job, nil
		},
	}
}

// postReveal is a small helper for issuing reveal requests with a bearer token.
func postReveal(t *testing.T, h http.Handler, ctx context.Context, repo, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/repos/"+repo+"/-/secrets/reveal", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRevealSecrets_HappyPath_PushTrigger(t *testing.T) {
	const repo = "org/myrepo"
	plaintext, _, err := citoken.GenerateRequestToken()
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}

	var revealCalled, touchedRepo string
	var revealNames []string
	fake := &fakeSecretsService{
		revealFn: func(_ context.Context, gotRepo string, names []string) (map[string][]byte, []string, error) {
			revealCalled = "yes"
			touchedRepo = gotRepo
			revealNames = append(revealNames, names...)
			return map[string][]byte{
				"DOCKERHUB_TOKEN": []byte("dh-secret"),
				"SLACK_INCOMING":  []byte("sl-secret\x00bin"), // include a NUL to prove base64 round-trips
			}, nil, nil
		},
	}
	jobStore := revealJobLookup(t, plaintext, &model.CIJob{
		ID:          "job-1",
		Repo:        repo,
		Branch:      "main",
		TriggerType: "push",
	})

	h := buildRevealServer(t, fake, jobStore, nil)
	ctx := t.Context()
	rec := postReveal(t, h, ctx, repo, plaintext,
		`{"names":["DOCKERHUB_TOKEN","SLACK_INCOMING"]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if revealCalled != "yes" {
		t.Fatal("Reveal was not invoked on the service")
	}
	if touchedRepo != repo {
		t.Errorf("Reveal got repo %q, want %q", touchedRepo, repo)
	}
	if len(revealNames) != 2 || revealNames[0] != "DOCKERHUB_TOKEN" || revealNames[1] != "SLACK_INCOMING" {
		t.Errorf("Reveal got names %v", revealNames)
	}

	var resp revealResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Values) != 2 {
		t.Fatalf("expected 2 values, got %d", len(resp.Values))
	}
	got, err := base64.StdEncoding.DecodeString(resp.Values["DOCKERHUB_TOKEN"])
	if err != nil {
		t.Fatalf("decode DOCKERHUB_TOKEN: %v", err)
	}
	if string(got) != "dh-secret" {
		t.Errorf("decoded DOCKERHUB_TOKEN = %q, want %q", got, "dh-secret")
	}
	got, err = base64.StdEncoding.DecodeString(resp.Values["SLACK_INCOMING"])
	if err != nil {
		t.Fatalf("decode SLACK_INCOMING: %v", err)
	}
	if string(got) != "sl-secret\x00bin" {
		t.Errorf("decoded SLACK_INCOMING = %q", got)
	}
	if len(resp.Missing) != 0 {
		t.Errorf("expected empty missing, got %v", resp.Missing)
	}
}

func TestRevealSecrets_HappyPath_ProposalByMember(t *testing.T) {
	const repo = "org/myrepo"
	plaintext, _, _ := citoken.GenerateRequestToken()

	propID := "prop-42"
	ms := &mockStore{
		getProposalFn: func(_ context.Context, gotRepo, gotID string) (*model.Proposal, error) {
			if gotRepo != repo || gotID != propID {
				t.Errorf("GetProposal repo=%q id=%q", gotRepo, gotID)
			}
			return &model.Proposal{ID: propID, Author: "alice@example.com"}, nil
		},
		listOrgMembersFn: func(_ context.Context, org string) ([]model.OrgMember, error) {
			if org != "org" {
				t.Errorf("ListOrgMembers org=%q", org)
			}
			return []model.OrgMember{
				{Org: "org", Identity: "bob@example.com"},
				{Org: "org", Identity: "alice@example.com"},
			}, nil
		},
	}
	fake := &fakeSecretsService{
		revealFn: func(_ context.Context, _ string, _ []string) (map[string][]byte, []string, error) {
			return map[string][]byte{"X": []byte("v")}, nil, nil
		},
	}
	jobStore := revealJobLookup(t, plaintext, &model.CIJob{
		ID:                "job-2",
		Repo:              repo,
		TriggerType:       "proposal",
		TriggerProposalID: &propID,
	})

	h := buildRevealServer(t, fake, jobStore, ms)
	rec := postReveal(t, h, t.Context(), repo, plaintext, `{"names":["X"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestRevealSecrets_Denied_ProposalByNonMember(t *testing.T) {
	const repo = "org/myrepo"
	plaintext, _, _ := citoken.GenerateRequestToken()

	propID := "prop-9"
	ms := &mockStore{
		getProposalFn: func(_ context.Context, _, _ string) (*model.Proposal, error) {
			return &model.Proposal{ID: propID, Author: "outsider@example.com"}, nil
		},
		listOrgMembersFn: func(_ context.Context, _ string) ([]model.OrgMember, error) {
			return []model.OrgMember{{Org: "org", Identity: "alice@example.com"}}, nil
		},
	}
	fake := &fakeSecretsService{
		revealFn: func(_ context.Context, _ string, _ []string) (map[string][]byte, []string, error) {
			t.Fatal("Reveal must not be called when policy denies the request")
			return nil, nil, nil
		},
	}
	jobStore := revealJobLookup(t, plaintext, &model.CIJob{
		ID:                "job-3",
		Repo:              repo,
		TriggerType:       "proposal_synchronized",
		TriggerProposalID: &propID,
	})

	// Capture logs to verify we don't leak secret names.
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	var buf threadSafeBuffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	h := buildRevealServer(t, fake, jobStore, ms)
	rec := postReveal(t, h, t.Context(), repo, plaintext, `{"names":["DOCKERHUB_TOKEN"]}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "secrets_blocked") {
		t.Errorf("expected secrets_blocked in body, got %s", body)
	}
	if !strings.Contains(body, "non_member_proposal") {
		t.Errorf("expected non_member_proposal reason in body, got %s", body)
	}

	logs := buf.String()
	if strings.Contains(logs, "DOCKERHUB_TOKEN") {
		t.Errorf("logs leak requested secret name:\n%s", logs)
	}
}

func TestRevealSecrets_Denied_UnknownTrigger(t *testing.T) {
	const repo = "org/myrepo"
	plaintext, _, _ := citoken.GenerateRequestToken()

	for _, trigger := range []string{"", "unknown_trigger"} {
		t.Run(trigger, func(t *testing.T) {
			fake := &fakeSecretsService{
				revealFn: func(_ context.Context, _ string, _ []string) (map[string][]byte, []string, error) {
					t.Fatal("Reveal must not be called for denied trigger")
					return nil, nil, nil
				},
			}
			jobStore := revealJobLookup(t, plaintext, &model.CIJob{
				ID: "j", Repo: repo, TriggerType: trigger,
			})
			h := buildRevealServer(t, fake, jobStore, nil)
			rec := postReveal(t, h, t.Context(), repo, plaintext, `{"names":["X"]}`)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected 403 for trigger %q, got %d; body: %s", trigger, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "secrets_blocked") {
				t.Errorf("expected secrets_blocked reason, got %s", rec.Body.String())
			}
		})
	}
}

func TestRevealSecrets_SomeMissing(t *testing.T) {
	const repo = "org/myrepo"
	plaintext, _, _ := citoken.GenerateRequestToken()

	fake := &fakeSecretsService{
		revealFn: func(_ context.Context, _ string, names []string) (map[string][]byte, []string, error) {
			if len(names) != 3 {
				t.Errorf("expected 3 names, got %d", len(names))
			}
			return map[string][]byte{
				"A": []byte("a-val"),
				"B": []byte("b-val"),
			}, []string{"C"}, nil
		},
	}
	jobStore := revealJobLookup(t, plaintext, &model.CIJob{
		ID: "j", Repo: repo, TriggerType: "manual",
	})

	h := buildRevealServer(t, fake, jobStore, nil)
	rec := postReveal(t, h, t.Context(), repo, plaintext, `{"names":["A","B","C"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp revealResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Values) != 2 {
		t.Errorf("expected 2 values, got %d", len(resp.Values))
	}
	if len(resp.Missing) != 1 || resp.Missing[0] != "C" {
		t.Errorf("expected Missing=[C], got %v", resp.Missing)
	}
}

func TestRevealSecrets_RepoMismatch_404(t *testing.T) {
	plaintext, _, _ := citoken.GenerateRequestToken()
	jobStore := revealJobLookup(t, plaintext, &model.CIJob{
		ID: "j", Repo: "acme/api", TriggerType: "push",
	})
	fake := &fakeSecretsService{
		revealFn: func(_ context.Context, _ string, _ []string) (map[string][]byte, []string, error) {
			t.Fatal("Reveal must not be called on repo mismatch")
			return nil, nil, nil
		},
	}
	h := buildRevealServer(t, fake, jobStore, nil)
	rec := postReveal(t, h, t.Context(), "acme/other", plaintext, `{"names":["X"]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestRevealSecrets_BadToken_401(t *testing.T) {
	jobStore := &stubJobTokenStore{
		lookupFn: func(_ context.Context, _ string) (*model.CIJob, error) {
			return nil, db.ErrTokenInvalid
		},
	}
	fake := &fakeSecretsService{}
	h := buildRevealServer(t, fake, jobStore, nil)

	// No Authorization header at all.
	rec := postReveal(t, h, t.Context(), "org/myrepo", "", `{"names":["X"]}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing token, got %d; body: %s", rec.Code, rec.Body.String())
	}

	// Bearer token present but invalid.
	rec = postReveal(t, h, t.Context(), "org/myrepo", "garbage", `{"names":["X"]}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for invalid token, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestRevealSecrets_EmptyNames_400(t *testing.T) {
	plaintext, _, _ := citoken.GenerateRequestToken()
	jobStore := revealJobLookup(t, plaintext, &model.CIJob{
		ID: "j", Repo: "org/myrepo", TriggerType: "push",
	})
	fake := &fakeSecretsService{
		revealFn: func(_ context.Context, _ string, _ []string) (map[string][]byte, []string, error) {
			t.Fatal("Reveal must not be called with empty names")
			return nil, nil, nil
		},
	}
	h := buildRevealServer(t, fake, jobStore, nil)
	rec := postReveal(t, h, t.Context(), "org/myrepo", plaintext, `{"names":[]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty names, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestRevealSecrets_InvalidName_400(t *testing.T) {
	plaintext, _, _ := citoken.GenerateRequestToken()
	jobStore := revealJobLookup(t, plaintext, &model.CIJob{
		ID: "j", Repo: "org/myrepo", TriggerType: "push",
	})
	fake := &fakeSecretsService{
		revealFn: func(_ context.Context, _ string, _ []string) (map[string][]byte, []string, error) {
			t.Fatal("Reveal must not be called with invalid name")
			return nil, nil, nil
		},
	}
	h := buildRevealServer(t, fake, jobStore, nil)
	for _, bad := range []string{"lowercase", "WITH-DASH", "1LEADING_DIGIT", "WITH SPACE", strings.Repeat("A", 65)} {
		t.Run(bad, func(t *testing.T) {
			body := fmt.Sprintf(`{"names":[%q]}`, bad)
			rec := postReveal(t, h, t.Context(), "org/myrepo", plaintext, body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for %q, got %d; body: %s", bad, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestRevealSecrets_ServiceError_500_NoLeak(t *testing.T) {
	plaintext, _, _ := citoken.GenerateRequestToken()

	// The realistic case: decrypt fails with a generic KMS error. We assert
	// (a) 500, (b) the requested secret NAME does not leak into logs (the
	// design forbids logging names, only the op + repo + job_id + trigger),
	// and (c) the response body is the generic 500 message — no name, no
	// value, no internal error string.
	const requestedName = "DOCKERHUB_TOKEN"
	fake := &fakeSecretsService{
		revealFn: func(_ context.Context, _ string, _ []string) (map[string][]byte, []string, error) {
			return nil, nil, errors.New("kms unavailable")
		},
	}
	jobStore := revealJobLookup(t, plaintext, &model.CIJob{
		ID: "j", Repo: "org/myrepo", TriggerType: "push",
	})
	h := buildRevealServer(t, fake, jobStore, nil)

	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	var buf threadSafeBuffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	rec := postReveal(t, h, t.Context(), "org/myrepo", plaintext,
		fmt.Sprintf(`{"names":[%q]}`, requestedName))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), requestedName) {
		t.Errorf("response leaks requested secret name: %s", rec.Body.String())
	}
	logs := buf.String()
	if strings.Contains(logs, requestedName) {
		t.Fatalf("logs leak requested secret name:\n%s", logs)
	}
	if !strings.Contains(logs, "reveal_secrets") {
		t.Errorf("expected op=reveal_secrets in logs, got:\n%s", logs)
	}
}
