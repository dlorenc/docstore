# CI Architecture

DocStore CI runs on GKE using [Kata Containers](https://katacontainers.io/) with the Cloud Hypervisor (CLH) backend. Each build executes inside a real microVM — no DinD nesting, no privileged host containers.

## Architecture Overview

```
docstore repo push
  └─► docstore server fires webhook (com.docstore.commit.created)
        └─► ci-scheduler (standard GKE node, :8080)
              └─► INSERT INTO ci_jobs (status='queued')
                    └─► ci-worker pod (Kata CLH microVM) polls ci_jobs
                          └─► claim job → run buildkitd executor
                                └─► results → ci_jobs + logs → GCS
```

### Components

| File | Role |
|---|---|
| `cmd/ci-scheduler/main.go` | Webhook receiver; inserts `ci_jobs` rows; serves `/run` for manual triggers; reaps stale claimed jobs |
| `cmd/ci-worker/main.go` | Polls `ci_jobs`, claims one job, runs executor, uploads logs, posts check run results, then exits |
| `internal/executor/executor.go` | Translates `.docstore/ci.yaml` checks into BuildKit LLB DAGs and dispatches them to buildkitd |
| `entrypoint-worker.sh` | Kata VM startup: GCR auth → loop-ext4 setup → buildkitd → dockerd → ci-worker |
| `deploy/k8s/ci-worker.yaml` | Kata CLH Deployment (`runtimeClassName: kata-clh`); 3 replicas; each pod handles one job then exits |
| `deploy/k8s/ci-scheduler.yaml` | Standard GKE Deployment (1 replica) + internal LoadBalancer reachable via VPC Direct Egress |
| `deploy/k8s/debug-kata.yaml` | Throwaway privileged ubuntu:24.04 pod with `runtimeClassName: kata-clh` for kernel investigation |

### Network topology

```
Cloud Run (docstore)
  └─[VPC Direct Egress]─► internal LB :8080
                                 └─► ci-scheduler pod (docstore-ci namespace)
                                           └─► PostgreSQL (Cloud SQL proxy sidecar)

ci-worker pods (kata-clh microVM)
  ├─► buildkitd tcp://localhost:1234
  ├─► dockerd   tcp://127.0.0.1:2375
  └─► log HTTP  :8081  (proxied through ci-scheduler for live log streaming)
```

### Job lifecycle

1. Docstore outbox dispatches `com.docstore.commit.created` to `POST /webhook` on ci-scheduler.
2. ci-scheduler verifies the HMAC signature, parses the CloudEvent, and inserts a row into `ci_jobs` with `status='queued'`.
3. A ci-worker pod polls `ClaimCIJob` (atomic `UPDATE ... WHERE status='queued' LIMIT 1 RETURNING *`).
4. The worker fetches `.docstore/ci.yaml` from `main`, downloads the branch source tree as a tar archive, executes all checks via BuildKit, uploads logs to GCS, and posts `check_runs` back to docstore.
5. The worker calls `CompleteCIJob` (sets `status='done'` or `'failed'`) and exits.
6. The Deployment controller replaces the exited pod, maintaining the pool size (3 replicas by default).
7. ci-scheduler reaps jobs whose `last_heartbeat_at` has gone stale (missed heartbeats from crashed workers) every 30 seconds, resetting them to `queued`.

### Manual trigger

```bash
curl -X POST http://<ci-scheduler-ip>:8080/run \
  -H "Content-Type: application/json" \
  -d '{"repo": "acme/myrepo", "branch": "feature/x", "head_sequence": 42}'
```

Returns `{"run_id": "..."}`. Poll status with `GET /run/{id}`.

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
```

### Apply

```bash
kubectl apply -f deploy/k8s/ci-scheduler.yaml
kubectl apply -f deploy/k8s/ci-worker.yaml
kubectl rollout status deployment/ci-scheduler -n docstore-ci
kubectl rollout status deployment/ci-worker -n docstore-ci
```

Subsequent deploys happen automatically via the `build-and-deploy-ci` CI job on every push to `main`.

### Environment variables (ci-worker)

| Variable | Required | Description |
|---|---|---|
| `DATABASE_URL` | yes | PostgreSQL connection string (via Cloud SQL proxy) |
| `DOCSTORE_URL` | yes | Base URL of the docstore server |
| `POD_NAME` | yes | Injected via Downward API; used to claim jobs |
| `POD_IP` | yes | Injected via Downward API; used for live log proxying |
| `BUILDKIT_ADDR` | no | buildkitd address; defaults to `tcp://localhost:1234` |
| `LOG_STORE` | no | `gcs` (production) or `local`; defaults to `local` |
| `LOG_BUCKET` | if gcs | GCS bucket name for build logs |

### Environment variables (ci-scheduler)

| Variable | Required | Description |
|---|---|---|
| `DATABASE_URL` | yes | PostgreSQL connection string |
| `DOCSTORE_URL` | yes | Base URL of the docstore server |
| `WEBHOOK_SECRET` | no | HMAC secret for verifying incoming webhook deliveries |
| `RUNNER_URL` | no | Public URL of this scheduler (used to auto-register with docstore) |
