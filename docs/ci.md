# CI System

DocStore has a built-in CI system. When a commit is pushed to any branch, the server emits a `com.docstore.commit.created` webhook event. The CI system picks up the event, fetches the repo source, and runs the checks defined in `.docstore/ci.yaml` on `main`.

## Architecture

```
docstore server
    │ POST /subscriptions webhook
    │ X-DocStore-Signature: sha256=<hmac>
    ▼
ci-scheduler (GKE, 1 replica)
    │ INSERT ci_jobs (status=queued)
    │ reap stale claimed jobs every 30s
    ▼
PostgreSQL ci_jobs table
    │ SELECT FOR UPDATE SKIP LOCKED
    ▼
ci-worker pods (GKE, 3 replicas, Kata CLH microVMs)
    │ claim job (status=claimed)
    │ heartbeat every 30s
    │ fetch .docstore/ci.yaml from main
    │ download branch source tar.gz
    │ run checks via BuildKit LLB
    │ upload logs to GCS
    │ POST /repos/{repo}/-/check for each result
    └─ exit (pod replaced by Deployment controller)
```

## Components

### ci-scheduler (`cmd/ci-scheduler`)

A lightweight HTTP service that:

- Receives webhook deliveries from the docstore outbox at `POST /webhook`.
- Verifies the HMAC-SHA256 signature (`X-DocStore-Signature: sha256=<hex>`).
- Parses `com.docstore.commit.created` events and inserts a row into `ci_jobs`.
- Serves job status at `GET /run/{id}`.
- Proxies live logs at `GET /run/{id}/logs/{check}` — either reverse-proxying to the worker pod (while the job is claimed) or redirecting to the GCS log URL (after completion).
- Provides a manual trigger at `POST /run`.
- Runs a stale-job reaper every 30 seconds to reclaim jobs that have missed their heartbeat.

**Flags / environment variables:**

| Flag | Env var | Description |
|---|---|---|
| `-port` | — | HTTP listen port (default `8080`) |
| `-docstore-url` | `DOCSTORE_URL` | Base URL of the docstore server |
| `-scheduler-url` | `RUNNER_URL` | Public URL of this scheduler (used to auto-register the webhook subscription) |
| `-webhook-secret` | `WEBHOOK_SECRET` | HMAC secret for webhook signature verification |
| — | `DATABASE_URL` | PostgreSQL DSN (required) |
| — | `LOG_LEVEL` | Log level |
| — | `LOG_FORMAT` | `json` (default) or `text` |

On startup, if `DOCSTORE_URL`, `RUNNER_URL`, and `WEBHOOK_SECRET` are all set, the scheduler self-registers a webhook subscription with the docstore server.

### ci-worker (`cmd/ci-worker`)

A long-lived process that:

