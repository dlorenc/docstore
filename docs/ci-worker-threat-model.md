# CI Worker Threat Model

This document describes the security boundary for docstore's CI execution
environment and the mitigations in place to limit the blast radius of malicious
user code.

## Trust boundary

The `ci-worker` binary is trusted. Everything that runs after it hands execution
to BuildKit — user-defined build steps executing inside the Kata CLH
microVM — is treated as adversarial.

```
ci-worker binary (trusted)
  ├── claims job from ci-scheduler (K8s SA proof)
  ├── fetches ci.yaml via request_token
  ├── obtains presigned archive URL via request_token
  └── hands off to BuildKit ← trust boundary
        └── user build steps (untrusted)
              ├── reads request_token from /run/secrets/
              └── has host network namespace (--oci-worker-net=host)
```

## Credentials available inside the VM

| Credential | How obtained | Notes |
|---|---|---|
| `request_token` | BuildKit secret mount at `/run/secrets/docstore_oidc_request_token` | Readable by any build step |
| OIDC token URL | BuildKit secret mount at `/run/secrets/docstore_oidc_request_url` | Needed to exchange request_token for JWT |
| GCP metadata server | Plain HTTP to `169.254.169.254` | See mitigations below |
| Docker daemon | `tcp://localhost:2375`, unauthenticated; `DOCKER_HOST` is set | Gives full container control within the VM |
| Cluster-internal network | `--oci-worker-net=host` gives build containers the VM's network namespace | Can reach cluster services |

## What the request_token can do

The `request_token` is a short-lived opaque token bound to a single CI job. It
is accepted by endpoints on the docstore server and the ci-scheduler. All
docstore endpoints enforce that `job.Repo` matches the URL path repo:

| Server | Endpoint | Purpose |
|---|---|---|
| docstore | `POST /repos/{repo}/-/archive/presign` | Get presigned source archive URL |
| docstore | `POST /repos/{repo}/-/check/{name}/logs` | Upload check run log content |
| docstore | `GET /repos/{repo}/-/ci/config` | Fetch `.docstore/ci.yaml` for the job's branch/sequence |
| docstore | `POST /repos/{repo}/-/check` | Report check run status |
| ci-scheduler | `POST /jobs/{id}/heartbeat` | Keep job alive (cluster-internal only) |
| ci-scheduler | `POST /jobs/{id}/complete` | Report job completion (cluster-internal only) |

The ci-scheduler endpoints are only reachable from within the cluster
(`ci-scheduler.docstore-ci.svc.cluster.local`). Both validate the request_token
and enforce that the token's job ID matches the URL `{id}`.

The request_token can also be exchanged at the ci-oidc endpoint for a
short-lived OIDC JWT. The audience determines what the JWT can access:

- `aud=ci-registry` — authenticate to the BuildKit layer cache registry
- `aud=docstore` — authenticate to the docstore API (see below)

## OIDC JWT (aud=docstore) permissions

The OIDC JWT is validated by the docstore server. After validation, the request
is checked against an allowlist before reaching the inner API mux:

1. The URL path repo must match `jobID.Repo` — no cross-repo access.
2. The endpoint must be permitted by the job's declared permissions.

Default permissions (no `permissions:` block in ci.yaml): `checks: write` only,
which allows `POST /repos/{own-repo}/-/check`.

Elevated permissions can be declared in `.docstore/ci.yaml`:

```yaml
permissions:
  contents: write    # commit, branch, merge, rebase, purge
  proposals: write   # open proposals, post reviews/comments
  issues: write      # create/close/comment on issues
  releases: write    # create/delete releases
  ci: write          # trigger CI runs on own repo
```

**Permissions are evaluated at job dispatch time, not at request time.** For
proposal (PR) jobs, permissions are read from the *target branch* (base branch)
ci.yaml, not the source branch. A PR cannot grant itself elevated permissions —
they only take effect after the permission change is reviewed and merged. See
[ci.md](ci.md) for details.

