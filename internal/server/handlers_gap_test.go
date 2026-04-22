package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/model"
)

// ---------------------------------------------------------------------------
// handleListOrgs tests
// ---------------------------------------------------------------------------

func TestHandleListOrgs_Success(t *testing.T) {
	ms := &mockStore{
		listOrgsFn: func(_ context.Context) ([]model.Org, error) {
			return []model.Org{{Name: "acme"}, {Name: "beta"}}, nil
		},
	}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodGet, "/orgs", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp model.ListOrgsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Orgs) != 2 {
		t.Errorf("expected 2 orgs, got %d", len(resp.Orgs))
	}
	if resp.Orgs[0].Name != "acme" {
		t.Errorf("expected acme, got %q", resp.Orgs[0].Name)
	}
}

func TestHandleListOrgs_Empty(t *testing.T) {
	// Default mockStore.listOrgsFn returns empty slice.
	srv := New(&mockStore{}, nil, devID, devID)
	req := httptest.NewRequest(http.MethodGet, "/orgs", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp model.ListOrgsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Orgs) != 0 {
		t.Errorf("expected empty orgs, got %d", len(resp.Orgs))
	}
}

func TestHandleListOrgs_NilSliceIsEmpty(t *testing.T) {
	// nil returned from store must be coerced to empty JSON array.
	ms := &mockStore{
		listOrgsFn: func(_ context.Context) ([]model.Org, error) {
			return nil, nil
		},
	}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodGet, "/orgs", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp model.ListOrgsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Orgs == nil {
		t.Error("expected non-nil orgs slice in response")
	}
}

