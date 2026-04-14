# DocStore VCS — Design Document

## Overview

A simplified version control system built on a versioned document store. One long-lived branch (`main`), short-lived feature branches, and pull request workflows. No DAG, no packfiles, no garbage collection.

## Data Model

Six tables. All writes are appends — nothing is mutated.

**documents** stores immutable file versions. Every save creates a new row.

| Column | Type | Notes |
|---|---|---|
| version_id | uuid | PK |
| path | text | e.g. `src/main.py` |
| content | blob | file contents |
| content_hash | sha256 | dedup + integrity |
| created_at | timestamp | |
| created_by | text | author |

**file_commits** is the core event log. One row per file change. A globally incrementing `sequence` provides total ordering and serves as the commit identity — all rows sharing a sequence number are one atomic commit.

| Column | Type | Notes |
|---|---|---|
| commit_id | uuid | PK, unique per file row |
| sequence | bigint | globally monotonic, shared across files in one atomic commit |
| path | text | file that changed |
| version_id | uuid | FK → documents, null = delete |
| branch | text | `main` or `feature/*` |
| message | text | same across all rows in a sequence |
| author | text | same across all rows in a sequence |
| created_at | timestamp | |

**branches** are named pointers.

| Column | Type | Notes |
|---|---|---|
| name | text | PK |
| head_sequence | bigint | latest sequence on this branch |
| base_sequence | bigint | where it forked from main |
| status | enum | active / merged / abandoned |

**roles** maps identities to coarse-grained permissions.

| Column | Type | Notes |
|---|---|---|
| identity | text | PK — email or service account |
| role | enum | reader, writer, maintainer, admin |

**reviews** records approvals (or rejections) scoped to a branch at a specific head sequence.

| Column | Type | Notes |
|---|---|---|
| id | uuid | PK |
| branch | text | FK → branches |
| reviewer | text | identity of the reviewer |
| sequence | bigint | branch head_sequence at time of review |
| status | enum | approved / rejected / dismissed |
| created_at | timestamp | |

**check_runs** stores external CI status reports for a branch at a specific head sequence.

| Column | Type | Notes |
|---|---|---|
| id | uuid | PK |
| branch | text | FK → branches |
| sequence | bigint | branch head_sequence at time of report |
| check_name | text | e.g. `ci/build`, `ci/lint` |
| status | enum | pending / passed / failed |
| reporter | text | identity of the reporting service account |
| created_at | timestamp | |

**Indexes**: `(branch, path, sequence DESC)` for both file history and tree materialization (the `DISTINCT ON (path) ... ORDER BY path, sequence DESC` query walks each path group and grabs the first matching row, making materialization O(unique paths) rather than O(total commits)), `(content_hash)` on documents for dedup, `(sequence)` for atomic commit lookup, `(branch, sequence)` on reviews for looking up approvals by branch head, `(branch, sequence, check_name)` on check_runs for policy evaluation.

## Content Dedup

On `POST /commit`, the server hashes each file's content and checks the `(content_hash)` index on documents. If a matching row exists, it reuses the existing `version_id` — no new row is inserted into documents. The `file_commits` row references the existing `version_id`. This means `version_id` is content-addressed: two files with identical content share the same `version_id` and blob storage.

## Atomic Commits

A single `POST /commit` with multiple files produces multiple `file_commits` rows that all share the same `sequence` number. The sequence counter increments once per commit, not once per file. This means `sequence` is the commit identity — to see everything in a commit, query `WHERE sequence = :seq`. Materialization is unaffected since `DISTINCT ON (path) ... ORDER BY path, sequence DESC` resolves ties naturally when paths are different.

## Concurrency

All write operations (`POST /commit`, `/merge`, `/rebase`) run inside a transaction that begins with `SELECT ... FOR UPDATE` on the target branch row. This serializes writes per-branch — concurrent commits to different branches don't block each other, while concurrent commits to the same branch serialize cleanly. Merge and rebase lock both the source branch and main. The contention window is small (a few row inserts per transaction), and the design's usage pattern of short-lived single-author feature branches means lock contention is minimal in practice.

