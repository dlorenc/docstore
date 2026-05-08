# Contributing

## Developer setup

**Requirements:** Go 1.25+, Docker, PostgreSQL (for integration tests).

```bash
git clone https://github.com/dlorenc/docstore
cd docstore
go build ./...    # compile everything
go vet ./...      # static analysis
```

## Running tests

Integration tests use [testcontainers-go](https://golang.testcontainers.io/) to spin up a real PostgreSQL container. Docker must be running.

```bash
go test ./... -count=1
```

The `-count=1` flag disables test caching, which is important for integration tests that interact with a real database.

To run only unit tests (no Docker required):

```bash
go test ./internal/policy/... ./internal/executor/...
```

## Building binaries

```bash
make build      # builds bin/docstore (server)
make build-ds   # builds bin/ds (CLI) with default remote URL compiled in
```

Override the default remote at build time:

```bash
DEFAULT_REMOTE=https://your-server.example.com make build-ds
```

## Running a local server

```bash
# Start Postgres however you prefer, then:
export DATABASE_URL="postgres://localhost/docstore?sslmode=disable"
go run ./cmd/docstore --dev-identity you@example.com
```

The server runs on port 8080. With `--dev-identity`, OAuth JWT validation is bypassed and every request is treated as the given identity. **This flag is for local development only and must never be used in production.** Production is deployed at `https://docstore.dev` with direct Google OAuth 2.0 authentication.

## Project structure

```
cmd/
  docstore/      — server binary
  ds/            — CLI binary
  ci-scheduler/  — CI scheduler binary
  ci-worker/     — CI worker binary
internal/
  server/        — HTTP handlers and middleware
  cli/           — CLI application logic
  db/            — Database layer (pgx + migrations)
  store/         — Read-only store interface
  policy/        — OPA policy engine
  executor/      — BuildKit executor
  events/        — Event broker and outbox
  tui/           — Bubble Tea terminal UI
  blob/          — External blob storage (GCS/local)
  logstore/      — CI log storage
api/             — Shared API types (wire format)
sdk/go/docstore/ — Go SDK (separate module)
deploy/k8s/      — Kubernetes manifests
scripts/         — GCP/GKE setup scripts
test/hello/      — Example repo used in integration tests
```

## Adding a new HTTP handler

Every new handler must follow this checklist:

### 1. Call `s.validateRepo(w, r, repo)` first

Before any database access, call `validateRepo` to ensure the repo exists and the caller has access. Missing this check allows unauthenticated/unauthorized access to resources in nonexistent or inaccessible repos.

```go
func (s *server) handleMyEndpoint(w http.ResponseWriter, r *http.Request) {
    repo := r.PathValue("name")
    if !s.validateRepo(w, r, repo) {
        return
    }
    // ... rest of handler
}
```

### 2. Guard against `main` on branch-mutation endpoints

Any endpoint that modifies branch state (not just reads) must reject attempts to mutate `main`:

```go
bname := r.PathValue("bname")
if bname == "main" {
    writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cannot modify main branch"})
    return
}
```

This is consistent with `handleDeleteBranch` and `handleMerge`.

### 3. Check the RBAC role

Use the role already set in context by `RBACMiddleware`. Do not re-query the database:

```go
role := RoleFromContext(r.Context())

// Reads: reader+
// No explicit check needed — RBACMiddleware allows all GET.

// Writes: writer+
if role != model.RoleWriter && role != model.RoleMaintainer && role != model.RoleAdmin {
    writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
    return
}

// Admin-only:
if role != model.RoleAdmin {
    writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
    return
}
```

Or use `roleAllows` for new paths by updating the middleware's `roleAllows` function in `internal/server/middleware.go`.

### 4. Register the route

Add the route in `buildHandler` in `internal/server/server.go`:

```go
inner.HandleFunc("POST /repos/", s.handleReposPrefix)
// For repo-scoped routes, add a case to handleReposPrefix.
```

For repo-scoped endpoints, add a case in the `handleReposPrefix` dispatcher in `internal/server/handlers.go`.

### 5. Write tests

Add handler tests to `internal/server/integration_test.go`. Use the `newTestServer` helper to spin up a test server with a real database via testcontainers.

## Documentation

Documentation source lives in `docs/` as Markdown files, rendered by MkDocs with the Material theme.

**Local preview:**

```bash
pip install mkdocs-material
mkdocs serve
```

The live site reloads on each save at `http://127.0.0.1:8000`.

**Test the Go server locally:**

```bash
mkdocs build
KO_DATA_PATH=cmd/docs/.kodata go run ./cmd/docs
```

This builds the static site into `site/` and serves it via the Go server at `http://localhost:8080`.

## Before submitting a PR

Run the full test suite and verify the build:

```bash
go test ./... -count=1
go build ./...
go vet ./...
```

Do not open a PR with failing tests. If a pre-existing test is flaky and unrelated to your change, note this explicitly in the PR description and notify the team.

## Code style

- Follow standard Go conventions (`gofmt`, `go vet`).
- Use `slog` for structured logging, not `log.Printf`.
- Return errors from DB functions rather than logging inside them.
- Write table-driven tests where appropriate.
- Do not add doc comments or type annotations to code you did not change.
