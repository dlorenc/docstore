module github.com/dlorenc/docstore/sdk/go/docstore

go 1.25.5

require github.com/dlorenc/docstore v0.0.0

// The SDK is versioned separately from the server module but needs the public
// api package from the server tree. While this lives in the monorepo and the
// server hasn't been tagged yet, resolve it via replace. Drop when the server
// publishes a tag and this module is tagged for the first time.
replace github.com/dlorenc/docstore => ../../..
