package main

import (
	"cmp"
	"context"
	"encoding/base64"
	"flag"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "github.com/lib/pq"

	"github.com/dlorenc/docstore/internal/automerge"
	"github.com/dlorenc/docstore/internal/blob"
	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/events"
	"github.com/dlorenc/docstore/internal/logstore"
	"github.com/dlorenc/docstore/internal/policy"
	"github.com/dlorenc/docstore/internal/server"
	"github.com/dlorenc/docstore/internal/store"
)

func main() {
	devIdentity := flag.String("dev-identity", "", "bypass Google ID token validation and use this identity (local dev/testing only)")
	bootstrapAdmin := flag.String("bootstrap-admin", "", "identity granted admin access to repos with no admin assigned yet")
	oauthClientID := flag.String("oauth-client-id", "", "Google OAuth 2.0 client ID advertised via /.well-known/ds-config (overrides OAUTH_CLIENT_ID env var)")
	oauthClientSecret := flag.String("oauth-client-secret", "", "Google OAuth 2.0 client secret for the web OAuth callback handler (overrides OAUTH_CLIENT_SECRET env var)")
	sessionSecretB64 := flag.String("session-secret", "", "base64-encoded HMAC secret for signing session cookies (overrides SESSION_SECRET env var)")
	flag.Parse()

	// Set up structured logger based on LOG_FORMAT and LOG_LEVEL env vars.
	// Default is JSON (GCP Cloud Run picks this up natively).
	// LOG_LEVEL accepts: debug, info, warn, error (default: info).
	var logLevel slog.LevelVar
	if lvlStr := os.Getenv("LOG_LEVEL"); lvlStr != "" {
		if err := logLevel.UnmarshalText([]byte(lvlStr)); err != nil {
			// Unknown level — leave at default (Info) and warn below.
			logLevel.Set(slog.LevelInfo)
		}
	}
	var handler slog.Handler
	if os.Getenv("LOG_FORMAT") == "text" {
		handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: &logLevel})
	} else {
		handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: &logLevel})
	}
	logger := slog.New(handler)
	slog.SetDefault(logger)

	// Also accept env vars for container-based configuration.
	if *devIdentity == "" {
		*devIdentity = os.Getenv("DEV_IDENTITY")
	}
	if *bootstrapAdmin == "" {
		*bootstrapAdmin = os.Getenv("BOOTSTRAP_ADMIN")
	}
	if *oauthClientID == "" {
		*oauthClientID = os.Getenv("OAUTH_CLIENT_ID")
	}
	if *oauthClientSecret == "" {
		*oauthClientSecret = os.Getenv("OAUTH_CLIENT_SECRET")
	}
	if *sessionSecretB64 == "" {
		*sessionSecretB64 = os.Getenv("SESSION_SECRET")
	}

	if *devIdentity != "" {
		logger.Warn("Google ID token validation disabled", "dev_identity", *devIdentity)
	}
	if *bootstrapAdmin != "" {
		logger.Info("bootstrap admin configured", "identity", *bootstrapAdmin)
	}

	port := cmp.Or(os.Getenv("PORT"), "8080")

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		logger.Error("DATABASE_URL environment variable is required")
		os.Exit(1)
	}

	database, err := db.Open(dsn)
	if err != nil {
		logger.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer database.Close()

	if err := db.RunMigrations(database); err != nil {
		logger.Error("failed to run migrations", "error", err)
		os.Exit(1)
	}
	logger.Info("migrations complete")

	commitStore := db.NewStore(database)

	// Initialize external blob store from environment variables.
	//   DOCSTORE_BLOB_STORE=gcs|local   (default: local)
	//   DOCSTORE_BLOB_BUCKET=my-bucket  (required for gcs)
	//   DOCSTORE_BLOB_THRESHOLD_BYTES=1048576 (default 1 MB)
	//   DOCSTORE_BLOB_LOCAL_DIR=/tmp/docstore-blobs
	var bs blob.BlobStore
	blobStoreType := os.Getenv("DOCSTORE_BLOB_STORE")
	blobThreshold := int64(1 << 20) // default 1 MB
	if threshStr := os.Getenv("DOCSTORE_BLOB_THRESHOLD_BYTES"); threshStr != "" {
		if v, err := strconv.ParseInt(threshStr, 10, 64); err == nil {
			blobThreshold = v
		}
	}

	switch blobStoreType {
	case "gcs":
		bucket := os.Getenv("DOCSTORE_BLOB_BUCKET")
		if bucket == "" {
			logger.Error("DOCSTORE_BLOB_BUCKET is required when DOCSTORE_BLOB_STORE=gcs")
			os.Exit(1)
		}
		gcsStore, err := blob.NewGCSBlobStore(context.Background(), bucket)
		if err != nil {
			logger.Error("failed to create GCS blob store", "error", err)
			os.Exit(1)
		}
		bs = gcsStore
		logger.Info("blob store configured", "backend", "gcs", "bucket", bucket, "threshold_bytes", blobThreshold)
	case "", "local":
		localDir := cmp.Or(os.Getenv("DOCSTORE_BLOB_LOCAL_DIR"), "/tmp/docstore-blobs")
		localStore, err := blob.NewLocalBlobStore(localDir)
		if err != nil {
			logger.Error("failed to create local blob store", "error", err)
			os.Exit(1)
		}
		bs = localStore
		logger.Info("blob store configured", "backend", "local", "dir", localDir, "threshold_bytes", blobThreshold)
	default:
		logger.Error("unknown DOCSTORE_BLOB_STORE value", "value", blobStoreType)
		os.Exit(1)
	}

	commitStore.SetBlobStore(bs, blobThreshold)

	// Create the event broker and start the outbox dispatcher.
	// The dispatcher runs until dispatchCancel is called on shutdown.
	broker := events.NewBroker(database)
	dispatchCtx, dispatchCancel := context.WithCancel(context.Background())
	events.StartDispatcher(dispatchCtx, database, dsn, broker)

	// Configure presigned archive URLs (optional).
	// ARCHIVE_HMAC_SECRET: base64-encoded raw HMAC secret bytes.
	// ARCHIVE_BASE_URL: public server base URL used when constructing presigned archive
	// download URLs returned to CI workers, e.g. "https://docstore.dev". Must use
	// http or https scheme and include a host. Trailing slash is stripped automatically.
	archiveHMACSecretB64 := os.Getenv("ARCHIVE_HMAC_SECRET")
	archiveBaseURL := os.Getenv("ARCHIVE_BASE_URL")
	if archiveBaseURL != "" {
		parsedBaseURL, parseErr := url.Parse(archiveBaseURL)
		if parseErr != nil {
			logger.Error("invalid ARCHIVE_BASE_URL", "error", parseErr)
			os.Exit(1)
		}
		if parsedBaseURL.Scheme != "http" && parsedBaseURL.Scheme != "https" {
			logger.Error("ARCHIVE_BASE_URL must use http or https scheme", "url", archiveBaseURL)
			os.Exit(1)
		}
		if parsedBaseURL.Host == "" {
			logger.Error("ARCHIVE_BASE_URL must include a host", "url", archiveBaseURL)
			os.Exit(1)
		}
		archiveBaseURL = strings.TrimRight(archiveBaseURL, "/")
	}
	var archiveHMACSecret []byte
	if archiveHMACSecretB64 != "" {
		archiveHMACSecret, err = base64.StdEncoding.DecodeString(archiveHMACSecretB64)
		if err != nil {
			logger.Error("invalid ARCHIVE_HMAC_SECRET", "error", err)
			os.Exit(1)
		}
	} else {
		logger.Warn("ARCHIVE_HMAC_SECRET not set — presigned archive URLs disabled")
	}

	// Configure OIDC job token authentication (optional).
	// OIDC_JWKS_URL: JWKS endpoint of the OIDC issuer.
	// OIDC_AUDIENCE: expected audience claim (default: "docstore").
	// OIDC_ISSUER: expected issuer claim (default: "https://oidc.docstore.dev").
	oidcJWKSURL := os.Getenv("OIDC_JWKS_URL")
	oidcAudience := cmp.Or(os.Getenv("OIDC_AUDIENCE"), "docstore")
	oidcIssuer := cmp.Or(os.Getenv("OIDC_ISSUER"), "https://oidc.docstore.dev")
	if oidcJWKSURL == "" {
		logger.Warn("OIDC_JWKS_URL not set — job OIDC token authentication disabled")
	}

	// Decode session secret for signing web UI session cookies.
	// SESSION_SECRET: base64-encoded raw HMAC secret bytes.
	var sessionSecret []byte
	if *sessionSecretB64 != "" {
		sessionSecret, err = base64.StdEncoding.DecodeString(*sessionSecretB64)
		if err != nil {
			logger.Error("invalid SESSION_SECRET", "error", err)
			os.Exit(1)
		}
	} else {
		logger.Warn("SESSION_SECRET not set — web UI session cookie auth disabled")
	}

	// Initialize log store for CI check log uploads.
	ls, err := logstore.NewFromEnv(context.Background())
	if err != nil {
		logger.Error("failed to create log store", "error", err)
		os.Exit(1)
	}

	jobStore := db.NewStore(database)
	srv := server.NewWithOIDC(commitStore, database, bs, broker,
		*devIdentity, *bootstrapAdmin, *oauthClientID, *oauthClientSecret,
		jobStore, archiveHMACSecret, archiveBaseURL,
		oidcJWKSURL, oidcAudience, oidcIssuer, ls, sessionSecret)

	// Start the auto-merge worker. It subscribes to check.reported and
	// review.submitted events and merges branches with auto_merge=true.
	readStore := store.New(database)
	autoMergeWorker := automerge.New(broker, commitStore, readStore, policy.NewCache())
	workerCtx, workerCancel := context.WithCancel(context.Background())
	go autoMergeWorker.Run(workerCtx)

	httpServer := &http.Server{
		Addr:         ":" + port,
		Handler:      srv,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start server in background.
	go func() {
		logger.Info("starting server", "port", port, "dev_identity", *devIdentity != "")
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	// Wait for interrupt signal, then gracefully shut down.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	logger.Info("shutting down")

	// Stop the outbox dispatcher and auto-merge worker.
	dispatchCancel()
	workerCancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown error", "error", err)
		os.Exit(1)
	}
	logger.Info("stopped")
}
