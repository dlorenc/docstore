# Deployment

DocStore is deployed as two components:

1. **Docstore server** — a single stateless binary on Cloud Run backed by Cloud SQL (PostgreSQL).
2. **CI system** — two GKE workloads (`ci-scheduler` and `ci-worker`) in the `docstore-ci` namespace.

## Prerequisites

- A GCP project with billing enabled.
- `gcloud` CLI authenticated.
- `kubectl` configured for the GKE cluster.
- Docker and `ko` installed for building images.

## One-time GCP setup

Run the setup script to create the service accounts, GCS log bucket, and Secret Manager secrets:

```bash
# Uses PROJECT=dlorenc-chainguard and REGION=us-central1 by default.
bash scripts/setup.sh

# Override:
PROJECT=my-project REGION=us-east1 bash scripts/setup.sh
```

The script creates:
- `ci-runner` GCP service account with `roles/storage.objectUser` on the `docstore-ci-logs` bucket.
- Secret Manager secrets `ci-runner-webhook-secret` and `ci-runner-url` (populated with placeholder values — update them before deploying).
- IAM bindings for the deployer service account.

## One-time GKE setup

```bash
bash scripts/setup-gke.sh
```

This configures Workload Identity for the GKE cluster, creates the `docstore-ci` namespace, and creates Kubernetes Secrets from GCP Secret Manager.

## Server environment variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `DATABASE_URL` | yes | — | PostgreSQL DSN (`postgres://user:pass@host/db`) |
| `PORT` | no | `8080` | HTTP listen port |
| `DEV_IDENTITY` | no | — | Bypass IAP JWT validation (dev only) |
| `BOOTSTRAP_ADMIN` | no | — | Identity with admin access to repos with no admin yet |
| `LOG_FORMAT` | no | `json` | Log format: `json` or `text` |
| `LOG_LEVEL` | no | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `DOCSTORE_BLOB_STORE` | no | `local` | Blob backend: `gcs` or `local` |
| `DOCSTORE_BLOB_BUCKET` | if gcs | — | GCS bucket for large file content |
| `DOCSTORE_BLOB_THRESHOLD_BYTES` | no | `1048576` | Files larger than this go to the blob store (default 1 MB) |
| `DOCSTORE_BLOB_LOCAL_DIR` | no | `/tmp/docstore-blobs` | Local blob directory when `DOCSTORE_BLOB_STORE=local` |

## Cloud SQL setup

The server connects to PostgreSQL via Cloud SQL. The Cloud Run deployment uses the Cloud SQL Auth Proxy sidecar (via `--add-cloudsql-instances`). The `DATABASE_URL` secret is stored in Secret Manager and injected at runtime.

Migrations run automatically on startup from embedded SQL files. To reset the database schema (destroys all data):

```bash
DATABASE_URL="..." make db-reset
```

## Cloud Run deployment

Deployment is automated via GitHub Actions (`.github/workflows/deploy.yml`) on every push to `main`. The workflow:

1. Builds the server image using `ko` and pushes to `us-central1-docker.pkg.dev/dlorenc-chainguard/images/docstore`.
2. Deploys to Cloud Run with:
   - Cloud SQL instance: `dlorenc-chainguard:us-central1:docstore-mvp`
   - Secret: `DATABASE_URL` from `docstore-db-url` in Secret Manager
   - 1 min instance, auto-scaling enabled (SSE events are DB-backed via event_log)
   - `--no-cpu-throttling` — keeps the CPU active for background goroutines
   - VPC egress via `--network=default --subnet=default --vpc-egress=private-ranges-only` so Cloud Run can reach the internal GKE load balancer

To deploy manually:

```bash
# Build and push
KO_DOCKER_REPO=us-central1-docker.pkg.dev/dlorenc-chainguard/images/docstore \
  ko build --bare --tags latest ./cmd/docstore

# Deploy
gcloud run deploy docstore \
  --image=us-central1-docker.pkg.dev/dlorenc-chainguard/images/docstore:latest \
  --project=dlorenc-chainguard \
  --region=us-central1 \
  --service-account=docstore-server@dlorenc-chainguard.iam.gserviceaccount.com \
  --add-cloudsql-instances=dlorenc-chainguard:us-central1:docstore-mvp \
  --update-secrets=DATABASE_URL=docstore-db-url:latest \
  --allow-unauthenticated \
  --min-instances=1 \
  --no-cpu-throttling \
  --port=8080
```

