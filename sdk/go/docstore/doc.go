// Package docstore is the official Go client library for the docstore server.
//
// # Usage
//
// Construct a client with the base URL of a docstore server and one or more
// authentication options, then use the resource-scoped handles to call the
// API:
//
//	client, err := docstore.NewClient("https://api.example.com",
//	    docstore.WithBearerToken(os.Getenv("DOCSTORE_TOKEN")),
//	)
//	if err != nil {
//	    log.Fatal(err)
//	}
//
//	// Commit files on a branch.
//	_, err = client.Repo("acme/platform").Commit(ctx, api.CommitRequest{
//	    Branch:  "main",
//	    Message: "update config",
//	    Files: []api.FileChange{
//	        {Path: "config.yaml", Content: []byte("version: 2\n")},
//	    },
//	})
//
//	// Read a file at the head of main.
//	file, err := client.Repo("acme/platform").File(ctx, "config.yaml",
//	    docstore.AtHead("main"))
//
// # Authentication
//
// WithBearerToken sets Authorization: Bearer <token>. This is forward-
// compatible with the API-token feature tracked in issue #101 of the server
// repo; today the server validates GCP IAP JWTs via the
// X-Goog-IAP-JWT-Assertion header, which is typically terminated by a
// reverse proxy. When calling the server directly (for example from within
// the same VPC behind IAP), provide an IAP-authenticated *http.Client via
// WithHTTPClient.
//
// WithIdentity sets the X-DocStore-Identity header, which the server
// honours when running in dev mode (no IAP). Use it for local development
// and test fixtures.
//
// # Errors
//
// All non-2xx responses are decoded into a *Error value that carries the
// HTTP status code and the server's error message. Common status codes also
// match sentinel errors (ErrNotFound, ErrUnauthorized, ErrForbidden) via
// errors.Is. Merge and rebase responses that fail with 409 Conflict or with
// a policy denial are wrapped into *ConflictError or *PolicyError values
// that expose the structured payload; unwrap them with errors.As.
package docstore
