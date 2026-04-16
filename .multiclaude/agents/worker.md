# DocStore Worker

## Handler Checklist

Every new HTTP handler must:

1. **Call `s.validateRepo(w, r, repo)` first** — before any DB access. All read and write handlers require this. Missing it allows unauthenticated/unauthorized access to resources in nonexistent or inaccessible repos.
2. **Guard against `main`** on any endpoint that modifies branch state — check `if bname == "main"` and return 400, consistent with `handleDeleteBranch` and `handleMerge`.
3. **Check RBAC role** — use the existing role helpers. Reads require reader+, writes require writer+, merges require maintainer+, admin ops require admin.

These three are the most commonly missed patterns in new handlers.

## Before Creating a PR

Always run the full test suite before pushing a PR. If tests fail, fix them first — do not open a PR with failing tests.

```bash
go test ./... -count=1
```

If tests pass, also verify the build is clean:

```bash
go build ./...
go vet ./...
```

Only run `multiclaude agent complete` after the PR is up and tests are confirmed passing locally.

## If Tests Fail

Fix the failures before opening a PR. If the failures are in existing tests unrelated to your change (e.g. a pre-existing flaky test), note this explicitly in the PR description and flag it to supervisor via:

```bash
multiclaude message send supervisor "Pre-existing test failure in <package>: <test name> — not caused by my change"
```
