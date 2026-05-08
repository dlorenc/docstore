# Repo-level secrets

Repo secrets let you store opaque key/value credentials and surface them to
CI jobs as BuildKit secret mounts. Plaintext values never appear in any read
API, log line, event payload, or webhook delivery — the only times they leave
the database are when the admin first writes them, and when a worker mounts
them into a step container at run time.

## Quick start

Set a secret, then reference it from a check.

```bash
# Set a value from stdin (the only way; --value=... is refused).
echo -n "$DOCKERHUB_PAT" | ds secrets set DOCKERHUB_TOKEN -

# List metadata. Plaintext is never returned.
ds secrets list

# Reference it from a check in .docstore/ci.yaml:
cat <<'EOF' > .docstore/ci.yaml
checks:
  push-image:
    image: cgr.dev/chainguard/crane
    steps:
      - sh -c 'crane auth login --username u --password "$(cat /run/secrets/DOCKERHUB_TOKEN)" docker.io'
      - crane push ./image.tar docker.io/me/app:latest
    secrets:
      - DOCKERHUB_TOKEN
EOF

ds commit -m "wire push-image check"
```

When the check runs, the worker resolves the allowlist against the repo's
secrets, decrypts the values in memory, and mounts each at
`/run/secrets/<NAME>` inside the step's container. Steps read the file (not
an environment variable — see [why](#why-secret-mounts-not-environment-variables)).

## Naming and size limits

| Constraint           | Rule                                                       |
|----------------------|------------------------------------------------------------|
| Name shape           | `^[A-Z][A-Z0-9_]{0,63}$` — POSIX env-var convention        |
| Reserved prefix      | `DOCSTORE_*` is reserved for built-ins (e.g. OIDC tokens) |
| Maximum value size   | 32 KiB                                                     |
| Empty values         | Refused                                                    |

Larger blobs belong in object storage with a presigned URL, not in this
table. The cap is enforced server-side; the CLI also pre-checks before any
HTTP call so a misconfigured `--from-file=` fails fast without buffering
arbitrary input.

## Renaming a secret inside a check

A check's `secrets:` entry can be either the literal repo name (the value
mounts at `/run/secrets/<NAME>`) or a single-key map that aliases the repo
name to a different local name:

```yaml
checks:
  build:
    secrets:
      - DOCKERHUB_TOKEN                       # local == repo
      - SLACK_WEBHOOK_URL: SLACK_INCOMING     # local SLACK_WEBHOOK_URL = repo SLACK_INCOMING
```

Useful when a single repo secret feeds multiple checks under different
names, or when the repo's canonical name is awkward in a step. Different
checks may reuse the same local name — each step has its own
`/run/secrets/` namespace.

## Who can see what

| Action               | Required role        |
|----------------------|----------------------|
| List metadata        | repo `reader`+       |
| Set / update         | repo `admin`         |
| Delete               | repo `admin`         |
| Read plaintext (CI)  | scheduler-service auth |

There is **no read-value endpoint**. The only way to retrieve a plaintext
value is through the worker's authenticated dispatch path, where it is
decrypted into the worker's memory, handed to BuildKit, and discarded when
the step ends.

## When CI gets secrets

The scheduler applies a fixed gating policy to every CI run before
attaching secrets:

| Trigger                                       | Secrets injected? |
|-----------------------------------------------|-------------------|
| Post-submit on an internal branch (`push`)    | yes               |
| Manual rerun by `writer`/`maintainer`/`admin` | yes               |
| Scheduled (cron)                              | yes               |
| Proposal opened by org member                 | yes               |
| Proposal opened by non-member contributor     | **no**            |
| Probe / dry-run                               | no                |

Mirrors GitHub's "secrets aren't passed to PRs from forks" rule. Encoded in
the server, not user-configurable in v1. A denied request returns 403
`secrets_blocked: <reason>` with reason strings like `non_member_proposal`,
`trigger_not_allowed`, `proposal_not_found` — useful for diagnosing why a
CI run unexpectedly lost access to a credential.

## How secrets are stored

Each row is sealed with envelope encryption:

```
DEK (32 random bytes) ─► AES-256-GCM ─► ciphertext  (in repo_secrets row)
                                       └► nonce       (in repo_secrets row)

DEK ─► KMS.Encrypt ─► encrypted_dek                   (in repo_secrets row)
                     └► kms_key_name                  (in repo_secrets row)
```

A DB-only compromise yields no plaintext: the DEKs themselves are wrapped
under a Cloud KMS key the docstore service must call out to in order to
unwrap. KEK rotation in KMS is transparent — `Decrypt` accepts old key
versions. Per-secret rotation is `ds secrets set` with a new value;
`updated_at` bumps and the previous DEK is discarded.

## Threat model

What this protects against:

1. **Database snapshot leakage.** Restored DB without access to the KMS key
   stays sealed.
2. **API surface readers.** No HTTP endpoint returns plaintext.
3. **Forks / external proposals.** Gating denies secrets to PRs from
   non-org-members before the worker ever asks.
4. **Per-secret blast radius.** Each secret has its own DEK; a single DEK
   leak does not unwrap the rest of the repo.
5. **Audit trail tamper.** Every Set / Delete / Reveal emits a structured
   event; the `secret.accessed` event records which job consumed the value.

What this does **not** protect against:

1. **A malicious step.** Anything the step process can read, the step can
   exfiltrate. The mount appears at `/run/secrets/<NAME>` and the step can
   `cat` it. If you don't trust the steps, do not give them secrets.
2. **A compromised worker.** A worker with a valid `request_token` can call
   the reveal endpoint for the secrets its job is authorised to see.
