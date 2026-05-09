package db

import (
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"time"
)

// Open opens a PostgreSQL connection using the provided DSN.
// Connection pool settings can be overridden via environment variables:
//
//	DB_MAX_OPEN_CONNS   (default: 25)
//	DB_MAX_IDLE_CONNS   (default: 5)
//	DB_CONN_MAX_LIFETIME (default: 5m, parsed by time.ParseDuration)
func Open(dsn string) (*sql.DB, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("db open: %w", err)
	}

	db.SetMaxOpenConns(envInt("DB_MAX_OPEN_CONNS", 25))
	db.SetMaxIdleConns(envInt("DB_MAX_IDLE_CONNS", 5))
	db.SetConnMaxLifetime(envDuration("DB_CONN_MAX_LIFETIME", 5*time.Minute))

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("db ping: %w", err)
	}
	slog.Info("database connected", "dsn_host", extractHost(dsn))
	return db, nil
}

// envInt reads an integer from the named environment variable, returning
// defaultVal if the variable is unset or unparseable.
func envInt(name string, defaultVal int) int {
	s := os.Getenv(name)
	if s == "" {
		return defaultVal
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		slog.Warn("invalid env var, using default", "var", name, "value", s, "default", defaultVal)
		return defaultVal
	}
	return v
}

// envDuration reads a duration from the named environment variable, returning
// defaultVal if the variable is unset or unparseable.
func envDuration(name string, defaultVal time.Duration) time.Duration {
	s := os.Getenv(name)
	if s == "" {
		return defaultVal
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		slog.Warn("invalid env var, using default", "var", name, "value", s, "default", defaultVal)
		return defaultVal
	}
	return d
}

// extractHost returns just the hostname from a DSN, avoiding logging credentials.
func extractHost(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.Host == "" {
		return "unknown"
	}
	return u.Hostname()
}
