# CLI Reference

All commands are provided by the `ds` binary (`cmd/ds`). Most commands require a workspace (a directory containing `.docstore/config.json`). Org, repo, and role management commands work from any directory.

## Workspace commands

### `ds init`

Initialize a workspace in the current directory.

```
ds init [<remote-url>] [--author <name>] [--repo <owner/name>]
```

- `<remote-url>` — Server base URL. If omitted, uses the URL compiled into the binary.
- `--author` — Default author identity for commits.
- `--repo` — Repository to use in `owner/name` format. If omitted, the server's first repo is used.

Creates `.docstore/config.json` and `.docstore/state.json`.

### `ds status`

Show files changed locally since the last `ds pull` or `ds commit`.

```
ds status
```

Computes diff by comparing local file hashes against `state.json`. No network round-trip.

### `ds commit`

Commit all local changes to the current branch.

```
ds commit -m "message"
```

All changed, added, and deleted files are uploaded in one atomic request. The commit message is required.

### `ds checkout`

Switch branches or create a new branch.

```
ds checkout <branch>          # switch to existing branch
ds checkout -b <branch>       # create and switch to new branch
ds checkout -b <branch> --draft  # create as draft branch
```

Switching branches requires a clean working tree (no uncommitted changes).

### `ds pull`

Sync local files from the server.

```
ds pull [--skip-verify]
```

Downloads the current tree for the active branch and overwrites local files. `--skip-verify` skips hash-chain integrity verification.

### `ds merge`

Merge the current branch into `main`.

```
ds merge
```

Calls `POST /repos/{repo}/-/merge`. Fails if active policies block the merge (e.g. missing reviews or failing checks).

### `ds rebase`

Rebase the current branch onto the latest `main` head.

```
ds rebase
```

Replays the branch's commits on top of the current `main` head. Stops and writes conflict files if conflicts are detected.

### `ds resolve`

Mark a rebase conflict as resolved.

```
ds resolve <path>
```

After manually editing the conflicted file, run `ds resolve <path>` to mark it resolved, then continue with `ds commit`.

### `ds verify`

Verify the commit chain integrity for the current repo.

```
ds verify
```

Downloads the chain from `GET /repos/{repo}/-/chain` and verifies each hash link locally.

### `ds ready`

Mark the current branch as ready (not draft).

```
ds ready
```

Promotes a draft branch to active. Equivalent to `PATCH /repos/{repo}/-/branch/{name}` with `{draft: false}`.

### `ds diff`

Show files changed on the current branch relative to its base on `main`.

```
ds diff
```

### `ds log`

Show commit history.

```
ds log [path] [--limit N]
```

- `path` — Optional file path to show history for.
- `--limit N` — Maximum number of commits to show (default 20).

### `ds show`

Inspect a commit or file at a specific sequence.

```
ds show <sequence> [path]
```

- `<sequence>` — Integer sequence number.
- `path` — If provided, show the file content at that sequence. Otherwise show commit metadata.

### `ds purge`

Purge old merged/abandoned branches and their commits.

```
ds purge --older-than <Nd> [--dry-run]
```

- `--older-than <Nd>` — Purge branches last updated more than N days ago. Example: `--older-than 30d`.
- `--dry-run` — Print what would be purged without deleting.

Admin-only.

## Branch management

### `ds branches`

List branches.

```
ds branches [--status active|merged|abandoned] [--draft] [--include-draft]
```

- `--status` — Filter by status (default `active`).
- `--draft` — Show only draft branches.
- `--include-draft` — Include draft branches in the listing.

### `ds branch delete`

Delete a branch from the current repo.

```
ds branch delete <name>
```

Maintainer-only.

## Review and CI workflow

### `ds reviews`

List reviews for a branch.

```
ds reviews [--branch <name>]
```

If `--branch` is omitted, uses the current branch. Stale reviews (superseded by newer commits) are marked `[stale]`.

### `ds review`

Submit a review.

```
ds review --status approved|rejected [--body "..."] [--branch <name>]
```

- `--status` — Required. `approved` or `rejected`.
- `--body` — Optional review comment.
- `--branch` — Branch to review (default: current branch).

### `ds checks`

List CI check runs for a branch.

```
ds checks [--branch <name>] [--all]
```

- `--branch` — Branch to show checks for (default: current branch).
- `--all` — Show all check runs, including stale ones.

Stale check runs are marked `[stale]`.

### `ds check`

Report a CI check result (used by CI systems, not humans).

```
ds check --name <check_name> --status passed|failed [--branch <name>] [--log-url <url>] [--sequence <n>]
```

- `--name` — Check name (e.g. `ci/build`).
- `--status` — `passed` or `failed`.
- `--branch` — Branch to report on (default: current branch).
- `--log-url` — URL to build logs.
- `--sequence` — Sequence number this check applies to (default: current head).

### `ds comment`

Add an inline file comment on a branch.

```
ds comment --path <path> --body <body> [--branch <name>]
```

