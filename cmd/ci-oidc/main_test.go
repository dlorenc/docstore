package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dlorenc/docstore/internal/citoken"
	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/model"
	"github.com/lestrrat-go/jwx/v3/jwt"
)

// ---------------------------------------------------------------------------
// stubStore satisfies the tokenStore interface for tests.
// ---------------------------------------------------------------------------

type stubStore struct {
	job      *model.CIJob
	lookupFn func(ctx context.Context, hashedToken string) (*model.CIJob, error)

	recordedJTI      string
	recordedJobID    string
	recordedAudience string
}

func (s *stubStore) LookupRequestToken(ctx context.Context, hashedToken string) (*model.CIJob, error) {
	if s.lookupFn != nil {
		return s.lookupFn(ctx, hashedToken)
	}
	if s.job == nil {
		return nil, db.ErrTokenInvalid
	}
	return s.job, nil
}

func (s *stubStore) RecordOIDCToken(ctx context.Context, jti, jobID, audience string, exp time.Time) error {
	s.recordedJTI = jti
	s.recordedJobID = jobID
	s.recordedAudience = audience
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func newTestServer(t *testing.T, store tokenStore) (*server, citoken.Signer) {
	t.Helper()
	signer, err := citoken.NewLocalSigner()
	if err != nil {
		t.Fatalf("NewLocalSigner: %v", err)
	}
	s := &server{
		store:     store,
		signer:    signer,
		issuerURL: "https://oidc.example.test",
	}
	return s, signer
}

func newRequest(t *testing.T, method, path string, body []byte) *http.Request {
	t.Helper()
	var bodyReader *bytes.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	} else {
		bodyReader = bytes.NewReader(nil)
	}
	r, err := http.NewRequest(method, path, bodyReader)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return r
}

func defaultJob() *model.CIJob {
	return &model.CIJob{
		ID:          "job-uuid-1234",
		Repo:        "org/repo",
		Branch:      "main",
		Sequence:    10,
		Status:      "claimed",
		TriggerType: "push",
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestHandleDiscovery(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t, &stubStore{})

	r := newRequest(t, http.MethodGet, "/.well-known/openid-configuration", nil)
	w := httptest.NewRecorder()
	s.handleDiscovery(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var doc map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode discovery doc: %v", err)
	}
	if doc["issuer"] != s.issuerURL {
		t.Errorf("issuer = %v, want %q", doc["issuer"], s.issuerURL)
	}
	wantJWKSURI := s.issuerURL + "/.well-known/jwks.json"
	if doc["jwks_uri"] != wantJWKSURI {
		t.Errorf("jwks_uri = %v, want %q", doc["jwks_uri"], wantJWKSURI)
	}
	if cc := w.Header().Get("Cache-Control"); cc == "" {
		t.Error("Cache-Control header missing")
	}
}

func TestHandleJWKS(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t, &stubStore{})

	r := newRequest(t, http.MethodGet, "/.well-known/jwks.json", nil)
	w := httptest.NewRecorder()
	s.handleJWKS(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var jwks struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &jwks); err != nil {
		t.Fatalf("decode jwks: %v", err)
	}
	if len(jwks.Keys) == 0 {
		t.Fatal("expected at least one key in JWKS")
	}
	key := jwks.Keys[0]
	if key["kid"] == "" || key["kid"] == nil {
		t.Error("expected non-empty kid in JWKS key")
	}
	if key["alg"] != "RS256" {
		t.Errorf("alg = %v, want RS256", key["alg"])
	}
}

