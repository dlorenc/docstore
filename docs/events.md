# Events

DocStore emits structured events for every significant mutation. Events use the [CloudEvents 1.0](https://cloudevents.io/) envelope format and are delivered via two mechanisms:

- **SSE streams** — real-time, in-memory, best-effort delivery over a persistent HTTP connection.
- **Webhook subscriptions** — durable delivery to an HTTPS endpoint, with automatic retries and HMAC signing.

---

## Event envelope

Every event is a CloudEvents 1.0 JSON object:

```json
{
  "specversion": "1.0",
  "type": "com.docstore.commit.created",
  "source": "/repos/acme/platform",
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "time": "2024-06-01T12:00:00Z",
  "datacontenttype": "application/json",
  "data": { ... }
}
```

| Field | Type | Description |
|---|---|---|
| `specversion` | string | Always `"1.0"`. |
| `type` | string | Event type string (see table below). |
| `source` | string | Resource path that emitted the event (e.g. `/repos/acme/platform`). |
| `id` | string | UUID, unique per event. |
| `time` | RFC3339 | UTC timestamp of when the event was emitted. |
| `datacontenttype` | string | Always `"application/json"`. |
| `data` | object | Event-specific payload (see table below). |

---

## Event types

| Event type | Trigger | Key payload fields |
|---|---|---|
| `com.docstore.repo.created` | Repo created | `repo`, `owner`, `created_by` |
| `com.docstore.repo.deleted` | Repo deleted | `repo`, `deleted_by` |
| `com.docstore.commit.created` | Commit written to a branch | `repo`, `branch`, `sequence`, `author`, `message`, `file_count` |
| `com.docstore.branch.created` | Branch created | `repo`, `branch`, `base_sequence`, `created_by` |
| `com.docstore.branch.merged` | Branch merged into `main` | `repo`, `branch`, `sequence`, `merged_by` |
| `com.docstore.branch.rebased` | Branch rebased onto `main` | `repo`, `branch`, `new_base_sequence`, `new_head_sequence`, `commits_replayed`, `rebased_by` |
| `com.docstore.branch.abandoned` | Branch marked abandoned | `repo`, `branch`, `abandoned_by` |
| `com.docstore.check.reported` | CI check result posted | `repo`, `branch`, `sequence`, `check_name`, `status`, `reporter` |
| `com.docstore.merge.blocked` | Merge attempted but blocked by policy | `repo`, `branch`, `actor`, `policies` |
| `com.docstore.proposal.opened` | Proposal opened on a branch | `repo`, `branch`, `base_branch`, `proposal_id`, `author`, `sequence` |
| `com.docstore.proposal.closed` | Proposal closed without merge | `repo`, `branch`, `proposal_id` |
| `com.docstore.proposal.merged` | Proposal merged | `repo`, `branch`, `base_branch`, `proposal_id` |
| `com.docstore.review.submitted` | Review submitted on a branch | `repo`, `branch`, `sequence`, `reviewer`, `status` |
| `com.docstore.org.created` | Org created | `org`, `created_by` |
| `com.docstore.org.deleted` | Org deleted | `org`, `deleted_by` |
| `com.docstore.role.changed` | Repo role granted or revoked | `repo`, `identity`, `role`, `changed_by` |

### Payload field reference

**Commit created** (`com.docstore.commit.created`):
```json
{
  "repo": "acme/platform",
  "branch": "feature/add-retry",
  "sequence": 43,
  "author": "alice@example.com",
  "message": "add retry logic",
  "file_count": 2
}
```

**Branch merged** (`com.docstore.branch.merged`):
```json
{
  "repo": "acme/platform",
  "branch": "feature/add-retry",
  "sequence": 44,
  "merged_by": "alice@example.com"
}
```

**Branch rebased** (`com.docstore.branch.rebased`):
```json
{
  "repo": "acme/platform",
  "branch": "feature/add-retry",
  "new_base_sequence": 44,
  "new_head_sequence": 46,
  "commits_replayed": 2,
  "rebased_by": "alice@example.com"
}
```

**Merge blocked** (`com.docstore.merge.blocked`):
```json
{
  "repo": "acme/platform",
  "branch": "feature/add-retry",
  "actor": "alice@example.com",
  "policies": ["require_review", "ci_must_pass"]
}
```

**Check reported** (`com.docstore.check.reported`):
```json
{
  "repo": "acme/platform",
  "branch": "feature/add-retry",
  "sequence": 43,
  "check_name": "ci/build",
  "status": "passed",
  "reporter": "ci-worker@system"
}
```

**Review submitted** (`com.docstore.review.submitted`):
```json
{
  "repo": "acme/platform",
  "branch": "feature/add-retry",
  "sequence": 43,
  "reviewer": "bob@example.com",
  "status": "approved"
}
```

**Role changed** (`com.docstore.role.changed`):
```json
{
  "repo": "acme/platform",
  "identity": "bob@example.com",
  "role": "writer",
  "changed_by": "alice@example.com"
}
```

---

## SSE streaming

DocStore exposes two Server-Sent Events endpoints for real-time event delivery. SSE delivery is in-memory and best-effort — disconnected clients miss events that were emitted while they were away. For durable delivery, use [webhook subscriptions](#webhook-subscriptions).

> **Multi-instance note:** SSE fan-out is in-process. If you run more than one server instance (`--max-instances > 1`), a client is only guaranteed to receive events from the instance it is connected to. For reliable delivery across instances, use webhooks.

### Repo-scoped stream

```
GET /repos/{name}/-/events
```

**Authorization:** Reader+ on the repo.

**Query parameters:**

| Parameter | Description |
|---|---|
| `types` | Comma-separated list of event types to receive. Omit to receive all types. |

The server sends one SSE message per event:

```
data: {"specversion":"1.0","type":"com.docstore.commit.created",...}

data: {"specversion":"1.0","type":"com.docstore.branch.merged",...}
```

A keepalive comment (`: keepalive`) is sent every 15 seconds to prevent proxies from closing the connection.

**Example — stream all events on a repo:**

```bash
curl -N -H "Authorization: Bearer $TOKEN" \
  https://docstore.example.com/repos/acme/platform/-/events
```

**Example — stream only commit and merge events:**

```bash
curl -N -H "Authorization: Bearer $TOKEN" \
  "https://docstore.example.com/repos/acme/platform/-/events?types=com.docstore.commit.created,com.docstore.branch.merged"
```

### Global stream

```
GET /events
```

**Authorization:** Global admin only.

**Query parameters:**

| Parameter | Description |
|---|---|
| `repo` | Exact repo name to filter (e.g. `acme/platform`). Omit to receive events from all repos. |

**Example — stream all events across all repos:**

```bash
curl -N -H "Authorization: Bearer $ADMIN_TOKEN" \
  https://docstore.example.com/events
```

**Example — filter to a single repo:**

```bash
curl -N -H "Authorization: Bearer $ADMIN_TOKEN" \
  "https://docstore.example.com/events?repo=acme/platform"
```

---

## Webhook subscriptions

Webhook subscriptions provide durable, retried delivery to an HTTPS endpoint. The server writes each matching event to a transactional outbox and delivers it asynchronously.

### Authorization

| Operation | Required role |
|---|---|
| Create subscription (repo-scoped, `repo` field set) | Reader+ on the target repo |
| Create subscription (global, `repo` field omitted) | Global admin |
| List subscriptions | Global admin |
| Delete subscription | Global admin |
| Resume a suspended subscription | Global admin |

### Event filtering

A subscription receives events based on two optional filters. Both must match for an event to be delivered:

| Filter field | Behavior when omitted | Behavior when set |
|---|---|---|
| `repo` | Receives events from **all repos** | Receives events only from the named repo |
| `event_types` | Receives **all event types** | Receives only the listed event types |

### HMAC-SHA256 request signing

When a subscription is created with a non-empty `secret` in its config, every webhook delivery includes an `X-DocStore-Signature` header:

```
X-DocStore-Signature: sha256=<hex-encoded-hmac-sha256>
```

The signature is computed over the raw request body:

```
HMAC-SHA256(key=secret, message=body)
```

**Verification example (Go):**

```go
import (
    "crypto/hmac"
    "crypto/sha256"
    "encoding/hex"
)

func verify(body []byte, secret, header string) bool {
    mac := hmac.New(sha256.New, []byte(secret))
    mac.Write(body)
    expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
    return hmac.Equal([]byte(header), []byte(expected))
}
```

**Verification example (Python):**

```python
import hmac, hashlib

def verify(body: bytes, secret: str, header: str) -> bool:
    sig = hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
    expected = f"sha256={sig}"
    return hmac.compare_digest(header, expected)
```

Always use a constant-time comparison (`hmac.Equal` / `hmac.compare_digest`) to prevent timing attacks.

### Retry and suspend policy

| Behavior | Detail |
|---|---|
| HTTP timeout | 10 seconds per delivery attempt |
| Success condition | HTTP response status 200–299 |
| Retry backoff | Exponential: 1 s, 2 s, 4 s, 8 s, …, capped at 1 hour |
| Max attempts | 10 per outbox row |
| Auto-suspend | After 10 failed attempts the subscription is suspended and no further events are sent |
| Resume | `POST /subscriptions/{id}/resume` clears the suspension and resets the failure counter |
| Outbox retention | Delivered rows are deleted after 7 days |

### Managing subscriptions

#### Create a subscription

```
POST /subscriptions
Content-Type: application/json
```

**Body:**

```json
{
  "repo": "acme/platform",
  "event_types": ["com.docstore.commit.created", "com.docstore.branch.merged"],
  "backend": "webhook",
  "config": {
    "url": "https://hooks.example.com/docstore",
    "secret": "my-hmac-secret"
  }
}
```

- `repo` — Optional. Omit to create a global subscription that receives events from all repos.
- `event_types` — Optional. Omit to receive all event types.
- `secret` — Optional. When set, deliveries are signed with `X-DocStore-Signature`.

**Response 201:**

```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "repo": "acme/platform",
  "event_types": ["com.docstore.commit.created", "com.docstore.branch.merged"],
  "backend": "webhook",
  "config": {"url": "https://hooks.example.com/docstore", "secret": "my-hmac-secret"},
  "created_at": "2024-06-01T12:00:00Z",
  "created_by": "alice@example.com",
  "suspended_at": null,
  "failure_count": 0
}
```

#### List subscriptions

```
GET /subscriptions
```

**Response 200:**

```json
{
  "subscriptions": [
    {
      "id": "550e8400-e29b-41d4-a716-446655440000",
      "repo": "acme/platform",
      "event_types": ["com.docstore.commit.created"],
      "backend": "webhook",
      "config": {"url": "https://hooks.example.com/docstore"},
      "created_at": "2024-06-01T12:00:00Z",
      "created_by": "alice@example.com",
      "suspended_at": null,
      "failure_count": 0
    }
  ]
}
```

A non-null `suspended_at` means the subscription was automatically suspended after 10 failed deliveries. Resume it before new events will be sent.

#### Delete a subscription

```
DELETE /subscriptions/{id}
```

**Response 204.**

#### Resume a suspended subscription

```
POST /subscriptions/{id}/resume
```

Clears `suspended_at` and resets `failure_count` to 0. The subscription immediately re-enters the active delivery queue.

**Response 204.**

### curl examples

**Create a global subscription for all commit events:**

```bash
curl -X POST https://docstore.example.com/subscriptions \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "backend": "webhook",
    "event_types": ["com.docstore.commit.created"],
    "config": {
      "url": "https://hooks.example.com/docstore",
      "secret": "my-hmac-secret"
    }
  }'
```

**Create a repo-scoped subscription for all event types:**

```bash
curl -X POST https://docstore.example.com/subscriptions \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "repo": "acme/platform",
    "backend": "webhook",
    "config": {
      "url": "https://hooks.example.com/acme-platform"
    }
  }'
```

**List all subscriptions:**

```bash
curl -H "Authorization: Bearer $ADMIN_TOKEN" \
  https://docstore.example.com/subscriptions
```

**Delete a subscription:**

```bash
curl -X DELETE -H "Authorization: Bearer $ADMIN_TOKEN" \
  https://docstore.example.com/subscriptions/550e8400-e29b-41d4-a716-446655440000
```

**Resume a suspended subscription:**

```bash
curl -X POST -H "Authorization: Bearer $ADMIN_TOKEN" \
  https://docstore.example.com/subscriptions/550e8400-e29b-41d4-a716-446655440000/resume
```

### ds CLI examples

Subscription management is also available via the `ds` CLI:

**Create a subscription:**

```bash
# Repo-scoped, specific event types, with HMAC secret
ds subscriptions create \
  --repo acme/platform \
  --events com.docstore.commit.created,com.docstore.branch.merged \
  --url https://hooks.example.com/docstore \
  --secret my-hmac-secret

# Global subscription, all event types
ds subscriptions create \
  --url https://hooks.example.com/global
```

**List subscriptions:**

```bash
ds subscriptions list
```

**Delete a subscription:**

```bash
ds subscriptions delete <id>
```

**Resume a suspended subscription:**

```bash
ds subscriptions resume <id>
```
