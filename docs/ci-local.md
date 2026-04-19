# Local CI with `ds ci run`

`ds ci run` is a local CI emulator that runs your project's CI checks in the
exact same BuildKit environment as presubmit CI — no docstore server required.
Use it to verify CI will pass before pushing, iterate quickly on failing checks,
or test trigger-specific behavior from your working directory.

## Usage

```
ds ci run [--check <name>] [--trigger push|proposal|proposal_synchronized] [--base-branch <branch>] [--buildkit <addr>]
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--check <name>` | (all checks) | Run only the named check — for tight iteration on a single failing check |
| `--trigger <type>` | `push` | Trigger type used when evaluating `if:` expressions |
| `--base-branch <branch>` | `""` | Base branch for `proposal`/`proposal_synchronized` trigger context |
| `--buildkit <addr>` | `$BUILDKIT_ADDR` or `tcp://localhost:1234` | buildkitd address |

## How it works

1. Reads `.docstore/ci.yaml` from the current working directory (no network call).
2. Reads the current branch name from the local workspace state (same as `ds status`).
3. Passes the working directory as the source for BuildKit — **uncommitted changes are included**. This is intentional: the point is to test what you're currently working on.
4. Runs all checks in parallel via BuildKit, using the same LLB DAG as production CI.
5. Streams logs to stdout per check, prefixed with the check name.
6. Prints `[pass]` / `[fail]` per check at completion and an overall summary.
7. Exits 0 if all checks pass, 1 if any fail.

No results are posted to the docstore server. Everything is purely local.

## Prerequisites

You need a running buildkitd instance. Options:

- **Docker Desktop**: use the bundled buildkitd socket:
  ```
  ds ci run --buildkit unix:///run/buildkit/buildkitd.sock
  ```
- **Standalone buildkitd**: start with `buildkitd` and use the default `tcp://localhost:1234`.
- **Custom address**: pass `--buildkit <addr>` or set the `BUILDKIT_ADDR` environment variable.

## Example output

```
$ ds ci run
running 3 checks on branch feature/auth (trigger: push)

[lint]  running...
[test]  running...
[build]  running...

[lint]  ./internal/auth/auth.go:42:5: error: unused variable 'x'
[lint]  [fail]
[build]  [pass]
[test]  ok  github.com/dlorenc/docstore/internal/auth  3.2s
[test]  [pass]

2 passed, 1 failed
```

Exit status 1 (one or more checks failed).

## Running a single check

Use `--check` to run only one check for fast iteration:

```
$ ds ci run --check lint
running 1 check on branch feature/auth (trigger: push)

[lint]  running...

[lint]  [pass]

1 passed, 0 failed
```

## Testing proposal trigger behavior

If your `ci.yaml` uses `if:` expressions that depend on the trigger type (e.g.
`if: trigger == "proposal"`), you can simulate a proposal trigger:

```
$ ds ci run --trigger proposal --base-branch main
```

If you specify `--trigger proposal` or `--trigger proposal_synchronized` without
`--base-branch`, a warning is printed because `base_branch` will be empty, which
may cause `if:` expressions that reference it to behave differently than in CI.

## Uncommitted changes

Unlike production CI (which tests the committed branch head), `ds ci run` uses
your working directory as-is. Uncommitted changes are included. This is by
design — the purpose is to test what you're working on right now, not the last
commit.

To test exactly what CI would test on push, commit your changes first with
`ds commit -m "..."` and then run `ds ci run`.
