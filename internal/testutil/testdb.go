package testutil

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/dlorenc/docstore/internal/db"
)

// TestDB creates a fresh PostgreSQL database for the current test and runs
// migrations. It returns a connected *sql.DB that is automatically cleaned up
// when the test finishes.
//
// The connection string is read from DOCSTORE_TEST_DSN. If the variable is
// not set the test is skipped. The DSN should point to a PostgreSQL server
// (the database name in the DSN is ignored; a unique temp database is created).
//
// Example:
//
//	export DOCSTORE_TEST_DSN="postgres://localhost:5432/postgres?sslmode=disable"
func TestDB(t *testing.T) *sql.DB {
	t.Helper()

	dsn := os.Getenv("DOCSTORE_TEST_DSN")
	if dsn == "" {
		t.Skip("DOCSTORE_TEST_DSN not set, skipping integration test")
	}

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DOCSTORE_TEST_DSN: %v", err)
	}

	// Connect to the default "postgres" database to create the temp database.
	adminURL := *u
	adminURL.Path = "/postgres"
	adminDB, err := sql.Open("postgres", adminURL.String())
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}

	dbName := fmt.Sprintf("docstore_test_%d_%d", os.Getpid(), time.Now().UnixNano())

	if _, err := adminDB.Exec("CREATE DATABASE " + dbName); err != nil {
		adminDB.Close()
		t.Fatalf("create test database: %v", err)
	}

	// Connect to the temp database.
	testURL := *u
	testURL.Path = "/" + dbName
	testDB, err := sql.Open("postgres", testURL.String())
	if err != nil {
		adminDB.Exec("DROP DATABASE IF EXISTS " + dbName)
		adminDB.Close()
		t.Fatalf("connect to test database: %v", err)
	}

	// Run schema migrations.
	if _, err := testDB.Exec(db.MigrationSQL); err != nil {
		testDB.Close()
		adminDB.Exec("DROP DATABASE IF EXISTS " + dbName)
		adminDB.Close()
		t.Fatalf("run migrations: %v", err)
	}

	t.Cleanup(func() {
		testDB.Close()
		adminDB.Exec("DROP DATABASE IF EXISTS " + dbName)
		adminDB.Close()
	})

	return testDB
}
