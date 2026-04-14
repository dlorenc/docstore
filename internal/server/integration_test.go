package server_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dlorenc/docstore/internal/server"
	"github.com/dlorenc/docstore/internal/store"
	"github.com/dlorenc/docstore/internal/testutil"
)

// seed inserts test data for HTTP-level integration tests.
func seed(t *testing.T, db *sql.DB) {
	t.Helper()

	stmts := []string{
		`INSERT INTO documents (version_id, path, content, content_hash, created_by)
		 VALUES ('aaaaaaaa-0000-0000-0000-000000000001', 'hello.txt', 'hello world', 'hash_hello_v1', 'alice')`,
		`INSERT INTO documents (version_id, path, content, content_hash, created_by)
		 VALUES ('aaaaaaaa-0000-0000-0000-000000000002', 'hello.txt', 'hello world v2', 'hash_hello_v2', 'alice')`,
		`INSERT INTO documents (version_id, path, content, content_hash, created_by)
		 VALUES ('aaaaaaaa-0000-0000-0000-000000000003', 'world.txt', 'the world', 'hash_world_v1', 'bob')`,
		`INSERT INTO documents (version_id, path, content, content_hash, created_by)
		 VALUES ('aaaaaaaa-0000-0000-0000-000000000004', 'deleted.txt', 'gone soon', 'hash_deleted_v1', 'alice')`,
		`INSERT INTO file_commits (commit_id, sequence, path, version_id, branch, message, author)
		 VALUES ('cccccccc-0000-0000-0000-000000000001', 1, 'hello.txt', 'aaaaaaaa-0000-0000-0000-000000000001', 'main', 'initial commit', 'alice')`,
		`INSERT INTO file_commits (commit_id, sequence, path, version_id, branch, message, author)
		 VALUES ('cccccccc-0000-0000-0000-000000000002', 1, 'world.txt', 'aaaaaaaa-0000-0000-0000-000000000003', 'main', 'initial commit', 'alice')`,
		`INSERT INTO file_commits (commit_id, sequence, path, version_id, branch, message, author)
		 VALUES ('cccccccc-0000-0000-0000-000000000003', 2, 'hello.txt', 'aaaaaaaa-0000-0000-0000-000000000002', 'main', 'update hello', 'alice')`,
		`INSERT INTO file_commits (commit_id, sequence, path, version_id, branch, message, author)
		 VALUES ('cccccccc-0000-0000-0000-000000000004', 3, 'deleted.txt', 'aaaaaaaa-0000-0000-0000-000000000004', 'main', 'add deleted', 'bob')`,
		`INSERT INTO file_commits (commit_id, sequence, path, version_id, branch, message, author)
		 VALUES ('cccccccc-0000-0000-0000-000000000005', 4, 'deleted.txt', NULL, 'main', 'remove deleted', 'bob')`,
		`UPDATE branches SET head_sequence = 4 WHERE name = 'main'`,
	}

	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\nstatement: %s", err, stmt)
		}
	}
}

func TestIntegrationTreeEndpoint(t *testing.T) {
	db := testutil.TestDB(t)
	seed(t, db)
	handler := server.New(nil, db)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/tree")
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
	db := testutil.TestDB(t)
	seed(t, db)
	handler := server.New(nil, db)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	t.Run("get file content", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/file/hello.txt")
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
		resp, err := http.Get(srv.URL + "/file/nope.txt")
		if err != nil {
			t.Fatalf("GET /file/nope.txt: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", resp.StatusCode)
		}
	})

	t.Run("file history", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/file/hello.txt/history")
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
		resp, err := http.Get(srv.URL + "/file/hello.txt?at=1")
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

func TestIntegrationCommitEndpoint(t *testing.T) {
	db := testutil.TestDB(t)
	seed(t, db)
	handler := server.New(nil, db)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	t.Run("existing commit", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/commit/1")
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
		resp, err := http.Get(srv.URL + "/commit/999")
		if err != nil {
			t.Fatalf("GET /commit/999: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", resp.StatusCode)
		}
	})

	t.Run("invalid sequence", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/commit/abc")
		if err != nil {
			t.Fatalf("GET /commit/abc: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", resp.StatusCode)
		}
	})
}
