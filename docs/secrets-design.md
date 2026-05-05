# Repo-level secrets — design

Status: draft, awaiting implementation.
Owner: TBD.

## Goal

Allow repo admins to store opaque key/value secrets that CI jobs read at
runtime through BuildKit secret mounts (`/run/secrets/<NAME>`). Plaintext
must never return from any read API, never appear in logs / events /
webhooks, and never be passed to CI runs from untrusted contexts (proposals
opened by non-members).

## Non-goals (v1)

- Org-level secrets shared across an org's repos.
- Per-environment / per-stage secrets (prod vs. staging within one repo).
- Per-branch secrets.
- Bring-your-own-key (BYOK).
- Approval workflows for secret access.
- Automatic rotation hooks.

All extensions, not blockers.

## Threat model

1. DB-only compromise must NOT yield plaintext (envelope encryption with KMS).
2. Plaintext exits the system **only** as the request body of
   `PUT /-/secrets/{name}` from an authenticated admin, and as a BuildKit
   secret mount inside a step's container at run time.
3. CI jobs from external-fork proposals get **no** secrets.
4. Per-secret data encryption key (DEK) so a single DEK leak doesn't
   compromise the rest.
5. Secrets are **not** part of the per-branch commit hash chain in
   `internal/db/store.go:35`. Editing a secret does not produce a commit.

## Data model

```sql
CREATE TABLE repo_secrets (
    id            TEXT PRIMARY KEY,                  -- ULID
    repo          TEXT NOT NULL,                     -- "org/name"
    name          TEXT NOT NULL,                     -- e.g. DOCKERHUB_TOKEN
    description   TEXT NOT NULL DEFAULT '',
    ciphertext    BYTEA NOT NULL,                    -- AES-256-GCM(plaintext, DEK)
    nonce         BYTEA NOT NULL,                    -- 12-byte GCM nonce
    encrypted_dek BYTEA NOT NULL,                    -- KMS.Encrypt(DEK)
    kms_key_name  TEXT NOT NULL,                     -- KMS resource path used
    size_bytes    INT NOT NULL,                      -- length of plaintext
    created_by    TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL,
    updated_by    TEXT,
    updated_at    TIMESTAMPTZ,
    last_used_at  TIMESTAMPTZ,
    UNIQUE(repo, name)
);
CREATE INDEX repo_secrets_by_repo ON repo_secrets(repo);
```

### Envelope encryption

Standard pattern:

- DEK = 32 random bytes per secret, generated server-side.
- `ciphertext` = AES-256-GCM(plaintext, DEK, nonce).
- `encrypted_dek` = KMS.Encrypt(DEK), using a single repo-secrets KMS key
  (separate from the JWT signing key in `internal/citoken/citoken.go`).
- One KMS API call per write/read.
- KEK rotation is transparent — KMS Decrypt accepts old key versions.

### Constraints

- Name pattern: `[A-Z][A-Z0-9_]{0,63}` (POSIX env-var shape).
- Reserved prefix `DOCSTORE_*` is forbidden at write time — used by built-in
  secrets like `docstore_oidc_request_token`.
- Value size cap: 32 KiB. Larger blobs belong in object storage with a
  presigned URL, not in this table.

## API surface

```
GET    /repos/{owner}/{name}/-/secrets             list metadata only
PUT    /repos/{owner}/{name}/-/secrets/{secname}   create or update
DELETE /repos/{owner}/{name}/-/secrets/{secname}   delete
```

`PUT` body: `{ "value": "<plaintext>", "description": "..." }`. Response
echoes metadata only.

`GET` response — names, sizes, timestamps, who created/updated, `last_used_at`.
Never the value. There is no read-value endpoint at all; CI access goes
through the scheduler service path described below.

## RBAC

| Action              | Required role           |
|---------------------|-------------------------|
| List metadata       | repo `reader`+          |
| Create / update     | repo `admin`            |
| Delete              | repo `admin`            |
| Read plaintext (CI) | scheduler-service auth  |

Uses the existing `RoleType` enum (`api/types.go:38–45`); no new role.

## CLI

```
ds secrets list
ds secrets set DOCKERHUB_TOKEN -            # reads stdin
ds secrets set DOCKERHUB_TOKEN --from-file=tok.txt
ds secrets set DOCKERHUB_TOKEN --description "..."
ds secrets unset DOCKERHUB_TOKEN
```

No `--value=` flag — refuse to take plaintext from argv to keep it out of
shell history and `ps` listings.

## CI integration

### `.docstore/ci.yaml` extension