func TestHandleToken_ValidRequest(t *testing.T) {
	t.Parallel()
	store := &stubStore{job: defaultJob()}
	s, _ := newTestServer(t, store)

	body, _ := json.Marshal(map[string]string{"audience": "https://example.com"})
	r := newRequest(t, http.MethodPost, "/ci/token", body)
	r.Header.Set("Authorization", "Bearer validtoken")
	w := httptest.NewRecorder()
	s.handleToken(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	tokenStr := resp["token"]
	if tokenStr == "" {
		t.Fatal("expected non-empty token field")
	}

	// Parse without verification to check claims.
	tok, err := jwt.ParseInsecure([]byte(tokenStr))
	if err != nil {
		t.Fatalf("ParseInsecure: %v", err)
	}
	iss, ok := tok.Issuer()
	if !ok || iss != s.issuerURL {
		t.Errorf("iss = %q, want %q", iss, s.issuerURL)
	}
	sub, ok := tok.Subject()
	if !ok || sub == "" {
		t.Error("expected non-empty sub")
	}
	aud, ok := tok.Audience()
	if !ok || len(aud) == 0 || aud[0] != "https://example.com" {
		t.Errorf("aud = %v, want [https://example.com]", aud)
	}
	jti, ok := tok.JwtID()
	if !ok || jti == "" {
		t.Error("expected non-empty jti")
	}
	var repo string
	if err := tok.Get("repo", &repo); err != nil || repo != defaultJob().Repo {
		t.Errorf("repo = %q, want %q", repo, defaultJob().Repo)
	}
	var org string
	if err := tok.Get("org", &org); err != nil || org != "org" {
		t.Errorf("org = %q, want %q", org, "org")
	}
	var branch string
	if err := tok.Get("branch", &branch); err != nil || branch != "main" {
		t.Errorf("branch = %q, want %q", branch, "main")
	}

	// RecordOIDCToken should have been called with matching jti.
	if store.recordedJTI != jti {
		t.Errorf("recorded jti = %q, want %q", store.recordedJTI, jti)
	}
	if store.recordedJobID != defaultJob().ID {
		t.Errorf("recorded job_id = %q, want %q", store.recordedJobID, defaultJob().ID)
	}
}

func TestHandleToken_InvalidToken(t *testing.T) {
	t.Parallel()
	store := &stubStore{
		lookupFn: func(_ context.Context, _ string) (*model.CIJob, error) {
			return nil, db.ErrTokenInvalid
		},
	}
	s, _ := newTestServer(t, store)

	body, _ := json.Marshal(map[string]string{"audience": "https://example.com"})
	r := newRequest(t, http.MethodPost, "/ci/token", body)
	r.Header.Set("Authorization", "Bearer badtoken")
	w := httptest.NewRecorder()
	s.handleToken(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestHandleToken_MissingAudience(t *testing.T) {
	t.Parallel()
	store := &stubStore{job: defaultJob()}
	s, _ := newTestServer(t, store)

	body, _ := json.Marshal(map[string]string{"audience": ""})
	r := newRequest(t, http.MethodPost, "/ci/token", body)
	r.Header.Set("Authorization", "Bearer validtoken")
	w := httptest.NewRecorder()
	s.handleToken(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleToken_MissingAuthHeader(t *testing.T) {
	t.Parallel()
	store := &stubStore{job: defaultJob()}
	s, _ := newTestServer(t, store)

	body, _ := json.Marshal(map[string]string{"audience": "https://example.com"})
	r := newRequest(t, http.MethodPost, "/ci/token", body)
	// No Authorization header.
	w := httptest.NewRecorder()
	s.handleToken(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestHandleToken_CheckNameInSubject(t *testing.T) {
	t.Parallel()
	store := &stubStore{job: defaultJob()}
	s, _ := newTestServer(t, store)

	body, _ := json.Marshal(map[string]string{
		"audience":   "https://example.com",
		"check_name": "ci/deploy",
	})
	r := newRequest(t, http.MethodPost, "/ci/token", body)
	r.Header.Set("Authorization", "Bearer validtoken")
	w := httptest.NewRecorder()
	s.handleToken(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp) //nolint:errcheck
	tok, err := jwt.ParseInsecure([]byte(resp["token"]))
	if err != nil {
		t.Fatalf("ParseInsecure: %v", err)
	}
	sub, _ := tok.Subject()
	wantSub := "repo:org/repo:branch:main:check:ci/deploy"
	if sub != wantSub {
		t.Errorf("sub = %q, want %q", sub, wantSub)
	}
}

func TestHandleToken_RefTypeMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		triggerType string
		wantRefType string
	}{
		{"push", "post-submit"},
		{"proposal", "pre-submit"},
		{"proposal_synchronized", "pre-submit"},
		{"schedule", "schedule"},
		{"manual", "manual"},
		{"unknown-type", "unknown"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.triggerType, func(t *testing.T) {
			t.Parallel()
			job := defaultJob()
			job.TriggerType = tc.triggerType
			store := &stubStore{job: job}
			s, _ := newTestServer(t, store)

			body, _ := json.Marshal(map[string]string{"audience": "https://example.com"})
			r := newRequest(t, http.MethodPost, "/ci/token", body)
			r.Header.Set("Authorization", "Bearer validtoken")
			w := httptest.NewRecorder()
			s.handleToken(w, r)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
			}

			var resp map[string]string
			json.Unmarshal(w.Body.Bytes(), &resp) //nolint:errcheck
			tok, err := jwt.ParseInsecure([]byte(resp["token"]))
			if err != nil {
				t.Fatalf("ParseInsecure: %v", err)
			}
			var refType string
			if err := tok.Get("ref_type", &refType); err != nil {
				t.Fatalf("get ref_type: %v", err)
			}
			if refType != tc.wantRefType {
				t.Errorf("ref_type = %q, want %q", refType, tc.wantRefType)
			}
		})
	}
}

// Verify that triggerToRefType is exercised (also tested indirectly above).
func TestTriggerToRefType(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, out string }{
		{"push", "post-submit"},
		{"proposal", "pre-submit"},
		{"proposal_synchronized", "pre-submit"},
		{"schedule", "schedule"},
		{"manual", "manual"},
		{"", "unknown"},
		{"other", "unknown"},
	}
	for _, tt := range tests {
		got := triggerToRefType(tt.in)
		if got != tt.out {
			t.Errorf("triggerToRefType(%q) = %q, want %q", tt.in, got, tt.out)
		}
	}
}

