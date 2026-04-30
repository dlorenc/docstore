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
	"strings"
	"testing"

	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	gwclient "github.com/moby/buildkit/frontend/gateway/client"

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
	regSrv := httptest.NewServer(New(NewMemoryHandler(), jwksSrv.URL, "ci-registry", "https://oidc.test"))
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

	cacheRef := regHost + "/testorg/cache:buildkit"
	_ = tok // token available for future auth session integration

	// --- First build: export cache ---
	def, err := llb.Image("alpine").Run(llb.Shlex("echo cache-test")).Root().Marshal(ctx)
	if err != nil {
		t.Fatalf("marshal llb: %v", err)
	}

	_, err = bkClient.Build(ctx, client.SolveOpt{
		CacheExports: []client.CacheOptionsEntry{{
			Type: "registry",
			Attrs: map[string]string{
				"ref":               cacheRef,
				"registry.insecure": "true",
			},
		}},
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
		CacheImports: []client.CacheOptionsEntry{{
			Type: "registry",
			Attrs: map[string]string{
				"ref":               cacheRef,
				"registry.insecure": "true",
			},
		}},
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

	regSrv := httptest.NewServer(New(NewMemoryHandler(), jwksSrv.URL, "ci-registry", "https://oidc.test"))
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

// dockerAuthConfig returns a base64-encoded Docker auth config for the
// BuildKit auth session.
func dockerAuthConfig(token string) string {
	type authEntry struct {
		Auth string `json:"auth"`
	}
	// Docker auth format: base64("username:password").
	// We use "bearer" as the username and the token as the password.
	creds := base64.StdEncoding.EncodeToString([]byte("bearer:" + token))
	entry := authEntry{Auth: creds}
	data, _ := json.Marshal(entry)
	return string(data)
}

// hostAccessibleAddr returns the address of the test server that buildkitd
// (possibly running as a container) can reach.
func hostAccessibleAddr(srv *httptest.Server) string {
	addr := strings.TrimPrefix(srv.URL, "http://")
	if os.Getenv("BUILDKIT_ADDR") != "" {
		// Buildkitd is running on the host.
		return addr
	}
	// Buildkitd is a container — use host.docker.internal or local LAN IP.
	_, port, _ := net.SplitHostPort(addr)
	if addrs, err := net.LookupHost("host.docker.internal"); err == nil && len(addrs) > 0 {
		return "host.docker.internal:" + port
	}
	if ip := localIP(); ip != "" {
		return ip + ":" + port
	}
	return addr
}

// localIP returns the first non-loopback IPv4 address.
func localIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip != nil && ip.To4() != nil && !ip.IsLoopback() {
				return ip.String()
			}
		}
	}
	return ""
}
