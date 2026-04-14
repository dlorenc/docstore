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
// database, runs the given migration function, and returns a connected *sql.DB.
// The container is automatically terminated when the test finishes.
//
// If the DOCSTORE_TEST_DSN environment variable is set, it is used instead of
// starting a container (useful for local development with an existing server).
func TestDB(t *testing.T, migrate func(*sql.DB) error) *sql.DB {
	t.Helper()

	// Allow override for local dev.
	if dsn := os.Getenv("DOCSTORE_TEST_DSN"); dsn != "" {
		return testDBFromDSN(t, dsn, migrate)
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
	if err := migrate(testDB); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	return testDB
}

// testDBFromDSN is the legacy path: use a pre-existing PostgreSQL server via DSN.
// It creates a unique database per test so parallel tests don't interfere.
func testDBFromDSN(t *testing.T, dsn string, migrate func(*sql.DB) error) *sql.DB {
	t.Helper()

	// Connect to the server using the provided DSN (admin connection).
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}
	defer admin.Close()

	// Create a unique database for this test.
	dbName := fmt.Sprintf("docstore_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE " + dbName); err != nil {
		t.Fatalf("create test database %s: %v", dbName, err)
	}
	t.Cleanup(func() {
		// Reconnect as admin to drop the test database.
		c, err := sql.Open("postgres", dsn)
		if err != nil {
			t.Logf("reconnect to drop test db: %v", err)
			return
		}
		defer c.Close()
		if _, err := c.Exec("DROP DATABASE IF EXISTS " + dbName); err != nil {
			t.Logf("drop test database %s: %v", dbName, err)
		}
	})

	// Build a DSN for the new database by replacing the dbname parameter.
	testDSN := replaceDBName(dsn, dbName)
	d, err := sql.Open("postgres", testDSN)
	if err != nil {
		t.Fatalf("connect to test database %s: %v", dbName, err)
	}
	t.Cleanup(func() { d.Close() })

	if err := migrate(d); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	return d
}

// replaceDBName replaces the dbname in a PostgreSQL DSN (key=value format)
// or appends it if not present.
func replaceDBName(dsn, dbName string) string {
	// Handle key=value format DSNs.
	// Try to replace existing dbname parameter.
	parts := splitDSN(dsn)
	found := false
	for i, p := range parts {
		if len(p) > 7 && p[:7] == "dbname=" {
			parts[i] = "dbname=" + dbName
			found = true
			break
		}
	}
	if !found {
		parts = append(parts, "dbname="+dbName)
	}
	result := ""
	for i, p := range parts {
		if i > 0 {
			result += " "
		}
		result += p
	}
	return result
}

// splitDSN splits a key=value DSN into its space-separated parts.
func splitDSN(dsn string) []string {
	var parts []string
	current := ""
	inQuote := false
	for _, c := range dsn {
		switch {
		case c == '\'' && !inQuote:
			inQuote = true
			current += string(c)
		case c == '\'' && inQuote:
			inQuote = false
			current += string(c)
		case c == ' ' && !inQuote:
			if current != "" {
				parts = append(parts, current)
				current = ""
			}
		default:
			current += string(c)
		}
	}
	if current != "" {
		parts = append(parts, current)
	}
	return parts
}
