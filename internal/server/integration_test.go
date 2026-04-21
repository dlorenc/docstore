package server_test

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dbpkg "github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/server"
	"github.com/dlorenc/docstore/internal/store"
	"github.com/dlorenc/docstore/internal/testutil"
)

// seed inserts test data for HTTP-level integration tests.
func seed(t *testing.T, db *sql.DB) {
	t.Helper()

	stmts := []string{
		// Ensure the 'default' org and repo rows exist so validateRepo passes.
		`INSERT INTO orgs (name) VALUES ('default') ON CONFLICT DO NOTHING`,
		`INSERT INTO repos (name, owner) VALUES ('default/default', 'default') ON CONFLICT DO NOTHING`,
		`INSERT INTO documents (repo, version_id, path, content, content_hash, created_by)
		 VALUES ('default/default', 'aaaaaaaa-0000-0000-0000-000000000001', 'hello.txt', 'hello world', 'hash_hello_v1', 'alice')`,
		`INSERT INTO documents (repo, version_id, path, content, content_hash, created_by)
		 VALUES ('default/default', 'aaaaaaaa-0000-0000-0000-000000000002', 'hello.txt', 'hello world v2', 'hash_hello_v2', 'alice')`,
		`INSERT INTO documents (repo, version_id, path, content, content_hash, created_by)
		 VALUES ('default/default', 'aaaaaaaa-0000-0000-0000-000000000003', 'world.txt', 'the world', 'hash_world_v1', 'bob')`,
		`INSERT INTO documents (repo, version_id, path, content, content_hash, created_by)
		 VALUES ('default/default', 'aaaaaaaa-0000-0000-0000-000000000004', 'deleted.txt', 'gone soon', 'hash_deleted_v1', 'alice')`,
		// Insert commits rows for global sequence allocation.
		`INSERT INTO commits (repo, sequence, branch, message, author) OVERRIDING SYSTEM VALUE VALUES
		 ('default/default', 1, 'main', 'initial commit', 'alice'),
		 ('default/default', 2, 'main', 'update hello',   'alice'),
		 ('default/default', 3, 'main', 'add deleted',    'bob'),
		 ('default/default', 4, 'main', 'remove deleted', 'bob')`,
		`SELECT setval('commits_sequence_seq', 4, true)`,
		`INSERT INTO file_commits (repo, commit_id, sequence, path, version_id, branch)
		 VALUES ('default/default', 'cccccccc-0000-0000-0000-000000000001', 1, 'hello.txt', 'aaaaaaaa-0000-0000-0000-000000000001', 'main')`,
		`INSERT INTO file_commits (repo, commit_id, sequence, path, version_id, branch)
		 VALUES ('default/default', 'cccccccc-0000-0000-0000-000000000002', 1, 'world.txt', 'aaaaaaaa-0000-0000-0000-000000000003', 'main')`,
		`INSERT INTO file_commits (repo, commit_id, sequence, path, version_id, branch)
		 VALUES ('default/default', 'cccccccc-0000-0000-0000-000000000003', 2, 'hello.txt', 'aaaaaaaa-0000-0000-0000-000000000002', 'main')`,
		`INSERT INTO file_commits (repo, commit_id, sequence, path, version_id, branch)
		 VALUES ('default/default', 'cccccccc-0000-0000-0000-000000000004', 3, 'deleted.txt', 'aaaaaaaa-0000-0000-0000-000000000004', 'main')`,
		`INSERT INTO file_commits (repo, commit_id, sequence, path, version_id, branch)
		 VALUES ('default/default', 'cccccccc-0000-0000-0000-000000000005', 4, 'deleted.txt', NULL, 'main')`,
		`UPDATE branches SET head_sequence = 4 WHERE name = 'main'`,
	}

	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\nstatement: %s", err, stmt)
		}
	}
}

func TestIntegrationTreeEndpoint(t *testing.T) {
	t.Parallel()
	db := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	seed(t, db)
	handler := server.New(dbpkg.NewStore(db), db, "test@example.com", "test@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/repos/default/default/-/tree")
	if err != nil {
		t.Fatalf("GET /tree: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var entries []store.TreeEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("expected 2 tree entries, got %d", len(entries))
	}
}

func TestIntegrationFileEndpoint(t *testing.T) {
	t.Parallel()
	db := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	seed(t, db)
	handler := server.New(dbpkg.NewStore(db), db, "test@example.com", "test@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	t.Run("get file content", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/repos/default/default/-/file/hello.txt")
		if err != nil {
			t.Fatalf("GET /file/hello.txt: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}

		var fc store.FileContent
		if err := json.NewDecoder(resp.Body).Decode(&fc); err != nil {
			t.Fatalf("decode: %v", err)
		}

		if fc.ContentHash != "hash_hello_v2" {
			t.Errorf("expected hash_hello_v2, got %s", fc.ContentHash)
		}
	})

	t.Run("file not found", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/repos/default/default/-/file/nope.txt")
		if err != nil {
			t.Fatalf("GET /file/nope.txt: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", resp.StatusCode)
		}
	})

	t.Run("file history", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/repos/default/default/-/file/hello.txt/history")
		if err != nil {
			t.Fatalf("GET /file/hello.txt/history: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}

		var entries []store.FileHistoryEntry
		if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
			t.Fatalf("decode: %v", err)
		}

		if len(entries) != 2 {
			t.Fatalf("expected 2 history entries, got %d", len(entries))
		}
	})

	t.Run("file at sequence", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/repos/default/default/-/file/hello.txt?at=1")
		if err != nil {
			t.Fatalf("GET /file/hello.txt?at=1: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}

		var fc store.FileContent
		if err := json.NewDecoder(resp.Body).Decode(&fc); err != nil {
			t.Fatalf("decode: %v", err)
		}

		if fc.ContentHash != "hash_hello_v1" {
			t.Errorf("expected hash_hello_v1, got %s", fc.ContentHash)
		}
	})
}

func TestIntegrationBranchesEndpoint(t *testing.T) {
	t.Parallel()
	database := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	seed(t, database)

	// Add feature branch.
	if _, err := database.Exec(
		"INSERT INTO branches (name, head_sequence, base_sequence, status) VALUES ('feature/a', 5, 2, 'active')",
	); err != nil {
		t.Fatalf("seed branch: %v", err)
	}

	handler := server.New(dbpkg.NewStore(database), database, "test@example.com", "test@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	t.Run("list all branches", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/repos/default/default/-/branches")
		if err != nil {
			t.Fatalf("GET /branches: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}

		var branches []store.BranchInfo
		if err := json.NewDecoder(resp.Body).Decode(&branches); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(branches) != 2 {
			t.Fatalf("expected 2 branches, got %d", len(branches))
		}
	})

	t.Run("filter active", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/repos/default/default/-/branches?status=active")
		if err != nil {
			t.Fatalf("GET /branches?status=active: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}

		var branches []store.BranchInfo
		if err := json.NewDecoder(resp.Body).Decode(&branches); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(branches) != 2 {
			t.Fatalf("expected 2 active branches, got %d", len(branches))
		}
	})
}

func TestIntegrationDiffEndpoint(t *testing.T) {
	t.Parallel()
	database := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	seed(t, database)

	// Create a branch with changes.
	stmts := []string{
		`INSERT INTO branches (name, head_sequence, base_sequence, status) VALUES ('feature/diff', 5, 2, 'active')`,
		`INSERT INTO documents (repo, version_id, path, content, content_hash, created_by)
		 VALUES ('default/default', 'aaaaaaaa-0000-0000-0000-000000000010', 'new.txt', 'new file', 'hash_new', 'carol')`,
		`INSERT INTO commits (repo, sequence, branch, message, author) OVERRIDING SYSTEM VALUE VALUES
		 ('default/default', 5, 'feature/diff', 'add new', 'carol')`,
		`SELECT setval('commits_sequence_seq', 5, true)`,
		`INSERT INTO file_commits (repo, commit_id, sequence, path, version_id, branch)
		 VALUES ('default/default', 'cccccccc-0000-0000-0000-000000000010', 5, 'new.txt', 'aaaaaaaa-0000-0000-0000-000000000010', 'feature/diff')`,
	}
	for _, stmt := range stmts {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt)
		}
	}

	handler := server.New(dbpkg.NewStore(database), database, "test@example.com", "test@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	t.Run("diff with branch", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/repos/default/default/-/diff?branch=feature/diff")
		if err != nil {
			t.Fatalf("GET /diff: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}

		var result store.DiffResult
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(result.BranchChanges) != 1 {
			t.Fatalf("expected 1 changed file, got %d", len(result.BranchChanges))
		}
		if result.BranchChanges[0].Path != "new.txt" {
			t.Errorf("expected new.txt, got %s", result.BranchChanges[0].Path)
		}
	})

	t.Run("diff missing branch param", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/repos/default/default/-/diff")
		if err != nil {
			t.Fatalf("GET /diff: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", resp.StatusCode)
		}
	})

	t.Run("diff nonexistent branch", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/repos/default/default/-/diff?branch=nonexistent")
		if err != nil {
			t.Fatalf("GET /diff: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", resp.StatusCode)
		}
	})
}

func TestIntegrationCommitEndpoint(t *testing.T) {
	t.Parallel()
	db := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	seed(t, db)
	handler := server.New(nil, db, "test@example.com", "")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	t.Run("existing commit", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/repos/default/default/-/commit/1")
		if err != nil {
			t.Fatalf("GET /commit/1: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}

		var detail store.CommitDetail
		if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
			t.Fatalf("decode: %v", err)
		}

		if detail.Sequence != 1 {
			t.Errorf("expected sequence 1, got %d", detail.Sequence)
		}
		if len(detail.Files) != 2 {
			t.Fatalf("expected 2 files, got %d", len(detail.Files))
		}
	})

	t.Run("nonexistent commit", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/repos/default/default/-/commit/999")
		if err != nil {
			t.Fatalf("GET /commit/999: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", resp.StatusCode)
		}
	})

	t.Run("invalid sequence", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/repos/default/default/-/commit/abc")
		if err != nil {
			t.Fatalf("GET /commit/abc: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", resp.StatusCode)
		}
	})
}

