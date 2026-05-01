package main

import (
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"
)

// site is embedded at build time by running `mkdocs build -d cmd/docs/site`
// before `ko build`. The `all:` prefix includes dot-files.
//
//go:embed all:site
var siteEmbed embed.FS

func main() {
	port := "8080"
	if p := os.Getenv("PORT"); p != "" {
		port = p
	}

	// Strip the "site/" prefix so the FS root is the MkDocs output directory.
	siteFS, err := fs.Sub(siteEmbed, "site")
	if err != nil {
		slog.Error("sub fs", "err", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.Handle("/", handler(siteFS))

	addr := fmt.Sprintf(":%s", port)
	slog.Info("starting docs server", "addr", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}

func handler(siteFS fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(siteFS))

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
		fileServer.ServeHTTP(rw, r)
		if rw.status == http.StatusNotFound {
			serve404(w, siteFS)
		}
	})
}

func serve404(w http.ResponseWriter, siteFS fs.FS) {
	data, err := fs.ReadFile(siteFS, "404.html")
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
