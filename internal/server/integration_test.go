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
	db := testutil.TestDB(t, dbpkg.RunMigrations)
	seed(t, db)
	handler := server.New(dbpkg.NewStore(db), db, "test@example.com", "test@example.com")
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
	db := testutil.TestDB(t, dbpkg.RunMigrations)
	seed(t, db)
	handler := server.New(dbpkg.NewStore(db), db, "test@example.com", "test@example.com")
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
	database := testutil.TestDB(t, dbpkg.RunMigrations)
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
	database := testutil.TestDB(t, dbpkg.RunMigrations)
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

	handler := server.New(dbpkg.NewStore(database), database, "test@example.com", "test@example.com")
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
	db := testutil.TestDB(t, dbpkg.RunMigrations)
	seed(t, db)
	handler := server.New(nil, db, "test@example.com", "")
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
	database := testutil.TestDB(t, dbpkg.RunMigrations)
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
	database := testutil.TestDB(t, dbpkg.RunMigrations)
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
	handler := server.New(nil, nil, "", "")
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
	database := testutil.TestDB(t, dbpkg.RunMigrations)
	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, identity, identity)
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
	database := testutil.TestDB(t, dbpkg.RunMigrations)
	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, "test@example.com", "test@example.com")
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
	database := testutil.TestDB(t, dbpkg.RunMigrations)
	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, "test@example.com", "test@example.com")
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
	database := testutil.TestDB(t, dbpkg.RunMigrations)
	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, "test@example.com", "test@example.com")
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
	database := testutil.TestDB(t, dbpkg.RunMigrations)
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
	database := testutil.TestDB(t, dbpkg.RunMigrations)
	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, "test@example.com", "test@example.com")
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
	database := testutil.TestDB(t, dbpkg.RunMigrations)
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
	database := testutil.TestDB(t, dbpkg.RunMigrations)
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

// ---------------------------------------------------------------------------
// Review and check-run integration tests
// ---------------------------------------------------------------------------

