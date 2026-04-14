package db

import _ "embed"

//go:embed migrations/000001_initial_schema.up.sql
var migration001 string

//go:embed migrations/000002_add_commits_table.up.sql
var migration002 string

// MigrationSQL is the combined SQL for all schema migrations, run in order.
var MigrationSQL = migration001 + "\n" + migration002

//go:embed migrations/000001_initial_schema.down.sql
var MigrationDownSQL string
