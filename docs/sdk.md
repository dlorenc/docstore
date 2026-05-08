# Go SDK

The Go SDK lives at `sdk/go/docstore` and is a separate Go module (`github.com/dlorenc/docstore/sdk/go/docstore`).

## Installation

```bash
go get github.com/dlorenc/docstore/sdk/go/docstore
```

While the server module is untagged (monorepo development), the SDK's `go.mod` uses a `replace` directive to point at the local tree. For external consumers, this will be dropped once the server publishes a tag.

## Creating a client

```go
import "github.com/dlorenc/docstore/sdk/go/docstore"

client, err := docstore.NewClient("https://docstore.dev",
    docstore.WithBearerToken("your-token"),
)
if err != nil {
    log.Fatal(err)
}
```

### Client options

| Option | Description |
|---|---|
| `WithBearerToken(token string)` | Sets `Authorization: Bearer <token>` on every request. |
| `WithIdentity(identity string)` | Sets `X-DocStore-Identity` header (dev mode only — server must have `DEV_IDENTITY` set). |
| `WithHTTPClient(h *http.Client)` | Replaces the default `http.DefaultClient`. Use for custom timeouts, proxies, or a custom transport. |
| `WithUserAgent(ua string)` | Overrides the `User-Agent` header (default: `docstore-go-sdk/0.1`). |

## Repo-scoped operations

Use `Client.Repo("owner/name")` to get a `*RepoClient` scoped to a specific repository:

```go
repo := client.Repo("acme/platform")
```

`RepoClient` is cheap to construct and safe for concurrent use.

### Reading files

```go
// Read from the head of main (default).
file, err := repo.File(ctx, "config.yaml", docstore.AtHead("main"))
fmt.Printf("%s: %s\n", file.Path, string(file.Content))

// Read at a specific sequence.
file, err = repo.File(ctx, "config.yaml", docstore.AtSequence(42))

// Read at a named release.
file, err = repo.File(ctx, "config.yaml", docstore.AtRelease("v1.0.0"))
```

### Listing the tree

```go
tree, err := repo.Tree(ctx, docstore.TreeAtHead("feature/x"))
for _, entry := range tree.Entries {
    fmt.Println(entry.Path, entry.ContentType)
}
```

### Committing files

```go
import "github.com/dlorenc/docstore/api"

resp, err := repo.Commit(ctx, api.CommitRequest{
    Branch:  "feature/x",
    Message: "update config",
    Files: []api.FileChange{
        {Path: "config.yaml", Content: []byte("version: 2\n")},
        // Nil content deletes the file:
        {Path: "old.txt", Content: nil},
    },
})
if err != nil {
    log.Fatal(err)
}
fmt.Println("sequence:", resp.Sequence)
```

### Branch operations

```go
// List branches.
branches, err := repo.Branches(ctx)

// Create a branch.
_, err = repo.CreateBranch(ctx, api.CreateBranchRequest{Name: "feature/x"})

// Get branch metadata.
branch, err := repo.Branch(ctx, "feature/x")

// Check merge policy status.
status, err := repo.BranchStatus(ctx, "feature/x")
fmt.Println("mergeable:", status.Mergeable)
for _, p := range status.Policies {
    fmt.Printf("  %s: pass=%v reason=%s\n", p.Name, p.Pass, p.Reason)
}

// Delete a branch.
_, err = repo.DeleteBranch(ctx, "feature/x")
```

### Merge and rebase

```go
// Merge a branch into main.
result, err := repo.Merge(ctx, api.MergeRequest{Branch: "feature/x"})
if err != nil {
    // Check for conflict or policy error:
    var conflict *docstore.ConflictError
    var policyErr *docstore.PolicyError
    if errors.As(err, &conflict) {
        fmt.Println("conflicts:", conflict.Paths)
    } else if errors.As(err, &policyErr) {
        fmt.Println("policy denied:", policyErr.Results)
    }
}

// Rebase a branch.
_, err = repo.Rebase(ctx, api.RebaseRequest{Branch: "feature/x"})
```