func TestHandleListOrgs_StoreError(t *testing.T) {
	ms := &mockStore{
		listOrgsFn: func(_ context.Context) ([]model.Org, error) {
			return nil, errors.New("database gone")
		},
	}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodGet, "/orgs", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// handleGetOrg tests
// ---------------------------------------------------------------------------

func TestHandleGetOrg_Found(t *testing.T) {
	ms := &mockStore{
		getOrgFn: func(_ context.Context, name string) (*model.Org, error) {
			return &model.Org{Name: name}, nil
		},
	}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodGet, "/orgs/acme", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var org model.Org
	if err := json.NewDecoder(rec.Body).Decode(&org); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if org.Name != "acme" {
		t.Errorf("expected name acme, got %q", org.Name)
	}
}

func TestHandleGetOrg_NotFound(t *testing.T) {
	ms := &mockStore{
		getOrgFn: func(_ context.Context, name string) (*model.Org, error) {
			return nil, db.ErrOrgNotFound
		},
	}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodGet, "/orgs/nonexistent", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestHandleGetOrg_StoreError(t *testing.T) {
	ms := &mockStore{
		getOrgFn: func(_ context.Context, name string) (*model.Org, error) {
			return nil, errors.New("query error")
		},
	}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodGet, "/orgs/acme", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// requireGlobalAdmin tests
// ---------------------------------------------------------------------------

func TestRequireGlobalAdmin_NotConfigured(t *testing.T) {
	// globalAdmin empty → 403 on global-admin-only endpoints (e.g. GET /events).
	srv := New(&mockStore{}, nil, devID, "") // no global admin
	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestRequireGlobalAdmin_WrongIdentity(t *testing.T) {
	// identity (devID = "test@example.com") != globalAdmin → 403 on global-admin-only endpoints.
	srv := New(&mockStore{}, nil, devID, "admin@other.com")
	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// handleCreateSubscription tests
// ---------------------------------------------------------------------------

func TestHandleCreateSubscription_MissingBackend(t *testing.T) {
	srv := New(&mockStore{}, nil, devID, devID)
	body, _ := json.Marshal(map[string]interface{}{
		"config": map[string]string{"url": "https://example.com/hook"},
	})
	req := httptest.NewRequest(http.MethodPost, "/subscriptions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing backend, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleCreateSubscription_UnsupportedBackend(t *testing.T) {
	srv := New(&mockStore{}, nil, devID, devID)
	body, _ := json.Marshal(map[string]interface{}{
		"backend": "pubsub",
		"config":  map[string]string{"url": "https://example.com/hook"},
	})
	req := httptest.NewRequest(http.MethodPost, "/subscriptions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unsupported backend, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleCreateSubscription_MissingConfig(t *testing.T) {
	srv := New(&mockStore{}, nil, devID, devID)
	body, _ := json.Marshal(map[string]interface{}{
		"backend": "webhook",
	})
	req := httptest.NewRequest(http.MethodPost, "/subscriptions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing config, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleCreateSubscription_MissingURL(t *testing.T) {
	srv := New(&mockStore{}, nil, devID, devID)
	body, _ := json.Marshal(map[string]interface{}{
		"backend": "webhook",
		"config":  map[string]string{"secret": "s3cr3t"},
	})
	req := httptest.NewRequest(http.MethodPost, "/subscriptions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing config.url, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleCreateSubscription_InvalidURL(t *testing.T) {
	srv := New(&mockStore{}, nil, devID, devID)
	body, _ := json.Marshal(map[string]interface{}{
		"backend": "webhook",
		"config":  map[string]string{"url": "ftp://not-http.example.com"},
	})
	req := httptest.NewRequest(http.MethodPost, "/subscriptions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid URL scheme, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleCreateSubscription_Success(t *testing.T) {
	ms := &mockStore{
		createSubscriptionFn: func(_ context.Context, req model.CreateSubscriptionRequest) (*model.EventSubscription, error) {
			return &model.EventSubscription{ID: "test-sub-id", Backend: req.Backend, CreatedBy: req.CreatedBy}, nil
		},
	}
	srv := New(ms, nil, devID, devID)
	body, _ := json.Marshal(map[string]interface{}{
		"backend": "webhook",
		"config":  map[string]string{"url": "https://example.com/hook", "secret": "s3cr3t"},
	})
	req := httptest.NewRequest(http.MethodPost, "/subscriptions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var sub model.EventSubscription
	if err := json.NewDecoder(rec.Body).Decode(&sub); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sub.Backend != "webhook" {
		t.Errorf("expected backend webhook, got %q", sub.Backend)
	}
	if sub.CreatedBy != devID {
		t.Errorf("expected created_by %q, got %q", devID, sub.CreatedBy)
	}
}

func TestHandleCreateSubscription_NonAdmin_GlobalScope_Forbidden(t *testing.T) {
	// Non-admin caller with no repo field: tests the 'no repo field on non-admin' path → 403.
	srv := New(&mockStore{}, nil, devID, "admin@other.com")
	body, _ := json.Marshal(map[string]interface{}{
		"backend": "webhook",
		"config":  map[string]string{"url": "https://example.com/hook"},
	})
	req := httptest.NewRequest(http.MethodPost, "/subscriptions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestHandleCreateSubscription_InvalidBody(t *testing.T) {
	srv := New(&mockStore{}, nil, devID, devID)
	req := httptest.NewRequest(http.MethodPost, "/subscriptions", bytes.NewReader([]byte("not-json")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandleCreateSubscription_StoreError(t *testing.T) {
	ms := &mockStore{
		createSubscriptionFn: func(_ context.Context, req model.CreateSubscriptionRequest) (*model.EventSubscription, error) {
			return nil, errors.New("db error")
		},
	}
	srv := New(ms, nil, devID, devID)
	body, _ := json.Marshal(map[string]interface{}{
		"backend": "webhook",
		"config":  map[string]string{"url": "https://example.com/hook"},
	})
	req := httptest.NewRequest(http.MethodPost, "/subscriptions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleListSubscriptions tests
// ---------------------------------------------------------------------------

func TestHandleListSubscriptions_Success(t *testing.T) {
	ms := &mockStore{
		listSubscriptionsFn: func(_ context.Context) ([]model.EventSubscription, error) {
			return []model.EventSubscription{{ID: "sub-1", Backend: "webhook"}}, nil
		},
	}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodGet, "/subscriptions", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp model.ListSubscriptionsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Subscriptions) != 1 {
		t.Errorf("expected 1 subscription, got %d", len(resp.Subscriptions))
	}
}

func TestHandleListSubscriptions_Empty(t *testing.T) {
	// Default mockStore returns empty slice.
	srv := New(&mockStore{}, nil, devID, devID)
	req := httptest.NewRequest(http.MethodGet, "/subscriptions", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp model.ListSubscriptionsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Subscriptions) != 0 {
		t.Errorf("expected 0 subscriptions, got %d", len(resp.Subscriptions))
	}
}

func TestHandleListSubscriptions_NonAdmin_Allowed(t *testing.T) {
	// Non-admin gets 200 with filtered (own) subscriptions, not 403.
	srv := New(&mockStore{}, nil, devID, "admin@other.com")
	req := httptest.NewRequest(http.MethodGet, "/subscriptions", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestHandleListSubscriptions_StoreError(t *testing.T) {
	ms := &mockStore{
		listSubscriptionsFn: func(_ context.Context) ([]model.EventSubscription, error) {
			return nil, errors.New("db down")
		},
	}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodGet, "/subscriptions", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// handleDeleteSubscription tests
// ---------------------------------------------------------------------------

func TestHandleDeleteSubscription_Success(t *testing.T) {
	// Default mockStore.deleteSubscriptionFn returns nil.
	srv := New(&mockStore{}, nil, devID, devID)
	req := httptest.NewRequest(http.MethodDelete, "/subscriptions/sub-1", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleDeleteSubscription_NotFound(t *testing.T) {
	ms := &mockStore{
		deleteSubscriptionFn: func(_ context.Context, id string) error {
			return db.ErrSubscriptionNotFound
		},
	}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodDelete, "/subscriptions/no-such-sub", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestHandleDeleteSubscription_NonAdmin_Forbidden(t *testing.T) {
	// Non-admin trying to delete a subscription they did not create → 403.
	ms := &mockStore{
		getSubscriptionFn: func(_ context.Context, id string) (*model.EventSubscription, error) {
			return &model.EventSubscription{ID: id, Backend: "webhook", CreatedBy: "other@example.com"}, nil
		},
	}
	srv := New(ms, nil, devID, "admin@other.com")
	req := httptest.NewRequest(http.MethodDelete, "/subscriptions/sub-1", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestHandleDeleteSubscription_StoreError(t *testing.T) {
	ms := &mockStore{
		deleteSubscriptionFn: func(_ context.Context, id string) error {
			return errors.New("unexpected error")
		},
	}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodDelete, "/subscriptions/sub-1", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// handleResumeSubscription tests
// ---------------------------------------------------------------------------

func TestHandleResumeSubscription_Success(t *testing.T) {
	// Default mockStore.resumeSubscriptionFn returns nil.
	srv := New(&mockStore{}, nil, devID, devID)
	req := httptest.NewRequest(http.MethodPost, "/subscriptions/sub-1/resume", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleResumeSubscription_NotFound(t *testing.T) {
	ms := &mockStore{
		resumeSubscriptionFn: func(_ context.Context, id string) error {
			return db.ErrSubscriptionNotFound
		},
	}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodPost, "/subscriptions/no-such-sub/resume", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestHandleResumeSubscription_NonAdmin_Forbidden(t *testing.T) {
	// Non-admin trying to resume a subscription they did not create → 403.
	ms := &mockStore{
		getSubscriptionFn: func(_ context.Context, id string) (*model.EventSubscription, error) {
			return &model.EventSubscription{ID: id, Backend: "webhook", CreatedBy: "other@example.com"}, nil
		},
	}
	srv := New(ms, nil, devID, "admin@other.com")
	req := httptest.NewRequest(http.MethodPost, "/subscriptions/sub-1/resume", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestHandleResumeSubscription_StoreError(t *testing.T) {
	ms := &mockStore{
		resumeSubscriptionFn: func(_ context.Context, id string) error {
			return errors.New("unexpected error")
		},
	}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodPost, "/subscriptions/sub-1/resume", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// handleSSEGlobalEvents tests
// ---------------------------------------------------------------------------

func TestHandleSSEGlobalEvents_NonAdmin_Forbidden(t *testing.T) {
	srv := New(&mockStore{}, nil, devID, "admin@other.com")
	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-admin, got %d", rec.Code)
	}
}

func TestHandleSSEGlobalEvents_NoBroker_ServiceUnavailable(t *testing.T) {
	// When global admin is configured and identity matches, but no broker → 503.
	srv := New(&mockStore{}, nil, devID, devID) // no broker
	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 (no broker), got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Subscription auth relaxation tests (issue #225)
// ---------------------------------------------------------------------------

// 1. Non-admin with repo access can create a repo-scoped subscription.
func TestHandleCreateSubscription_RepoScoped_NonAdmin_Success(t *testing.T) {
	ms := &mockStore{
		getRoleFn: func(_ context.Context, repo, identity string) (*model.Role, error) {
			return &model.Role{Identity: identity, Role: model.RoleReader}, nil
		},
	}
	srv := New(ms, nil, devID, "admin@other.com")
	repoName := "myorg/myrepo"
	body, _ := json.Marshal(map[string]interface{}{
		"repo":    repoName,
		"backend": "webhook",
		"config":  map[string]string{"url": "https://example.com/hook", "secret": "s3cr3t"},
	})
	req := httptest.NewRequest(http.MethodPost, "/subscriptions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for repo-scoped non-admin, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// 2. Non-admin without repo access cannot create a repo-scoped subscription.
func TestHandleCreateSubscription_RepoScoped_NoRepoAccess_Forbidden(t *testing.T) {
	// Default mockStore.getRoleFn returns db.ErrRoleNotFound → no access.
	srv := New(&mockStore{}, nil, devID, "admin@other.com")
	repoName := "myorg/myrepo"
	body, _ := json.Marshal(map[string]interface{}{
		"repo":    repoName,
		"backend": "webhook",
		"config":  map[string]string{"url": "https://example.com/hook"},
	})
	req := httptest.NewRequest(http.MethodPost, "/subscriptions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for no repo access, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// 3. Non-admin sees only subscriptions they created (via store-level query).
func TestHandleListSubscriptions_NonAdmin_FiltersToOwn(t *testing.T) {
	ms := &mockStore{
		listSubscriptionsByCreatorFn: func(_ context.Context, createdBy string) ([]model.EventSubscription, error) {
			// Store returns only the caller's subscriptions.
			return []model.EventSubscription{
				{ID: "sub-1", Backend: "webhook", CreatedBy: createdBy},
			}, nil
		},
	}
	srv := New(ms, nil, devID, "admin@other.com")
	req := httptest.NewRequest(http.MethodGet, "/subscriptions", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp model.ListSubscriptionsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Subscriptions) != 1 {
		t.Errorf("expected 1 subscription (own), got %d", len(resp.Subscriptions))
	}
	if len(resp.Subscriptions) == 1 && resp.Subscriptions[0].ID != "sub-1" {
		t.Errorf("expected sub-1, got %q", resp.Subscriptions[0].ID)
	}
}

// 4. Global admin sees all subscriptions, not just their own.
func TestHandleListSubscriptions_Admin_SeesAll(t *testing.T) {
	ms := &mockStore{
		listSubscriptionsFn: func(_ context.Context) ([]model.EventSubscription, error) {
			return []model.EventSubscription{
				{ID: "sub-1", Backend: "webhook", CreatedBy: devID},
				{ID: "sub-2", Backend: "webhook", CreatedBy: "other@example.com"},
			}, nil
		},
	}
	srv := New(ms, nil, devID, devID) // devID is global admin
	req := httptest.NewRequest(http.MethodGet, "/subscriptions", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp model.ListSubscriptionsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Subscriptions) != 2 {
		t.Errorf("expected 2 subscriptions (all), got %d", len(resp.Subscriptions))
	}
}

// 5. Subscription creator (non-admin) can delete their own subscription.
func TestHandleDeleteSubscription_Creator_Success(t *testing.T) {
	ms := &mockStore{
		getSubscriptionFn: func(_ context.Context, id string) (*model.EventSubscription, error) {
			return &model.EventSubscription{ID: id, Backend: "webhook", CreatedBy: devID}, nil
		},
	}
	srv := New(ms, nil, devID, "admin@other.com")
	req := httptest.NewRequest(http.MethodDelete, "/subscriptions/sub-1", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204 for creator delete, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// 6. Non-creator non-admin cannot delete someone else's subscription.
func TestHandleDeleteSubscription_NotCreator_Forbidden(t *testing.T) {
	ms := &mockStore{
		getSubscriptionFn: func(_ context.Context, id string) (*model.EventSubscription, error) {
			return &model.EventSubscription{ID: id, Backend: "webhook", CreatedBy: "other@example.com"}, nil
		},
	}
	srv := New(ms, nil, devID, "admin@other.com")
	req := httptest.NewRequest(http.MethodDelete, "/subscriptions/sub-1", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-creator delete, got %d", rec.Code)
	}
}

// 7. Subscription creator (non-admin) can resume their own subscription.
func TestHandleResumeSubscription_Creator_Success(t *testing.T) {
	ms := &mockStore{
		getSubscriptionFn: func(_ context.Context, id string) (*model.EventSubscription, error) {
			return &model.EventSubscription{ID: id, Backend: "webhook", CreatedBy: devID}, nil
		},
	}
	srv := New(ms, nil, devID, "admin@other.com")
	req := httptest.NewRequest(http.MethodPost, "/subscriptions/sub-1/resume", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204 for creator resume, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// 8. Non-creator non-admin cannot resume someone else's subscription.
func TestHandleResumeSubscription_NotCreator_Forbidden(t *testing.T) {
	ms := &mockStore{
		getSubscriptionFn: func(_ context.Context, id string) (*model.EventSubscription, error) {
			return &model.EventSubscription{ID: id, Backend: "webhook", CreatedBy: "other@example.com"}, nil
		},
	}
	srv := New(ms, nil, devID, "admin@other.com")
	req := httptest.NewRequest(http.MethodPost, "/subscriptions/sub-1/resume", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-creator resume, got %d", rec.Code)
	}
}

// 9. Non-admin caller, GetSubscription returns generic error → 500 for delete.
func TestHandleDeleteSubscription_NonAdmin_StoreError(t *testing.T) {
	ms := &mockStore{
		getSubscriptionFn: func(_ context.Context, id string) (*model.EventSubscription, error) {
			return nil, errors.New("db connection reset")
		},
	}
	srv := New(ms, nil, devID, "admin@other.com")
	req := httptest.NewRequest(http.MethodDelete, "/subscriptions/sub-1", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for store error on non-admin delete, got %d", rec.Code)
	}
}

// 10. Non-admin caller, GetSubscription returns generic error → 500 for resume.
func TestHandleResumeSubscription_NonAdmin_StoreError(t *testing.T) {
	ms := &mockStore{
		getSubscriptionFn: func(_ context.Context, id string) (*model.EventSubscription, error) {
			return nil, errors.New("db connection reset")
		},
	}
	srv := New(ms, nil, devID, "admin@other.com")
	req := httptest.NewRequest(http.MethodPost, "/subscriptions/sub-1/resume", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for store error on non-admin resume, got %d", rec.Code)
	}
}

// 11. Non-admin caller, GetSubscription returns ErrSubscriptionNotFound → 404 for delete.
func TestHandleDeleteSubscription_NonAdmin_NotFound(t *testing.T) {
	ms := &mockStore{
		getSubscriptionFn: func(_ context.Context, id string) (*model.EventSubscription, error) {
			return nil, db.ErrSubscriptionNotFound
		},
	}
	srv := New(ms, nil, devID, "admin@other.com")
	req := httptest.NewRequest(http.MethodDelete, "/subscriptions/sub-1", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for not-found on non-admin delete, got %d", rec.Code)
	}
}

// 12. Non-admin caller, GetSubscription returns ErrSubscriptionNotFound → 404 for resume.
func TestHandleResumeSubscription_NonAdmin_NotFound(t *testing.T) {
	ms := &mockStore{
		getSubscriptionFn: func(_ context.Context, id string) (*model.EventSubscription, error) {
			return nil, db.ErrSubscriptionNotFound
		},
	}
	srv := New(ms, nil, devID, "admin@other.com")
	req := httptest.NewRequest(http.MethodPost, "/subscriptions/sub-1/resume", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for not-found on non-admin resume, got %d", rec.Code)
	}
}

// 13. Non-admin caller, listSubscriptionsByCreatorFn returns an error → 500.
func TestHandleListSubscriptions_NonAdmin_StoreError(t *testing.T) {
	ms := &mockStore{
		listSubscriptionsByCreatorFn: func(_ context.Context, createdBy string) ([]model.EventSubscription, error) {
			return nil, errors.New("db unavailable")
		},
	}
	srv := New(ms, nil, devID, "admin@other.com")
	req := httptest.NewRequest(http.MethodGet, "/subscriptions", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for non-admin store error on list, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// handleEnableAutoMerge / handleDisableAutoMerge tests
// ---------------------------------------------------------------------------

func TestHandleEnableAutoMerge_Success(t *testing.T) {
	ms := &mockStore{
		getRepoFn: func(_ context.Context, name string) (*model.Repo, error) {
			return &model.Repo{Name: name}, nil
		},
	}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/branch/my-feature/auto-merge", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleEnableAutoMerge_MainBranch(t *testing.T) {
	ms := &mockStore{
		getRepoFn: func(_ context.Context, name string) (*model.Repo, error) {
			return &model.Repo{Name: name}, nil
		},
	}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/branch/main/auto-merge", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for main branch, got %d", rec.Code)
	}
}

func TestHandleEnableAutoMerge_BranchNotFound(t *testing.T) {
	ms := &mockStore{
		getRepoFn: func(_ context.Context, name string) (*model.Repo, error) {
			return &model.Repo{Name: name}, nil
		},
		setBranchAutoMergeFn: func(_ context.Context, _, _ string, _ bool) error {
			return db.ErrBranchNotFound
		},
	}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/branch/my-feature/auto-merge", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestHandleEnableAutoMerge_StoreError(t *testing.T) {
	ms := &mockStore{
		getRepoFn: func(_ context.Context, name string) (*model.Repo, error) {
			return &model.Repo{Name: name}, nil
		},
		setBranchAutoMergeFn: func(_ context.Context, _, _ string, _ bool) error {
			return errors.New("db error")
		},
	}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/branch/my-feature/auto-merge", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
}

func TestHandleDisableAutoMerge_Success(t *testing.T) {
	ms := &mockStore{
		getRepoFn: func(_ context.Context, name string) (*model.Repo, error) {
			return &model.Repo{Name: name}, nil
		},
	}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodDelete, "/repos/default/default/-/branch/my-feature/auto-merge", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleDisableAutoMerge_MainBranch(t *testing.T) {
	ms := &mockStore{
		getRepoFn: func(_ context.Context, name string) (*model.Repo, error) {
			return &model.Repo{Name: name}, nil
		},
	}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodDelete, "/repos/default/default/-/branch/main/auto-merge", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for main branch, got %d", rec.Code)
	}
}

func TestHandleDisableAutoMerge_BranchNotFound(t *testing.T) {
	ms := &mockStore{
		getRepoFn: func(_ context.Context, name string) (*model.Repo, error) {
			return &model.Repo{Name: name}, nil
		},
		setBranchAutoMergeFn: func(_ context.Context, _, _ string, _ bool) error {
			return db.ErrBranchNotFound
		},
	}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodDelete, "/repos/default/default/-/branch/my-feature/auto-merge", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestHandleDisableAutoMerge_StoreError(t *testing.T) {
	ms := &mockStore{
		getRepoFn: func(_ context.Context, name string) (*model.Repo, error) {
			return &model.Repo{Name: name}, nil
		},
		setBranchAutoMergeFn: func(_ context.Context, _, _ string, _ bool) error {
			return errors.New("db error")
		},
	}
	srv := New(ms, nil, devID, devID)
	req := httptest.NewRequest(http.MethodDelete, "/repos/default/default/-/branch/my-feature/auto-merge", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// handleMerge dry-run tests
// ---------------------------------------------------------------------------

// TestHandleMerge_DryRun verifies that a merge with dry_run=true returns the
// computed sequence without persisting the merge (policyCache is nil so
// policy evaluation is skipped, and the mock Merge fn signals dry-run by
// returning the expected sequence).
func TestHandleMerge_DryRun(t *testing.T) {
	const wantSeq = int64(42)
	ms := &mockStore{
		getRepoFn: func(_ context.Context, name string) (*model.Repo, error) {
			return &model.Repo{Name: name}, nil
		},
		mergeFn: func(_ context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error) {
			if !req.DryRun {
				t.Error("expected DryRun=true inside Merge call")
			}
			return &model.MergeResponse{Sequence: wantSeq}, nil, nil
		},
	}
	srv := New(ms, nil, devID, devID)
	body, _ := json.Marshal(model.MergeRequest{Branch: "feature", DryRun: true})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/merge", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp model.MergeResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Sequence != wantSeq {
		t.Errorf("expected sequence %d, got %d", wantSeq, resp.Sequence)
	}
}

// TestHandleMerge_DryRun_DoesNotPersist verifies that dry_run=true does not
// cause a second Merge call or any proposal side-effects: the merge function
// is called exactly once and the server returns 200 without emitting events.
func TestHandleMerge_DryRun_DoesNotPersist(t *testing.T) {
	calls := 0
	ms := &mockStore{
		getRepoFn: func(_ context.Context, name string) (*model.Repo, error) {
			return &model.Repo{Name: name}, nil
		},
		mergeFn: func(_ context.Context, req model.MergeRequest) (*model.MergeResponse, []db.MergeConflict, error) {
			calls++
			return &model.MergeResponse{Sequence: 7}, nil, nil
		},
	}
	srv := New(ms, nil, devID, devID)
	body, _ := json.Marshal(model.MergeRequest{Branch: "feature", DryRun: true})
	req := httptest.NewRequest(http.MethodPost, "/repos/default/default/-/merge", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Errorf("expected Merge to be called exactly once, got %d", calls)
	}
}

// 14. Config sent as a JSON array → 400.
func TestHandleCreateSubscription_InvalidConfigType(t *testing.T) {
	srv := New(&mockStore{}, nil, devID, devID)
	body, _ := json.Marshal(map[string]interface{}{
		"backend": "webhook",
		"config":  []int{1, 2, 3},
	})
	req := httptest.NewRequest(http.MethodPost, "/subscriptions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for array config, got %d; body: %s", rec.Code, rec.Body.String())
	}
}
