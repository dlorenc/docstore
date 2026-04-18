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

### `ds init [<remote-url>] [--author <name>] [--repo <name>]`

Initialize a docstore workspace in the current directory.

- `<remote-url>` — base server URL, optionally including the repo path (`https://host/repos/acme/myrepo`). **Optional** when `make build-ds` was used — the default Cloud Run URL is compiled in.
- `--author <name>` — identity to use for commits (defaults to OS username)
- `--repo <name>` — full repo name `owner/name` (overrides any name embedded in the URL; defaults to `default/default`)

Creates `.docstore/config.json` and `.docstore/state.json`, then syncs files from `main`.

```bash
ds init https://docstore.example.com/repos/acme/docs --author jane@example.com

# If built with make build-ds, the URL can be omitted:
ds init --repo acme/docs
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

Binary files (images, PDFs, compiled assets, etc.) are detected automatically and committed with the appropriate `content_type` MIME type.

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

Show what files changed on the current branch relative to its base, and what changed on `main` in the same window (potentially conflicting). Binary files are shown with a `[binary]` label instead of content.

```
Changed files on 'feature/my-change':
  changed: docs/guide.md
  changed: images/logo.png [binary]
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

### `ds branches [--status active|merged|abandoned]`

List all branches with their head and base sequences.

```bash
ds branches
ds branches --status active
```

---

### `ds reviews [--branch <name>]`

List reviews for the current branch (or `--branch <name>`). Stale reviews (before the latest commit) are marked `[stale]`.

---

### `ds review --status approved|rejected [--body "..."] [--branch <name>]`

Submit a review for the current branch (or `--branch <name>`).

```bash
ds review --status approved
ds review --status rejected --body "needs more tests"
```

A reviewer cannot approve their own commits.

---

### `ds checks [--branch <name>]`

List CI check runs for the current branch. Stale checks are marked `[stale]`.

---

### `ds check --name <name> --status passed|failed [--branch <name>]`

Report a CI check result.

```bash
ds check --name ci/build --status passed
```

---

### `ds tui`

Launch the Bubble Tea terminal UI for reviewing branches without leaving the terminal.

- Branch list with review/CI summary columns; `j`/`k` to navigate
- Branch detail panels: Diff / Reviews / Checks — cycle with `Tab`
- Review overlay with Approve/Reject toggle; `Esc` to cancel
- Inline merge prompt (`y`/`N`)
- `R` to refresh; `q` to quit

---

### `ds import-git <path> [--mode squash|replay]`

Import a local git repository's default branch into docstore `main`.

- `replay` (default) — one docstore commit per git commit; original author embedded in message as `[git-author: email]`
- `squash` — single commit with all files at HEAD

```bash
ds import-git /path/to/my-git-repo
ds import-git /path/to/my-git-repo --mode squash
```

---

### `ds orgs` / `ds repos` / `ds roles`

Manage orgs, repos, and roles without a workspace. These commands use the compiled-in default remote or accept `--remote <url>`.

```bash
# Orgs
ds orgs                          # list orgs
ds orgs create acme              # create an org
ds orgs delete acme              # delete an org
ds orgs repos acme               # list repos in acme

# Repos
ds repos                         # list all repos
ds repos create acme myrepo      # create acme/myrepo
ds repos delete acme/myrepo      # delete acme/myrepo

# Roles (require --remote or workspace context)
ds roles                         # list roles in current repo
ds roles set alice@example.com admin    # grant admin
ds roles delete alice@example.com       # revoke role
```

Valid roles: `reader`, `writer`, `maintainer`, `admin`.

---

## CI

