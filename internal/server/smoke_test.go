package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	tcnetwork "github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestDockerSmoke builds the Docker image from the repo root, starts it
// alongside a Postgres testcontainer on a shared network, and verifies that
// GET /tree?branch=main returns 200 with an empty array.
func TestDockerSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Docker smoke test in short mode")
	}

	ctx := context.Background()

	// Compute repo root: two levels up from internal/server/smoke_test.go
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine source file path")
	}
	repoRoot := filepath.Join(filepath.Dir(filename), "..", "..")

	// Create a dedicated Docker network so the app container can reach Postgres
	// by a stable hostname rather than a dynamic IP.
	net, err := tcnetwork.New(ctx)
	if err != nil {
		t.Fatalf("create network: %v", err)
	}
	t.Cleanup(func() {
		if err := net.Remove(ctx); err != nil {
			t.Logf("remove network: %v", err)
		}
	})
	networkName := net.Name

	const pgAlias = "postgres"

	// Start Postgres on the shared network.
	pgCtr, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("docstore"),
		postgres.WithUsername("docstore"),
		postgres.WithPassword("docstore"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(30*time.Second),
		),
		testcontainers.CustomizeRequest(testcontainers.GenericContainerRequest{
			ContainerRequest: testcontainers.ContainerRequest{
				Networks: []string{networkName},
				NetworkAliases: map[string][]string{
					networkName: {pgAlias},
				},
			},
		}),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() {
		if err := pgCtr.Terminate(ctx); err != nil {
			t.Logf("terminate postgres: %v", err)
		}
	})

	// Build and start the application container from the repo root.
	// DATABASE_URL uses the Postgres container's network alias as the host.
	dbURL := fmt.Sprintf("postgres://docstore:docstore@%s:5432/docstore?sslmode=disable", pgAlias)

	appCtr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			FromDockerfile: testcontainers.FromDockerfile{
				Context:    repoRoot,
				Dockerfile: "Dockerfile",
				KeepImage:  false,
			},
			Networks: []string{networkName},
			Env: map[string]string{
				"DATABASE_URL": dbURL,
				"PORT":         "8080",
			},
			ExposedPorts: []string{"8080/tcp"},
			WaitingFor: wait.ForHTTP("/healthz").
				WithPort("8080/tcp").
				WithStartupTimeout(120 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start app container: %v", err)
	}
	t.Cleanup(func() {
		if err := appCtr.Terminate(ctx); err != nil {
			t.Logf("terminate app container: %v", err)
		}
	})

	// Resolve the host-side address for the app container.
	host, err := appCtr.Host(ctx)
	if err != nil {
		t.Fatalf("get host: %v", err)
	}
	port, err := appCtr.MappedPort(ctx, "8080/tcp")
	if err != nil {
		t.Fatalf("get port: %v", err)
	}

	baseURL := fmt.Sprintf("http://%s:%s", host, port.Port())

	// GET /tree?branch=main must return 200 with an empty JSON array on a fresh DB.
	resp, err := http.Get(baseURL + "/tree?branch=main")
	if err != nil {
		t.Fatalf("GET /tree: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var entries []json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected empty array, got %d entries", len(entries))
	}
}