The compiled-in default remote URL for `ds` is `https://docstore-efuj4cj54a-uc.a.run.app` (set in `Makefile` as `DEFAULT_REMOTE`).

## GKE deployment (CI system)

The CI system runs in GKE cluster `docstore-ci` in region `us-central1`, project `dlorenc-chainguard`.

### ci-scheduler

Receives webhook events from docstore, queues jobs in the `ci_jobs` PostgreSQL table, reaps stale jobs, and serves job-status and log-proxy endpoints.

Deploy:
```bash
kubectl apply -f deploy/k8s/ci-scheduler.yaml -n docstore-ci
```

The manifest creates:
- A `Deployment` with 1 replica on the default node pool.
- An internal `LoadBalancer` Service (reachable from Cloud Run via VPC Direct Egress, not exposed to the internet).
- A Cloud SQL Proxy sidecar for database access.

Environment variables injected from Kubernetes Secrets:
- `DATABASE_URL` from Secret `ci-scheduler/database-url`
- `WEBHOOK_SECRET` from Secret `ci-runner/webhook-secret` (optional)
- `RUNNER_URL` from Secret `ci-runner/runner-url` (optional — the scheduler's own public URL, used to self-register the webhook subscription)

### ci-worker

Runs inside Kata Container microVMs (RuntimeClass `kata-clh`) on GKE nodes labelled `runtime=kata`. Each pod claims one job, executes it, and exits. The Deployment controller (3 replicas) replaces exited pods.

Deploy:
```bash
kubectl apply -f deploy/k8s/ci-worker.yaml -n docstore-ci
```

The manifest:
- Uses `runtimeClassName: kata-clh` for microVM isolation.
- Sets `securityContext.privileged: true` (safe inside Kata — the VM is the security boundary).
- Requests 2 CPU and 8 GiB RAM per pod.
- Exposes port 8081 for live log access.

Environment variables:
- `DATABASE_URL` from Secret `ci-scheduler/database-url`
- `DOCSTORE_URL` — hardcoded to `https://docstore-efuj4cj54a-uc.a.run.app`
- `POD_NAME` and `POD_IP` injected from the pod's downward API
- `LOG_STORE=gcs` and `LOG_BUCKET=docstore-ci-logs` — enables GCS log upload

### Automated CI deployment

The deploy workflow (`deploy.yml`) also builds and deploys both CI binaries after the server deploys:

1. Builds `ci-scheduler` with `docker buildx` using `Dockerfile.ci-scheduler` → pushes to Artifact Registry.
2. Applies `deploy/k8s/ci-scheduler.yaml` and rolls out the new image.
3. Updates the `ci-runner-url` Secret Manager secret with the internal LB IP.
4. Builds `ci-worker` with `docker buildx` using `Dockerfile.ci-worker` → pushes to Artifact Registry.
5. Applies `deploy/k8s/ci-worker.yaml` and rolls out the new image.

## First deployment checklist

1. Run `bash scripts/setup.sh` to create GCP resources.
2. Run `bash scripts/setup-gke.sh` to configure GKE Workload Identity.
3. Set the webhook HMAC secret in Secret Manager:
   ```bash
   echo -n 'your-secret' | gcloud secrets versions add ci-runner-webhook-secret \
     --data-file=- --project=dlorenc-chainguard
   ```
4. Push to `main` to trigger the deploy workflow.
5. After deploy, verify with `curl https://docstore-efuj4cj54a-uc.a.run.app/healthz`.
6. Create the first org and repo:
   ```bash
   ds orgs create myorg
   ds repos create myorg myrepo
   ```
7. Assign a bootstrap admin:
   ```bash
   # Set BOOTSTRAP_ADMIN env var on the Cloud Run service, then use ds:
   ds roles set alice@example.com admin
   ```

## Horizontal scaling

The event broker persists all events to the `event_log` PostgreSQL table and uses `pg_notify` for real-time wake-ups. SSE streams (`GET /events`, `GET /repos/{name}/-/events`) poll `event_log` directly, so every instance sees every event regardless of which instance processed the original mutation. Multiple Cloud Run instances are fully supported.
