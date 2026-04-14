package server_test

import (
	"database/sql"
	"encoding/json"
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
		// Ensure the 'default' repo row exists so validateRepo passes.
		`INSERT INTO repos (name) VALUES ('default') ON CONFLICT DO NOTHING`,
		`INSERT INTO documents (version_id, path, content, content_hash, created_by)
		 VALUES ('aaaaaaaa-0000-0000-0000-000000000001', 'hello.txt', 'hello world', 'hash_hello_v1', 'alice')`,
		`INSERT INTO documents (version_id, path, content, content_hash, created_by)
		 VALUES ('aaaaaaaa-0000-0000-0000-000000000002', 'hello.txt', 'hello world v2', 'hash_hello_v2', 'alice')`,
		`INSERT INTO documents (version_id, path, content, content_hash, created_by)
		 VALUES ('aaaaaaaa-0000-0000-0000-000000000003', 'world.txt', 'the world', 'hash_world_v1', 'bob')`,
		`INSERT INTO documents (version_id, path, content, content_hash, created_by)
		 VALUES ('aaaaaaaa-0000-0000-0000-000000000004', 'deleted.txt', 'gone soon', 'hash_deleted_v1', 'alice')`,
		// Insert commits rows for global sequence allocation.
		`INSERT INTO commits (sequence, branch, message, author) OVERRIDING SYSTEM VALUE VALUES
		 (1, 'main', 'initial commit', 'alice'),
		 (2, 'main', 'update hello',   'alice'),
		 (3, 'main', 'add deleted',    'bob'),
		 (4, 'main', 'remove deleted', 'bob')`,
		`SELECT setval('commits_sequence_seq', 4, true)`,
		`INSERT INTO file_commits (commit_id, sequence, path, version_id, branch)
		 VALUES ('cccccccc-0000-0000-0000-000000000001', 1, 'hello.txt', 'aaaaaaaa-0000-0000-0000-000000000001', 'main')`,
		`INSERT INTO file_commits (commit_id, sequence, path, version_id, branch)
		 VALUES ('cccccccc-0000-0000-0000-000000000002', 1, 'world.txt', 'aaaaaaaa-0000-0000-0000-000000000003', 'main')`,
		`INSERT INTO file_commits (commit_id, sequence, path, version_id, branch)
		 VALUES ('cccccccc-0000-0000-0000-000000000003', 2, 'hello.txt', 'aaaaaaaa-0000-0000-0000-000000000002', 'main')`,
		`INSERT INTO file_commits (commit_id, sequence, path, version_id, branch)
		 VALUES ('cccccccc-0000-0000-0000-000000000004', 3, 'deleted.txt', 'aaaaaaaa-0000-0000-0000-000000000004', 'main')`,
		`INSERT INTO file_commits (commit_id, sequence, path, version_id, branch)
		 VALUES ('cccccccc-0000-0000-0000-000000000005', 4, 'deleted.txt', NULL, 'main')`,
		`UPDATE branches SET head_sequence = 4 WHERE name = 'main'`,
	}

	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\nstatement: %s", err, stmt)
		}
	}
}

