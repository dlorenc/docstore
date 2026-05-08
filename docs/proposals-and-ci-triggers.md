# Proposals and CI Triggers

This document covers the proposals feature and the CI trigger model introduced in issue #179.

---

## Proposals

A **proposal** is a named intent to merge a branch into a base branch. Proposals are lightweight
pre-merge records — they carry a title, an optional description, and track the lifecycle of the
branch from creation through merge or close.

### Why proposals?

Branches already hold the diff, reviews, and check runs. A proposal adds:

- A human-readable title and description surfaced to reviewers
- A stable identity that OPA policies can gate on (require an open proposal before merging)
- Lifecycle events that trigger CI runs on proposal open and commit push

### Data model

| Field | Type | Notes |
|---|---|---|
| `id` | UUID | Unique proposal identifier |
| `repo` | string | Full repo name (e.g. `acme/myrepo`) |
| `branch` | string | Source branch being proposed |
| `base_branch` | string | Target branch (usually `main`) |
| `title` | string | Human-readable title |
| `description` | string | Optional extended description |
| `author` | string | Identity that opened the proposal |
| `state` | enum | `open`, `closed`, or `merged` |
| `created_at` | timestamp | |
| `updated_at` | timestamp | |

### Lifecycle

```
                   ds proposal open
                         │
                         ▼
                       open
                     /       \
     ds merge        │         │  ds proposal close
  (branch merges)    ▼         ▼
                  merged     closed
```

- **open → merged**: automatically when `POST /merge` succeeds for the proposal's branch
- **open → closed**: manually via `ds proposal close <id>` or `POST /proposals/:id/close`

A branch can have at most one open proposal at a time. Attempting to open a second proposal while
one is already open returns `409 Conflict`.

### CLI usage

#### Open a proposal

```
ds proposal open --title "Add retry logic" [--branch <name>] [--base main] [--description "..."]
```

- `--title` (required): short title displayed in reviews and CI
- `--branch`: branch to propose; defaults to the current workspace branch
- `--base`: target base branch; defaults to `main`
- `--description`: optional extended description

Example:

```bash
ds proposal open --title "Fix login timeout" --branch feature/login-fix --base main
# Proposal opened: a3f7c2d8-1234-...
```

#### List proposals

```
ds proposal list [--state open|closed|merged]
```

Default shows all proposals. Filter by state with `--state`.

```bash
ds proposal list --state open
```

Output example:

```
ID                                    BRANCH              STATE   TITLE
a3f7c2d8-1234-5678-abcd-000000000001  feature/login-fix   open    Fix login timeout
```

#### Close a proposal

```
ds proposal close <proposal-id>
```

Closes an open proposal without merging the branch. Only the proposal author or a maintainer may
close a proposal.

```bash
ds proposal close a3f7c2d8-1234-5678-abcd-000000000001
# Proposal a3f7c2d8-... closed
```

### How proposals relate to reviews

Reviews (`ds review`) are attached to a branch at a specific head sequence, not to a proposal.
When a proposal exists, the TUI and `GET /-/branch/:name/status` surface the proposal metadata
alongside the branch's reviews and check runs. The proposal provides a stable title for the review
thread; the reviews themselves are stored and queried against the branch.

### Requiring an open proposal via OPA policy

Policies receive an `input.proposal` object (or `null` if no open proposal exists for the branch).
Fields: `id`, `branch`, `base_branch`, `title`, `state`.

To block merges unless an open proposal exists:

```rego
package docstore.require_proposal

import rego.v1

default allow := false

allow if { input.proposal != null }

reason := "an open proposal is required before merging"
```

Store this file at `.docstore/policy/require_proposal.rego` on `main` (via the normal
branch → review → merge workflow) to enforce it.

---

## CI Trigger Model (`.docstore/ci.yaml`)

The CI configuration file lives at `.docstore/ci.yaml` in the repository. It is read from the
branch under test (pinned to the head sequence at the time the job was queued), so changes to
`ci.yaml` on a branch take effect for that branch's CI runs without requiring a merge first.

### Complete annotated example

