# CI Runner

The CI runner is a standalone HTTP service (`cmd/ci-runner`) that executes `.docstore/ci.yaml` checks against source trees using [BuildKit](https://github.com/moby/buildkit) as the execution engine. It is the core of the docstore CI system (issue #75, Milestone 0.5).

## Overview

The runner sits between docstore and a co-located `buildkitd` instance. It accepts a source directory and a check configuration over HTTP, translates each check into an LLB DAG, dispatches the build to BuildKit, collects logs, and returns pass/fail results synchronously.

In the full production flow (Milestone 1):
1. A commit lands on a branch
2. The runner fetches `.docstore/ci.yaml` from `main` and downloads the branch tree to disk
3. The runner calls itself (`POST /run`) with the local source path
4. Results are written back to docstore as check runs via `POST /-/check`

Milestone 0.5 (what is implemented) covers step 3 only — the executor and HTTP API, with no docstore integration.

## How It Works

### `.docstore/ci.yaml` DSL

Committed to `main` (always read from `main`, never from the branch under test):

```yaml
checks:
  - name: ci/build
    image: golang:1.25
    steps:
      - go build ./...
      - go vet ./...

  - name: ci/test
    image: golang:1.25
    steps:
      - go test -race ./...

  - name: ci/lint
    image: golangci/golangci-lint:latest
    steps:
      - golangci-lint run
```

Fields:
- **`name`** — check run name posted to docstore; conventionally namespaced with `/` (e.g. `ci/build`)
- **`image`** — any pullable Docker image
- **`steps`** — ordered shell commands run sequentially inside the image with source mounted at `/src`; a non-zero exit fails the check and skips remaining steps

No matrix, conditionals, or artifact config in v1.

### LLB Translation

`internal/executor` (`executor.go:78`) translates each check into an independent LLB chain. The source directory is mounted as a local input named `"src"`.

For each check, the executor:
1. Starts from the check's container image with working directory `/src`
2. Chains each step as a `Run` vertex with the source mounted at `/src`
3. **Threads `srcMount` forward between steps** — each step receives the output `/src` from the previous step as its input mount, so in-source changes (e.g. `go generate` producing files) persist across steps within one check
4. Marshals the final DAG and dispatches to buildkitd

Key snippet (`executor.go:83-90`):
```go
state := llb.Image(check.Image).Dir("/src")
srcMount := source  // llb.Local("src")

for _, step := range check.Steps {
    exec := state.Run(
        llb.Args([]string{"sh", "-c", step}),
        llb.AddMount("/src", srcMount),
    )
    state = exec.Root()
    srcMount = exec.GetMount("/src")  // carry forward for next step
}
```

Each check is independent — checks do not share filesystem state with each other.

### BuildKit Dispatch and Log Collection

After marshaling the LLB definition, the executor calls `client.Solve` with the local source directory mapped to the `"src"` input (`executor.go:116-118`). BuildKit uploads the source once (content-addressed) and executes the DAG.

Logs are collected from the `SolveStatus` stream in a goroutine (`executor.go:107-113`): every `VertexLog` entry has its `.Data` bytes appended to a buffer. After `Solve` returns, the buffer is the complete combined stdout+stderr for all steps in that check. Both stdout and stderr are captured.

If `Solve` returns an error and no log bytes were collected, the error message is used as the log value.

## HTTP API

### `POST /run`

**Request:**
```json
{
  "source_dir": "/absolute/path/to/source",
  "config": {
    "checks": [
      {
        "name": "ci/build",
        "image": "golang:1.25",
        "steps": ["go build ./...", "go vet ./..."]
      },
      {
        "name": "ci/test",
        "image": "golang:1.25",
        "steps": ["go test -race ./..."]
      }
    ]
  }
}
```

Validation applied before execution:
- `source_dir` is required, must be an absolute path, and must exist on disk
- Every check must have a non-empty `image`
- Every check must have at least one step

**Response** (synchronous — waits for all checks):
```json
{
  "checks": [
    {"name": "ci/build", "status": "passed", "logs": "ok\tgithub.com/..."},
    {"name": "ci/test",  "status": "failed", "logs": "FAIL: TestFoo ..."}
  ]
}
```

`status` is `"passed"` or `"failed"`. The response is only sent after all checks have completed (including failing ones).

**curl example:**
```bash
curl -s -X POST http://localhost:8080/run \
  -H "Content-Type: application/json" \
  -d '{
    "source_dir": "/tmp/my-source",
    "config": {
      "checks": [
        {
          "name": "ci/hello",
          "image": "alpine",
          "steps": ["echo hello"]
        }
      ]
    }
  }' | jq .
```

**Error responses:**
- `400 Bad Request` — missing/invalid request fields (body includes a plain-text reason)
- `500 Internal Server Error` — executor failure

## Running Locally

### Prerequisites

Install and start `buildkitd`:

```bash
# macOS via Homebrew
brew install buildkit

# Start buildkitd (needs root or appropriate permissions)
sudo buildkitd &
# or use the default socket location:
# sudo buildkitd --addr unix:///run/buildkit/buildkitd.sock
```

On Linux you can also run:
```bash
sudo buildkitd --oci-worker-snapshotter=native &
```

Wait for the socket to appear before starting the runner:
```bash
until [ -S /run/buildkit/buildkitd.sock ]; do sleep 0.1; done
```

### Start the Runner

```bash
go run ./cmd/ci-runner \
  --buildkit-addr unix:///run/buildkit/buildkitd.sock \
  --port 8080
```

Flags:

| Flag | Default | Description |
|---|---|---|
| `--buildkit-addr` | `unix:///run/buildkit/buildkitd.sock` | buildkitd socket address |
| `--port` | `8080` | HTTP listen port |

Log format defaults to JSON. Set `LOG_FORMAT=text` for human-readable output:
```bash
LOG_FORMAT=text go run ./cmd/ci-runner
```

### Example Request

```bash
# Create a temp source directory with a Go module
mkdir -p /tmp/ci-test
cat > /tmp/ci-test/main.go << 'EOF'
package main
func main() {}
EOF
cat > /tmp/ci-test/go.mod << 'EOF'
module example.com/test
go 1.21
EOF

# Run a check
curl -s -X POST http://localhost:8080/run \
  -H "Content-Type: application/json" \
  -d '{
    "source_dir": "/tmp/ci-test",
    "config": {
      "checks": [
        {
          "name": "ci/build",
          "image": "golang:1.21",
          "steps": ["go build ./..."]
        }
      ]
    }
  }' | jq .
```

The server handles `SIGINT`/`SIGTERM` with a 10-second graceful shutdown window.

## Running Tests

### Unit Tests (no buildkitd required)

There are currently no pure unit tests in `internal/executor` — all tests are integration tests that require a running `buildkitd`.

### Integration Tests

Tests in `internal/executor/executor_test.go` skip automatically when `buildkitd` is unavailable. They require either:
- `BUILDKIT_ADDR` env var set to the socket address, or
- `/run/buildkit/buildkitd.sock` present

With buildkitd running:

```bash
# Use default socket
go test ./internal/executor/... -count=1 -v

# Or specify socket explicitly
BUILDKIT_ADDR=unix:///run/buildkit/buildkitd.sock \
  go test ./internal/executor/... -count=1 -v
```

The integration tests cover:
- `TestPass` — single step that succeeds; verifies `status: passed` and log output
- `TestFail` — single step that exits 1; verifies `status: failed`
- `TestMultiCheck` — two checks in parallel (one pass, one fail); verifies independent results
- `TestLogCapture` — two steps writing to stdout and stderr; verifies both streams captured

These tests pull the `alpine` image from Docker Hub — first run requires internet access.

### Full Test Suite

```bash
go test ./... -count=1
go build ./...
go vet ./...
```

## Architecture Notes

### Step Chaining (`srcMount` threading)

Within a single check, `srcMount` is threaded forward through each step (`executor.go:90`: `srcMount = exec.GetMount("/src")`). This means the `/src` directory seen by step N is the output of step N-1, not the original source. Files created or modified by earlier steps (e.g. generated code, compiled artifacts) are visible to later steps in the same check.

Each check starts from the original `llb.Local("src")`, so checks do not affect each other's source view.

### Parallel Check Execution

All checks in `Config.Checks` are dispatched concurrently via goroutines (`executor.go:52-67`). Each goroutine calls `runCheck`, which issues its own `client.Solve` call. BuildKit can execute multiple solves in parallel on the same daemon. Results are written to a pre-allocated slice indexed by position, so result ordering matches input ordering.

Panics inside goroutines are recovered and turned into `status: failed` results with the panic message as the log.

### Synchronous Response Model

`POST /run` blocks until all checks complete. The HTTP server intentionally omits `WriteTimeout` (`main.go:91-95`) since long-running builds must not be cut off by a server-side write deadline. Execution timeout is controlled by the request context (i.e. the caller's HTTP client timeout or a context deadline passed in via the request).

### Logging

The server uses `log/slog` with structured JSON output by default (set `LOG_FORMAT=text` for text format). All log entries go to stdout at `INFO` level or above.
