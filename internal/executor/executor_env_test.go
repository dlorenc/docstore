package executor_test

import (
	"context"
	"strings"
	"testing"

	"github.com/dlorenc/docstore/internal/ciconfig"
	"github.com/dlorenc/docstore/internal/executor"
)

// TestValidateEnvAcceptsValid asserts the regex's positive boundaries: leading
// letter or underscore, uppercase / lowercase / digits / underscore body. Mixed
// case is intentionally allowed (npm_config_*, http_proxy etc. are real-world).
func TestValidateEnvAcceptsValid(t *testing.T) {
	good := []string{
		"FOO",
		"foo",
		"Foo_Bar",
		"_LEADING_UNDERSCORE",
		"foo123",
		"A",
		"_",
		"NODE_ENV",
		"npm_config_registry",
	}
	for _, k := range good {
		t.Run(k, func(t *testing.T) {
			cfg := &executor.Config{Env: map[string]string{k: "v"}}
			if err := executor.ValidateEnv(cfg); err != nil {
				t.Errorf("expected %q to validate, got %v", k, err)
			}
		})
	}
}

// TestValidateEnvRejectsBadShapes covers every regex-rejection axis: empty,
// leading digit, hyphen, dot, space, equals.
func TestValidateEnvRejectsBadShapes(t *testing.T) {
	bad := []string{
		"",
		"1FOO",
		"FOO-BAR",
		"FOO.BAR",
		"FOO BAR",
		"FOO=BAR",
		"FOO/BAR",
	}
	for _, k := range bad {
		t.Run(k, func(t *testing.T) {
			cfg := &executor.Config{Env: map[string]string{k: "v"}}
			if err := executor.ValidateEnv(cfg); err == nil {
				t.Errorf("expected %q to fail validation", k)
			}
		})
	}
}

// TestValidateEnvRejectsReserved guards the DOCSTORE_ prefix and the bare CI
// key (case-insensitive) so user values can never silently shadow auto-injected
// docstore vars.
func TestValidateEnvRejectsReserved(t *testing.T) {
	bad := []string{
		"CI",
		"ci",
		"Ci",
		"DOCSTORE_FOO",
		"docstore_foo",
		"DOCSTORE_",
		"DOCSTORE_RUN_ID",
	}
	for _, k := range bad {
		t.Run(k, func(t *testing.T) {
			cfg := &executor.Config{Env: map[string]string{k: "v"}}
			err := executor.ValidateEnv(cfg)
			if err == nil {
				t.Errorf("expected %q to fail validation", k)
				return
			}
			if !strings.Contains(err.Error(), "reserved") {
				t.Errorf("expected error to mention 'reserved', got %v", err)
			}
		})
	}
}

// TestValidateEnvScopes confirms validation walks both top-level Env and every
// check's Env, and that errors are aggregated rather than short-circuited.
func TestValidateEnvScopes(t *testing.T) {
	cfg := &executor.Config{
		Env: map[string]string{"1BAD": "v"},
		Checks: []executor.Check{
			{Name: "ci/a", Env: map[string]string{"DOCSTORE_X": "v"}},
			{Name: "ci/b", Env: map[string]string{"GOOD": "v"}},
		},
	}
	err := executor.ValidateEnv(cfg)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"top-level env", `check "ci/a" env`, "1BAD", "DOCSTORE_X"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q: %s", want, msg)
		}
	}
}

// TestValidateEnvNilSafe ensures the helper tolerates nil/empty configs.
func TestValidateEnvNilSafe(t *testing.T) {
	if err := executor.ValidateEnv(nil); err != nil {
		t.Errorf("nil cfg should validate, got %v", err)
	}
	if err := executor.ValidateEnv(&executor.Config{}); err != nil {
		t.Errorf("empty cfg should validate, got %v", err)
	}
}

// TestBuildAutoEnvAlwaysIncludesCI asserts CI=true is unconditional even when
// every metadata field is zero — this is the one var users always rely on.
func TestBuildAutoEnvAlwaysIncludesCI(t *testing.T) {
	got := executor.BuildAutoEnv(executor.AutoEnvMetadata{})
	if got["CI"] != "true" {
		t.Errorf("CI=true missing, got %v", got)
	}
	if len(got) != 1 {
		t.Errorf("expected only CI for empty meta, got %v", got)
	}
}

// TestBuildAutoEnvFull populates every field and asserts every expected key is
// emitted with the right value. Sequence is rendered as decimal.
func TestBuildAutoEnvFull(t *testing.T) {
	got := executor.BuildAutoEnv(executor.AutoEnvMetadata{
		RunID:      "job-42",
		Repo:       "acme/svc",
		Branch:     "feature/x",
		Sequence:   17,
		Event:      "proposal",
		BaseBranch: "main",
		ProposalID: "p-1",
	})
	want := map[string]string{
		"CI":                   "true",
		"DOCSTORE_RUN_ID":      "job-42",
		"DOCSTORE_REPO":        "acme/svc",
		"DOCSTORE_BRANCH":      "feature/x",
		"DOCSTORE_SEQUENCE":    "17",
		"DOCSTORE_EVENT":       "proposal",
		"DOCSTORE_BASE_BRANCH": "main",
		"DOCSTORE_PROPOSAL_ID": "p-1",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: want %q got %q", k, v, got[k])
		}
	}
	if len(got) != len(want) {
		t.Errorf("unexpected keys: got %v", got)
	}
}