The `on:` trigger filter (which controls whether CI runs at all for a given
proposal) is evaluated from the *source branch* ci.yaml. A PR author can
therefore suppress or expand which base branches trigger CI on their branch, but
this does not affect permissions, which remain base-branch-gated.

## GCP metadata server

The `entrypoint-worker.sh` adds an iptables rule to block outbound traffic to
`169.254.169.254` before buildkitd and dockerd start:

```sh
iptables -I OUTPUT -d 169.254.169.254 -j DROP
```

**This is not a hard security boundary.** Because build steps run with host
networking and the Docker daemon is unauthenticated, a sufficiently motivated
build step can remove this rule and reach the metadata server:

```sh
docker run --net=host --cap-add=NET_ADMIN alpine \
  sh -c "iptables -F OUTPUT && curl -H 'Metadata-Flavor: Google' \
  http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token"
```

The real security boundary is the ci-runner GCP service account's IAM grants,
which are intentionally minimal (see below). The iptables rule is defense-in-depth.

## ci-runner GCP service account permissions

The `ci-runner@dlorenc-chainguard.iam.gserviceaccount.com` SA is bound to the
`ci-worker` Kubernetes service account via Workload Identity. Its grants are:

| Scope | Role | Rationale |
|---|---|---|
| Project | `roles/artifactregistry.reader` | Pull the ci-worker container image |

No other project-level roles. No bucket-level grants.

Notably absent and intentionally so:
- **No `roles/cloudsql.client`** — ci-worker talks to ci-scheduler over HTTP; it
  never connects to the database directly.
- **No GCS access** — log writes go through the docstore server's
  `request_token`-gated endpoint; ci-worker has no direct GCS dependency.

## ci-registry cache access

The BuildKit layer cache registry uses a separate SA
(`ci-registry@dlorenc-chainguard.iam.gserviceaccount.com`) with
`roles/storage.objectAdmin` on the cache bucket. Access is scoped at two levels:

1. **Org-level**: the OIDC JWT audience `ci-registry` is required.
2. **Repo-level**: `auth.go` enforces exact repo equality — a token for
   `acme/repo-a` can only push/pull `acme/repo-a:*` refs, not `acme/repo-b:*`.

## K8s service account token

The K8s SA token for the ci-worker pod is used to claim jobs from ci-scheduler
(k8sproof validation). The scheduler enforces one-claim-per-pod: once a pod has
claimed a job, its SA token cannot be used to claim another. A malicious build
step that steals the SA token and calls `/claim` will receive a rejection.

## What is NOT reachable

- Other tenants' `request_token`s or source archives — separate Kata VMs, no
  state sharing between jobs
- The OIDC JWT signing key — lives in GCP KMS, never touches the VM
- Cross-repo API operations — enforced at the OIDC JWT allowlist gate
- Other tenants' presigned archive URLs — `job.Repo == URL repo` enforced in
  the presign handler
- Cross-org ci-registry operations — enforced in `auth.go`
- Cloud SQL — ci-runner SA has no `cloudsql.client` grant
- Other tenants' build logs — ci-runner SA has no GCS grants; log access goes
  through the docstore server which enforces repo-level authorization

## Residual risks and future work

- **iptables bypass**: a privileged build step with Docker daemon access can
  remove the metadata server block. Mitigated by minimal SA permissions. Long-term
  fix: run buildkitd/dockerd as a separate less-privileged process, or use a
  network policy at the Kata VM level.
- **Cluster-internal network**: host networking gives build steps access to
  cluster services. The ci-scheduler and docstore server do not accept requests
  from arbitrary cluster workloads, but this is worth hardening with NetworkPolicy.
- **Cache poisoning within same org**: repo-level scoping in ci-registry
  prevents cross-repo cache poisoning. Cache integrity relies on BuildKit's
  content-addressable layer verification.
