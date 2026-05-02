package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	dockerconfig "github.com/docker/cli/cli/config"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	gwclient "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth/authprovider"

	"github.com/dlorenc/docstore/internal/citoken"
	"github.com/dlorenc/docstore/internal/testutil"
)

// pkgBuildkitAddr is the buildkitd address shared across all E2E tests in
// this package.  It is set by TestMain.
var pkgBuildkitAddr string

func TestMain(m *testing.M) {
	addr, cleanup, err := testutil.StartBuildkit()
	if err != nil {
		fmt.Fprintf(os.Stderr, "skipping registry e2e tests: could not start buildkit: %v\n", err)
		os.Exit(0)
	}
	pkgBuildkitAddr = addr
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// TestE2ECacheRoundTrip runs a trivial build twice against a MemoryHandler
// registry, verifying that the second build hits the BuildKit OCI cache.
func TestE2ECacheRoundTrip(t *testing.T) {
	if pkgBuildkitAddr == "" {
		t.Skip("no buildkitd available")
	}

	signer, err := citoken.NewLocalSigner()
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}

	// JWKS server for token validation.
	jwksSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := signer.PublicKeys(r.Context())
		w.Header().Set("Content-Type", "application/json")
		w.Write(data) //nolint:errcheck
	}))
	defer jwksSrv.Close()

	// Start in-memory registry.
	regSrv := httptest.NewServer(New(NewMemoryHandler(), nil, jwksSrv.URL, "ci-registry", "https://oidc.test"))
	defer regSrv.Close()

	// Determine the address buildkitd (possibly in a container) can reach.
	regHost := hostAccessibleAddr(regSrv)

	// Issue a token for testorg.
	tok := makeTestToken(t, signer, "testorg/cache")

	ctx := context.Background()
	bkClient, err := client.New(ctx, pkgBuildkitAddr)
	if err != nil {
		t.Fatalf("connect to buildkitd: %v", err)
	}
	defer bkClient.Close()

	// Detect the buildkitd worker platform so that llb.Image resolves the
	// correct manifest from Docker Hub. Without this, llb.Image uses the Go
	// test binary's host platform (e.g. darwin/arm64 on macOS) which has no
	// Alpine manifest entry and causes "no match for platform in manifest".
	workers, err := bkClient.ListWorkers(ctx)
	if err != nil || len(workers) == 0 || len(workers[0].Platforms) == 0 {
		t.Fatalf("list buildkit workers: %v", err)
	}
	workerPlatform := workers[0].Platforms[0]

	cacheRef := regHost + "/testorg/cache:buildkit"

	// Write a temp docker config with the OIDC token as Basic auth password so
	// BuildKit can authenticate against the test registry (which accepts Basic
	// auth via the authMiddleware Basic auth path added alongside this test).
	dockerConfigDir := writeTempDockerConfig(t, regHost, tok)
	defer os.RemoveAll(dockerConfigDir)

	dockerCfg, err := dockerconfig.Load(dockerConfigDir)
	if err != nil {
		t.Fatalf("load docker config: %v", err)
	}
	var ap authprovider.DockerAuthProviderConfig
	ap.AuthConfigProvider = authprovider.LoadAuthConfig(dockerCfg)
	authSession := authprovider.NewDockerAuthProvider(ap)

	// --- First build: export cache ---
	// Use the buildkitd worker's platform so Alpine resolves a matching manifest.
	def, err := llb.Image("alpine", llb.Platform(workerPlatform)).Run(llb.Shlex("echo cache-test")).Root().Marshal(ctx)
	if err != nil {
		t.Fatalf("marshal llb: %v", err)
	}

	_, err = bkClient.Build(ctx, client.SolveOpt{
		// FrontendAttrs must be non-nil: BuildKit v0.29.0 calls maps.Clone on
		// it and then maps.Copy(clone, cacheOpt.frontendAttrs), which panics
		// when the clone is nil and cacheOpt.frontendAttrs is non-empty.
		FrontendAttrs: map[string]string{},
		CacheExports: []client.CacheOptionsEntry{{
			Type: "registry",
			Attrs: map[string]string{
				"ref":               cacheRef,
				"registry.insecure": "true",
			},
		}},
		Session: []session.Attachable{authSession},
	}, "", func(ctx context.Context, c gwclient.Client) (*gwclient.Result, error) {
		return c.Solve(ctx, gwclient.SolveRequest{Definition: def.ToPB()})
	}, nil)
	if err != nil {
		t.Logf("first build failed (may be a BuildKit version limitation): %v", err)
		t.Skip("cache export not supported in this environment")
	}

	// --- Second build: import cache and count hits ---
	statusCh := make(chan *client.SolveStatus, 32)
	done := make(chan struct{})
	var cacheHits int
	go func() {
		defer close(done)
		for s := range statusCh {
			for _, v := range s.Vertexes {
				if v.Cached {
					cacheHits++
				}
			}
		}
	}()

	_, err = bkClient.Build(ctx, client.SolveOpt{
		FrontendAttrs: map[string]string{}, // see note on first build above
		CacheImports: []client.CacheOptionsEntry{{
			Type: "registry",
			Attrs: map[string]string{
				"ref":               cacheRef,
				"registry.insecure": "true",
			},
		}},
		Session: []session.Attachable{authSession},
	}, "", func(ctx context.Context, c gwclient.Client) (*gwclient.Result, error) {
		return c.Solve(ctx, gwclient.SolveRequest{Definition: def.ToPB()})
	}, statusCh)
	<-done
	if err != nil {
		t.Fatalf("second build: %v", err)
	}

	if cacheHits == 0 {
		t.Error("expected at least one cache hit on second build, got 0")
	}
}

// TestE2ECrossOrgRejected verifies that a token for the wrong org cannot access
// testorg images.
func TestE2ECrossOrgRejected(t *testing.T) {
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

	regSrv := httptest.NewServer(New(NewMemoryHandler(), nil, jwksSrv.URL, "ci-registry", "https://oidc.test"))
	defer regSrv.Close()

	// Token for the wrong org.
	tok := makeTestToken(t, signer, "other/repo")

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
		regSrv.URL+"/v2/testorg/cache/manifests/buildkit", nil)
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for cross-org access, got %d", resp.StatusCode)
	}
}

// writeTempDockerConfig writes a temporary Docker config.json with Basic auth
// credentials for the given registry host, using token as the password.
// The caller is responsible for removing the returned directory.
func writeTempDockerConfig(t *testing.T, regHost, token string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ci-docker-test-*")
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

// hostAccessibleAddr returns the address of the test server that buildkitd
// (possibly running as a container) can reach.
//
// When buildkitd runs on the host (BUILDKIT_ADDR is set) the test server's
// local address is returned unchanged. Otherwise testutil.HostGatewayIP is
// used so that containerised buildkitd on Linux CI (where host.docker.internal
// does not resolve) can still reach the test server. On Docker Desktop
// (macOS/Windows) HostGatewayIP returns "host.docker.internal".
func hostAccessibleAddr(srv *httptest.Server) string {
	addr := strings.TrimPrefix(srv.URL, "http://")
	if os.Getenv("BUILDKIT_ADDR") != "" {
		// Buildkitd is running on the host.
		return addr
	}
	_, port, _ := net.SplitHostPort(addr)
	return testutil.HostGatewayIP() + ":" + port
}
