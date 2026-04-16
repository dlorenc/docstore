package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/dlorenc/docstore/internal/executor"
	"github.com/dlorenc/docstore/internal/testutil"
)

// pkgBuildkitAddr is the buildkitd address shared across all tests in this package.
var pkgBuildkitAddr string

func TestMain(m *testing.M) {
	addr, cleanup, err := testutil.StartBuildkit()
	if err != nil {
		fmt.Fprintf(os.Stderr, "skipping ci-runner integration tests: could not start buildkit: %v\n", err)
		os.Exit(0)
	}
	pkgBuildkitAddr = addr
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// newIntegrationServer starts a ci-runner HTTP server connected to buildkitd
// and returns its httptest.Server. Both are closed when the test finishes.
func newIntegrationServer(t *testing.T) *httptest.Server {
	t.Helper()
	exec, err := executor.New(pkgBuildkitAddr)
	if err != nil {
		t.Fatalf("cannot connect to buildkitd at %s: %v", pkgBuildkitAddr, err)
	}
	srv := httptest.NewServer(newMux(exec))
	t.Cleanup(func() {
		srv.Close()
		exec.Close()
	})
	return srv
}

// postJSON sends a POST /run with v marshalled as JSON and returns the response.
func postJSON(t *testing.T, srv *httptest.Server, v any) *http.Response {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	resp, err := http.Post(srv.URL+"/run", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST /run: %v", err)
	}
	return resp
}

// TestIntegration_ValidRun posts a valid single-check request and expects a 200
// response with the check marked "passed".
func TestIntegration_ValidRun(t *testing.T) {
	srv := newIntegrationServer(t)

	dir := t.TempDir()
	resp := postJSON(t, srv, runRequest{
		SourceDir: dir,
		Config: executor.Config{
			Checks: []executor.Check{
				{Name: "ci/hello", Image: "alpine", Steps: []string{"echo hello"}},
			},
		},
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var result runResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(result.Checks) != 1 {
		t.Fatalf("expected 1 check result, got %d", len(result.Checks))
	}
	if result.Checks[0].Status != "passed" {
		t.Errorf("expected passed, got %s (logs: %s)", result.Checks[0].Status, result.Checks[0].Logs)
	}
}

// TestIntegration_MissingSourceDir expects a 400 when source_dir is absent.
func TestIntegration_MissingSourceDir(t *testing.T) {
	srv := newIntegrationServer(t)

	resp := postJSON(t, srv, runRequest{
		Config: executor.Config{
			Checks: []executor.Check{
				{Name: "ci/hello", Image: "alpine", Steps: []string{"echo hello"}},
			},
		},
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

// TestIntegration_RelativeSourceDir expects a 400 when source_dir is not absolute.
func TestIntegration_RelativeSourceDir(t *testing.T) {
	srv := newIntegrationServer(t)

	resp := postJSON(t, srv, runRequest{
		SourceDir: "relative/path",
		Config: executor.Config{
			Checks: []executor.Check{
				{Name: "ci/hello", Image: "alpine", Steps: []string{"echo hello"}},
			},
		},
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

// TestIntegration_NonExistentSourceDir expects a 400 when source_dir does not exist.
func TestIntegration_NonExistentSourceDir(t *testing.T) {
	srv := newIntegrationServer(t)

	resp := postJSON(t, srv, runRequest{
		SourceDir: "/nonexistent/path/that/does/not/exist",
		Config: executor.Config{
			Checks: []executor.Check{
				{Name: "ci/hello", Image: "alpine", Steps: []string{"echo hello"}},
			},
		},
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

// TestIntegration_MissingImage expects a 400 when a check has no image.
func TestIntegration_MissingImage(t *testing.T) {
	srv := newIntegrationServer(t)
	dir := t.TempDir()

	resp := postJSON(t, srv, runRequest{
		SourceDir: dir,
		Config: executor.Config{
			Checks: []executor.Check{
				{Name: "ci/hello", Steps: []string{"echo hello"}},
			},
		},
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

// TestIntegration_MissingSteps expects a 400 when a check has no steps.
func TestIntegration_MissingSteps(t *testing.T) {
	srv := newIntegrationServer(t)
	dir := t.TempDir()

	resp := postJSON(t, srv, runRequest{
		SourceDir: dir,
		Config: executor.Config{
			Checks: []executor.Check{
				{Name: "ci/hello", Image: "alpine"},
			},
		},
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

// TestIntegration_TwoChecks posts a request with two checks (one pass, one fail)
// and verifies the correct per-check statuses in the 200 response.
func TestIntegration_TwoChecks(t *testing.T) {
	srv := newIntegrationServer(t)
	dir := t.TempDir()

	resp := postJSON(t, srv, runRequest{
		SourceDir: dir,
		Config: executor.Config{
			Checks: []executor.Check{
				{Name: "ci/pass", Image: "alpine", Steps: []string{"echo ok"}},
				{Name: "ci/fail", Image: "alpine", Steps: []string{"exit 1"}},
			},
		},
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var result runResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(result.Checks) != 2 {
		t.Fatalf("expected 2 check results, got %d", len(result.Checks))
	}
	byName := make(map[string]executor.CheckResult)
	for _, c := range result.Checks {
		byName[c.Name] = c
	}
	if byName["ci/pass"].Status != "passed" {
		t.Errorf("ci/pass: expected passed, got %s", byName["ci/pass"].Status)
	}
	if byName["ci/fail"].Status != "failed" {
		t.Errorf("ci/fail: expected failed, got %s", byName["ci/fail"].Status)
	}
}

// TestIntegration_InvalidJSON expects a 400 when the request body is not valid JSON.
func TestIntegration_InvalidJSON(t *testing.T) {
	srv := newIntegrationServer(t)

	resp, err := http.Post(srv.URL+"/run", "application/json", bytes.NewReader([]byte("not valid json {")))
	if err != nil {
		t.Fatalf("POST /run: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}
