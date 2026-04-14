package db

import (
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
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
	slog.Info("database connected", "dsn_host", extractHost(dsn))
	return db, nil
}

// extractHost returns just the hostname from a DSN, avoiding logging credentials.
func extractHost(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.Host == "" {
		return "unknown"
	}
	return u.Hostname()
}
