# CI Runner

The CI runner is a standalone HTTP service (`cmd/ci-runner`) that executes `.docstore/ci.yaml` checks against source trees using [BuildKit](https://github.com/moby/buildkit) as the execution engine. It is the core of the docstore CI system (issue #75, Milestone 0.5).

## Overview

The runner sits between docstore and a co-located `buildkitd` instance. It accepts a source directory and a check configuration over HTTP, translates each check into an LLB DAG, dispatches the build to BuildKit, collects logs, and returns pass/fail results synchronously.

In the full production flow:
1. A commit lands on a branch
2. Docstore's outbox dispatcher sends a `commit.created` CloudEvent to the ci-runner webhook
3. The ci-runner fetches `.docstore/ci.yaml` from `main` and downloads the branch tree to disk
4. BuildKit executes the checks; logs are written to GCS
5. Results are written back to docstore as check runs via `POST /-/check`

**Production deployment:** ci-runner runs on GKE Autopilot (cluster `chainguardener`, namespace `docstore-ci`) using rootless buildkitd (`moby/buildkit-rootless`). The docstore Cloud Run service delivers webhook events to ci-runner's internal GKE LoadBalancer (IP `10.128.0.40`) via VPC Direct Egress. Logs are stored at `gs://docstore-ci-logs`. See [GKE Deployment](#gke-deployment) for details.

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

`internal/executor` (`executor.go`) translates each check into an independent LLB chain using the BuildKit gateway API (`client.Build`). The source directory is mounted as a local input named `"src"`.

**Image ENV injection (gateway API):** The executor uses `client.Build` rather than `client.Solve` so it can call `ResolveImageConfig` to fetch the image's OCI config and extract its `Env` array. These ENV variables are then injected as `llb.AddEnv` `RunOption`s on every exec. This is required because `--oci-worker-no-process-sandbox` (needed for rootless buildkitd on GKE Autopilot, where `SYS_ADMIN` is blocked) does not apply the image's ENV automatically. Without this fix, tools like `go` are not found at runtime because the image's `PATH` is never set.

For each check, the executor:
1. Resolves the image config via `ResolveImageConfig` to extract ENV variables
2. Starts from the check's container image with working directory `/src`
3. Chains each step as a `Run` vertex with the source mounted at `/src` and all image ENV variables applied
4. **Threads `srcMount` forward between steps** — each step receives the output `/src` from the previous step as its input mount, so in-source changes (e.g. `go generate` producing files) persist across steps within one check
5. Marshals the final DAG and dispatches via the gateway solve

Key snippet:
```go
// Resolve image ENV so tools like `go` are on PATH under --oci-worker-no-process-sandbox.
imageRef := check.Image
if named, err := reference.ParseNormalizedNamed(check.Image); err == nil {
    imageRef = reference.TagNameOnly(named).String()
}
var imageEnv []string
if _, _, cfgBytes, err := c.ResolveImageConfig(ctx, imageRef, ...); err == nil {
    var imgCfg specs.Image
    if json.Unmarshal(cfgBytes, &imgCfg) == nil {
        imageEnv = imgCfg.Config.Env
    }
}
envOpts := make([]llb.RunOption, 0, len(imageEnv))
for _, env := range imageEnv {
    k, v, _ := strings.Cut(env, "=")
    envOpts = append(envOpts, llb.AddEnv(k, v))
}

state := llb.Image(check.Image).Dir("/src")
srcMount := source  // llb.Local("src")

for _, step := range check.Steps {
    runOpts := []llb.RunOption{
        llb.Args([]string{"sh", "-c", step}),
        llb.AddMount("/src", srcMount),
    }
    runOpts = append(runOpts, envOpts...)
    exec := state.Run(runOpts...)
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

### `POST /webhook` (Milestone 2)

Receives CloudEvents webhook deliveries from docstore's outbox dispatcher.

**Authentication:** Verified via `X-DocStore-Signature: sha256=<hex>` HMAC header using the `--webhook-secret` flag. If no secret is configured, the header is not checked.

**Supported event types:**
- `com.docstore.commit.created` — extracts `repo`, `branch`, `sequence` from the data field and calls `POST /run` internally
- All other types — acknowledged with `200 OK` and ignored (forward-compat)

**Responses:**
- `200 OK` — event accepted (or ignored)
- `400 Bad Request` — invalid signature or malformed body

---

### `GET /run/{run_id}` (Milestone 2)

Returns the current status of an async CI run. Runs are tracked in-memory; history is lost on restart.

**Response:**
```json
{"run_id": "...", "state": "running", "repo": "...", "branch": "...", "head_seq": 42, "started_at": "..."}
{"run_id": "...", "state": "done",    "checks": [{"name":"ci/build","status":"passed","logs":"..."}]}
{"run_id": "...", "state": "failed",  "error": "config fetch failed: ..."}
```

**States:** `running`, `done`, `failed`

**Error responses:**
- `404 Not Found` — unknown run_id

---

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
  --docstore-url http://localhost:8000 \
  --port 8080
```

Flags:

| Flag | Default | Description |
|---|---|---|
| `--buildkit-addr` | `unix:///run/buildkit/buildkitd.sock` | buildkitd socket address |
| `--port` | `8080` | HTTP listen port |
| `--docstore-url` | (required) | Base URL of the docstore server |
| `--dev-identity` | `""` | Identity header for local dev (sets X-Goog-IAP-JWT-Assertion) |
| `--run-timeout` | `30m` | Maximum duration for a single CI run |
| `--runner-url` | `""` | Public URL of this runner (enables auto-registration with docstore) |
| `--webhook-secret` | `""` | HMAC secret for verifying incoming webhook deliveries |

Environment variables (log storage):

| Variable | Default | Description |
|---|---|---|
| `LOG_STORE` | `local` | Log store backend: `local` or `gcs` |
| `LOG_LOCAL_DIR` | `/tmp/ci-logs` | Directory for local log store (when `LOG_STORE=local`) |
| `LOG_BUCKET` | (required when gcs) | GCS bucket name (when `LOG_STORE=gcs`) |

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

### End-to-End Tests

The e2e test (`cmd/ci-runner/e2e_test.go`) requires Docker to start real postgres, docstore, and buildkitd containers. It is tagged `//go:build e2e` and excluded from the default test run.

```bash
# Run the e2e test (requires Docker)
go test ./cmd/ci-runner/ -tags=e2e -run TestE2EGoPipeline -v -timeout=10m
```

The test (`TestE2EGoPipeline`):
1. Starts a Postgres container via testcontainers and runs docstore migrations
2. Starts a real docstore HTTP server wired to the database
3. Starts a testcontainers buildkitd instance
4. Creates an org, repo, commits `.docstore/ci.yaml` (using `golang:1.25`) and the `test/hello` source tree to `main`, then creates a feature branch
5. Registers a webhook subscription pointing at the in-process ci-runner
6. Posts a commit to the feature branch to trigger `commit.created` via the outbox dispatcher
7. Polls docstore `GET /repos/{repo}/-/branch/{branch}/checks` until all three checks (`ci/build`, `ci/test`, `ci/vet`) complete with `passed`

## GKE Deployment

The production ci-runner runs on GKE Autopilot rather than Cloud Run because buildkitd requires Linux user namespaces (`SYS_ADMIN`) that Autopilot blocks. The rootless buildkitd image (`moby/buildkit:v0.29.0-rootless`) works around this using `rootlesskit`, which sets up mount namespaces inside a user namespace.

### Components

| File | Purpose |
|---|---|
| `Dockerfile.ci-runner-gke` | Two-stage build: Go binary + rootless buildkitd image |
| `entrypoint-gke.sh` | Configures GCR auth, starts rootlesskit buildkitd, then starts ci-runner |
| `deploy/k8s/ci-runner.yaml` | GKE Deployment + internal LoadBalancer + Workload Identity SA |
| `scripts/setup-gke.sh` | One-time GKE setup (Workload Identity, secrets, IAM) |

### Network topology

```
Cloud Run (docstore)
  └─[VPC Direct Egress]─► internal LB 10.128.0.40:8080
                                 └─► ci-runner pod (docstore-ci namespace)
                                           └─► rootlesskit buildkitd (tcp://localhost:1234)
```

Docstore uses VPC Direct Egress to reach the GKE internal LoadBalancer IP. The ci-runner is not exposed to the public internet.

### Security

- **Workload Identity:** The `ci-runner` k8s ServiceAccount is bound to the `ci-runner@dlorenc-chainguard.iam.gserviceaccount.com` GCP SA via `iam.workloadIdentityUser`. This grants access to GCS log storage without static credentials.
- **seccompProfile / AppArmor:** Both must be `Unconfined` for rootlesskit to set up mount namespaces. This is configured in `deploy/k8s/ci-runner.yaml`.
- **Webhook HMAC:** The ci-runner verifies the `X-DocStore-Signature` header on all incoming webhooks using the `WEBHOOK_SECRET` env var (sourced from a k8s Secret backed by Secret Manager).

### First-time setup

```bash
# One-time setup (idempotent):
bash scripts/setup.sh          # GCP SA, GCS bucket, Secret Manager secrets, IAM
bash scripts/setup-gke.sh      # Workload Identity binding, k8s namespace and Secret

# Build and push the image:
docker build -f Dockerfile.ci-runner-gke \
  -t us-central1-docker.pkg.dev/dlorenc-chainguard/images/ci-runner-gke:latest .
docker push us-central1-docker.pkg.dev/dlorenc-chainguard/images/ci-runner-gke:latest

# Deploy:
kubectl apply -f deploy/k8s/ci-runner.yaml
kubectl rollout status deployment/ci-runner -n docstore-ci
```

After deploy, the `build-and-deploy-ci-runner` CI job in `deploy.yml` handles subsequent deploys automatically on every push to `main`. It also updates the `ci-runner-url` Secret Manager secret with the internal LB IP if it has changed.

### rootlesskit flags

`entrypoint-gke.sh` starts buildkitd with:
- `--oci-worker-snapshotter=native` — uses overlay-less snapshotter compatible with Autopilot
- `--oci-worker-no-process-sandbox` — disables the per-process sandbox (required because there is no seccomp/apparmor inside the user namespace); the image ENV fix in `executor.go` compensates for the side effect of this flag not applying image ENV

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
