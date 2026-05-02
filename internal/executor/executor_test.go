package executor_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dlorenc/docstore/internal/ciconfig"
	"github.com/dlorenc/docstore/internal/citoken"
	"github.com/dlorenc/docstore/internal/executor"
	"github.com/dlorenc/docstore/internal/registry"
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

	cfg := executor.Config{
		Checks: []executor.Check{
			{Name: "ci/hello", Image: "alpine", Steps: []string{"echo hello"}},
		},
	}

	results, err := exec.Run(context.Background(), "", cfg, ciconfig.TriggerContext{})
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

	cfg := executor.Config{
		Checks: []executor.Check{
			{Name: "ci/fail", Image: "alpine", Steps: []string{"exit 1"}},
		},
	}

	results, err := exec.Run(context.Background(), "", cfg, ciconfig.TriggerContext{})
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

	cfg := executor.Config{
		Checks: []executor.Check{
			{Name: "ci/pass", Image: "alpine", Steps: []string{"echo ok"}},
			{Name: "ci/fail", Image: "alpine", Steps: []string{"exit 1"}},
		},
	}

	results, err := exec.Run(context.Background(), "", cfg, ciconfig.TriggerContext{})
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


// TestIfConditionSkipsCheck verifies that a check whose if: expression evaluates
// to false against the TriggerContext is omitted from results entirely.
func TestIfConditionSkipsCheck(t *testing.T) {
	exec := newExecutor(t)
	dir := t.TempDir()

	cfg := executor.Config{
		Checks: []executor.Check{
			{
				Name:  "ci/push-only",
				Image: "alpine",
				Steps: []string{"echo should-not-run"},
				If:    `event.type == "push"`,
			},
		},
	}

	// Trigger type is "proposal", so the push-only check must be skipped.
	results, err := exec.Run(context.Background(), dir, cfg, ciconfig.TriggerContext{Type: "proposal"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results (check skipped), got %d: %+v", len(results), results)
	}
}

// TestIfConditionRunsCheck verifies that a check whose if: expression evaluates
// to true against the TriggerContext is included in results.
func TestIfConditionRunsCheck(t *testing.T) {
	exec := newExecutor(t)
	dir := t.TempDir()

	cfg := executor.Config{
		Checks: []executor.Check{
			{
				Name:  "ci/push-only",
				Image: "alpine",
				Steps: []string{"echo ran"},
				If:    `event.type == "push"`,
			},
		},
	}

	results, err := exec.Run(context.Background(), dir, cfg, ciconfig.TriggerContext{Type: "push"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Status != "passed" {
		t.Errorf("expected passed, got %s (logs: %s)", results[0].Status, results[0].Logs)
	}
}

// TestIfConditionMixedChecks verifies that when multiple checks have different
// if: conditions, only the ones matching the TriggerContext run.
func TestIfConditionMixedChecks(t *testing.T) {
	exec := newExecutor(t)
	dir := t.TempDir()

	cfg := executor.Config{
		Checks: []executor.Check{
			{
				Name:  "ci/push-check",
				Image: "alpine",
				Steps: []string{"echo push-ran"},
				If:    `event.type == "push"`,
			},
			{
				Name:  "ci/proposal-check",
				Image: "alpine",
				Steps: []string{"echo proposal-ran"},
				If:    `event.type == "proposal"`,
			},
			{
				Name:  "ci/always",
				Image: "alpine",
				Steps: []string{"echo always-ran"},
				// No if: condition — always runs.
			},
		},
	}

	results, err := exec.Run(context.Background(), dir, cfg, ciconfig.TriggerContext{Type: "push"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Only ci/push-check and ci/always should run; ci/proposal-check is skipped.
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d: %+v", len(results), results)
	}
	byName := make(map[string]executor.CheckResult)
	for _, r := range results {
		byName[r.Name] = r
	}
	if _, ok := byName["ci/push-check"]; !ok {
		t.Errorf("ci/push-check should have run but is absent from results")
	}
	if _, ok := byName["ci/always"]; !ok {
		t.Errorf("ci/always should have run but is absent from results")
	}
	if _, ok := byName["ci/proposal-check"]; ok {
		t.Errorf("ci/proposal-check should have been skipped but appears in results")
	}
}

// TestIfConditionBranchFilter verifies that event.branch filtering works.
func TestIfConditionBranchFilter(t *testing.T) {
	exec := newExecutor(t)
	dir := t.TempDir()

	cfg := executor.Config{
		Checks: []executor.Check{
			{
				Name:  "ci/main-only",
				Image: "alpine",
				Steps: []string{"echo main-ran"},
				If:    `event.branch == "main"`,
			},
		},
	}

	// feature branch — should be skipped.
	results, err := exec.Run(context.Background(), dir, cfg, ciconfig.TriggerContext{Type: "push", Branch: "feature"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results for feature branch, got %d", len(results))
	}

	// main branch — should run.
	results, err = exec.Run(context.Background(), dir, cfg, ciconfig.TriggerContext{Type: "push", Branch: "main"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for main branch, got %d", len(results))
	}
	if results[0].Status != "passed" {
		t.Errorf("expected passed, got %s", results[0].Status)
	}
}

// TestIfConditionAndExpression verifies that compound && expressions are
// evaluated correctly — both sub-conditions must hold for the check to run.
func TestIfConditionAndExpression(t *testing.T) {
	exec := newExecutor(t)
	dir := t.TempDir()

	cfg := executor.Config{
		Checks: []executor.Check{
			{
				Name:  "ci/push-main",
				Image: "alpine",
				Steps: []string{"echo push-main-ran"},
				If:    `event.type == "push" && event.branch == "main"`,
			},
		},
	}

	// push to main — should run.
	results, err := exec.Run(context.Background(), dir, cfg, ciconfig.TriggerContext{Type: "push", Branch: "main"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for push+main, got %d", len(results))
	}

	// push to feature — should be skipped (branch condition false).
	results, err = exec.Run(context.Background(), dir, cfg, ciconfig.TriggerContext{Type: "push", Branch: "feature"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results for push+feature, got %d", len(results))
	}

	// proposal to main — should be skipped (type condition false).
	results, err = exec.Run(context.Background(), dir, cfg, ciconfig.TriggerContext{Type: "proposal", Branch: "main"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results for proposal+main, got %d", len(results))
	}
}

// TestIfConditionOrExpression verifies that || expressions run the check when
// either sub-condition is true.
func TestIfConditionOrExpression(t *testing.T) {
	exec := newExecutor(t)
	dir := t.TempDir()

	cfg := executor.Config{
		Checks: []executor.Check{
			{
				Name:  "ci/push-or-proposal",
				Image: "alpine",
				Steps: []string{"echo ran"},
				If:    `event.type == "push" || event.type == "proposal"`,
			},
		},
	}

	for _, triggerType := range []string{"push", "proposal"} {
		results, err := exec.Run(context.Background(), dir, cfg, ciconfig.TriggerContext{Type: triggerType})
		if err != nil {
			t.Fatalf("Run(%s): %v", triggerType, err)
		}
		if len(results) != 1 {
			t.Errorf("type=%s: expected 1 result, got %d", triggerType, len(results))
		}
	}

	// schedule — neither condition matches, should be skipped.
	results, err := exec.Run(context.Background(), dir, cfg, ciconfig.TriggerContext{Type: "schedule"})
	if err != nil {
		t.Fatalf("Run(schedule): %v", err)
	}
	if len(results) != 0 {
		t.Errorf("type=schedule: expected 0 results, got %d", len(results))
	}
}

// TestIfConditionAllSkipped verifies that Run returns an empty (non-nil) slice
// when every check's if: condition is false.
func TestIfConditionAllSkipped(t *testing.T) {
	exec := newExecutor(t)
	dir := t.TempDir()

	cfg := executor.Config{
		Checks: []executor.Check{
			{Name: "ci/a", Image: "alpine", Steps: []string{"echo a"}, If: `event.type == "push"`},
			{Name: "ci/b", Image: "alpine", Steps: []string{"echo b"}, If: `event.type == "proposal"`},
		},
	}

	results, err := exec.Run(context.Background(), dir, cfg, ciconfig.TriggerContext{Type: "schedule"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results when all checks skipped, got %d: %+v", len(results), results)
	}
}

// TestLocalPathSource verifies that passing a local directory path as the source
// causes its contents to be available under /src inside the check container.
// This exercises the llb.Local code path used by ds ci run.
func TestLocalPathSource(t *testing.T) {
	exec := newExecutor(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello-from-local\n"), 0o644); err != nil {
		t.Fatalf("create test file: %v", err)
	}

	cfg := executor.Config{
		Checks: []executor.Check{
			{
				Name:  "ci/local-src",
				Image: "alpine",
				Steps: []string{"cat /src/hello.txt"},
			},
		},
	}

	results, err := exec.Run(context.Background(), dir, cfg, ciconfig.TriggerContext{})
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
	if !strings.Contains(r.Logs, "hello-from-local") {
		t.Errorf("expected file content in logs, got: %s", r.Logs)
	}
}

// TestPathTraversalRejected verifies that a local source path containing ".."
// segments is rejected before any build is attempted.
func TestPathTraversalRejected(t *testing.T) {
	exec := newExecutor(t)

	cfg := executor.Config{
		Checks: []executor.Check{
			{Name: "ci/test", Image: "alpine", Steps: []string{"echo hi"}},
		},
	}

	badPaths := []string{
		"../../etc/passwd",
		"../secret",
		"foo/../../bar",
	}
	for _, path := range badPaths {
		results, err := exec.Run(context.Background(), path, cfg, ciconfig.TriggerContext{})
		if err != nil {
			t.Fatalf("Run(%q): unexpected error: %v", path, err)
		}
		if len(results) != 1 {
			t.Fatalf("Run(%q): expected 1 result, got %d", path, len(results))
		}
		if results[0].Status != "failed" {
			t.Errorf("Run(%q): expected status failed, got %s (logs: %s)", path, results[0].Status, results[0].Logs)
		}
		if !strings.Contains(results[0].Logs, "..") {
			t.Errorf("Run(%q): expected error message mentioning '..', got: %s", path, results[0].Logs)
		}
	}
}

// TestLogCapture verifies that stdout and stderr from steps both appear in
// the Logs field.
func TestLogCapture(t *testing.T) {
	exec := newExecutor(t)

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

	results, err := exec.Run(context.Background(), "", cfg, ciconfig.TriggerContext{})
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

// TestE2ECacheWithChecksumAndSecrets verifies that BuildKit cache hits occur on
// a second run when the source archive has the same checksum but a different URL,
// and when OIDCRequestToken differs between runs.
//
// Run 1: source = /archive/v1, ArchiveChecksum = sha256:..., OIDCRequestToken = "test-token-1"
// Run 2: source = /archive/v2 (different URL!), same checksum, OIDCRequestToken = "test-token-2"
//
// If the checksum is used as the cache key (content-addressed), run 2 should hit
// the cache. If the URL is used as the cache key, run 2 will miss (CacheHits == 0),
// which indicates the bug: distinct URLs produce cache misses even for identical content.
func TestE2ECacheWithChecksumAndSecrets(t *testing.T) {
	if pkgBuildkitAddr == "" {
		t.Skip("no buildkitd available")
	}

	// --- Build a tar archive containing hello.txt ---
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	fileContent := []byte("hello world")
	hdr := &tar.Header{
		Name: "hello.txt",
		Mode: 0644,
		Size: int64(len(fileContent)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := tw.Write(fileContent); err != nil {
		t.Fatalf("write tar content: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	tarBytes := tarBuf.Bytes()

	// Compute sha256 checksum in "sha256:<hex>" format.
	sum := sha256.New()
	sum.Write(tarBytes)
	archiveChecksum := "sha256:" + hex.EncodeToString(sum.Sum(nil))
	t.Logf("archive checksum: %s", archiveChecksum)

	// --- HTTP server serving the archive at two different paths ---
	archiveSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/archive/v1" || r.URL.Path == "/archive/v2" {
			w.Header().Set("Content-Type", "application/x-tar")
			w.Write(tarBytes) //nolint:errcheck
			return
		}
		http.NotFound(w, r)
	}))
	defer archiveSrv.Close()

	// --- In-memory registry with OIDC auth ---
	signer, err := citoken.NewLocalSigner()
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}

	jwksSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := signer.PublicKeys(r.Context())
		w.Header().Set("Content-Type", "application/json")
		w.Write(data) //nolint:errcheck
	}))
	defer jwksSrv.Close()

	regSrv := httptest.NewServer(registry.New(registry.NewMemoryHandler(), nil, jwksSrv.URL, "ci-registry", "https://oidc.test"))
	defer regSrv.Close()

	// Determine the host:port that buildkitd (possibly in a container) can use.
	regHost := cacheTestHostAddr(regSrv)
	archiveBaseURL := "http://" + cacheTestHostAddr(archiveSrv)

	// Issue a long-lived token for testorg (used for Docker config auth).
	claims := citoken.JobClaims{
		Issuer:   "https://oidc.test",
		Subject:  "repo:testorg/cache:branch:main:check:test",
		Audience: "ci-registry",
		Repo:     "testorg/cache",
		Branch:   "main",
		JobID:    "cache-test-job",
	}
	tok, err := citoken.IssueJWT(context.Background(), signer, claims)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	// Write a temporary Docker config with Basic auth for the registry.
	dockerConfigDir := cacheTestWriteDockerConfig(t, regHost, tok)
	defer os.RemoveAll(dockerConfigDir)

	cacheRef := regHost + "/testorg/cache:buildkit"

	check := executor.Check{
		Name:  "build",
		Image: "alpine",
		Steps: []string{`cat /src/hello.txt`},
	}

	exec := newExecutor(t)

	// --- Run 1: cold start ---
	t.Log("Run 1: cold start")
	cfg1 := executor.Config{
		Checks:           []executor.Check{check},
		CacheRef:         cacheRef,
		DockerConfigDir:  dockerConfigDir,
		ArchiveChecksum:  archiveChecksum,
		OIDCRequestToken: "test-token-1",
	}
	results1, err := exec.Run(context.Background(), archiveBaseURL+"/archive/v1", cfg1, ciconfig.TriggerContext{})
	if err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	if len(results1) != 1 {
		t.Fatalf("Run 1: expected 1 result, got %d", len(results1))
	}
	r1 := results1[0]
	t.Logf("Run 1: status=%s cacheHits=%d logs=%q", r1.Status, r1.CacheHits, r1.Logs)
	if r1.Status != "passed" {
		t.Errorf("Run 1: expected passed, got %s (logs: %s)", r1.Status, r1.Logs)
	}
	// First run is a cold start; expect 0 cache hits.
	if r1.CacheHits != 0 {
		t.Logf("Run 1: unexpectedly got %d cache hits (may be stale buildkit state)", r1.CacheHits)
	}

	// --- Run 2: different URL, same checksum, different OIDCRequestToken ---
	t.Log("Run 2: warm cache (different URL, same checksum)")
	cfg2 := executor.Config{
		Checks:           []executor.Check{check},
		CacheRef:         cacheRef,
		DockerConfigDir:  dockerConfigDir,
		ArchiveChecksum:  archiveChecksum,
		OIDCRequestToken: "test-token-2",
	}
	results2, err := exec.Run(context.Background(), archiveBaseURL+"/archive/v2", cfg2, ciconfig.TriggerContext{})
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if len(results2) != 1 {
		t.Fatalf("Run 2: expected 1 result, got %d", len(results2))
	}
	r2 := results2[0]
	t.Logf("Run 2: status=%s cacheHits=%d logs=%q", r2.Status, r2.CacheHits, r2.Logs)
	if r2.Status != "passed" {
		t.Errorf("Run 2: expected passed, got %s (logs: %s)", r2.Status, r2.Logs)
	}
	if r2.CacheHits == 0 {
		t.Errorf("Run 2: expected > 0 cache hits (same checksum, different URL), got 0 — "+
			"this indicates the URL is used as the BuildKit cache key instead of the content checksum")
	} else {
		t.Logf("Run 2: got %d cache hits — content-addressed cache is working", r2.CacheHits)
	}
}

// cacheTestHostAddr returns the host:port of a test server that buildkitd
// (possibly running as a container) can reach.
//
// When buildkitd runs on the host (BUILDKIT_ADDR is set) the test server's
// local address is returned unchanged. Otherwise the host gateway IP is used so
// that a containerised buildkitd running on Linux CI (where host.docker.internal
// does not resolve) can still reach the test server. On Docker Desktop
// (macOS/Windows) HostGatewayIP returns "host.docker.internal", which Docker
// Desktop injects into every container's /etc/hosts.
func cacheTestHostAddr(srv *httptest.Server) string {
	addr := strings.TrimPrefix(srv.URL, "http://")
	if os.Getenv("BUILDKIT_ADDR") != "" {
		// Host-native buildkitd can reach localhost directly.
		return addr
	}
	// Containerised buildkitd: use the host gateway IP.
	_, port, _ := net.SplitHostPort(addr)
	return testutil.HostGatewayIP() + ":" + port
}

// cacheTestWriteDockerConfig writes a temporary Docker config.json with Basic
// auth credentials for the given registry host, using token as the password.
// The caller must remove the returned directory when done.
func cacheTestWriteDockerConfig(t *testing.T, regHost, token string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ci-exec-docker-test-*")
	if err != nil {
		t.Fatalf("create docker config dir: %v", err)
	}
	creds := base64.StdEncoding.EncodeToString([]byte("ci-worker:" + token))
	cfg := map[string]any{
		"auths": map[string]any{
			regHost: map[string]string{
				"auth": creds,
			},
		},
	}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o600); err != nil {
		t.Fatalf("write docker config: %v", err)
	}
	return dir
}
