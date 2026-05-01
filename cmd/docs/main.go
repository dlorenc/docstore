package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

var siteDir = func() string {
	if v := os.Getenv("KO_DATA_PATH"); v != "" {
		return v
	}
	return "cmd/docs/.kodata"
}()

func main() {
	port := "8080"
	if p := os.Getenv("PORT"); p != "" {
		port = p
	}

	sitePath := filepath.Join(siteDir, "site")

	mux := http.NewServeMux()
	mux.Handle("/", handler(sitePath))

	addr := fmt.Sprintf(":%s", port)
	slog.Info("starting docs server", "addr", addr, "site", sitePath)
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}

func handler(sitePath string) http.Handler {
	fs := http.FileServer(http.Dir(sitePath))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Security headers on all responses.
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")

		// Cache-Control: immutable for hashed assets, no-cache for everything else.
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}

		// Custom 404.
		rw := &responseWriter{ResponseWriter: w}
		fs.ServeHTTP(rw, r)
		if rw.status == http.StatusNotFound {
			serve404(w, sitePath)
		}
	})
}

func serve404(w http.ResponseWriter, sitePath string) {
	data, err := os.ReadFile(filepath.Join(sitePath, "404.html"))
	if err != nil {
		http.Error(w, "404 not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	w.Write(data) //nolint:errcheck
}

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (rw *responseWriter) WriteHeader(status int) {
	rw.status = status
	if status != http.StatusNotFound {
		rw.ResponseWriter.WriteHeader(status)
		rw.wrote = true
	}
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	if rw.status == http.StatusNotFound {
		// Discard the default 404 body; serve404 will write the custom one.
		return len(b), nil
	}
	rw.wrote = true
	return rw.ResponseWriter.Write(b)
}