DocStore has a built-in CI system that runs `.docstore/ci.yaml` checks on every branch commit. Jobs are queued in PostgreSQL and executed inside [Kata CLH](https://katacontainers.io/) microVMs on GKE using [BuildKit](https://github.com/moby/buildkit) as the execution engine.

See [docs/ci-architecture.md](docs/ci-architecture.md) for the full architecture, Kata guest environment details, and deployment instructions.

---

## Policy Engine

The policy engine is a merge gate. Before any branch can merge into `main`, the server evaluates all active Rego policies and returns a pass/fail result for each one. You can also query the policy status for a branch at any time without merging via `GET /repos/{name}/-/branch/:bname/status`.

If any policy denies the merge, `ds merge` exits with an error listing which policies failed and why. Policies have no effect on reads, commits, or branch creation — only merges are gated.

### Creating a Policy

Drop a `.rego` file at `.docstore/policy/<name>.rego` on the `main` branch. Policy files are versioned like any other file and go through the normal branch → review → merge workflow, so policy changes themselves require approval before taking effect.

**Package naming:** every policy must declare `package docstore.<name>` where `<name>` is a short identifier (e.g. `require_review`, `ci_must_pass`). The name shown in API responses and error messages is derived from the last segment of the package path.

**Required rules:**

```rego
package docstore.my_policy

import rego.v1

default allow := false

allow if {
    # ... conditions ...
}

reason := "human-readable denial message"
```

- `allow` — required boolean; defaults to `false`.
- `reason` — optional string; returned in the denial message when `allow` is `false`. Use `if` syntax in rule bodies and include `import rego.v1` (OPA v1 syntax is required).

### Input Schema

Every policy evaluation receives an `input` document with the following fields (see `internal/policy/engine.go`):

| Field | Type | Description |
|---|---|---|
| `input.actor` | string | Identity of the user attempting the merge (e.g. `alice@example.com`) |
| `input.actor_roles` | array of strings | Roles held by the actor in this repo (e.g. `["maintainer"]`); empty if none |
| `input.action` | string | Always `"merge"` |
| `input.repo` | string | Full repo name (e.g. `acme/myrepo`) |
| `input.branch` | string | Name of the branch being merged |
| `input.draft` | bool | Whether the branch is marked as a draft |
| `input.changed_paths` | array of strings | Paths modified on the branch relative to its base |
| `input.reviews` | array of objects | Reviews at the current head (see below) |
| `input.check_runs` | array of objects | CI check results at the current head (see below) |
| `input.owners` | object | Map of file path → owner list (see OWNERS Files below) |
| `input.head_sequence` | number | Branch head sequence number |
| `input.base_sequence` | number | Sequence where the branch forked from main |

**Review objects** (`input.reviews[i]`):
- `reviewer` — identity who submitted the review
- `status` — `"approved"` or `"rejected"`
- `sequence` — sequence number the review was submitted at

**Check run objects** (`input.check_runs[i]`):
- `check_name` — name of the check (e.g. `"ci/build"`)
- `status` — `"passed"` or `"failed"`
- `sequence` — sequence number the check was reported at

### OWNERS Files

Place an `OWNERS` file in any directory. One identity per line; lines starting with `#` are comments; blank lines are ignored.

```
# src/OWNERS
alice@example.com
bob@example.com
```

Ownership is resolved by **longest-prefix matching** (see `internal/policy/owners.go`). If `src/pkg/OWNERS` exists, it takes precedence over `src/OWNERS` for files under `src/pkg/`. The root `OWNERS` file (at the repo root) is the fallback for any path not covered by a more specific file. If no OWNERS file covers a path, `input.owners["that/path"]` is `null`.

In policies, `input.owners` is keyed by **file path** (not directory), with each value being the resolved owner list for that file:

```rego
input.owners["src/api.go"]  # => ["alice@example.com", "bob@example.com"]
```

### Staleness Rule

Reviews and check runs are tied to `head_sequence`. Any new commit on the branch advances `head_sequence`, which automatically invalidates all prior reviews and checks — the policy engine sees empty lists until the branch is re-reviewed and re-checked at the new head. This prevents approving a branch then sneaking in additional commits before merging.

`ds reviews` and `ds checks` show stale entries marked `[stale]` so reviewers know what needs to be re-done after a new commit.

### Bootstrap Mode

If no `.rego` files exist at `.docstore/policy/` on `main`, the policy engine is disabled and all merges are allowed (subject to RBAC role checks). This avoids a chicken-and-egg problem when setting up a new repo — you don't need a policy approval to land the first policy.

### Example Policies

**1. Require at least one approval before merge**

```rego
package docstore.require_review

import rego.v1

default allow := false

allow if {
    some rev in input.reviews
    rev.status == "approved"
}

reason := "at least one approved review is required"
```

**2. Require a specific CI check to pass**

```rego
package docstore.ci_must_pass

import rego.v1

default allow := false

allow if {
    some check in input.check_runs
    check.check_name == "ci/build"
    check.status == "passed"
}

reason := "ci/build check must pass before merging"
```

**3. Require codeowner approval for changes under `.docstore/`**

This pattern gates on path: if any changed file lives under `.docstore/`, a reviewer must be listed in its owners list.

```rego
package docstore.codeowner_approval

import rego.v1

default allow := false

# Pass if no .docstore/ paths are touched.
allow if {
    not any_docstore_path_changed
}

# Pass if a codeowner has approved.
allow if {
    any_docstore_path_changed
    some rev in input.reviews
    rev.status == "approved"
    some path in input.changed_paths
    strings.has_prefix(path, ".docstore/")
    some owner in input.owners[path]
    owner == rev.reviewer
}

any_docstore_path_changed if {
    some path in input.changed_paths
    strings.has_prefix(path, ".docstore/")
}

reason := "a codeowner must approve changes to .docstore/"
```

**4. Block merges from draft branches**

```rego
package docstore.no_draft_merge

import rego.v1

default allow := false

allow if {
    not input.draft
}

reason := "draft branches cannot be merged"
```

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
# Using the CLI (recommended)
ds orgs create acme
ds repos create acme myrepo

# Or via the API
curl -X POST https://docstore.example.com/orgs \
  -H "Content-Type: application/json" \
  -d '{"name": "acme"}'

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
