package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
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
		if !filepath.IsAbs(req.SourceDir) {
			http.Error(w, "source_dir must be an absolute path", http.StatusBadRequest)
			return
		}
		if _, err := os.Stat(req.SourceDir); err != nil {
			http.Error(w, fmt.Sprintf("source_dir does not exist: %v", err), http.StatusBadRequest)
			return
		}
		for i, check := range req.Config.Checks {
			if check.Image == "" {
				http.Error(w, fmt.Sprintf("check[%d] (%q): image is required", i, check.Name), http.StatusBadRequest)
				return
			}
			if len(check.Steps) == 0 {
				http.Error(w, fmt.Sprintf("check[%d] (%q): at least one step is required", i, check.Name), http.StatusBadRequest)
				return
			}
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
		Addr:        ":" + *port,
		Handler:     mux,
		ReadTimeout: 10 * time.Second,
		// WriteTimeout is intentionally not set: long-running CI builds stream
		// responses back over the HTTP connection and must not be cut off by a
		// server-side write deadline. The execution timeout is controlled by the
		// request context instead.
		IdleTimeout: 60 * time.Second,
	}

	// Start server in background.
	go func() {
		slog.Info("starting ci-runner", "port", *port, "buildkit_addr", *buildkitAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	// Wait for interrupt signal, then gracefully shut down.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	slog.Info("shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("shutdown error", "error", err)
		os.Exit(1)
	}
	if err := exec.Close(); err != nil {
		slog.Error("executor close error", "error", err)
	}
	slog.Info("stopped")
}