### `ds comments`

List inline file comments.

```
ds comments [--path <path>] [--branch <name>]
```

## Terminal UI

### `ds tui`

Launch the terminal UI.

```
ds tui
```

A Bubble Tea TUI showing a branch list and a detail panel with Diff, Reviews, and Checks tabs. Press Enter to inspect a branch, `m` to merge, `q` to quit.

## Import

### `ds import-git`

Import a local git repository into docstore `main`.

```
ds import-git <path> [--mode squash|replay]
```

- `<path>` — Path to a local git repository.
- `--mode squash` — Import all files as a single commit (default `replay`).
- `--mode replay` — Import each git commit as a separate docstore commit.

## Release management

### `ds release create`

Create a named release pinned to a sequence.

```
ds release create <name> [--sequence N] [--notes 'text']
```

If `--sequence` is omitted, the current head sequence is used.

### `ds release list`

List all releases.

```
ds release list
```

### `ds release show`

Show release metadata and tree.

```
ds release show <name>
```

### `ds release delete`

Delete a release (admin-only).

```
ds release delete <name>
```

## Org management

Commands in this group do not require a workspace.

```bash
ds orgs                          # list all orgs
ds orgs create <name>            # create an org
ds orgs get <name>               # get org details
ds orgs delete <name>            # delete an org (fails if it has repos)
ds orgs repos <name>             # list repos in an org
```

### Org membership

```bash
ds org members list <org>
ds org members add <org> <identity> --role owner|member
ds org members remove <org> <identity>
```

### Org invitations

```bash
ds org invites list <org>
ds org invites create <org> --email <email> --role owner|member
ds org invites accept <org> <token>
ds org invites revoke <org> <invite-id>
```

## Repo management

```bash
ds repos                         # list all repos
ds repos create <owner> <name>   # create a repo
ds repos delete <owner/name>     # delete a repo

ds repo list                     # alias for ds repos
ds repo create <owner/name>      # create (accepts owner/name format)
ds repo get <owner/name>         # get repo details
ds repo delete <owner/name>      # delete a repo
```

## Event subscriptions

Subscription management via the CLI requires the appropriate role (reader+ for repo-scoped, global admin for global). See [docs/events.md](events.md) for the full guide.

### `ds subscriptions create`

Create a webhook subscription.

```
ds subscriptions create --url <url> [--repo <owner/name>] [--event-types <type1,type2,...>] [--secret <secret>]
```

- `--url` — Required. HTTPS endpoint to deliver events to.
- `--repo` — Scope to a specific repo. Omit for a global subscription (admin-only).
- `--event-types` — Comma-separated list of event types to receive. Omit to receive all types.
- `--secret` — HMAC-SHA256 signing secret. When set, deliveries include `X-DocStore-Signature: sha256=<hmac>`.

```bash
# Repo-scoped, specific event types, with HMAC signing
ds subscriptions create \
  --repo acme/platform \
  --event-types com.docstore.commit.created,com.docstore.branch.merged \
  --url https://hooks.example.com/docstore \
  --secret my-hmac-secret

# Global subscription, all event types
ds subscriptions create --url https://hooks.example.com/global
```

### `ds subscriptions list`

List all subscriptions.

```
ds subscriptions list
```

Prints a table with columns: ID, REPO (`(all)` for global subscriptions), BACKEND, SUSPENDED (the suspension timestamp, or `no` if active).

### `ds subscriptions delete`

Delete a subscription.

```
ds subscriptions delete <id>
```

### `ds subscriptions resume`

Resume a suspended subscription.

```
ds subscriptions resume <id>
```

Clears the suspension and resets the failure counter to 0. The subscription immediately re-enters the active delivery queue.

## Role management

Role commands are scoped to the current workspace's repo.

```bash
ds roles                         # list roles for the current repo
ds roles set <identity> <role>   # set a role
ds roles delete <identity>       # remove a role

# Aliases:
ds role list
ds role set <identity> <role>
ds role delete <identity>
```

Valid roles: `reader`, `writer`, `maintainer`, `admin`. See [policy.md](policy.md) for what each role permits.

## Repo secrets

Repo secrets are opaque key/value credentials surfaced to CI jobs as
BuildKit secret mounts at `/run/secrets/<NAME>`. Plaintext is never
returned through any read API. See [secrets.md](secrets.md) for the full
model, threat model, and CI integration.

```bash
ds secrets list                                       # list metadata only
ds secrets set <NAME> -                               # value from stdin
ds secrets set <NAME> --from-file=<path>              # value from file
ds secrets set <NAME> --description="<text>" -        # with description
ds secrets unset <NAME>                               # delete
```

`--value=<plaintext>` is **refused** — plaintext from `argv` would land in
shell history and `ps eww`. The `set` and `unset` subcommands require
admin role on the repo. Names must match `^[A-Z][A-Z0-9_]{0,63}$` and may
not use the reserved `DOCSTORE_` prefix; values are capped at 32 KiB.
