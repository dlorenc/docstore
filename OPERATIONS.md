# DocStore — Operations Guide

This document covers deployment, configuration, API reference, database schema, and day-to-day operations for running a DocStore server.

---

## Architecture

```
┌─────────────────────────────────────────────────┐
│  Cloud Run (or any container runtime)           │
│                                                 │
│  cmd/docstore/main.go                           │
│    → IAPMiddleware (validate GCP IAP JWTs)      │
│    → RBACMiddleware (repo-scoped role checks)   │
│    → HTTP handlers (server package)             │
│                                                 │
└──────────────────────┬──────────────────────────┘
                       │
                       ▼
             Cloud SQL (PostgreSQL)
             ┌─────────────────────┐
             │ orgs                │
             │ repos               │
             │ branches            │
             │ commits             │
             │ documents           │
             │ file_commits        │
             │ roles               │
             │ reviews             │
             │ check_runs          │
             └─────────────────────┘
```

**Key design points:**

- All VCS state is in Postgres; there is no local disk state on the server
- Content is deduplicated: identical file bytes are stored once per repo (`documents` table, content-addressed by SHA256)
- Sequence numbers are globally monotonic within a repo (`commits` table uses `BIGSERIAL`)
- Authentication is delegated to GCP IAP; the server validates the `X-Goog-IAP-JWT-Assertion` header
- RBAC is enforced per-repo via the `roles` table

---

## Building

### Standard build

```bash
make build      # produces bin/docstore
make build-ds   # produces bin/ds (CLI client)
```

### Docker / ko

A `Dockerfile` is included for standard Docker builds:

```bash
docker build -t docstore .
```

The image uses a two-stage build (Go 1.25 builder → Alpine final) and sets `ENTRYPOINT ["docstore"]`.

