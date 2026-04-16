package testutil

import (
	"context"
	"fmt"
	"os"
	"time"

	dockercontainer "github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// StartBuildkit starts a buildkitd container for use in tests and returns the
// BUILDKIT_ADDR (tcp://host:port) and a cleanup function.
//
// If BUILDKIT_ADDR is already set in the environment it is returned directly
// with a no-op cleanup, following the same pattern as StartSharedPostgres.
func StartBuildkit() (addr string, cleanup func(), err error) {
	if addr := os.Getenv("BUILDKIT_ADDR"); addr != "" {
		return addr, func() {}, nil
	}

	ctx := context.Background()

	ctr, err := testcontainers.Run(ctx, "moby/buildkit:v0.29.0",
		testcontainers.WithExposedPorts("1234/tcp"),
		testcontainers.WithCmd("--addr", "tcp://0.0.0.0:1234"),
		testcontainers.WithHostConfigModifier(func(hc *dockercontainer.HostConfig) {
			hc.Privileged = true
		}),
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort("1234/tcp").WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		return "", func() {}, fmt.Errorf("start buildkit container: %w", err)
	}

	bkAddr, err := ctr.PortEndpoint(ctx, "1234/tcp", "tcp")
	if err != nil {
		_ = ctr.Terminate(ctx)
		return "", func() {}, fmt.Errorf("get buildkit endpoint: %w", err)
	}

	return bkAddr, func() {
		if termErr := ctr.Terminate(ctx); termErr != nil {
			fmt.Fprintf(os.Stderr, "terminate buildkit container: %v\n", termErr)
		}
	}, nil
}
