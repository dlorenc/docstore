package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dlorenc/docstore/internal/executor"
	"github.com/dlorenc/docstore/internal/logstore"
	"github.com/dlorenc/docstore/internal/model"
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

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// mockDocstore is an httptest.Server that simulates the docstore API for integration tests.
// It serves a ci.yaml config, a minimal source tree, and records posted check runs.
type mockDocstore struct {
	srv        *httptest.Server
	mu         sync.Mutex
	checkRuns  []model.CreateCheckRunRequest
	sourceDir  string // temp dir with source files to serve
	ciYAML     []byte
}

func newMockDocstore(t *testing.T, ciYAML []byte, sourceDir string) *mockDocstore {
	t.Helper()
	md := &mockDocstore{
		ciYAML:    ciYAML,
		sourceDir: sourceDir,
	}
	md.srv = httptest.NewServer(http.HandlerFunc(md.handle))
	t.Cleanup(md.srv.Close)
	return md
}

func (md *mockDocstore) handle(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	switch {
	case strings.HasSuffix(path, "/-/file/.docstore/ci.yaml") && r.URL.Query().Get("branch") == "main":
		resp := model.FileResponse{
			Path:    ".docstore/ci.yaml",
			Content: md.ciYAML,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck

	case strings.HasSuffix(path, "/-/archive"):
		// Build and serve a tar archive of all files in sourceDir.
		tarData, err := buildDirTar(md.sourceDir)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-tar")
		w.Write(tarData) //nolint:errcheck

	case strings.HasSuffix(path, "/-/check"):
		var req model.CreateCheckRunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		md.mu.Lock()
		md.checkRuns = append(md.checkRuns, req)
		md.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(model.CreateCheckRunResponse{ID: "cr-1", Sequence: 1}) //nolint:errcheck

	default:
		http.NotFound(w, r)
	}
}

// buildDirTar creates an in-memory tar archive from all files under dir.
func buildDirTar(dir string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	err := walkDirTar(tw, dir, dir)
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func walkDirTar(tw *tar.Writer, root, current string) error {
	infos, err := os.ReadDir(current)
	if err != nil {
		return err
	}
	for _, info := range infos {
		full := current + "/" + info.Name()
		if info.IsDir() {
			if err := walkDirTar(tw, root, full); err != nil {
				return err
			}
		} else {
			rel := strings.TrimPrefix(full, root+"/")
			content, err := os.ReadFile(full)
			if err != nil {
				return err
			}
			hdr := &tar.Header{
				Name:     rel,
				Size:     int64(len(content)),
				Mode:     0644,
				Typeflag: tar.TypeReg,
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if _, err := tw.Write(content); err != nil {
				return err
			}
		}
	}
	return nil
}

func (md *mockDocstore) waitForCheckName(t *testing.T, checkName string, timeout time.Duration) *model.CreateCheckRunRequest {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		md.mu.Lock()
		for _, cr := range md.checkRuns {
			if cr.CheckName == checkName && cr.Status != model.CheckRunPending {
				cr := cr // copy
				md.mu.Unlock()
				return &cr
			}
		}
		md.mu.Unlock()
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("timed out waiting for non-pending check run %q", checkName)
	return nil
}

// newIntegrationServer starts a ci-runner HTTP server connected to buildkitd
// and the given mock docstore server. The test server and executor are closed
// when the test ends.
func newIntegrationServer(t *testing.T, docstoreURL string) *httptest.Server {
	t.Helper()
	exec, err := executor.New(pkgBuildkitAddr)
	if err != nil {
		t.Fatalf("cannot connect to buildkitd at %s: %v", pkgBuildkitAddr, err)
	}
	ls, _ := logstore.NewLocalLogStore(t.TempDir())
	srv := httptest.NewServer(newMux(context.Background(), exec, ls, docstoreURL, &http.Client{}, 30*time.Minute, ""))
	t.Cleanup(func() {
		srv.Close()
		exec.Close() //nolint:errcheck
	})
	return srv
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestIntegration_PostRun_ReturnsRunID verifies that POST /run returns a
// run_id immediately and the goroutine runs checks asynchronously.
func TestIntegration_PostRun_ReturnsRunID(t *testing.T) {
	ciYAML := []byte(`checks:
  - name: ci/hello
    image: alpine
    steps:
      - echo hello
`)
	srcDir := t.TempDir()
	// Write a dummy file so the tree is non-empty.
	os.WriteFile(srcDir+"/README.md", []byte("hello"), 0o644)

	md := newMockDocstore(t, ciYAML, srcDir)
	srv := newIntegrationServer(t, md.srv.URL)

	body := `{"repo":"default/myrepo","branch":"feature/x","head_sequence":1}`
	resp, err := http.Post(srv.URL+"/run", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /run: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var result runResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.RunID == "" {
		t.Error("expected non-empty run_id")
	}
}

// TestIntegration_FullFlow runs a single passing check and verifies that the
// mock docstore receives both the pending and the final passed check run.
func TestIntegration_FullFlow(t *testing.T) {
	ciYAML := []byte(`checks:
  - name: ci/hello
    image: alpine
    steps:
      - echo hello
`)
	srcDir := t.TempDir()
	os.WriteFile(srcDir+"/README.md", []byte("hello"), 0o644)

	md := newMockDocstore(t, ciYAML, srcDir)
	srv := newIntegrationServer(t, md.srv.URL)

	body := `{"repo":"default/myrepo","branch":"feature/x","head_sequence":1}`
	resp, err := http.Post(srv.URL+"/run", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /run: %v", err)
	}
	resp.Body.Close()

	// Wait for the async check run to complete (up to 60s for image pull).
	cr := md.waitForCheckName(t, "ci/hello", 60*time.Second)
	if cr == nil {
		return // already failed in waitForCheckName
	}
	if cr.Status != model.CheckRunPassed {
		t.Errorf("expected passed, got %q", cr.Status)
	}
}

// TestIntegration_FailingCheck verifies a failing check is reported as failed.
func TestIntegration_FailingCheck(t *testing.T) {
	ciYAML := []byte(`checks:
  - name: ci/fail
    image: alpine
    steps:
      - exit 1
`)
	srcDir := t.TempDir()
	os.WriteFile(srcDir+"/README.md", []byte("hello"), 0o644)

	md := newMockDocstore(t, ciYAML, srcDir)
	srv := newIntegrationServer(t, md.srv.URL)

	body := `{"repo":"default/myrepo","branch":"feature/x","head_sequence":2}`
	resp, err := http.Post(srv.URL+"/run", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /run: %v", err)
	}
	resp.Body.Close()

	cr := md.waitForCheckName(t, "ci/fail", 60*time.Second)
	if cr == nil {
		return
	}
	if cr.Status != model.CheckRunFailed {
		t.Errorf("expected failed, got %q", cr.Status)
	}
}

// TestIntegration_InvalidJSON expects a 400 when the request body is not valid JSON.
func TestIntegration_InvalidJSON(t *testing.T) {
	md := newMockDocstore(t, nil, t.TempDir())
	srv := newIntegrationServer(t, md.srv.URL)

	resp, err := http.Post(srv.URL+"/run", "application/json", strings.NewReader("not valid json {"))
	if err != nil {
		t.Fatalf("POST /run: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

// TestIntegration_MissingRepo expects a 400 when repo is absent.
func TestIntegration_MissingRepo(t *testing.T) {
	md := newMockDocstore(t, nil, t.TempDir())
	srv := newIntegrationServer(t, md.srv.URL)

	resp, err := http.Post(srv.URL+"/run", "application/json", strings.NewReader(`{"branch":"feature/x"}`))
	if err != nil {
		t.Fatalf("POST /run: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

// TestIntegration_MissingBranch expects a 400 when branch is absent.
func TestIntegration_MissingBranch(t *testing.T) {
	md := newMockDocstore(t, nil, t.TempDir())
	srv := newIntegrationServer(t, md.srv.URL)

	resp, err := http.Post(srv.URL+"/run", "application/json", strings.NewReader(`{"repo":"default/myrepo"}`))
	if err != nil {
		t.Fatalf("POST /run: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

// Ensure context is used (suppress unused import).
var _ = context.Background
