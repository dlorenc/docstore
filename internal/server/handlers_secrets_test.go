package server

import (
	"bytes"
	"context"
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
