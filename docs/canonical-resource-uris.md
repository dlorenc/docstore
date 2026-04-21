# Canonical Resource URIs

Every resource in docstore has a canonical URI that uniquely identifies it across repos. These URIs are used by agents and external tools to reference resources without ambiguity.

## URI Scheme

The `repo` path segment is the full repo identifier (e.g. `acme/myrepo`).

| Resource | Canonical URI pattern | Example |
|---|---|---|
| Branch (current head) | `{repo}/branches/{name}` | `acme/myrepo/branches/feature/x` |
| Branch at sequence | `{repo}/branches/{name}@{seq}` | `acme/myrepo/branches/feature/x@42` |
| CheckRun | `{repo}/checks/{id}` | `acme/myrepo/checks/550e8400-e29b-41d4-a716-446655440000` |
| Review | `{repo}/reviews/{id}` | `acme/myrepo/reviews/6ba7b810-9dad-11d1-80b4-00c04fd430c8` |
| Proposal | `{repo}/proposals/{id}` | `acme/myrepo/proposals/7c9e6679-7425-40de-944b-e07fc1f90ae7` |
| File (on branch) | `{repo}/branches/{branch}/{path}` | `acme/myrepo/branches/feature/x/src/main.go` |
| Issue | `{repo}/issues/{number}` | `acme/myrepo/issues/42` |

## Repo field in API types

The `Branch`, `CheckRun`, and `Review` types each carry a `repo` field so that agents holding a reference to one of these objects can construct the canonical URI without needing additional context.

```json
{
  "repo": "acme/myrepo",
  "name": "feature/x",
  "head_sequence": 42,
  ...
}
```

This field is always populated by the server from the URL path; clients do not need to set it.

## Constructing a URI

Given a `Branch` object:

```
{branch.repo}/branches/{branch.name}
```

Given a `Branch` object at a specific sequence:

```
{branch.repo}/branches/{branch.name}@{branch.head_sequence}
```

Given a `CheckRun` object:

```
{check_run.repo}/checks/{check_run.id}
```

Given a `Review` object:

```
{review.repo}/reviews/{review.id}
```
