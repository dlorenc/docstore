// Command ci-registry is an in-cluster OCI registry for BuildKit cache,
// backed by GCS blob storage and authenticated via CI job OIDC tokens.
package main

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"cloud.google.com/go/storage"

	"github.com/dlorenc/docstore/internal/registry"
)

func main() {
	gcsBucket := mustEnv("GCS_BUCKET")
	jwksURL := mustEnv("JWKS_URL")
	audience := cmp.Or(os.Getenv("OIDC_AUDIENCE"), "docstore")
	issuer := cmp.Or(os.Getenv("OIDC_ISSUER"), "https://oidc.docstore.dev")
	port := cmp.Or(os.Getenv("PORT"), "8080")

	// Set up structured logging.
	var logLevel slog.LevelVar
	if lvlStr := os.Getenv("LOG_LEVEL"); lvlStr != "" {
		if err := logLevel.UnmarshalText([]byte(lvlStr)); err != nil {
			logLevel.Set(slog.LevelInfo)
		}
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: &logLevel})))

	ctx := context.Background()

	// Set up GCS client and blob handler.
	gcsClient, err := storage.NewClient(ctx)
	if err != nil {
		slog.Error("create gcs client", "error", err)
		os.Exit(1)
	}
	defer gcsClient.Close()

	bucket := gcsClient.Bucket(gcsBucket)
	blobHandler := registry.NewGCSHandler(bucket)
	manifestStore := registry.NewGCSManifestStore(bucket)
	registryHandler := registry.New(blobHandler, manifestStore, jwksURL, audience, issuer)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("/", registryHandler)

	addr := ":" + port
	slog.Info("ci-registry starting",
		"addr", addr,
		"gcs_bucket", gcsBucket,
		"jwks_url", jwksURL,
		"audience", audience,
		"issuer", issuer,
	)

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("listen", "error", err)
		os.Exit(1)
	}
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fmt.Fprintf(os.Stderr, "error: %s is required\n", key)
		os.Exit(1)
	}
	return v
}
