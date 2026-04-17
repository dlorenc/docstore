package executor

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth/authprovider"
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
	source := llb.Local("src")
	state := llb.Image(check.Image).Dir("/src")
	srcMount := source

	for _, step := range check.Steps {
		exec := state.Run(
			llb.Args([]string{"sh", "-c", step}),
			llb.WithCustomName(check.Name+": "+step),
			llb.AddMount("/src", srcMount),
		)
		state = exec.Root()
		srcMount = exec.GetMount("/src")
	}

	def, err := state.Marshal(ctx, llb.LinuxAmd64)
	if err != nil {
		return CheckResult{
			Name:   check.Name,
			Status: "failed",
			Logs:   fmt.Sprintf("marshal error: %v", err),
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

	_, solveErr := e.client.Solve(ctx, def, client.SolveOpt{
		LocalDirs: map[string]string{"src": sourceDir},
		Session: []session.Attachable{
			authprovider.NewDockerAuthProvider(authprovider.DockerAuthProviderConfig{}),
		},
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
