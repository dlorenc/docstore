# Policy and Access Control

DocStore has three layers of access control:

1. **IAP authentication** — validates GCP Identity-Aware Proxy JWTs to establish identity.
2. **RBAC** — per-repo role assignments that gate which HTTP methods each identity can call.
3. **OPA policy engine** — Rego-based merge gates that can require reviews, CI checks, and OWNERS approval.

## Authentication (IAP)

The server validates the `X-Goog-IAP-JWT-Assertion` header on every request (except `GET /healthz`). The JWT is RS256-signed by Google. Public keys are fetched from `https://www.gstatic.com/iap/verify/public_key-jwk` and cached for 1 hour. The identity is extracted from the `email` claim.

**Dev mode:** Set `DEV_IDENTITY=alice@example.com` (or `--dev-identity`) on the server to bypass JWT validation. All requests are treated as that identity.

## RBAC roles

Each repo has an independent role table. Roles are:

| Role | Description |
|---|---|
| `reader` | Read-only access to all repo data |
| `writer` | Can commit to non-main branches, submit reviews, add comments |
| `maintainer` | All writer permissions + create branches, merge, rebase, delete branches, create releases |
| `admin` | All maintainer permissions + manage roles, delete releases, purge commits |

### Role enforcement

The RBAC middleware (`internal/server/middleware.go`) checks roles for all `/repos/{name}/-/` endpoints. The specific rules:

| Action | Minimum role |
|---|---|
| Any `GET` | `reader` |
| `POST /commit` (to non-main branch) | `writer` |
| `POST /commit` (to `main` directly) | `maintainer` |
| `POST /review-comment`, `DELETE /review-comment/*` | `writer` |
| `PATCH /branch/*` (draft promotion) | `writer` |
| `POST /branch`, `POST /merge`, `POST /rebase` | `maintainer` |
| `DELETE /branch/*` | `maintainer` |
| `POST /releases` | `maintainer` |
| `DELETE /releases/*` | `admin` |
| `GET /roles`, `PUT /roles/*`, `DELETE /roles/*` | `admin` |

### Bootstrap admin

If `BOOTSTRAP_ADMIN=alice@example.com` is set on the server, that identity has admin access to any repo that has no admin assigned yet. Once a repo has at least one admin, the bootstrap flag is ignored for that repo.

### Managing roles

```bash
# Via CLI (requires a workspace in the target repo):
ds roles                              # list roles
ds roles set bob@example.com writer  # assign
ds roles delete bob@example.com      # remove

# Via API:
PUT /repos/acme/platform/-/roles/bob@example.com
{"role": "writer"}

DELETE /repos/acme/platform/-/roles/bob@example.com
```

## OPA policy engine

The policy engine runs on `POST /repos/{name}/-/merge` (and `GET /repos/{name}/-/branch/{name}/status` for dry-run evaluation). It evaluates all `.rego` files in `.docstore/policy/` on the `main` branch.

### Bootstrap mode

If no `.rego` files exist, the engine is nil and all merges are allowed. This avoids a chicken-and-egg problem when bootstrapping a repo before any policies are in place.

### Policy file format

Each `.rego` file must declare `package docstore.<name>` and define an `allow` rule (and optionally a `reason` rule):

```rego
package docstore.require_review

import future.keywords.if

default allow = false

allow if {
    # At least one approved review at or after the branch head.
    some review in input.reviews
    review.status == "approved"
    review.sequence >= input.base_sequence
}

reason = "at least one approved review is required" if {
    not allow
}
```

The policy name is derived from the last segment of the package path (e.g. `require_review`).

### Policy input

Every policy evaluation receives an `Input` struct as `input`:

```json
{
  "actor": "alice@example.com",
  "actor_roles": ["maintainer"],
  "action": "merge",
  "repo": "acme/platform",
  "branch": "feature/x",
  "draft": false,
  "changed_paths": ["config.yaml", "docs/guide.md"],
  "reviews": [
    {"reviewer": "bob@example.com", "status": "approved", "sequence": 43}
  ],
  "check_runs": [
    {"check_name": "ci/build", "status": "passed", "sequence": 43},
    {"check_name": "ci/test", "status": "passed", "sequence": 43}
  ],
  "owners": {
    "docs/": ["carol@example.com"],
    "": ["alice@example.com", "bob@example.com"]
  },
  "head_sequence": 43,
  "base_sequence": 30
}
```

Field details:

- `actor` — Identity performing the merge.
- `actor_roles` — The actor's roles on this repo (always a list; typically one element).
- `action` — Always `"merge"` for policy evaluation.
- `branch` — Branch being merged.
- `draft` — Whether the branch is a draft.
- `changed_paths` — All file paths changed on the branch relative to `base_sequence`.
- `reviews` — Reviews created at `head_sequence` or any earlier sequence on this branch. Stale means the review was created before the current head.
- `check_runs` — Same staleness rule as reviews.
- `owners` — Map from path prefix to list of owner emails. Derived from OWNERS files (see below). The `""` key is the root OWNERS file.
- `head_sequence` — Current head sequence of the branch.
- `base_sequence` — The sequence on `main` at which the branch was created or last rebased.

### Evaluation rules

- Each policy file is evaluated independently. All must pass (`allow = true`) for the merge to proceed.
- Evaluation has a 5-second timeout per policy. A timed-out policy returns an error (HTTP 500), not a silent deny.
- The policy result for each file is returned in the merge/status response:
  ```json
  {"name": "require_review", "pass": false, "reason": "at least one approved review is required"}
  ```

### Policy caching

Compiled OPA policies are cached per repo. The cache is invalidated when:
- A commit is pushed directly to `main` (`POST /repos/{name}/-/commit` with `branch=main`).
- A merge is completed (`POST /repos/{name}/-/merge`).

## OWNERS files

OWNERS files define code owners per directory. The server loads them from the materialized `main` tree when building the policy input.

### Format

An OWNERS file is a plain text file, one email per line:

```
alice@example.com
bob@example.com
```

Place OWNERS files at any directory level:

```
OWNERS                  (root owners)
docs/OWNERS             (owners for docs/ and below)
config/prod/OWNERS      (owners for config/prod/ and below)
```

### Longest-prefix matching

For each changed file path, the server finds the longest-prefix OWNERS file. For example, if `docs/guide.md` is changed and both `OWNERS` and `docs/OWNERS` exist, `docs/OWNERS` wins. The resolved owners for each prefix are included in `input.owners`.

### Using OWNERS in policies

```rego
package docstore.require_owners_approval

import future.keywords.if
import future.keywords.every

default allow = false

# Find the owners for a given path by longest-prefix match.
owners_for(path) := owners if {
    prefixes := {prefix | input.owners[prefix]; startswith(path, prefix)}
    prefix := max(prefixes)
    owners := input.owners[prefix]
}

allow if {
    every path in input.changed_paths {
        required_owners := owners_for(path)
        some review in input.reviews
        review.status == "approved"
        review.sequence >= input.base_sequence
        review.reviewer in required_owners
    }
}

reason = "all changed paths must be approved by their OWNERS" if {
    not allow
}
```

## Deploying policies

Add `.rego` files to `.docstore/policy/` on the `main` branch:

```bash
mkdir -p .docstore/policy
cat > .docstore/policy/require_review.rego <<'EOF'
package docstore.require_review
...
EOF

ds checkout main
ds commit -m "add require_review policy"
```

The policy takes effect immediately on the next merge attempt (cache is invalidated by the commit to main).
