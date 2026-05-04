# Getting Started

## Install the `ds` CLI

Build from source (Go 1.25+ required):

```bash
git clone https://github.com/dlorenc/docstore
cd docstore
make build-ds
# Produces bin/ds with the default server URL compiled in
```

The build bakes in the default remote URL (`https://docstore.dev`) via `-ldflags`. You can override it at build time:

```bash
DEFAULT_REMOTE=https://your-server.example.com make build-ds
```

Copy `bin/ds` somewhere on your `$PATH`.

## Local development setup

To run a local server, you need PostgreSQL. The easiest path is:

```bash
# Start Postgres (any method — docker, homebrew, etc.)
export DATABASE_URL="postgres://localhost/docstore?sslmode=disable"

# Run migrations and start the server with dev auth bypass (local dev only)
go run ./cmd/docstore --dev-identity you@example.com

# Or with env vars instead of flags:
DEV_IDENTITY=you@example.com DATABASE_URL=... go run ./cmd/docstore
```

With `--dev-identity` set, the server skips OAuth JWT validation and treats every request as that identity. **This is for local development only** — do not set this flag in production. Production uses direct Google OAuth at `https://docstore.dev`.

The server listens on port 8080 by default (`PORT` env var overrides it).

## First commit walkthrough

### 1. Initialize a workspace

```bash
mkdir my-project && cd my-project
ds init http://localhost:8080 --author alice@example.com --repo myorg/myrepo
```

This creates a `.docstore/` directory with `config.json` (remote URL, repo path, author) and `state.json` (current branch, head sequence, file hashes).

If you compiled `ds` with the default remote URL, the URL argument is optional:

```bash
ds init --repo myorg/myrepo
```

### 2. Create and switch to a branch

```bash
ds checkout -b feature/hello
```

### 3. Add files and commit

```bash
echo "Hello, DocStore!" > hello.txt
ds status          # shows hello.txt as new
ds commit -m "add hello.txt"
```

`ds commit` uploads all changed files to the server in one atomic operation.

### 4. View history and diff

```bash
ds log             # list commits on the current branch
ds diff            # show files changed vs main
```

### 5. Merge to main

```bash
ds merge
```

Merge is a server-side operation. If active policies require reviews or CI checks, `ds merge` will fail with the policy reason until the requirements are satisfied.

### 6. Pull updates from the server

```bash
ds checkout main
ds pull
```

`ds pull` downloads the current tree from the server and writes files to disk, replacing your local copies.

## Config and state files

| File | Purpose |
|---|---|
| `.docstore/config.json` | Remote URL, repo path (`owner/name`), default author |
| `.docstore/state.json` | Active branch, head sequence number, per-file content hashes |

State is used to compute `ds status` without a round-trip to the server. It is updated by `ds pull`, `ds commit`, and `ds checkout`.
