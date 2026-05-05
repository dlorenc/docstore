package executor_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/dlorenc/docstore/internal/executor"
)

// parseChecks parses a YAML doc with `checks: [...]` into the executor.Config.
// All secrets parsing tests share this helper so they exercise the same path
// the CLI (internal/cli/ci.go) and ciconfig parsing pipeline use in production.
func parseChecks(t *testing.T, src string) (executor.Config, error) {
	t.Helper()
	var cfg executor.Config
	err := yaml.Unmarshal([]byte(src), &cfg)
	return cfg, err
}

// TestSecretsParseSimple verifies that a bare scalar entry expands to a
// SecretRequest where LocalName equals RepoName.
func TestSecretsParseSimple(t *testing.T) {
	cfg, err := parseChecks(t, `
checks:
  - name: build
    image: alpine
    steps: ["echo hi"]
    secrets:
      - FOO
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []executor.SecretRequest{{LocalName: "FOO", RepoName: "FOO"}}
	if !reflect.DeepEqual(cfg.Checks[0].Secrets, want) {
		t.Errorf("got %+v, want %+v", cfg.Checks[0].Secrets, want)
	}
}

// TestSecretsParseRename verifies the single-key mapping form expands to
// LocalName=key, RepoName=value.
func TestSecretsParseRename(t *testing.T) {
	cfg, err := parseChecks(t, `
checks:
  - name: build
    image: alpine
    steps: ["echo hi"]
    secrets:
      - SLACK_WEBHOOK_URL: SLACK_INCOMING
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []executor.SecretRequest{{LocalName: "SLACK_WEBHOOK_URL", RepoName: "SLACK_INCOMING"}}
	if !reflect.DeepEqual(cfg.Checks[0].Secrets, want) {
		t.Errorf("got %+v, want %+v", cfg.Checks[0].Secrets, want)
	}
}

// TestSecretsParseMixed verifies both forms can coexist in a single block.
func TestSecretsParseMixed(t *testing.T) {
	cfg, err := parseChecks(t, `
checks:
  - name: build
    image: alpine
    steps: ["echo hi"]
    secrets:
      - DOCKERHUB_TOKEN
      - SLACK_WEBHOOK_URL: SLACK_INCOMING
      - GITHUB_TOKEN
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []executor.SecretRequest{
		{LocalName: "DOCKERHUB_TOKEN", RepoName: "DOCKERHUB_TOKEN"},
		{LocalName: "SLACK_WEBHOOK_URL", RepoName: "SLACK_INCOMING"},
		{LocalName: "GITHUB_TOKEN", RepoName: "GITHUB_TOKEN"},
	}
	if !reflect.DeepEqual(cfg.Checks[0].Secrets, want) {
		t.Errorf("got %+v, want %+v", cfg.Checks[0].Secrets, want)
	}
}

// TestSecretsParseMalformed table-tests every shape we explicitly reject.
// Each substring assertion is loose so wording can change without breaking the
// test, but tight enough that an unrelated error doesn't pass silently.
func TestSecretsParseMalformed(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantSub string
	}{
		{
			name: "empty scalar",
			yaml: `
checks:
  - name: build
    image: alpine
    steps: ["echo hi"]
    secrets:
      - ""
`,
			wantSub: "must not be empty",
		},
		{
			name: "sequence entry",
			yaml: `
checks:
  - name: build
    image: alpine
    steps: ["echo hi"]
    secrets:
      - [a, b]
`,
			wantSub: "string or single-key mapping",
		},
		{
			name: "mapping with two keys",
			yaml: `
checks:
  - name: build
    image: alpine
    steps: ["echo hi"]
    secrets:
      - {A: X, B: Y}
`,
			wantSub: "exactly one key",
		},
		{
			name: "non-string value (int)",
			yaml: `
checks:
  - name: build
    image: alpine
    steps: ["echo hi"]
    secrets:
      - LOCAL: 42
`,
			wantSub: "value must be a string",
		},
		{
			name: "non-string value (sequence)",
			yaml: `
checks:
  - name: build
    image: alpine
    steps: ["echo hi"]
    secrets:
      - LOCAL: [a, b]
`,
			wantSub: "value must be a string",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseChecks(t, tc.yaml)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestValidateSecretsBadNameShapes covers every regex-rejection axis: case,
// leading character, allowed chars, length boundary, empty.
func TestValidateSecretsBadNameShapes(t *testing.T) {
	cases := []struct {
		name  string
		local string
		repo  string
	}{
		{"lowercase local", "foo", "FOO"},
		{"lowercase repo", "FOO", "foo"},
		{"leading digit local", "1FOO", "FOO"},
		{"leading underscore local", "_FOO", "FOO"},
		{"hyphen local", "FOO-BAR", "FOO"},
		{"hyphen repo", "FOO", "FOO-BAR"},
		{"too long local", "A" + strings.Repeat("B", 64), "FOO"}, // 65 chars
		{"empty local", "", "FOO"},
		{"empty repo", "FOO", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &executor.Config{Checks: []executor.Check{{
				Name: "c", Image: "alpine", Steps: []string{"echo"},
				Secrets: []executor.SecretRequest{{LocalName: tc.local, RepoName: tc.repo}},
			}}}
			if err := executor.ValidateSecretsAllowlist(cfg); err == nil {
				t.Errorf("expected error for local=%q repo=%q", tc.local, tc.repo)
			}
		})
	}
}

// TestValidateSecretsAllowsValidNames asserts the regex's positive boundaries:
// 64-char names, all-uppercase, digits-after-letter, underscores all pass.
func TestValidateSecretsAllowsValidNames(t *testing.T) {
	good := []string{
		"A",
		"FOO",
		"FOO_BAR",
		"FOO123",
		"A_1_2_3",
		"A" + strings.Repeat("B", 63), // 64 chars exactly
	}
	for _, name := range good {
		t.Run(name, func(t *testing.T) {
			cfg := &executor.Config{Checks: []executor.Check{{
				Name: "c", Image: "alpine", Steps: []string{"echo"},
				Secrets: []executor.SecretRequest{{LocalName: name, RepoName: name}},
			}}}
			if err := executor.ValidateSecretsAllowlist(cfg); err != nil {
				t.Errorf("expected %q to validate, got %v", name, err)
			}
		})
	}
}

// TestValidateSecretsReservedPrefix rejects DOCSTORE_* on either side. The
// case-insensitive lowercase variant doesn't even pass the regex, but the
// uppercase form is regex-valid and only the prefix check stops it.
func TestValidateSecretsReservedPrefix(t *testing.T) {
	cases := []struct {
		name  string
		local string
		repo  string
	}{
		{"prefix on local", "DOCSTORE_FOO", "FOO"},
		{"prefix on repo", "FOO", "DOCSTORE_FOO"},
		{"prefix exactly", "DOCSTORE_", "FOO"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &executor.Config{Checks: []executor.Check{{
				Name: "c", Image: "alpine", Steps: []string{"echo"},
				Secrets: []executor.SecretRequest{{LocalName: tc.local, RepoName: tc.repo}},
			}}}
			err := executor.ValidateSecretsAllowlist(cfg)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), "DOCSTORE_") {
				t.Errorf("expected error to mention DOCSTORE_ prefix, got %v", err)
			}
		})
	}
}

// TestValidateSecretsDuplicateLocal rejects two entries with the same local
// name in a single check (path collision under /run/secrets).
func TestValidateSecretsDuplicateLocal(t *testing.T) {
	cfg := &executor.Config{Checks: []executor.Check{{
		Name: "build", Image: "alpine", Steps: []string{"echo"},
		Secrets: []executor.SecretRequest{
			{LocalName: "FOO", RepoName: "BAR"},
			{LocalName: "FOO", RepoName: "BAZ"},
		},
	}}}
	err := executor.ValidateSecretsAllowlist(cfg)
	if err == nil {
		t.Fatalf("expected duplicate-local error, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate local name") {
		t.Errorf("expected duplicate-local in %v", err)
	}
}

// TestValidateSecretsSameLocalAcrossChecks: two checks each have a "FOO"
// local — that's fine because each check has its own /run/secrets namespace.
func TestValidateSecretsSameLocalAcrossChecks(t *testing.T) {
	cfg := &executor.Config{Checks: []executor.Check{
		{Name: "a", Image: "alpine", Steps: []string{"echo"}, Secrets: []executor.SecretRequest{{LocalName: "FOO", RepoName: "FOO"}}},
		{Name: "b", Image: "alpine", Steps: []string{"echo"}, Secrets: []executor.SecretRequest{{LocalName: "FOO", RepoName: "FOO"}}},
	}}
	if err := executor.ValidateSecretsAllowlist(cfg); err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

// TestValidateSecretsErrorsJoin asserts the validator surfaces every problem
// in one shot, not just the first. Three independent issues across two checks
// should all appear in the joined error.
func TestValidateSecretsErrorsJoin(t *testing.T) {
	cfg := &executor.Config{Checks: []executor.Check{
		{
			Name: "alpha", Image: "alpine", Steps: []string{"echo"},
			Secrets: []executor.SecretRequest{
				{LocalName: "lowercase", RepoName: "OK"},          // bad local
				{LocalName: "GOOD", RepoName: "DOCSTORE_BUILTIN"}, // reserved repo
			},
		},
		{
			Name: "beta", Image: "alpine", Steps: []string{"echo"},
			Secrets: []executor.SecretRequest{
				{LocalName: "DUP", RepoName: "X"},
				{LocalName: "DUP", RepoName: "Y"}, // duplicate local
			},
		},
	}}
	err := executor.ValidateSecretsAllowlist(cfg)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}

	// errors.Join produces an error implementing Unwrap() []error. Verify all
	// three sub-errors are reachable, not just present in the formatted string.
	type unwrapper interface{ Unwrap() []error }
	u, ok := err.(unwrapper)
	if !ok {
		t.Fatalf("expected joined error, got %T", err)
	}
	subs := u.Unwrap()
	if len(subs) != 3 {
		t.Errorf("expected 3 sub-errors, got %d: %v", len(subs), subs)
	}

	msg := err.Error()
	for _, want := range []string{`"lowercase"`, "DOCSTORE_", "duplicate local name"} {
		if !strings.Contains(msg, want) {
			t.Errorf("joined error missing %q; full: %s", want, msg)
		}
	}
}

// TestValidateSecretsNilConfigOK guards the nil-pointer fast path so callers
// can invoke ValidateSecretsAllowlist before knowing whether parsing produced
// a config at all.
func TestValidateSecretsNilConfigOK(t *testing.T) {
	if err := executor.ValidateSecretsAllowlist(nil); err != nil {
		t.Errorf("expected nil error for nil config, got %v", err)
	}
}

// TestSecretsJSONRoundTrip locks the JSON wire format. The HTTP path is the
// only place a Check's secrets cross a process boundary, so we need marshal
// and unmarshal to be exact inverses.
func TestSecretsJSONRoundTrip(t *testing.T) {
	original := executor.Check{
		Name:  "build",
		Image: "alpine",
		Steps: []string{"echo hi"},
		Secrets: []executor.SecretRequest{
			{LocalName: "FOO", RepoName: "FOO"},
			{LocalName: "SLACK_WEBHOOK_URL", RepoName: "SLACK_INCOMING"},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Pin the wire format so accidental tag changes break this test.
	if !strings.Contains(string(data), `"local_name":"FOO"`) ||
		!strings.Contains(string(data), `"repo_name":"FOO"`) {
		t.Errorf("unexpected JSON wire format: %s", data)
	}

	var got executor.Check
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, original) {
		t.Errorf("roundtrip mismatch:\n got  %+v\n want %+v", got, original)
	}
}

// TestSecretsOmitEmpty asserts the zero-length slice is omitted from JSON,
// matching the omitempty tag and avoiding bloating wire payloads for the
// common no-secrets case.
func TestSecretsOmitEmpty(t *testing.T) {
	c := executor.Check{Name: "x", Image: "alpine", Steps: []string{"echo"}}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "secrets") {
		t.Errorf("expected secrets to be omitted, got: %s", data)
	}
}

// Compile-time guard: errors.Join with no errors must return nil. This is
// part of stdlib but the validator relies on it for the happy path.
var _ = errors.Join
