package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dlorenc/docstore/internal/server"
)

func TestDSConfigEndpoint_NoIAP(t *testing.T) {
	// Server without IAP client ID configured.
	h := server.NewWithReadStore(nil, nil, "dev@example.com", "")
	req := httptest.NewRequest("GET", "/.well-known/ds-config", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var got map[string]any
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	auth, ok := got["auth"].(map[string]any)
	if !ok {
		t.Fatalf("expected auth object, got %T", got["auth"])
	}
	if auth["type"] != "none" {
		t.Errorf("expected type=none, got %v", auth["type"])
	}
}

func TestDSConfigEndpoint_WithIAP(t *testing.T) {
	// Server with IAP client ID configured.
	h := server.NewWithBroker(nil, nil, nil, nil, "dev@example.com", "", "test-client-id", "test-secret")
	req := httptest.NewRequest("GET", "/.well-known/ds-config", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var got map[string]any
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	auth, ok := got["auth"].(map[string]any)
	if !ok {
		t.Fatalf("expected auth object, got %T", got["auth"])
	}
	if auth["type"] != "iap" {
		t.Errorf("expected type=iap, got %v", auth["type"])
	}
	if auth["client_id"] != "test-client-id" {
		t.Errorf("expected client_id=test-client-id, got %v", auth["client_id"])
	}
	if auth["client_secret"] != "test-secret" {
		t.Errorf("expected client_secret=test-secret, got %v", auth["client_secret"])
	}
}

func TestDSConfigEndpoint_WithIAPNoSecret(t *testing.T) {
	// Server with IAP client ID but no client secret.
	h := server.NewWithBroker(nil, nil, nil, nil, "dev@example.com", "", "test-client-id", "")
	req := httptest.NewRequest("GET", "/.well-known/ds-config", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var got map[string]any
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	auth, ok := got["auth"].(map[string]any)
	if !ok {
		t.Fatalf("expected auth object, got %T", got["auth"])
	}
	if auth["type"] != "iap" {
		t.Errorf("expected type=iap, got %v", auth["type"])
	}
	if _, has := auth["client_secret"]; has {
		t.Errorf("expected no client_secret field when secret is empty")
	}
}

func TestDSConfigEndpoint_Unauthenticated(t *testing.T) {
	// Endpoint must be accessible without auth (no IAP token required).
	// Use a server without dev identity — real IAP mode — and verify 200 (not 401).
	h := server.NewWithBroker(nil, nil, nil, nil, "", "", "some-client-id", "")
	req := httptest.NewRequest("GET", "/.well-known/ds-config", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (unauthenticated access allowed), got %d", w.Code)
	}
}
