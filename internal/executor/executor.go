package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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

	"github.com/dlorenc/docstore/internal/ciconfig"
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
	If    string   `json:"if,omitempty"    yaml:"if,omitempty"`    // expression; empty = always run
	Needs []string `json:"needs,omitempty" yaml:"needs,omitempty"` // parsed but not yet implemented
}

// CheckResult is the result of executing a single check.
type CheckResult struct {
	Name   string `json:"name"`
	Status string `json:"status"` // "passed" or "failed"
	Logs   string `json:"logs"`
}

// Executor translates a Config into an LLB DAG and dispatches it to buildkitd.
type Executor struct {
	client      *client.Client
	cacheBucket string // empty = no cache
}

// New creates an Executor connected to the buildkitd at buildkitAddr.
// cacheBucket is an optional GCS bucket name for BuildKit cache; pass "" to disable.
func New(buildkitAddr, cacheBucket string) (*Executor, error) {
	c, err := client.New(context.Background(), buildkitAddr)
	if err != nil {
		return nil, fmt.Errorf("connect to buildkitd at %s: %w", buildkitAddr, err)
	}
	return &Executor{client: c, cacheBucket: cacheBucket}, nil
}

// Run executes all checks in cfg in parallel against the given source.
// source can be:
//   - an HTTP(S) URL of a tar archive — BuildKit fetches it directly via llb.HTTP
//   - a local filesystem path — BuildKit uploads it via llb.Local (used by ds ci run)
//   - "" — uses an empty scratch state (useful in tests that do not read source files)
//
// Checks whose if: condition evaluates to false are skipped (omitted from results).
// It returns once all checks have resolved.
func (e *Executor) Run(ctx context.Context, source string, cfg Config, triggerCtx ciconfig.TriggerContext) ([]CheckResult, error) {
	// Pre-filter: evaluate if: conditions before launching goroutines.
	type indexedCheck struct {
		idx   int
		check Check
	}
	var toRun []indexedCheck
	for i, check := range cfg.Checks {
		if check.If == "" {
			toRun = append(toRun, indexedCheck{i, check})
			continue
		}
		run, err := ciconfig.EvalIf(check.If, triggerCtx)
		if err != nil {
			slog.Warn("if: condition evaluation error, running job anyway",
				"check", check.Name, "if", check.If, "error", err)
			toRun = append(toRun, indexedCheck{i, check})
			continue
		}
		if !run {
			slog.Info("skipping job: if condition false", "check", check.Name, "if", check.If)
			continue
		}
		toRun = append(toRun, indexedCheck{i, check})
	}

	results := make([]CheckResult, len(toRun))
	var wg sync.WaitGroup
	for j, ic := range toRun {
		wg.Add(1)
		go func(j int, check Check) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					results[j] = CheckResult{
						Name:   check.Name,
						Status: "failed",
						Logs:   fmt.Sprintf("panic: %v", r),
					}
				}
			}()
			results[j] = e.runCheck(ctx, source, check)
		}(j, ic.check)
	}
	wg.Wait()
	return results, nil
}

// Close closes the underlying buildkit client.
func (e *Executor) Close() error {
	return e.client.Close()
}

// runCheck executes a single check and returns its result.
func (e *Executor) runCheck(ctx context.Context, source string, check Check) CheckResult {
	isHTTP := strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://")
	isLocal := source != "" && !isHTTP

	// Reject local paths containing ".." segments to prevent directory traversal.
	if isLocal {
		cleaned := filepath.Clean(source)
		for _, seg := range strings.Split(cleaned, string(filepath.Separator)) {
			if seg == ".." {
				return CheckResult{
					Name:   check.Name,
					Status: "failed",
					Logs:   "invalid source path: '..' path traversal is not allowed",
				}
			}
		}
	}

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

	solveOpt := client.SolveOpt{
		// FrontendAttrs must be non-nil: BuildKit v0.29 calls maps.Clone(opt.FrontendAttrs)
		// then maps.Copy(result, cacheOpt.frontendAttrs), which panics when CacheImports/
		// CacheExports are set and FrontendAttrs is nil.
		FrontendAttrs: map[string]string{},
		Session: []session.Attachable{
			authprovider.NewDockerAuthProvider(ap),
		},
	}
	if isLocal {
		solveOpt.LocalDirs = map[string]string{
			"src": source,
		}
	}
	if e.cacheBucket != "" {
		solveOpt.CacheImports = []client.CacheOptionsEntry{
			{Type: "gcs", Attrs: map[string]string{
				"bucket": e.cacheBucket,
				"prefix": "buildkit-cache/",
			}},
		}
		solveOpt.CacheExports = []client.CacheOptionsEntry{
			{Type: "gcs", Attrs: map[string]string{
				"bucket": e.cacheBucket,
				"prefix": "buildkit-cache/",
				"mode":   "max",
			}},
		}
	}

	_, solveErr := e.client.Build(ctx, solveOpt, "", func(ctx context.Context, c gwclient.Client) (*gwclient.Result, error) {
		// Resolve image config to get ENV (e.g. PATH in golang:1.25).
		// --oci-worker-no-process-sandbox skips applying image ENV automatically,
		// and empirically the standard OCI worker also requires explicit injection.
		imageRef := check.Image
		if named, err := reference.ParseNormalizedNamed(check.Image); err == nil {
			imageRef = reference.TagNameOnly(named).String()
		}
		var envOpts []llb.RunOption
		_, _, cfgBytes, err := c.ResolveImageConfig(ctx, imageRef,
			sourceresolver.Opt{ImageOpt: &sourceresolver.ResolveImageOpt{}})
		if err != nil {
			return nil, fmt.Errorf("resolve image config for %s: %w", imageRef, err)
		}
		var imgCfg specs.Image
		if json.Unmarshal(cfgBytes, &imgCfg) == nil {
			for _, env := range imgCfg.Config.Env {
				k, v, _ := strings.Cut(env, "=")
				envOpts = append(envOpts, llb.AddEnv(k, v))
			}
		}
		// Inject DOCKER_HOST so docker:cli checks can reach the dockerd running
		// in the Kata VM. Build containers run with --oci-worker-net=host so they
		// share the host network namespace and can reach dockerd via TCP.
		envOpts = append(envOpts, llb.AddEnv("DOCKER_HOST", "tcp://localhost:2375"))

		// Build the source mount based on the source type:
		//   HTTP(S) URL → BuildKit fetches the tar directly; busybox extracts it to /src.
		//   local path  → BuildKit uploads the directory via llb.Local (ds ci run path).
		//   ""          → empty scratch state (tests that do not access source files).
		var srcMount llb.State
		if isHTTP {
			httpSrc := llb.HTTP(source, llb.Filename("source.tar"))
			srcMount = llb.Image("busybox:latest").
				Run(
					llb.Args([]string{"sh", "-c", "mkdir -p /src && tar -xf /archive/source.tar -C /src"}),
					llb.AddMount("/archive", httpSrc),
				).GetMount("/src")
		} else if isLocal {
			srcMount = llb.Local("src")
		} else {
			srcMount = llb.Scratch()
		}
		state := llb.Image(check.Image).Dir("/src")

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