### Diff

```go
diff, err := repo.Diff(ctx, "feature/x")
for _, change := range diff.Changes {
    fmt.Printf("%s (%s)\n", change.Path, change.Status)
}
```

### Reviews and checks

```go
// Submit a review.
_, err = repo.SubmitReview(ctx, api.CreateReviewRequest{
    Branch: "feature/x",
    Status: api.ReviewApproved,
    Body:   "LGTM",
})

// List reviews.
reviews, err := repo.Reviews(ctx, "feature/x")

// Report a CI check.
_, err = repo.ReportCheck(ctx, api.CreateCheckRunRequest{
    Branch:    "feature/x",
    CheckName: "ci/build",
    Status:    api.CheckRunPassed,
})

// List checks.
checks, err := repo.Checks(ctx, "feature/x")
```

### Roles

```go
// List roles.
roles, err := repo.Roles(ctx)

// Set a role.
err = repo.SetRole(ctx, "bob@example.com", api.RoleWriter)

// Delete a role.
err = repo.DeleteRole(ctx, "bob@example.com")
```

### Releases

```go
// Create a release at the current head.
release, err := repo.CreateRelease(ctx, api.CreateReleaseRequest{
    Name:  "v1.0.0",
    Notes: "First stable release",
})

// List releases.
releases, err := repo.ListReleases(ctx)

// Get a release.
release, err = repo.GetRelease(ctx, "v1.0.0")

// Delete a release (admin-only).
err = repo.DeleteRelease(ctx, "v1.0.0")
```

### Chain verification

```go
chain, err := repo.ChainSegment(ctx, 1, 50)
// Verify hash links locally...
```

### Purge (admin-only)

```go
result, err := repo.Purge(ctx, api.PurgeRequest{OlderThan: "720h"})
```

## Org and repo management

```go
orgs := client.Orgs()
repos := client.Repos()

// Create an org.
org, err := orgs.Create(ctx, "acme")

// List orgs.
list, err := orgs.List(ctx)

// Create a repo.
repo, err := repos.Create(ctx, api.CreateRepoRequest{Owner: "acme", Name: "platform"})
```

## Error types

| Type | How to match | Returned when |
|---|---|---|
| `*docstore.ConflictError` | `errors.As` | Merge or rebase has conflicts |
| `*docstore.PolicyError` | `errors.As` | Merge rejected by a policy |
| `docstore.ErrNotFound` | `errors.Is` | 404 response |
| `docstore.ErrUnauthorized` | `errors.Is` | 401 response |
| `docstore.ErrForbidden` | `errors.Is` | 403 response |
| `docstore.ErrConflict` | `errors.Is` | 409 response (non-merge) |

Use `errors.As` for `*ConflictError` and `*PolicyError`; use `errors.Is` for the sentinel values.

## Complete example

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/dlorenc/docstore/api"
    "github.com/dlorenc/docstore/sdk/go/docstore"
)

func main() {
    client, err := docstore.NewClient("https://docstore.dev",
        docstore.WithBearerToken("your-token"),
    )
    if err != nil {
        log.Fatal(err)
    }

    ctx := context.Background()
    repo := client.Repo("acme/platform")

    // Read the current config.
    file, err := repo.File(ctx, "config.yaml", docstore.AtHead("main"))
    if err != nil {
        log.Fatal(err)
    }
    fmt.Printf("config.yaml: %s\n", string(file.Content))

    // Commit a change on a branch.
    _, err = repo.Commit(ctx, api.CommitRequest{
        Branch:  "feature/update-config",
        Message: "bump version to 3",
        Files: []api.FileChange{
            {Path: "config.yaml", Content: []byte("version: 3\n")},
        },
    })
    if err != nil {
        log.Fatal(err)
    }
}
```
