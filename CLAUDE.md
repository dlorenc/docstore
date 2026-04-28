# Docstore — Developer Guide

## Dev loop: web UI iteration

### 1. Start Postgres

```bash
docker compose up -d postgres
```

The container exposes Postgres on `localhost:5432` with:
- user: `docstore`
- password: `docstore`
- database: `docstore`

```bash
export DATABASE_URL="postgres://docstore:docstore@localhost:5432/docstore?sslmode=disable"
```

### 2. Run the server in dev mode

`DEV_IDENTITY` bypasses IAP authentication (required for local dev).
`DEV_UI` makes the server read HTML templates from `internal/ui/templates/` on
disk instead of the embedded copies, so template changes take effect on the next
request without recompiling.

```bash
export DEV_IDENTITY="you@example.com"
export DEV_UI=1
export DATABASE_URL="postgres://docstore:docstore@localhost:5432/docstore?sslmode=disable"
go run ./cmd/docstore
```

The server starts on `http://localhost:8080`. The web UI is at
`http://localhost:8080/ui/`.

### 3. Hot-reload on Go changes (air)

Install [air](https://github.com/air-verse/air) if you haven't:

```bash
go install github.com/air-verse/air@latest
```

Then run from the repo root (the `.air.toml` is pre-configured):

```bash
DEV_IDENTITY="you@example.com" DEV_UI=1 DATABASE_URL="postgres://docstore:docstore@localhost:5432/docstore?sslmode=disable" air
```

Air watches `*.go` files and rebuilds+restarts the server automatically.
Template/CSS/JS edits take effect immediately without restart because `DEV_UI`
reads them from disk at request time.

### 4. Visual iteration with Playwright MCP

With the server running, you can use the Playwright MCP tool to inspect and
iterate on the UI visually:

```
// In Claude Code:
mcp__playwright__browser_navigate({ url: "http://localhost:8080/ui/" })
mcp__playwright__browser_screenshot({})
```

Typical loop:
1. Edit a template in `internal/ui/templates/` or CSS in `internal/ui/static/`
2. Refresh the browser (or re-screenshot via Playwright MCP)
3. Inspect, adjust, repeat

Go code changes trigger an air rebuild; template/CSS changes are instant.

## Environment variables

| Variable | Purpose |
|---|---|
| `DATABASE_URL` | Postgres DSN (required) |
| `DEV_IDENTITY` | Bypass IAP — use this email as the caller identity |
| `DEV_UI` | Read templates from disk instead of embedded binary |
| `PORT` | HTTP listen port (default: `8080`) |
| `LOG_FORMAT` | `text` for human-readable logs (default: JSON) |
| `LOG_LEVEL` | `debug`, `info`, `warn`, `error` (default: `info`) |

## Running tests

```bash
go test ./... -count=1
go build ./...
go vet ./...
```

Tests that require Postgres use `TEST_DATABASE_URL`; if unset those tests are
skipped automatically.
