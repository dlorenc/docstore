package db

import (
	"fmt"
	"os"
	"testing"

	"github.com/dlorenc/docstore/internal/testutil"
)

var sharedAdminDSN string

func TestMain(m *testing.M) {
	dsn, cleanup, err := testutil.StartSharedPostgres()
	if err != nil {
		fmt.Fprintf(os.Stderr, "start shared postgres: %v\n", err)
		os.Exit(1)
	}
	sharedAdminDSN = dsn
	code := m.Run()
	cleanup()
	os.Exit(code)
}
