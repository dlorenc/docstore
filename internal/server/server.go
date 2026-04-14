package server

import (
	"encoding/json"
	"net/http"
)

// New returns an http.Handler with all routes registered.
func New() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", handleHealth)

	// Read endpoints (placeholders).
	mux.HandleFunc("GET /tree", notImplemented)
	mux.HandleFunc("GET /file/{path...}", notImplemented)
	mux.HandleFunc("GET /commit/{sequence}", notImplemented)
	mux.HandleFunc("GET /diff", notImplemented)
	mux.HandleFunc("GET /branches", notImplemented)
	mux.HandleFunc("GET /branch/{name}/status", notImplemented)

	// Write endpoints (placeholders).
	mux.HandleFunc("POST /commit", notImplemented)
	mux.HandleFunc("POST /branch", notImplemented)
	mux.HandleFunc("POST /merge", notImplemented)
	mux.HandleFunc("POST /rebase", notImplemented)
	mux.HandleFunc("POST /review", notImplemented)
	mux.HandleFunc("POST /check", notImplemented)
	mux.HandleFunc("DELETE /branch/{name}", notImplemented)

	return mux
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func notImplemented(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotImplemented)
	json.NewEncoder(w).Encode(map[string]string{"error": "not implemented"})
}