1. Waits for buildkitd (tcp://localhost:1234) and dockerd (tcp://localhost:2375) to be ready (up to 5 minutes each).
2. Polls the `ci_jobs` table with `SELECT FOR UPDATE SKIP LOCKED` to claim one job.
3. Sends a heartbeat to the database every 30 seconds while the job runs.
4. Executes the job (see below).
5. Writes the final status and log URL back to `ci_jobs`.
6. Exits. The Kubernetes Deployment controller creates a fresh pod for the next job.

**Required environment variables:**

| Variable | Description |
|---|---|
| `DATABASE_URL` | PostgreSQL DSN |
| `DOCSTORE_URL` | Base URL of the docstore server |
| `POD_NAME` | Kubernetes pod name (injected via downward API) |
| `POD_IP` | Kubernetes pod IP (injected via downward API) |
| `BUILDKIT_ADDR` | buildkitd address (default `tcp://localhost:1234`) |
| `LOG_STORE` | Log backend: `gcs` or `local` |
| `LOG_BUCKET` | GCS bucket for logs (required when `LOG_STORE=gcs`) |

The worker serves live logs on port 8081 at `GET /logs/{check}`. The scheduler's log proxy endpoint reverse-proxies to this port while the job is running.

### Entrypoint (`entrypoint-worker.sh`)

The Dockerfile.ci-worker sets this script as the container entrypoint. It:

1. Fetches a GCP workload identity token from the instance metadata server and writes `~/.docker/config.json` to authenticate buildkitd against GCR and Artifact Registry.
2. Creates loop device nodes (`/dev/loop0` through `/dev/loop7`) manually — udev does not run inside Kata VMs.
3. Creates a 20 GiB sparse file, formats it as ext4, and mounts it at `/var/lib/buildkit`. The Kata CLH guest rootfs is virtiofs, which does not support overlayfs upper directories; ext4 does.
4. Starts buildkitd at `tcp://localhost:1234` with `--oci-worker-net=host` and `--oci-worker-snapshotter=overlayfs`.
5. Starts dockerd at `tcp://127.0.0.1:2375`.
6. `exec`s `ci-worker`.

### Kata Container isolation

ci-worker pods use `runtimeClassName: kata-clh`, which runs each pod inside a Kata Cloud-Hypervisor microVM. This provides a real Linux kernel per pod, so Docker and BuildKit run natively without privileged containers at the host level. The `securityContext.privileged: true` flag in the pod spec applies inside the VM only.

## Job execution flow

For each job, the ci-worker:

1. **Fetches CI config.** Downloads `.docstore/ci.yaml` from `main` via `GET /repos/{repo}/-/file/.docstore/ci.yaml?branch=main`.

2. **Downloads branch source.** Fetches a tar.gz of the branch at the job's head sequence via `GET /repos/{repo}/-/archive?branch={branch}&at={seq}` and extracts it to a temporary directory.

3. **Marks checks pending.** For each check in `ci.yaml`, posts `POST /repos/{repo}/-/check` with `status=pending`.

4. **Runs all checks in parallel** via the executor (`internal/executor`).

5. **Uploads logs.** Writes each check's log to a temp file (served live at `:8081`) and uploads to GCS.

6. **Posts results.** For each check result, posts `POST /repos/{repo}/-/check` with `status=passed` or `status=failed` and the GCS log URL.

## Executor (`internal/executor`)

The executor translates a `Config` into a BuildKit LLB DAG and dispatches it to buildkitd.

For each check (run in parallel via goroutines):

1. Resolves the image config from the registry to extract environment variables (e.g. `PATH` in `golang:1.25`).
2. Injects all image ENV variables and `DOCKER_HOST=tcp://localhost:2375` so check steps can use Docker.
3. Mounts the source directory as a local BuildKit input named `src`.
4. Chains the steps sequentially: each step runs `sh -c <step>` in the image with `/src` mounted. The `/src` mount is passed forward between steps so file mutations persist across steps within the same check.
5. Marshals the LLB DAG and calls `client.Build`. Log output is collected from the `SolveStatus` channel.
6. Returns `CheckResult{Name, Status, Logs}`.

## `.docstore/ci.yaml` DSL

The CI configuration file lives at `.docstore/ci.yaml` on `main`. Example:

```yaml
checks:
  - name: ci/build
    image: golang:1.25
    steps:
      - go build ./...

  - name: ci/test
    image: golang:1.25
    steps:
      - go test ./...

  - name: ci/vet
    image: golang:1.25
    steps:
      - go vet ./...
```

### Fields

- `checks` — List of checks to run in parallel.
- `checks[].name` — Check name reported to docstore (e.g. `ci/build`). Must be unique within the file.
- `checks[].image` — OCI image to use as the build environment. The image is pulled by buildkitd.
- `checks[].steps` — Shell commands run in sequence inside the image with the source mounted at `/src`. Each step is executed as `sh -c <step>`. Steps within a check run sequentially; file mutations in `/src` persist between steps.

### Notes

- The config is always read from `main`, not from the branch being tested. Update `ci.yaml` by merging to `main`.
- All checks run in parallel.
- A check fails if any step exits non-zero.
- `DOCKER_HOST=tcp://localhost:2375` is injected automatically, so check steps can run `docker build`, `docker push`, etc. The dockerd is running in the same Kata VM.

## ci_jobs table

The scheduler and workers coordinate through a PostgreSQL table (`ci_jobs`). Key columns:

| Column | Type | Description |
|---|---|---|
| `id` | UUID | Job ID |
| `repo` | text | Repo full name |
| `branch` | text | Branch name |
| `sequence` | int8 | Head sequence at time of event |
| `status` | text | `queued`, `claimed`, `passed`, `failed` |
| `worker_pod_ip` | text | IP of the claiming worker (for log proxy) |
| `last_heartbeat_at` | timestamptz | Updated every 30s by the worker |
| `log_url` | text | GCS URL for the first check's logs |
| `error_message` | text | Set if the job itself failed (not a check) |

A job is considered stale if its heartbeat is more than 90 seconds old and its status is `claimed`. The scheduler reaps stale jobs back to `queued` every 30 seconds.

## Webhook signature verification

The docstore outbox signs webhook deliveries with HMAC-SHA256:

```
X-DocStore-Signature: sha256=<hex(hmac-sha256(body, secret))>
```

The scheduler verifies this signature before processing any event. Mismatched signatures are rejected with HTTP 400.
