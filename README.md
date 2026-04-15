# DocStore

DocStore is a version control system for structured documents, built on Postgres. It provides a git-like workflow — branches, commits, merges, rebases, and conflict resolution — with all state stored server-side in a relational database.

## Overview

- **Server-side VCS**: all history lives in Postgres; the working directory is a local cache
- **Multi-repo**: one server hosts many named repositories, organized into orgs
- **Orgs**: repos belong to an org; repo names are `owner/name` full paths (e.g. `acme/myrepo`)
- **Branching model**: one permanent `main` branch; all work happens on named feature branches
- **Content deduplication**: identical file contents are stored once (content-addressed by SHA256)
- **RBAC**: per-repo roles (reader / writer / maintainer / admin)

---

## Quick Start

### 1. Install the CLI

```bash
go install github.com/dlorenc/docstore/cmd/ds@latest
```

Or build from source:

```bash
make build-ds    # produces bin/ds
```

### 2. Initialize a workspace

Repos are owned by orgs. The repo name is an `owner/name` full path (e.g. `acme/myrepo`).

```bash
ds init https://docstore.example.com/repos/acme/myrepo
```

This creates a `.docstore/` config directory and downloads the current tree from `main`.

You can also pass the repo name separately:

```bash
ds init https://docstore.example.com --repo acme/myrepo
```

If no repo is specified and the URL has no `/repos/` segment, the repo name defaults to `default/default`.

### 3. Make changes, commit, and push

```bash
# See what changed
ds status

# Commit all local changes
ds commit -m "add new policy file"
```

### 4. Work on a branch

```bash
# Create and switch to a new branch
ds checkout -b feature/my-change

# Make some edits, then commit
ds commit -m "work in progress"

# Merge back into main when ready
ds merge
```

---

## Identity

Every commit is attributed to an identity. The identity is resolved in this order:

1. The `author` field in `.docstore/config.json` (set during `ds init`)
2. The `--author` flag passed to `ds init`
3. The current OS username (fallback)

The identity is sent in the `X-DocStore-Identity` HTTP header on every request. On a production server with IAP enabled, the server ignores this header and uses the IAP-validated email instead.

To override the identity at workspace creation time:

```bash
ds init https://docstore.example.com/repos/myrepo --author alice@example.com
```

---

## CLI Reference

### `ds init <remote-url> [--author <name>] [--repo <name>]`

Initialize a docstore workspace in the current directory.

- `<remote-url>` — base server URL, optionally including the repo path (`https://host/repos/acme/myrepo`)
- `--author <name>` — identity to use for commits (defaults to OS username)
- `--repo <name>` — full repo name `owner/name` (overrides any name embedded in the URL; defaults to `default/default`)

Creates `.docstore/config.json` and `.docstore/state.json`, then syncs files from `main`.

```bash
ds init https://docstore.example.com/repos/acme/docs --author jane@example.com
```

---

### `ds status`

Show local changes relative to the last sync.

```
On branch feature/my-change

Changes:
  new:      docs/guide.md
  modified: README.md
  deleted:  old-file.txt
```

---

### `ds commit -m "<message>"`

Commit all local changes (new, modified, and deleted files) to the current branch on the server.

- `-m "<message>"` — required commit message

```bash
ds commit -m "update access control docs"
```

The server assigns a monotonically increasing sequence number. The local state is updated after a successful commit so `ds status` shows clean.

---

### `ds checkout <branch>`

Switch to an existing branch. Requires a clean working directory (no uncommitted changes).

```bash
ds checkout main
```

Downloads files for the target branch and updates the local state.

---

### `ds checkout -b <branch>`

Create a new branch from the current `main` head and switch to it.

```bash
ds checkout -b feature/new-auth
```

The new branch starts at `main`'s current head sequence. Local files are not changed — the branch inherits whatever you currently have checked out.

---

### `ds pull`

Sync local files from the current branch on the server. Requires a clean working directory.

```bash
ds pull
```

Useful to pick up commits made by other users on the same branch.

---

### `ds merge`

Merge the current branch into `main`. Fails if there are conflicts; use `ds diff` to preview before merging.

```bash
ds merge
```

On success, switches the local workspace to `main` and syncs files. On conflict, lists the conflicting files and exits with an error — the branch is not merged.

---

### `ds diff`

Show what files changed on the current branch relative to its base, and what changed on `main` in the same window (potentially conflicting).

```
Changed files on 'feature/my-change':
  changed: docs/guide.md
  deleted: old-file.txt

Changed files on main:
  changed: docs/guide.md

Conflicts:
  docs/guide.md (main: <version-id>, branch: <version-id>)
```

---

