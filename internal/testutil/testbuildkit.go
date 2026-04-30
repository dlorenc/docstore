package testutil

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/moby/buildkit/client"
	dockercontainer "github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// buildkitdConfig is the buildkitd.toml content used in tests.
// It enables plain-HTTP access to registries commonly used in test setups
// (host.docker.internal for containerised buildkitd reaching a host httptest
// server, and localhost for host-native buildkitd).
const buildkitdConfig = `
[registry."host.docker.internal"]
  http = true
  insecure = true

[registry."localhost"]
  http = true
  insecure = true
`

// StartBuildkit starts a buildkitd container for use in tests and returns the
// BUILDKIT_ADDR (tcp://host:port) and a cleanup function.
//
// If BUILDKIT_ADDR is already set in the environment it is returned directly
// with a no-op cleanup, following the same pattern as StartSharedPostgres.
func StartBuildkit() (addr string, cleanup func(), err error) {
	if addr := os.Getenv("BUILDKIT_ADDR"); addr != "" {
		return addr, func() {}, nil
	}

	// Write the buildkitd config to a temp file so it can be bind-mounted into
	// the container. Files must be mounted (not copied after start) so that
	// buildkitd reads the config when the process first starts.
	cfgFile, err := os.CreateTemp("", "buildkitd-*.toml")
	if err != nil {
		return "", func() {}, fmt.Errorf("create buildkitd config tempfile: %w", err)
	}
	if _, err := cfgFile.WriteString(buildkitdConfig); err != nil {
		cfgFile.Close()
		os.Remove(cfgFile.Name())
		return "", func() {}, fmt.Errorf("write buildkitd config: %w", err)
	}
	cfgFile.Close()
	cfgPath := cfgFile.Name()

	ctx := context.Background()

	ctr, err := testcontainers.Run(ctx, "moby/buildkit:v0.29.0",
		testcontainers.WithExposedPorts("1234/tcp"),
		testcontainers.WithCmd("--addr", "tcp://0.0.0.0:1234", "--config", "/etc/buildkit/buildkitd.toml"),
		testcontainers.WithHostConfigModifier(func(hc *dockercontainer.HostConfig) {
			hc.Privileged = true
			// Bind-mount the config so buildkitd reads it at startup.
			hc.Binds = append(hc.Binds, cfgPath+":/etc/buildkit/buildkitd.toml:ro")
		}),
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort("1234/tcp").WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		os.Remove(cfgPath)
		return "", func() {}, fmt.Errorf("start buildkit container: %w", err)
	}

	bkAddr, err := ctr.PortEndpoint(ctx, "1234/tcp", "tcp")
	if err != nil {
		_ = ctr.Terminate(ctx)
		os.Remove(cfgPath)
		return "", func() {}, fmt.Errorf("get buildkit endpoint: %w", err)
	}

	// wait.ForListeningPort only confirms TCP is bound; buildkitd may still be
	// initialising its OCI worker and gRPC service. Poll ListWorkers until the
	// daemon actually responds so that tests don't race against a half-started
	// buildkitd.
	if err := waitForBuildkitReady(ctx, bkAddr, 60*time.Second); err != nil {
		_ = ctr.Terminate(ctx)
		os.Remove(cfgPath)
		return "", func() {}, fmt.Errorf("buildkitd at %s did not become ready: %w", bkAddr, err)
	}

	return bkAddr, func() {
		if termErr := ctr.Terminate(ctx); termErr != nil {
			fmt.Fprintf(os.Stderr, "terminate buildkit container: %v\n", termErr)
		}
		os.Remove(cfgPath)
	}, nil
}

// waitForBuildkitReady polls the buildkitd gRPC API until ListWorkers succeeds
// or the deadline is reached. This bridges the gap between the TCP port
// becoming reachable and the daemon being fully initialised.
func waitForBuildkitReady(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		c, err := client.New(ctx, addr)
		if err != nil {
			lastErr = err
			time.Sleep(500 * time.Millisecond)
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, lastErr = c.ListWorkers(probeCtx)
		cancel()
		c.Close()
		if lastErr == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timeout after %s; last error: %w", timeout, lastErr)
}
