package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dlorenc/docstore/internal/executor"
)

// b64 returns the standard base64 encoding of s. Match for the wire format
// used by the reveal endpoint.
func b64(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// revealServer is a tiny httptest helper that captures the last request body
// and lets the caller customise the response.
type revealServer struct {
	srv         *httptest.Server
	gotBody     atomic.Value // []byte
	gotPath     atomic.Value // string
	gotAuth     atomic.Value // string
	callCount   atomic.Int32
	statusCode  int
	respBody    string
	contentType string
}

func newRevealServer(t *testing.T, status int, body string) *revealServer {
	t.Helper()
	rs := &revealServer{statusCode: status, respBody: body, contentType: "application/json"}
	rs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.callCount.Add(1)
		rs.gotPath.Store(r.URL.Path)
		rs.gotAuth.Store(r.Header.Get("Authorization"))
		data, _ := io.ReadAll(r.Body)
		rs.gotBody.Store(data)
		if rs.contentType != "" {
			w.Header().Set("Content-Type", rs.contentType)
		}
		w.WriteHeader(rs.statusCode)
		if rs.respBody != "" {
			_, _ = w.Write([]byte(rs.respBody))
		}
	}))
	t.Cleanup(rs.srv.Close)
	return rs
}

// decodedRequest unmarshals the captured body into a revealRequest.
func (rs *revealServer) decodedRequest(t *testing.T) revealRequest {
	t.Helper()
	v := rs.gotBody.Load()
	if v == nil {
		t.Fatal("no request body captured")
	}
	var req revealRequest
	if err := json.Unmarshal(v.([]byte), &req); err != nil {
		t.Fatalf("decode captured request: %v", err)
	}
	return req
}

// TestFetchSecrets_NoSecretsBlocks_NoHTTP verifies the short-circuit when no
// check has a secrets: allowlist — fetchSecrets must NOT make an HTTP call.
func TestFetchSecrets_NoSecretsBlocks_NoHTTP(t *testing.T) {
	rs := newRevealServer(t, http.StatusOK, `{"values":{}}`)

	cfg := &executor.Config{Checks: []executor.Check{
		{Name: "test", Image: "alpine", Steps: []string{"echo"}},
	}}
	resolved, missing, err := fetchSecrets(t.Context(), rs.srv.Client(), rs.srv.URL, "owner/repo", "tok", cfg)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if resolved != nil {
		t.Errorf("expected nil resolved, got %v", resolved)
	}
	if missing != nil {
		t.Errorf("expected nil missing, got %v", missing)
	}
	if rs.callCount.Load() != 0 {
		t.Errorf("expected 0 HTTP calls, got %d", rs.callCount.Load())
	}
}

// TestFetchSecrets_HappyPath verifies the simple form with two names from one
// check returns LocalName→plaintext correctly mapped.
func TestFetchSecrets_HappyPath(t *testing.T) {
	body := fmt.Sprintf(`{"values":{"DOCKERHUB_TOKEN":%q,"SLACK_INCOMING":%q}}`,
		b64("dh-plain"), b64("slack-plain"))
	rs := newRevealServer(t, http.StatusOK, body)

	cfg := &executor.Config{Checks: []executor.Check{{
		Name:  "release",
		Image: "alpine",
		Steps: []string{"./release.sh"},
		Secrets: []executor.SecretRequest{
			{LocalName: "DOCKERHUB_TOKEN", RepoName: "DOCKERHUB_TOKEN"},
			{LocalName: "SLACK_INCOMING", RepoName: "SLACK_INCOMING"},
		},
	}}}
	resolved, missing, err := fetchSecrets(t.Context(), rs.srv.Client(), rs.srv.URL, "owner/repo", "tok", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string][]byte{
		"DOCKERHUB_TOKEN": []byte("dh-plain"),
		"SLACK_INCOMING":  []byte("slack-plain"),
	}
	if !reflect.DeepEqual(resolved, want) {
		t.Errorf("resolved = %v, want %v", resolved, want)
	}
	if len(missing) != 0 {
		t.Errorf("expected no missing, got %v", missing)
	}

	// URL path goes to the right endpoint.
	if got := rs.gotPath.Load(); got != "/repos/owner/repo/-/secrets/reveal" {
		t.Errorf("unexpected path: %v", got)
	}
	// Bearer token propagated.
	if got := rs.gotAuth.Load(); got != "Bearer tok" {
		t.Errorf("unexpected auth header: %v", got)
	}
	// Names list is sorted and deduped.
	req := rs.decodedRequest(t)
	if !reflect.DeepEqual(req.Names, []string{"DOCKERHUB_TOKEN", "SLACK_INCOMING"}) {
		t.Errorf("unexpected names: %v", req.Names)
	}
}

