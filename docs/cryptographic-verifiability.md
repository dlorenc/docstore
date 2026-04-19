# Cryptographic Verifiability in DocStore

DocStore combines three cryptographic mechanisms to give a strong end-to-end integrity story:

1. **Content addressing** — every file version is identified by its SHA-256 hash, enabling deduplication and tamper detection at the byte level.
2. **Hash chaining** — every commit on a branch is linked to its predecessor through a SHA-256 hash that covers all commit metadata and file content hashes, forming an append-only log.
3. **HMAC webhook signing** — outbound event notifications are signed with HMAC-SHA256 so receivers can verify they originated from a trusted DocStore server.

Together these mean: if any file byte, commit field, or event payload is tampered with after the fact, the tampering is detectable.

---

## 1. Content Addressing

### How it works

Every document version stored in DocStore is identified by the SHA-256 hash of its raw bytes:

```
content_hash = hex(SHA256(file_bytes))
```

This is computed on both sides:

- **Server** (`internal/db/store.go`): when a commit lands, each file's bytes are hashed before storage.
- **Client** (`internal/cli/app.go`): before pushing, the client hashes each local file to detect changes and to send the content hash as part of the commit payload.

### Deduplication

Because the key is the content hash rather than a filename or timestamp, two commits that reference identical file content share the same underlying `version_id` and storage row. No additional bytes are written. This is transparent to callers — the document API always presents a path-addressed view — but it means storage is automatically deduplicated across the entire repository.

When an external blob store is configured (`blobStore` + `blobThreshold`), large files are stored in GCS (or another backend) keyed by their content hash. The database row stores a `blob_key` equal to the content hash; the blob store is the source of truth for the bytes.

### File deletion

Deleted files are represented as a file entry with a `nil` `version_id` (and therefore an empty content hash string `""`). This distinguishes "file was removed at this commit" from "file has not changed".

### Diagram

```
write(path, bytes)
      │
      ▼
 SHA256(bytes) ──► content_hash ──► stored in documents.content_hash
      │                                   │
      │                                   ▼
      └──────────────────────────► used as input to commit hash chain
```

---

## 2. Hash Chaining

### Chain structure

Each commit on a branch contributes one entry to that branch's hash chain. The chain is a strict sequence: `commit_hash[N]` is a deterministic function of the previous commit's hash and all observable fields of commit N. A verifier who knows `commit_hash[N-1]` can independently reproduce `commit_hash[N]` from publicly available chain data and detect any retroactive edit.

### Hash input specification

The server computes each commit's hash in `computeCommitHash` (`internal/db/store.go:35`):

```
hash_input =
    prev_hash + "\n"
    seq       + "\n"
    repo      + "\n"
    branch    + "\n"
    author    + "\n"
    message   + "\n"
    created_at_utc_rfc3339nano + "\n"
    for each file, sorted by path:
        path + ":" + content_hash + "\n"

commit_hash = hex(SHA256(hash_input))
```

Field notes:

| Field | Type | Notes |
|-------|------|-------|
| `prev_hash` | hex string | Hash of the previous commit on this branch, or the genesis hash `000...0` (64 zeros) for the first commit |
| `seq` | decimal string | Global monotonic sequence number (not per-branch) |
| `repo` | string | Repository name |
| `branch` | string | Branch name |
| `author` | string | Authenticated identity of the commit author |
| `message` | string | Commit message |
| `created_at` | RFC3339Nano, UTC | Server-assigned creation time |
| files | sorted by path | Each entry is `path:content_hash`; deleted files have empty content hash |

Files are **sorted by path** before hashing so the result is canonical regardless of the order in which the server processes them internally.

### Per-branch chains

Each branch maintains its own independent chain. When computing `prev_hash` for a new commit on branch `feature/x`, the server queries for the most recent commit **on that branch** with `sequence < current`. Commits on `main` or other branches are invisible to this lookup.

```
main:      [seq=1, h=A] → [seq=3, h=C] → [seq=5, h=E]
feature/x:              → [seq=2, h=B] → [seq=4, h=D]
```

`seq=4` on `feature/x` has `prev_hash = B` (from seq=2 on feature/x), not `C` (from seq=3 on main).

### Pre-feature commits

Commits created before hash chaining was introduced have `commit_hash = NULL` in the database. `ds verify` skips these and resets `prev_hash` to the genesis hash, so verification continues cleanly from the next hashed commit.

### Genesis hash

```
genesisHash = "0000000000000000000000000000000000000000000000000000000000000000"
```

This sentinel is used as `prev_hash` for the very first hashed commit on a branch (or after a chain reset from a NULL commit). It is not itself a hash of any real data.

---

## 3. The `ds verify` Command

`ds verify` is the client-side tool for auditing chain integrity. It fetches the server's current chain and walks it locally, recomputing each hash.

### Algorithm

```
1. Fetch the server's current head sequence for the current branch.
2. Fetch all chain entries from seq=1 to head (GET /repos/{repo}/-/chain?from=1&to=head).
3. Filter entries to the current branch.
4. prevHash = genesisHash; foundFirst = false
5. For each entry in sequence order:
   a. If commit_hash is NULL → print SKIP; reset prevHash = genesisHash; continue
   b. If foundFirst is false → accept this entry's hash as the anchor (TOFU);
      set prevHash = entry.commit_hash; print OK; foundFirst = true; continue
   c. Recompute: expected = computeCommitHash(prevHash, entry.*)
   d. If expected == entry.commit_hash → prevHash = entry.commit_hash; print OK
   e. Else → print FAIL with expected vs actual; record error; continue
6. Return error if any FAIL was encountered.
```

**Trust-on-first-use (TOFU)**: The very first hashed commit is accepted without verification — there is nothing earlier to chain from. Every subsequent commit is verified against the recomputed hash.

