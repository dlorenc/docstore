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

	if err := ws1.Init(srv.URL, "", "alice"); err != nil {
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
	if err := ws2.Init(srv.URL, "", "bob"); err != nil {
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

// TestCLILog_EndToEnd verifies that ds log returns commits in newest-first order.
func TestCLILog_EndToEnd(t *testing.T) {
	srv := newRealServer(t)

	ws, out := newTestApp(t, srv)
	if err := ws.Init(srv.URL, "", "alice"); err != nil {
		t.Fatalf("Init: %v", err)
	}

	writeFile(t, ws, "a.txt", "aaa")
	if err := ws.Commit("first commit"); err != nil {
		t.Fatalf("Commit 1: %v", err)
	}

	writeFile(t, ws, "b.txt", "bbb")
	if err := ws.Commit("second commit"); err != nil {
		t.Fatalf("Commit 2: %v", err)
	}

	out.Reset()
	if err := ws.Log("", 20); err != nil {
		t.Fatalf("Log: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "second commit") {
		t.Errorf("expected 'second commit' in log output:\n%s", output)
	}
	if !strings.Contains(output, "first commit") {
		t.Errorf("expected 'first commit' in log output:\n%s", output)
	}
	// newest first: second commit appears before first commit
	if strings.Index(output, "second commit") > strings.Index(output, "first commit") {
		t.Errorf("expected newest commit first:\n%s", output)
	}
}

// TestCLIRebase_EndToEnd exercises the full rebase workflow with a real server.
func TestCLIRebase_EndToEnd(t *testing.T) {
	srv := newRealServer(t)

	ws, _ := newTestApp(t, srv)
	if err := ws.Init(srv.URL, "", "alice"); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Commit something to main.
	writeFile(t, ws, "main.txt", "main content")
	if err := ws.Commit("base commit"); err != nil {
		t.Fatalf("Commit base: %v", err)
	}

	// Create a feature branch and commit.
	if err := ws.CheckoutNew("feature/rebase-test"); err != nil {
		t.Fatalf("CheckoutNew: %v", err)
	}
	writeFile(t, ws, "feature.txt", "feature content")
	if err := ws.Commit("feature commit"); err != nil {
		t.Fatalf("Commit feature: %v", err)
	}

	// Advance main past the feature branch base.
	if err := ws.Checkout("main"); err != nil {
		t.Fatalf("Checkout main: %v", err)
	}
	writeFile(t, ws, "main2.txt", "more main content")
	if err := ws.Commit("main advance"); err != nil {
		t.Fatalf("Commit main advance: %v", err)
	}

	// Go back to feature branch and rebase.
	if err := ws.Checkout("feature/rebase-test"); err != nil {
		t.Fatalf("Checkout feature: %v", err)
	}

	wsOut := &strings.Builder{}
	ws.Out = wsOut
	if err := ws.Rebase(); err != nil {
		t.Fatalf("Rebase: %v", err)
	}

	if !strings.Contains(wsOut.String(), "Rebased") {
		t.Errorf("expected 'Rebased' in output:\n%s", wsOut.String())
	}

	// state.json sequence should be updated.
	st, _ := ws.loadState()
	if st.Sequence == 0 {
		t.Error("expected non-zero sequence in state after rebase")
	}
}

// TestCLIResolve_EndToEnd exercises the resolve workflow: write conflict files,
// create a resolved version, call Resolve, verify commit and cleanup.
func TestCLIResolve_EndToEnd(t *testing.T) {
	srv := newRealServer(t)

	ws, _ := newTestApp(t, srv)
	if err := ws.Init(srv.URL, "", "alice"); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Create a branch so we can commit to it.
	if err := ws.CheckoutNew("feature/resolve-test"); err != nil {
		t.Fatalf("CheckoutNew: %v", err)
	}

	// Simulate conflict files written by a rebase.
	writeFile(t, ws, "README.md.main", "main version")
	writeFile(t, ws, "README.md.branch", "branch version")
	writeFile(t, ws, "README.md", "resolved version")

	wsOut := &strings.Builder{}
	ws.Out = wsOut
	if err := ws.Resolve("README.md"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if !strings.Contains(wsOut.String(), "resolved README.md") {
		t.Errorf("expected resolution message in output:\n%s", wsOut.String())
	}

	// Conflict files should be gone.
	if _, err := os.Stat(filepath.Join(ws.Dir, "README.md.main")); !os.IsNotExist(err) {
		t.Error("expected README.md.main to be removed")
	}
	if _, err := os.Stat(filepath.Join(ws.Dir, "README.md.branch")); !os.IsNotExist(err) {
		t.Error("expected README.md.branch to be removed")
	}

	// State should track README.md.
	st, _ := ws.loadState()
	if _, ok := st.Files["README.md"]; !ok {
		t.Error("expected README.md in state files after resolve")
	}
}
