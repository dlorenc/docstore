# Concepts

## What is DocStore?

DocStore is a server-side version control system. Unlike Git, there is no local object database. Every operation — commit, branch, merge, diff — happens on the server and is recorded in PostgreSQL. Clients interact through a REST API or the `ds` CLI.

## Orgs and repos

Repos are organized under orgs. A repo's full path is `owner/name` (e.g. `acme/platform`). The `/-/` separator in API URLs prevents ambiguity when repo or org names contain slashes in the future:

```
GET /repos/acme/platform/-/tree
```

## Branching model

There is one permanent branch: `main`. All other branches are feature branches created from the current head of `main`.

```
main:  ──A──B──C──────────────────E── (after merge)
                \                /
feature/x:       D──E──F (before merge)
```

- `ds checkout -b feature/x` creates a branch rooted at the current `main` head (stored as `base_sequence`).
- Commits to the branch increment the branch's `head_sequence`.
- Merging replays branch commits on top of the latest `main` head (or fast-forwards if `main` hasn't moved). Conflicts are detected file-by-file.
- Rebasing replays branch commits onto the latest `main` head, updating `base_sequence`. Use it to pick up changes from `main` before merging.
- Branch status is one of: `active`, `merged`, or `abandoned`.

## Draft branches

A branch can be created with `--draft` (`ds checkout -b feature/x --draft`). Draft branches are hidden from default `ds branches` output. Writers can promote a draft to ready with `ds ready`. Policies can inspect `input.draft` in Rego to block merging draft branches.

## Content addressing

Every file version is identified by a SHA256 hash of its content. The `documents` table stores one row per unique (repo, content_hash) pair. When two branches commit the same bytes for different files, the content is stored once. The `file_commits` table is the event log that links (commit sequence, path) to a version.

## Sequence numbers

Every commit on every branch in a repo shares a single monotonically increasing sequence counter. This means you can answer "what did file X look like at sequence 42?" across all branches. Sequences are the primary way to pin a point in history (reviews, check runs, and releases all store the sequence at which they were created).

## Content types

Files can carry an optional `content_type` field. If omitted, the server defaults to `application/octet-stream`. Binary files (detected by the CLI by scanning for null bytes) are stored with the binary flag set and shown as `[binary]` in diffs.

## Merge policies and staleness

After a new commit is pushed to a branch, all reviews and check runs created before that commit are considered stale. The `ds reviews` and `ds checks` commands mark stale items with `[stale]`. Merge policies can require that the current head has passing checks and approving reviews (not stale ones).

## Releases

A release pins a specific sequence number under a human-readable name (e.g. `v1.2.0`). Use `ds release create <name>` or `POST /repos/{name}/-/release`. Releases are immutable; deleting one removes the label, not the commits.

## How DocStore differs from Git

| | Git | DocStore |
|---|---|---|
| Storage | Local `.git/` object store | PostgreSQL (server-side) |
| Auth | SSH keys / HTTPS tokens | GCP IAP (JWT) |
| Access control | Repository-level (GitHub) | Per-repo RBAC with 4 roles |
| Merge gating | Branch protection rules | OPA Rego policies + OWNERS |
| CI | External (GitHub Actions) | Built-in (`ci.yaml` + BuildKit) |
| History | Commit graph (DAG) | Linear sequence per repo |
| Offline work | Full clone + local commits | Requires server connection |
