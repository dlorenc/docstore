# API Reference

All endpoints (except `GET /healthz`) require authentication. In production, the server validates the GCP IAP JWT from the `X-Goog-IAP-JWT-Assertion` header. In dev/test, set the `DEV_IDENTITY` env var on the server to bypass JWT validation.

Repo-scoped endpoints use the `/-/` separator to allow org/repo names to contain slashes without ambiguity:

```
GET /repos/acme/platform/-/tree
POST /repos/myorg/subrepo/-/commit
```

## Health

### `GET /healthz`

Returns server status. No authentication required.

**Response 200:**
```json
{"status": "ok"}
```

## Orgs

### `POST /orgs`

Create an org.

**Body:**
```json
{"name": "acme"}
```

**Response 201:** Org object.

### `GET /orgs`

List all orgs.

**Response 200:**
```json
{"orgs": [...]}
```

### `GET /orgs/{org}`

Get a single org.

**Response 200:** Org object. 404 if not found.

### `DELETE /orgs/{org}`

Delete an org. Fails with 409 if the org has repos.

**Response 204.**

### `GET /orgs/{org}/repos`

List repos owned by the org.

**Response 200:**
```json
{"repos": [...]}
```

## Org membership

### `GET /orgs/{org}/members`

List org members.

### `POST /orgs/{org}/members/{identity}`

Add a member. Body: `{"role": "owner|member"}`.

### `DELETE /orgs/{org}/members/{identity}`

Remove a member.

## Org invitations

### `GET /orgs/{org}/invites`

List pending invitations.

### `POST /orgs/{org}/invites`

Create an invitation. Body: `{"email": "...", "role": "owner|member"}`. Returns a token.

### `POST /orgs/{org}/invites/{token}/accept`

Accept an invitation by token.

### `DELETE /orgs/{org}/invites/{id}`

Revoke a pending invitation.

## Repos

### `POST /repos`

Create a repo.

**Body:**
```json
{"owner": "acme", "name": "platform"}
```

**Response 201:** Repo object.

### `GET /repos`

List all repos.

### `GET /repos/{name}`

Get a repo. `{name}` is the full path (`acme/platform`).

### `DELETE /repos/{name}`

Delete a repo.

## Branches

### `GET /repos/{name}/-/branches`

List branches.

**Query parameters:**
- `status` — `active` (default), `merged`, or `abandoned`.
- `draft` — `true` to show only draft branches.
- `include_draft` — `true` to include draft branches.

**Response 200:**
```json
{"branches": [{"name": "...", "head_sequence": 42, "base_sequence": 10, "status": "active", "draft": false, "created_by": "...", "created_at": "..."}]}
```

### `POST /repos/{name}/-/branch`

Create a branch rooted at the current `main` head.

**Body:**
```json
{"name": "feature/x"}
```

**Response 201.**

### `GET /repos/{name}/-/branch/{bname}`

Get branch metadata (head sequence, base sequence, status, draft flag).

### `PATCH /repos/{name}/-/branch/{bname}`

Update branch flags. Currently supports promoting draft to ready.

**Body:**
```json
{"draft": false}
```

### `DELETE /repos/{name}/-/branch/{bname}`

Delete a branch. Cannot delete `main`. Maintainer-only.

### `GET /repos/{name}/-/branch/{bname}/status`

Evaluate merge policies without merging. Returns mergeability and per-policy results.

**Response 200:**
```json
{
  "mergeable": true,
  "policies": [
    {"name": "require_review", "pass": true, "reason": ""}
  ]
}
```

## Commits

### `POST /repos/{name}/-/commit`

Create an atomic commit containing one or more file changes.

**Body:**
```json
{
  "branch": "feature/x",
  "message": "update config",
  "files": [
    {"path": "config.yaml", "content": "<base64>", "content_type": "text/yaml"},
    {"path": "old.txt"}
  ]
}
```

- `files[].content` — Base64-encoded file content. Omit or set to null to delete the file.
- `files[].content_type` — Optional MIME type.

The commit author is always the authenticated identity; clients cannot override it.

**Response 201:**
```json
{"sequence": 43}
```

### `GET /repos/{name}/-/commit/{sequence}`

Get commit metadata for a specific sequence number.

**Response 200:**
```json
{
  "sequence": 43,
  "branch": "feature/x",
  "message": "update config",
  "author": "alice@example.com",
  "created_at": "...",
  "files": [{"path": "config.yaml", "version_id": "..."}]
}
```

## Tree and files

### `GET /repos/{name}/-/tree`

Materialize the directory tree.

**Query parameters:**
- `branch` — Branch name (default `main`).
- `at` — Sequence number to materialize at.
- `limit` — Maximum number of entries (default 100).
- `after` — Pagination cursor (path of the last entry from the previous page).

**Response 200:**
```json
{
  "entries": [
    {"path": "config.yaml", "version_id": "...", "content_type": "text/yaml", "binary": false}
  ]
}
```

### `GET /repos/{name}/-/file/{path}`

Get file content.

**Query parameters:**
- `branch` — Branch name (default `main`).
- `at` — Sequence number.

**Response 200:**
```json
{"path": "config.yaml", "content": "<base64>", "content_type": "text/yaml", "version_id": "..."}
```

404 if the file does not exist on the branch at the requested sequence.

### `GET /repos/{name}/-/file/{path}/history`

Get the commit history for a file.

**Query parameters:**
- `branch` — Branch name (default `main`).
- `limit` — Maximum entries (default 100).
- `after` — Pagination cursor (sequence number of the last entry).