```yaml
on:
  push:
    branches: [main]            # trigger on pushes to main (deploy-style)
  proposal:
    base_branches: [main]       # trigger when a proposal targeting main is opened
  schedule:
    - cron: '0 2 * * *'         # nightly at 2 AM UTC
  # manual: trigger via POST /repos/:name/-/ci/run on docstore (requires writer role) — no config needed, always enabled

checks:
  - name: ci/test
    image: golang:1.24
    steps:
      - go test ./...
      - go vet ./...

  - name: ci/deploy
    image: google/cloud-sdk:slim
    needs: [ci/test]
    if: "event.type == 'push' && event.branch == 'main'"
    steps:
      - ./deploy.sh
```

### `on:` — trigger types

All four trigger types can coexist in a single `on:` block. Triggers are independent — a single
commit can fire both a `push` job (if the branch matches `branches`) and a
`proposal_synchronized` job (if the branch has an open proposal and `proposal:` is configured).

#### `push`

Fires on every commit pushed to a matching branch.

```yaml
on:
  push:
    branches: [main]        # only main
    # branches: [main, release/**]  # glob patterns supported
    # (omit branches: to match all branches)
```

- `branches`: list of glob patterns (uses `**`, `*`, `?`, character classes). Omitting `branches`
  or leaving it empty matches all branches.

#### `proposal`

Fires when a proposal is **opened** (`ds proposal open`) targeting a matching base branch.

```yaml
on:
  proposal:
    base_branches: [main]   # only proposals targeting main
```

- `base_branches`: list of glob patterns for the proposal's target (base) branch. Omit to match
  all base branches.

When a branch with an open proposal receives a new commit, a separate `proposal_synchronized`
event is also fired (see event types below).

#### `schedule`

Fires at a cron schedule. Runs are triggered against `main` at its current head sequence.

```yaml
on:
  schedule:
    - cron: '0 2 * * *'     # nightly at 2 AM UTC (standard 5-field cron)
    - cron: '0 12 * * 1'    # every Monday at noon UTC
```

#### `manual`

Always enabled. Trigger a run via the docstore server (requires writer role):

```bash
curl -X POST https://docstore.dev/repos/acme/myrepo/-/ci/run \
  -H "Content-Type: application/json" \
  -H "Proxy-Authorization: Bearer $(gcloud auth print-identity-token)" \
  -d '{"branch": "feature/x"}'
# Returns: {"run_id": "..."}
```

No configuration needed in `ci.yaml`.

### `checks:` — job definitions

Each entry in `checks` is an independent job that runs inside a container image. All checks in a
config run concurrently unless limited by `needs` dependencies.

| Field | Required | Notes |
|---|---|---|
| `name` | yes | Check run name posted to docstore (e.g. `ci/build`); conventionally namespaced with `/` |
| `image` | yes | Any pullable Docker image |
| `steps` | yes | Ordered shell commands run sequentially inside the image with source mounted at `/src` |
| `if` | no | Conditional expression; check is skipped (not failed) when false. Empty = always run. |
| `needs` | no | List of check names that must complete before this one starts (parsed; see below) |

Steps within a single check share the `/src` filesystem — files written by an earlier step are
visible to later steps.

### `if:` expressions

Per-check `if:` expressions filter which checks run for a given trigger event. A check with no
`if:` key always runs.

#### Supported fields

| Field | Description | Example value |
|---|---|---|
| `event.type` | What triggered the run | `push`, `proposal`, `proposal_synchronized`, `manual`, `schedule` |
| `event.branch` | Branch being tested | `main`, `feature/login-fix` |
| `event.base_branch` | Proposal target branch (proposals only; empty otherwise) | `main` |
| `event.proposal_id` | Proposal UUID (proposals only; empty otherwise) | `a3f7c2d8-...` |

#### Supported operators

| Operator | Description |
|---|---|
| `==` | Equality |
| `!=` | Inequality |
| `&&` | Logical AND |
| `\|\|` | Logical OR |
| `(...)` | Grouping |

String literals use single or double quotes: `"main"` or `'main'`.

#### Examples

```yaml
# Only run on direct pushes to main
if: "event.type == 'push' && event.branch == 'main'"

# Only run on proposal events (opened or synchronized)
if: "event.type == 'proposal' || event.type == 'proposal_synchronized'"

# Only run on proposals targeting main
if: "event.type == 'proposal' && event.base_branch == 'main'"

# Scheduled or manual only
if: "event.type == 'schedule' || event.type == 'manual'"
```

