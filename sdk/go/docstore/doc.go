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
// WithBearerToken sets Authorization: Bearer <token>. The server validates
// Google ID tokens issued to the configured OAuth client. This is the standard
// authentication path used by the ds CLI after `ds login`.
//
// WithIdentity sets the X-DocStore-Identity header, which the server
// honours when running in dev mode (no OAuth). Use it for local development
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