```yaml
checks:
  build:
    image: cgr.dev/chainguard/go
    commands: ["./scripts/release.sh"]
    secrets:
      - DOCKERHUB_TOKEN
      - SLACK_WEBHOOK_URL: "{{ secrets.SLACK_INCOMING }}"   # optional rename
```

`secrets:` is a per-step **allowlist**. A step sees only what it asks for.
Parser in `internal/ciconfig/` rejects unknown names at config-load time so
the user gets the error before scheduling.

### Gating policy

| Trigger                                       | Secrets injected? |
|-----------------------------------------------|-------------------|
| Post-submit on internal branch                | yes               |
| Proposal opened by org member                 | yes               |
| Proposal opened by non-member contributor     | no                |
| Manual rerun by `writer`/`maintainer`/`admin` | yes               |
| Probe / dry-run                               | no                |

Encoded in the scheduler. Not user-configurable in v1. Mirrors GitHub's
PR-from-fork rule.

### Plaintext lifetime

```
KMS.Decrypt → scheduler RAM → authenticated RPC to worker
            → BuildKit secretsprovider.FromMap
            → /run/secrets/<NAME> mount inside step container
            → discarded at step end
```

Never written to disk on the docstore side. The existing executor already
has the right hook (`internal/executor/executor.go:226–334`) — extend
`secretMap` and `llb.AddSecret` with user-defined names alongside
`docstore_oidc_request_token`.

### Log scrubbing

Worker-side wrapper around step output: Aho–Corasick over the small set of
injected secret values, replacing matches with `***`. Backstop for
`echo $TOKEN` mistakes — not a security boundary.

## Events / audit

Emit on the existing broker — names only, never values:

- `secret.created` { repo, name, id, size_bytes, actor }
- `secret.updated` { repo, name, id, size_bytes, actor }
- `secret.deleted` { repo, name, id, actor }
- `secret.accessed` { repo, name, id, job_id, sequence, branch }

The last one fires when the scheduler attaches a secret to a CI run. Wire
through HMAC-signed webhooks (`docs/cryptographic-verifiability.md:159`),
so SIEM integration is free.

## Operational concerns

- **KEK rotation**: handled by GCP KMS automatic rotation; no docstore
  code change.
- **Per-secret rotation**: `ds secrets set` with a new value; `updated_at`
  bumps.
- **Backup**: standard Postgres dump. A restored DB without KMS access
  stays sealed — the desired property.
- **Repo rename**: `UPDATE repo_secrets SET repo=$1 WHERE repo=$2` in the
  same transaction as the rename.
- **Hard delete** on `DELETE`. Past CI runs that already consumed the
  value live in their step exec record — out of docstore's scope.
- **Dev mode**: add `LocalEncryptor` mirroring the `LocalSigner` pattern
  in `internal/citoken/citoken.go:103`. Persists an AES key to
  `~/.docstore/dev-encryption-key`. Server prints a banner: "DEV MODE —
  secrets sealed by local key, NOT KMS".

## Implementation phases

Each phase is independently shippable.

1. **`internal/secrets/`** — service interface (`Encryptor`, `Service`),
   KMS encryptor, local encryptor, schema migration, unit tests against a
   stub encryptor and integration tests against the real Postgres
   testcontainer.
2. **REST handlers** — `internal/server/handlers_secrets.go` plus RBAC
   tests.
3. **CLI** — `ds secrets {list,set,unset}` subcommands.
4. **ciconfig parser** — `secrets:` per check; reject unknown names.
5. **Scheduler integration** — gating policy + batch decrypt + job-spec
   wiring.
6. **Executor wiring** — extend `secretMap` / `llb.AddSecret`; worker-side
   scrubber.
7. **Event types** plumbed through the broker.
8. **`docs/secrets.md`** — user model, threat model, rotation, dev mode.

## Open trade-offs

- **One KEK per server vs. per repo.** v1: one KEK. Per-repo KEK gives
  stronger blast-radius isolation if a single repo's IAM is compromised,
  at the cost of N KMS keys to manage. Worth revisiting at scale.
- **Audit-event hash chain.** v1: events flow through the broker but are
  not in the per-branch `commit_hash` chain. Putting them in a separate
  `secret_events` chain (same SHA-256 pattern) gives tamper-evident audit
  cheaply. Probably worth doing in v1.
- **Symmetric vs. asymmetric KMS.** Use symmetric KMS (Cloud KMS
  `ENCRYPT_DECRYPT`). The existing citoken key is asymmetric (signing);
  these are different keys for different purposes.