## Diff

### `GET /repos/{name}/-/diff`

Show files changed on a branch relative to its base on `main`.

**Query parameters:**
- `branch` — Branch name (required).

**Response 200:**
```json
{
  "changes": [
    {"path": "config.yaml", "status": "modified", "binary": false, "diff": "..."}
  ],
  "conflicts": []
}
```

## Merge and rebase

### `POST /repos/{name}/-/merge`

Merge a branch into `main`. Policy evaluation runs before the merge.

**Body:**
```json
{"branch": "feature/x"}
```

**Response 200:** Merge result including new `main` head sequence.

**Response 409:** Merge conflicts (body includes conflict details).

**Response 403:** Policy denied (body includes policy results).

### `POST /repos/{name}/-/rebase`

Rebase a branch onto the current `main` head.

**Body:**
```json
{"branch": "feature/x"}
```

**Response 200:** Rebase result.

**Response 409:** Rebase conflicts.

## Reviews

### `POST /repos/{name}/-/review`

Submit a review.

**Body:**
```json
{"branch": "feature/x", "status": "approved", "body": "LGTM"}
```

`status` is `approved` or `rejected`.

**Response 201:** Review object including ID, reviewer, sequence, and created_at.

### `GET /repos/{name}/-/branch/{branch}/reviews`

List reviews for a branch.

**Query parameters:**
- `at` — Show only reviews at or before this sequence.

**Response 200:** Array of review objects.

## Check runs

### `POST /repos/{name}/-/check`

Report a CI check result.

**Body:**
```json
{
  "branch": "feature/x",
  "check_name": "ci/build",
  "status": "passed",
  "log_url": "https://...",
  "sequence": 43
}
```

`status` is `passed`, `failed`, or `pending`.

**Response 201:** Check run object.

### `GET /repos/{name}/-/branch/{branch}/checks`

List check runs for a branch.

**Query parameters:**
- `at` — Show only check runs at or before this sequence.

## Review comments

### `POST /repos/{name}/-/comment`

Add an inline comment on a file in a branch.

**Body:**
```json
{"branch": "feature/x", "path": "config.yaml", "body": "Needs update"}
```

### `GET /repos/{name}/-/branch/{branch}/comments`

List review comments. Optional `path` query parameter to filter by file.

### `DELETE /repos/{name}/-/comment/{id}`

Delete a review comment. Writers can delete their own comments; maintainers can delete any.

## Roles

### `GET /repos/{name}/-/roles`

List role assignments. Admin-only.

### `PUT /repos/{name}/-/roles/{identity}`

Set a role. Admin-only.

**Body:**
```json
{"role": "writer"}
```

### `DELETE /repos/{name}/-/roles/{identity}`

Remove a role assignment. Admin-only.

## Archive

### `GET /repos/{name}/-/archive`

Download the repo tree as a `.tar.gz`.

**Query parameters:**
- `branch` — Branch name (default `main`).
- `at` — Sequence number.

## Chain

### `GET /repos/{name}/-/chain`

Get commits in a sequence range for local hash-chain verification.

**Query parameters:**
- `from` — Start sequence (inclusive).
- `to` — End sequence (inclusive).

## Releases

### `POST /repos/{name}/-/releases`

Create a release. Maintainer-only.

**Body:**
```json
{"name": "v1.0.0", "sequence": 43, "notes": "First release"}
```

### `GET /repos/{name}/-/releases`

List releases in reverse chronological order.

### `GET /repos/{name}/-/releases/{name}`

Get a release.

### `DELETE /repos/{name}/-/releases/{name}`

Delete a release. Admin-only.

## Purge

### `POST /repos/{name}/-/purge`

Delete merged/abandoned branches and their commits older than a cutoff. Admin-only.

**Body:**
```json
{"older_than": "720h"}
```

## Events

### `GET /repos/{name}/-/events`

Server-Sent Events stream for a repo. Emits events on commits, merges, reviews, and check runs.

### `GET /events`

Global SSE stream. Admin-only.

## Event subscriptions (webhooks)

See [docs/events.md](events.md) for a full guide covering event types, SSE streaming, HMAC signing, and the retry/suspend policy.

### `POST /subscriptions`

Create a webhook subscription.

**Authorization:**
- `repo` field set → Reader+ on the named repo.
- `repo` field omitted → Global admin only.

**Body:**
```json
{
  "repo": "acme/platform",
  "event_types": ["com.docstore.commit.created"],
  "backend": "webhook",
  "config": {"url": "https://...", "secret": "hmac-secret"}
}
```

- `repo` — Optional. Omit to create a global subscription that receives events from all repos.
- `event_types` — Optional. Omit to receive all event types.
- `config.secret` — Optional. When set, each delivery includes an `X-DocStore-Signature: sha256=<hmac>` header.

**Response 201:** EventSubscription object.

### `GET /subscriptions`

List subscriptions. Global admins see all subscriptions; non-admin authenticated users see only their own.

**Response 200:**
```json
{"subscriptions": [...]}
```

A non-null `suspended_at` field means the subscription was automatically suspended after 10 failed deliveries and must be resumed manually.

### `DELETE /subscriptions/{id}`

Delete a subscription. Global admin or the subscription creator.

**Response 204.**

### `POST /subscriptions/{id}/resume`

Resume a suspended subscription. Global admin or the subscription creator. Clears `suspended_at` and resets `failure_count` to 0.

**Response 204.**