func TestIntegrationTreeEndpoint(t *testing.T) {
	db := testutil.TestDB(t, dbpkg.MigrationSQL)
	seed(t, db)
	handler := server.New(dbpkg.NewStore(db), db, "test@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/repos/default/tree")
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
	db := testutil.TestDB(t, dbpkg.MigrationSQL)
	seed(t, db)
	handler := server.New(dbpkg.NewStore(db), db, "test@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	t.Run("get file content", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/repos/default/file/hello.txt")
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
		resp, err := http.Get(srv.URL + "/repos/default/file/nope.txt")
		if err != nil {
			t.Fatalf("GET /file/nope.txt: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", resp.StatusCode)
		}
	})

	t.Run("file history", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/repos/default/file/hello.txt/history")
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
		resp, err := http.Get(srv.URL + "/repos/default/file/hello.txt?at=1")
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
	database := testutil.TestDB(t, dbpkg.MigrationSQL)
	seed(t, database)

	// Add feature branch.
	if _, err := database.Exec(
		"INSERT INTO branches (name, head_sequence, base_sequence, status) VALUES ('feature/a', 5, 2, 'active')",
	); err != nil {
		t.Fatalf("seed branch: %v", err)
	}

	handler := server.New(dbpkg.NewStore(database), database, "test@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	t.Run("list all branches", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/repos/default/branches")
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
		resp, err := http.Get(srv.URL + "/repos/default/branches?status=active")
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
	database := testutil.TestDB(t, dbpkg.MigrationSQL)
	seed(t, database)

	// Create a branch with changes.
	stmts := []string{
		`INSERT INTO branches (name, head_sequence, base_sequence, status) VALUES ('feature/diff', 5, 2, 'active')`,
		`INSERT INTO documents (version_id, path, content, content_hash, created_by)
		 VALUES ('aaaaaaaa-0000-0000-0000-000000000010', 'new.txt', 'new file', 'hash_new', 'carol')`,
		`INSERT INTO commits (sequence, branch, message, author) OVERRIDING SYSTEM VALUE VALUES
		 (5, 'feature/diff', 'add new', 'carol')`,
		`SELECT setval('commits_sequence_seq', 5, true)`,
		`INSERT INTO file_commits (commit_id, sequence, path, version_id, branch)
		 VALUES ('cccccccc-0000-0000-0000-000000000010', 5, 'new.txt', 'aaaaaaaa-0000-0000-0000-000000000010', 'feature/diff')`,
	}
	for _, stmt := range stmts {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt)
		}
	}

	handler := server.New(dbpkg.NewStore(database), database, "test@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	t.Run("diff with branch", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/repos/default/diff?branch=feature/diff")
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
		resp, err := http.Get(srv.URL + "/repos/default/diff")
		if err != nil {
			t.Fatalf("GET /diff: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", resp.StatusCode)
		}
	})

	t.Run("diff nonexistent branch", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/repos/default/diff?branch=nonexistent")
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
	db := testutil.TestDB(t, dbpkg.MigrationSQL)
	seed(t, db)
	handler := server.New(nil, db, "test@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	t.Run("existing commit", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/repos/default/commit/1")
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
		resp, err := http.Get(srv.URL + "/repos/default/commit/999")
		if err != nil {
			t.Fatalf("GET /commit/999: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", resp.StatusCode)
		}
	})

	t.Run("invalid sequence", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/repos/default/commit/abc")
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
	database := testutil.TestDB(t, dbpkg.MigrationSQL)
	seed(t, database)

	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, "test@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Seed a branch to delete.
	if _, err := database.Exec(
		"INSERT INTO branches (name, head_sequence, base_sequence, status) VALUES ('feature/to-delete', 4, 4, 'active')",
	); err != nil {
		t.Fatalf("seed branch: %v", err)
	}

	t.Run("delete active branch returns 204", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/repos/default/branch/feature/to-delete", nil)
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
		req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/repos/default/branch/main", nil)
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
		req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/repos/default/branch/nonexistent", nil)
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
		req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/repos/default/branch/feature/to-delete", nil)
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
	database := testutil.TestDB(t, dbpkg.MigrationSQL)
	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, "test@example.com")
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
	r := post("/repos/default/commit", `{"branch":"main","message":"initial","author":"alice","files":[{"path":"base.txt","content":"YmFzZQ=="}]}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("POST /commit to main: expected 201, got %d", r.StatusCode)
	}

	// Step 2: Create branch (base=1, head=1).
	r = post("/repos/default/branch", `{"name":"feature/rebase-flow"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("POST /branch: expected 201, got %d", r.StatusCode)
	}

	// Step 3: Commit to branch (seq=2, adds "branch.txt").
	r = post("/repos/default/commit", `{"branch":"feature/rebase-flow","message":"branch work","author":"bob","files":[{"path":"branch.txt","content":"YnJhbmNo"}]}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("POST /commit to branch: expected 201, got %d", r.StatusCode)
	}

	// Step 4: Advance main (seq=3, adds "other.txt").
	r = post("/repos/default/commit", `{"branch":"main","message":"main advance","author":"alice","files":[{"path":"other.txt","content":"b3RoZXI="}]}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("POST /commit to advance main: expected 201, got %d", r.StatusCode)
	}

	// Step 5: GET /diff — should show main_changes before rebase.
	diffResp, err := http.Get(srv.URL + "/repos/default/diff?branch=feature/rebase-flow")
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
	r = post("/repos/default/rebase", `{"branch":"feature/rebase-flow","author":"bob"}`)
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
	diffResp2, err := http.Get(srv.URL + "/repos/default/diff?branch=feature/rebase-flow")
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
	r = post("/repos/default/merge", `{"branch":"feature/rebase-flow","author":"alice"}`)
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
	// No devIdentity → real IAP auth enforced.
	handler := server.New(nil, nil, "")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// POST /repos/default/commit without X-Goog-IAP-JWT-Assertion must return 401.
	resp, err := http.Post(srv.URL+"/repos/default/commit", "application/json",
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
	const identity = "alice@example.com"
	database := testutil.TestDB(t, dbpkg.MigrationSQL)
	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, identity)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// POST /repos/default/commit with a different author in the body — it must be ignored.
	resp, err := http.Post(srv.URL+"/repos/default/commit", "application/json",
		strings.NewReader(`{"branch":"main","message":"test commit","author":"not-alice@example.com","files":[{"path":"hello.txt","content":"aGVsbG8="}]}`))
	if err != nil {
		t.Fatalf("POST /commit: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /commit: expected 201, got %d", resp.StatusCode)
	}

	// GET /repos/default/commit/1 and verify the author is the identity, not the body value.
	getResp, err := http.Get(srv.URL + "/repos/default/commit/1")
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
	database := testutil.TestDB(t, dbpkg.MigrationSQL)
	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, "test@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/repos", "application/json",
		strings.NewReader(`{"name":"myrepo"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
}

func TestHandleCreateRepo_Duplicate(t *testing.T) {
	database := testutil.TestDB(t, dbpkg.MigrationSQL)
	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, "test@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	for i, wantCode := range []int{http.StatusCreated, http.StatusConflict} {
		resp, err := http.Post(srv.URL+"/repos", "application/json",
			strings.NewReader(`{"name":"dup"}`))
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
	database := testutil.TestDB(t, dbpkg.MigrationSQL)
	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, "test@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Create a repo first.
	resp, err := http.Post(srv.URL+"/repos", "application/json",
		strings.NewReader(`{"name":"todel"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Delete it.
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/repos/todel", nil)
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
	database := testutil.TestDB(t, dbpkg.MigrationSQL)
	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, "test@example.com")
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
	database := testutil.TestDB(t, dbpkg.MigrationSQL)
	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, "test@example.com")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Create a repo to add alongside the seeded 'default'.
	resp, err := http.Post(srv.URL+"/repos", "application/json",
		strings.NewReader(`{"name":"extra"}`))
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
	database := testutil.TestDB(t, dbpkg.MigrationSQL)
	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, "test@example.com")
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
		r := post("/repos", `{"name":"`+name+`"}`)
		r.Body.Close()
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("create repo %s: expected 201, got %d", name, r.StatusCode)
		}
	}

	// Commit unique files to each repo.
	r := post("/repos/alpha/commit", `{"branch":"main","message":"alpha init","files":[{"path":"alpha.txt","content":"YWxwaGE="}]}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("commit alpha: expected 201, got %d", r.StatusCode)
	}

	r = post("/repos/beta/commit", `{"branch":"main","message":"beta init","files":[{"path":"beta.txt","content":"YmV0YQ=="}]}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("commit beta: expected 201, got %d", r.StatusCode)
	}

	// GET /repos/alpha/tree must NOT contain beta.txt.
	resp, err := http.Get(srv.URL + "/repos/alpha/tree")
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

	// GET /repos/beta/tree must NOT contain alpha.txt.
	resp2, err := http.Get(srv.URL + "/repos/beta/tree")
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
	database := testutil.TestDB(t, dbpkg.MigrationSQL)
	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, "test@example.com")
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
	r := post("/repos", `{"name":"cleanup-test"}`)
	r.Body.Close()
	r = post("/repos/cleanup-test/commit", `{"branch":"main","message":"init","files":[{"path":"f.txt","content":"dGVzdA=="}]}`)
	r.Body.Close()

	// Verify tree is non-empty.
	resp, err := http.Get(srv.URL + "/repos/cleanup-test/tree")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 before delete, got %d", resp.StatusCode)
	}

	// Delete the repo.
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/repos/cleanup-test", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}

	// Repo should no longer exist.
	resp, err = http.Get(srv.URL + "/repos/cleanup-test")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", resp.StatusCode)
	}
}