// TestBuildAutoEnvOmitsZero confirms Sequence=0 and empty strings drop out so
// `ds ci run` (which has no sequence) emits only the fields it has.
func TestBuildAutoEnvOmitsZero(t *testing.T) {
	got := executor.BuildAutoEnv(executor.AutoEnvMetadata{
		RunID:    "local",
		Branch:   "main",
		Sequence: 0,
	})
	for _, absent := range []string{"DOCSTORE_REPO", "DOCSTORE_SEQUENCE", "DOCSTORE_EVENT", "DOCSTORE_BASE_BRANCH", "DOCSTORE_PROPOSAL_ID"} {
		if _, ok := got[absent]; ok {
			t.Errorf("%s should be absent, got %v", absent, got)
		}
	}
	if got["DOCSTORE_RUN_ID"] != "local" {
		t.Errorf("DOCSTORE_RUN_ID: want local, got %q", got["DOCSTORE_RUN_ID"])
	}
}

// TestEnvReachesStep is a buildkit-backed end-to-end check: a step that
// inspects every layer of the env merge (top-level, per-check, auto-injected,
// DOCSTORE_CHECK_NAME) and exits non-zero if any value is wrong. The step
// command does the assertions so a passing status proves the wiring.
func TestEnvReachesStep(t *testing.T) {
	exec := newExecutor(t)

	cfg := executor.Config{
		Env: map[string]string{
			"FROM_TOP":      "top",
			"OVERRIDE_ME":   "from-top",
			"SHARED_PREFIX": "global",
		},
		AutoEnv: executor.BuildAutoEnv(executor.AutoEnvMetadata{
			RunID:  "test-run",
			Branch: "main",
			Event:  "push",
		}),
		Checks: []executor.Check{{
			Name:  "ci/env",
			Image: "alpine",
			Env: map[string]string{
				"FROM_CHECK":  "check",
				"OVERRIDE_ME": "from-check",
			},
			Steps: []string{
				`[ "$FROM_TOP" = "top" ] || { echo "FROM_TOP wrong: $FROM_TOP"; exit 1; }`,
				`[ "$FROM_CHECK" = "check" ] || { echo "FROM_CHECK wrong: $FROM_CHECK"; exit 1; }`,
				`[ "$OVERRIDE_ME" = "from-check" ] || { echo "precedence broken: $OVERRIDE_ME"; exit 1; }`,
				`[ "$CI" = "true" ] || { echo "CI not set"; exit 1; }`,
				`[ "$DOCSTORE_RUN_ID" = "test-run" ] || { echo "DOCSTORE_RUN_ID wrong: $DOCSTORE_RUN_ID"; exit 1; }`,
				`[ "$DOCSTORE_BRANCH" = "main" ] || { echo "DOCSTORE_BRANCH wrong"; exit 1; }`,
				`[ "$DOCSTORE_EVENT" = "push" ] || { echo "DOCSTORE_EVENT wrong"; exit 1; }`,
				`[ "$DOCSTORE_CHECK_NAME" = "ci/env" ] || { echo "DOCSTORE_CHECK_NAME wrong: $DOCSTORE_CHECK_NAME"; exit 1; }`,
			},
		}},
	}

	results, err := exec.Run(context.Background(), "", cfg, ciconfig.TriggerContext{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Status != "passed" {
		t.Errorf("env check failed: %s\nlogs:\n%s", results[0].Status, results[0].Logs)
	}
}

// TestAutoEnvBeatsUserAttemptToShadow is a defense-in-depth assertion: even if
// validation were skipped (it isn't, in production) and a user managed to set
// CI in their Env map, AutoEnv would still win because mergeEnv applies it
// last. We bypass ValidateEnv here on purpose to exercise the runtime guarantee.
func TestAutoEnvBeatsUserAttemptToShadow(t *testing.T) {
	exec := newExecutor(t)

	cfg := executor.Config{
		Env: map[string]string{
			"CI": "false-from-user", // would have failed ValidateEnv; we're not calling it
		},
		AutoEnv: executor.BuildAutoEnv(executor.AutoEnvMetadata{}),
		Checks: []executor.Check{{
			Name:  "ci/auto-wins",
			Image: "alpine",
			Steps: []string{
				`[ "$CI" = "true" ] || { echo "user shadowed CI: $CI"; exit 1; }`,
			},
		}},
	}

	results, err := exec.Run(context.Background(), "", cfg, ciconfig.TriggerContext{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 1 || results[0].Status != "passed" {
		t.Errorf("expected pass, got %+v", results)
	}
}
