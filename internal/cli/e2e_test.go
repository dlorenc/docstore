package cli

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	dbpkg "github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/server"
	"github.com/dlorenc/docstore/internal/testutil"
)

// newRealServer starts an httptest.Server backed by a testcontainer Postgres
// instance running the full server handler with a real database.
func newRealServer(t *testing.T) *httptest.Server {
	t.Helper()
	database := testutil.TestDB(t, dbpkg.MigrationSQL)
	writeStore := dbpkg.NewStore(database)
	handler := server.New(writeStore, database, "test@example.com")
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// TestFullWorkflow exercises the complete CLI lifecycle against a real server
// backed by a testcontainer Postgres instance:
//
//	init → modify file → commit → checkout -b → add file → commit →
//	diff → checkout main → merge → pull from second workspace → verify files
func TestFullWorkflow(t *testing.T) {
	srv := newRealServer(t)

	// ── Workspace 1: init against empty server ────────────────────────────

	ws1, ws1Out := newTestApp(t, srv)

	if err := ws1.Init(srv.URL, "alice"); err != nil {
		t.Fatalf("ws1 Init: %v", err)
	}
	if !strings.Contains(ws1Out.String(), "Initialized") {
		t.Errorf("Init: expected 'Initialized' in output, got: %s", ws1Out.String())
	}

	st, _ := ws1.loadState()
	if st.Branch != "main" {
		t.Fatalf("ws1 state.branch after init = %q, want main", st.Branch)
	}
	if len(st.Files) != 0 {
		t.Errorf("ws1 state.files = %d, want 0 (empty server)", len(st.Files))
	}

	// ── Modify a file and commit on main ─────────────────────────────────

	writeFile(t, ws1, "README.md", "# Hello World")
	if err := ws1.Commit("initial commit"); err != nil {
		t.Fatalf("ws1 Commit: %v", err)
	}

	st, _ = ws1.loadState()
	seq1 := st.Sequence
	if seq1 == 0 {
		t.Fatal("ws1: expected non-zero sequence after commit")
	}

	// ── Workspace 2: init while main has README.md ────────────────────────

	ws2, _ := newTestApp(t, srv)
	if err := ws2.Init(srv.URL, "bob"); err != nil {
		t.Fatalf("ws2 Init: %v", err)
	}

	readmeContent, err := os.ReadFile(filepath.Join(ws2.Dir, "README.md"))
	if err != nil {
		t.Fatalf("ws2: README.md not written after init: %v", err)
	}
	if string(readmeContent) != "# Hello World" {
		t.Errorf("ws2 README.md = %q, want %q", string(readmeContent), "# Hello World")
	}

	st2, _ := ws2.loadState()
	if st2.Sequence != seq1 {
		t.Errorf("ws2 state.sequence = %d, want %d", st2.Sequence, seq1)
	}

	// ── Back on workspace 1: checkout a new branch ────────────────────────

	ws1Out.Reset()
	if err := ws1.CheckoutNew("feature/add-guide"); err != nil {
		t.Fatalf("ws1 CheckoutNew: %v", err)
	}
	if !strings.Contains(ws1Out.String(), "Switched to new branch 'feature/add-guide'") {
		t.Errorf("CheckoutNew: unexpected output: %s", ws1Out.String())
	}

	cfg, _ := ws1.loadConfig()
	if cfg.Branch != "feature/add-guide" {
		t.Errorf("ws1 config.branch = %q, want feature/add-guide", cfg.Branch)
	}

	// ── Add a file on the branch and commit ──────────────────────────────

	writeFile(t, ws1, "docs/guide.md", "# Guide")
	if err := ws1.Commit("add guide"); err != nil {
		t.Fatalf("ws1 Commit on branch: %v", err)
	}

	// ── Diff: should show docs/guide.md as changed ───────────────────────

	ws1Out.Reset()
	if err := ws1.Diff(); err != nil {
		t.Fatalf("ws1 Diff: %v", err)
	}
	if !strings.Contains(ws1Out.String(), "docs/guide.md") {
		t.Errorf("ws1 Diff: expected docs/guide.md in output: %s", ws1Out.String())
	}

	// ── Checkout main: docs/guide.md should disappear from disk ──────────

	if err := ws1.Checkout("main"); err != nil {
		t.Fatalf("ws1 Checkout main: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws1.Dir, "docs", "guide.md")); !os.IsNotExist(err) {
		t.Error("ws1: docs/guide.md should not exist on main branch")
	}
	if _, err := os.Stat(filepath.Join(ws1.Dir, "README.md")); err != nil {
		t.Error("ws1: README.md should still exist on main")
	}

	cfg, _ = ws1.loadConfig()
	if cfg.Branch != "main" {
		t.Errorf("ws1 config.branch after checkout main = %q, want main", cfg.Branch)
	}

	// ── Switch back to feature branch: docs/guide.md reappears ───────────

	if err := ws1.Checkout("feature/add-guide"); err != nil {
		t.Fatalf("ws1 Checkout feature/add-guide: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws1.Dir, "docs", "guide.md")); err != nil {
		t.Fatalf("ws1: docs/guide.md should reappear on feature branch: %v", err)
	}

	// ── Merge the branch into main ────────────────────────────────────────

	ws1Out.Reset()
	if err := ws1.Merge(); err != nil {
		t.Fatalf("ws1 Merge: %v", err)
	}
	if !strings.Contains(ws1Out.String(), "Merged") {
		t.Errorf("ws1 Merge: expected 'Merged' in output: %s", ws1Out.String())
	}

	// After merge ws1 should be on main with both files present.
	cfg, _ = ws1.loadConfig()
	if cfg.Branch != "main" {
		t.Errorf("ws1 config.branch after merge = %q, want main", cfg.Branch)
	}
	if _, err := os.Stat(filepath.Join(ws1.Dir, "README.md")); err != nil {
		t.Error("ws1: README.md missing on main after merge")
	}
	if _, err := os.Stat(filepath.Join(ws1.Dir, "docs", "guide.md")); err != nil {
		t.Error("ws1: docs/guide.md missing on main after merge")
	}

	st, _ = ws1.loadState()
	if len(st.Files) != 2 {
		t.Errorf("ws1 state.files = %d, want 2 after merge", len(st.Files))
	}

	// ── Workspace 2: pull to get the merged changes ───────────────────────

	if err := ws2.Pull(); err != nil {
		t.Fatalf("ws2 Pull: %v", err)
	}

	guideContent, err := os.ReadFile(filepath.Join(ws2.Dir, "docs", "guide.md"))
	if err != nil {
		t.Fatalf("ws2: docs/guide.md missing after pull: %v", err)
	}
	if string(guideContent) != "# Guide" {
		t.Errorf("ws2 docs/guide.md = %q, want %q", string(guideContent), "# Guide")
	}

	if _, err := os.Stat(filepath.Join(ws2.Dir, "README.md")); err != nil {
		t.Error("ws2: README.md missing after pull")
	}

	st2, _ = ws2.loadState()
	if len(st2.Files) != 2 {
		t.Errorf("ws2 state.files = %d, want 2 after pull", len(st2.Files))
	}
}
