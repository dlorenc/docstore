# DocStore VCS — Design Document

## Overview

A simplified version control system built on a versioned document store. One long-lived branch (`main`), short-lived feature branches, and pull request workflows. No DAG, no packfiles, no garbage collection.

## Data Model

Eight tables. Documents and file_commits are append-only. Branches are mutable (head_sequence and status are updated in place).

**orgs** is the top-level namespace. Every repo belongs to exactly one org. The org name must equal the first path segment of the repo's full name.

| Column | Type | Notes |
|---|---|---|
| name | text | PK — e.g. `acme` |
| created_at | timestamp | |
| created_by | text | identity that created the org |

**documents** stores immutable file versions. Every save creates a new row.

| Column | Type | Notes |
|---|---|---|
| version_id | uuid | PK |
| path | text | e.g. `src/main.py` |
| content | blob | file contents |
| content_hash | sha256 | dedup + integrity |
| created_at | timestamp | |
| created_by | text | author |

**commits** stores per-commit metadata. One row per atomic commit.

| Column | Type | Notes |
|---|---|---|
| sequence | bigint | PK, globally monotonic |
| message | text | commit message |
| author | text | committer identity (from IAP) |
| created_at | timestamp | |

**file_commits** is the core event log. One row per file change. All rows sharing a `sequence` reference the same commit.

| Column | Type | Notes |
|---|---|---|
| commit_id | uuid | PK, unique per file row |
| sequence | bigint | FK → commits |
| path | text | file that changed |
| version_id | uuid | FK → documents, null = delete |
| branch | text | `main` or `feature/*` |

**repos** are named tenants owned by an org. The name is the full path (e.g. `acme/myrepo` or `acme/team/subrepo`); the owner is always the first path segment.

| Column | Type | Notes |
|---|---|---|
| name | text | PK — full path, e.g. `acme/myrepo` |
| owner | text | FK → orgs, must equal `split_part(name, '/', 1)` |
| created_at | timestamp | |
| created_by | text | |

**branches** are named pointers.

| Column | Type | Notes |
|---|---|---|
| repo | text | FK → repos |
| name | text | branch name (may contain `/`) |
| head_sequence | bigint | latest sequence on this branch |
| base_sequence | bigint | where it forked from main |
| status | enum | active / merged / abandoned |
| created_at | timestamp | |
| created_by | text | identity that created the branch |

Primary key: `(repo, name)`.

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
| body | text | optional review comment |
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

A single `POST /commit` inserts one row into `commits` (allocating the next sequence number, storing the message and author) and multiple rows into `file_commits` that all reference that sequence. The sequence counter increments once per commit, not once per file. This means `sequence` is the commit identity — to see everything in a commit, query `WHERE sequence = :seq`. Materialization is unaffected since `DISTINCT ON (path) ... ORDER BY path, sequence DESC` resolves ties naturally when paths are different.

## Concurrency

All write operations (`POST /commit`, `/merge`, `/rebase`) run inside a transaction that begins with `SELECT ... FOR UPDATE` on the target branch row. This serializes writes per-branch — concurrent commits to different branches don't block each other, while concurrent commits to the same branch serialize cleanly. Merge and rebase lock both the source branch and main, always acquiring the main lock first to prevent deadlocks. The contention window is small (a few row inserts per transaction), and the design's usage pattern of short-lived single-author feature branches means lock contention is minimal in practice.

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

The server loads policies from the materialized `main` tree and caches them in memory. Policies are reloaded whenever main's `head_sequence` advances (on commit or merge to main). This ensures policies are always fresh with no staleness window, at the cost of a small reload on each main write.

The bootstrap/fallback policy (used when no `.docstore/policy/*.rego` files exist) is wide open — all authenticated users can perform all actions, subject only to role checks in Layer 2. Policies are purely additive restrictions. This allows initial setup without a chicken-and-egg problem; the first policy commit then locks things down.

### OWNERS Files

`OWNERS` files are regular versioned documents that can exist at any directory level. Each contains a list of identities authorized to approve changes under that path. Ownership is inherited — a file at `src/OWNERS` covers `src/` and all subdirectories unless overridden by a more specific `OWNERS` file.

At policy evaluation time, the engine reads OWNERS from the materialized `main` tree and builds the `owners` map in the input document. Policies can then require that at least one reviewer is a listed owner for each changed path.

## API

All endpoints are REST. Sequences are returned in every mutation response. List endpoints (`/tree`, `/file/:path/history`, `/commit/:sequence`) accept `?limit=N&after=cursor` for pagination. The cursor is the last `sequence` (or `path` for tree) from the previous page. Default limit is 100.

