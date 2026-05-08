# CI Architecture

DocStore CI runs on GKE using [Kata Containers](https://katacontainers.io/) with the Cloud Hypervisor (CLH) backend. Each build executes inside a real microVM — no DinD nesting, no privileged host containers.

## Architecture Overview

```
docstore events (CloudEvents via outbox)
  ├─ com.docstore.commit.created  ──────────────────────────────┐
  └─ com.docstore.proposal.opened ──────────────────────────────┤
                                                                 ▼
                                               ci-scheduler (standard GKE node, :8080)
                                               ├─ POST /webhook   ← webhook delivery
                                               ├─ POST /claim     ← K8s SA token auth
                                               ├─ POST /jobs/{id}/heartbeat ← request_token auth
                                               ├─ POST /jobs/{id}/complete  ← request_token auth
                                               └─ cron runner     ← schedule trigger
                                                         │
                                               INSERT INTO ci_jobs (trigger_type, ...)
                                                         │
                                               ci-worker pod (Kata CLH microVM) calls POST /claim
                                               ├─ K8s SA token → request_token + oidc_token_url
                                               ├─ exchange request_token → OIDC JWT
                                               ├─ GET presigned archive URL
                                               ├─ fetch .docstore/ci.yaml (JWT auth)
                                               ├─ evaluate if: conditions
                                               ├─ run checks via BuildKit executor
                                               ├─ POST logs → docstore server (request_token auth) → GCS
                                               └─ POST check_runs → docstore (JWT auth)
```

### Components

| File | Role |
|---|---|
| `cmd/ci-scheduler/main.go` | Webhook receiver; inserts `ci_jobs` rows; validates K8s SA tokens; issues request_tokens; reaps stale claimed jobs |
| `cmd/ci-worker/main.go` | Claims job via POST /claim with K8s SA token; exchanges request_token for OIDC JWT; runs executor; uploads logs via POST /repos/:repo/-/check/:name/logs on docstore server (request_token auth); posts check run results; then exits |
| `cmd/ci-oidc/main.go` | Exchanges request_tokens for signed OIDC JWTs; serves OIDC discovery and JWKS endpoints |
| `internal/k8sproof/k8sproof.go` | Validates K8s projected SA tokens and pod provenance (owner chain, age, service account) |
| `internal/executor/executor.go` | Translates `.docstore/ci.yaml` checks into BuildKit LLB DAGs and dispatches them to buildkitd |
| `internal/archivesign/archivesign.go` | HMAC-SHA256 signing and verification for presigned archive URLs |
| `entrypoint-worker.sh` | Kata VM startup: GCR auth → loop-ext4 setup → buildkitd → dockerd → ci-worker |
| `deploy/k8s/ci-worker.yaml` | Kata CLH KEDA ScaledJob (`runtimeClassName: kata-clh`); KEDA creates one pod per queued job; each pod handles one job then exits |
| `deploy/k8s/ci-scheduler.yaml` | Standard GKE Deployment (1 replica) + internal LoadBalancer reachable via VPC Direct Egress |
| `deploy/k8s/debug-kata.yaml` | Throwaway privileged ubuntu:24.04 pod with `runtimeClassName: kata-clh` for kernel investigation |

### Network topology

```
Cloud Run (docstore)
  └─[VPC Direct Egress]─► internal LB :8080
                                 └─► ci-scheduler pod (docstore-ci namespace)
                                           └─► PostgreSQL (Cloud SQL proxy sidecar)

ci-worker pods (kata-clh microVM)
  ├─► ci-scheduler :8080  (claim, heartbeat, complete)
  ├─► ci-oidc (internal Cloud Run)  (POST /ci/token)
  ├─► docstore Cloud Run (JWT-authenticated API calls)
  ├─► buildkitd tcp://localhost:1234
  ├─► dockerd   tcp://127.0.0.1:2375
  └─► log HTTP  :8081  (proxied through ci-scheduler for live log streaming)

oidc.docstore.dev (Cloud Run domain mapping → ci-oidc-public)
  ├─► GET /.well-known/openid-configuration
  └─► GET /.well-known/jwks.json
```

---

## Security Architecture

### Why: the problem with static credentials

The original CI system gave workers direct database access (`DATABASE_URL`). This created a large blast radius: any compromised worker could read or modify any CI job. The new system replaces static shared credentials with cryptographically verifiable, scoped, short-lived job identity.

### How: the authentication chain

```
Pod starts (Kata CLH microVM)
    │
    ├─ K8s projected SA token (auto-mounted at
    │  /var/run/secrets/kubernetes.io/serviceaccount/token)
    │
    ▼
POST ci-scheduler/claim
    │  K8s TokenReview + provenance checks (internal/k8sproof):
    │  • token valid, SA = system:serviceaccount:docstore-ci:ci-worker
    │  • pod age < 4h (warm pool workers stay valid)
    │  • owner chain: Pod → batch/v1 Job → keda.sh/v1alpha1 ScaledJob(ci-worker)
    │
    ▼
request_token (32-byte random, base64url, plaintext returned once)
SHA-256 hash stored in ci_jobs table
    │
    ├─ Used for: POST /heartbeat, POST /complete, POST /archive/presign
    │
    ▼
POST ci-oidc/ci/token (internal Cloud Run — GKE only)
    │  Validates request_token against ci_jobs table
    │  Signs JWT via Cloud KMS (RS256, private key never leaves KMS)
    │
    ▼
Job OIDC JWT (valid 1 hour)
    iss: https://oidc.docstore.dev
    sub: repo:acme/myrepo:branch:main:check:ci/test
    aud: docstore
    jti: <uuid> (written to ci_oidc_tokens audit table)
    job_id, repo, org, branch, check_name, ref_type, sequence
    │
    ├─ Authorization: Bearer <jwt> on all docstore API calls
    │  (fetch config, mark checks pending, post check results)
    │
    └─ Can be exchanged with GCP Workload Identity Federation, AWS STS,
       HashiCorp Vault for cloud provider credentials — no static secrets needed
```

### Pod provenance chain

The KEDA ScaledJob creates a new Kubernetes Job (and pod) for each item in the queue. ci-scheduler verifies this lineage:

```
KEDA ScaledJob "ci-worker"
    └─ creates: batch/v1 Job (per queue item)
                    └─ creates: Pod (runs Kata CLH microVM)
                                    └─ mounts: projected SA token
                                               (bound to the pod's lifetime)
```

The `internal/k8sproof` package walks this owner reference chain via the Kubernetes API. A token from any pod not owned by the `ci-worker` ScaledJob is rejected, preventing spoofed claims from arbitrary pods.

### OIDC issuer as a trust anchor

`oidc.docstore.dev` acts as a standard OIDC identity provider. The JWKS is publicly accessible so external systems can:

- Verify the JWT signature without trusting DocStore's internal state
- Use GCP Workload Identity Federation, AWS STS, or HashiCorp Vault to exchange the job JWT for cloud credentials
- Audit token issuance via the `ci_oidc_tokens` table (jti, job_id, audience, exp)

---

### Event subscription and routing

ci-scheduler subscribes to docstore events at startup. When `RUNNER_URL` is set, the scheduler automatically registers a webhook subscription with docstore on boot:

```
POST /repos/*/subscriptions   (wildcard — all repos)
  backend: webhook
  endpoint: <RUNNER_URL>/webhook
  event_types: [com.docstore.commit.created, com.docstore.proposal.opened]
```

Deliveries are HMAC-SHA256 signed using `WEBHOOK_SECRET`. The scheduler verifies the `X-DocStore-Signature` header on every request and rejects unverified payloads.

**Incoming event types and what they trigger:**

| CloudEvent type | Trigger type enqueued | Condition |
|---|---|---|
| `com.docstore.commit.created` | `push` | Always, unless `on.push.branches` filter excludes the branch |
| `com.docstore.commit.created` | `proposal_synchronized` | Additionally, if the pushed branch has an open proposal and `on.proposal` is configured |
| `com.docstore.proposal.opened` | `proposal` | If `on.proposal.base_branches` matches the proposal's base branch |
| *(cron runner)* | `schedule` | If the current minute matches a `on.schedule[].cron` expression |
| `POST /repos/:name/-/ci/run` on docstore (auth-protected, writer+) | `manual` | Always |

The `proposal_synchronized` trigger is synthetic — ci-scheduler detects it by querying `GET /repos/:name/-/proposals?state=open&branch=<branch>` after receiving a `commit.created` event. No separate event is emitted by docstore.

### Job lifecycle

1. An event arrives at `POST /webhook` (or the cron runner fires, or `POST /repos/:name/-/ci/run` is called on the docstore server).
2. ci-scheduler verifies the HMAC signature (webhook path only), parses the CloudEvent, and reads `.docstore/ci.yaml` from the branch under test.
3. The `on:` block in ci.yaml is evaluated against the event. If no trigger matches, the request is acknowledged and no job is enqueued.
4. A `ci_jobs` row is inserted with `status='queued'` and trigger metadata (`trigger_type`, `trigger_branch`, `trigger_base_branch`, `trigger_proposal_id`).
5. A ci-worker pod calls `POST /claim` with its Kubernetes projected service account token. ci-scheduler validates the token via the K8s TokenReview API and verifies pod provenance (service account, pod age, owner chain). On success, it atomically claims the oldest queued job and returns the job row plus a one-time `request_token` and `oidc_token_url`.
6. The worker exchanges the `request_token` for a short-lived OIDC JWT by calling `POST <oidc_token_url>` on the ci-oidc internal service.
7. The worker calls `POST /repos/{repo}/-/archive/presign` (authenticated with `request_token`) to obtain a presigned, HMAC-signed archive URL. The URL is passed to BuildKit as the source — no credentials are embedded.
8. The worker fetches `.docstore/ci.yaml` from the branch at the pinned sequence (JWT auth) and builds a `TriggerContext` from the job row.
9. For each check, the worker evaluates the `if:` expression against the `TriggerContext`. Checks that evaluate to false are skipped entirely.
10. Remaining checks are executed concurrently via BuildKit LLB inside the Kata microVM.
11. The worker POSTs logs to the docstore server at `POST /repos/:repo/-/check/:name/logs` (request_token auth); the server writes the logs to GCS. The worker then posts `check_runs` back to docstore (JWT auth).
12. The worker calls `POST /jobs/{id}/complete` (authenticated with `request_token`) to record final status.
13. The pod exits. KEDA sees the queue depth change and creates a new pod for the next job.
14. ci-scheduler reaps jobs whose `last_heartbeat_at` has gone stale (missed heartbeats from crashed workers) every 30 seconds, resetting them to `queued`.

### Trigger context

Each `ci_jobs` row carries the full trigger context, which is surfaced to ci-worker and used to evaluate `if:` expressions:

| Column | Type | Description |
|---|---|---|
| `trigger_type` | text | `push`, `proposal`, `proposal_synchronized`, `manual`, `schedule` |
| `trigger_branch` | text | Branch that triggered the run |
| `trigger_base_branch` | text | Proposal target branch (`proposal`/`proposal_synchronized` only) |
| `trigger_proposal_id` | text | Proposal UUID (`proposal`/`proposal_synchronized` only) |

These map directly to the `event.*` fields available in `if:` expressions (see [proposals-and-ci-triggers.md](proposals-and-ci-triggers.md)).

### Manual trigger

The manual trigger endpoint is on the main docstore server (requires writer role):

```bash
curl -X POST https://docstore.dev/repos/acme/myrepo/-/ci/run \
  -H "Content-Type: application/json" \
  -H "Proxy-Authorization: Bearer $(gcloud auth print-identity-token)" \
  -d '{"branch": "feature/x"}'
```

Returns `{"run_id": "..."}`. Manual runs use `trigger_type=manual` and bypass the `on:` block filter — they always enqueue regardless of what triggers are configured.

---

## Kata CLH Guest Environment

Each ci-worker pod runs as a full microVM. The container image is the VM's userland.

### Storage

| Path | Backend | Notes |
|---|---|---|
| `/` (rootfs) | virtiofs | Read-only style; **no overlayfs upper dir support** (EINVAL) |
| `/var/lib/buildkit` | loop-backed ext4 | Created at startup by `entrypoint-worker.sh`; supports overlayfs |
| `/var/lib/docker` | virtiofs | dockerd uses `vfs` snapshotter here — acceptable since CI builds go through buildkitd, not dockerd |

### Loop-ext4 setup

Kata's `privileged_without_host_devices=true` means host `/dev/loop*` nodes are not present inside the VM. The guest kernel has `CONFIG_BLK_DEV_LOOP=y` (built-in) but udev does not run, so loop nodes must be created manually.

`entrypoint-worker.sh` does this at every pod startup:

```sh
# Create loop control and device nodes (udev doesn't run in Kata guest)
mknod /dev/loop-control c 10 237
for i in $(seq 0 7); do mknod /dev/loop$i b 7 $i; done

# Create a 20G sparse file (no bytes written — disk-backed via virtiofs)
truncate -s 20G /var/lib/buildkit.img
mkfs.ext4 -F -q -E lazy_itable_init=1,lazy_journal_init=1 /var/lib/buildkit.img
LOOP=$(losetup -f --show /var/lib/buildkit.img)
mount "$LOOP" /var/lib/buildkit
```

Setup time: ~150 ms. The sparse file has no RAM pressure — space is allocated on the host virtiofs backing store on demand.

buildkitd then starts with `--oci-worker-snapshotter=overlayfs` on the ext4 mount, where overlayfs upper dirs work correctly.

---

## Why This Approach: History of Attempts

We went through five attempts before landing on loop-backed ext4. Each is documented here so we don't re-try dead ends.

| Attempt | Approach | Failure |
|---|---|---|
| 1 | fuse-overlayfs snapshotter | `/dev/fuse` missing — GKE Kata sets `privileged_without_host_devices=true` |
| 2 | `modprobe fuse` | FUSE is built-in (`CONFIG_FUSE_FS=y`), no kernel module to load |
| 3 | `mknod /dev/fuse` | fuse-overlayfs worked for the snapshotter layer, but runc still needs **kernel** overlayfs for container rootfs assembly — two-level problem |
| 4 | Loop-mounted ext4 | Loop devices not present (udev not running) — fixed by `mknod /dev/loop*` → became the final approach |
| 5 | tmpfs | overlayfs worked, but the ~1.5 GB golang image in RAM caused OOM kills at 8 Gi memory limit |

**Final:** loop-backed sparse ext4 file — disk-backed, no RAM pressure, ~150 ms setup.

---

## Debugging Without Rebuilding the Image

`deploy/k8s/debug-kata.yaml` provides a throwaway privileged `ubuntu:24.04` pod running inside the same `kata-clh` runtime. Use it to probe kernel config, test device creation, or test mounts — much faster than rebuilding and pushing the ci-worker image.

```bash
kubectl apply -f deploy/k8s/debug-kata.yaml
kubectl exec -it -n docstore-ci kata-debug -- bash

# Inside the pod:
zcat /proc/config.gz | grep CONFIG_BLK_DEV_LOOP
ls /dev/loop*
mknod /dev/loop0 b 7 0 && losetup --help

kubectl delete -f deploy/k8s/debug-kata.yaml
```

The pod exits after 1 hour (`sleep 3600`) and is not restarted (`restartPolicy: Never`).

---

## Performance

| Snapshotter | Build time |
|---|---|
| Native (overlay-less) | ~8.5 min/build |
| overlayfs on loop-ext4 | ~88 s/build |

Measured on the same codebase and check suite. The ~6× improvement is from overlayfs caching image layers between build steps rather than unpacking them from scratch with the native snapshotter.

---

## Deployment

### Prerequisites

```bash
bash scripts/setup.sh       # GCP SA, GCS bucket, Secret Manager secrets, IAM
bash scripts/setup-gke.sh   # Workload Identity binding, k8s namespace and Secrets
```

### Build and push

```bash
docker build -f Dockerfile.ci-worker \
  -t us-central1-docker.pkg.dev/dlorenc-chainguard/images/ci-worker:latest .
docker push us-central1-docker.pkg.dev/dlorenc-chainguard/images/ci-worker:latest

docker build -f Dockerfile.ci-scheduler \
  -t us-central1-docker.pkg.dev/dlorenc-chainguard/images/ci-scheduler:latest .
docker push us-central1-docker.pkg.dev/dlorenc-chainguard/images/ci-scheduler:latest

docker build -f Dockerfile.ci-oidc \
  -t us-central1-docker.pkg.dev/dlorenc-chainguard/images/ci-oidc:latest .
docker push us-central1-docker.pkg.dev/dlorenc-chainguard/images/ci-oidc:latest
```

### Apply

```bash
kubectl apply -f deploy/k8s/ci-scheduler.yaml
kubectl apply -f deploy/k8s/ci-worker.yaml
kubectl rollout status deployment/ci-scheduler -n docstore-ci
```

Subsequent deploys happen automatically via the `build-and-deploy-ci` CI job on every push to `main`.

### Environment variables (ci-worker)

| Variable | Required | Description |
|---|---|---|
| `DOCSTORE_URL` | yes | Base URL of the docstore server |
| `CI_SCHEDULER_URL` | yes | URL of the ci-scheduler service |
| `BUILDKIT_ADDR` | no | buildkitd address; defaults to `tcp://localhost:1234` |
| `LOG_STORE` | no | `gcs` (production) or `local`; defaults to `local` |
| `LOG_BUCKET` | if gcs | GCS bucket name for build logs |

The Kubernetes projected service account token is auto-mounted at `/var/run/secrets/kubernetes.io/serviceaccount/token`.

### Environment variables (ci-scheduler)

| Variable | Required | Description |
|---|---|---|
| `DATABASE_URL` | yes | PostgreSQL connection string |
| `DOCSTORE_URL` | yes | Base URL of the docstore server |
| `OIDC_TOKEN_URL` | yes | URL of the ci-oidc `/ci/token` endpoint (returned to workers on /claim) |
| `WEBHOOK_SECRET` | no | HMAC secret for verifying incoming webhook deliveries |
| `RUNNER_URL` | no | Public URL of this scheduler (used to auto-register with docstore) |
