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
    │ POST /claim         ← K8s SA token auth + pod provenance checks
    │ POST /jobs/{id}/heartbeat ← request_token auth
    │ POST /jobs/{id}/complete  ← request_token auth
    ▼
ci-worker pods (GKE, KEDA ScaledJob, Kata CLH microVMs)
    │ POST /claim with K8s SA token
    │ receive job + request_token + oidc_token_url
    │ exchange request_token → OIDC JWT
    │ POST /archive/presign with request_token → presigned URL
    │ fetch .docstore/ci.yaml (JWT auth)
    │ run checks via BuildKit LLB (presigned source URL, no credentials)
    │ upload logs to GCS
    │ POST /repos/{repo}/-/check (JWT auth)
    │ POST /jobs/{id}/complete (request_token)
    └─ exit (KEDA creates a fresh pod for the next job)
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
- Serves `POST /claim` — validates K8s projected SA tokens with pod provenance checks, claims the next queued job, and returns a `request_token`.
- Serves `POST /jobs/{id}/heartbeat` — request_token authenticated; updates the job heartbeat.
- Serves `POST /jobs/{id}/complete` — request_token authenticated; marks the job passed or failed.
- Serves `GET /queue-depth` — returns the count of queued jobs for the KEDA autoscaler.

**Flags / environment variables:**

| Flag | Env var | Description |
|---|---|---|
| `-port` | — | HTTP listen port (default `8080`) |
| `-docstore-url` | `DOCSTORE_URL` | Base URL of the docstore server |
| `-scheduler-url` | `RUNNER_URL` | Public URL of this scheduler (used to auto-register the webhook subscription) |
| `-webhook-secret` | `WEBHOOK_SECRET` | HMAC secret for webhook signature verification |
| `-oidc-token-url` | `OIDC_TOKEN_URL` | URL of the CI OIDC token endpoint returned to workers on /claim |
| — | `DATABASE_URL` | PostgreSQL DSN (required) |
| — | `LOG_LEVEL` | Log level |
| — | `LOG_FORMAT` | `json` (default) or `text` |

On startup, if `DOCSTORE_URL`, `RUNNER_URL`, and `WEBHOOK_SECRET` are all set, the scheduler self-registers a webhook subscription with the docstore server.

### ci-worker (`cmd/ci-worker`)

A process that:

1. Waits for buildkitd (`tcp://localhost:1234`) and dockerd (`tcp://localhost:2375`) to be ready (up to 5 minutes each).
2. Calls `POST /claim` on ci-scheduler with its Kubernetes projected service account token to claim a queued job. Returns 204 (no job) or a JSON body with the job, `request_token`, and `oidc_token_url`.
3. Exchanges the `request_token` for a short-lived OIDC JWT by calling `oidc_token_url`.
4. Sends a heartbeat to ci-scheduler via `POST /jobs/{id}/heartbeat` (authenticated with `request_token`) every 30 seconds while the job runs.
5. Executes the job (see below).
6. Reports final status to ci-scheduler via `POST /jobs/{id}/complete` (authenticated with `request_token`).
7. Exits. KEDA creates a fresh pod for the next job.

**Required environment variables:**

| Variable | Description |
|---|---|
| `DOCSTORE_URL` | Base URL of the docstore server |
| `CI_SCHEDULER_URL` | URL of the ci-scheduler service |
| `LOG_STORE` | Log backend: `gcs` or `local` |
| `LOG_BUCKET` | GCS bucket for logs (required when `LOG_STORE=gcs`) |
| `BUILDKIT_ADDR` | buildkitd address (default `tcp://localhost:1234`) |

The Kubernetes projected service account token is auto-mounted at `/var/run/secrets/kubernetes.io/serviceaccount/token` and is read automatically by the worker. No explicit env var is required.

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

## Authentication and Job Identity

Workers no longer authenticate with a shared database credential. The authentication chain is:

### 1. K8s token validation (POST /claim)

Worker pods present their Kubernetes projected service account token in `Authorization: Bearer <token>` to ci-scheduler's `POST /claim` endpoint. ci-scheduler validates it via the Kubernetes TokenReview API and verifies pod provenance:

- Token is valid and issued for `system:serviceaccount:docstore-ci:ci-worker`
- Pod was created within the last 4 hours (freshness check)
- Pod's owner chain: Pod → `batch/v1` Job → `keda.sh/v1alpha1` ScaledJob named `ci-worker`
- Each pod handles at most one job (enforced by job claiming semantics)

### 2. request_token

On successful claim, ci-scheduler generates a cryptographically random `request_token` (32 bytes, base64url-encoded). The SHA-256 hash is stored in the database; the plaintext is returned once to the worker. The `request_token` is used to:

- Authenticate `POST /jobs/{id}/heartbeat`
- Authenticate `POST /jobs/{id}/complete`
- Authenticate `POST /repos/{repo}/-/archive/presign`
- Exchange for an OIDC JWT

### 3. OIDC JWT exchange

The worker calls `POST <oidc_token_url>` (the ci-oidc Cloud Run internal service) with the `request_token`. The response is a signed JWT with these claims:

| Claim | Value |
|---|---|
| `iss` | `https://oidc.docstore.dev` |
| `sub` | `repo:{repo}:branch:{branch}:check:{check_name}` |
| `aud` | `docstore` |
| `jti` | unique UUID (written to `ci_oidc_tokens` audit table) |
| `job_id` | CI job UUID |
| `repo` | full repo name (e.g. `acme/myrepo`) |
| `org` | first path segment of repo |
| `branch` | branch name |
| `check_name` | check name (may be empty) |
| `ref_type` | `post-submit`, `pre-submit`, `schedule`, or `manual` |
| `sequence` | head sequence number |
| `exp` | iat + 1 hour |