A check whose `if:` expression is false is **skipped** (not posted to docstore as a check run).
This means a skipped check does not satisfy a merge policy that requires it — design your `if:`
conditions so required checks always run for the events that trigger merges.

#### `needs:` (parsed, not yet enforced)

The `needs:` field is parsed and stored but dependency ordering is not yet enforced — all checks
run concurrently regardless of `needs`. This field is reserved for a future release.

---

## Event integration

### CloudEvents emitted by docstore

The docstore server emits CloudEvents through its outbox for all proposal lifecycle transitions. ci-scheduler subscribes to these and maps them to CI trigger types.

| Event type | When emitted | Payload fields |
|---|---|---|
| `com.docstore.commit.created` | Every commit on any branch | `repo`, `branch`, `sequence` |
| `com.docstore.proposal.opened` | `ds proposal open` or `POST /proposals` | `repo`, `branch`, `base_branch`, `proposal_id`, `author`, `sequence` |
| `com.docstore.proposal.closed` | `ds proposal close` or `POST /proposals/:id/close` | `repo`, `branch`, `proposal_id` |
| `com.docstore.proposal.merged` | Branch merges while a proposal is open | `repo`, `branch`, `base_branch`, `proposal_id` |

`proposal.closed` and `proposal.merged` are emitted for downstream consumers (webhooks, external integrations) but do not currently trigger CI runs.

### How `proposal_synchronized` works

`proposal_synchronized` is not a docstore event — it is a synthetic trigger type generated by ci-scheduler. When ci-scheduler receives a `commit.created` event, it:

1. Checks whether the pushed branch has an open proposal via `GET /repos/:name/-/proposals?state=open&branch=<branch>`.
2. If an open proposal exists and the `on: proposal:` block matches, enqueues a second job with `trigger_type=proposal_synchronized` in addition to any `push` job.

This means a single commit to a proposed branch can enqueue two independent CI jobs — one for post-submit testing and one for pre-submit gating — with different `if:` conditions selecting which checks run in each.

### Subscription auto-registration

When ci-scheduler starts with `RUNNER_URL` set, it registers a webhook subscription with docstore at startup:

```
POST /repos/*/subscriptions
{
  "backend": "webhook",
  "event_types": ["com.docstore.commit.created", "com.docstore.proposal.opened"],
  "config": {"url": "<RUNNER_URL>/webhook", "secret": "<WEBHOOK_SECRET>"}
}
```

The `*` wildcard means the subscription receives events from all repos on the server. Deliveries are signed with HMAC-SHA256 using `WEBHOOK_SECRET`; the scheduler rejects unsigned or incorrectly signed deliveries.

If `RUNNER_URL` is not set (e.g. local development), auto-registration is skipped and events must be delivered manually or via an existing subscription.

---

## Backward compatibility

Repos whose `.docstore/ci.yaml` has **no `on:` block** get the previous behavior: CI runs on every
commit to every branch (equivalent to `push:` with no `branches:` filter, matching all branches).

If `.docstore/ci.yaml` does not exist on the branch under test, CI is skipped entirely and no
check runs are posted.

---

## API reference

### Proposal endpoints

All proposal endpoints are repo-scoped under `/repos/{name}/-/`.

| Method | Path | Description |
|---|---|---|
| `POST` | `/proposals` | Open a proposal. Body: `{branch, base_branch, title, description?}`. Returns `{id}`. |
| `GET` | `/proposals` | List proposals. Query: `?state=open\|closed\|merged&branch=<name>`. |
| `GET` | `/proposals/:id` | Get a single proposal by ID. |
| `PATCH` | `/proposals/:id` | Update title or description. Body: `{title?, description?}`. Author or maintainer only. |
| `POST` | `/proposals/:id/close` | Close an open proposal. Author or maintainer only. |

The merge endpoint (`POST /merge`) automatically transitions the proposal to `merged` state when
the branch merge succeeds.

### OPA policy input

The `input.proposal` field is populated when evaluating merge policies for a branch that has an
open proposal:

```json
{
  "proposal": {
    "id": "a3f7c2d8-1234-5678-abcd-000000000001",
    "branch": "feature/login-fix",
    "base_branch": "main",
    "title": "Fix login timeout",
    "state": "open"
  }
}
```

`input.proposal` is `null` when no open proposal exists for the branch being merged.
