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
	"slices"
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
	"github.com/moby/buildkit/session/secrets/secretsprovider"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/dlorenc/docstore/internal/ciconfig"
)

// Config mirrors the .docstore/ci.yaml DSL and is used for both JSON decode
// in the HTTP handler and YAML parse in Milestone 1.
type Config struct {
	Checks []Check `json:"checks" yaml:"checks"`

	// CacheRef is an optional BuildKit registry cache reference of the form
	// "host/org/repo:tag". When set, cache is exported to and imported from
	// this OCI reference using the registry backend with insecure (HTTP) mode.
	// Not parsed from ci.yaml; populated by the CI worker at runtime.
	CacheRef string `json:"-" yaml:"-"`

	// DockerConfigDir is an optional directory containing a config.json for
	// registry authentication. When set, overrides DOCKER_CONFIG and the
	// default ~/.docker directory. Not parsed from ci.yaml.
	DockerConfigDir string `json:"-" yaml:"-"`

	// ArchiveChecksum is the expected sha256 digest of the source archive in
	// the format "sha256:<hex>". When non-empty, passed to llb.HTTP so BuildKit
	// verifies the downloaded archive before use. Not parsed from ci.yaml.
	ArchiveChecksum string `json:"-" yaml:"-"`

	// OIDCRequestToken and OIDCRequestURL are the per-job OIDC credentials.
	// When set, they are exposed to check steps via BuildKit secret mounts at
	// /run/secrets/docstore_oidc_request_token and
	// /run/secrets/docstore_oidc_request_url respectively.
	// Secrets are NOT part of the BuildKit cache key, so using secret mounts
	// avoids busting the cache on every run (unlike env var injection).
	// Not parsed from ci.yaml; populated by the CI worker at runtime.
	OIDCRequestToken string `json:"-" yaml:"-"`
	OIDCRequestURL   string `json:"-" yaml:"-"`
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
	Name      string `json:"name"`
	Status    string `json:"status"` // "passed" or "failed"
	Logs      string `json:"logs"`
	CacheHits int    `json:"-"` // number of BuildKit vertexes served from cache
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
		check := ic.check
		wg.Go(func() {
			defer func() {
				if r := recover(); r != nil {
					results[j] = CheckResult{
						Name:   check.Name,
						Status: "failed",
						Logs:   fmt.Sprintf("panic: %v", r),
					}
				}
			}()
			results[j] = e.runCheck(ctx, source, check, cfg.CacheRef, cfg.DockerConfigDir, cfg.ArchiveChecksum, cfg.OIDCRequestToken, cfg.OIDCRequestURL)
		})
	}
	wg.Wait()
	return results, nil
}

// Close closes the underlying buildkit client.
func (e *Executor) Close() error {
	return e.client.Close()
}

