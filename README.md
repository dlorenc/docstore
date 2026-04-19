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

## Quickstart

This walkthrough takes you from a blank server to a repo with CI, policies, and collaborators.

### 1. Install the CLI

```bash
go install github.com/dlorenc/docstore/cmd/ds@latest
```

Or build from source:

```bash
make build-ds    # produces bin/ds
```

### 2. Create an org and repo

Repos live under orgs. Create the org first, then the repo:

```bash
ds orgs create acme
ds repos create acme myrepo
```

The full repo name is `acme/myrepo`. You can nest further (`acme/team/myrepo`) if needed.

### 3. Initialize a workspace and make your first commit

```bash
# Initialize a local workspace pointing at the new repo
ds init https://docstore.example.com/repos/acme/myrepo --author alice@example.com

# Add some files
echo "# My Project" > README.md
mkdir -p src
echo 'package main\n\nfunc main() {}' > src/main.go

# See what will be committed
ds status

# Commit directly to main (allowed until policies are in place)
ds commit -m "initial commit"
```

`ds init` creates a `.docstore/` config directory and syncs the current `main` tree locally. The first commit lands directly on `main` — no branch required.

### 4. Push code via a branch

All changes after the initial setup flow through branches. Create a branch, do your work, open a proposal, get it reviewed, and merge.

```bash
# Create and switch to a new branch
ds checkout -b feature/add-retry

# Edit files
echo "retry logic here" >> src/main.go

# Commit to the branch
ds commit -m "add retry on transient errors"

# Open a proposal — this signals the branch is ready for review and triggers CI
ds proposal open --title "Add retry logic for transient errors"

# See your proposal
ds proposal list
```

While the proposal is open, collaborators can review:

```bash
# A collaborator reviews the branch
ds review --status approved --body "LGTM"
```

Once CI passes and reviews are satisfied, merge:

```bash
ds merge
```

`ds merge` evaluates all active policies (CI, reviews, etc.) and either merges into `main` or exits with a list of failing policy checks.

### 5. Set up CI

Create a `.docstore/ci.yaml` on a branch. CI runs automatically on every commit and every time a proposal is opened or updated.

```bash
ds checkout -b chore/add-ci

mkdir -p .docstore
cat > .docstore/ci.yaml << 'EOF'
on:
  push:
    branches: [main]          # post-submit: run after every merge to main
  proposal:
    base_branches: [main]     # pre-submit: run when a proposal targets main

checks:
  - name: ci/test
    image: golang:1.24
    steps:
      - go test ./...
      - go vet ./...

  - name: ci/deploy
    image: google/cloud-sdk:slim
    if: "event.type == 'push' && event.branch == 'main'"
    steps:
      - ./deploy.sh
EOF

ds commit -m "add CI config"
ds proposal open --title "Add CI"
```

The `ci/deploy` check only runs on post-submit pushes to `main` (not on proposal pre-submit runs), thanks to the `if:` condition.

See [docs/proposals-and-ci-triggers.md](docs/proposals-and-ci-triggers.md) for the full trigger reference including schedules, `if:` expressions, and all event fields.

### 6. Set up merge policies

Policies are Rego files at `.docstore/policy/*.rego` on `main`. They gate every `ds merge`. Start with requiring an approved review and a passing CI check:

```bash
ds checkout -b chore/add-policies

mkdir -p .docstore/policy

cat > .docstore/policy/require_review.rego << 'EOF'
package docstore.require_review

import rego.v1

default allow := false

allow if {
    some rev in input.reviews
    rev.status == "approved"
}

reason := "at least one approved review is required"
EOF

cat > .docstore/policy/ci_must_pass.rego << 'EOF'
package docstore.ci_must_pass

import rego.v1

default allow := false

allow if {
    some check in input.check_runs
    check.check_name == "ci/test"
    check.status == "passed"
}

reason := "ci/test must pass before merging"
EOF

ds commit -m "add review and CI policies"
ds proposal open --title "Add merge policies"
```

Once these policies are merged to `main`, every subsequent merge — including the merge of this branch — requires a passing review and CI. Bootstrap mode (no policies on `main` yet) allows the first policy to land without pre-approval.

See [docs/policy.md](docs/policy.md) for the full input schema, OWNERS file support, and more example policies.

### 7. Invite collaborators

Grant roles to give others access:

```bash
# Grant bob write access (can commit and open proposals)
ds roles set bob@example.com writer

# Grant carol maintainer access (can also merge and manage roles below their level)
ds roles set carol@example.com maintainer

# Check who has access
ds roles
```

Valid roles: `reader` (read-only), `writer` (commit + proposal), `maintainer` (merge + role management), `admin` (full access).

Collaborators initialize their own workspaces pointing at the same repo:

```bash
# Bob on his machine
ds init https://docstore.example.com/repos/acme/myrepo --author bob@example.com
```

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