func TestIntegrationDeleteBranch(t *testing.T) {
	t.Parallel()
	database := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	seed(t, database)

	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, "test@example.com", "test@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Seed a branch to delete.
	if _, err := database.Exec(
		"INSERT INTO branches (name, head_sequence, base_sequence, status) VALUES ('feature/to-delete', 4, 4, 'active')",
	); err != nil {
		t.Fatalf("seed branch: %v", err)
	}

	t.Run("delete active branch returns 204", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/repos/default/default/-/branch/feature/to-delete", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("DELETE /branch/feature/to-delete: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("expected 204, got %d", resp.StatusCode)
		}

		// Verify status is 'abandoned' in DB.
		var status string
		if err := database.QueryRow("SELECT status FROM branches WHERE name = 'feature/to-delete'").Scan(&status); err != nil {
			t.Fatalf("query branch: %v", err)
		}
		if status != "abandoned" {
			t.Errorf("expected status 'abandoned', got %q", status)
		}
	})

	t.Run("delete main returns 400", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/repos/default/default/-/branch/main", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("DELETE /branch/main: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", resp.StatusCode)
		}
	})

	t.Run("delete nonexistent branch returns 404", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/repos/default/default/-/branch/nonexistent", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("DELETE /branch/nonexistent: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", resp.StatusCode)
		}
	})

	t.Run("delete abandoned branch returns 409", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/repos/default/default/-/branch/feature/to-delete", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("DELETE /branch/feature/to-delete: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("expected 409, got %d", resp.StatusCode)
		}
	})
}

