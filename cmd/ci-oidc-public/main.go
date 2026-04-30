// Package main implements the ci-oidc-public binary.
//
// ci-oidc-public is a public-facing Cloud Run service that serves OIDC
// discovery metadata and the JSON Web Key Set so that external parties can
// verify CI OIDC tokens.  It does NOT issue tokens; that is ci-oidc's job.
//
// Endpoints:
//
//	GET /.well-known/openid-configuration  — OIDC provider metadata
//	GET /.well-known/jwks.json             — JSON Web Key Set
//	GET /healthz                           — liveness probe
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dlorenc/docstore/internal/citoken"
)

// ---------------------------------------------------------------------------
// server holds handler dependencies.
// ---------------------------------------------------------------------------

type server struct {
	signer    citoken.Signer
	issuerURL string
}

// ---------------------------------------------------------------------------
// GET /.well-known/openid-configuration
// ---------------------------------------------------------------------------

func (s *server) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	doc := map[string]any{
		"issuer":                                s.issuerURL,
		"jwks_uri":                              s.issuerURL + "/.well-known/jwks.json",
		"response_types_supported":              []string{"id_token"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
	}
	data, err := json.Marshal(doc)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.WriteHeader(http.StatusOK)
	w.Write(data) //nolint:errcheck
}

// ---------------------------------------------------------------------------
// GET /.well-known/jwks.json
// ---------------------------------------------------------------------------

func (s *server) handleJWKS(w http.ResponseWriter, r *http.Request) {
	data, err := s.signer.PublicKeys(r.Context())
	if err != nil {
		slog.Error("failed to get public keys", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.WriteHeader(http.StatusOK)
	w.Write(data) //nolint:errcheck
}

// ---------------------------------------------------------------------------
// GET /healthz
// ---------------------------------------------------------------------------

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`)) //nolint:errcheck
}

// ---------------------------------------------------------------------------
// HTTP mux
// ---------------------------------------------------------------------------

func newMux(s *server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealth)
	mux.HandleFunc("GET /.well-known/openid-configuration", s.handleDiscovery)
	mux.HandleFunc("GET /.well-known/jwks.json", s.handleJWKS)
	return mux
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	port := flag.String("port", "8080", "HTTP listen port")
	issuerURL := flag.String("issuer-url", "https://oidc.docstore.dev", "OIDC issuer URL")
	kmsKey := flag.String("kms-key", "", "KMS key version resource name (empty = use LocalSigner for dev)")
	flag.Parse()

	if v := os.Getenv("PORT"); v != "" {
		*port = v
	}
	if v := os.Getenv("ISSUER"); v != "" {
		*issuerURL = v
	}
	if v := os.Getenv("KMS_KEY"); v != "" {
		*kmsKey = v
	}

	// Set up structured logging.
	var logLevel slog.LevelVar
	if lvlStr := os.Getenv("LOG_LEVEL"); lvlStr != "" {
		if err := logLevel.UnmarshalText([]byte(lvlStr)); err != nil {
			logLevel.Set(slog.LevelInfo)
		}
	}
	var handler slog.Handler
	if os.Getenv("LOG_FORMAT") == "text" {
		handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: &logLevel})
	} else {
		handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: &logLevel})
	}
	slog.SetDefault(slog.New(handler))

	// Build signer.
	ctx := context.Background()
	var signer citoken.Signer
	if *kmsKey != "" {
		kmsSigner, err := citoken.NewKMSSigner(ctx, *kmsKey)
		if err != nil {
			slog.Error("failed to create KMS signer", "error", err)
			os.Exit(1)
		}
		signer = kmsSigner
		slog.Info("using KMS signer", "kms_key", *kmsKey)
	} else {
		slog.Warn("KMS_KEY not set — using LocalSigner (dev mode only)")
		localSigner, err := citoken.NewLocalSigner()
		if err != nil {
			slog.Error("failed to create local signer", "error", err)
			os.Exit(1)
		}
		signer = localSigner
	}

	srv := &server{
		signer:    signer,
		issuerURL: strings.TrimRight(*issuerURL, "/"),
	}

	httpSrv := &http.Server{
		Addr:        ":" + *port,
		Handler:     newMux(srv),
		ReadTimeout: 10 * time.Second,
		IdleTimeout: 60 * time.Second,
	}

	go func() {
		slog.Info("starting ci-oidc-public", "port", *port, "issuer", *issuerURL)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	slog.Info("shutting down")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown error", "error", err)
		os.Exit(1)
	}
	slog.Info("stopped")
}
