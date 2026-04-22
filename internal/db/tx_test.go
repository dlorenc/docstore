package db

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/dlorenc/docstore/internal/testutil"
)

func TestWithTx_Commit(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	ctx := context.Background()

	orgName := "withtx-commit-org"
	err := WithTx(ctx, d, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO orgs (name, created_by) VALUES ($1, 'test')", orgName)
		return err
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}

	// Verify the insert was committed.
	var exists bool
	if err := d.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM orgs WHERE name = $1)", orgName).Scan(&exists); err != nil {
		t.Fatalf("query: %v", err)
	}
	if !exists {
		t.Error("expected org to exist after successful commit")
	}
}

func TestWithTx_RollbackOnError(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	ctx := context.Background()

	orgName := "withtx-rollback-org"
	wantErr := errors.New("simulated failure")

	err := WithTx(ctx, d, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT INTO orgs (name, created_by) VALUES ($1, 'test')", orgName); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected wantErr, got %v", err)
	}

	// Verify the insert was rolled back.
	var exists bool
	if err := d.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM orgs WHERE name = $1)", orgName).Scan(&exists); err != nil {
		t.Fatalf("query: %v", err)
	}
	if exists {
		t.Error("expected org to not exist after rollback")
	}
}

func TestWithTx_RollbackOnPanic(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	ctx := context.Background()

	orgName := "withtx-panic-org"

	panicked := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
			}
		}()
		_ = WithTx(ctx, d, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, "INSERT INTO orgs (name, created_by) VALUES ($1, 'test')", orgName); err != nil {
				return err
			}
			panic("simulated panic")
		})
	}()
	if !panicked {
		t.Error("expected panic to propagate")
	}

	// Verify the insert was rolled back.
	var exists bool
	if err := d.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM orgs WHERE name = $1)", orgName).Scan(&exists); err != nil {
		t.Fatalf("query: %v", err)
	}
	if exists {
		t.Error("expected org to not exist after panic rollback")
	}
}

func TestWithTx_ErrRollback(t *testing.T) {
	t.Parallel()
	d := testutil.TestDBFromShared(t, sharedAdminDSN, RunMigrations)
	ctx := context.Background()

	orgName := "withtx-errrollback-org"

	// errRollback should cause rollback but return nil to caller.
	err := WithTx(ctx, d, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT INTO orgs (name, created_by) VALUES ($1, 'test')", orgName); err != nil {
			return err
		}
		return errRollback
	})
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}

	// Verify the insert was rolled back.
	var exists bool
	if err := d.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM orgs WHERE name = $1)", orgName).Scan(&exists); err != nil {
		t.Fatalf("query: %v", err)
	}
	if exists {
		t.Error("expected org to not exist after errRollback rollback")
	}
}
