package testutil

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestDB starts a PostgreSQL container via testcontainers-go, creates a fresh
// database, runs the given schema migration, and returns a connected *sql.DB.
// The container is automatically terminated when the test finishes.
//
// If the DOCSTORE_TEST_DSN environment variable is set, it is used instead of
// starting a container (useful for local development with an existing server).
func TestDB(t *testing.T, migrationSQL string) *sql.DB {
	t.Helper()

	// Allow override for local dev.
	if dsn := os.Getenv("DOCSTORE_TEST_DSN"); dsn != "" {
		return testDBFromDSN(t, dsn, migrationSQL)
	}

	ctx := context.Background()

	dbName := fmt.Sprintf("docstore_test_%d", time.Now().UnixNano())

	ctr, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase(dbName),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(30*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}

	t.Cleanup(func() {
		if err := ctr.Terminate(ctx); err != nil {
			t.Logf("terminate postgres container: %v", err)
		}
	})

	connStr, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("get connection string: %v", err)
	}

	testDB, err := sql.Open("postgres", connStr)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(func() { testDB.Close() })

	// Run schema migrations.
	if _, err := testDB.Exec(migrationSQL); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	return testDB
}

// testDBFromDSN is the legacy path: use a pre-existing PostgreSQL server via DSN.
func testDBFromDSN(t *testing.T, dsn, migrationSQL string) *sql.DB {
	t.Helper()

	d, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}

	// Clean and recreate schema so each test starts fresh.
	for _, stmt := range []string{
		"DROP TABLE IF EXISTS check_runs CASCADE",
		"DROP TABLE IF EXISTS reviews CASCADE",
		"DROP TABLE IF EXISTS roles CASCADE",
		"DROP TABLE IF EXISTS file_commits CASCADE",
		"DROP TABLE IF EXISTS documents CASCADE",
		"DROP TABLE IF EXISTS branches CASCADE",
		"DROP TYPE IF EXISTS branch_status CASCADE",
		"DROP TYPE IF EXISTS role_type CASCADE",
		"DROP TYPE IF EXISTS review_status CASCADE",
		"DROP TYPE IF EXISTS check_status CASCADE",
	} {
		if _, err := d.Exec(stmt); err != nil {
			t.Fatalf("cleanup %q: %v", stmt, err)
		}
	}

	if _, err := d.Exec(migrationSQL); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	t.Cleanup(func() { d.Close() })
	return d
}
