package main

// contract_test.go — cross-package contract tests for the scheduler.
//
// TestBranchesEndpointContract was previously here to verify that the HTTP
// envelope for GET /-/branches matched what fetchBranchHead expected to decode.
// That test is no longer needed: fetchBranchHead now reads directly from the
// internal/store.Store (bypassing HTTP entirely), so there is no HTTP decoding
// to validate.
//
// If the /branches wire format ever needs a cross-package contract test, it
// should live in internal/server's test suite rather than here.
