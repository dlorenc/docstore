package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// secretSetBody mirrors the JSON shape sent by SecretsSet. Defined in the test
// to keep the wire-shape assertion explicit and decoupled from the production
// anonymous struct.
type secretSetBody struct {
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
}

// initSecretsWorkspace prepares an App with a usable workspace pointing at the
// given remote.
func initSecretsWorkspace(t *testing.T, app *App, remote string) {
	t.Helper()
	initWorkspace(t, app, remote, "main", "alice")
}

func TestSecretsList(t *testing.T) {
	created := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	updated := time.Date(2026, 4, 15, 12, 30, 0, 0, time.UTC)
	lastUsed := time.Date(2026, 5, 1, 9, 15, 0, 0, time.UTC)
	updatedBy := "bob"

	body := []SecretMetadata{
		{
			ID:          "01H...A",
			Name:        "DOCKERHUB_TOKEN",
			Description: "Docker Hub publish token",
			SizeBytes:   42,
			CreatedBy:   "alice",
			CreatedAt:   created,
			UpdatedBy:   &updatedBy,
			UpdatedAt:   &updated,
			LastUsedAt:  &lastUsed,
		},
		{
			ID:          "01H...B",
			Name:        "SLACK_WEBHOOK_URL",
			Description: "",
			SizeBytes:   88,
			CreatedBy:   "alice",
			CreatedAt:   created,
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/repos/default/default/-/secrets" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(body)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	app, out := newTestApp(t, srv)
	initSecretsWorkspace(t, app, srv.URL)

	if err := app.SecretsList(); err != nil {
		t.Fatalf("SecretsList: %v", err)
	}
	got := out.String()

	wantSubstrings := []string{
		"NAME", "SIZE", "UPDATED", "LAST_USED", "DESCRIPTION",
		"DOCKERHUB_TOKEN", "42", updated.Format(time.RFC3339),
		lastUsed.Format(time.RFC3339), "Docker Hub publish token",
		"SLACK_WEBHOOK_URL", "88",
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(got, s) {
			t.Errorf("output missing %q\n%s", s, got)
		}
	}

	// SLACK_WEBHOOK_URL has no UpdatedAt: should fall back to CreatedAt.
	if !strings.Contains(got, created.Format(time.RFC3339)) {
		t.Errorf("expected created timestamp fallback in output:\n%s", got)
	}
	// SLACK_WEBHOOK_URL has no LastUsedAt: should print "-".
	if !strings.Contains(got, "-") {
		t.Errorf("expected '-' for missing LAST_USED:\n%s", got)
	}
}

func TestSecretsList_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"not authorized"}`))
	}))
	defer srv.Close()

	app, _ := newTestApp(t, srv)
	initSecretsWorkspace(t, app, srv.URL)

	err := app.SecretsList()
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not authorized") {
		t.Errorf("expected server error surfaced, got: %v", err)
	}
}

func TestSecretsSet_PutsCorrectBody(t *testing.T) {
	const secretValue = "super-secret-value"
	var (
		gotMethod string
		gotPath   string
		gotBody   secretSetBody
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	app, out := newTestApp(t, srv)
	initSecretsWorkspace(t, app, srv.URL)

	err := app.SecretsSet("DOCKERHUB_TOKEN", strings.NewReader(secretValue), "publish")
	if err != nil {
		t.Fatalf("SecretsSet: %v", err)
	}
	if gotMethod != "PUT" {
		t.Errorf("method = %q, want PUT", gotMethod)
	}
	if gotPath != "/repos/default/default/-/secrets/DOCKERHUB_TOKEN" {
		t.Errorf("path = %q", gotPath)
	}
	if gotBody.Value != secretValue {
		t.Errorf("value = %q, want %q", gotBody.Value, secretValue)
	}
	if gotBody.Description != "publish" {
		t.Errorf("description = %q, want %q", gotBody.Description, "publish")
	}
	if !strings.Contains(out.String(), "Set DOCKERHUB_TOKEN") {
		t.Errorf("expected success message, got: %s", out.String())
	}
	if !strings.Contains(out.String(), "18 bytes") {
		t.Errorf("expected size in output, got: %s", out.String())
	}
}

func TestSecretsSet_OmitsEmptyDescription(t *testing.T) {
	var rawBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	app, _ := newTestApp(t, srv)
	initSecretsWorkspace(t, app, srv.URL)

	if err := app.SecretsSet("FOO", strings.NewReader("bar"), ""); err != nil {
		t.Fatalf("SecretsSet: %v", err)
	}
	if strings.Contains(string(rawBody), "description") {
		t.Errorf("expected description omitted from JSON, got: %s", string(rawBody))
	}
}

func TestSecretsSet_RejectsBadNames(t *testing.T) {
	cases := []struct {
		name    string
		secret  string
		wantSub string
	}{
		{"lowercase", "my_token", "must match"},
		{"leading_digit", "1FOO", "must match"},
		{"with_hyphen", "FOO-BAR", "must match"},
		{"too_long", strings.Repeat("A", 65), "must match"},
		{"reserved_prefix", "DOCSTORE_TOKEN", "reserved"},
		{"empty", "", "must match"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var called bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.WriteHeader(http.StatusCreated)
			}))
			defer srv.Close()

			app, _ := newTestApp(t, srv)
			initSecretsWorkspace(t, app, srv.URL)

			err := app.SecretsSet(tc.secret, strings.NewReader("value"), "")
			if err == nil {
				t.Fatalf("expected error for name %q", tc.secret)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q missing %q", err.Error(), tc.wantSub)
			}
			if called {
				t.Errorf("server should not be called for invalid name %q", tc.secret)
			}
		})
	}
}

func TestSecretsSet_RejectsOversize(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	app, _ := newTestApp(t, srv)
	initSecretsWorkspace(t, app, srv.URL)

	big := bytes.Repeat([]byte("A"), maxSecretValueBytes+1)
	err := app.SecretsSet("FOO", bytes.NewReader(big), "")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Errorf("expected size error, got: %v", err)
	}
	if called {
		t.Errorf("server should not be called for oversized value")
	}
}

func TestSecretsSet_AcceptsExactMaxSize(t *testing.T) {
	var receivedLen int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b secretSetBody
		_ = json.NewDecoder(r.Body).Decode(&b)
		receivedLen = len(b.Value)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	app, _ := newTestApp(t, srv)
	initSecretsWorkspace(t, app, srv.URL)

	big := bytes.Repeat([]byte("A"), maxSecretValueBytes)
	if err := app.SecretsSet("FOO", bytes.NewReader(big), ""); err != nil {
		t.Fatalf("SecretsSet: %v", err)
	}
	if receivedLen != maxSecretValueBytes {
		t.Errorf("server got %d bytes, want %d", receivedLen, maxSecretValueBytes)
	}
}

func TestSecretsSet_RejectsEmpty(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	app, _ := newTestApp(t, srv)
	initSecretsWorkspace(t, app, srv.URL)

	if err := app.SecretsSet("FOO", strings.NewReader(""), ""); err == nil {
		t.Fatalf("expected empty-value error")
	}
	if called {
		t.Errorf("server should not be called for empty value")
	}
}

func TestSecretsSet_ServerErrorDoesNotLeakValue(t *testing.T) {
	const secretValue = "super-secret-value"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"kms unavailable"}`))
	}))
	defer srv.Close()

	app, _ := newTestApp(t, srv)
	initSecretsWorkspace(t, app, srv.URL)

	err := app.SecretsSet("FOO", strings.NewReader(secretValue), "desc")
	if err == nil {
		t.Fatalf("expected error")
	}
	if strings.Contains(err.Error(), secretValue) {
		t.Errorf("error must not include secret value: %v", err)
	}
	if !strings.Contains(err.Error(), "kms unavailable") {
		t.Errorf("expected server error surfaced, got: %v", err)
	}
}

