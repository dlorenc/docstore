# test/hello — CI smoke test repo

A minimal Go program used to validate the docstore CI runner end-to-end.

## Purpose

This directory is committed to the docstore server (`default/default` repo) as a
source tree for the CI runner to build. It exists to verify the full pipeline:

1. A commit lands on a branch in docstore
2. The `commit.created` event fires via the eventing system
3. The ci-runner webhook receives the event
4. The ci-runner fetches `.docstore/ci.yaml` from `main` and clones the branch source
5. BuildKit executes the checks (`go build`, `go test`, `go vet`)
6. Check run results appear in docstore via `GET /-/branch/{branch}/checks`

## Why it lives here

The ci-runner always fetches `.docstore/ci.yaml` from the `main` branch of the
docstore repo, and clones the branch under test as the source tree. Keeping this
test project in the git repo means it's versioned alongside the CI runner code and
can be updated when the ci.yaml format or runner behavior changes.

## Future

When docstore self-hosts its own CI (building and testing the docstore server
itself), this directory will be replaced by a real `.docstore/ci.yaml` at the repo
root and the full docstore source tree will be the thing under test.
