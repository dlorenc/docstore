package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"

	dockerconfig "github.com/docker/cli/cli/config"
	"github.com/distribution/reference"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	gwclient "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth/authprovider"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
)

// Config mirrors the .docstore/ci.yaml DSL and is used for both JSON decode
// in the HTTP handler and YAML parse in Milestone 1.
type Config struct {
	Checks []Check `json:"checks" yaml:"checks"`
}

// Check is a single CI check configuration.
type Check struct {
	Name  string   `json:"name"  yaml:"name"`
	Image string   `json:"image" yaml:"image"`
	Steps []string `json:"steps" yaml:"steps"`
}

// CheckResult is the result of executing a single check.
type CheckResult struct {
	Name   string `json:"name"`
	Status string `json:"status"` // "passed" or "failed"
	Logs   string `json:"logs"`
}

// Executor translates a Config into an LLB DAG and dispatches it to buildkitd.
type Executor struct {
	client *client.Client
}

// New creates an Executor connected to the buildkitd at buildkitAddr.
func New(buildkitAddr string) (*Executor, error) {
	c, err := client.New(context.Background(), buildkitAddr)
	if err != nil {
		return nil, fmt.Errorf("connect to buildkitd at %s: %w", buildkitAddr, err)
	}
	return &Executor{client: c}, nil
}

// Run executes all checks in cfg in parallel against sourceDir.
// It returns once all checks have resolved.
func (e *Executor) Run(ctx context.Context, sourceDir string, cfg Config) ([]CheckResult, error) {
	results := make([]CheckResult, len(cfg.Checks))
	var wg sync.WaitGroup
	for i, check := range cfg.Checks {
		wg.Add(1)
		go func(i int, check Check) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					results[i] = CheckResult{
						Name:   check.Name,
						Status: "failed",
						Logs:   fmt.Sprintf("panic: %v", r),
					}
				}
			}()
			results[i] = e.runCheck(ctx, sourceDir, check)
		}(i, check)
	}
	wg.Wait()
	return results, nil
}

// Close closes the underlying buildkit client.
func (e *Executor) Close() error {
	return e.client.Close()
}

// runCheck executes a single check and returns its result.
func (e *Executor) runCheck(ctx context.Context, sourceDir string, check Check) CheckResult {
	var logBuf bytes.Buffer
	ch := make(chan *client.SolveStatus)

	var collectWg sync.WaitGroup
	collectWg.Add(1)
	go func() {
		defer collectWg.Done()
		for status := range ch {
			for _, vlog := range status.Logs {
				logBuf.Write(vlog.Data)
			}
		}
	}()

	dockerConfigDir := os.Getenv("DOCKER_CONFIG")
	if dockerConfigDir == "" {
		home := os.Getenv("HOME")
		if home == "" {
			home = "/root"
		}
		dockerConfigDir = home + "/.docker"
	}
	dockerCfg, _ := dockerconfig.Load(dockerConfigDir)

	var ap authprovider.DockerAuthProviderConfig
	if dockerCfg != nil {
		ap.AuthConfigProvider = authprovider.LoadAuthConfig(dockerCfg)
	}

	localDirs := map[string]string{"src": sourceDir}

	_, solveErr := e.client.Build(ctx, client.SolveOpt{
		LocalDirs: localDirs,
		Session: []session.Attachable{
			authprovider.NewDockerAuthProvider(ap),
		},
	}, "", func(ctx context.Context, c gwclient.Client) (*gwclient.Result, error) {
		// Resolve image config to get ENV (e.g. PATH in golang:1.25).
		// --oci-worker-no-process-sandbox skips applying image ENV automatically,
		// and empirically the standard OCI worker also requires explicit injection.
		imageRef := check.Image
		if named, err := reference.ParseNormalizedNamed(check.Image); err == nil {
			imageRef = reference.TagNameOnly(named).String()
		}
		var envOpts []llb.RunOption
		if _, _, cfgBytes, err := c.ResolveImageConfig(ctx, imageRef,
			sourceresolver.Opt{ImageOpt: &sourceresolver.ResolveImageOpt{}}); err == nil {
			var imgCfg specs.Image
			if json.Unmarshal(cfgBytes, &imgCfg) == nil {
				for _, env := range imgCfg.Config.Env {
					k, v, _ := strings.Cut(env, "=")
					envOpts = append(envOpts, llb.AddEnv(k, v))
				}
			}
		}
		// Inject DOCKER_HOST so docker:cli checks can reach the dockerd running
		// in the Kata VM. Build containers run with --oci-worker-net=host so they
		// share the host network namespace and can reach dockerd via TCP.
		envOpts = append(envOpts, llb.AddEnv("DOCKER_HOST", "tcp://localhost:2375"))

		source := llb.Local("src")
		state := llb.Image(check.Image).Dir("/src")
		srcMount := source

		for _, step := range check.Steps {
			runOpts := []llb.RunOption{
				llb.Args([]string{"sh", "-c", step}),
				llb.WithCustomName(check.Name + ": " + step),
				llb.AddMount("/src", srcMount),
			}
			runOpts = append(runOpts, envOpts...)
			exec := state.Run(runOpts...)
			state = exec.Root()
			srcMount = exec.GetMount("/src")
		}

		// Use the host architecture so builds work on both amd64 (GKE) and
		// arm64 (Apple Silicon dev machines).
		var platformOpt llb.ConstraintsOpt
		if runtime.GOARCH == "arm64" {
			platformOpt = llb.LinuxArm64
		} else {
			platformOpt = llb.LinuxAmd64
		}
		def, err := state.Marshal(ctx, platformOpt)
		if err != nil {
			return nil, fmt.Errorf("marshal: %w", err)
		}

		return c.Solve(ctx, gwclient.SolveRequest{
			Definition: def.ToPB(),
		})
	}, ch)

	collectWg.Wait()

	status := "passed"
	logs := logBuf.String()
	if solveErr != nil {
		status = "failed"
		if logs == "" {
			logs = solveErr.Error()
		}
	}

	return CheckResult{Name: check.Name, Status: status, Logs: logs}
}
