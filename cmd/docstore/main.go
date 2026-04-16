package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	_ "github.com/lib/pq"

	"github.com/dlorenc/docstore/internal/blob"
	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/server"
)

func main() {
	devIdentity := flag.String("dev-identity", "", "bypass IAP JWT validation and use this identity (local dev/testing only)")
	bootstrapAdmin := flag.String("bootstrap-admin", "", "identity granted admin access to repos with no admin assigned yet")
	flag.Parse()

	// Set up structured logger based on LOG_FORMAT env var.
	// Default is JSON (GCP Cloud Run picks this up natively).
	var handler slog.Handler
	if os.Getenv("LOG_FORMAT") == "text" {
		handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	} else {
		handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	}
	logger := slog.New(handler)
	slog.SetDefault(logger)

	// Also accept DEV_IDENTITY and BOOTSTRAP_ADMIN env vars for container-based testing.
	if *devIdentity == "" {
		*devIdentity = os.Getenv("DEV_IDENTITY")
	}
	if *bootstrapAdmin == "" {
		*bootstrapAdmin = os.Getenv("BOOTSTRAP_ADMIN")
	}

	if *devIdentity != "" {
		logger.Warn("IAP JWT validation disabled", "dev_identity", *devIdentity)
	}
	if *bootstrapAdmin != "" {
		logger.Info("bootstrap admin configured", "identity", *bootstrapAdmin)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

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
		localDir := os.Getenv("DOCSTORE_BLOB_LOCAL_DIR")
		if localDir == "" {
			localDir = "/tmp/docstore-blobs"
		}
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

	srv := server.NewWithBlobStore(commitStore, database, bs, *devIdentity, *bootstrapAdmin)

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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		logger.Error("shutdown error", "error", err)
		os.Exit(1)
	}
	logger.Info("stopped")
}