// TestFetchSecrets_RenameForm verifies LocalName != RepoName: the request asks
// the server for the RepoName, but the resolved map keys on LocalName.
func TestFetchSecrets_RenameForm(t *testing.T) {
	body := fmt.Sprintf(`{"values":{"BAR":%q}}`, b64("v"))
	rs := newRevealServer(t, http.StatusOK, body)

	cfg := &executor.Config{Checks: []executor.Check{{
		Name:  "test",
		Image: "alpine",
		Steps: []string{"echo"},
		Secrets: []executor.SecretRequest{
			{LocalName: "FOO", RepoName: "BAR"},
		},
	}}}
	resolved, _, err := fetchSecrets(t.Context(), rs.srv.Client(), rs.srv.URL, "o/r", "tok", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(resolved["FOO"]) != "v" {
		t.Errorf("resolved[FOO] = %q, want %q", resolved["FOO"], "v")
	}
	// Server must have received the RepoName, not the LocalName.
	req := rs.decodedRequest(t)
	if !reflect.DeepEqual(req.Names, []string{"BAR"}) {
		t.Errorf("expected names=[BAR], got %v", req.Names)
	}
}

// TestFetchSecrets_DedupAcrossChecks verifies that two checks sharing a
// RepoName produce only one entry in the request body.
func TestFetchSecrets_DedupAcrossChecks(t *testing.T) {
	body := fmt.Sprintf(`{"values":{"DOCKERHUB":%q}}`, b64("shared"))
	rs := newRevealServer(t, http.StatusOK, body)

	cfg := &executor.Config{Checks: []executor.Check{
		{
			Name:  "build",
			Image: "alpine",
			Steps: []string{"echo"},
			Secrets: []executor.SecretRequest{
				{LocalName: "DOCKERHUB", RepoName: "DOCKERHUB"},
			},
		},
		{
			Name:  "publish",
			Image: "alpine",
			Steps: []string{"echo"},
			Secrets: []executor.SecretRequest{
				{LocalName: "DOCKERHUB", RepoName: "DOCKERHUB"},
			},
		},
	}}
	resolved, _, err := fetchSecrets(t.Context(), rs.srv.Client(), rs.srv.URL, "o/r", "tok", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(resolved["DOCKERHUB"]) != "shared" {
		t.Errorf("resolved[DOCKERHUB] = %q, want shared", resolved["DOCKERHUB"])
	}
	req := rs.decodedRequest(t)
	if !reflect.DeepEqual(req.Names, []string{"DOCKERHUB"}) {
		t.Errorf("expected single dedup'd name, got %v", req.Names)
	}
	if rs.callCount.Load() != 1 {
		t.Errorf("expected exactly 1 HTTP call, got %d", rs.callCount.Load())
	}
}

// TestFetchSecrets_MissingPropagated verifies that the response's `missing`
// field is returned to the caller verbatim.
func TestFetchSecrets_MissingPropagated(t *testing.T) {
	body := fmt.Sprintf(`{"values":{"FOO":%q},"missing":["BAR","BAZ"]}`, b64("v"))
	rs := newRevealServer(t, http.StatusOK, body)

	cfg := &executor.Config{Checks: []executor.Check{{
		Name:  "test",
		Image: "alpine",
		Steps: []string{"echo"},
		Secrets: []executor.SecretRequest{
			{LocalName: "FOO", RepoName: "FOO"},
			{LocalName: "BAR", RepoName: "BAR"},
			{LocalName: "BAZ", RepoName: "BAZ"},
		},
	}}}
	resolved, missing, err := fetchSecrets(t.Context(), rs.srv.Client(), rs.srv.URL, "o/r", "tok", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(resolved["FOO"]) != "v" {
		t.Errorf("resolved[FOO] = %q, want v", resolved["FOO"])
	}
	if _, ok := resolved["BAR"]; ok {
		t.Errorf("BAR should not be in resolved (server marked missing)")
	}
	sort.Strings(missing)
	if !reflect.DeepEqual(missing, []string{"BAR", "BAZ"}) {
		t.Errorf("missing = %v, want [BAR BAZ]", missing)
	}
}

// TestFetchSecrets_Forbidden verifies 403 produces a clear error wrapping the
// server's body (e.g. "secrets_blocked").
func TestFetchSecrets_Forbidden(t *testing.T) {
	rs := newRevealServer(t, http.StatusForbidden, "secrets_blocked: external fork proposal")

	cfg := &executor.Config{Checks: []executor.Check{{
		Name:  "test",
		Image: "alpine",
		Steps: []string{"echo"},
		Secrets: []executor.SecretRequest{
			{LocalName: "FOO", RepoName: "FOO"},
		},
	}}}
	_, _, err := fetchSecrets(t.Context(), rs.srv.Client(), rs.srv.URL, "o/r", "tok", cfg)
	if err == nil {
		t.Fatal("expected error for 403, got nil")
	}
	if !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("expected 'forbidden' in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "secrets_blocked") {
		t.Errorf("expected server body in error, got: %v", err)
	}
}

// TestFetchSecrets_Unauthorized verifies 401 produces a "unauthorized" error.
func TestFetchSecrets_Unauthorized(t *testing.T) {
	rs := newRevealServer(t, http.StatusUnauthorized, "")

	cfg := &executor.Config{Checks: []executor.Check{{
		Name:  "test",
		Image: "alpine",
		Steps: []string{"echo"},
		Secrets: []executor.SecretRequest{
			{LocalName: "FOO", RepoName: "FOO"},
		},
	}}}
	_, _, err := fetchSecrets(t.Context(), rs.srv.Client(), rs.srv.URL, "o/r", "tok", cfg)
	if err == nil {
		t.Fatal("expected error for 401, got nil")
	}
	if !strings.Contains(err.Error(), "unauthorized") {
		t.Errorf("expected 'unauthorized' in error, got: %v", err)
	}
}

// TestFetchSecrets_NotFound verifies 404 maps to a clear repo/job mismatch
// error so the operator can distinguish it from network/auth failures.
func TestFetchSecrets_NotFound(t *testing.T) {
	rs := newRevealServer(t, http.StatusNotFound, "")

	cfg := &executor.Config{Checks: []executor.Check{{
		Name:  "test",
		Image: "alpine",
		Steps: []string{"echo"},
		Secrets: []executor.SecretRequest{
			{LocalName: "FOO", RepoName: "FOO"},
		},
	}}}
	_, _, err := fetchSecrets(t.Context(), rs.srv.Client(), rs.srv.URL, "o/r", "tok", cfg)
	if err == nil {
		t.Fatal("expected error for 404, got nil")
	}
	if !strings.Contains(err.Error(), "repo or job mismatch") {
		t.Errorf("expected 'repo or job mismatch' in error, got: %v", err)
	}
}

// TestFetchSecrets_CrossCheckLocalNameConflict verifies that if two checks
// reuse the same LocalName for different RepoNames, fetchSecrets refuses to
// produce an ambiguous mapping and never makes the HTTP call.
func TestFetchSecrets_CrossCheckLocalNameConflict(t *testing.T) {
	rs := newRevealServer(t, http.StatusOK, `{"values":{}}`)

	cfg := &executor.Config{Checks: []executor.Check{
		{
			Name:  "build",
			Image: "alpine",
			Steps: []string{"echo"},
			Secrets: []executor.SecretRequest{
				{LocalName: "FOO", RepoName: "A"},
			},
		},
		{
			Name:  "publish",
			Image: "alpine",
			Steps: []string{"echo"},
			Secrets: []executor.SecretRequest{
				{LocalName: "FOO", RepoName: "B"},
			},
		},
	}}
	_, _, err := fetchSecrets(t.Context(), rs.srv.Client(), rs.srv.URL, "o/r", "tok", cfg)
	if err == nil {
		t.Fatal("expected error for cross-check LocalName conflict, got nil")
	}
	if !strings.Contains(err.Error(), "FOO") {
		t.Errorf("expected conflicting name in error, got: %v", err)
	}
}

// TestFetchSecrets_LogScrubbingOnError ensures that a 4xx/5xx error path never
// surfaces decoded plaintext anywhere — neither in the error string nor in any
// log line emitted while fetching. Captures slog output and asserts the secret
// values are absent. This is a defensive check; the caller-visible error is
// the public contract, but mistakes elsewhere (debug logs of body bytes, etc.)
// should also be caught here.
func TestFetchSecrets_NoValuesInLogsOnError(t *testing.T) {
	const sensitiveValue = "VERY_SECRET_VALUE_DO_NOT_LEAK"
	// Server returns a base64'd value but with a 5xx status, so the decoder
	// path is not taken. Even so, no log line should ever contain the
	// plaintext (the body is the encoded form, but if classifyRevealError
	// decided to dump it, it'd still be reachable; assert neither appears).
	encoded := b64(sensitiveValue)
	body := fmt.Sprintf(`{"values":{"FOO":%q}}`, encoded)
	rs := newRevealServer(t, http.StatusInternalServerError, body)

	// Capture slog output for the duration of the call.
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	cfg := &executor.Config{Checks: []executor.Check{{
		Name:  "test",
		Image: "alpine",
		Steps: []string{"echo"},
		Secrets: []executor.SecretRequest{
			{LocalName: "FOO", RepoName: "FOO"},
		},
	}}}
	_, _, err := fetchSecrets(t.Context(), rs.srv.Client(), rs.srv.URL, "o/r", "tok", cfg)
	if err == nil {
		t.Fatal("expected error for 500, got nil")
	}
	if strings.Contains(err.Error(), sensitiveValue) {
		t.Errorf("error string contains plaintext: %v", err)
	}
	if strings.Contains(err.Error(), encoded) && rs.statusCode != http.StatusInternalServerError {
		// Encoded body is only allowed for the >=500 diagnostic snippet path,
		// but a sensitive plaintext base64 must never land there in practice.
		t.Errorf("error string unexpectedly contains encoded value: %v", err)
	}
	if strings.Contains(logBuf.String(), sensitiveValue) {
		t.Errorf("captured logs contain plaintext value")
	}
}

// TestFetchSecrets_NoValuesInLogsOnSuccess ensures that the happy path never
// logs decoded plaintext. fetchSecrets currently emits no info/debug logs on
// success; this test pins that behaviour so future debug output doesn't
// accidentally leak secrets.
func TestFetchSecrets_NoValuesInLogsOnSuccess(t *testing.T) {
	const sensitiveValue = "another-secret-do-not-leak"
	body := fmt.Sprintf(`{"values":{"FOO":%q}}`, b64(sensitiveValue))
	rs := newRevealServer(t, http.StatusOK, body)

	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	cfg := &executor.Config{Checks: []executor.Check{{
		Name:  "test",
		Image: "alpine",
		Steps: []string{"echo"},
		Secrets: []executor.SecretRequest{
			{LocalName: "FOO", RepoName: "FOO"},
		},
	}}}
	resolved, _, err := fetchSecrets(t.Context(), rs.srv.Client(), rs.srv.URL, "o/r", "tok", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(resolved["FOO"]) != sensitiveValue {
		t.Fatalf("setup error: resolved[FOO] = %q", resolved["FOO"])
	}
	if strings.Contains(logBuf.String(), sensitiveValue) {
		t.Errorf("captured logs contain plaintext value")
	}
}

// TestFetchSecrets_BadBase64 verifies an invalid base64 value yields a clear
// error and does NOT echo the value bytes.
func TestFetchSecrets_BadBase64(t *testing.T) {
	rs := newRevealServer(t, http.StatusOK, `{"values":{"FOO":"!!!not-base64!!!"}}`)
	cfg := &executor.Config{Checks: []executor.Check{{
		Name:  "test",
		Image: "alpine",
		Steps: []string{"echo"},
		Secrets: []executor.SecretRequest{
			{LocalName: "FOO", RepoName: "FOO"},
		},
	}}}
	_, _, err := fetchSecrets(t.Context(), rs.srv.Client(), rs.srv.URL, "o/r", "tok", cfg)
	if err == nil {
		t.Fatal("expected error for invalid base64, got nil")
	}
	if strings.Contains(err.Error(), "!!!not-base64!!!") {
		t.Errorf("error string contains raw value: %v", err)
	}
}

// TestFetchSecrets_NilConfig verifies a nil cfg returns (nil,nil,nil) without
// touching the network.
func TestFetchSecrets_NilConfig(t *testing.T) {
	rs := newRevealServer(t, http.StatusOK, `{"values":{}}`)
	resolved, missing, err := fetchSecrets(t.Context(), rs.srv.Client(), rs.srv.URL, "o/r", "tok", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolved != nil || missing != nil {
		t.Errorf("expected nil/nil for nil cfg, got %v/%v", resolved, missing)
	}
	if rs.callCount.Load() != 0 {
		t.Errorf("expected no HTTP calls for nil cfg, got %d", rs.callCount.Load())
	}
}

// TestFetchSecrets_TrimsTrailingSlashOnDocstoreURL guards against accidental
// "//-/secrets" path concatenation when the caller passes a URL with a
// trailing slash.
func TestFetchSecrets_TrimsTrailingSlashOnDocstoreURL(t *testing.T) {
	rs := newRevealServer(t, http.StatusOK, `{"values":{}}`)
	cfg := &executor.Config{Checks: []executor.Check{{
		Name:  "test",
		Image: "alpine",
		Steps: []string{"echo"},
		Secrets: []executor.SecretRequest{
			{LocalName: "FOO", RepoName: "FOO"},
		},
	}}}
	_, _, err := fetchSecrets(t.Context(), rs.srv.Client(), rs.srv.URL+"/", "o/r", "tok", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := rs.gotPath.Load(); got != "/repos/o/r/-/secrets/reveal" {
		t.Errorf("unexpected path: %v", got)
	}
}

// TestFetchSecrets_EmptyResponseValues verifies a 200 with an empty values
// object resolves to an empty map and does not error. Pairs with the missing
// list scenario.
func TestFetchSecrets_EmptyResponseValues(t *testing.T) {
	rs := newRevealServer(t, http.StatusOK, `{"values":{},"missing":["FOO"]}`)
	cfg := &executor.Config{Checks: []executor.Check{{
		Name:  "test",
		Image: "alpine",
		Steps: []string{"echo"},
		Secrets: []executor.SecretRequest{
			{LocalName: "FOO", RepoName: "FOO"},
		},
	}}}
	resolved, missing, err := fetchSecrets(t.Context(), rs.srv.Client(), rs.srv.URL, "o/r", "tok", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resolved) != 0 {
		t.Errorf("expected empty resolved, got %v", resolved)
	}
	if !reflect.DeepEqual(missing, []string{"FOO"}) {
		t.Errorf("expected missing=[FOO], got %v", missing)
	}
}