### URL structure

Org and repo management endpoints are at the top level:

```
POST   /orgs                     create an org
GET    /orgs                     list orgs
GET    /orgs/{org}               get an org
DELETE /orgs/{org}               delete an org (fails if org has repos)
GET    /orgs/{org}/repos         list repos in an org

POST   /repos                    create a repo
GET    /repos                    list all repos
```

All repo-scoped operations use a `/-/` separator to unambiguously separate the (possibly slash-containing) repo name from the endpoint:

```
GET    /repos/acme/myrepo/-/tree
POST   /repos/acme/myrepo/-/commit
GET    /repos/acme/team/subrepo/-/branches
```

The pattern is `/repos/{full-repo-name}/-/{endpoint}`. The full repo name may contain multiple slashes (e.g. `acme/team/subrepo`).

### Org endpoints

`POST /orgs` — Create an org. Body: `{name}`.

`GET /orgs` — List all orgs.

`GET /orgs/{org}` — Get a single org.

`DELETE /orgs/{org}` — Delete an org. Returns `409 Conflict` if the org still has repos.

`GET /orgs/{org}/repos` — List repos belonging to an org.

### Repo endpoints

`POST /repos` — Create a repo. Body: `{owner, name}` where `owner` is the org name and `name` is the repo path within the org (may contain slashes). The full repo identifier is `owner/name`.

`GET /repos` — List all repos.

`GET /repos/{full-name}` — Get a repo by its full name.

`DELETE /repos/{full-name}` — Hard-delete a repo.

### Repo-scoped reads

`GET /repos/{name}/-/tree?branch=main&at=sequence` — Materialize. Returns `[{path, version_id, content_hash}]` for every live file at that sequence. Omit `at` for head.

`GET /repos/{name}/-/file/:path/history?branch=main` — File history. Returns `[{sequence, version_id, message, author, created_at}]` ordered by sequence desc.

`GET /repos/{name}/-/file/:path?branch=main&at=sequence` — Get file content at a point in time.

`GET /repos/{name}/-/diff?branch=feature/x` — PR diff. Returns files changed on the branch relative to its `base_sequence`, plus any conflicting paths on main since that base.

`GET /repos/{name}/-/commit/:sequence` — Show all files changed in a single atomic commit.

`GET /repos/{name}/-/branches` — List all branches. Returns `[{name, head_sequence, base_sequence, status}]`. Supports `?status=active` filter.

`GET /repos/{name}/-/branch/:bname/status` — Evaluate all merge policies for a branch without actually merging. Returns `{mergeable: bool, policies: [{name, pass, reason}]}`. Useful for UI status checks and CI gates.

`GET /repos/{name}/-/branch/:bname/reviews` — List reviews for a branch. Returns `[{id, reviewer, sequence, status, body, created_at}]` ordered by created_at desc.

`GET /repos/{name}/-/branch/:bname/checks` — List check runs for a branch. Returns `[{id, sequence, check_name, status, reporter, created_at}]` ordered by created_at desc.

### Repo-scoped writes

`POST /repos/{name}/-/commit` — Commit one or more file changes atomically. Body: `{branch, files: [{path, content}], message}`. Policy-evaluated: the engine checks the actor's role and the target branch (e.g., direct commits to `main` can be blocked by policy). Allocates a single sequence number, writes one row to commits and one row per file to file_commits (all referencing that sequence), advances the branch head.

`POST /repos/{name}/-/branch` — Create a branch. Body: `{name}`. Policy-evaluated. Sets `base_sequence` to main's current head.

`POST /repos/{name}/-/merge` — Merge a branch into main. Body: `{branch}`. Policy-evaluated: the engine evaluates all merge policies (required reviews, passing checks, OWNERS approval) before proceeding. On policy failure, returns `{policies: [{name, pass, reason}]}` with a 403. **Staleness rule**: the server only includes reviews and check_runs whose `sequence` matches the branch's current `head_sequence` in the policy input. Any new commit to the branch advances `head_sequence`, which automatically invalidates all prior reviews and checks — policies see an empty list until the branch is re-reviewed and re-checked at the new head. The merge uses `base_sequence` from the branches table as the fork point:

