# DocStore VCS — MVP Implementation Plan

## Goal

Exercise the core client-server loop: commit files, read them back, branch, and merge. Server in Go on Cloud Run, managed Postgres on Cloud SQL. Six phases, each building on the last.

## Testing Strategy

All server-side database tests use [testcontainers-go](https://github.com/testcontainers/testcontainers-go) to spin up a real Postgres container per test suite. No mocks for SQL — every store-layer test runs against real Postgres. CLI tests use `net/http/httptest` to stand up an in-process server backed by a testcontainer Postgres instance, exercising the full HTTP round-trip.

**Test helper** (`internal/testutil/testdb.go`):

```go
func NewTestDB(t *testing.T) *pgxpool.Pool {
    t.Helper()
    ctx := context.Background()
    pg, err := postgres.Run(ctx, "postgres:15-alpine",
        postgres.WithDatabase("docstore_test"),
        testcontainers.WithWaitStrategy(
            wait.ForLog("database system is ready to accept connections").
                WithOccurrence(2).WithStartupTimeout(30*time.Second)),
    )
    require.NoError(t, err)
    t.Cleanup(func() { pg.Terminate(ctx) })

    connStr, err := pg.ConnectionString(ctx, "sslmode=disable")
    require.NoError(t, err)
    pool, err := pgxpool.New(ctx, connStr)
    require.NoError(t, err)
    t.Cleanup(func() { pool.Close() })

    // Run migrations
    RunMigrations(t, pool)
    return pool
}
```

Each test suite calls `NewTestDB(t)` once in `TestMain` or at the top of each test function. The container is destroyed on cleanup — tests are fully isolated.

**GitHub Actions** (`.github/workflows/ci.yml`):

```yaml
name: CI
on: [push, pull_request]
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.22'
      - name: Run tests
        run: go test -v -race -count=1 ./...
      - name: Run tests with coverage
        run: go test -coverprofile=coverage.out ./...
      - name: Upload coverage
        uses: actions/upload-artifact@v4
        with:
          name: coverage
          path: coverage.out
  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.22'
      - uses: golangci/golangci-lint-action@v6
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.22'
      - run: go build ./cmd/server
      - run: go build ./cmd/ds
```

Docker is pre-installed on `ubuntu-latest` runners — testcontainers works out of the box.

---

## Explicitly Deferred

These exist in DESIGN.md but are **not** part of the MVP:

- **Auth**: No IAP. Identity passed via `X-DocStore-Identity` header (trusted, no validation).
- **Authorization**: No `roles` table, no OPA policy engine, no OWNERS files. All requests are allowed.
- **Reviews & checks**: No `reviews` table, no `check_runs` table, no `/review` or `/check` endpoints.
- **Rebase**: No `POST /rebase`.
- **Purge**: No `POST /purge`.
- **Pagination**: All list endpoints return everything. No `?limit=N&after=cursor`.
- **File history**: No `GET /file/:path/history`.
- **CLI commands**: No `ds log`, `ds show`, `ds resolve`.

---

## Phase 1 — Schema + Commit + Read

**Depends on**: nothing

### What gets built

**Schema (Postgres migrations)**

Three tables from DESIGN.md, subset of columns:

```sql
CREATE TABLE documents (
    version_id  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    path        TEXT NOT NULL,
    content     BYTEA NOT NULL,
    content_hash TEXT NOT NULL,  -- hex-encoded SHA-256
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by  TEXT NOT NULL
);
CREATE INDEX idx_documents_content_hash ON documents (content_hash);

CREATE TABLE commits (
    sequence   BIGSERIAL PRIMARY KEY,
    message    TEXT NOT NULL,
    author     TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE file_commits (
    commit_id  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    sequence   BIGINT NOT NULL REFERENCES commits(sequence),
    path       TEXT NOT NULL,
    version_id UUID REFERENCES documents(version_id),  -- NULL = delete
    branch     TEXT NOT NULL DEFAULT 'main'
);
CREATE INDEX idx_file_commits_branch_path_seq
    ON file_commits (branch, path, sequence DESC);

CREATE TABLE branches (
    name           TEXT PRIMARY KEY,
    head_sequence  BIGINT NOT NULL DEFAULT 0,
    base_sequence  BIGINT NOT NULL DEFAULT 0,
    status         TEXT NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'merged', 'abandoned')),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by     TEXT NOT NULL
);

-- Seed main branch
INSERT INTO branches (name, head_sequence, base_sequence, created_by)
VALUES ('main', 0, 0, 'system');
```

**Go server**

- `main.go` — HTTP server, routes, Postgres connection pool.
- Database driver: `pgx/v5`.
- Migrations: run on startup via embedded SQL files (`embed` package).
- Identity: read `X-DocStore-Identity` header on every request, default to `anonymous` if missing.

**Endpoints**

`POST /commit` — Commit files to `main`.

- Request: `{"branch": "main", "files": [{"path": "...", "content": "base64..."}], "message": "..."}`
- Content is base64-encoded in the JSON body.
- For each file: SHA-256 hash the content, check `documents` for an existing row with that `content_hash`. If found, reuse its `version_id`. Otherwise insert a new `documents` row.
- Insert one `commits` row (get back `sequence`), then one `file_commits` row per file.
- All within a single transaction. Lock the branch row with `SELECT ... FOR UPDATE` before inserting.
- Advance `branches.head_sequence` to the new sequence.
- Response: `{"sequence": N}`

`GET /tree?branch=main` — Materialize the file tree.

- Query: `SELECT DISTINCT ON (path) path, version_id, content_hash FROM file_commits JOIN documents USING (version_id) WHERE branch = $1 AND sequence <= $2 ORDER BY path, sequence DESC`
- Filter out rows where `version_id IS NULL` (deleted files).
- Uses the branch's `head_sequence` as the upper bound (or `?at=N` if provided).
- Response: `[{"path": "...", "version_id": "...", "content_hash": "..."}]`

`GET /file/:path?branch=main` — Get file content.

- Materialize to find the `version_id` for the path, then fetch content from `documents`.
- Response: raw file content, `Content-Type: application/octet-stream`.
- 404 if the path doesn't exist or was deleted.

**Stubbed/skipped**

- Only `main` branch accepted in Phase 1. Return 400 if `branch != "main"`.
- No pagination on `/tree`.
- No `?at=` parameter (always uses head).

### Tests

**Unit tests** (`internal/server/store_test.go`) — store layer against testcontainer Postgres:

| Test | What it verifies |
|---|---|
| `TestCommit_SingleFile` | Insert one file, get back sequence=1, verify documents + file_commits rows |
| `TestCommit_MultiFile_Atomic` | Commit 2 files, both share the same sequence number |
| `TestCommit_ContentDedup` | Commit same content twice → only 1 documents row, 2 file_commits rows referencing the same version_id |
| `TestCommit_AdvancesHead` | After commit, `branches.head_sequence` equals the new sequence |
| `TestCommit_EmptyFiles` | Reject commit with zero files → 400 |
| `TestCommit_EmptyMessage` | Reject commit with blank message → 400 |
| `TestMaterializeTree_Empty` | Fresh DB returns empty tree |
| `TestMaterializeTree_AfterCommit` | Commit 2 files, tree returns both |
| `TestMaterializeTree_LatestVersion` | Commit file, update it, tree returns only the latest version |
| `TestMaterializeTree_DeletedFile` | Commit file, delete it (version_id=NULL), tree omits it |
| `TestGetFileContent` | Commit a file, fetch by path, content matches |
| `TestGetFileContent_NotFound` | Fetch non-existent path → 404 |
| `TestGetFileContent_Deleted` | Commit then delete a file, fetch → 404 |
| `TestMigrations_Idempotent` | Run migrations twice — no error |

**Integration tests** (`internal/server/handler_test.go`) — full HTTP round-trip using `httptest.Server` + testcontainer Postgres:

| Test | What it verifies |
|---|---|
| `TestHTTP_CommitAndReadTree` | POST /commit → GET /tree, verify paths match |
| `TestHTTP_CommitAndReadFile` | POST /commit → GET /file/:path, verify raw content |
| `TestHTTP_IdentityHeader` | Commit with X-DocStore-Identity → author in commits table matches |
| `TestHTTP_IdentityDefault` | Commit without header → author is "anonymous" |
| `TestHTTP_CommitBadJSON` | Malformed body → 400 |
| `TestHTTP_CommitNonMainBranch` | Branch != "main" in Phase 1 → 400 |
| `TestHTTP_SequentialCommits` | Two commits → sequences are 1, 2 |

### Success criteria

```bash
# Start server (assumes Postgres running locally)
export DATABASE_URL="postgres://localhost:5432/docstore?sslmode=disable"
go run ./cmd/server

# Commit two files atomically
curl -s -X POST http://localhost:8080/commit \
  -H "Content-Type: application/json" \
  -H "X-DocStore-Identity: alice" \
  -d '{"branch":"main","message":"initial commit","files":[
    {"path":"README.md","content":"'$(echo -n "# Hello" | base64)'"},
    {"path":"src/main.go","content":"'$(echo -n "package main" | base64)'"}
  ]}' | jq .
# → {"sequence": 1}

# Read the tree
curl -s http://localhost:8080/tree?branch=main | jq .
# → [{path: "README.md", ...}, {path: "src/main.go", ...}]

# Read a file
curl -s http://localhost:8080/file/README.md?branch=main
# → # Hello

# Commit again with one file changed — verify dedup
curl -s -X POST http://localhost:8080/commit \
  -H "Content-Type: application/json" \
  -H "X-DocStore-Identity: alice" \
  -d '{"branch":"main","message":"update readme","files":[
    {"path":"README.md","content":"'$(echo -n "# Hello World" | base64)'"}
  ]}' | jq .
# → {"sequence": 2}

# Tree still shows two files, README has new content
curl -s http://localhost:8080/tree?branch=main | jq length
# → 2
curl -s http://localhost:8080/file/README.md?branch=main
# → # Hello World

# Dedup: documents table should have 3 rows (two README versions + one main.go)
# Verify with: SELECT count(*) FROM documents; → 3
```

---

## Phase 2 — Branching

**Depends on**: Phase 1

### What gets built

**Endpoints**

`POST /branch` — Create a branch.

- Request: `{"name": "feature/foo"}`
- Sets `base_sequence` to main's current `head_sequence`.
- `head_sequence` starts equal to `base_sequence` (the branch inherits main's state).
- 409 if branch already exists.
- Response: `{"name": "feature/foo", "base_sequence": N}`

`GET /branches` — List branches.

- Optional query param: `?status=active`
- Response: `[{"name": "...", "head_sequence": N, "base_sequence": N, "status": "..."}]`

`GET /diff?branch=feature/foo` — Branch diff relative to fork point.

- Find files changed on the branch since `base_sequence`.
- Find files changed on main since `base_sequence`.
- Report changed paths and flag conflicts (paths in both sets).
- Response: `{"branch_changes": [{"path": "...", "action": "modify|add|delete"}], "main_changes": [...], "conflicts": ["path", ...]}`

**Modifications to existing endpoints**

- `POST /commit` now accepts any active branch, not just `main`.
- `GET /tree?branch=feature/foo` materializes the branch view: main's tree at `base_sequence`, overlaid with the branch's changes up to `head_sequence`. Query: union of main file_commits up to `base_sequence` and branch file_commits from `base_sequence+1` to `head_sequence`, then `DISTINCT ON (path) ... ORDER BY path, sequence DESC`.
- `GET /file/:path?branch=feature/foo` uses the same materialization logic.

**Stubbed/skipped**

- `DELETE /branch/:name` — not yet.
- Branch name validation — only require non-empty, no spaces.

### Tests

**Unit tests** (`internal/server/store_test.go` — extended):

| Test | What it verifies |
|---|---|
| `TestCreateBranch` | Creates branch, base_sequence = main's head_sequence |
| `TestCreateBranch_Duplicate` | Create same branch twice → error |
| `TestCreateBranch_BaseSequenceMatchesMain` | Advance main with commits, create branch, base_sequence matches |
| `TestCommit_ToBranch` | Commit to a non-main branch, branch head advances, main head unchanged |
| `TestCommit_ToNonexistentBranch` | Commit to unknown branch → error |
| `TestCommit_ToMergedBranch` | Commit to a merged branch → error |
| `TestMaterializeTree_Branch` | Branch inherits main files at base_sequence, plus its own commits |
| `TestMaterializeTree_BranchDoesNotSeeMainAdvances` | Commit to main after branch creation → branch tree unchanged |
| `TestMaterializeTree_BranchOverridesMainFile` | Branch modifies a file from main → branch tree shows branch version |
| `TestListBranches` | Create branches, list returns all |
| `TestListBranches_FilterByStatus` | Filter by ?status=active excludes merged branches |
| `TestDiff_AddedFile` | Branch adds a file → branch_changes includes it as "add" |
| `TestDiff_ModifiedFile` | Branch modifies a main file → branch_changes includes it as "modify" |
| `TestDiff_DeletedFile` | Branch deletes a main file → branch_changes includes it as "delete" |
| `TestDiff_NoConflicts` | No overlapping changes → conflicts is empty |
| `TestDiff_WithConflicts` | Both branch and main touch same file → conflicts includes the path |

**Integration tests** (`internal/server/handler_test.go` — extended):

| Test | What it verifies |
|---|---|
| `TestHTTP_CreateBranchAndCommit` | POST /branch → POST /commit to branch → GET /tree shows branch files |
| `TestHTTP_BranchIsolation` | Commit to branch, GET /tree?branch=main doesn't include it |
| `TestHTTP_GetFileFromBranch` | GET /file/:path?branch=feature → returns branch version of file |
| `TestHTTP_DiffEndpoint` | POST /branch, commit to it, GET /diff returns changes |
| `TestHTTP_CreateBranch409` | POST /branch with existing name → 409 |

### Success criteria

```bash
# Create a branch
curl -s -X POST http://localhost:8080/branch \
  -H "Content-Type: application/json" \
  -H "X-DocStore-Identity: alice" \
  -d '{"name":"feature/add-tests"}' | jq .
# → {"name": "feature/add-tests", "base_sequence": 2}

# List branches
curl -s http://localhost:8080/branches | jq .
# → [{name: "main", ...}, {name: "feature/add-tests", ...}]

# Commit to the branch
curl -s -X POST http://localhost:8080/commit \
  -H "Content-Type: application/json" \
  -H "X-DocStore-Identity: alice" \
  -d '{"branch":"feature/add-tests","message":"add test","files":[
    {"path":"src/main_test.go","content":"'$(echo -n "package main" | base64)'"}
  ]}' | jq .
# → {"sequence": 3}

# Tree on the branch shows main's files PLUS the new file
curl -s "http://localhost:8080/tree?branch=feature/add-tests" | jq length
# → 3

# Tree on main still shows 2 files
curl -s http://localhost:8080/tree?branch=main | jq length
# → 2

# Diff shows the branch change
curl -s "http://localhost:8080/diff?branch=feature/add-tests" | jq .
# → {"branch_changes": [{"path": "src/main_test.go", "action": "add"}], "main_changes": [], "conflicts": []}
```

---

## Phase 3 — Merge

**Depends on**: Phase 2

### What gets built

**Endpoints**

`POST /merge` — Merge a branch into main.

- Request: `{"branch": "feature/foo"}`
- Runs inside a single transaction. Locks main first, then the source branch (consistent ordering to prevent deadlocks).
- Steps (from DESIGN.md):
  1. **Branch changes**: `DISTINCT ON (path)` from `file_commits WHERE branch = :branch AND sequence > :base_sequence ORDER BY path, sequence DESC`
  2. **Main changes**: same query shape for `branch = 'main'` and `sequence > :base_sequence`
  3. **Conflict check**: intersection of changed paths. If non-empty → 409 with `{"conflicts": [...]}`
  4. **If clean**: insert one new `commits` row, copy branch-changed files as new `file_commits` rows on `main` (all sharing the new sequence), advance main's `head_sequence`, set branch status to `merged`.
- Response (success): `{"sequence": N, "merged": true}`
- Response (conflict): `409 {"conflicts": ["path1", "path2"]}`
- 400 if branch is already merged or abandoned.
- 404 if branch doesn't exist.

**Stubbed/skipped**

- No policy evaluation (reviews, checks, OWNERS). Merge always proceeds if no conflicts.
- No `POST /rebase` — if main has advanced, the user must manually resolve.

### Tests

**Unit tests** (`internal/server/store_test.go` — extended):

| Test | What it verifies |
|---|---|
| `TestMerge_CleanMerge` | Branch adds file, merge succeeds, file appears on main |
| `TestMerge_MultipleFiles` | Branch adds 3 files, all appear on main after merge with a single new sequence |
| `TestMerge_BranchModifiesMainFile` | Branch updates a main file, merge succeeds, main has the branch version |
| `TestMerge_BranchDeletesFile` | Branch deletes a main file (version_id=NULL), merge propagates the deletion |
| `TestMerge_Conflict` | Both main and branch modify same file after fork → 409 with conflict list |
| `TestMerge_ConflictMultiplePaths` | Multiple conflicting paths → all listed in response |
| `TestMerge_NoConflict_DifferentPaths` | Main modifies file A, branch modifies file B → merge succeeds |
| `TestMerge_BranchStatusSetMerged` | After merge, branch status = "merged" |
| `TestMerge_MainHeadAdvances` | After merge, main head_sequence equals the new merge commit sequence |
| `TestMerge_AlreadyMerged` | Merge a branch that's already merged → error |
| `TestMerge_NonexistentBranch` | Merge unknown branch → error |
| `TestMerge_LockOrdering` | Concurrent merges of different branches both succeed (no deadlock) |

**Integration tests** (`internal/server/handler_test.go` — extended):

| Test | What it verifies |
|---|---|
| `TestHTTP_MergeSuccess` | Full flow: create branch → commit → POST /merge → GET /tree on main shows merged files |
| `TestHTTP_MergeConflict409` | Two branches touch same file, first merge succeeds, second returns 409 |
| `TestHTTP_MergeAlreadyMerged400` | Merge same branch twice → 400 |
| `TestHTTP_MergeNonexistent404` | POST /merge with unknown branch → 404 |
| `TestHTTP_MergeMainBranch400` | POST /merge with branch=main → 400 |

**Concurrency test** (`internal/server/store_test.go`):

| Test | What it verifies |
|---|---|
| `TestConcurrentCommitsSameBranch` | 10 goroutines commit to the same branch simultaneously → all succeed with unique sequential sequence numbers, no data corruption |
| `TestConcurrentMergesDifferentBranches` | Create 5 branches with non-overlapping files, merge all concurrently → all succeed |

### Success criteria

```bash
# (continuing from Phase 2 state — feature/add-tests has one commit)

# Merge the branch
curl -s -X POST http://localhost:8080/merge \
  -H "Content-Type: application/json" \
  -H "X-DocStore-Identity: alice" \
  -d '{"branch":"feature/add-tests"}' | jq .
# → {"sequence": 4, "merged": true}

# Main now has 3 files
curl -s http://localhost:8080/tree?branch=main | jq length
# → 3

# Branch is marked merged
curl -s http://localhost:8080/branches?status=merged | jq '.[0].name'
# → "feature/add-tests"

# Conflict scenario: two branches touch the same file
curl -s -X POST http://localhost:8080/branch \
  -H "Content-Type: application/json" \
  -H "X-DocStore-Identity: alice" \
  -d '{"name":"feature/a"}'

curl -s -X POST http://localhost:8080/branch \
  -H "Content-Type: application/json" \
  -H "X-DocStore-Identity: bob" \
  -d '{"name":"feature/b"}'

# Both modify README.md
curl -s -X POST http://localhost:8080/commit \
  -H "Content-Type: application/json" \
  -H "X-DocStore-Identity: alice" \
  -d '{"branch":"feature/a","message":"edit readme","files":[
    {"path":"README.md","content":"'$(echo -n "version A" | base64)'"}
  ]}'

curl -s -X POST http://localhost:8080/commit \
  -H "Content-Type: application/json" \
  -H "X-DocStore-Identity: bob" \
  -d '{"branch":"feature/b","message":"edit readme","files":[
    {"path":"README.md","content":"'$(echo -n "version B" | base64)'"}
  ]}'

# Merge feature/a succeeds
curl -s -X POST http://localhost:8080/merge \
  -H "Content-Type: application/json" \
  -H "X-DocStore-Identity: alice" \
  -d '{"branch":"feature/a"}' | jq .merged
# → true

# Merge feature/b fails with conflict
curl -s -w "\n%{http_code}" -X POST http://localhost:8080/merge \
  -H "Content-Type: application/json" \
  -H "X-DocStore-Identity: bob" \
  -d '{"branch":"feature/b"}'
# → {"conflicts":["README.md"]}
# → 409
```

---

## Phase 4 — CLI: init, status, commit

**Depends on**: Phase 3

### What gets built

**CLI binary**: `cmd/ds/main.go` — a Go CLI using `cobra` or bare `os.Args` (keep it simple).

**Commands**

`ds init <remote-url>` — Initialize a local workspace.

- Creates `.docstore/` directory in the current folder.
- Writes `.docstore/config.json`: `{"remote": "http://localhost:8080", "branch": "main"}`
- Fetches `GET /tree?branch=main` from the remote.
- Downloads each file via `GET /file/:path?branch=main` and writes to disk.
- Writes `.docstore/state.json`: `{"branch": "main", "head_sequence": N, "files": {"path": "content_hash", ...}}`
- Identity: read from `DS_IDENTITY` env var or default to OS username.

`ds status` — Show local changes.

- Reads `.docstore/state.json` for the last-synced file hashes.
- Walks the local directory (excluding `.docstore/`), hashes each file.
- Compares to `state.json` to detect: new files, modified files, deleted files.
- Prints a summary (like `git status`).

`ds commit -m "message"` — Commit local changes.

- Runs `ds status` logic to find changed files.
- Sends `POST /commit` with the changed files (base64-encoded content).
- On success, updates `state.json` with the new head_sequence and file hashes.
- Handles file deletions: send a file entry with `null` content (or a `"delete": true` flag — match whatever the server expects for `version_id = NULL`).

**Stubbed/skipped**

- No `.gitignore`-style exclusions. Only `.docstore/` is excluded.
- No interactive staging — all changes are committed at once.
- File deletions in commit: deferred if complex (can send deletes as `{"path": "...", "content": null}`).

### Tests

CLI tests start a real `httptest.Server` wired to the real server handlers backed by a testcontainer Postgres. The CLI functions are called programmatically (not via `os/exec`) against a temp directory.

**Unit tests** (`internal/cli/status_test.go`, `internal/cli/commit_test.go`):

| Test | What it verifies |
|---|---|
| `TestDetectChanges_Clean` | State matches disk → no changes detected |
| `TestDetectChanges_Modified` | File content changed on disk → reported as modified |
| `TestDetectChanges_NewFile` | File on disk not in state → reported as new |
| `TestDetectChanges_Deleted` | File in state not on disk → reported as deleted |
| `TestDetectChanges_ExcludesDocstore` | `.docstore/` directory is always excluded |
| `TestDetectChanges_NestedDirs` | Files in subdirectories are detected correctly |
| `TestHashFile` | SHA-256 matches known digest |

**Integration tests** (`internal/cli/integration_test.go`) — CLI against a real server + testcontainer Postgres:

| Test | What it verifies |
|---|---|
| `TestInit_EmptyRepo` | `init` against empty server → creates `.docstore/`, empty working tree, state.json with sequence 0 |
| `TestInit_WithExistingFiles` | Commit files via API, then `init` → files appear on disk, state.json lists them |
| `TestInit_WritesConfig` | config.json contains correct remote URL and branch=main |
| `TestStatus_Clean` | After init, status reports no changes |
| `TestStatus_AfterModify` | Modify a file → status reports it modified |
| `TestStatus_AfterAdd` | Add a new file → status reports it new |
| `TestStatus_AfterDelete` | Delete a file → status reports it deleted |
| `TestCommit_SendsChanges` | Modify file, commit → server's tree reflects the change |
| `TestCommit_UpdatesState` | After commit, state.json has new sequence and updated hashes |
| `TestCommit_NoChanges` | Commit with no local changes → error, no server round-trip |
| `TestCommit_NewAndModified` | Add one file, modify another, commit → server gets both changes in one sequence |

### Success criteria

```bash
# Build the CLI
go build -o ds ./cmd/ds

# Init a workspace
mkdir /tmp/test-workspace && cd /tmp/test-workspace
ds init http://localhost:8080
# → Initialized docstore workspace (3 files, sequence 7)
cat .docstore/config.json
# → {"remote": "http://localhost:8080", "branch": "main"}
ls
# → README.md  src/

# Status shows clean
ds status
# → On branch main (sequence 7)
# → nothing to commit, working tree clean

# Make a change
echo "new content" > README.md
ds status
# → On branch main (sequence 7)
# → Modified: README.md

# Commit
ds commit -m "update readme from CLI"
# → Committed sequence 8 (1 file changed)

# Status clean again
ds status
# → On branch main (sequence 8)
# → nothing to commit, working tree clean

# New file
echo "test" > newfile.txt
ds commit -m "add newfile"
# → Committed sequence 9 (1 file changed)
```

---

## Phase 5 — CLI: branch workflow

**Depends on**: Phase 4

### What gets built

**Commands**

`ds checkout -b <name>` — Create a branch and switch to it.

- Sends `POST /branch {"name": "..."}`.
- Updates `.docstore/config.json` to set current branch.
- Updates `.docstore/state.json` with the new branch context (same files, branch's base_sequence as head).

`ds checkout <name>` — Switch to an existing branch (or `main`).

- Checks for uncommitted local changes. If any, error out.
- Fetches `GET /tree?branch=<name>` from the server.
- Diffs the remote tree against local files.
- Writes/updates/deletes local files to match.
- Updates `config.json` (branch) and `state.json` (sequence, file hashes).

`ds pull` — Pull latest changes for the current branch.

- Flow from DESIGN.md:
  1. Check for uncommitted changes → error if any.
  2. `GET /tree?branch=<current>` for the remote tree.
  3. Diff remote tree against local `state.json`.
  4. Download changed/new files, delete removed files.
  5. Update `state.json`.

`ds merge` — Merge the current branch into main.

- Must be on a non-main branch.
- Sends `POST /merge {"branch": "<current>"}`.
- On success: switches to main (`ds checkout main` logic).
- On conflict: prints conflict list, exits with error.

`ds diff` — Show what changed on the current branch.

- Sends `GET /diff?branch=<current>`.
- Prints changed paths grouped by action (added, modified, deleted).
- Flags conflicts if any.

**Stubbed/skipped**

- No `ds resolve` — conflicts must be resolved by committing to the branch after main changes are addressed manually.
- No `ds rebase`.
- No `ds log` or `ds show`.

### Tests

**Integration tests** (`internal/cli/integration_test.go` — extended):

| Test | What it verifies |
|---|---|
| `TestCheckoutNewBranch` | `checkout -b feature/x` → config.json branch updated, server has new branch |
| `TestCheckoutNewBranch_AlreadyExists` | `checkout -b` with existing name → error |
| `TestCheckoutExistingBranch` | Switch to an existing branch → local files match that branch's tree |
| `TestCheckoutExistingBranch_UncommittedChanges` | Dirty working tree → checkout refuses with error |
| `TestCheckoutMain_AfterBranch` | Create branch, add file, switch to main → branch file gone from disk |
| `TestCheckoutBranch_RestoresFiles` | Switch to main then back to branch → branch file reappears |
| `TestPull_NewRemoteFiles` | Another client commits via API, `pull` downloads the new file |
| `TestPull_UpdatedRemoteFile` | Remote file updated, `pull` overwrites local with new content |
| `TestPull_DeletedRemoteFile` | Remote file deleted, `pull` removes it locally |
| `TestPull_UncommittedChanges` | Dirty working tree → pull refuses with error |
| `TestPull_AlreadyUpToDate` | Pull with no remote changes → no-op, reports up-to-date |
| `TestPull_UpdatesStateSequence` | After pull, state.json sequence matches server head |
| `TestMergeCLI_Success` | On branch, `merge` → branch merged, switched to main, files present |
| `TestMergeCLI_OnMain` | `merge` while on main → error |
| `TestMergeCLI_Conflict` | Conflicting changes → merge fails, stays on branch, prints conflicts |
| `TestDiffCLI` | On branch with changes, `diff` prints added/modified/deleted paths |
| `TestDiffCLI_OnMain` | `diff` while on main → error |

**End-to-end workflow test** (`internal/cli/e2e_test.go`):

| Test | What it verifies |
|---|---|
| `TestFullWorkflow` | init → modify file → commit → checkout -b → add file → commit → diff → checkout main → merge → pull from second workspace → verify files. Exercises the entire CLI in sequence against a real server. |

### Success criteria

```bash
# Create a branch
ds checkout -b feature/cli-test
# → Created branch feature/cli-test (base sequence 9)
# → Switched to branch feature/cli-test

# Make a change and commit
echo "cli test" > cli-test.txt
ds commit -m "add cli test file"
# → Committed sequence 10 (1 file changed)

# Check diff
ds diff
# → Branch: feature/cli-test (base: 9, head: 10)
# → Added: cli-test.txt

# Switch back to main
ds checkout main
# → Switched to branch main (sequence 9)
ls cli-test.txt 2>/dev/null
# → (no output — file doesn't exist on main)

# Switch back to branch
ds checkout feature/cli-test
# → Switched to branch feature/cli-test (sequence 10)
cat cli-test.txt
# → cli test

# Merge
ds merge
# → Merged feature/cli-test into main (sequence 11)
# → Switched to branch main
ls cli-test.txt
# → cli-test.txt

# Pull scenario: another client commits, we pull
curl -s -X POST http://localhost:8080/commit \
  -H "Content-Type: application/json" \
  -H "X-DocStore-Identity: bob" \
  -d '{"branch":"main","message":"remote change","files":[
    {"path":"from-bob.txt","content":"'$(echo -n "hello from bob" | base64)'"}
  ]}'
ds pull
# → Pulled 1 new file (sequence 12)
cat from-bob.txt
# → hello from bob
```

---

## Phase 6 — Containerize + Deploy

**Depends on**: Phase 5

### What gets built

**Dockerfile** (multi-stage)

```dockerfile
FROM golang:1.22 AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o server ./cmd/server
RUN CGO_ENABLED=0 go build -o ds ./cmd/ds

FROM gcr.io/distroless/static-debian12
COPY --from=builder /app/server /server
COPY --from=builder /app/ds /ds
ENTRYPOINT ["/server"]
```

**Cloud Run deployment**

- Service: `docstore-server`
- Port: 8080 (Cloud Run default)
- Environment variable: `DATABASE_URL` pointing to Cloud SQL instance via Unix socket.
- Cloud SQL connection: use Cloud SQL Auth Proxy sidecar (built into Cloud Run's Cloud SQL integration).
- Min instances: 0, Max instances: 1 (MVP, single instance is fine).

**Cloud SQL setup**

- Instance: `docstore-mvp`
- Postgres 15.
- Database: `docstore`
- User: `docstore-server` with IAM auth or password (MVP can use password).

**CLI configuration**

- `ds init https://docstore-HASH-uc.a.run.app` — point at the deployed URL.
- `X-DocStore-Identity` header: CLI reads `DS_IDENTITY` env var.

**Stubbed/skipped**

- No IAP in front of Cloud Run (use `--allow-unauthenticated` for MVP, or basic Cloud Run IAM).
- No custom domain.
- No CI/CD pipeline — manual `gcloud run deploy`.

### Tests

**Container build test** (in CI):

| Test | What it verifies |
|---|---|
| `docker-build` (CI job) | `docker build .` succeeds — catches missing files, broken imports |

**Smoke test against container** (`internal/server/smoke_test.go`):

| Test | What it verifies |
|---|---|
| `TestDockerSmoke` | Build the Docker image with testcontainers' `GenericContainer`, start it alongside a Postgres testcontainer on a shared Docker network, hit `GET /tree?branch=main` → 200 with `[]`. Verifies the packaged binary boots, runs migrations, and serves requests. |

**GitHub Actions extension** — add a `docker` job to CI:

```yaml
  docker:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: Build Docker image
        run: docker build -t docstore-server:test .
```

The smoke test runs as part of `go test ./...` since it uses testcontainers (Docker-in-Docker works on GitHub Actions `ubuntu-latest`).

### Success criteria

```bash
# Build and push
gcloud builds submit --tag gcr.io/PROJECT/docstore-server

# Deploy
gcloud run deploy docstore-server \
  --image gcr.io/PROJECT/docstore-server \
  --add-cloudsql-instances PROJECT:REGION:docstore-mvp \
  --set-env-vars DATABASE_URL="host=/cloudsql/PROJECT:REGION:docstore-mvp dbname=docstore user=docstore-server password=..." \
  --allow-unauthenticated

# Get the URL
URL=$(gcloud run services describe docstore-server --format='value(status.url)')

# Test against deployed server
curl -s $URL/tree?branch=main | jq .
# → []

ds init $URL
echo "deployed!" > hello.txt
ds commit -m "first deployed commit"
# → Committed sequence 1 (1 file changed)

curl -s $URL/file/hello.txt?branch=main
# → deployed!
```

---

## Project Layout

```
docstore/
├── .github/
│   └── workflows/
│       └── ci.yml            # Test + lint + build + docker
├── cmd/
│   ├── server/
│   │   └── main.go          # HTTP server entrypoint
│   └── ds/
│       └── main.go          # CLI entrypoint
├── internal/
│   ├── server/
│   │   ├── handler.go       # HTTP handlers
│   │   ├── handler_test.go  # HTTP integration tests
│   │   ├── store.go         # Database operations
│   │   ├── store_test.go    # Store unit tests (testcontainers)
│   │   ├── smoke_test.go    # Docker container smoke test
│   │   └── middleware.go    # Identity extraction
│   ├── cli/
│   │   ├── init.go
│   │   ├── status.go
│   │   ├── status_test.go   # Status/change detection unit tests
│   │   ├── commit.go
│   │   ├── commit_test.go   # Commit unit tests
│   │   ├── checkout.go
│   │   ├── pull.go
│   │   ├── merge.go
│   │   ├── diff.go
│   │   ├── integration_test.go  # CLI integration tests
│   │   └── e2e_test.go          # Full workflow end-to-end test
│   └── testutil/
│       └── testdb.go        # Shared testcontainer Postgres helper
├── migrations/
│   └── 001_initial.sql
├── Dockerfile
├── go.mod
├── go.sum
├── DESIGN.md
└── MVP.md
```
