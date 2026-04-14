package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/store"
)

// CommitStore abstracts the database operations needed by the commit handler.
type CommitStore interface {
	Commit(ctx context.Context, req model.CommitRequest) (*model.CommitResponse, error)
}

// New returns an http.Handler with all routes registered.
// commitStore provides write operations (POST /commit); pass nil if only
// read/health endpoints are needed.
// database provides read operations (GET /tree, /file, /commit); pass nil
// if only write/health endpoints are needed (e.g. in unit tests).
func New(commitStore CommitStore, database *sql.DB) http.Handler {
	s := &server{commitStore: commitStore}
	if database != nil {
		s.readStore = store.New(database)
	}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", handleHealth)

	// Read endpoints.
	mux.HandleFunc("GET /tree", s.handleTree)
	mux.HandleFunc("GET /file/{path...}", s.handleFile)
	mux.HandleFunc("GET /commit/{sequence}", s.handleGetCommit)
	mux.HandleFunc("GET /diff", notImplemented)
	mux.HandleFunc("GET /branches", notImplemented)
	mux.HandleFunc("GET /branch/{name}/status", notImplemented)

	// Write endpoints.
	mux.HandleFunc("POST /commit", s.handleCommit)
	mux.HandleFunc("POST /branch", notImplemented)
	mux.HandleFunc("POST /merge", notImplemented)
	mux.HandleFunc("POST /rebase", notImplemented)
	mux.HandleFunc("POST /review", notImplemented)
	mux.HandleFunc("POST /check", notImplemented)
	mux.HandleFunc("DELETE /branch/{name}", notImplemented)

	return mux
}

type server struct {
	commitStore CommitStore
	readStore   *store.Store
}

func (s *server) handleCommit(w http.ResponseWriter, r *http.Request) {
	var req model.CommitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if req.Branch == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "branch is required"})
		return
	}
	if len(req.Files) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "at least one file is required"})
		return
	}
	if req.Message == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message is required"})
		return
	}
	if req.Author == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "author is required"})
		return
	}
	for _, f := range req.Files {
		if f.Path == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file path is required"})
			return
		}
	}

	resp, err := s.commitStore.Commit(r.Context(), req)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrBranchNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "branch not found"})
		case errors.Is(err, db.ErrBranchNotActive):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "branch is not active"})
		default:
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal server error"})
		}
		return
	}

	writeJSON(w, http.StatusCreated, resp)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func notImplemented(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "not implemented"})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
