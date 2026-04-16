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
var testDBSeq int64

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

	ctr, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("postgres"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(30*time.Second),
		),
	)
	if err != nil {
		return "", func() {}, fmt.Errorf("start postgres container: %w", err)
	}

	connStr, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = ctr.Terminate(ctx)
		return "", func() {}, fmt.Errorf("get connection string: %w", err)
	}

	return connStr, func() {
		if termErr := ctr.Terminate(ctx); termErr != nil {
			fmt.Fprintf(os.Stderr, "terminate postgres container: %v\n", termErr)
		}
	}, nil
}

// TestDBFromShared creates an isolated database from a shared admin DSN,
// runs migrations, and returns a connected *sql.DB. The database is dropped
// when the test finishes. Use this in combination with StartSharedPostgres.
func TestDBFromShared(t testing.TB, adminDSN string, migrate func(*sql.DB) error) *sql.DB {
	t.Helper()
	return testDBFromDSN(t, adminDSN, migrate)
}

// testDBFromDSN is the legacy path: use a pre-existing PostgreSQL server via DSN.
// It creates a unique database per test so parallel tests don't interfere.
func testDBFromDSN(t testing.TB, dsn string, migrate func(*sql.DB) error) *sql.DB {
	t.Helper()

	// Connect to the server using the provided DSN (admin connection).
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}
	defer admin.Close()

	// Create a unique database for this test. The atomic counter prevents
	// collisions when many parallel tests call this simultaneously.
	dbName := fmt.Sprintf("docstore_test_%d_%d", time.Now().UnixNano(), atomic.AddInt64(&testDBSeq, 1))
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