For Cloud Run deployments you can also use [ko](https://ko.build/):

```bash
ko build ./cmd/docstore
```

---

## Deployment: Cloud Run + Cloud SQL

### Minimum required setup

1. **Cloud SQL (PostgreSQL)**: provision a Cloud SQL PostgreSQL instance
2. **Cloud Run service**: deploy the container image
3. **IAP**: enable Identity-Aware Proxy on the Cloud Run service (or the load balancer in front of it)

### Environment variables

| Variable        | Required | Description                                                                          |
|----------------|----------|--------------------------------------------------------------------------------------|
| `DATABASE_URL`  | Yes      | PostgreSQL DSN, e.g. `postgres://user:pass@host/dbname?sslmode=require`             |
| `PORT`          | No       | Port to listen on (default: `8080`)                                                  |
| `DEV_IDENTITY`  | No       | Bypass IAP and use this identity for all requests (local dev / container testing only) |
| `BOOTSTRAP_ADMIN` | No     | Identity granted admin access to repos that have no admin assigned yet             |

### Flags (command-line)

| Flag                | Description                                                              |
|--------------------|--------------------------------------------------------------------------|
| `--dev-identity`    | Same as `DEV_IDENTITY` env var; bypasses IAP JWT validation              |
| `--bootstrap-admin` | Same as `BOOTSTRAP_ADMIN` env var; grants bootstrap admin access         |

The flag takes precedence over the env var; the env var is only used as a fallback when the flag is not set (empty string).

### Example Cloud Run deployment

```bash
gcloud run deploy docstore \
  --image gcr.io/my-project/docstore \
  --set-env-vars DATABASE_URL="postgres://..." \
  --set-env-vars BOOTSTRAP_ADMIN="alice@example.com" \
  --platform managed \
  --region us-central1 \
  --no-allow-unauthenticated   # IAP handles auth
```

---

## Migrations

Migrations are managed with [golang-migrate](https://github.com/golang-migrate/migrate) and embedded directly in the binary via `//go:embed`.

**Migrations run automatically on startup.** `db.RunMigrations` is called before the HTTP server starts. It is idempotent — if all migrations are already applied, it returns without error.

Migration files live in `internal/db/migrations/` and follow the naming convention:

```
000001_initial_schema.up.sql
000001_initial_schema.down.sql
```

### Adding a new migration

1. Create two files in `internal/db/migrations/`:
   ```
   000002_your_description.up.sql    ← forward migration
   000002_your_description.down.sql  ← rollback (can be empty if irreversible)
   ```
2. Write the SQL
3. The next server restart will apply it automatically

Rollbacks must be applied manually using the `migrate` CLI tool if needed:

```bash
migrate -database "$DATABASE_URL" -path internal/db/migrations down 1
```

### Schema reset (destructive)

Use this procedure when a migration has been applied in-place to an existing migration file (rather than added as a new numbered migration). All data will be lost.

1. Reset the database:
   ```bash
   DATABASE_URL=<your-url> make db-reset
   ```
2. Deploy or restart the server — migrations re-run automatically on startup via m.Up().

The db-reset target drops and recreates the public schema using psql. This removes all tables including schema_migrations, so the server treats the database as fresh on next start.

---

## IAP Authentication

In production, all non-health-check requests must carry a valid GCP IAP JWT in the `X-Goog-IAP-JWT-Assertion` header. The server:

1. Fetches IAP public keys from `https://www.gstatic.com/iap/verify/public_key-jwk` (cached for 1 hour)
2. Verifies the RS256 JWT signature
3. Extracts the `email` claim as the caller's identity
4. Rejects the request with `401 Unauthorized` if the token is missing, invalid, or expired

The server does **not** require callers to set `X-DocStore-Identity` — IAP identity takes priority. Any `X-DocStore-Identity` header sent by the client is used only on the non-production dev-identity path.

### Dev / local bypass

Set `--dev-identity alice@example.com` (or `DEV_IDENTITY=alice@example.com`) to skip JWT validation entirely. **Never use this in production.**

---

## RBAC

Access control is enforced at the repo level. Each identity has one role per repo, stored in the `roles` table.

### Roles

| Role          | Permissions                                                                                  |
|--------------|----------------------------------------------------------------------------------------------|
| `reader`      | GET all endpoints within the repo                                                            |
| `writer`      | reader + POST /commit (to non-main branches only)                                            |
| `maintainer`  | writer + POST /commit to any branch + POST /branch, /merge, /rebase + DELETE /branch/*      |
| `admin`       | maintainer + GET/PUT/DELETE /roles                                                           |

Writers cannot commit directly to `main`; they must use a branch and request a merge (performed by a maintainer or admin).

### Bootstrap admin

The `--bootstrap-admin` flag (or `BOOTSTRAP_ADMIN` env var) grants a specified identity full admin access to any repo that has no admin yet. Once a repo gains an admin via `PUT /repos/:name/roles/:identity`, the bootstrap flag is ignored for that repo.

**Bootstrap admin procedure:**

```bash
# 1. Deploy with --bootstrap-admin set to your identity
# 2. Create an org and repo (the default org + repo are seeded by migration)
curl -X POST https://docstore.example.com/orgs \
  -H "Content-Type: application/json" \
  -d '{"name": "acme"}'

curl -X POST https://docstore.example.com/repos \
  -H "Content-Type: application/json" \
  -d '{"owner": "acme", "name": "myrepo"}'

# 3. Grant yourself (or someone) admin access
curl -X PUT https://docstore.example.com/repos/acme/myrepo/-/roles/alice@example.com \
  -H "Content-Type: application/json" \
  -d '{"role": "admin"}'

# 4. Now the bootstrap identity loses special access for that repo.
# 5. Optionally redeploy without --bootstrap-admin.
```

---

## OPA Policy Engine

DocStore embeds [Open Policy Agent](https://www.openpolicyagent.org/) for fine-grained merge access control. Policies are Rego files stored in the repo itself under `.docstore/policy/`.

### Writing Policies

Every policy file must:
1. Use `package docstore.<name>` (e.g. `package docstore.require_review`)
2. Define a boolean `allow` rule (defaults to `false`)
3. Optionally define a `reason` string rule for denial messages

Use OPA v1 syntax (`import rego.v1`; `allow if { ... }`).

**Example — require at least one approval:**

```rego
package docstore.require_review

import rego.v1

default allow := false

allow if {
    some rev in input.reviews
    rev.status == "approved"
}

reason := "at least one review approval required before merging"
```

**Example — require a passing CI check:**

```rego
package docstore.require_ci

import rego.v1

default allow := false

allow if {
    some cr in input.check_runs
    cr.check_name == "ci/build"
    cr.status == "passed"
}

reason := "ci/build must pass before merging"
```

### Policy Input Schema

On `POST /merge` (and `GET /branch/:name/status`), the server passes this input to every policy:

```json
{
  "action": "merge",
  "actor": "alice@example.com",
  "actor_roles": ["maintainer"],
  "repo": "acme/myrepo",
  "branch": "feature/new-api",
  "changed_paths": ["src/api.go", "src/api_test.go"],
  "reviews": [
    {"reviewer": "bob@example.com", "status": "approved", "sequence": 42}
  ],
  "check_runs": [
    {"check_name": "ci/build", "status": "passed", "sequence": 42}
  ],
  "owners": {
    "src/api.go": ["bob@example.com", "carol@example.com"],
    "src/api_test.go": ["bob@example.com", "carol@example.com"]
  },
  "head_sequence": 42,
  "base_sequence": 38
}
```

- `reviews` and `check_runs` include **only current-head** entries; stale entries (from before the latest commit) are excluded automatically.
- `owners` is keyed by file path. The server resolves each changed path to its effective owners via longest-prefix directory matching.

### Bootstrap Mode

When no `.docstore/policy/*.rego` files exist on `main`, all merges are permitted (subject only to role checks). This avoids a chicken-and-egg setup problem. Once the first policy file is committed, it takes effect immediately.

### Cache Invalidation

The server caches compiled policies and OWNERS maps per repo. The cache is invalidated after:
- A successful `POST /merge` to main
- A `POST /commit` that targets `main` directly

### OWNERS Files

`OWNERS` files live at any directory level in the repo. Format: one identity per line; `#` starts a comment; blank lines are ignored.

```
# src/OWNERS
alice@example.com
bob@example.com
```

Inheritance works by longest-prefix match: `src/pkg/OWNERS` takes precedence over `src/OWNERS` for files under `src/pkg/`. The root `OWNERS` (at repo root) is the fallback for all unmatched paths.

---

## API Reference

All endpoints (except `/healthz`) require authentication. Errors are returned as:

```json
{"error": "human-readable message"}
```

### URL structure

Org and repo management are at the top level. All repo-scoped operations use the `/-/` separator to unambiguously separate the full repo name (which may contain slashes) from the endpoint:

```
POST   /orgs                             create an org
GET    /orgs                             list orgs
GET    /orgs/{org}                       get an org
DELETE /orgs/{org}                       delete an org
GET    /orgs/{org}/repos                 list repos in an org

POST   /repos                            create a repo
GET    /repos                            list all repos
GET    /repos/{full-name}                get a repo (full-name may contain slashes)
DELETE /repos/{full-name}                delete a repo

GET    /repos/acme/myrepo/-/tree         example repo-scoped endpoint
POST   /repos/acme/myrepo/-/commit       example repo-scoped endpoint
```

---

### Health

#### `GET /healthz`

No authentication required. Returns `200 OK` when the server is running.

```json
{"status": "ok"}
```

---

### Org Management

#### `POST /orgs`

Create a new org.

**Request body:**
```json
{"name": "acme"}
```

**Response `201 Created`:**
```json
{
  "name": "acme",
  "created_at": "2024-01-15T12:00:00Z",
  "created_by": "alice@example.com"
}
```

**Errors:** `409 Conflict` if org already exists.

---

#### `GET /orgs`

List all orgs.

**Response `200 OK`:**
```json
{
  "orgs": [
    {"name": "acme", "created_at": "...", "created_by": "..."}
  ]
}
```

---

#### `GET /orgs/{org}`

Get a single org.

**Response `200 OK`:** same shape as an element of `GET /orgs`.

**Errors:** `404 Not Found`.

---

#### `DELETE /orgs/{org}`

Delete an org. Fails if the org still has repos.

**Response:** `204 No Content`

**Errors:** `404 Not Found`; `409 Conflict` if org has repos.

---

#### `GET /orgs/{org}/repos`

List all repos owned by an org.

**Response `200 OK`:**
```json
{
  "repos": [
    {"name": "acme/myrepo", "owner": "acme", "created_at": "...", "created_by": "..."}
  ]
}
```

---

### Repo Management

#### `POST /repos`

Create a new repository. The repo is owned by an existing org.

**Request body:**
```json
{
  "owner": "acme",
  "name": "myrepo"
}
```

- `owner` — the org name (must already exist)
- `name` — the repo path within the org (may contain slashes for subgroup nesting, e.g. `team/subrepo`)
- The full repo identifier is `owner/name` (e.g. `acme/myrepo` or `acme/team/subrepo`)

**Response `201 Created`:**
```json
{
  "name": "acme/myrepo",
  "owner": "acme",
  "created_at": "2024-01-15T12:00:00Z",
  "created_by": "alice@example.com"
}
```

**Errors:** `409 Conflict` if repo already exists; `404 Not Found` if org does not exist.

---

#### `GET /repos`

List all repositories.

**Response `200 OK`:**
```json
{
  "repos": [
    {"name": "acme/myrepo", "owner": "acme", "created_at": "...", "created_by": "..."}
  ]
}
```

---

#### `GET /repos/{name}`

Get a single repository by its full name (e.g. `acme/myrepo`).

**Response `200 OK`:** same shape as an element of the `GET /repos` list.

**Errors:** `404 Not Found`.

---

#### `DELETE /repos/{name}`

Hard-delete a repository (all branches, commits, documents, and roles).

**Response:** `204 No Content`

**Errors:** `404 Not Found`.

---

### Branches

All branch endpoints use the `/-/` separator. For a repo named `acme/myrepo`, the URL is `/repos/acme/myrepo/-/branches`.

#### `GET /repos/{name}/-/branches`

List all branches in the repo.

**Query params:**
- `status` (optional) — filter by branch status: `active`, `merged`, or `abandoned`

**Response `200 OK`:**
```json
[
  {
    "name": "main",
    "head_sequence": 42,
    "base_sequence": 0,
    "status": "active"
  },
  {
    "name": "feature/my-change",
    "head_sequence": 45,
    "base_sequence": 42,
    "status": "active"
  }
]
```

---

#### `POST /repos/{name}/-/branch`

Create a new branch from the current `main` head. Branch names may contain slashes.

**Request body:**
```json
{"name": "feature/my-change"}
```

**Response `201 Created`:**
```json
{
  "name": "feature/my-change",
  "base_sequence": 42
}
```

**Errors:** `409 Conflict` if branch already exists; `400 Bad Request` if name is `"main"`.

**RBAC:** maintainer or admin.

---

#### `DELETE /repos/{name}/-/branch/{bname}`

Delete a branch. `main` cannot be deleted. Branch names may contain slashes.

**Response:** `204 No Content`

**Errors:** `404 Not Found`; `409 Conflict` if the branch is already merged or abandoned.

**RBAC:** maintainer or admin.

---

### Commits

#### `POST /repos/{name}/-/commit`

Commit one or more file changes to a branch.

**Request body:**
```json
{
  "branch": "feature/my-change",
  "message": "update access control docs",
  "files": [
    {"path": "docs/guide.md", "content": "<base64-encoded bytes>"},
    {"path": "images/logo.png", "content": "<base64-encoded bytes>", "content_type": "image/png"},
    {"path": "old-file.txt"}
  ]
}
```

- `content` is the raw file bytes encoded as base64 (standard JSON encoding of `[]byte`)
- `content_type` (optional) — MIME type for the file; omit for plain text files. The CLI sets this automatically for detected binary files.
- A file entry with no `content` field (or `null`) is a **delete**
- `author` in the request body is ignored; the server uses the IAP-authenticated identity

**Response `201 Created`:**
```json
{
  "sequence": 43,
  "files": [
    {"path": "docs/guide.md", "version_id": "<uuid>"},
    {"path": "old-file.txt", "version_id": null}
  ]
}
```

**Errors:** `404 Not Found` (branch); `409 Conflict` (branch not active).

**RBAC:** writer+ (writers cannot target `main`; maintainer/admin can commit to any branch).

---

#### `GET /repos/{name}/-/commit/{sequence}`

Get commit metadata and the list of files changed.

**Response `200 OK`:**
```json
{
  "sequence": 43,
  "message": "update access control docs",
  "author": "alice@example.com",
  "created_at": "2024-01-15T12:00:00Z",
  "files": [
    {"path": "docs/guide.md", "version_id": "<uuid>"},
    {"path": "old-file.txt", "version_id": null}
  ]
}
```

**Errors:** `404 Not Found`.

---

### Tree and File Content

#### `GET /repos/{name}/-/tree`

Materialize the full file tree for a branch at an optional sequence number. Supports cursor-based pagination.

**Query params:**
- `branch` — branch name (default: `main`)
- `at` — sequence number to materialize at (default: current head)
- `limit` — max entries per page (default: 100)
- `after` — cursor: last `path` from previous page

**Response `200 OK`:** array of tree entries:
```json
[
  {
    "path": "docs/guide.md",
    "version_id": "<uuid>",
    "content_hash": "<sha256-hex>"
  }
]
```

Returns an empty array `[]` for empty trees.

---

#### `GET /repos/{name}/-/file/{path...}`

Get the content of a file on a branch at an optional sequence.

**Query params:**
- `branch` — branch name (default: `main`)
- `at` — sequence number (default: current head)

**Response `200 OK`:**
```json
{
  "path": "docs/guide.md",
  "version_id": "<uuid>",
  "content_hash": "<sha256-hex>",
  "content": "<base64-encoded bytes>",
  "content_type": "image/png"
}
```

- `content_type` is omitted when the file has no stored MIME type (i.e. plain text files).

**Errors:** `404 Not Found`.

---

#### `GET /repos/{name}/-/file/{path...}/history`

Get the commit history for a file on a branch.

**Query params:**
- `branch` — branch name (default: `main`)
- `limit` — max entries (default: 100)
- `after` — cursor: last `sequence` from previous page (pagination)

**Response `200 OK`:** array of history entries:
```json
[
  {
    "sequence": 43,
    "version_id": "<uuid>",
    "message": "update access control docs",
    "author": "alice@example.com",
    "created_at": "2024-01-15T12:00:00Z"
  }
]
```

---

### Diff

#### `GET /repos/{name}/-/diff`

Compare a branch against its base sequence on `main`, showing what changed on each side and any conflicts.

**Query params:**
- `branch` — required; branch to compare

**Response `200 OK`:**
```json
{
  "branch_changes": [
    {"path": "docs/guide.md", "version_id": "<uuid>"},
    {"path": "images/logo.png", "version_id": "<uuid>", "binary": true},
    {"path": "old-file.txt", "version_id": null}
  ],
  "main_changes": [
    {"path": "docs/guide.md", "version_id": "<uuid>"}
  ],
  "conflicts": [
    {
      "path": "docs/guide.md",
      "main_version_id": "<uuid>",
      "branch_version_id": "<uuid>"
    }
  ]
}
```

- `version_id: null` means the file was deleted on that side
- `binary: true` is set for files that have a stored `content_type` (omitted for text files)
- `conflicts` is omitted when empty

**Errors:** `400 Bad Request` (missing branch); `404 Not Found` (branch).

---

### Merge

#### `POST /repos/{name}/-/merge`

Merge a branch into `main`. Cannot merge `main` into itself.

**Request body:**
```json
{"branch": "feature/my-change"}
```

- `author` in the body is ignored; the server uses the IAP-authenticated identity

**Response `200 OK` (success):**
```json
{"sequence": 46}
```

**Response `403 Forbidden` (policy denied):**
```json
{
  "policies": [
    {"name": "require_review", "pass": false, "reason": "at least one approval required"}
  ]
}
```

**Response `409 Conflict` (merge conflicts):**
```json
{
  "conflicts": [
    {
      "path": "docs/guide.md",
      "main_version_id": "<uuid>",
      "branch_version_id": "<uuid>"
    }
  ]
}
```

**Errors:** `404 Not Found` (branch); `403 Forbidden` (policy denied); `409 Conflict` (branch not active, or conflicts).

**RBAC:** maintainer or admin.

---

### Rebase

#### `POST /repos/{name}/-/rebase`

Replay branch commits on top of the current `main` head. Updates the branch's `base_sequence` and `head_sequence`. Cannot rebase `main`.

**Request body:**
```json
{"branch": "feature/my-change"}
```

- `author` in the body is ignored; the server uses the IAP-authenticated identity

**Response `200 OK` (success):**
```json
{
  "base_sequence": 50,
  "head_sequence": 53,
  "commits_replayed": 3
}
```

**Response `409 Conflict` (rebase conflicts):**
```json
{
  "conflicts": [
    {
      "path": "docs/guide.md",
      "main_version_id": "<uuid>",
      "branch_version_id": "<uuid>"
    }
  ]
}
```

**Errors:** `404 Not Found` (branch); `400 Bad Request` (branch not active or is `main`).

**RBAC:** maintainer or admin.

---

### Reviews

#### `POST /repos/{name}/-/review`

Submit a review (approval, rejection, or dismissal) for a branch.

- A reviewer cannot approve their own commits (`403 Forbidden`).
- The review is scoped to the branch's current head sequence at the time of the call.

**Request body:**
```json
{
  "branch": "feature/my-change",
  "status": "approved",
  "body": "LGTM"
}
```

Status values: `approved`, `rejected`, `dismissed`.

**Response `201 Created`:**
```json
{
  "id": "<uuid>",
  "sequence": 43
}
```

**Errors:** `404 Not Found` (branch); `403 Forbidden` (self-approval).

---

#### `GET /repos/{name}/-/branch/{branch}/reviews`

List reviews for a branch, optionally scoped to a specific head sequence.

**Query params:**
- `at` — sequence number to filter by (optional)

**Response `200 OK`:** array of review objects:
```json
[
  {
    "id": "<uuid>",
    "branch": "feature/my-change",
    "reviewer": "bob@example.com",
    "sequence": 43,
    "status": "approved",
    "body": "LGTM",
    "created_at": "2024-01-15T12:05:00Z"
  }
]
```

---

### Check Runs

#### `POST /repos/{name}/-/check`

Report an external CI check run result for a branch.

**Request body:**
```json
{
  "branch": "feature/my-change",
  "check_name": "unit-tests",
  "status": "passed"
}
```

Status values: `pending`, `passed`, `failed`.

**Response `201 Created`:**
```json
{
  "id": "<uuid>",
  "sequence": 43
}
```

**Errors:** `404 Not Found` (branch).

---

#### `GET /repos/{name}/-/branch/{branch}/checks`

List check runs for a branch, optionally scoped to a specific head sequence.

**Query params:**
- `at` — sequence number to filter by (optional)

**Response `200 OK`:** array of check run objects:
```json
[
  {
    "id": "<uuid>",
    "branch": "feature/my-change",
    "sequence": 43,
    "check_name": "unit-tests",
    "status": "passed",
    "reporter": "ci-bot@example.com",
    "created_at": "2024-01-15T12:10:00Z"
  }
]
```

---

### Role Management

All role endpoints require `admin` role.

#### `GET /repos/{name}/-/roles`

List all roles in the repo.

**Response `200 OK`:**
```json
{
  "roles": [
    {"identity": "alice@example.com", "role": "admin"},
    {"identity": "bob@example.com", "role": "writer"}
  ]
}
```

---

#### `PUT /repos/{name}/-/roles/{identity}`

Set or update the role for an identity. Identity may contain slashes (e.g. email addresses are routed correctly).

**Request body:**
```json
{"role": "writer"}
```

Valid roles: `reader`, `writer`, `maintainer`, `admin`.

**Response `200 OK`:**
```json
{"identity": "bob@example.com", "role": "writer"}
```

---

#### `DELETE /repos/{name}/-/roles/{identity}`

Remove an identity's role from the repo.

**Response:** `204 No Content`

**Errors:** `404 Not Found`.

---

### Branch Status

#### `GET /repos/{name}/-/branch/{bname}/status`

Evaluate all merge policies for a branch without actually merging. Useful for CI gates and UI indicators.

**Response `200 OK`:**
```json
{
  "mergeable": true,
  "policies": [
    {"name": "require_review", "pass": true, "reason": ""},
    {"name": "require_ci",     "pass": true, "reason": ""}
  ]
}
```

- `mergeable` — `true` if all policies pass (or no policies are defined)
- `policies` — one entry per loaded policy; empty array when in bootstrap mode
- `reason` — human-readable denial reason when `pass` is `false`; empty string otherwise

**RBAC:** reader or above.

---

## Database Schema

All data tables are scoped to a `repo` column (foreign key to `repos.name`). The schema is defined in `internal/db/migrations/000001_initial_schema.up.sql`.

### `orgs`

Top-level namespace. Every repo must belong to an org.

| Column       | Type          | Description                        |
|-------------|--------------|-------------------------------------|
| `name`       | `TEXT PK`     | Unique org name (e.g. `acme`)      |
| `created_at` | `TIMESTAMPTZ` | Creation timestamp                 |
| `created_by` | `TEXT`        | Identity that created the org      |

Seeded with a `default` org on migration.

---

### `repos`

Named tenants owned by an org. The full repo identifier is the `name` (e.g. `acme/myrepo`); `owner` is the first path segment and must match an existing org.

| Column       | Type          | Description                                                    |
|-------------|--------------|----------------------------------------------------------------|
| `name`       | `TEXT PK`     | Full path, e.g. `acme/myrepo` or `acme/team/subrepo`         |
| `owner`      | `TEXT`        | FK → `orgs.name`; must equal `split_part(name, '/', 1)`      |
| `created_at` | `TIMESTAMPTZ` | Creation timestamp                                             |
| `created_by` | `TEXT`        | Identity that created the repo                                 |

Seeded with a `default/default` repo (owned by org `default`) on migration.

---

### `branches`

Named pointers to a sequence, scoped to a repo.

| Column          | Type              | Description                                |
|----------------|-------------------|--------------------------------------------|
| `repo`          | `TEXT`            | FK → `repos.name`                          |
| `name`          | `TEXT`            | Branch name (may contain `/`)              |
| `head_sequence` | `BIGINT`          | Current tip sequence (0 = no commits yet)  |
| `base_sequence` | `BIGINT`          | Sequence where branch forked from `main`   |
| `status`        | `branch_status`   | `active`, `merged`, or `abandoned`         |
| `created_at`    | `TIMESTAMPTZ`     |                                            |
| `created_by`    | `TEXT`            |                                            |

Primary key: `(repo, name)`. Seeded with `(default, main)`.

---

### `commits`

Global sequence allocation; one row per atomic commit.

| Column       | Type         | Description                            |
|-------------|-------------|----------------------------------------|
| `sequence`   | `BIGSERIAL PK` | Globally monotonic per repo (actually global across all repos due to BIGSERIAL) |
| `branch`     | `TEXT`       | Branch this commit targets             |
| `message`    | `TEXT`       | Commit message                         |
| `author`     | `TEXT`       | IAP-authenticated identity             |
| `created_at` | `TIMESTAMPTZ`|                                        |
| `repo`       | `TEXT`       | FK → `repos.name`                     |

---

### `documents`

Immutable, content-addressed file versions. Identical content is stored once per repo.

| Column         | Type          | Description                                    |
|---------------|--------------|------------------------------------------------|
| `version_id`   | `UUID PK`    | Unique version identifier                      |
| `path`         | `TEXT`       | File path (as stored in the commit)            |
| `content`      | `BYTEA`      | Raw file bytes                                 |
| `content_hash` | `TEXT`       | SHA256 hex digest of `content`                 |
| `content_type` | `TEXT`       | MIME type (nullable; set for binary files)     |
| `created_at`   | `TIMESTAMPTZ`|                                                |
| `created_by`   | `TEXT`       | Identity                                       |
| `repo`         | `TEXT`       | FK → `repos.name`                             |

Index: `(repo, content_hash)` — used for deduplication lookup.

---

### `file_commits`

Core event log. One row per file change within a commit. Multiple rows share the same `sequence`.

| Column      | Type      | Description                                         |
|------------|----------|-----------------------------------------------------|
| `commit_id` | `UUID PK` | Unique row identifier                               |
| `sequence`  | `BIGINT`  | FK → `commits.sequence`                             |
| `path`      | `TEXT`    | File path                                           |
| `version_id`| `UUID`    | FK → `documents.version_id`; `NULL` means delete   |
| `branch`    | `TEXT`    | Branch name                                         |
| `repo`      | `TEXT`    | FK → `repos.name`                                  |

Indexes:
- `(repo, branch, path, sequence DESC)` — used for tree materialization and file history
- `(repo, sequence)` — used for commit lookup and diff

---

### `roles`

Identity-to-permission mapping per repo.

| Column     | Type        | Description                                |
|-----------|------------|---------------------------------------------|
| `repo`     | `TEXT`      | FK → `repos.name`                          |
| `identity` | `TEXT`      | IAP email address or service account       |
| `role`     | `role_type` | `reader`, `writer`, `maintainer`, `admin`  |

Primary key: `(repo, identity)`.

---

### `reviews`

Approval/rejection records for a branch at a specific head sequence.

| Column       | Type            | Description                                          |
|-------------|----------------|------------------------------------------------------|
| `id`         | `UUID PK`      |                                                      |
| `repo`       | `TEXT`         | FK → `repos.name`                                   |
| `branch`     | `TEXT`         | FK (composite) → `branches(repo, name)`             |
| `reviewer`   | `TEXT`         | IAP identity of the reviewer                        |
| `sequence`   | `BIGINT`       | Branch head at time of review                        |
| `status`     | `review_status`| `approved`, `rejected`, or `dismissed`               |
| `body`       | `TEXT`         | Optional review comment                              |
| `created_at` | `TIMESTAMPTZ`  |                                                      |

Index: `(repo, branch, sequence)`.

---

### `check_runs`

External CI check results for a branch at a specific head sequence.

| Column       | Type           | Description                                          |
|-------------|---------------|------------------------------------------------------|
| `id`         | `UUID PK`     |                                                      |
| `repo`       | `TEXT`        | FK → `repos.name`                                   |
| `branch`     | `TEXT`        | FK (composite) → `branches(repo, name)`             |
| `sequence`   | `BIGINT`      | Branch head at time of check run                     |
| `check_name` | `TEXT`        | Name of the check (e.g. `unit-tests`)                |
| `status`     | `check_status`| `pending`, `passed`, or `failed`                     |
| `reporter`   | `TEXT`        | IAP identity of the reporting service                |
| `created_at` | `TIMESTAMPTZ` |                                                      |

Index: `(repo, branch, sequence, check_name)`.

---

## SSE Horizontal Scaling Limitation

Server-Sent Events (SSE) fan-out (`GET /repos/{name}/-/events` and `GET /events`) is implemented in-process using an in-memory broker. This works correctly at `--max-instances=1` (e.g. a single Cloud Run instance).

**If you scale beyond one instance**, SSE clients connected to different instances will only receive events emitted by *their* instance. Events emitted by instance A are not forwarded to SSE clients connected to instance B.

To support SSE at multiple instances, replace the in-process `events.Broker` with a Pub/Sub-backed fan-out (Milestone 2 Pub/Sub backend provides the durability primitive; SSE fan-out would require an additional layer such as Redis Pub/Sub or a shared GCP Pub/Sub subscription).

**Webhook delivery is not affected** — webhooks are delivered via the `event_outbox` database table and the dispatcher goroutine correctly uses `FOR UPDATE SKIP LOCKED` to prevent double-delivery across instances.

---

## Monitoring

### Health check

`GET /healthz` returns `200 {"status": "ok"}` and requires no authentication. Wire this to your load balancer or Cloud Run health check configuration.

### Logging

The server uses the standard `log/slog` package for structured JSON logging. All startup events, migration status, request details, and errors are written to stderr as JSON. Cloud Run's log collection will parse this automatically.

---

## Runbook: First Deployment

1. Provision Cloud SQL PostgreSQL instance
2. Create a database and a service account user with `CREATE`, `SELECT`, `INSERT`, `UPDATE`, `DELETE` permissions
3. Construct `DATABASE_URL`: `postgres://user:pass@/dbname?host=/cloudsql/project:region:instance`
4. Build and push the container image
5. Deploy to Cloud Run with `DATABASE_URL` and `BOOTSTRAP_ADMIN` set
6. Enable IAP on the Cloud Run service
7. Verify: `curl https://<url>/healthz` → `{"status":"ok"}`
8. Create an org and your first repo (migration seeds `default` org and `default/default` repo; skip if those are sufficient):
   ```bash
   curl -X POST https://<url>/orgs \
     -H "Content-Type: application/json" \
     -d '{"name": "acme"}'

   curl -X POST https://<url>/repos \
     -H "Content-Type: application/json" \
     -d '{"owner": "acme", "name": "myrepo"}'
   ```
   > **Note:** When IAP is enabled, the proxy automatically injects the
   > `X-Goog-IAP-JWT-Assertion` header — clients never set it directly.
   > The examples above work as-is when the request passes through IAP.
   > For local testing with `--dev-identity`, no auth header is needed since
   > IAP validation is bypassed entirely.
9. Grant yourself admin:
   ```bash
   curl -X PUT https://<url>/repos/acme/myrepo/-/roles/you@example.com \
     -H "Content-Type: application/json" \
     -d '{"role": "admin"}'
   ```
10. Initialize a local workspace: `ds init https://<url>/repos/acme/myrepo`

---

## Initial Setup: CI Runner

The CI runner is a separate Cloud Run service (`ci-runner`) that executes checks triggered by docstore events. Before the first push to `main` will succeed, run the one-time setup script to provision the required infrastructure.

### When to run

Run `scripts/setup.sh` once per project, before pushing to `main` for the first time. It is safe to re-run (all steps are idempotent).

### What it creates

| Resource | Description |
|---------|-------------|
| `ci-runner` service account | Runs the Cloud Run service |
| `gs://docstore-ci-logs` GCS bucket | Stores CI check run logs |
| `ci-runner-webhook-secret` Secret Manager secret | HMAC secret for webhook payload verification |
| `ci-runner-url` Secret Manager secret | Public URL of the deployed ci-runner service |
| IAM bindings | See below |

**IAM bindings created:**
- `ci-runner` SA → `roles/storage.objectCreator` on `gs://docstore-ci-logs`
- `ci-runner` SA → `roles/secretmanager.secretAccessor` on both secrets (runtime access)
- `docstore-deployer` SA → `roles/run.admin` on the project (to deploy the service)
- `docstore-deployer` SA → `roles/iam.serviceAccountUser` on `ci-runner` SA (to assign it to the Cloud Run service)
- `docstore-deployer` SA → `roles/secretmanager.viewer` on both secrets (to validate `--update-secrets` references at deploy time)

**Required APIs enabled:** `run.googleapis.com`, `secretmanager.googleapis.com`, `storage.googleapis.com`, `artifactregistry.googleapis.com`

### How to run

```bash
bash scripts/setup.sh
```

Override project or region if needed:

```bash
PROJECT=my-project REGION=us-east1 bash scripts/setup.sh
```

### Updating placeholder secrets

The script creates both secrets with a `PLACEHOLDER` value. Before the first deploy you must update them:

**1. Set the real HMAC webhook secret** (choose any random string; must match what the docstore outbox dispatcher sends):

```bash
echo -n 'your-hmac-secret' | gcloud secrets versions add ci-runner-webhook-secret \
  --data-file=- --project=dlorenc-chainguard
```

**2. Push to `main`** — `deploy.yml` builds and deploys the ci-runner image.

**3. After the first deploy, get the service URL and update `ci-runner-url`:**

```bash
URL=$(gcloud run services describe ci-runner \
  --region=us-central1 --project=dlorenc-chainguard --format='value(status.url)')
echo -n "${URL}" | gcloud secrets versions add ci-runner-url \
  --data-file=- --project=dlorenc-chainguard
```

The `ci-runner-url` secret is read at runtime by the ci-runner service and used by the docstore event outbox dispatcher to know where to send webhook events.

### Full E2E flow after setup

1. Run `scripts/setup.sh` (once)
2. Update `ci-runner-webhook-secret` with the real HMAC secret
3. Push to `main` → `deploy.yml` runs two parallel jobs:
   - `deploy` — builds and deploys the docstore server via ko
   - `build-and-deploy-ci-runner` — builds the ci-runner Docker image and deploys it to Cloud Run gen2
4. Retrieve the ci-runner URL and update `ci-runner-url` (step 3 above)
5. Commits to any branch in docstore trigger the outbox dispatcher, which POSTs to `ci-runner/webhook` (future endpoint), which runs `.docstore/ci.yaml` checks and posts results back via `POST /repos/{name}/-/check`

### Notes

- `--allow-unauthenticated` is intentional on the ci-runner service. It receives webhook POSTs from the docstore outbox dispatcher; request authenticity is verified via HMAC signature (WEBHOOK_SECRET), not IAP.
- The docstore service is also `--allow-unauthenticated` in this project, so the ci-runner SA does not need `roles/run.invoker` on the docstore service.
- The ci-runner uses `--execution-environment=gen2` (required for running buildkitd inside the container) with `--concurrency=1` to avoid concurrent buildkitd access per instance.

---

## CLI Reference

The `ds` CLI wraps the DocStore API. All workspace commands require a `.docstore/` directory (created by `ds init`). Org, repo, and role management commands work without a workspace when a default remote URL is compiled in.

### Building the CLI

```bash
make build-ds   # produces bin/ds with the default Cloud Run URL baked in
```

To override the compiled-in URL:

```bash
DEFAULT_REMOTE=https://your-server.example.com make build-ds
```

### Workspace Commands

| Command | Description |
|---|---|
| `ds init [<url>]` | Initialize a workspace. URL optional if compiled-in default exists. |
| `ds status` | Show local changes vs last synced state. |
| `ds commit -m "msg"` | Commit all local changes; binary files detected automatically. |
| `ds checkout <branch>` | Switch to an existing branch (requires clean working tree). |
| `ds checkout -b <branch>` | Create and switch to a new branch. |
| `ds pull` | Sync local files from the current branch on the server. |
| `ds merge` | Merge current branch into main. |
| `ds rebase` | Rebase current branch onto latest main. |
| `ds resolve <path>` | Mark a rebase conflict as resolved. |
| `ds diff` | Show branch diff; binary files shown as `[binary]`. |
| `ds log [path] [--limit N]` | Show commit history. |
| `ds show <seq> [path]` | Inspect a commit or file at a sequence. |

### Review and CI Workflow

| Command | Description |
|---|---|
| `ds branches [--status active\|merged\|abandoned]` | List branches with head/base sequences. |
| `ds reviews [--branch <name>]` | List reviews; stale reviews (before latest commit) shown with `[stale]`. |
| `ds review --status approved\|rejected [--body "..."] [--branch <name>]` | Submit a review. |
| `ds checks [--branch <name>]` | List CI check runs; stale checks shown with `[stale]`. |
| `ds check --name <name> --status passed\|failed [--branch <name>]` | Report a CI check result. |

### Terminal UI

```bash
ds tui
```

A Bubble Tea terminal UI with:
- **Branch list view** — active branches with review and CI summary; `j`/`k` to navigate
- **Branch detail view** — Diff / Reviews / Checks panels; cycle with `Tab`
  - Diff panel: `+`/`~`/`-` file list; expand inline with `Enter`; binary files shown as `[binary]`
  - Reviews panel: one-per-reviewer with stale indicators; approval summary
  - Checks panel: check runs with stale indicators
- **Review overlay** — Approve / Reject toggle + optional body; `Esc` to cancel
- **Inline merge** — `y`/`N` prompt; surfaces conflicts on failure
- `R` to refresh; `q` to go back / quit

### Importing a Git Repository

```bash
ds import-git <path-to-local-git-repo> [--mode squash|replay]
```

Imports the default branch of a local git repository into the docstore `main` branch.

- `replay` (default) — one docstore commit per git commit (merge commits skipped); original author embedded in the message as `[git-author: email]`
- `squash` — single commit with all files at HEAD; message prefixed with `[git-import]`

Binary files are detected automatically. Deleted files are handled correctly.

### Org / Repo / Role Management

These commands work without a local workspace. They read the compiled-in default remote or accept `--remote <url>`.

```bash
# Orgs
ds orgs                           # list all orgs
ds orgs create <name>             # create an org
ds orgs delete <name>             # delete an org (fails if org has repos)
ds orgs repos <name>              # list repos in an org

# Repos
ds repos                          # list all repos
ds repos create <owner> <name>    # create a repo (owner = org name)
ds repos delete <owner>/<name>    # delete a repo

# Roles (require a workspace or --remote)
ds roles                          # list roles in current repo
ds roles set <identity> <role>    # set role (reader|writer|maintainer|admin)
ds roles delete <identity>        # remove a role
```