JWTs are signed with RS256 using a Cloud KMS asymmetric key. The private key never leaves KMS.

### 4. API authentication

Workers include the JWT as `Authorization: Bearer <token>` on all docstore API calls (fetch config, post check results).

### 5. Job identity on check runs

When a worker posts check results via `POST /repos/:repo/-/check`, the docstore server validates the JWT via `JobTokenMiddleware` and records the identity as `ci-job:{job_id}` — the `reporter` on the check run.

## Presigned Archive URLs

BuildKit cannot hold bearer tokens. To fetch the source archive securely without embedding credentials in the build context:

1. Worker calls `POST /repos/{repo}/-/archive/presign` with `Authorization: Bearer {request_token}`.
2. Server verifies the `request_token` matches the job and the repo in the token matches the request path.
3. Server generates an HMAC-SHA256 signed URL valid for 1 hour:
   ```
   https://docstore.dev/repos/{repo}/-/archive?branch={branch}&at={seq}&expires={unix}&sig={hex}
   ```
4. The HMAC covers `repo`, `branch`, `sequence`, and `expires` (newline-separated) with a server-side secret.
5. Worker passes this URL as the BuildKit source — no credentials are needed to fetch it.

## Job execution flow

For each job, the ci-worker:

1. **Fetches CI config.** Downloads `.docstore/ci.yaml` from the branch under test at the job's head sequence via `GET /repos/{repo}/-/file/.docstore/ci.yaml`, authenticated with the OIDC JWT.

2. **Gets a presigned archive URL.** Calls `POST /repos/{repo}/-/archive/presign` with the `request_token` to obtain a time-limited, HMAC-signed URL for the source archive. No credentials are embedded in the URL itself.

3. **Marks checks pending.** For each check in `ci.yaml`, posts `POST /repos/{repo}/-/check` with `status=pending`, authenticated with the OIDC JWT.

4. **Runs all checks in parallel** via the executor (`internal/executor`). The executor fetches the source archive via the presigned URL using BuildKit's `llb.HTTP` source.

5. **Uploads logs.** Writes each check's log to a temp file (served live at `:8081`) and uploads to GCS.

6. **Posts results.** For each check result, posts `POST /repos/{repo}/-/check` with `status=passed` or `status=failed` and the GCS log URL, authenticated with the OIDC JWT.

## OIDC Service

The OIDC identity provider runs as two separate Cloud Run services using the same `ci-oidc` image:

### ci-oidc (internal ingress — GKE only)

Deployed with `--ingress=internal`, reachable from GKE worker pods via VPC. Serves the full endpoint set:

- `POST /ci/token` — accepts `request_token`, issues a signed JWT for the job
- `GET /.well-known/openid-configuration` — OIDC discovery document
- `GET /.well-known/jwks.json` — RSA public key for JWT verification
- `GET /healthz`

**Required env vars:** `DATABASE_URL`, `KMS_KEY_VERSION`

### ci-oidc-public (public — oidc.docstore.dev)

Deployed with `--ingress=all --allow-unauthenticated` and `PUBLIC_ONLY=true`. Serves only the discovery endpoints (no database access, no token issuance):

- `GET /.well-known/openid-configuration`
- `GET /.well-known/jwks.json`
- `GET /healthz`

**Required env vars:** `KMS_KEY_VERSION` only (no database access)

The CNAME `oidc.docstore.dev → ghs.googlehosted.com` is configured via Cloud Run domain mapping so that external systems (GCP WIF, AWS STS, HashiCorp Vault) can discover the JWKS and verify job JWTs.

## Executor (`internal/executor`)

The executor translates a `Config` into a BuildKit LLB DAG and dispatches it to buildkitd.

For each check (run in parallel via goroutines):

1. Resolves the image config from the registry to extract environment variables (e.g. `PATH` in `golang:1.25`).
2. Injects all image ENV variables and `DOCKER_HOST=tcp://localhost:2375` so check steps can use Docker.
3. Fetches the source archive from the presigned URL via BuildKit `llb.HTTP`.
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

- The config is always read from the branch under test at its head sequence, not from `main`. However, `on:` trigger filtering is evaluated against `main` at the time of enqueueing.
- All checks run in parallel.
- A check fails if any step exits non-zero.
- `DOCKER_HOST=tcp://localhost:2375` is injected automatically, so check steps can run `docker build`, `docker push`, etc. The dockerd is running in the same Kata VM.
- `DOCSTORE_OIDC_REQUEST_TOKEN` and `DOCSTORE_OIDC_REQUEST_URL` are injected into each check's environment so steps can themselves obtain OIDC tokens if needed.

## ci_jobs table

The scheduler and workers coordinate through a PostgreSQL table (`ci_jobs`). Key columns:

| Column | Type | Description |
|---|---|---|
| `id` | UUID | Job ID |
| `repo` | text | Repo full name |
| `branch` | text | Branch name |
| `sequence` | int8 | Head sequence at time of event |
| `status` | text | `queued`, `claimed`, `passed`, `failed` |
| `worker_pod_name` | text | Pod name of the claiming worker (from K8s token claims) |
| `worker_pod_ip` | text | IP of the claiming worker (for log proxy) |
| `request_token_hash` | text | SHA-256 hash of the issued request_token |
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