3. **Transformed leaks.** The worker-side log scrubber replaces verbatim
   value bytes in step output. It does **not** catch base64-, hex-, or
   otherwise-encoded copies. Treat it as a backstop for `echo $TOKEN`
   mistakes, not a security boundary.

## Why secret mounts, not environment variables

Steps read secrets from `/run/secrets/<NAME>`, not from environment
variables. Two reasons:

1. **Cache key isolation.** BuildKit hashes a step's environment into the
   layer cache key. Injecting a secret as an env var means rotating the
   secret busts the entire cache, even when the step's behaviour is
   unchanged. Secret mounts are excluded from the cache key by design.
2. **Process listing leaks.** A subprocess that prints `/proc/self/environ`
   or that gets traced by `ps eww` exposes env vars. File mounts have
   normal filesystem-read semantics with the step's own privileges.

## Audit events

Every state change publishes a CloudEvent on the existing event broker.
Events carry names, sizes, ids, and identities — never values.

| Event type                       | Fields                                                  |
|----------------------------------|---------------------------------------------------------|
| `com.docstore.secret.created`    | `repo, name, id, size_bytes, actor`                     |
| `com.docstore.secret.updated`    | `repo, name, id, size_bytes, actor`                     |
| `com.docstore.secret.deleted`    | `repo, name, id, actor`                                 |
| `com.docstore.secret.accessed`   | `repo, name, job_id, sequence, branch`                  |

Pipe these into a SIEM via the existing webhook subscription path. The
HMAC signing on outbound deliveries (`X-DocStore-Signature: sha256=...`)
covers the full event body — see
[Cryptographic Verifiability](cryptographic-verifiability.md#4-hmac-webhook-signing)
for the verification recipe.

## Operational notes

### Production: set `DOCSTORE_SECRETS_KMS_KEY`

In production mode (no `DEV_IDENTITY`), the server requires
`DOCSTORE_SECRETS_KMS_KEY` to point at a Cloud KMS symmetric
`ENCRYPT_DECRYPT` key. Missing → server fails loud at startup. Use a key
distinct from the citoken signing key — they serve different purposes and
should be rotated independently.

### Local dev: file-backed key

In dev mode (`DEV_IDENTITY` set), the server uses a `LocalEncryptor`
backed by a 32-byte AES key persisted at `~/.docstore/dev-encryption-key`
(file mode `0600`, parent dir `0700`). The first invocation generates the
key; subsequent invocations reuse it.

The server prints a startup banner:

```
WARN  DEV MODE — secrets sealed by local key, NOT KMS  key_path=/home/you/.docstore/dev-encryption-key
```

Do not set `DEV_IDENTITY` in production — it disables OAuth, and disables
KMS-backed encryption.

### Backup and restore

A standard Postgres dump captures `repo_secrets` with sealed bytes intact.
Restoring into an environment without access to the original KMS key
leaves the rows readable by the row schema but unable to decrypt — the
desired property. Restoring into the original environment Just Works
because KMS rotation accepts old key versions.

### Repo rename

Secrets follow the repo on rename — the rename transaction updates the
`repo` column on every row. No re-encryption needed; the sealed bytes are
not bound to the repo name.

### Hard delete is hard

`DELETE` removes the row. CI runs that already consumed the value live in
their step exec record (which docstore does not store) and are out of
scope. If you care about that residual exposure surface, rotate before
deleting.

## Reference

### CLI

```
ds secrets list
ds secrets set <NAME> -                  # value from stdin
ds secrets set <NAME> --from-file=<path>
ds secrets set <NAME> [...] [--description=<text>]
ds secrets unset <NAME>
```

`--value=<plaintext>` is **refused**. Plaintext from `argv` would land in
shell history, in `ps eww`, and in audit logs that capture command lines.

### REST

| Method   | Path                                              | Auth                | Role  |
|----------|---------------------------------------------------|---------------------|-------|
| `GET`    | `/repos/{owner}/{name}/-/secrets`                 | Google OAuth        | reader+ |
| `PUT`    | `/repos/{owner}/{name}/-/secrets/{secname}`       | Google OAuth        | admin |
| `DELETE` | `/repos/{owner}/{name}/-/secrets/{secname}`       | Google OAuth        | admin |
| `POST`   | `/repos/{owner}/{name}/-/secrets/reveal`          | CI request_token    | (gated) |

`PUT` body: `{"value": "<plaintext>", "description": "..."}`. Response
returns metadata only — id, size, timestamps, actor — never the value.

### `.docstore/ci.yaml` schema

```yaml
checks:
  <check-name>:
    image: <image>
    steps: [...]
    secrets:
      - REPO_NAME                       # local == repo
      - LOCAL_NAME: REPO_NAME           # rename
```

Validation runs at config-load time:

- Both `LOCAL_NAME` and `REPO_NAME` must match `^[A-Z][A-Z0-9_]{0,63}$`.
- Neither may use the `DOCSTORE_` reserved prefix.
- Within one check, `LOCAL_NAME` must be unique.
- Multi-key maps and non-string scalars are rejected with a concrete
  pointer at the offending entry.

The server cannot know at parse time whether a referenced repo secret
actually exists; that resolution happens at dispatch time. A name that the
repo does not have configured is reported back to the worker in the
`missing` array of the reveal response — the worker decides whether to
fail the run or proceed.

## See also

- [CI Architecture](ci-architecture.md) — how the worker, scheduler, and
  docstore fit together.
- [CI Worker Threat Model](ci-worker-threat-model.md) — the worker's
  isolation guarantees that secrets rely on.
- [Cryptographic Verifiability](cryptographic-verifiability.md) — the rest
  of docstore's integrity story.
- [Events](events.md) — webhook subscriptions and the broader event
  model.
