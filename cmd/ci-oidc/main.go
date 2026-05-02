// Package main implements the ci-oidc binary.
//
// ci-oidc is a standalone Cloud Run service that issues OIDC identity tokens
// for CI jobs. It provides:
//   - GET  /.well-known/openid-configuration  — OIDC discovery document
//   - GET  /.well-known/jwks.json             — JSON Web Key Set
//   - POST /ci/token                          — issue a signed JWT for a CI job
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/lib/pq"

	"github.com/dlorenc/docstore/internal/citoken"
	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/model"
	"github.com/lestrrat-go/jwx/v3/jwt"
)

// ---------------------------------------------------------------------------
// Store interface — minimal subset of db.Store needed by ci-oidc.
// ---------------------------------------------------------------------------

type tokenStore interface {
	LookupRequestToken(ctx context.Context, hashedToken string) (*model.CIJob, error)
	RecordOIDCToken(ctx context.Context, jti, jobID, audience string, exp time.Time) error
}

// ---------------------------------------------------------------------------
// server holds handler dependencies.
// ---------------------------------------------------------------------------

type server struct {
	store     tokenStore
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
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	w.Write(data) //nolint:errcheck
}

// ---------------------------------------------------------------------------
// POST /ci/token
// ---------------------------------------------------------------------------

type tokenRequest struct {
	Audience  string `json:"audience"`
	CheckName string `json:"check_name"`
}

func (s *server) handleToken(w http.ResponseWriter, r *http.Request) {
	// 1. Extract Bearer token.
	authHdr := r.Header.Get("Authorization")
	plaintext := strings.TrimPrefix(authHdr, "Bearer ")
	if plaintext == "" || plaintext == authHdr {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// 2. Validate request token → look up job.
	hashed := citoken.HashRequestToken(plaintext)
	job, err := s.store.LookupRequestToken(r.Context(), hashed)
	if err == db.ErrTokenInvalid {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err != nil {
		slog.Error("lookup request token failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// 3. Decode request body.
	body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	var req tokenRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Audience == "" {
		http.Error(w, "audience is required", http.StatusBadRequest)
		return
	}

	// 4. Derive ref_type from trigger type.
	refType := triggerToRefType(job.TriggerType)

	// 5. Extract org as first path segment of repo (e.g. "acme" from "acme/myrepo").
	org := job.Repo
	if idx := strings.Index(job.Repo, "/"); idx >= 0 {
		org = job.Repo[:idx]
	}

	// 6. Build claims.
	checkName := req.CheckName
	claims := citoken.JobClaims{
		Issuer:      s.issuerURL,
		Subject:     fmt.Sprintf("repo:%s:branch:%s:check:%s", job.Repo, job.Branch, checkName),
		Audience:    req.Audience,
		Repo:        job.Repo,
		Org:         org,
		Branch:      job.Branch,
		CheckName:   checkName,
		RefType:     refType,
		TriggeredBy: "",
		JobID:       job.ID,
		Sequence:    job.Sequence,
		Permissions: job.Permissions,
	}

	// 7. Issue JWT.
	tokenStr, err := citoken.IssueJWT(r.Context(), s.signer, claims)
	if err != nil {
		slog.Error("issue jwt failed", "job_id", job.ID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// 8. Extract jti from the issued token (parse without verification since we just issued it).
	parsed, err := jwt.ParseInsecure([]byte(tokenStr))
	if err != nil {
		slog.Error("parse issued jwt failed", "job_id", job.ID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	jti, ok := parsed.JwtID()
	if !ok || jti == "" {
		slog.Error("issued jwt missing jti", "job_id", job.ID)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	exp := time.Now().Add(time.Hour)

	// 9. Record audit log entry.
	if err := s.store.RecordOIDCToken(r.Context(), jti, job.ID, req.Audience, exp); err != nil {
		slog.Error("record oidc token failed", "job_id", job.ID, "jti", jti, "error", err)
		// Non-fatal: token was issued; log and continue.
	}

	// 10. Return token.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"token": tokenStr}) //nolint:errcheck
}

// triggerToRefType maps a CI job trigger_type to an OIDC ref_type claim.
func triggerToRefType(triggerType string) string {
	switch triggerType {
	case "push":
		return "post-submit"
	case "proposal", "proposal_synchronized":
		return "pre-submit"
	case "schedule":
		return "schedule"
	case "manual":
		return "manual"
	default:
		return "unknown"
	}
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

// newPublicMux returns a mux that serves only the public OIDC discovery
// endpoints: /healthz, /.well-known/openid-configuration, /.well-known/jwks.json.
// It does NOT register POST /ci/token. Used when PUBLIC_ONLY=true.
func newPublicMux(s *server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealth)
	mux.HandleFunc("GET /.well-known/openid-configuration", s.handleDiscovery)
	mux.HandleFunc("GET /.well-known/jwks.json", s.handleJWKS)
	return mux
}

func newMux(s *server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealth)
	mux.HandleFunc("GET /.well-known/openid-configuration", s.handleDiscovery)
	mux.HandleFunc("GET /.well-known/jwks.json", s.handleJWKS)
	mux.HandleFunc("POST /ci/token", s.handleToken)
	return mux
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	port := flag.String("port", "8080", "HTTP listen port")
	issuerURL := flag.String("issuer-url", "https://oidc.docstore.dev", "OIDC issuer URL")
	kmsKeyVersion := flag.String("kms-key-version", "", "KMS key version resource name (empty = use LocalSigner)")
	publicOnly := flag.Bool("public-only", false, "Only serve public endpoints (/.well-known/*, /healthz); skip DATABASE_URL")
	flag.Parse()

	if v := os.Getenv("PORT"); v != "" && *port == "8080" {
		*port = v
	}
	if v := os.Getenv("ISSUER_URL"); v != "" {
		*issuerURL = v
	}
	if v := os.Getenv("KMS_KEY_VERSION"); v != "" {
		*kmsKeyVersion = v
	}
	if v := os.Getenv("PUBLIC_ONLY"); v == "true" || v == "1" {
		*publicOnly = true
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

	// Build signer (needed in all modes for PublicKeys()).
	ctx := context.Background()
	var signer citoken.Signer
	if *kmsKeyVersion != "" {
		kmsSigner, err := citoken.NewKMSSigner(ctx, *kmsKeyVersion)
		if err != nil {
			slog.Error("failed to create KMS signer", "error", err)
			os.Exit(1)
		}
		signer = kmsSigner
		slog.Info("using KMS signer", "key_version", *kmsKeyVersion)
	} else {
		slog.Warn("KMS_KEY_VERSION not set — using LocalSigner (dev mode only)")
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

	var mux *http.ServeMux
	if *publicOnly {
		slog.Info("starting ci-oidc in public-only mode")
		mux = newPublicMux(srv)
	} else {
		// Connect to Postgres.
		dsn := os.Getenv("DATABASE_URL")
		if dsn == "" {
			slog.Error("DATABASE_URL environment variable is required")
			os.Exit(1)
		}
		database, err := db.Open(dsn)
		if err != nil {
			slog.Error("failed to connect to database", "error", err)
			os.Exit(1)
		}
		defer database.Close()

		srv.store = db.NewStore(database)
		mux = newMux(srv)
	}

	httpSrv := &http.Server{
		Addr:        ":" + *port,
		Handler:     mux,
		ReadTimeout: 10 * time.Second,
		IdleTimeout: 60 * time.Second,
	}

	go func() {
		slog.Info("starting ci-oidc", "port", *port, "issuer", *issuerURL)
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
