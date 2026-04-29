package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dlorenc/docstore/internal/ciconfig"
	"github.com/dlorenc/docstore/internal/executor"
)

// DefaultBuildkitAddr is the default buildkitd address used by ds ci run.
const DefaultBuildkitAddr = "tcp://localhost:1234"

// ErrChecksFailed is returned by CIRun when checks execute but one or more fail.
// The caller should exit 1 without printing an additional error message,
// since CIRun already printed the failure summary.
var ErrChecksFailed = errors.New("checks failed")

// CIRun runs CI checks locally against the working directory.
// No docstore server is required; it reads .docstore/ci.yaml from disk,
// connects to buildkitd, and streams results to a.Out.
func (a *App) CIRun(checkFilter, trigger, baseBranch, buildkitAddr string) error {
	// 1. Read and parse .docstore/ci.yaml from working directory.
	ciYAMLPath := filepath.Join(a.Dir, configDir, "ci.yaml")
	data, err := os.ReadFile(ciYAMLPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no .docstore/ci.yaml found — is this a docstore workspace?")
		}
		return fmt.Errorf("reading ci.yaml: %w", err)
	}
	var cfg executor.Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse ci.yaml: %w", err)
	}
	if len(cfg.Checks) == 0 {
		return fmt.Errorf("no checks defined in .docstore/ci.yaml")
	}

	// 2. Read current branch from workspace config.
	workCfg, err := a.loadConfig()
	if err != nil {
		return fmt.Errorf("reading workspace config: %w", err)
	}
	branch := workCfg.Branch

	// 3. Apply check filter if specified.
	if checkFilter != "" {
		found := false
		for _, check := range cfg.Checks {
			if check.Name == checkFilter {
				found = true
				cfg.Checks = []executor.Check{check}
				break
			}
		}
		if !found {
			return fmt.Errorf("no check named %q in ci.yaml", checkFilter)
		}
	}

	// 4. Warn if trigger implies a base branch but none was provided.
	if (trigger == "proposal" || trigger == "proposal_synchronized") && baseBranch == "" {
		fmt.Fprintf(a.Out, "warning: --trigger %s without --base-branch; base_branch will be empty (may affect if: expressions)\n", trigger)
	}

	// 5. Build TriggerContext.
	triggerCtx := ciconfig.TriggerContext{
		Type:       trigger,
		Branch:     branch,
		BaseBranch: baseBranch,
	}

	// 6. Print header.
	n := len(cfg.Checks)
	word := "checks"
	if n == 1 {
		word = "check"
	}
	fmt.Fprintf(a.Out, "running %d %s on branch %s (trigger: %s)\n\n", n, word, branch, trigger)
	for _, check := range cfg.Checks {
		fmt.Fprintf(a.Out, "[%s]  running...\n", check.Name)
	}
	fmt.Fprintln(a.Out)

	// 7. Connect to buildkitd.
	exec, err := executor.New(buildkitAddr)
	if err != nil {
		return fmt.Errorf("could not connect to buildkitd at %s — is buildkitd running? (%w)", buildkitAddr, err)
	}
	defer exec.Close()

	// 8. Run all checks. executor.Run blocks until all checks complete.
	results, err := exec.Run(context.Background(), a.Dir, cfg, triggerCtx)
	if err != nil {
		return fmt.Errorf("executor: %w", err)
	}

	// 9. Print per-check output with name prefix.
	passed := 0
	failed := 0
	for _, result := range results {
		prefix := "[" + result.Name + "]"
		logs := strings.TrimRight(result.Logs, "\n")
		if logs != "" {
			for line := range strings.SplitSeq(logs, "\n") {
				fmt.Fprintf(a.Out, "%s  %s\n", prefix, line)
			}
		}
		if result.Status == "passed" {
			fmt.Fprintf(a.Out, "%s  [pass]\n", prefix)
			passed++
		} else {
			fmt.Fprintf(a.Out, "%s  [fail]\n", prefix)
			failed++
		}
	}

	// 10. Overall summary.
	fmt.Fprintf(a.Out, "\n%d passed, %d failed\n", passed, failed)

	if failed > 0 {
		return ErrChecksFailed
	}
	return nil
}
