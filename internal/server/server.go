package server

import (
	"encoding/json"
	"net/http"
)

// Server holds dependencies and registers HTTP routes.
type Server struct {
	store Store
}

// New returns an http.Handler with all routes registered.
func New(store Store) http.Handler {
	s := &Server{store: store}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", handleHealth)

	// Read endpoints.
	mux.HandleFunc("GET /tree", notImplemented)
	mux.HandleFunc("GET /file/{path...}", notImplemented)
	mux.HandleFunc("GET /commit/{sequence}", notImplemented)
	mux.HandleFunc("GET /diff", s.handleDiff)
	mux.HandleFunc("GET /branches", s.handleListBranches)
	mux.HandleFunc("GET /branch/{name}/status", notImplemented)

	// Write endpoints.
	mux.HandleFunc("POST /commit", notImplemented)
	mux.HandleFunc("POST /branch", s.handleCreateBranch)
	mux.HandleFunc("POST /merge", s.handleMerge)
	mux.HandleFunc("POST /rebase", s.handleRebase)
	mux.HandleFunc("POST /review", notImplemented)
	mux.HandleFunc("POST /check", notImplemented)
	mux.HandleFunc("DELETE /branch/{name...}", s.handleDeleteBranch)

	return mux
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func notImplemented(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "not implemented")
}

// writeJSON encodes v as JSON and writes it with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
