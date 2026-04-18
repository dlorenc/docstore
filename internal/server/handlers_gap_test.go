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
	// globalAdmin empty → 403.
	srv := New(&mockStore{}, nil, devID, "") // no global admin
	req := httptest.NewRequest(http.MethodGet, "/subscriptions", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestRequireGlobalAdmin_WrongIdentity(t *testing.T) {
	// identity (devID = "test@example.com") != globalAdmin → 403.
	srv := New(&mockStore{}, nil, devID, "admin@other.com")
	req := httptest.NewRequest(http.MethodGet, "/subscriptions", nil)
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
	srv := New(&mockStore{}, nil, devID, devID)
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
}

func TestHandleCreateSubscription_NonAdmin_Forbidden(t *testing.T) {
	// devID != global admin → 403.
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

func TestHandleListSubscriptions_NonAdmin_Forbidden(t *testing.T) {
	srv := New(&mockStore{}, nil, devID, "admin@other.com")
	req := httptest.NewRequest(http.MethodGet, "/subscriptions", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
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
	srv := New(&mockStore{}, nil, devID, "admin@other.com")
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
	srv := New(&mockStore{}, nil, devID, "admin@other.com")
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
