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
  --ingress=internal-and-cloud-load-balancing \
  --args=--bootstrap-admin=dlorenc@chainguard.dev \
  --min-instances=1 \
  --no-cpu-throttling \
  --port=8080
```

The compiled-in default remote URL for `ds` is `https://docstore.dev` (set in `Makefile` as `DEFAULT_REMOTE`).

## Authentication (Cloud IAP)

Production traffic reaches the server exclusively through a Global HTTPS Load Balancer at `https://docstore.dev`. Google Cloud Identity-Aware Proxy (IAP) is enabled on the backend service — any Google account can sign in; no allowlist is required (`allAuthenticatedUsers` has `roles/iap.httpsResourceAccessor`).

### How it works

1. The client (browser or `ds` CLI) sends a request to `https://docstore.dev`.
2. The Global HTTPS LB terminates TLS using the managed certificate `docstore-cert`.
3. IAP validates the user's Google session and injects an `X-Goog-IAP-JWT-Assertion` header signed by Google.
4. The request is forwarded to Cloud Run via the serverless NEG `docstore-neg`.
5. The server's `IAPMiddleware` validates the JWT (RS256, keys from `https://www.gstatic.com/iap/verify/public_key-jwk`) and extracts the `email` claim as the caller's identity.

Cloud Run ingress is set to `internal-and-cloud-load-balancing`, so direct requests to `*.run.app` are blocked. All traffic must go through the load balancer.

`DEV_IDENTITY` / `--dev-identity` is **for local development only** and must never be set on the production Cloud Run service.

### One-time infrastructure (already provisioned)

The following resources were created once in project `dlorenc-chainguard` and do not need to be re-created on normal deploys. They are documented here so the setup can be reproduced in a new project.

```bash
# Static external IP
gcloud compute addresses create docstore-ip \
  --global --project=dlorenc-chainguard

# Serverless NEG pointing at the Cloud Run service
gcloud compute network-endpoint-groups create docstore-neg \
  --region=us-central1 \
  --network-endpoint-type=serverless \
  --cloud-run-service=docstore \
  --project=dlorenc-chainguard

# Backend service with IAP enabled
gcloud compute backend-services create docstore-backend \
  --global \
  --protocol=HTTPS \
  --project=dlorenc-chainguard

gcloud compute backend-services add-backend docstore-backend \
  --global \
  --network-endpoint-group=docstore-neg \
  --network-endpoint-group-region=us-central1 \
  --project=dlorenc-chainguard

# Enable IAP on the backend service
gcloud iap web enable --resource-type=backend-services \
  --service=docstore-backend \
  --project=dlorenc-chainguard

# Grant all Google accounts access via IAP
gcloud iap web add-iam-policy-binding \
  --resource-type=backend-services \
  --service=docstore-backend \
  --member=allAuthenticatedUsers \
  --role=roles/iap.httpsResourceAccessor \
  --project=dlorenc-chainguard

# URL map, managed SSL cert, HTTPS proxy, and forwarding rule
gcloud compute url-maps create docstore-map \
  --default-service=docstore-backend \
  --global --project=dlorenc-chainguard

gcloud compute ssl-certificates create docstore-cert \
  --domains=docstore.dev \
  --global --project=dlorenc-chainguard

gcloud compute target-https-proxies create docstore-https-proxy \
  --url-map=docstore-map \
  --ssl-certificates=docstore-cert \
  --global --project=dlorenc-chainguard

gcloud compute forwarding-rules create docstore-https-rule \
  --address=docstore-ip \
  --target-https-proxy=docstore-https-proxy \
  --ports=443 \
  --global --project=dlorenc-chainguard

# Cloud DNS zone and A record
gcloud dns managed-zones create docstore-dev \
  --dns-name=docstore.dev \
  --description="docstore.dev" \
  --project=dlorenc-chainguard

gcloud dns record-sets create docstore.dev \
  --zone=docstore-dev \
  --type=A \
  --ttl=300 \
  --rrdatas=34.54.166.224 \
  --project=dlorenc-chainguard
```

The domain `docstore.dev` is registered via Cloud Domains in project `dlorenc-chainguard` with its nameservers pointed at the Cloud DNS zone above.

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
- `DOCSTORE_URL` — hardcoded to `https://docstore-efuj4cj54a-uc.a.run.app` (note: `deploy/k8s/ci-worker.yaml` should be updated to `https://docstore.dev` separately)
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
4. Provision the Global HTTPS LB + IAP infrastructure (see "One-time infrastructure" commands in the Authentication section above). This only needs to be done once per project.
5. Push to `main` to trigger the deploy workflow.
6. After deploy, verify with `curl https://docstore.dev/healthz`.
7. Create the first org and repo:
   ```bash
   ds orgs create myorg
   ds repos create myorg myrepo
   ```
8. Assign a bootstrap admin (the `--bootstrap-admin` flag is already set on the Cloud Run service as `dlorenc@chainguard.dev`; update as needed):
   ```bash
   ds roles set you@example.com admin
   ```

## Horizontal scaling

The event broker persists all events to the `event_log` PostgreSQL table and uses `pg_notify` for real-time wake-ups. SSE streams (`GET /events`, `GET /repos/{name}/-/events`) poll `event_log` directly, so every instance sees every event regardless of which instance processed the original mutation. Multiple Cloud Run instances are fully supported.

### CLI authentication (ds login)

After enabling IAP, add the Desktop app OAuth client to IAP's programmatic access allowlist so `ds login` works:

```bash
cat > /tmp/iap-settings.yaml << 'YAML'
access_settings:
  oauth_settings:
    programmatic_clients:
      - <DESKTOP_APP_CLIENT_ID>
YAML

gcloud iap settings set /tmp/iap-settings.yaml \
  --project=<PROJECT> \
  --resource-type=backend-services \
  --service=<BACKEND_SERVICE_NAME>
```

This allows ID tokens with the Desktop app client's audience to pass through IAP. Without this step, IAP rejects CLI tokens with "Invalid JWT audience".
