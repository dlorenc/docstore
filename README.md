# DocStore

DocStore is a server-side version control system for structured documents, built on PostgreSQL. It exposes a REST API and a CLI (`ds`) that work like Git — branches, commits, merges, diffs — but without a local object store. All history lives in the database.

## Why DocStore?

- **No local clone.** Commit and read files from any machine without syncing a full history.
- **Content deduplication.** Files are SHA256-hashed; identical content across branches shares one row.
- **Policy-gated merges.** Embed OPA (Rego) policies and OWNERS files in the repo to enforce review and CI requirements before merge.
- **Built-in CI.** The CI system reads `.docstore/ci.yaml` from `main` and runs checks on every branch commit using BuildKit inside Kata Container microVMs.

## Quick links

| Document | What it covers |
|---|---|
| [docs/getting-started.md](docs/getting-started.md) | Install `ds`, init a workspace, first commit |
| [docs/concepts.md](docs/concepts.md) | Branching model, content addressing, how it differs from git |
| [docs/cli-reference.md](docs/cli-reference.md) | All `ds` commands with flags and examples |
| [docs/api-reference.md](docs/api-reference.md) | Full REST API |
| [docs/deployment.md](docs/deployment.md) | Cloud Run + Cloud SQL + GKE setup |
| [docs/ci.md](docs/ci.md) | CI system: ci-scheduler, ci-worker, `.docstore/ci.yaml` DSL |
| [docs/policy.md](docs/policy.md) | RBAC roles, OPA policy engine, OWNERS files |
| [docs/sdk.md](docs/sdk.md) | Go SDK |
| [CONTRIBUTING.md](CONTRIBUTING.md) | Developer setup, testing, adding handlers |

## Architecture at a glance

```
ds CLI  ──►  Cloud Run (docstore server)  ──►  Cloud SQL (PostgreSQL)
                      │
              webhook outbox ──►  ci-scheduler (GKE)  ──►  ci_jobs table
                                         ▼
                                  ci-worker pods (GKE, Kata CLH)
                                     ── BuildKit / LLB ──►  check results
```

The server is a single stateless binary (`cmd/docstore`) deployed on Cloud Run. All state is in PostgreSQL. The CI system is a separate pair of GKE workloads: `ci-scheduler` receives webhook events and queues jobs; `ci-worker` pods claim and execute jobs inside Kata Container microVMs.