1. **Branch changes**: find the latest version of each path changed on the branch since `base_sequence` (`WHERE branch = :branch AND sequence > :base_sequence`, `DISTINCT ON (path) ... ORDER BY path, sequence DESC`).
2. **Main changes**: find the latest version of each path changed on main since `base_sequence` (same query shape against `branch = 'main'`).
3. **Conflict check**: any path that appears in both sets is a conflict — return `{conflicts: [...]}` and abort.
4. **If clean**: insert new `file_commits` rows on main for each branch-changed path (all sharing a single new sequence), advance main's head, mark the branch as merged.

`POST /repos/{name}/-/rebase` — Rebase a branch onto main's current head. Body: `{branch}`. Policy-evaluated. The entire rebase runs in a single transaction. Replays the branch's file_commits grouped by their original sequence numbers, each group getting a new sequence. Updates the branch's `base_sequence` to main's current head. If any replayed path conflicts with a main change, the entire transaction rolls back and returns conflict details — no partial rebase state.

`POST /repos/{name}/-/review` — Submit a review for a branch. Body: `{branch, status, body}` where status is `approved` or `rejected` and body is an optional comment. Policy-evaluated. The review is recorded against the branch's current `head_sequence`. New commits invalidate prior reviews (see staleness rule on `/merge`).

`POST /repos/{name}/-/check` — Report CI check status. Body: `{branch, check_name, status}` where status is `passed` or `failed`. Policy-evaluated: only identities authorized for a given `check_name` can report results (prevents spoofing CI status). Recorded against the branch's current `head_sequence`. New commits invalidate prior checks (see staleness rule on `/merge`).

`DELETE /repos/{name}/-/branch/:bname` — Mark a branch as abandoned. Policy-evaluated.

`POST /repos/{name}/-/purge` — Delete `file_commits` and `commits` rows for branches with status `merged` or `abandoned`. Body: `{older_than}` where `older_than` is a duration (e.g. `"90d"`) — only branches whose last activity is older than this threshold are purged. Also deletes any `documents` rows whose `version_id` is no longer referenced by any remaining `file_commits` row. Policy-evaluated (admin-only). Returns `{branches_purged: N, file_commits_deleted: N, documents_deleted: N}`.

## Client

A CLI that wraps the API. Working directory tracked via a `.docstore` config file containing the remote URL, repo name, and current branch.

```
ds init <remote-url>
ds checkout -b <branch>      # POST /repos/{repo}/-/branch
ds status                     # diff local fs against GET /repos/{repo}/-/tree
ds commit -m "message"        # POST /repos/{repo}/-/commit with all changed files atomically
ds log [path]                 # GET /repos/{repo}/-/file/:path/history or full branch log
ds diff                       # GET /repos/{repo}/-/diff for current branch
ds merge                      # POST /repos/{repo}/-/merge
ds rebase                     # POST /repos/{repo}/-/rebase
ds pull                       # fetch latest tree for current branch, update local files
ds checkout main              # switch branch, GET /repos/{repo}/-/tree to update local files
ds show <sequence> [path]     # GET /repos/{repo}/-/commit/:seq or GET /repos/{repo}/-/file/:path?at=N
```

The CLI stores the base server URL and repo name (e.g. `default/default`) separately in `.docstore/config.json`. The `ds init` command accepts the repo name either embedded in the URL (`https://host/repos/acme/myrepo`) or via the `--repo` flag. The default repo is `default/default`.

**`ds pull` flow**:

1. Check for uncommitted local changes (`ds status`). If any exist, error with "uncommitted changes — commit or discard first".
2. `GET /tree?branch=<current>` to fetch the latest materialized tree from the server.
3. Compare the remote tree against the local `state.json` to find files that are new, changed, or deleted on the remote.
4. Download content for new/changed files via `GET /file/:path?branch=<current>`.
5. Write updated files to disk, delete locally any files removed on the remote.
6. Update `state.json` with the new head sequence and content hashes.

This covers both cases: pulling someone else's commits to your feature branch, and pulling the latest main after switching to it.

**Local state**: The client keeps a `.docstore/state.json` with the current branch, the last synced sequence, and content hashes of local files. `ds status` compares local hashes to the last synced tree to detect changes without hitting the server.

**Conflict resolution**: On merge or rebase conflicts, the API returns `{conflicts: [{path, main_version_id, branch_version_id}]}`. The client writes both versions to disk as `file.main` and `file.branch`, the user edits, and runs `ds resolve <path>` which commits the resolved file.

## What's Intentionally Left Out

Stashing, cherry-pick, tags, submodules, hooks, shallow clones, worktrees, reflog, interactive rebase, signed commits, audit logging, rate limiting. All of these can be added later without changing the core model if needed.