### `ds log [path] [--limit N]`

Show commit history.

- Without `path` — lists commits on the current branch (newest first), from the branch head down to the base sequence
- With `path` — shows commits that touched that specific file on the current branch
- `--limit N` — max entries to show (default: 20)

```bash
ds log                        # branch history
ds log docs/guide.md          # file history
ds log --limit 5              # last 5 commits
ds log docs/guide.md --limit 10
```

Output format:
```
seq 42    alice        2024-01-15  update access control docs
seq 41    bob          2024-01-14  initial draft
```

---

### `ds show <sequence> [path]`

Inspect a specific commit or a file's content at a given sequence number.

- Without `path` — prints commit metadata and the list of files changed
- With `path` — prints the raw file content at that sequence

```bash
ds show 42              # show commit 42
ds show 42 docs/guide.md  # show file content at sequence 42
```

---

### `ds rebase`

Rebase the current branch onto the latest `main` head. This replays branch commits on top of the current `main`, updating the branch's base and head sequences.

```bash
ds rebase
```

On success, updates local state with the new head sequence.

On conflict, writes conflict files for each affected path:

```
docs/guide.md.main    ← current main version
docs/guide.md.branch  ← current branch version
```

Edit `docs/guide.md` to your liking, then run `ds resolve docs/guide.md`.

---

### `ds resolve <path>`

Resolve a rebase conflict for `<path>`.

```bash
# After editing docs/guide.md to resolve the conflict:
ds resolve docs/guide.md
```

- Reads the resolved content from `<path>`
- Requires `<path>.main` and `<path>.branch` to exist (written by `ds rebase`)
- Commits the resolved content to the current branch
- Removes the conflict files
- Updates local state

Repeat for each conflicting file, then continue with `ds rebase` or `ds merge`.

---

## Branching and Merging

DocStore uses a simple, linear branching model:

```
main:    ──A──B──C──────────────M──
                 \             /
feature:          D──E──F─────
```

1. **Branch**: `ds checkout -b feature` forks from main at sequence C
2. **Commit**: `ds commit` adds D, E, F on the feature branch
3. **Diff** (optional): `ds diff` shows what changed and any conflicts
4. **Rebase** (optional): `ds rebase` replays D→E→F on top of the latest main head
5. **Merge**: `ds merge` fast-forward merges into main, creating M

If main has moved since the branch was created and the same files were edited on both sides, `ds merge` (and `ds rebase`) will report conflicts.

### Conflict Resolution Workflow

```bash
# 1. Attempt rebase (or merge)
ds rebase
# conflict: docs/guide.md (wrote docs/guide.md.main, docs/guide.md.branch)

# 2. Examine both versions
cat docs/guide.md.main    # what's on main
cat docs/guide.md.branch  # what's on the branch

# 3. Edit the working copy to the desired result
$EDITOR docs/guide.md

# 4. Mark as resolved
ds resolve docs/guide.md

# 5. Try again
ds merge
```

---

## Workspace Layout

```
<working-directory>/
├── .docstore/
│   ├── config.json   ← remote URL, repo, branch, author
│   └── state.json    ← last-synced sequence and file hashes
└── <your files>
```

`.docstore/` is excluded from commit scanning; never track it manually.

---

## Orgs and Repos

Repos live under orgs. An org must be created before a repo can be created in it.

```bash
# Create an org
curl -X POST https://docstore.example.com/orgs \
  -H "Content-Type: application/json" \
  -d '{"name": "acme"}'

# Create a repo in that org
curl -X POST https://docstore.example.com/repos \
  -H "Content-Type: application/json" \
  -d '{"owner": "acme", "name": "myrepo"}'
```

The full repo name is `owner/name` (e.g. `acme/myrepo`). Repo names can contain additional slashes for subgroup nesting (e.g. `acme/team/subrepo`), in which case `acme` is still the owner (org) and `team/subrepo` is the repo name within that org.

---

## Server URL Format

All repo-scoped API routes use a `/-/` separator to cleanly separate the full repo name (which may contain slashes) from the endpoint:

```
GET  /repos/acme/myrepo/-/tree
POST /repos/acme/myrepo/-/commit
GET  /repos/acme/team/subrepo/-/branches
```

For the CLI, you can give `ds init` the full path:

```bash
ds init https://docstore.example.com/repos/acme/myrepo
```

Or the base URL with `--repo`:

```bash
ds init https://docstore.example.com --repo acme/myrepo
```

Both are equivalent. The CLI stores only the base URL and repo name separately in `.docstore/config.json`. A fresh server seeds a `default` org and a `default/default` repo — the CLI defaults to this repo when no repo is specified.
