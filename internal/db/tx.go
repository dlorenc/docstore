package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
)

// errRollback is a sentinel returned from a WithTx closure to signal that the
// transaction should be rolled back without propagating an error to the caller.
// It is used by dry-run paths that compute results without persisting them.
var errRollback = errors.New("rollback")

// WithTx begins a transaction on db, calls fn with the transaction, and then
// commits or rolls back based on fn's return value:
//   - fn returns nil: commit.
//   - fn returns errRollback: rollback, return nil.
//   - fn returns any other error: rollback, return that error.
//   - fn panics: rollback, re-panic.
func WithTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) (retErr error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
				slog.Error("tx rollback failed during panic recovery", "error", rollbackErr)
			}
			panic(p)
		}
		if retErr != nil {
			if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
				slog.Error("tx rollback failed", "error", rollbackErr)
			}
			if errors.Is(retErr, errRollback) {
				retErr = nil
			}
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
