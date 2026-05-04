package server

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	dbpkg "github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/model"
)

// makeJobJWTFull creates a signed RS256 JWT for a CI job with configurable repo/branch.
func makeJobJWTFull(t *testing.T, key *rsa.PrivateKey, kid, issuer, audience, jobID, repo, branch string, exp time.Time) string {
	t.Helper()
	headerJSON, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"})
	payloadJSON, _ := json.Marshal(map[string]any{
		"iss":    issuer,
		"sub":    "repo:" + repo + ":branch:" + branch + ":check:",
		"aud":    audience,
		"exp":    exp.Unix(),
		"iat":    time.Now().Unix(),
		"job_id": jobID,
		"repo":   repo,
		"branch": branch,
	})
	headerB64 := base64.RawURLEncoding.EncodeToString(headerJSON)
	payloadB64 := base64.RawURLEncoding.EncodeToString(payloadJSON)
	signingInput := headerB64 + "." + payloadB64
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// buildOIDCServer constructs a server with OIDC enabled using a static in-memory key.
// Returns the http.Handler and the RSA key so callers can mint test tokens.
func buildOIDCServer(t *testing.T, ms WriteStore) (http.Handler, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	const kid = "oidc-test-key"
	const issuer = "https://oidc.docstore.dev"
	const audience = "docstore"

	// Pre-populate the key cache with our test key to avoid HTTP fetching.
	kc := &keyCache{
		keys:      map[string]crypto.PublicKey{kid: &key.PublicKey},
		fetchedAt: time.Now(),
		ttl:       time.Hour,
	}
	s := &server{
		commitStore:  ms,
		oidcKeyCache: kc,
		oidcAudience: audience,
		oidcIssuer:   issuer,
	}
	// Use devIdentity so OAuth is bypassed for non-OIDC paths in the same test server.
	handler := s.buildHandler("dev@example.com", "", ms)
	return handler, key
}

// oidcToken is a convenience wrapper for tests in this file.
func oidcToken(t *testing.T, key *rsa.PrivateKey, repo, branch string) string {
	t.Helper()
	const kid = "oidc-test-key"
	const issuer = "https://oidc.docstore.dev"
	const audience = "docstore"
	return makeJobJWTFull(t, key, kid, issuer, audience, "job-test-001", repo, branch, time.Now().Add(time.Hour))
}

// ---------------------------------------------------------------------------
// OIDC JWT repo scope enforcement tests
// ---------------------------------------------------------------------------

// TestOIDC_CheckRejectsWrongRepo verifies that a job token for acme/myrepo
// cannot POST a check result to victim/other (the cross-repo spoofing vector).
func TestOIDC_CheckRejectsWrongRepo(t *testing.T) {
	ms := &mockStore{
		getRepoFn: func(_ context.Context, name string) (*model.Repo, error) {
			return &model.Repo{Name: name}, nil
		},
	}
	handler, key := buildOIDCServer(t, ms)

	// Job token claims acme/myrepo but we POST to victim/other.
	tok := oidcToken(t, key, "acme/myrepo", "main")
	body, _ := json.Marshal(map[string]any{
		"branch":     "main",
		"check_name": "ci",
		"status":     "passed",
	})
	req := httptest.NewRequest(http.MethodPost, "/repos/victim/other/-/check", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for cross-repo check spoofing, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// TestOIDC_CheckAllowsOwnRepo verifies that a valid job token may POST a check
// result to its own repo's endpoint.
func TestOIDC_CheckAllowsOwnRepo(t *testing.T) {
	ms := &mockStore{
		getRepoFn: func(_ context.Context, name string) (*model.Repo, error) {
			return &model.Repo{Name: name}, nil
		},
		createCheckRunFn: func(_ context.Context, repo, branch, checkName string, status model.CheckRunStatus, reporter string, logURL *string, atSeq *int64, attempt int16, metadata json.RawMessage) (*model.CheckRun, error) {
			return &model.CheckRun{ID: "cr-1", Sequence: 1}, nil
		},
	}
	handler, key := buildOIDCServer(t, ms)

	tok := oidcToken(t, key, "acme/myrepo", "main")
	body, _ := json.Marshal(map[string]any{
		"branch":     "main",
		"check_name": "ci",
		"status":     "passed",
	})
	req := httptest.NewRequest(http.MethodPost, "/repos/acme/myrepo/-/check", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for own-repo check, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// TestOIDC_WriteToNonRepoPathDenied verifies that a job token cannot POST to
// non-repo-scoped write endpoints like POST /repos (create repo) or POST /orgs.
func TestOIDC_WriteToNonRepoPathDenied(t *testing.T) {
	ms := &mockStore{}
	handler, key := buildOIDCServer(t, ms)

	tok := oidcToken(t, key, "acme/myrepo", "main")

	writePaths := []string{
		"/repos",
		"/orgs",
	}
	for _, path := range writePaths {
		body, _ := json.Marshal(map[string]any{"name": "test"})
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s: expected 403 for job token on non-repo path, got %d; body: %s", path, rec.Code, rec.Body.String())
		}
	}
}

// TestOIDC_GetAcrossReposAllowed verifies that a job token may issue GET requests
// to paths outside its own repo. Read operations are not restricted by repo scope.
func TestOIDC_GetAcrossReposAllowed(t *testing.T) {
	ms := &mockStore{
		getRepoFn: func(_ context.Context, name string) (*model.Repo, error) {
			return &model.Repo{Name: name}, nil
		},
	}
	handler, key := buildOIDCServer(t, ms)

	// Token is for acme/myrepo, but we read from victim/other.
	tok := oidcToken(t, key, "acme/myrepo", "main")
	req := httptest.NewRequest(http.MethodGet, "/repos/victim/other", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// GET should succeed regardless of repo mismatch.
	if rec.Code == http.StatusForbidden {
		t.Fatalf("GET on different repo should not return 403 for OIDC token, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// TestOIDC_CrossRepoCommitDenied verifies that the repo scope enforcement covers
// endpoints other than /check (commit is another write endpoint).
func TestOIDC_CrossRepoCommitDenied(t *testing.T) {
	ms := &mockStore{
		getRepoFn: func(_ context.Context, name string) (*model.Repo, error) {
			return &model.Repo{Name: name}, nil
		},
	}
	handler, key := buildOIDCServer(t, ms)

	tok := oidcToken(t, key, "acme/myrepo", "main")
	body, _ := json.Marshal(map[string]any{
		"branch":  "feature/x",
		"message": "oops",
		"files":   []any{},
	})
	req := httptest.NewRequest(http.MethodPost, "/repos/victim/other/-/commit", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for cross-repo commit, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// TestOIDC_OwnRepoNonCheckEndpointDenied verifies that a job token cannot POST to
// endpoints other than /check on its own repo — the allowlist gate rejects them.
func TestOIDC_OwnRepoNonCheckEndpointDenied(t *testing.T) {
	ms := &mockStore{
		getRepoFn: func(_ context.Context, name string) (*model.Repo, error) {
			return &model.Repo{Name: name}, nil
		},
	}
	handler, key := buildOIDCServer(t, ms)

	// Token is for acme/myrepo; we POST to acme/myrepo/-/commit (own repo, wrong endpoint).
	tok := oidcToken(t, key, "acme/myrepo", "main")
	body, _ := json.Marshal(map[string]any{
		"branch":  "main",
		"message": "pwned",
		"files":   []any{},
	})
	req := httptest.NewRequest(http.MethodPost, "/repos/acme/myrepo/-/commit", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for own-repo non-check endpoint, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// TestOIDC_CrossRepoDeleteDenied verifies that a job token cannot DELETE a repo it
// does not own. The bare /repos/:name DELETE path must also be repo-scoped.
func TestOIDC_CrossRepoDeleteDenied(t *testing.T) {
	ms := &mockStore{}
	handler, key := buildOIDCServer(t, ms)

	tok := oidcToken(t, key, "acme/myrepo", "main")
	req := httptest.NewRequest(http.MethodDelete, "/repos/victim/other", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for cross-repo DELETE /repos/victim/other, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// validateRepo in handleCheck / handleReview
// ---------------------------------------------------------------------------

// TestHandleCheck_RepoNotFound verifies that handleCheck returns 404 when the
// repo does not exist (validateRepo added per handler checklist).
func TestHandleCheck_RepoNotFound(t *testing.T) {
	ms := &mockStore{
		getRepoFn: func(_ context.Context, name string) (*model.Repo, error) {
			return nil, dbpkg.ErrRepoNotFound
		},
	}
	srv := New(ms, nil, devID, devID)
	body, _ := json.Marshal(map[string]any{
		"branch":     "main",
		"check_name": "ci",
		"status":     "passed",
	})
	req := httptest.NewRequest(http.MethodPost, "/repos/noorg/norepo/-/check", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for non-existent repo in handleCheck, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleReview_RepoNotFound verifies that handleReview returns 404 when the
// repo does not exist (validateRepo added per handler checklist).
func TestHandleReview_RepoNotFound(t *testing.T) {
	ms := &mockStore{
		getRepoFn: func(_ context.Context, name string) (*model.Repo, error) {
			return nil, dbpkg.ErrRepoNotFound
		},
	}
	srv := New(ms, nil, devID, devID)
	body, _ := json.Marshal(map[string]any{
		"branch": "main",
		"status": "approved",
	})
	req := httptest.NewRequest(http.MethodPost, "/repos/noorg/norepo/-/review", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for non-existent repo in handleReview, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// jobEndpointAllowed unit tests
// ---------------------------------------------------------------------------

func TestJobEndpointAllowed_DefaultChecksPermission(t *testing.T) {
	// "check" is always allowed regardless of the permissions slice.
	if !jobEndpointAllowed("check", nil) {
		t.Error("check should be allowed with nil permissions")
	}
	if !jobEndpointAllowed("check", []string{}) {
		t.Error("check should be allowed with empty permissions")
	}
	if !jobEndpointAllowed("check", []string{"contents"}) {
		t.Error("check should be allowed with any permissions")
	}
}

func TestJobEndpointAllowed_ContentsPermission(t *testing.T) {
	contentsEndpoints := []string{"commit", "merge", "rebase", "purge", "branch", "branch/main", "branch/feat/foo"}
	for _, ep := range contentsEndpoints {
		if jobEndpointAllowed(ep, nil) {
			t.Errorf("%q should be denied without contents permission", ep)
		}
		if jobEndpointAllowed(ep, []string{"checks"}) {
			t.Errorf("%q should be denied with only checks permission", ep)
		}
		if !jobEndpointAllowed(ep, []string{"contents"}) {
			t.Errorf("%q should be allowed with contents permission", ep)
		}
	}
}

func TestJobEndpointAllowed_ProposalsPermission(t *testing.T) {
	proposalEndpoints := []string{"review", "comment", "comment/abc123", "proposals", "proposals/my-id", "proposals/my-id/close"}
	for _, ep := range proposalEndpoints {
		if jobEndpointAllowed(ep, nil) {
			t.Errorf("%q should be denied without proposals permission", ep)
		}
		if !jobEndpointAllowed(ep, []string{"proposals"}) {
			t.Errorf("%q should be allowed with proposals permission", ep)
		}
	}
}

func TestJobEndpointAllowed_IssuesPermission(t *testing.T) {
	issueEndpoints := []string{"issues", "issues/1", "issues/1/close", "issues/1/comments"}
	for _, ep := range issueEndpoints {
		if jobEndpointAllowed(ep, nil) {
			t.Errorf("%q should be denied without issues permission", ep)
		}
		if !jobEndpointAllowed(ep, []string{"issues"}) {
			t.Errorf("%q should be allowed with issues permission", ep)
		}
	}
}

func TestJobEndpointAllowed_ReleasesPermission(t *testing.T) {
	releaseEndpoints := []string{"releases", "releases/v1.0"}
	for _, ep := range releaseEndpoints {
		if jobEndpointAllowed(ep, nil) {
			t.Errorf("%q should be denied without releases permission", ep)
		}
		if !jobEndpointAllowed(ep, []string{"releases"}) {
			t.Errorf("%q should be allowed with releases permission", ep)
		}
	}
}

func TestJobEndpointAllowed_CIPermission(t *testing.T) {
	if jobEndpointAllowed("ci/run", nil) {
		t.Error("ci/run should be denied without ci permission")
	}
	if !jobEndpointAllowed("ci/run", []string{"ci"}) {
		t.Error("ci/run should be allowed with ci permission")
	}
}

func TestJobEndpointAllowed_UnknownEndpointDenied(t *testing.T) {
	unknownEndpoints := []string{"", "admin", "roles", "chain", "tree"}
	for _, ep := range unknownEndpoints {
		if jobEndpointAllowed(ep, []string{"checks", "contents", "proposals", "issues", "releases", "ci"}) {
			t.Errorf("%q should be denied even with all permissions", ep)
		}
	}
}

// TestOIDC_ContentsPermissionAllowsCommit verifies that a job with
// "contents" permission in its JWT can POST /-/commit to its own repo.
func TestOIDC_ContentsPermissionAllowsCommit(t *testing.T) {
	// Use a mock that recognizes the repo but we send empty files so
	// handleCommit returns 400 (before touching the service layer).
	// The key assertion is that the permissions gate does NOT return 403.
	ms := &mockStore{
		getRepoFn: func(_ context.Context, name string) (*model.Repo, error) {
			return &model.Repo{Name: name}, nil
		},
	}
	handler, key := buildOIDCServer(t, ms)

	// Build a token with permissions: ["checks", "contents"]
	tok := makeJobJWTWithPermissions(t, key, "acme/myrepo", "main", []string{"checks", "contents"})
	body, _ := json.Marshal(map[string]any{
		"branch":  "main",
		"message": "test commit",
		"files":   []any{}, // empty files → 400 from handler (not 403 from gate)
	})
	req := httptest.NewRequest(http.MethodPost, "/repos/acme/myrepo/-/commit", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusForbidden {
		t.Fatalf("expected commit to pass the permissions gate, got 403; body: %s", rec.Body.String())
	}
}

// TestOIDC_NoContentsPermissionDeniesCommit verifies that a job without
// "contents" permission cannot POST /-/commit even to its own repo.
func TestOIDC_NoContentsPermissionDeniesCommit(t *testing.T) {
	ms := &mockStore{
		getRepoFn: func(_ context.Context, name string) (*model.Repo, error) {
			return &model.Repo{Name: name}, nil
		},
	}
	handler, key := buildOIDCServer(t, ms)

	// Token has only the default "checks" permission (no contents).
	tok := makeJobJWTWithPermissions(t, key, "acme/myrepo", "main", []string{"checks"})
	body, _ := json.Marshal(map[string]any{
		"branch":  "main",
		"message": "test commit",
		"files":   []any{},
	})
	req := httptest.NewRequest(http.MethodPost, "/repos/acme/myrepo/-/commit", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for commit without contents permission, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// makeJobJWTWithPermissions creates a signed RS256 JWT with the given permissions claim.
func makeJobJWTWithPermissions(t *testing.T, key *rsa.PrivateKey, repo, branch string, permissions []string) string {
	t.Helper()
	const kid = "oidc-test-key"
	const issuer = "https://oidc.docstore.dev"
	const audience = "docstore"
	headerJSON, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"})
	payloadJSON, _ := json.Marshal(map[string]any{
		"iss":         issuer,
		"sub":         "repo:" + repo + ":branch:" + branch + ":check:",
		"aud":         audience,
		"exp":         time.Now().Add(time.Hour).Unix(),
		"iat":         time.Now().Unix(),
		"job_id":      "job-perm-test",
		"repo":        repo,
		"branch":      branch,
		"permissions": permissions,
	})
	headerB64 := base64.RawURLEncoding.EncodeToString(headerJSON)
	payloadB64 := base64.RawURLEncoding.EncodeToString(payloadJSON)
	signingInput := headerB64 + "." + payloadB64
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}
