package db

import (
	"database/sql"
	"fmt"
)

// Open opens a PostgreSQL connection using the provided DSN.
func Open(dsn string) (*sql.DB, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("db open: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("db ping: %w", err)
	}
	return db, nil
}