## Authentication

Identity is provided by GCP Identity-Aware Proxy (IAP). The server never manages credentials or sessions — IAP sits in front of the server and handles all authentication.

- **Users** authenticate via `gcloud` OIDC ID tokens. IAP validates the token and forwards the verified identity in the `X-Goog-IAP-JWT-Assertion` header.
- **Service accounts** (CI bots, automation) authenticate via GCP workload identity. Same header, same validation path.
- The `author` field on commits is derived from the verified IAP identity — clients cannot spoof it.
- Unauthenticated requests never reach the server.

## Authorization & Policy Engine

Three layers control what an authenticated identity can do.

**Layer 1 — IAP access.** IAP controls who can reach the server at all. This is a binary gate: you're in or you're not.

**Layer 2 — Roles table.** The `roles` table maps identities to coarse-grained roles: `reader`, `writer`, `maintainer`, `admin`. Readers can call any GET endpoint. Writers can commit to non-main branches. Maintainers can merge. Admins can manage roles and policies. This provides a fast, cheap first check before evaluating policies.

**Layer 3 — Policy engine.** An embedded [OPA](https://www.openpolicyagent.org/) instance evaluates Rego policies for fine-grained access control. Every write operation assembles an input document and evaluates it against the loaded policies. The engine returns allow/deny plus human-readable reasons.

### Policy Evaluation

Every write endpoint (`/commit`, `/merge`, `/review`, `/check`, `/branch`) assembles an input document and evaluates it before proceeding. The input document schema:

```json
{
  "action": "merge",
  "actor": "alice@example.com",
  "actor_roles": ["maintainer"],
  "branch": "feature/new-api",
  "changed_paths": ["src/api.go", "src/api_test.go"],
  "reviews": [
    {"reviewer": "bob@example.com", "status": "approved", "sequence": 42}
  ],
  "check_runs": [
    {"check_name": "ci/build", "status": "passed", "reporter": "ci-bot@project.iam.gserviceaccount.com"}
  ],
  "owners": {
    "src/": ["bob@example.com", "carol@example.com"]
  },
  "head_sequence": 42
}
```

Fields vary by action — `reviews` and `check_runs` are only populated for `/merge`, `changed_paths` is populated for `/commit` and `/merge`, etc.

### Policy Storage

Policies live in `.docstore/policy/*.rego` files in the repo on `main`. They are versioned documents like any other file and go through the same branch/review/merge workflow. This means policy changes require review and approval just like code changes.

The server loads policies from the materialized `main` tree. A bootstrap/fallback policy is embedded in the server config for initial setup and disaster recovery.

### OWNERS Files

`OWNERS` files are regular versioned documents that can exist at any directory level. Each contains a list of identities authorized to approve changes under that path. Ownership is inherited — a file at `src/OWNERS` covers `src/` and all subdirectories unless overridden by a more specific `OWNERS` file.

At policy evaluation time, the engine reads OWNERS from the materialized `main` tree and builds the `owners` map in the input document. Policies can then require that at least one reviewer is a listed owner for each changed path.

## API

All endpoints are REST. Sequences are returned in every mutation response. List endpoints (`/tree`, `/file/:path/history`, `/commit/:sequence`) accept `?limit=N&after=cursor` for pagination. The cursor is the last `sequence` (or `path` for tree) from the previous page. Default limit is 100.

**Reads**

`GET /tree?branch=main&at=sequence` — Materialize. Returns `[{path, version_id, content_hash}]` for every live file at that sequence. Omit `at` for head.

`GET /file/:path/history?branch=main` — File history. Returns `[{sequence, version_id, message, author, created_at}]` ordered by sequence desc.

`GET /file/:path?branch=main&at=sequence` — Get file content at a point in time.

`GET /diff?branch=feature/x` — PR diff. Returns files changed on the branch relative to its `base_sequence`, plus any conflicting paths on main since that base.

`GET /commit/:sequence` — Show all files changed in a single atomic commit.

`GET /branches` — List all branches. Returns `[{name, head_sequence, base_sequence, status}]`. Supports `?status=active` filter.

`GET /branch/:name/status` — Evaluate all merge policies for a branch without actually merging. Returns `{mergeable: bool, policies: [{name, pass: bool, reason}]}`. Useful for UI status checks and CI gates.

**Writes**

`POST /commit` — Commit one or more file changes atomically. Body: `{branch, files: [{path, content}], message}`. Policy-evaluated: the engine checks the actor's role and the target branch (e.g., direct commits to `main` can be blocked by policy). Allocates a single sequence number, writes to documents and file_commits (one row per file, all sharing that sequence), advances the branch head.

`POST /branch` — Create a branch. Body: `{name}`. Policy-evaluated. Sets `base_sequence` to main's current head.

`POST /merge` — Merge a branch into main. Body: `{branch}`. Policy-evaluated: the engine evaluates all merge policies (required reviews, passing checks, OWNERS approval) before proceeding. On policy failure, returns `{policies: [{name, pass, reason}]}` with a 403. The merge uses `base_sequence` from the branches table as the fork point:

1. **Branch changes**: find the latest version of each path changed on the branch since `base_sequence` (`WHERE branch = :branch AND sequence > :base_sequence`, `DISTINCT ON (path) ... ORDER BY path, sequence DESC`).
2. **Main changes**: find the latest version of each path changed on main since `base_sequence` (same query shape against `branch = 'main'`).
3. **Conflict check**: any path that appears in both sets is a conflict — return `{conflicts: [...]}` and abort.
4. **If clean**: insert new `file_commits` rows on main for each branch-changed path (all sharing a single new sequence), advance main's head, mark the branch as merged.

`POST /rebase` — Rebase a branch onto main's current head. Body: `{branch}`. Policy-evaluated. Replays the branch's file_commits grouped by their original sequence numbers, each group getting a new sequence. Updates the branch's `base_sequence` to main's current head. Fails with conflict details if any replayed path was also changed on main.

`POST /review` — Submit a review for a branch. Body: `{branch, status}` where status is `approved` or `rejected`. Policy-evaluated. The review is recorded against the branch's current `head_sequence` — if new commits are pushed, previous approvals are scoped to the old sequence and policies can require re-review.

`POST /check` — Report CI check status. Body: `{branch, check_name, status}` where status is `passed` or `failed`. Policy-evaluated: only identities authorized for a given `check_name` can report results (prevents spoofing CI status). Recorded against the branch's current `head_sequence`.

`DELETE /branch/:name` — Mark a branch as abandoned. Policy-evaluated.

## Client

A CLI that wraps the API. Working directory tracked via a `.docstore` config file containing the remote URL and current branch.

```
ds init <remote-url>
ds checkout -b <branch>      # POST /branch
ds status                     # diff local fs against GET /tree
ds commit -m "message"        # POST /commit with all changed files atomically
ds log [path]                 # GET /file/:path/history or full branch log
ds diff                       # GET /diff for current branch
ds merge                      # POST /merge
ds rebase                     # POST /rebase
ds checkout main              # switch branch, GET /tree to update local files
ds show <sequence> [path]     # GET /tree?at=N or GET /file/:path?at=N
```

**Local state**: The client keeps a `.docstore/state.json` with the current branch, the last synced sequence, and content hashes of local files. `ds status` compares local hashes to the last synced tree to detect changes without hitting the server.

**Conflict resolution**: On merge or rebase conflicts, the API returns `{conflicts: [{path, main_version_id, branch_version_id}]}`. The client writes both versions to disk as `file.main` and `file.branch`, the user edits, and runs `ds resolve <path>` which commits the resolved file.

## What's Intentionally Left Out

Stashing, cherry-pick, tags, submodules, hooks, shallow clones, worktrees, reflog, interactive rebase, signed commits, audit logging, rate limiting. All of these can be added later without changing the core model if needed.