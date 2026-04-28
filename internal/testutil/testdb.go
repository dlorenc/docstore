package testutil

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// testDBSeq is a monotonically increasing counter used to generate unique
// database names within a single test binary, preventing collisions when
// tests run in parallel.
var testDBSeq atomic.Int64

// StartSharedPostgres starts a single PostgreSQL container for use across a
// test package. Call it from TestMain before m.Run() and store the returned
// adminDSN in a package-level variable. Pass adminDSN to TestDBFromShared for
// each individual test. Call cleanup after m.Run() to terminate the container.
//
// If DOCSTORE_TEST_DSN is set it is returned directly with a no-op cleanup.
func StartSharedPostgres() (adminDSN string, cleanup func(), err error) {
	if dsn := os.Getenv("DOCSTORE_TEST_DSN"); dsn != "" {
		return dsn, func() {}, nil
	}

	ctx := context.Background()

	// WithReuseByName makes all test packages share a single persistent
	// container. The first run starts it; subsequent runs reattach to the
	// existing one. Ryuk automatically excludes reusable containers from its
	// cleanup sweep, so the container stays up between test runs.
	ctr, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("postgres"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		// Raise the connection limit so many parallel tests don't exhaust it.
		testcontainers.WithCmdArgs("-c", "max_connections=500"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(30*time.Second),
		),
		testcontainers.WithReuseByName("docstore-test-postgres"),
	)
	if err != nil {
		return "", func() {}, fmt.Errorf("start postgres container: %w", err)
	}

	connStr, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return "", func() {}, fmt.Errorf("get connection string: %w", err)
	}

	// No-op cleanup: the container is persistent and shared across packages.
	// Ryuk excludes reusable containers from automatic cleanup.
	return connStr, func() {}, nil
}

// TestDBFromShared creates an isolated database from a shared admin DSN,
// runs migrations, and returns a connected *sql.DB. The database is dropped
// when the test finishes. Use this in combination with StartSharedPostgres.
func TestDBFromShared(t testing.TB, adminDSN string, migrate func(*sql.DB) error) *sql.DB {
	t.Helper()
	return testDBFromDSN(t, adminDSN, migrate)
}

// TestDBAndDSNFromShared is like TestDBFromShared but also returns the DSN
// for the isolated test database. Use when you need the DSN directly (e.g.
// for pq.Listener or other low-level PostgreSQL drivers).
func TestDBAndDSNFromShared(t testing.TB, adminDSN string, migrate func(*sql.DB) error) (*sql.DB, string) {
	t.Helper()
	return testDBAndDSN(t, adminDSN, migrate)
}

// testDBFromDSN is the legacy path: use a pre-existing PostgreSQL server via DSN.
// It creates a unique database per test so parallel tests don't interfere.
func testDBFromDSN(t testing.TB, dsn string, migrate func(*sql.DB) error) *sql.DB {
	t.Helper()
	d, _ := testDBAndDSN(t, dsn, migrate)
	return d
}

// testDBAndDSN creates an isolated test database and returns both the *sql.DB
// and the DSN string for that database.
func testDBAndDSN(t testing.TB, dsn string, migrate func(*sql.DB) error) (*sql.DB, string) {
	t.Helper()

	// Connect to the server using the provided DSN (admin connection).
	// Limit the admin pool to a handful of connections — it only needs one
	// at a time to run DDL (CREATE/DROP DATABASE).
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}
	admin.SetMaxOpenConns(3)
	admin.SetMaxIdleConns(1)
	defer admin.Close()

	// Create a unique database for this test. The atomic counter prevents
	// collisions when many parallel tests call this simultaneously.
	dbName := fmt.Sprintf("docstore_test_%d_%d", time.Now().UnixNano(), testDBSeq.Add(1))
	if _, err := admin.Exec("CREATE DATABASE " + dbName); err != nil {
		t.Fatalf("create test database %s: %v", dbName, err)
	}

	// Build a DSN for the new database by replacing the dbname parameter.
	testDSN := replaceDBName(dsn, dbName)
	d, err := sql.Open("postgres", testDSN)
	if err != nil {
		t.Fatalf("connect to test database %s: %v", dbName, err)
	}
	// Cap the per-test pool so many parallel tests don't exhaust Postgres
	// max_connections. 5 open connections is ample for any single test.
	d.SetMaxOpenConns(5)
	d.SetMaxIdleConns(2)
	d.SetConnMaxLifetime(30 * time.Second)

	// Single cleanup closes the pool first, then terminates any lingering
	// backend connections before dropping the database. Combining both steps
	// in one function ensures correct ordering without relying on LIFO
	// semantics and avoids the "database is being accessed by other users"
	// error that can occur when pg_backend connections outlive d.Close().
	t.Cleanup(func() {
		d.Close()
		c, err := sql.Open("postgres", dsn)
		if err != nil {
			t.Logf("reconnect to drop test db: %v", err)
			return
		}
		defer c.Close()
		c.SetMaxOpenConns(3)
		c.SetMaxIdleConns(1)
		// Terminate any remaining backend connections to the test database so
		// that DROP DATABASE succeeds even if d.Close() left stragglers.
		_, _ = c.Exec(
			"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()",
			dbName,
		)
		if _, err := c.Exec("DROP DATABASE IF EXISTS " + dbName); err != nil {
			t.Logf("drop test database %s: %v", dbName, err)
		}
	})

	if err := migrate(d); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	return d, testDSN
}

// replaceDBName replaces the database name in a PostgreSQL DSN. It handles
// both URL format (postgres://...) and key=value format.
func replaceDBName(dsn, dbName string) string {
	// Handle URL-format DSNs (postgres:// or postgresql://).
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err == nil {
			u.Path = "/" + dbName
			return u.String()
		}
	}
	// Handle key=value format DSNs.
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
