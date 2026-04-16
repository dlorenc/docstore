package executor_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/dlorenc/docstore/internal/executor"
	"github.com/dlorenc/docstore/internal/testutil"
)

// pkgBuildkitAddr is the buildkitd address shared across all tests in this package.
var pkgBuildkitAddr string

func TestMain(m *testing.M) {
	addr, cleanup, err := testutil.StartBuildkit()
	if err != nil {
		fmt.Fprintf(os.Stderr, "skipping executor tests: could not start buildkit: %v\n", err)
		os.Exit(0)
	}
	pkgBuildkitAddr = addr
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// newExecutor creates an Executor for tests using the package-level buildkitd address.
func newExecutor(t *testing.T) *executor.Executor {
	t.Helper()
	exec, err := executor.New(pkgBuildkitAddr)
	if err != nil {
		t.Fatalf("cannot connect to buildkitd at %s: %v", pkgBuildkitAddr, err)
	}
	return exec
}

// TestPass verifies that a check whose steps all succeed is marked "passed"
// and that output from the steps appears in Logs.
func TestPass(t *testing.T) {
	exec := newExecutor(t)
	dir := t.TempDir()

	cfg := executor.Config{
		Checks: []executor.Check{
			{Name: "ci/hello", Image: "alpine", Steps: []string{"echo hello"}},
		},
	}

	results, err := exec.Run(context.Background(), dir, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if r.Status != "passed" {
		t.Errorf("expected status passed, got %s (logs: %s)", r.Status, r.Logs)
	}
	if !strings.Contains(r.Logs, "hello") {
		t.Errorf("expected logs to contain 'hello', got: %s", r.Logs)
	}
}

// TestFail verifies that a check whose step exits non-zero is marked "failed".
func TestFail(t *testing.T) {
	exec := newExecutor(t)
	dir := t.TempDir()

	cfg := executor.Config{
		Checks: []executor.Check{
			{Name: "ci/fail", Image: "alpine", Steps: []string{"exit 1"}},
		},
	}

	results, err := exec.Run(context.Background(), dir, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Status != "failed" {
		t.Errorf("expected status failed, got %s", results[0].Status)
	}
}

// TestMultiCheck verifies that two checks run in parallel, each getting the
// correct independent result.
func TestMultiCheck(t *testing.T) {
	exec := newExecutor(t)
	dir := t.TempDir()

	cfg := executor.Config{
		Checks: []executor.Check{
			{Name: "ci/pass", Image: "alpine", Steps: []string{"echo ok"}},
			{Name: "ci/fail", Image: "alpine", Steps: []string{"exit 1"}},
		},
	}

	results, err := exec.Run(context.Background(), dir, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	byName := make(map[string]executor.CheckResult)
	for _, r := range results {
		byName[r.Name] = r
	}

	if byName["ci/pass"].Status != "passed" {
		t.Errorf("ci/pass: expected passed, got %s", byName["ci/pass"].Status)
	}
	if byName["ci/fail"].Status != "failed" {
		t.Errorf("ci/fail: expected failed, got %s", byName["ci/fail"].Status)
	}
}

// TestLogCapture verifies that stdout and stderr from steps both appear in
// the Logs field.
func TestLogCapture(t *testing.T) {
	exec := newExecutor(t)
	dir := t.TempDir()

	cfg := executor.Config{
		Checks: []executor.Check{
			{
				Name:  "ci/logs",
				Image: "alpine",
				Steps: []string{
					"echo stdout-line",
					"echo stderr-line >&2",
				},
			},
		},
	}

	results, err := exec.Run(context.Background(), dir, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if r.Status != "passed" {
		t.Errorf("expected passed, got %s (logs: %s)", r.Status, r.Logs)
	}
	if !strings.Contains(r.Logs, "stdout-line") {
		t.Errorf("expected stdout in logs, got: %s", r.Logs)
	}
	if !strings.Contains(r.Logs, "stderr-line") {
		t.Errorf("expected stderr in logs, got: %s", r.Logs)
	}
}