func TestIntegrationRebase_FullFlow(t *testing.T) {
	t.Parallel()
	database := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, "test@example.com", "test@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	post := func(path, body string) *http.Response {
		t.Helper()
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		return resp
	}

	// Step 1: Commit to main (creates seq=1).
	r := post("/repos/default/default/-/commit", `{"branch":"main","message":"initial","author":"alice","files":[{"path":"base.txt","content":"YmFzZQ=="}]}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("POST /commit to main: expected 201, got %d", r.StatusCode)
	}

	// Step 2: Create branch (base=1, head=1).
	r = post("/repos/default/default/-/branch", `{"name":"feature/rebase-flow"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("POST /branch: expected 201, got %d", r.StatusCode)
	}

	// Step 3: Commit to branch (seq=2, adds "branch.txt").
	r = post("/repos/default/default/-/commit", `{"branch":"feature/rebase-flow","message":"branch work","author":"bob","files":[{"path":"branch.txt","content":"YnJhbmNo"}]}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("POST /commit to branch: expected 201, got %d", r.StatusCode)
	}

	// Step 4: Advance main (seq=3, adds "other.txt").
	r = post("/repos/default/default/-/commit", `{"branch":"main","message":"main advance","author":"alice","files":[{"path":"other.txt","content":"b3RoZXI="}]}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("POST /commit to advance main: expected 201, got %d", r.StatusCode)
	}

	// Step 5: GET /diff — should show main_changes before rebase.
	diffResp, err := http.Get(srv.URL + "/repos/default/default/-/diff?branch=feature/rebase-flow")
	if err != nil {
		t.Fatalf("GET /diff: %v", err)
	}
	defer diffResp.Body.Close()
	if diffResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /diff: expected 200, got %d", diffResp.StatusCode)
	}
	var diffResult store.DiffResult
	if err := json.NewDecoder(diffResp.Body).Decode(&diffResult); err != nil {
		t.Fatalf("decode diff: %v", err)
	}
	if len(diffResult.MainChanges) != 1 {
		t.Fatalf("expected 1 main_change before rebase, got %d", len(diffResult.MainChanges))
	}

	// Step 6: POST /rebase.
	r = post("/repos/default/default/-/rebase", `{"branch":"feature/rebase-flow","author":"bob"}`)
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		var errBody map[string]interface{}
		json.NewDecoder(r.Body).Decode(&errBody)
		t.Fatalf("POST /rebase: expected 200, got %d; body: %v", r.StatusCode, errBody)
	}
	var rebaseResp struct {
		BaseSequence    int64 `json:"base_sequence"`
		HeadSequence    int64 `json:"head_sequence"`
		CommitsReplayed int64 `json:"commits_replayed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&rebaseResp); err != nil {
		t.Fatalf("decode rebase response: %v", err)
	}
	if rebaseResp.CommitsReplayed != 1 {
		t.Errorf("expected 1 commit replayed, got %d", rebaseResp.CommitsReplayed)
	}

	// Step 7: GET /diff — should show no main_changes after rebase.
	diffResp2, err := http.Get(srv.URL + "/repos/default/default/-/diff?branch=feature/rebase-flow")
	if err != nil {
		t.Fatalf("GET /diff after rebase: %v", err)
	}
	defer diffResp2.Body.Close()
	if diffResp2.StatusCode != http.StatusOK {
		t.Fatalf("GET /diff after rebase: expected 200, got %d", diffResp2.StatusCode)
	}
	var diffResult2 store.DiffResult
	if err := json.NewDecoder(diffResp2.Body).Decode(&diffResult2); err != nil {
		t.Fatalf("decode diff2: %v", err)
	}
	if len(diffResult2.MainChanges) != 0 {
		t.Errorf("expected 0 main_changes after rebase, got %d", len(diffResult2.MainChanges))
	}

	// Step 8: POST /merge — should succeed cleanly.
	r = post("/repos/default/default/-/merge", `{"branch":"feature/rebase-flow","author":"alice"}`)
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		var errBody map[string]interface{}
		json.NewDecoder(r.Body).Decode(&errBody)
		t.Fatalf("POST /merge after rebase: expected 200, got %d; body: %v", r.StatusCode, errBody)
	}
}

// TestHTTP_AuthRequired verifies that write endpoints return 401 when no IAP JWT
// is present and the server is not in dev mode.
func TestHTTP_AuthRequired(t *testing.T) {
	t.Parallel()
	// No devIdentity → real IAP auth enforced.
	handler := server.New(nil, nil, "", "")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// POST /repos/default/default/-/commit without X-Goog-IAP-JWT-Assertion must return 401.
	resp, err := http.Post(srv.URL+"/repos/default/default/-/commit", "application/json",
		strings.NewReader(`{"branch":"main","message":"m","files":[{"path":"a.txt","content":"YQ=="}]}`))
	if err != nil {
		t.Fatalf("POST /commit: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["error"] != "unauthenticated" {
		t.Errorf("expected error=unauthenticated, got %q", body["error"])
	}
}

// TestHTTP_AuthIdentityUsedAsAuthor verifies that the authenticated identity
// (set via dev mode) is recorded as the commit author, not any value in the body.
func TestHTTP_AuthIdentityUsedAsAuthor(t *testing.T) {
	t.Parallel()
	const identity = "alice@example.com"
	database := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, identity, identity)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// POST /repos/default/default/-/commit with a different author in the body — it must be ignored.
	resp, err := http.Post(srv.URL+"/repos/default/default/-/commit", "application/json",
		strings.NewReader(`{"branch":"main","message":"test commit","author":"not-alice@example.com","files":[{"path":"hello.txt","content":"aGVsbG8="}]}`))
	if err != nil {
		t.Fatalf("POST /commit: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /commit: expected 201, got %d", resp.StatusCode)
	}

	// GET /repos/default/default/-/commit/1 and verify the author is the identity, not the body value.
	getResp, err := http.Get(srv.URL + "/repos/default/default/-/commit/1")
	if err != nil {
		t.Fatalf("GET /commit/1: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /commit/1: expected 200, got %d", getResp.StatusCode)
	}

	var detail store.CommitDetail
	if err := json.NewDecoder(getResp.Body).Decode(&detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.Author != identity {
		t.Errorf("expected author %q, got %q", identity, detail.Author)
	}
}

// ---------------------------------------------------------------------------
// New repo management integration tests
// ---------------------------------------------------------------------------

func TestHandleCreateRepo_Success(t *testing.T) {
	t.Parallel()
	database := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, "test@example.com", "test@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/repos", "application/json",
		strings.NewReader(`{"owner":"default","name":"myrepo"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
}

func TestHandleCreateRepo_Duplicate(t *testing.T) {
	t.Parallel()
	database := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, "test@example.com", "test@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	for i, wantCode := range []int{http.StatusCreated, http.StatusConflict} {
		resp, err := http.Post(srv.URL+"/repos", "application/json",
			strings.NewReader(`{"owner":"default","name":"dup"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != wantCode {
			t.Errorf("attempt %d: expected %d, got %d", i+1, wantCode, resp.StatusCode)
		}
	}
}

func TestHandleDeleteRepo_Success(t *testing.T) {
	t.Parallel()
	database := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, "test@example.com", "test@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Create a repo first.
	resp, err := http.Post(srv.URL+"/repos", "application/json",
		strings.NewReader(`{"owner":"default","name":"todel"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Delete it.
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/repos/default/todel", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}
}

func TestHandleDeleteRepo_NotFound(t *testing.T) {
	t.Parallel()
	database := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, "test@example.com", "test@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/repos/nonexistent", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestHandleListRepos(t *testing.T) {
	t.Parallel()
	database := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, "test@example.com", "test@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Create a repo to add alongside the seeded 'default'.
	resp, err := http.Post(srv.URL+"/repos", "application/json",
		strings.NewReader(`{"owner":"default","name":"extra"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	resp, err = http.Get(srv.URL + "/repos")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body struct {
		Repos []struct{ Name string `json:"name"` } `json:"repos"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Repos) < 2 {
		t.Fatalf("expected at least 2 repos, got %d", len(body.Repos))
	}
}

func TestIntegrationMultiRepo_FullIsolation(t *testing.T) {
	t.Parallel()
	database := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, "test@example.com", "test@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	post := func(path, body string) *http.Response {
		t.Helper()
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		return resp
	}

	// Create two repos.
	for _, name := range []string{"alpha", "beta"} {
		r := post("/repos", `{"owner":"default","name":"`+name+`"}`)
		r.Body.Close()
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("create repo %s: expected 201, got %d", name, r.StatusCode)
		}
	}

	// Commit unique files to each repo.
	r := post("/repos/default/alpha/-/commit", `{"branch":"main","message":"alpha init","files":[{"path":"alpha.txt","content":"YWxwaGE="}]}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("commit alpha: expected 201, got %d", r.StatusCode)
	}

	r = post("/repos/default/beta/-/commit", `{"branch":"main","message":"beta init","files":[{"path":"beta.txt","content":"YmV0YQ=="}]}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("commit beta: expected 201, got %d", r.StatusCode)
	}

	// GET /repos/alpha/-/tree must NOT contain beta.txt.
	resp, err := http.Get(srv.URL + "/repos/default/alpha/-/tree")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var alphaEntries []struct{ Path string `json:"path"` }
	json.NewDecoder(resp.Body).Decode(&alphaEntries)
	for _, e := range alphaEntries {
		if e.Path == "beta.txt" {
			t.Error("beta.txt should not appear in alpha's tree")
		}
	}
	if len(alphaEntries) != 1 || alphaEntries[0].Path != "alpha.txt" {
		t.Errorf("expected only alpha.txt in alpha tree, got %+v", alphaEntries)
	}

	// GET /repos/beta/-/tree must NOT contain alpha.txt.
	resp2, err := http.Get(srv.URL + "/repos/default/beta/-/tree")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var betaEntries []struct{ Path string `json:"path"` }
	json.NewDecoder(resp2.Body).Decode(&betaEntries)
	for _, e := range betaEntries {
		if e.Path == "alpha.txt" {
			t.Error("alpha.txt should not appear in beta's tree")
		}
	}
	if len(betaEntries) != 1 || betaEntries[0].Path != "beta.txt" {
		t.Errorf("expected only beta.txt in beta tree, got %+v", betaEntries)
	}
}

func TestIntegrationDeleteRepo_CleansUp(t *testing.T) {
	t.Parallel()
	database := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, "test@example.com", "test@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	post := func(path, body string) *http.Response {
		t.Helper()
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		return resp
	}

	// Create repo, add data.
	r := post("/repos", `{"owner":"default","name":"cleanup-test"}`)
	r.Body.Close()
	r = post("/repos/default/cleanup-test/-/commit", `{"branch":"main","message":"init","files":[{"path":"f.txt","content":"dGVzdA=="}]}`)
	r.Body.Close()

	// Verify tree is non-empty.
	resp, err := http.Get(srv.URL + "/repos/default/cleanup-test/-/tree")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 before delete, got %d", resp.StatusCode)
	}

	// Delete the repo.
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/repos/default/cleanup-test", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}

	// Repo should no longer exist.
	resp, err = http.Get(srv.URL + "/repos/default/cleanup-test")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Review and check-run integration tests
// ---------------------------------------------------------------------------

func TestIntegrationReviewFlow(t *testing.T) {
	t.Parallel()
	database := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, "alice@example.com", "alice@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	post := func(path, body string) *http.Response {
		t.Helper()
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		return resp
	}

	// Step 1: Create a branch.
	r := post("/repos/default/default/-/branch", `{"name":"feature/rev"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create branch: expected 201, got %d", r.StatusCode)
	}

	// Step 2: alice commits to the branch.
	r = post("/repos/default/default/-/commit", `{"branch":"feature/rev","message":"work","files":[{"path":"f.txt","content":"dGVzdA=="}]}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("commit: expected 201, got %d", r.StatusCode)
	}

	// Step 3: alice tries to approve her own commit — should get 403.
	r = post("/repos/default/default/-/review", `{"branch":"feature/rev","status":"approved","body":"self approve"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("self-approval: expected 403, got %d", r.StatusCode)
	}

	// Step 4: Switch server identity to bob who hasn't committed anything.
	handlerBob := server.New(writeStore, database, "bob@example.com", "bob@example.com")
	srvBob := httptest.NewServer(handlerBob)
	defer srvBob.Close()

	r, err := http.Post(srvBob.URL+"/repos/default/default/-/review", "application/json",
		strings.NewReader(`{"branch":"feature/rev","status":"approved","body":"LGTM"}`))
	if err != nil {
		t.Fatalf("bob review: %v", err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("bob review: expected 201, got %d", r.StatusCode)
	}

	var reviewResp struct {
		ID       string `json:"id"`
		Sequence int64  `json:"sequence"`
	}
	if err := json.NewDecoder(r.Body).Decode(&reviewResp); err != nil {
		t.Fatalf("decode review response: %v", err)
	}
	if reviewResp.ID == "" {
		t.Error("expected non-empty review id")
	}

	// Step 5: GET reviews for the branch — should return 1 review.
	getResp, err := http.Get(srv.URL + "/repos/default/default/-/branch/feature/rev/reviews")
	if err != nil {
		t.Fatalf("GET reviews: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET reviews: expected 200, got %d", getResp.StatusCode)
	}

	var reviews []struct {
		ID       string `json:"id"`
		Repo     string `json:"repo"`
		Reviewer string `json:"reviewer"`
		Status   string `json:"status"`
	}
	if err := json.NewDecoder(getResp.Body).Decode(&reviews); err != nil {
		t.Fatalf("decode reviews: %v", err)
	}
	if len(reviews) != 1 {
		t.Fatalf("expected 1 review, got %d", len(reviews))
	}
	if reviews[0].Status != "approved" {
		t.Errorf("expected status approved, got %q", reviews[0].Status)
	}
	if reviews[0].Repo != "default/default" {
		t.Errorf("expected repo default/default, got %q", reviews[0].Repo)
	}

	// Step 6: alice makes another commit — the prior review becomes stale.
	r2, err := http.Post(srv.URL+"/repos/default/default/-/commit", "application/json",
		strings.NewReader(`{"branch":"feature/rev","message":"update","files":[{"path":"f.txt","content":"dXBkYXRlZA=="}]}`))
	if err != nil {
		t.Fatalf("second commit: %v", err)
	}
	var commitResp struct{ Sequence int64 `json:"sequence"` }
	json.NewDecoder(r2.Body).Decode(&commitResp)
	r2.Body.Close()
	if r2.StatusCode != http.StatusCreated {
		t.Fatalf("second commit: expected 201, got %d", r2.StatusCode)
	}

	// The old review's sequence should differ from current head_sequence.
	if reviewResp.Sequence == commitResp.Sequence {
		t.Error("expected review to be stale after new commit")
	}
}

func TestIntegrationCheckRunFlow(t *testing.T) {
	t.Parallel()
	database := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, "ci-bot@example.com", "ci-bot@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	post := func(path, body string) *http.Response {
		t.Helper()
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		return resp
	}

	// Create a check run on main.
	r := post("/repos/default/default/-/check", `{"branch":"main","check_name":"ci/build","status":"passed"}`)
	defer r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create check: expected 201, got %d", r.StatusCode)
	}

	var checkResp struct {
		ID       string `json:"id"`
		Sequence int64  `json:"sequence"`
	}
	if err := json.NewDecoder(r.Body).Decode(&checkResp); err != nil {
		t.Fatalf("decode check response: %v", err)
	}
	if checkResp.ID == "" {
		t.Error("expected non-empty check run id")
	}

	// GET checks for main.
	getResp, err := http.Get(srv.URL + "/repos/default/default/-/branch/main/checks")
	if err != nil {
		t.Fatalf("GET checks: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET checks: expected 200, got %d", getResp.StatusCode)
	}

	var checks []struct {
		ID        string `json:"id"`
		Repo      string `json:"repo"`
		CheckName string `json:"check_name"`
		Status    string `json:"status"`
	}
	if err := json.NewDecoder(getResp.Body).Decode(&checks); err != nil {
		t.Fatalf("decode checks: %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("expected 1 check run, got %d", len(checks))
	}
	if checks[0].Status != "passed" {
		t.Errorf("expected status passed, got %q", checks[0].Status)
	}
	if checks[0].Repo != "default/default" {
		t.Errorf("expected repo default/default, got %q", checks[0].Repo)
	}
}

func TestIntegrationReviewRepoIsolation(t *testing.T) {
	t.Parallel()
	database := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	writeStore := dbpkg.NewStore(database)
	// alice is the reviewer; bob is the committer so alice can approve
	handler := server.New(writeStore, database, "alice@example.com", "alice@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	post := func(path, body string) *http.Response {
		t.Helper()
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		return resp
	}

	// Create two repos.
	for _, name := range []string{"repo-x", "repo-y"} {
		r := post("/repos", `{"owner":"default","name":"`+name+`"}`)
		r.Body.Close()
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("create repo %s: expected 201, got %d", name, r.StatusCode)
		}
	}

	// alice leaves a review on repo-x/main
	r := post("/repos/default/repo-x/-/review", `{"branch":"main","status":"approved"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("review repo-x: expected 201, got %d", r.StatusCode)
	}

	// GET /repos/repo-y/-/branch/main/reviews must be empty.
	getResp, err := http.Get(srv.URL + "/repos/default/repo-y/-/branch/main/reviews")
	if err != nil {
		t.Fatalf("GET reviews repo-y: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET reviews: expected 200, got %d", getResp.StatusCode)
	}

	var reviews []interface{}
	if err := json.NewDecoder(getResp.Body).Decode(&reviews); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(reviews) != 0 {
		t.Errorf("expected 0 reviews in repo-y, got %d", len(reviews))
	}
}

// ---------------------------------------------------------------------------
// RBAC integration tests
// ---------------------------------------------------------------------------

// TestIntegrationRBAC_FullFlow exercises the complete bootstrap → role assignment
// → role-constrained operations flow.
func TestIntegrationRBAC_FullFlow(t *testing.T) {
	t.Parallel()
	database := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	writeStore := dbpkg.NewStore(database)

	const bootstrapAdmin = "admin@example.com"
	const writerID = "writer@example.com"
	const maintainerID = "maintainer@example.com"

	// Helper: create a server with a specific dev identity.
	makeHandler := func(devID, bootAdmin string) *httptest.Server {
		h := server.New(writeStore, database, devID, bootAdmin)
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		return srv
	}

	doPost := func(srv *httptest.Server, path, body string) *http.Response {
		t.Helper()
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		return resp
	}
	doPut := func(srv *httptest.Server, path, body string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPut, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("PUT %s: %v", path, err)
		}
		return resp
	}
	doGet := func(srv *httptest.Server, path string) *http.Response {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		return resp
	}

	// Step 1: Create a repo as bootstrap admin.
	adminSrv := makeHandler(bootstrapAdmin, bootstrapAdmin)
	r := doPost(adminSrv, "/repos", `{"owner":"default","name":"rbacrepo"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: expected 201, got %d", r.StatusCode)
	}

	// Step 2: Bootstrap admin assigns writer and maintainer roles.
	r = doPut(adminSrv, "/repos/default/rbacrepo/-/roles/"+writerID, `{"role":"writer"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("assign writer role: expected 200, got %d", r.StatusCode)
	}

	r = doPut(adminSrv, "/repos/default/rbacrepo/-/roles/"+maintainerID, `{"role":"maintainer"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("assign maintainer role: expected 200, got %d", r.StatusCode)
	}

	// Step 3: Admin creates branch; writer commits to it.
	r = doPost(adminSrv, "/repos/default/rbacrepo/-/branch", `{"name":"feature/rbac-test"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("admin create branch: expected 201, got %d", r.StatusCode)
	}

	writerSrv := makeHandler(writerID, bootstrapAdmin)
	r = doPost(writerSrv, "/repos/default/rbacrepo/-/commit",
		`{"branch":"feature/rbac-test","message":"writer commit","files":[{"path":"f.txt","content":"aGVsbG8="}]}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("writer commit: expected 201, got %d", r.StatusCode)
	}

	// Step 4: Writer cannot merge (not maintainer).
	r = doPost(writerSrv, "/repos/default/rbacrepo/-/merge", `{"branch":"feature/rbac-test"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("writer merge: expected 403, got %d", r.StatusCode)
	}

	// Step 5: Maintainer can merge.
	maintainerSrv := makeHandler(maintainerID, bootstrapAdmin)
	r = doPost(maintainerSrv, "/repos/default/rbacrepo/-/merge", `{"branch":"feature/rbac-test"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("maintainer merge: expected 200, got %d", r.StatusCode)
	}

	// Step 6: Admin can read the tree.
	r = doGet(adminSrv, "/repos/default/rbacrepo/-/tree")
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("admin read tree: expected 200, got %d", r.StatusCode)
	}

	// Step 7: Unknown identity has no access.
	unknownSrv := makeHandler("unknown@example.com", bootstrapAdmin)
	r = doGet(unknownSrv, "/repos/default/rbacrepo/-/tree")
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("unknown identity read: expected 403, got %d", r.StatusCode)
	}
}

// TestIntegrationRBAC_CrossRepoIsolation verifies that a role in repo-a
// grants no access to repo-b.
func TestIntegrationRBAC_CrossRepoIsolation(t *testing.T) {
	t.Parallel()
	database := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	writeStore := dbpkg.NewStore(database)

	const bootstrapAdmin = "admin@example.com"
	const alice = "alice@example.com"

	adminSrv := httptest.NewServer(server.New(writeStore, database, bootstrapAdmin, bootstrapAdmin))
	t.Cleanup(adminSrv.Close)

	doPost := func(srv *httptest.Server, path, body string) *http.Response {
		t.Helper()
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		return resp
	}

	// Create two repos.
	for _, name := range []string{"repo-x", "repo-y"} {
		r := doPost(adminSrv, "/repos", `{"owner":"default","name":"`+name+`"}`)
		r.Body.Close()
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("create %s: expected 201, got %d", name, r.StatusCode)
		}
	}

	// Assign alice as admin in repo-x only.
	req, _ := http.NewRequest(http.MethodPut, adminSrv.URL+"/repos/default/repo-x/-/roles/"+alice, strings.NewReader(`{"role":"admin"}`))
	req.Header.Set("Content-Type", "application/json")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT role: %v", err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("assign role: expected 200, got %d", r.StatusCode)
	}

	// Alice server (no bootstrap — must use explicit role).
	aliceSrv := httptest.NewServer(server.New(writeStore, database, alice, ""))
	t.Cleanup(aliceSrv.Close)

	// Alice can access repo-x.
	resp, err := http.Get(aliceSrv.URL + "/repos/default/repo-x/-/tree")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("alice repo-x: expected 200, got %d", resp.StatusCode)
	}

	// Alice cannot access repo-y (no role there).
	resp, err = http.Get(aliceSrv.URL + "/repos/default/repo-y/-/tree")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("alice repo-y: expected 403, got %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Purge integration tests
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Merge/rebase conflict body assertions (HTTP-level)
// ---------------------------------------------------------------------------

func TestIntegrationMerge_ConflictBody(t *testing.T) {
	t.Parallel()
	database := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, "test@example.com", "test@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	post := func(path, body string) *http.Response {
		t.Helper()
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		return resp
	}

	// Step 1: Commit to main — shared.txt (seq=1).
	r := post("/repos/default/default/-/commit", `{"branch":"main","message":"init","files":[{"path":"shared.txt","content":"b3JpZ2luYWw="}]}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("commit to main: expected 201, got %d", r.StatusCode)
	}

	// Step 2: Create branch (base=1).
	r = post("/repos/default/default/-/branch", `{"name":"feature/merge-conflict"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create branch: expected 201, got %d", r.StatusCode)
	}

	// Step 3: Main modifies shared.txt (seq=2).
	r = post("/repos/default/default/-/commit", `{"branch":"main","message":"main update","files":[{"path":"shared.txt","content":"bWFpbi11cGRhdGU="}]}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("commit to main: expected 201, got %d", r.StatusCode)
	}

	// Step 4: Branch modifies shared.txt (seq=3).
	r = post("/repos/default/default/-/commit", `{"branch":"feature/merge-conflict","message":"branch update","files":[{"path":"shared.txt","content":"YnJhbmNoLXVwZGF0ZQ=="}]}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("commit to branch: expected 201, got %d", r.StatusCode)
	}

	// Step 5: POST /merge — must return 409 with conflict body.
	r = post("/repos/default/default/-/merge", `{"branch":"feature/merge-conflict"}`)
	defer r.Body.Close()
	if r.StatusCode != http.StatusConflict {
		var errBody map[string]interface{}
		json.NewDecoder(r.Body).Decode(&errBody)
		t.Fatalf("POST /merge: expected 409, got %d; body: %v", r.StatusCode, errBody)
	}

	var conflictResp struct {
		Conflicts []struct {
			Path            string `json:"path"`
			MainVersionID   string `json:"main_version_id"`
			BranchVersionID string `json:"branch_version_id"`
		} `json:"conflicts"`
	}
	if err := json.NewDecoder(r.Body).Decode(&conflictResp); err != nil {
		t.Fatalf("decode conflict response: %v", err)
	}
	if len(conflictResp.Conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(conflictResp.Conflicts))
	}
	c := conflictResp.Conflicts[0]
	if c.Path != "shared.txt" {
		t.Errorf("expected conflict path shared.txt, got %q", c.Path)
	}
	if c.MainVersionID == "" {
		t.Error("expected non-empty main_version_id")
	}
	if c.BranchVersionID == "" {
		t.Error("expected non-empty branch_version_id")
	}
	if c.MainVersionID == c.BranchVersionID {
		t.Error("main_version_id and branch_version_id must differ")
	}
}

func TestIntegrationRebase_ConflictBody(t *testing.T) {
	t.Parallel()
	database := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, "test@example.com", "test@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	post := func(path, body string) *http.Response {
		t.Helper()
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		return resp
	}

	// Step 1: Commit to main — shared.txt (seq=1).
	r := post("/repos/default/default/-/commit", `{"branch":"main","message":"init","files":[{"path":"shared.txt","content":"b3JpZ2luYWw="}]}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("commit to main: expected 201, got %d", r.StatusCode)
	}

	// Step 2: Create branch (base=1).
	r = post("/repos/default/default/-/branch", `{"name":"feature/rebase-conflict"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create branch: expected 201, got %d", r.StatusCode)
	}

	// Step 3: Branch modifies shared.txt (seq=2).
	r = post("/repos/default/default/-/commit", `{"branch":"feature/rebase-conflict","message":"branch update","files":[{"path":"shared.txt","content":"YnJhbmNoLXVwZGF0ZQ=="}]}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("commit to branch: expected 201, got %d", r.StatusCode)
	}

	// Step 4: Main modifies shared.txt (seq=3).
	r = post("/repos/default/default/-/commit", `{"branch":"main","message":"main update","files":[{"path":"shared.txt","content":"bWFpbi11cGRhdGU="}]}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("commit to main: expected 201, got %d", r.StatusCode)
	}

	// Step 5: POST /rebase — must return 409 with conflict body.
	r = post("/repos/default/default/-/rebase", `{"branch":"feature/rebase-conflict"}`)
	defer r.Body.Close()
	if r.StatusCode != http.StatusConflict {
		var errBody map[string]interface{}
		json.NewDecoder(r.Body).Decode(&errBody)
		t.Fatalf("POST /rebase: expected 409, got %d; body: %v", r.StatusCode, errBody)
	}

	var conflictResp struct {
		Conflicts []struct {
			Path            string `json:"path"`
			MainVersionID   string `json:"main_version_id"`
			BranchVersionID string `json:"branch_version_id"`
		} `json:"conflicts"`
	}
	if err := json.NewDecoder(r.Body).Decode(&conflictResp); err != nil {
		t.Fatalf("decode conflict response: %v", err)
	}
	if len(conflictResp.Conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(conflictResp.Conflicts))
	}
	c := conflictResp.Conflicts[0]
	if c.Path != "shared.txt" {
		t.Errorf("expected conflict path shared.txt, got %q", c.Path)
	}
	if c.MainVersionID == "" {
		t.Error("expected non-empty main_version_id")
	}
	if c.BranchVersionID == "" {
		t.Error("expected non-empty branch_version_id")
	}
	if c.MainVersionID == c.BranchVersionID {
		t.Error("main_version_id and branch_version_id must differ")
	}
}

// ---------------------------------------------------------------------------
// Branch lifecycle edge cases (HTTP-level)
// ---------------------------------------------------------------------------

func TestIntegrationBranchLifecycle_MergeAfterMerge(t *testing.T) {
	t.Parallel()
	database := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, "test@example.com", "test@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	post := func(path, body string) *http.Response {
		t.Helper()
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		return resp
	}

	// Create branch and commit to it.
	r := post("/repos/default/default/-/branch", `{"name":"feature/once"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create branch: expected 201, got %d", r.StatusCode)
	}
	r = post("/repos/default/default/-/commit", `{"branch":"feature/once","message":"work","files":[{"path":"f.txt","content":"dGVzdA=="}]}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("commit: expected 201, got %d", r.StatusCode)
	}

	// First merge — should succeed.
	r = post("/repos/default/default/-/merge", `{"branch":"feature/once"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("first merge: expected 200, got %d", r.StatusCode)
	}

	// Second merge of already-merged branch — should fail with 409.
	r = post("/repos/default/default/-/merge", `{"branch":"feature/once"}`)
	defer r.Body.Close()
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("second merge: expected 409, got %d", r.StatusCode)
	}
}

func TestIntegrationBranchLifecycle_CommitAfterMerge(t *testing.T) {
	t.Parallel()
	database := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, "test@example.com", "test@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	post := func(path, body string) *http.Response {
		t.Helper()
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		return resp
	}

	// Create branch, commit, and merge it.
	r := post("/repos/default/default/-/branch", `{"name":"feature/commit-after-merge"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create branch: expected 201, got %d", r.StatusCode)
	}
	r = post("/repos/default/default/-/commit", `{"branch":"feature/commit-after-merge","message":"initial","files":[{"path":"g.txt","content":"dGVzdA=="}]}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("commit: expected 201, got %d", r.StatusCode)
	}
	r = post("/repos/default/default/-/merge", `{"branch":"feature/commit-after-merge"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("merge: expected 200, got %d", r.StatusCode)
	}

	// Commit to merged branch — must fail with 409.
	r = post("/repos/default/default/-/commit", `{"branch":"feature/commit-after-merge","message":"post-merge","files":[{"path":"g.txt","content":"dXBkYXRlZA=="}]}`)
	defer r.Body.Close()
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("commit after merge: expected 409, got %d", r.StatusCode)
	}
}

func TestIntegrationBranchLifecycle_RebaseMain(t *testing.T) {
	t.Parallel()
	database := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, "test@example.com", "test@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/repos/default/default/-/rebase", "application/json",
		strings.NewReader(`{"branch":"main"}`))
	if err != nil {
		t.Fatalf("POST /rebase: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("rebase main: expected 400, got %d", resp.StatusCode)
	}
}

func TestIntegrationPurge_FullFlow(t *testing.T) {
	t.Parallel()
	database := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, "test@example.com", "test@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	post := func(path, body string) *http.Response {
		t.Helper()
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		return resp
	}
	del := func(path string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodDelete, srv.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("DELETE %s: %v", path, err)
		}
		return resp
	}

	// Step 1: Commit to main.
	r := post("/repos/default/default/-/commit", `{"branch":"main","message":"init","files":[{"path":"main.txt","content":"bWFpbg=="}]}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("commit to main: expected 201, got %d", r.StatusCode)
	}

	// Step 2: Create and merge a feature branch (tests merged-branch cleanup).
	r = post("/repos/default/default/-/branch", `{"name":"feature/merged"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create merged branch: expected 201, got %d", r.StatusCode)
	}
	r = post("/repos/default/default/-/commit", `{"branch":"feature/merged","message":"work","files":[{"path":"merged.txt","content":"bWVyZ2Vk"}]}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("commit to merged branch: expected 201, got %d", r.StatusCode)
	}
	r = post("/repos/default/default/-/merge", `{"branch":"feature/merged"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("merge: expected 200, got %d", r.StatusCode)
	}

	// Step 3: Create and abandon a second branch with a unique file (orphan candidate).
	r = post("/repos/default/default/-/branch", `{"name":"feature/abandoned"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create abandoned branch: expected 201, got %d", r.StatusCode)
	}
	r = post("/repos/default/default/-/commit", `{"branch":"feature/abandoned","message":"abandoned work","files":[{"path":"abandoned-only.txt","content":"b3JwaGFu"}]}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("commit to abandoned branch: expected 201, got %d", r.StatusCode)
	}
	r = del("/repos/default/default/-/branch/feature/abandoned")
	r.Body.Close()
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("delete branch: expected 204, got %d", r.StatusCode)
	}

	// Step 4: Age both branches and their commits.
	for _, branch := range []string{"feature/merged", "feature/abandoned"} {
		if _, err := database.Exec(`UPDATE branches SET created_at = now() - interval '100 days' WHERE repo = 'default/default' AND name = $1`, branch); err != nil {
			t.Fatalf("age branch %s: %v", branch, err)
		}
		if _, err := database.Exec(`UPDATE commits SET created_at = now() - interval '100 days' WHERE repo = 'default/default' AND branch = $1`, branch); err != nil {
			t.Fatalf("age commits %s: %v", branch, err)
		}
	}

	// Step 5: POST /purge with 1d threshold.
	r = post("/repos/default/default/-/purge", `{"older_than":"1d"}`)
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		var errBody map[string]interface{}
		json.NewDecoder(r.Body).Decode(&errBody)
		t.Fatalf("purge: expected 200, got %d; body: %v", r.StatusCode, errBody)
	}
	var purgeResp struct {
		BranchesPurged     int64 `json:"branches_purged"`
		FileCommitsDeleted int64 `json:"file_commits_deleted"`
		CommitsDeleted     int64 `json:"commits_deleted"`
		DocumentsDeleted   int64 `json:"documents_deleted"`
	}
	if err := json.NewDecoder(r.Body).Decode(&purgeResp); err != nil {
		t.Fatalf("decode purge response: %v", err)
	}

	if purgeResp.BranchesPurged != 2 {
		t.Errorf("expected 2 branches purged, got %d", purgeResp.BranchesPurged)
	}
	if purgeResp.FileCommitsDeleted < 2 {
		t.Errorf("expected at least 2 file_commits deleted, got %d", purgeResp.FileCommitsDeleted)
	}
	if purgeResp.CommitsDeleted < 1 {
		t.Errorf("expected at least 1 commit deleted, got %d", purgeResp.CommitsDeleted)
	}
	// abandoned-only.txt was never merged, so its document is orphaned.
	if purgeResp.DocumentsDeleted < 1 {
		t.Errorf("expected at least 1 orphaned document deleted, got %d", purgeResp.DocumentsDeleted)
	}

	// Step 6: Verify both branch rows are gone.
	for _, branch := range []string{"feature/merged", "feature/abandoned"} {
		var count int
		if err := database.QueryRow("SELECT count(*) FROM branches WHERE repo = 'default/default' AND name = $1", branch).Scan(&count); err != nil {
			t.Fatalf("query branch %s: %v", branch, err)
		}
		if count != 0 {
			t.Errorf("expected branch %s to be deleted, found %d rows", branch, count)
		}
	}

	// Step 7: Verify main is unaffected — its tree has main.txt and merged.txt.
	treeResp, err := http.Get(srv.URL + "/repos/default/default/-/tree")
	if err != nil {
		t.Fatalf("GET /tree: %v", err)
	}
	defer treeResp.Body.Close()
	if treeResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /tree: expected 200, got %d", treeResp.StatusCode)
	}
	var entries []struct{ Path string `json:"path"` }
	if err := json.NewDecoder(treeResp.Body).Decode(&entries); err != nil {
		t.Fatalf("decode tree: %v", err)
	}
	// main should have main.txt and merged.txt (2 files).
	if len(entries) != 2 {
		t.Errorf("expected main tree to have 2 entries after purge, got %d", len(entries))
	}
	paths := make(map[string]bool)
	for _, e := range entries {
		paths[e.Path] = true
	}
	if !paths["main.txt"] {
		t.Error("main.txt must still be present on main")
	}
	if !paths["merged.txt"] {
		t.Error("merged.txt (from merged branch) must still be present on main")
	}
}

// TestIntegrationReleases tests the full lifecycle: create, list, get, ?at= resolution, and delete.
func TestIntegrationReleases(t *testing.T) {
	t.Parallel()
	database := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	seed(t, database)
	handler := server.New(dbpkg.NewStore(database), database, "test@example.com", "test@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Helper for authenticated requests.
	doReq := func(method, url, body string) *http.Response {
		t.Helper()
		var reqBody *strings.Reader
		if body != "" {
			reqBody = strings.NewReader(body)
		} else {
			reqBody = strings.NewReader("")
		}
		req, err := http.NewRequest(method, url, reqBody)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		return resp
	}

	base := srv.URL + "/repos/default/default/-"

	// CREATE: explicit sequence.
	resp := doReq(http.MethodPost, base+"/releases", `{"name":"v1.0","sequence":2,"body":"first release"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create release: expected 201, got %d", resp.StatusCode)
	}
	var created struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Sequence int64  `json:"sequence"`
		Body     string `json:"body"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.Name != "v1.0" {
		t.Errorf("expected name=v1.0, got %s", created.Name)
	}
	if created.Sequence != 2 {
		t.Errorf("expected sequence=2, got %d", created.Sequence)
	}
	if created.Body != "first release" {
		t.Errorf("expected body='first release', got %s", created.Body)
	}

	// CREATE: duplicate name should conflict.
	resp2 := doReq(http.MethodPost, base+"/releases", `{"name":"v1.0","sequence":1}`)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Errorf("duplicate release: expected 409, got %d", resp2.StatusCode)
	}

	// CREATE: default sequence (omit sequence → uses main head = 4 from seed).
	resp3 := doReq(http.MethodPost, base+"/releases", `{"name":"v2.0"}`)
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusCreated {
		t.Fatalf("create v2.0: expected 201, got %d", resp3.StatusCode)
	}
	var created2 struct {
		Sequence int64 `json:"sequence"`
	}
	if err := json.NewDecoder(resp3.Body).Decode(&created2); err != nil {
		t.Fatalf("decode v2.0 response: %v", err)
	}
	if created2.Sequence != 4 {
		t.Errorf("expected default sequence=4 (main head), got %d", created2.Sequence)
	}

	// LIST: should see both releases, newest first.
	listResp := doReq(http.MethodGet, base+"/releases", "")
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list releases: expected 200, got %d", listResp.StatusCode)
	}
	var listBody struct {
		Releases []struct {
			Name     string `json:"name"`
			Sequence int64  `json:"sequence"`
		} `json:"releases"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&listBody); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(listBody.Releases) != 2 {
		t.Fatalf("expected 2 releases, got %d", len(listBody.Releases))
	}
	// Newest first: v2.0 (created later) should come first.
	if listBody.Releases[0].Name != "v2.0" {
		t.Errorf("expected newest release first (v2.0), got %s", listBody.Releases[0].Name)
	}

	// GET: single release.
	getResp := doReq(http.MethodGet, base+"/releases/v1.0", "")
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("get release: expected 200, got %d", getResp.StatusCode)
	}
	var getBody struct {
		Name     string `json:"name"`
		Sequence int64  `json:"sequence"`
	}
	if err := json.NewDecoder(getResp.Body).Decode(&getBody); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	if getBody.Name != "v1.0" || getBody.Sequence != 2 {
		t.Errorf("unexpected get response: %+v", getBody)
	}

	// GET: not found.
	notFound := doReq(http.MethodGet, base+"/releases/nonexistent", "")
	defer notFound.Body.Close()
	if notFound.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for nonexistent release, got %d", notFound.StatusCode)
	}

	// ?at= RESOLUTION: use release name on tree endpoint.
	treeAtRelease := doReq(http.MethodGet, base+"/tree?at=v1.0", "")
	defer treeAtRelease.Body.Close()
	if treeAtRelease.StatusCode != http.StatusOK {
		t.Fatalf("tree?at=v1.0: expected 200, got %d", treeAtRelease.StatusCode)
	}
	var treeEntries []struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(treeAtRelease.Body).Decode(&treeEntries); err != nil {
		t.Fatalf("decode tree at release: %v", err)
	}
	// At sequence 2 (v1.0), tree should have hello.txt and world.txt (deleted.txt added at seq=3).
	treePaths := make(map[string]bool)
	for _, e := range treeEntries {
		treePaths[e.Path] = true
	}
	if !treePaths["hello.txt"] {
		t.Error("expected hello.txt in tree at v1.0")
	}
	if treePaths["deleted.txt"] {
		t.Error("deleted.txt should not be present in tree at v1.0 (added at seq=3)")
	}

	// ?at= RESOLUTION: invalid value returns 400.
	badAt := doReq(http.MethodGet, base+"/tree?at=nonexistent-release", "")
	defer badAt.Body.Close()
	if badAt.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for unknown ?at= value, got %d", badAt.StatusCode)
	}

	// ?at= RESOLUTION: integer still works.
	treeAtInt := doReq(http.MethodGet, base+"/tree?at=1", "")
	defer treeAtInt.Body.Close()
	if treeAtInt.StatusCode != http.StatusOK {
		t.Errorf("tree?at=1: expected 200, got %d", treeAtInt.StatusCode)
	}

	// DELETE: remove v1.0.
	delResp := doReq(http.MethodDelete, base+"/releases/v1.0", "")
	defer delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete release: expected 204, got %d", delResp.StatusCode)
	}

	// GET after delete: should 404.
	afterDel := doReq(http.MethodGet, base+"/releases/v1.0", "")
	defer afterDel.Body.Close()
	if afterDel.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 after delete, got %d", afterDel.StatusCode)
	}

	// LIST after delete: only v2.0 remains.
	listResp2 := doReq(http.MethodGet, base+"/releases", "")
	defer listResp2.Body.Close()
	var listBody2 struct {
		Releases []struct{ Name string `json:"name"` } `json:"releases"`
	}
	if err := json.NewDecoder(listResp2.Body).Decode(&listBody2); err != nil {
		t.Fatalf("decode list after delete: %v", err)
	}
	if len(listBody2.Releases) != 1 || listBody2.Releases[0].Name != "v2.0" {
		t.Errorf("expected only v2.0 remaining, got %+v", listBody2.Releases)
	}
}

// ---------------------------------------------------------------------------
// Issue integration tests
// ---------------------------------------------------------------------------

func TestIntegrationIssues(t *testing.T) {
	t.Parallel()
	database := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	seed(t, database)

	handler := server.New(dbpkg.NewStore(database), database, "alice@example.com", "alice@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	base := srv.URL + "/repos/default/default/-"

	doReq := func(method, url, body string) *http.Response {
		t.Helper()
		var req *http.Request
		var err error
		if body != "" {
			req, err = http.NewRequest(method, url, strings.NewReader(body))
		} else {
			req, err = http.NewRequest(method, url, nil)
		}
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		return resp
	}

	// CREATE issue.
	createResp := doReq(http.MethodPost, base+"/issues",
		`{"title":"bug: crash on startup","body":"commit:1 is the culprit"}`)
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create issue: expected 201, got %d", createResp.StatusCode)
	}
	var created struct {
		ID     string `json:"id"`
		Number int64  `json:"number"`
	}
	if err := json.NewDecoder(createResp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.Number != 1 {
		t.Errorf("expected issue number 1, got %d", created.Number)
	}

	// GET issue.
	getResp := doReq(http.MethodGet, base+"/issues/1", "")
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("get issue: expected 200, got %d", getResp.StatusCode)
	}
	var got struct {
		Number int64  `json:"number"`
		Title  string `json:"title"`
		State  string `json:"state"`
	}
	if err := json.NewDecoder(getResp.Body).Decode(&got); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	if got.Title != "bug: crash on startup" {
		t.Errorf("expected title 'bug: crash on startup', got %q", got.Title)
	}
	if got.State != "open" {
		t.Errorf("expected state open, got %q", got.State)
	}

	// LIST issues (default: open).
	listResp := doReq(http.MethodGet, base+"/issues", "")
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list issues: expected 200, got %d", listResp.StatusCode)
	}
	var issueList struct {
		Issues []struct {
			Number int64 `json:"number"`
		} `json:"issues"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&issueList); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(issueList.Issues) != 1 {
		t.Errorf("expected 1 issue, got %d", len(issueList.Issues))
	}

	// REFS: body mentioned commit:1, so a commit ref should have been auto-created.
	refsResp := doReq(http.MethodGet, base+"/issues/1/refs", "")
	defer refsResp.Body.Close()
	if refsResp.StatusCode != http.StatusOK {
		t.Fatalf("list refs: expected 200, got %d", refsResp.StatusCode)
	}
	var refList struct {
		Refs []struct {
			RefType string `json:"ref_type"`
			RefID   string `json:"ref_id"`
		} `json:"refs"`
	}
	if err := json.NewDecoder(refsResp.Body).Decode(&refList); err != nil {
		t.Fatalf("decode refs response: %v", err)
	}
	if len(refList.Refs) != 1 || refList.Refs[0].RefType != "commit" || refList.Refs[0].RefID != "1" {
		t.Errorf("expected one commit ref for seq 1, got %+v", refList.Refs)
	}

	// CLOSE issue.
	closeResp := doReq(http.MethodPost, base+"/issues/1/close", `{"reason":"completed"}`)
	defer closeResp.Body.Close()
	if closeResp.StatusCode != http.StatusOK {
		t.Fatalf("close issue: expected 200, got %d", closeResp.StatusCode)
	}
	var closed struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(closeResp.Body).Decode(&closed); err != nil {
		t.Fatalf("decode close response: %v", err)
	}
	if closed.State != "closed" {
		t.Errorf("expected state closed, got %q", closed.State)
	}

	// LIST: should be empty for default open filter.
	listClosed := doReq(http.MethodGet, base+"/issues", "")
	defer listClosed.Body.Close()
	if err := json.NewDecoder(listClosed.Body).Decode(&issueList); err != nil {
		t.Fatalf("decode list closed: %v", err)
	}
	if len(issueList.Issues) != 0 {
		t.Errorf("expected 0 open issues after close, got %d", len(issueList.Issues))
	}

	// REOPEN issue.
	reopenResp := doReq(http.MethodPost, base+"/issues/1/reopen", "")
	defer reopenResp.Body.Close()
	if reopenResp.StatusCode != http.StatusOK {
		t.Fatalf("reopen issue: expected 200, got %d", reopenResp.StatusCode)
	}
	var reopened struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(reopenResp.Body).Decode(&reopened); err != nil {
		t.Fatalf("decode reopen response: %v", err)
	}
	if reopened.State != "open" {
		t.Errorf("expected state open after reopen, got %q", reopened.State)
	}

	// CREATE comment.
	commentResp := doReq(http.MethodPost, base+"/issues/1/comments", `{"body":"this is a comment"}`)
	defer commentResp.Body.Close()
	if commentResp.StatusCode != http.StatusCreated {
		t.Fatalf("create comment: expected 201, got %d", commentResp.StatusCode)
	}
	var commentCreated struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(commentResp.Body).Decode(&commentCreated); err != nil {
		t.Fatalf("decode comment create: %v", err)
	}
	if commentCreated.ID == "" {
		t.Error("expected non-empty comment ID")
	}

	// LIST comments.
	listCommentsResp := doReq(http.MethodGet, base+"/issues/1/comments", "")
	defer listCommentsResp.Body.Close()
	if listCommentsResp.StatusCode != http.StatusOK {
		t.Fatalf("list comments: expected 200, got %d", listCommentsResp.StatusCode)
	}
	var commentList struct {
		Comments []struct {
			ID   string `json:"id"`
			Body string `json:"body"`
		} `json:"comments"`
	}
	if err := json.NewDecoder(listCommentsResp.Body).Decode(&commentList); err != nil {
		t.Fatalf("decode comment list: %v", err)
	}
	if len(commentList.Comments) != 1 {
		t.Errorf("expected 1 comment, got %d", len(commentList.Comments))
	}

	// UPDATE comment.
	updateCommentResp := doReq(http.MethodPatch,
		base+"/issues/1/comments/"+commentCreated.ID,
		`{"body":"updated comment"}`)
	defer updateCommentResp.Body.Close()
	if updateCommentResp.StatusCode != http.StatusOK {
		t.Fatalf("update comment: expected 200, got %d", updateCommentResp.StatusCode)
	}

	// ADD explicit ref.
	addRefResp := doReq(http.MethodPost, base+"/issues/1/refs",
		`{"ref_type":"commit","ref_id":"2"}`)
	defer addRefResp.Body.Close()
	if addRefResp.StatusCode != http.StatusCreated {
		t.Fatalf("add ref: expected 201, got %d", addRefResp.StatusCode)
	}

	// LIST refs: now 2 (commit:1 from body + commit:2 from explicit).
	refsResp2 := doReq(http.MethodGet, base+"/issues/1/refs", "")
	defer refsResp2.Body.Close()
	var refList2 struct {
		Refs []struct{ RefType, RefID string } `json:"refs"`
	}
	if err := json.NewDecoder(refsResp2.Body).Decode(&refList2); err != nil {
		t.Fatalf("decode refs2: %v", err)
	}
	if len(refList2.Refs) != 2 {
		t.Errorf("expected 2 refs, got %d", len(refList2.Refs))
	}

	// COMMIT issues reverse lookup.
	commitIssuesResp := doReq(http.MethodGet, base+"/commit/1/issues", "")
	defer commitIssuesResp.Body.Close()
	if commitIssuesResp.StatusCode != http.StatusOK {
		t.Fatalf("commit issues: expected 200, got %d", commitIssuesResp.StatusCode)
	}
	var commitIssueList struct {
		Issues []struct{ Number int64 `json:"number"` } `json:"issues"`
	}
	if err := json.NewDecoder(commitIssuesResp.Body).Decode(&commitIssueList); err != nil {
		t.Fatalf("decode commit issues: %v", err)
	}
	if len(commitIssueList.Issues) != 1 || commitIssueList.Issues[0].Number != 1 {
		t.Errorf("expected issue 1 in commit/1/issues, got %+v", commitIssueList.Issues)
	}

	// DELETE comment.
	delCommentResp := doReq(http.MethodDelete,
		base+"/issues/1/comments/"+commentCreated.ID, "")
	defer delCommentResp.Body.Close()
	if delCommentResp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete comment: expected 204, got %d", delCommentResp.StatusCode)
	}

	// GET: 404 for unknown issue.
	notFoundResp := doReq(http.MethodGet, base+"/issues/999", "")
	defer notFoundResp.Body.Close()
	if notFoundResp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for unknown issue, got %d", notFoundResp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// seedWithRoles seeds the database with test data plus RBAC roles for alice,
// bob, and carol.
// ---------------------------------------------------------------------------

func seedWithRoles(t *testing.T, database *sql.DB) {
	t.Helper()
	seed(t, database)
	roleStmts := []string{
		`INSERT INTO roles (repo, identity, role) VALUES ('default/default', 'alice@example.com', 'writer')`,
		`INSERT INTO roles (repo, identity, role) VALUES ('default/default', 'bob@example.com', 'writer')`,
		`INSERT INTO roles (repo, identity, role) VALUES ('default/default', 'carol@example.com', 'maintainer')`,
	}
	for _, stmt := range roleStmts {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("seedWithRoles: %v\nstatement: %s", err, stmt)
		}
	}
}

// ---------------------------------------------------------------------------
// TEST-1: Cross-identity authz tests
// ---------------------------------------------------------------------------

func TestIntegrationIssueAuthz(t *testing.T) {
	t.Parallel()
	database := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	seedWithRoles(t, database)

	makeServer := func(identity string) *httptest.Server {
		h := server.New(dbpkg.NewStore(database), database, identity, "")
		return httptest.NewServer(h)
	}

	aliceSrv := makeServer("alice@example.com")
	defer aliceSrv.Close()
	bobSrv := makeServer("bob@example.com")
	defer bobSrv.Close()
	carolSrv := makeServer("carol@example.com")
	defer carolSrv.Close()

	base := "/repos/default/default/-"

	doReq := func(srv *httptest.Server, method, path, body string) *http.Response {
		t.Helper()
		var req *http.Request
		var err error
		if body != "" {
			req, err = http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		} else {
			req, err = http.NewRequest(method, srv.URL+path, nil)
		}
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return resp
	}

	// Alice creates an issue.
	createResp := doReq(aliceSrv, http.MethodPost, base+"/issues",
		`{"title":"alice's issue","body":"body text"}`)
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("alice create issue: expected 201, got %d", createResp.StatusCode)
	}
	var created struct {
		Number int64 `json:"number"`
	}
	if err := json.NewDecoder(createResp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	_ = created
	issueURL := base + "/issues/1"

	// Bob cannot update alice's issue.
	updateResp := doReq(bobSrv, http.MethodPatch, issueURL, `{"title":"bob changed it"}`)
	defer updateResp.Body.Close()
	if updateResp.StatusCode != http.StatusForbidden {
		t.Errorf("bob update alice's issue: expected 403, got %d", updateResp.StatusCode)
	}

	// Bob cannot close alice's issue.
	closeResp := doReq(bobSrv, http.MethodPost, issueURL+"/close", `{"reason":"completed"}`)
	defer closeResp.Body.Close()
	if closeResp.StatusCode != http.StatusForbidden {
		t.Errorf("bob close alice's issue: expected 403, got %d", closeResp.StatusCode)
	}

	// Alice closes her own issue so bob can try to reopen it.
	aliceCloseResp := doReq(aliceSrv, http.MethodPost, issueURL+"/close", `{"reason":"completed"}`)
	defer aliceCloseResp.Body.Close()
	if aliceCloseResp.StatusCode != http.StatusOK {
		t.Fatalf("alice close issue: expected 200, got %d", aliceCloseResp.StatusCode)
	}

	// Bob cannot reopen alice's closed issue.
	reopenResp := doReq(bobSrv, http.MethodPost, issueURL+"/reopen", "")
	defer reopenResp.Body.Close()
	if reopenResp.StatusCode != http.StatusForbidden {
		t.Errorf("bob reopen alice's issue: expected 403, got %d", reopenResp.StatusCode)
	}

	// Reopen via alice for comment tests.
	aliceReopenResp := doReq(aliceSrv, http.MethodPost, issueURL+"/reopen", "")
	defer aliceReopenResp.Body.Close()
	if aliceReopenResp.StatusCode != http.StatusOK {
		t.Fatalf("alice reopen issue: expected 200, got %d", aliceReopenResp.StatusCode)
	}

	// Alice creates a comment.
	aliceCommentResp := doReq(aliceSrv, http.MethodPost, issueURL+"/comments",
		`{"body":"alice's comment"}`)
	defer aliceCommentResp.Body.Close()
	if aliceCommentResp.StatusCode != http.StatusCreated {
		t.Fatalf("alice create comment: expected 201, got %d", aliceCommentResp.StatusCode)
	}
	var aliceComment struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(aliceCommentResp.Body).Decode(&aliceComment); err != nil {
		t.Fatalf("decode alice comment: %v", err)
	}

	// Bob cannot edit alice's comment.
	bobEditAliceResp := doReq(bobSrv, http.MethodPatch,
		issueURL+"/comments/"+aliceComment.ID, `{"body":"bob changed it"}`)
	defer bobEditAliceResp.Body.Close()
	if bobEditAliceResp.StatusCode != http.StatusForbidden {
		t.Errorf("bob edit alice's comment: expected 403, got %d", bobEditAliceResp.StatusCode)
	}

	// Bob creates his own comment and can edit it.
	bobCommentResp := doReq(bobSrv, http.MethodPost, issueURL+"/comments",
		`{"body":"bob's comment"}`)
	defer bobCommentResp.Body.Close()
	if bobCommentResp.StatusCode != http.StatusCreated {
		t.Fatalf("bob create comment: expected 201, got %d", bobCommentResp.StatusCode)
	}
	var bobComment struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(bobCommentResp.Body).Decode(&bobComment); err != nil {
		t.Fatalf("decode bob comment: %v", err)
	}

	bobEditOwnResp := doReq(bobSrv, http.MethodPatch,
		issueURL+"/comments/"+bobComment.ID, `{"body":"bob updated his own"}`)
	defer bobEditOwnResp.Body.Close()
	if bobEditOwnResp.StatusCode != http.StatusOK {
		t.Errorf("bob edit own comment: expected 200, got %d", bobEditOwnResp.StatusCode)
	}

	// Carol (maintainer) can close alice's issue.
	carolCloseResp := doReq(carolSrv, http.MethodPost, issueURL+"/close", `{"reason":"completed"}`)
	defer carolCloseResp.Body.Close()
	if carolCloseResp.StatusCode != http.StatusOK {
		t.Errorf("carol close alice's issue: expected 200, got %d", carolCloseResp.StatusCode)
	}

	// Carol (maintainer) can reopen it.
	carolReopenResp := doReq(carolSrv, http.MethodPost, issueURL+"/reopen", "")
	defer carolReopenResp.Body.Close()
	if carolReopenResp.StatusCode != http.StatusOK {
		t.Errorf("carol reopen issue: expected 200, got %d", carolReopenResp.StatusCode)
	}

	// Carol (maintainer) can update alice's issue.
	carolUpdateResp := doReq(carolSrv, http.MethodPatch, issueURL, `{"title":"carol updated"}`)
	defer carolUpdateResp.Body.Close()
	if carolUpdateResp.StatusCode != http.StatusOK {
		t.Errorf("carol update alice's issue: expected 200, got %d", carolUpdateResp.StatusCode)
	}

	// Carol (maintainer) can delete alice's comment.
	carolDelResp := doReq(carolSrv, http.MethodDelete,
		issueURL+"/comments/"+aliceComment.ID, "")
	defer carolDelResp.Body.Close()
	if carolDelResp.StatusCode != http.StatusNoContent {
		t.Errorf("carol delete alice's comment: expected 204, got %d", carolDelResp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// TEST-2: Idempotency tests
// ---------------------------------------------------------------------------

func TestIntegrationIssueIdempotency(t *testing.T) {
	t.Parallel()
	database := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	seed(t, database)

	handler := server.New(dbpkg.NewStore(database), database, "alice@example.com", "alice@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	base := srv.URL + "/repos/default/default/-"

	doReq := func(method, url, body string) *http.Response {
		t.Helper()
		var req *http.Request
		var err error
		if body != "" {
			req, err = http.NewRequest(method, url, strings.NewReader(body))
		} else {
			req, err = http.NewRequest(method, url, nil)
		}
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		return resp
	}

	// Create an issue.
	createResp := doReq(http.MethodPost, base+"/issues", `{"title":"idempotency test","body":""}`)
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create issue: expected 201, got %d", createResp.StatusCode)
	}
	issueURL := base + "/issues/1"

	// Reopen an open issue: should return 409.
	reopenOpenResp := doReq(http.MethodPost, issueURL+"/reopen", "")
	defer reopenOpenResp.Body.Close()
	if reopenOpenResp.StatusCode != http.StatusConflict {
		t.Errorf("reopen open issue: expected 409, got %d", reopenOpenResp.StatusCode)
	}

	// Close issue: first close succeeds.
	close1Resp := doReq(http.MethodPost, issueURL+"/close", `{"reason":"completed"}`)
	defer close1Resp.Body.Close()
	if close1Resp.StatusCode != http.StatusOK {
		t.Fatalf("first close: expected 200, got %d", close1Resp.StatusCode)
	}

	// Close issue: second close should return 409.
	close2Resp := doReq(http.MethodPost, issueURL+"/close", `{"reason":"completed"}`)
	defer close2Resp.Body.Close()
	if close2Resp.StatusCode != http.StatusConflict {
		t.Errorf("second close: expected 409, got %d", close2Resp.StatusCode)
	}

	// Reopen closed issue: succeeds.
	reopen1Resp := doReq(http.MethodPost, issueURL+"/reopen", "")
	defer reopen1Resp.Body.Close()
	if reopen1Resp.StatusCode != http.StatusOK {
		t.Fatalf("reopen closed issue: expected 200, got %d", reopen1Resp.StatusCode)
	}

	// Reopen again: should return 409.
	reopen2Resp := doReq(http.MethodPost, issueURL+"/reopen", "")
	defer reopen2Resp.Body.Close()
	if reopen2Resp.StatusCode != http.StatusConflict {
		t.Errorf("second reopen: expected 409, got %d", reopen2Resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// TEST-4: PATCH /issues/:number
// ---------------------------------------------------------------------------

func TestIntegrationIssueUpdate(t *testing.T) {
	t.Parallel()
	database := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	seed(t, database)

	handler := server.New(dbpkg.NewStore(database), database, "alice@example.com", "alice@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	base := srv.URL + "/repos/default/default/-"

	doReq := func(method, url, body string) *http.Response {
		t.Helper()
		var req *http.Request
		var err error
		if body != "" {
			req, err = http.NewRequest(method, url, strings.NewReader(body))
		} else {
			req, err = http.NewRequest(method, url, nil)
		}
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		return resp
	}

	// Create an issue.
	createResp := doReq(http.MethodPost, base+"/issues",
		`{"title":"original title","body":"original body"}`)
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create issue: expected 201, got %d", createResp.StatusCode)
	}
	var created struct {
		Number int64 `json:"number"`
	}
	if err := json.NewDecoder(createResp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	issueURL := base + "/issues/1"

	// PATCH both title and body.
	patchResp := doReq(http.MethodPatch, issueURL,
		`{"title":"updated title","body":"updated body"}`)
	defer patchResp.Body.Close()
	if patchResp.StatusCode != http.StatusOK {
		t.Fatalf("patch issue: expected 200, got %d", patchResp.StatusCode)
	}
	var patched struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := json.NewDecoder(patchResp.Body).Decode(&patched); err != nil {
		t.Fatalf("decode patch response: %v", err)
	}
	if patched.Title != "updated title" {
		t.Errorf("patch response: expected title 'updated title', got %q", patched.Title)
	}
	if patched.Body != "updated body" {
		t.Errorf("patch response: expected body 'updated body', got %q", patched.Body)
	}

	// GET: verify the update persisted.
	getResp := doReq(http.MethodGet, issueURL, "")
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("get issue: expected 200, got %d", getResp.StatusCode)
	}
	var got struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := json.NewDecoder(getResp.Body).Decode(&got); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	if got.Title != "updated title" {
		t.Errorf("get after patch: expected title 'updated title', got %q", got.Title)
	}
	if got.Body != "updated body" {
		t.Errorf("get after patch: expected body 'updated body', got %q", got.Body)
	}

	// PATCH only title (partial update): body should remain unchanged.
	patch2Resp := doReq(http.MethodPatch, issueURL, `{"title":"title only"}`)
	defer patch2Resp.Body.Close()
	if patch2Resp.StatusCode != http.StatusOK {
		t.Fatalf("partial patch: expected 200, got %d", patch2Resp.StatusCode)
	}
	var patched2 struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := json.NewDecoder(patch2Resp.Body).Decode(&patched2); err != nil {
		t.Fatalf("decode partial patch response: %v", err)
	}
	if patched2.Title != "title only" {
		t.Errorf("partial patch: expected title 'title only', got %q", patched2.Title)
	}
	if patched2.Body != "updated body" {
		t.Errorf("partial patch: expected body unchanged 'updated body', got %q", patched2.Body)
	}
}

func TestIntegrationRetryChecksFlow(t *testing.T) {
	t.Parallel()
	database := testutil.TestDBFromShared(t, sharedAdminDSN, dbpkg.RunMigrations)
	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, "ci-bot@example.com", "ci-bot@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	post := func(path, body string) *http.Response {
		t.Helper()
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		return resp
	}

	// Create a failed check run at attempt 1.
	r := post("/repos/default/default/-/check", `{"branch":"main","check_name":"ci/build","status":"failed"}`)
	defer r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create check: expected 201, got %d", r.StatusCode)
	}

	// Retry all failed checks.
	retryR := post("/repos/default/default/-/checks/retry", `{"branch":"main","sequence":0}`)
	defer retryR.Body.Close()
	if retryR.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(retryR.Body)
		t.Fatalf("retry: expected 202, got %d: %s", retryR.StatusCode, body)
	}
	var retryResp struct {
		Attempt int16 `json:"attempt"`
	}
	if err := json.NewDecoder(retryR.Body).Decode(&retryResp); err != nil {
		t.Fatalf("decode retry response: %v", err)
	}
	if retryResp.Attempt != 2 {
		t.Errorf("expected attempt 2, got %d", retryResp.Attempt)
	}

	// GET checks without history: should show attempt 2 (pending).
	getResp, err := http.Get(srv.URL + "/repos/default/default/-/branch/main/checks")
	if err != nil {
		t.Fatalf("GET checks: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET checks: expected 200, got %d", getResp.StatusCode)
	}
	var checks []struct {
		CheckName string `json:"check_name"`
		Status    string `json:"status"`
		Attempt   int16  `json:"attempt"`
	}
	if err := json.NewDecoder(getResp.Body).Decode(&checks); err != nil {
		t.Fatalf("decode checks: %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("expected 1 check run (latest attempt only), got %d", len(checks))
	}
	if checks[0].Attempt != 2 {
		t.Errorf("expected attempt 2, got %d", checks[0].Attempt)
	}
	if checks[0].Status != "pending" {
		t.Errorf("expected pending, got %q", checks[0].Status)
	}

	// GET checks with history=true: should show both attempts.
	histResp, err := http.Get(srv.URL + "/repos/default/default/-/branch/main/checks?history=true")
	if err != nil {
		t.Fatalf("GET checks history: %v", err)
	}
	defer histResp.Body.Close()
	if histResp.StatusCode != http.StatusOK {
		t.Fatalf("GET checks history: expected 200, got %d", histResp.StatusCode)
	}
	var allChecks []struct {
		Attempt int16 `json:"attempt"`
	}
	if err := json.NewDecoder(histResp.Body).Decode(&allChecks); err != nil {
		t.Fatalf("decode checks history: %v", err)
	}
	if len(allChecks) != 2 {
		t.Fatalf("expected 2 check runs with history=true, got %d", len(allChecks))
	}
}
