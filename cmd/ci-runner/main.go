package main

import (
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/dlorenc/docstore/internal/executor"
)

type runRequest struct {
	SourceDir string          `json:"source_dir"`
	Config    executor.Config `json:"config"`
}

type runResponse struct {
	Checks []executor.CheckResult `json:"checks"`
}

func main() {
	buildkitAddr := flag.String("buildkit-addr", "unix:///run/buildkit/buildkitd.sock", "buildkitd socket address")
	port := flag.String("port", "8080", "HTTP listen port")
	flag.Parse()

	var handler slog.Handler
	if os.Getenv("LOG_FORMAT") == "text" {
		handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	} else {
		handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	}
	slog.SetDefault(slog.New(handler))

	exec, err := executor.New(*buildkitAddr)
	if err != nil {
		slog.Error("failed to connect to buildkitd", "addr", *buildkitAddr, "error", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /run", func(w http.ResponseWriter, r *http.Request) {
		var req runRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.SourceDir == "" {
			http.Error(w, "source_dir is required", http.StatusBadRequest)
			return
		}

		results, err := exec.Run(r.Context(), req.SourceDir, req.Config)
		if err != nil {
			slog.Error("run failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(runResponse{Checks: results})
	})

	srv := &http.Server{
		Addr:         ":" + *port,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 300 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	slog.Info("starting ci-runner", "port", *port, "buildkit_addr", *buildkitAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}