func TestIntegrationReviewFlow(t *testing.T) {
	database := testutil.TestDB(t, dbpkg.RunMigrations)
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
	r := post("/repos/default/branch", `{"name":"feature/rev"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create branch: expected 201, got %d", r.StatusCode)
	}

	// Step 2: alice commits to the branch.
	r = post("/repos/default/commit", `{"branch":"feature/rev","message":"work","files":[{"path":"f.txt","content":"dGVzdA=="}]}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("commit: expected 201, got %d", r.StatusCode)
	}

	// Step 3: alice tries to approve her own commit — should get 403.
	r = post("/repos/default/review", `{"branch":"feature/rev","status":"approved","body":"self approve"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("self-approval: expected 403, got %d", r.StatusCode)
	}

	// Step 4: Switch server identity to bob who hasn't committed anything.
	handlerBob := server.New(writeStore, database, "bob@example.com", "bob@example.com")
	srvBob := httptest.NewServer(handlerBob)
	defer srvBob.Close()

	r, err := http.Post(srvBob.URL+"/repos/default/review", "application/json",
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
	getResp, err := http.Get(srv.URL + "/repos/default/branch/feature/rev/reviews")
	if err != nil {
		t.Fatalf("GET reviews: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET reviews: expected 200, got %d", getResp.StatusCode)
	}

	var reviews []struct {
		ID       string `json:"id"`
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

	// Step 6: alice makes another commit — the prior review becomes stale.
	r2, err := http.Post(srv.URL+"/repos/default/commit", "application/json",
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
	database := testutil.TestDB(t, dbpkg.RunMigrations)
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
	r := post("/repos/default/check", `{"branch":"main","check_name":"ci/build","status":"passed"}`)
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
	getResp, err := http.Get(srv.URL + "/repos/default/branch/main/checks")
	if err != nil {
		t.Fatalf("GET checks: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET checks: expected 200, got %d", getResp.StatusCode)
	}

	var checks []struct {
		ID        string `json:"id"`
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
}

func TestIntegrationReviewRepoIsolation(t *testing.T) {
	database := testutil.TestDB(t, dbpkg.RunMigrations)
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
		r := post("/repos", `{"name":"`+name+`"}`)
		r.Body.Close()
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("create repo %s: expected 201, got %d", name, r.StatusCode)
		}
	}

	// alice leaves a review on repo-x/main
	r := post("/repos/repo-x/review", `{"branch":"main","status":"approved"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("review repo-x: expected 201, got %d", r.StatusCode)
	}

	// GET /repos/repo-y/branch/main/reviews must be empty.
	getResp, err := http.Get(srv.URL + "/repos/repo-y/branch/main/reviews")
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
	database := testutil.TestDB(t, dbpkg.RunMigrations)
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
	r := doPost(adminSrv, "/repos", `{"name":"rbacrepo"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: expected 201, got %d", r.StatusCode)
	}

	// Step 2: Bootstrap admin assigns writer and maintainer roles.
	r = doPut(adminSrv, "/repos/rbacrepo/roles/"+writerID, `{"role":"writer"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("assign writer role: expected 200, got %d", r.StatusCode)
	}

	r = doPut(adminSrv, "/repos/rbacrepo/roles/"+maintainerID, `{"role":"maintainer"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("assign maintainer role: expected 200, got %d", r.StatusCode)
	}

	// Step 3: Admin creates branch; writer commits to it.
	r = doPost(adminSrv, "/repos/rbacrepo/branch", `{"name":"feature/rbac-test"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("admin create branch: expected 201, got %d", r.StatusCode)
	}

	writerSrv := makeHandler(writerID, bootstrapAdmin)
	r = doPost(writerSrv, "/repos/rbacrepo/commit",
		`{"branch":"feature/rbac-test","message":"writer commit","files":[{"path":"f.txt","content":"aGVsbG8="}]}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("writer commit: expected 201, got %d", r.StatusCode)
	}

	// Step 4: Writer cannot merge (not maintainer).
	r = doPost(writerSrv, "/repos/rbacrepo/merge", `{"branch":"feature/rbac-test"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("writer merge: expected 403, got %d", r.StatusCode)
	}

	// Step 5: Maintainer can merge.
	maintainerSrv := makeHandler(maintainerID, bootstrapAdmin)
	r = doPost(maintainerSrv, "/repos/rbacrepo/merge", `{"branch":"feature/rbac-test"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("maintainer merge: expected 200, got %d", r.StatusCode)
	}

	// Step 6: Admin can read the tree.
	r = doGet(adminSrv, "/repos/rbacrepo/tree")
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("admin read tree: expected 200, got %d", r.StatusCode)
	}

	// Step 7: Unknown identity has no access.
	unknownSrv := makeHandler("unknown@example.com", bootstrapAdmin)
	r = doGet(unknownSrv, "/repos/rbacrepo/tree")
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("unknown identity read: expected 403, got %d", r.StatusCode)
	}
}

// TestIntegrationRBAC_CrossRepoIsolation verifies that a role in repo-a
// grants no access to repo-b.
func TestIntegrationRBAC_CrossRepoIsolation(t *testing.T) {
	database := testutil.TestDB(t, dbpkg.RunMigrations)
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
		r := doPost(adminSrv, "/repos", `{"name":"`+name+`"}`)
		r.Body.Close()
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("create %s: expected 201, got %d", name, r.StatusCode)
		}
	}

	// Assign alice as admin in repo-x only.
	req, _ := http.NewRequest(http.MethodPut, adminSrv.URL+"/repos/repo-x/roles/"+alice, strings.NewReader(`{"role":"admin"}`))
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
	resp, err := http.Get(aliceSrv.URL + "/repos/repo-x/tree")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("alice repo-x: expected 200, got %d", resp.StatusCode)
	}

	// Alice cannot access repo-y (no role there).
	resp, err = http.Get(aliceSrv.URL + "/repos/repo-y/tree")
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

func TestIntegrationPurge_FullFlow(t *testing.T) {
	database := testutil.TestDB(t, dbpkg.RunMigrations)
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
	r := post("/repos/default/commit", `{"branch":"main","message":"init","files":[{"path":"main.txt","content":"bWFpbg=="}]}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("commit to main: expected 201, got %d", r.StatusCode)
	}

	// Step 2: Create and merge a feature branch (tests merged-branch cleanup).
	r = post("/repos/default/branch", `{"name":"feature/merged"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create merged branch: expected 201, got %d", r.StatusCode)
	}
	r = post("/repos/default/commit", `{"branch":"feature/merged","message":"work","files":[{"path":"merged.txt","content":"bWVyZ2Vk"}]}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("commit to merged branch: expected 201, got %d", r.StatusCode)
	}
	r = post("/repos/default/merge", `{"branch":"feature/merged"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("merge: expected 200, got %d", r.StatusCode)
	}

	// Step 3: Create and abandon a second branch with a unique file (orphan candidate).
	r = post("/repos/default/branch", `{"name":"feature/abandoned"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create abandoned branch: expected 201, got %d", r.StatusCode)
	}
	r = post("/repos/default/commit", `{"branch":"feature/abandoned","message":"abandoned work","files":[{"path":"abandoned-only.txt","content":"b3JwaGFu"}]}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("commit to abandoned branch: expected 201, got %d", r.StatusCode)
	}
	r = del("/repos/default/branch/feature/abandoned")
	r.Body.Close()
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("delete branch: expected 204, got %d", r.StatusCode)
	}

	// Step 4: Age both branches and their commits.
	for _, branch := range []string{"feature/merged", "feature/abandoned"} {
		if _, err := database.Exec(`UPDATE branches SET created_at = now() - interval '100 days' WHERE repo = 'default' AND name = $1`, branch); err != nil {
			t.Fatalf("age branch %s: %v", branch, err)
		}
		if _, err := database.Exec(`UPDATE commits SET created_at = now() - interval '100 days' WHERE repo = 'default' AND branch = $1`, branch); err != nil {
			t.Fatalf("age commits %s: %v", branch, err)
		}
	}

	// Step 5: POST /purge with 1d threshold.
	r = post("/repos/default/purge", `{"older_than":"1d"}`)
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
		if err := database.QueryRow("SELECT count(*) FROM branches WHERE repo = 'default' AND name = $1", branch).Scan(&count); err != nil {
			t.Fatalf("query branch %s: %v", branch, err)
		}
		if count != 0 {
			t.Errorf("expected branch %s to be deleted, found %d rows", branch, count)
		}
	}

	// Step 7: Verify main is unaffected — its tree has main.txt and merged.txt.
	treeResp, err := http.Get(srv.URL + "/repos/default/tree")
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