### Example output

```
seq 1     OK    a1b2c3d4e5f60001  Initial commit
seq 2     OK    f7e6d5c4b3a20002  Add README
seq 3     SKIP  (no commit_hash)  (pre-feature commit)
seq 4     OK    9988776655440003  Post-migration commit
seq 5     OK    1122334455660004  Update config
seq 6     FAIL  (expected 7766554433220005 got aabbccddeeff0006)  Tampered commit
```

A `FAIL` line means the stored hash does not match the recomputed hash given the previous link — either the commit's metadata was altered, its files were altered, or the `commit_hash` field itself was modified.

### Integration with `ds pull`

`ds pull` also verifies the chain incrementally as it downloads new commits. It stores the last known `commit_hash` in `.docstore/state.json` as a local anchor. On the next pull it verifies that the new entries chain correctly from the stored anchor. Pass `--skip-verify` to disable this check.

---

## 4. HMAC Webhook Signing

### Overview

When a DocStore event (commit, branch creation, etc.) is delivered to a webhook endpoint, the server signs the request body with HMAC-SHA256 using the subscriber's secret. The receiver can verify the signature independently, confirming both authenticity (the request came from this DocStore server) and integrity (the payload was not modified in transit).

### Signature computation

```go
// internal/events/outbox.go
func computeHMAC(body []byte, secret string) string {
    mac := hmac.New(sha256.New, []byte(secret))
    mac.Write(body)
    return hex.EncodeToString(mac.Sum(nil))
}
```

The signature is sent as an HTTP header:

```
X-DocStore-Signature: sha256=<hex-encoded-hmac>
```

### Verification (receiver side)

To verify an incoming webhook:

```python
import hmac, hashlib

def verify_webhook(body: bytes, secret: str, header: str) -> bool:
    expected = "sha256=" + hmac.new(
        secret.encode(), body, hashlib.sha256
    ).hexdigest()
    return hmac.compare_digest(expected, header)
```

Use a constant-time comparison (`hmac.compare_digest`) to prevent timing attacks.

### What is signed

The body is the complete CloudEvents JSON envelope exactly as transmitted — headers, envelope fields, and data payload together. The signature covers the full wire representation, so any modification to the event data (including the embedded commit hash) invalidates the signature.

### Subscription configuration

When creating a webhook subscription, the caller provides a `secret` string in the config JSON. The server stores this alongside the subscription and uses it for every delivery on that subscription. Rotating the secret requires updating the subscription.

### Delivery reliability

Webhooks use the transactional outbox pattern:

- Events are written to `event_outbox` in the same database transaction as the commit.
- A background dispatcher polls every 5 seconds and delivers pending rows.
- Failed deliveries are retried with exponential backoff (1s → 2s → 4s → … up to 1 hour).
- After 10 failed attempts the subscription is suspended.
- Delivered rows are retained for 7 days then deleted.

---

## 5. End-to-End Composition

The three mechanisms form a layered trust model:

```
                        ┌──────────────────────────────┐
                        │         Webhook receiver      │
                        │                               │
                        │  1. Verify HMAC signature     │
                        │     on X-DocStore-Signature   │
                        │                               │
                        │  2. Extract commit_hash from  │
                        │     event payload             │
                        │                               │
                        │  3. (optional) ds verify to   │
                        │     confirm chain integrity   │
                        └──────────────────────────────┘
                                      ▲
                                      │ signed HTTP POST
                                      │
                        ┌──────────────────────────────┐
                        │         DocStore server       │
                        │                               │
                        │  commit arrives               │
                        │    │                          │
                        │    ▼                          │
                        │  SHA256 each file ──► content_hash
                        │    │                          │
                        │    ▼                          │
                        │  computeCommitHash(           │
                        │    prev_hash,                 │
                        │    seq, repo, branch,         │
                        │    author, message,           │
                        │    created_at,                │
                        │    sorted file hashes)        │
                        │    │                          │
                        │    ▼                          │
                        │  store commit_hash ──────────►│
                        │    │                          │
                        │    ▼                          │
                        │  emit event (signed)          │
                        └──────────────────────────────┘
```

### What each layer protects

| Threat | Mechanism | How detected |
|--------|-----------|--------------|
| File bytes tampered in storage | Content addressing | `content_hash` no longer matches bytes |
| Commit metadata edited (author, message, timestamp) | Hash chain | `commit_hash` no longer matches recomputed value |
| Commit silently inserted or removed | Hash chain | Subsequent `prev_hash` values break |
| Webhook payload forged or replayed | HMAC signing | Signature mismatch on receiver |
| Webhook payload modified in transit | HMAC signing | Signature mismatch on receiver |

### Trust boundaries

- The **hash chain** is only as trustworthy as the server that computed it. A compromised server could rewrite the entire chain consistently. `ds verify` detects post-hoc tampering with stored data, but not a malicious server rewriting history at write time.
- **HMAC signing** protects the channel from server to receiver. It does not authenticate the original commit author beyond the `author` field in the commit (which the server records from the authenticated session).
- **Content addressing** provides strong byte-level integrity for stored file versions. Combined with the chain, a verifier who has independently cached any `commit_hash` value can detect any retroactive edits to any commit in the chain from that point forward.

### Verification checklist

For a full audit of a repository branch:

1. Run `ds verify` — confirms every commit hash chains correctly from genesis to head.
2. For any specific commit, fetch its chain entry and independently compute `computeCommitHash` to reproduce the stored hash.
3. For each file in a commit, download the version and confirm `hex(SHA256(bytes)) == content_hash`.
4. On the webhook receiver, verify `X-DocStore-Signature` on every delivery using `hmac.compare_digest`.