// runCheck executes a single check and returns its result.
// cacheRef, when non-empty, enables BuildKit registry cache export/import.
// dockerConfigDir, when non-empty, overrides the Docker config directory for auth.
// archiveChecksum, when non-empty, passes llb.Checksum to BuildKit for HTTP sources.
// oidcToken and oidcURL, when non-empty, are exposed via BuildKit secret mounts so
// they do not bust the cache key (unlike env var injection).
func (e *Executor) runCheck(ctx context.Context, source string, check Check, cacheRef, dockerConfigDir, archiveChecksum, oidcToken, oidcURL string) CheckResult {
	isHTTP := strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://")
	isLocal := source != "" && !isHTTP

	// Reject local paths containing ".." segments to prevent directory traversal.
	if isLocal {
		cleaned := filepath.Clean(source)
		if slices.Contains(strings.Split(cleaned, string(filepath.Separator)), "..") {
			return CheckResult{
				Name:   check.Name,
				Status: "failed",
				Logs:   "invalid source path: '..' path traversal is not allowed",
			}
		}
	}

	var logBuf bytes.Buffer
	ch := make(chan *client.SolveStatus)

	var cacheHits int
	var collectWg sync.WaitGroup
	collectWg.Go(func() {
		for status := range ch {
			for _, vlog := range status.Logs {
				logBuf.Write(vlog.Data)
			}
			for _, v := range status.Vertexes {
				if v.Cached {
					cacheHits++
					slog.Debug("vertex cached", "check", check.Name, "vertex", v.Name)
				}
				if v.Completed != nil && v.Started != nil {
					slog.Info("vertex complete",
						"check", check.Name,
						"vertex", v.Name,
						"cached", v.Cached,
						"duration_ms", v.Completed.Sub(*v.Started).Milliseconds(),
					)
				}
			}
		}
	})

	effectiveDockerConfigDir := dockerConfigDir
	if effectiveDockerConfigDir == "" {
		effectiveDockerConfigDir = os.Getenv("DOCKER_CONFIG")
		if effectiveDockerConfigDir == "" {
			home := os.Getenv("HOME")
			if home == "" {
				home = "/root"
			}
			effectiveDockerConfigDir = home + "/.docker"
		}
	}
	dockerCfg, _ := dockerconfig.Load(effectiveDockerConfigDir)

	var ap authprovider.DockerAuthProviderConfig
	if dockerCfg != nil {
		ap.AuthConfigProvider = authprovider.LoadAuthConfig(dockerCfg)
	}

	sessionAttachables := []session.Attachable{authprovider.NewDockerAuthProvider(ap)}
	if oidcToken != "" {
		// Expose OIDC credentials as BuildKit secrets so they don't bust the
		// cache key. Steps read from /run/secrets/docstore_oidc_request_token
		// and /run/secrets/docstore_oidc_request_url.
		secretMap := map[string][]byte{
			"docstore_oidc_request_token": []byte(oidcToken),
			"docstore_oidc_request_url":   []byte(oidcURL),
		}
		sessionAttachables = append(sessionAttachables, secretsprovider.FromMap(secretMap))
	}

	solveOpt := client.SolveOpt{
		FrontendAttrs: map[string]string{},
		Session:       sessionAttachables,
	}
	if cacheRef != "" {
		solveOpt.CacheExports = []client.CacheOptionsEntry{{
			Type: "registry",
			Attrs: map[string]string{
				"ref":               cacheRef,
				"registry.insecure": "true",
				// mode=max exports all intermediate layers, not just the
				// final result. CI checks typically produce no output
				// artifact, so mode=min would export nothing. mode=max
				// caches base image layers (e.g. alpine) so subsequent
				// runs skip the Docker Hub fetch entirely.
				"mode": "max",
			},
		}}
		solveOpt.CacheImports = []client.CacheOptionsEntry{{
			Type: "registry",
			Attrs: map[string]string{
				"ref":               cacheRef,
				"registry.insecure": "true",
			},
		}}
	}
	if isLocal {
		solveOpt.LocalDirs = map[string]string{
			"src": source,
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
		// DOCKER_HOST is cache-stable (same value every run) so env var is fine here.
		envOpts = append(envOpts, llb.AddEnv("DOCKER_HOST", "tcp://localhost:2375"))

		// Build the source mount based on the source type:
		//   HTTP(S) URL → BuildKit fetches the tar directly; busybox extracts it to /src.
		//   local path  → BuildKit uploads the directory via llb.Local (ds ci run path).
		//   ""          → empty scratch state (tests that do not access source files).
		var srcMount llb.State
		if isHTTP {
			httpOpts := []llb.HTTPOption{llb.Filename("source.tar")}
			if archiveChecksum != "" {
				httpOpts = append(httpOpts, llb.Checksum(digest.Digest(archiveChecksum)))
			}
			httpSrc := llb.HTTP(source, httpOpts...)
			// /src must be declared as an explicit output mount (via llb.AddMount with
			// llb.Scratch() as the base) so that GetMount("/src") returns the populated
			// state. Without it, GetMount returns nil and the golang step sees an empty /src.
			srcMount = llb.Image("busybox:latest").
				Run(
					llb.Args([]string{"sh", "-c", "tar -xf /archive/source.tar -C /src"}),
					llb.AddMount("/archive", httpSrc),
					llb.AddMount("/src", llb.Scratch()),
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
				llb.AddMount("/go/pkg/mod", llb.Scratch(), llb.AsPersistentCacheDir("go-mod", llb.CacheMountShared)),
				llb.AddMount("/root/.cache/go-build", llb.Scratch(), llb.AsPersistentCacheDir("go-build", llb.CacheMountShared)),
				llb.AddMount("/root/.npm", llb.Scratch(), llb.AsPersistentCacheDir("npm", llb.CacheMountShared)),
				llb.AddMount("/root/.cache/pip", llb.Scratch(), llb.AsPersistentCacheDir("pip", llb.CacheMountShared)),
				llb.AddMount("/usr/local/cargo/registry", llb.Scratch(), llb.AsPersistentCacheDir("cargo-registry", llb.CacheMountShared)),
			}
			runOpts = append(runOpts, envOpts...)
			if oidcToken != "" {
				runOpts = append(runOpts,
					llb.AddSecret("/run/secrets/docstore_oidc_request_token", llb.SecretID("docstore_oidc_request_token"), llb.SecretOptional),
					llb.AddSecret("/run/secrets/docstore_oidc_request_url", llb.SecretID("docstore_oidc_request_url"), llb.SecretOptional),
				)
			}
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

	return CheckResult{Name: check.Name, Status: status, Logs: logs, CacheHits: cacheHits}
}