func TestSecretsUnset(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	app, out := newTestApp(t, srv)
	initSecretsWorkspace(t, app, srv.URL)

	if err := app.SecretsUnset("DOCKERHUB_TOKEN"); err != nil {
		t.Fatalf("SecretsUnset: %v", err)
	}
	if gotMethod != "DELETE" {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
	if gotPath != "/repos/default/default/-/secrets/DOCKERHUB_TOKEN" {
		t.Errorf("path = %q", gotPath)
	}
	if !strings.Contains(out.String(), "Unset DOCKERHUB_TOKEN") {
		t.Errorf("unexpected output: %s", out.String())
	}
}

func TestSecretsUnset_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	app, _ := newTestApp(t, srv)
	initSecretsWorkspace(t, app, srv.URL)

	err := app.SecretsUnset("MISSING")
	if err == nil {
		t.Fatalf("expected error for 404")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected friendly not-found message, got: %v", err)
	}
}

func TestSecretsUnset_RejectsBadName(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	app, _ := newTestApp(t, srv)
	initSecretsWorkspace(t, app, srv.URL)

	if err := app.SecretsUnset("bad-name"); err == nil {
		t.Fatalf("expected validation error")
	}
	if called {
		t.Errorf("server should not be called for invalid name")
	}
}

func TestSecretsUnset_ServerErrorMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"forbidden: admin role required"}`))
	}))
	defer srv.Close()

	app, _ := newTestApp(t, srv)
	initSecretsWorkspace(t, app, srv.URL)

	err := app.SecretsUnset("FOO")
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "admin role required") {
		t.Errorf("expected server error surfaced, got: %v", err)
	}
}
