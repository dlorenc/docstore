package db

import _ "embed"

//go:embed migrations/000001_initial_schema.up.sql
var MigrationSQL string

//go:embed migrations/000001_initial_schema.down.sql
var MigrationDownSQL string
