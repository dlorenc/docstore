package db

import (
	"database/sql"
	"fmt"
	_ "embed"
)

//go:embed migrations/000001_initial_schema.up.sql
var migration001 string

//go:embed migrations/000002_add_commits_table.up.sql
var migration002 string

//go:embed migrations/000003_multitenancy.up.sql
var migration003 string

// MigrationSQL is the combined SQL for all schema migrations, run in order.
var MigrationSQL = migration001 + "\n" + migration002 + "\n" + migration003

//go:embed migrations/000001_initial_schema.down.sql
var MigrationDownSQL string

// RunMigrations executes all migrations against the given database.
func RunMigrations(database *sql.DB) error {
	if _, err := database.Exec(MigrationSQL); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}
	return nil
}
