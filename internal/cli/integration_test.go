package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// ds pull
// ---------------------------------------------------------------------------

// TestIntegrationPull_UpdatesFile verifies that pull downloads a file committed
// by another workspace and updates the local working directory.
func TestIntegrationPull_UpdatesFile(t *testing.T) {
	t.Parallel()
	srv := newRealServer(t)

	// ws1: commit a file to main.
	ws1, _ := newTestApp(t, srv)
	if err := ws1.Init(srv.URL, "", "alice"); err != nil {
		t.Fatalf("ws1 Init: %v", err)
	}
	writeFile(t, ws1, "hello.txt", "hello from alice")
	if err := ws1.Commit("add hello.txt"); err != nil {
		t.Fatalf("ws1 Commit: %v", err)
	}

	// ws2: init (gets hello.txt), then ws1 updates it, ws2 pulls.
	ws2, ws2Out := newTestApp(t, srv)
	if err := ws2.Init(srv.URL, "", "bob"); err != nil {
		t.Fatalf("ws2 Init: %v", err)
	}

	// Verify ws2 has the initial file.
	content, err := os.ReadFile(filepath.Join(ws2.Dir, "hello.txt"))
	if err != nil {
		t.Fatalf("ws2: hello.txt missing after init: %v", err)
	}
	if string(content) != "hello from alice" {
		t.Errorf("ws2 hello.txt = %q, want %q", string(content), "hello from alice")
	}

	// ws1: update the file.
	writeFile(t, ws1, "hello.txt", "updated by alice")
	if err := ws1.Commit("update hello.txt"); err != nil {
		t.Fatalf("ws1 second Commit: %v", err)
	}

	// ws2: pull should update the file.
	ws2Out.Reset()
	if err := ws2.Pull(); err != nil {
		t.Fatalf("ws2 Pull: %v", err)
	}

	updated, err := os.ReadFile(filepath.Join(ws2.Dir, "hello.txt"))
	if err != nil {
		t.Fatalf("ws2: hello.txt missing after pull: %v", err)
	}
	if string(updated) != "updated by alice" {
		t.Errorf("ws2 hello.txt after pull = %q, want %q", string(updated), "updated by alice")
	}
	if !strings.Contains(ws2Out.String(), "1 downloaded") {
		t.Errorf("expected '1 downloaded' in pull output: %s", ws2Out.String())
	}
}

// TestIntegrationPull_NoOp verifies that pull with no remote changes
// succeeds without downloading any files.
func TestIntegrationPull_NoOp(t *testing.T) {
	t.Parallel()
	srv := newRealServer(t)

	ws, wsOut := newTestApp(t, srv)
	if err := ws.Init(srv.URL, "", "alice"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	writeFile(t, ws, "file.txt", "content")
	if err := ws.Commit("initial"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Pull again — nothing changed.
	wsOut.Reset()
	if err := ws.Pull(); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if !strings.Contains(wsOut.String(), "0 downloaded") {
		t.Errorf("expected '0 downloaded' in pull output: %s", wsOut.String())
	}
}

// TestIntegrationPull_DirtyBlocked verifies that pull with uncommitted local
// changes returns an error.
func TestIntegrationPull_DirtyBlocked(t *testing.T) {
	t.Parallel()
	srv := newRealServer(t)

	ws, _ := newTestApp(t, srv)
	if err := ws.Init(srv.URL, "", "alice"); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Create an uncommitted file — makes the tree dirty.
	writeFile(t, ws, "uncommitted.txt", "not committed")

	err := ws.Pull()
	if err == nil {
		t.Fatal("expected error for dirty tree, got nil")
	}
	if !strings.Contains(err.Error(), "uncommitted changes") {
		t.Errorf("expected 'uncommitted changes' error, got: %v", err)
	}
}

// TestIntegrationPull_RemovesDeletedFile verifies that pull removes a file
// that has been deleted on the server (committed with nil content).
func TestIntegrationPull_RemovesDeletedFile(t *testing.T) {
	t.Parallel()
	srv := newRealServer(t)

	// ws1: commit a file then delete it.
	ws1, _ := newTestApp(t, srv)
	if err := ws1.Init(srv.URL, "", "alice"); err != nil {
		t.Fatalf("ws1 Init: %v", err)
	}
	writeFile(t, ws1, "temp.txt", "temporary")
	if err := ws1.Commit("add temp.txt"); err != nil {
		t.Fatalf("ws1 Commit: %v", err)
	}

	// ws2: init (downloads temp.txt).
	ws2, _ := newTestApp(t, srv)
	if err := ws2.Init(srv.URL, "", "bob"); err != nil {
		t.Fatalf("ws2 Init: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws2.Dir, "temp.txt")); err != nil {
		t.Fatalf("ws2: temp.txt should exist after init: %v", err)
	}

	// ws1: delete temp.txt by removing it locally and committing.
	if err := os.Remove(filepath.Join(ws1.Dir, "temp.txt")); err != nil {
		t.Fatalf("ws1: removing temp.txt: %v", err)
	}
	if err := ws1.Commit("delete temp.txt"); err != nil {
		t.Fatalf("ws1 delete Commit: %v", err)
	}

	// ws2: pull should remove temp.txt from disk.
	if err := ws2.Pull(); err != nil {
		t.Fatalf("ws2 Pull: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws2.Dir, "temp.txt")); !os.IsNotExist(err) {
		t.Error("ws2: temp.txt should be removed after pull")
	}

	st2, _ := ws2.loadState()
	if _, ok := st2.Files["temp.txt"]; ok {
		t.Error("ws2: temp.txt should not be in state after pull")
	}
}

// ---------------------------------------------------------------------------
// ds checkout -b / ds checkout
// ---------------------------------------------------------------------------

// TestIntegrationCheckout_BranchCycle creates a branch, switches back to main,
// then switches to the feature branch again — verifying that files are properly
// restored on each checkout and that branch isolation holds.
func TestIntegrationCheckout_BranchCycle(t *testing.T) {
	t.Parallel()
	srv := newRealServer(t)

	ws, _ := newTestApp(t, srv)
	if err := ws.Init(srv.URL, "", "alice"); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Commit a base file on main.
	writeFile(t, ws, "base.txt", "base content")
	if err := ws.Commit("base commit"); err != nil {
		t.Fatalf("Commit base: %v", err)
	}

	// Create feature branch and add feature.txt.
	if err := ws.CheckoutNew("feature/cycle"); err != nil {
		t.Fatalf("CheckoutNew: %v", err)
	}
	cfg, _ := ws.loadConfig()
	if cfg.Branch != "feature/cycle" {
		t.Errorf("branch after CheckoutNew = %q, want feature/cycle", cfg.Branch)
	}

	writeFile(t, ws, "feature.txt", "feature content")
	if err := ws.Commit("add feature.txt"); err != nil {
		t.Fatalf("Commit on feature: %v", err)
	}

	// Switch back to main: feature.txt should disappear.
	if err := ws.Checkout("main"); err != nil {
		t.Fatalf("Checkout main: %v", err)
	}
	cfg, _ = ws.loadConfig()
	if cfg.Branch != "main" {
		t.Errorf("branch after Checkout main = %q, want main", cfg.Branch)
	}
	if _, err := os.Stat(filepath.Join(ws.Dir, "feature.txt")); !os.IsNotExist(err) {
		t.Error("feature.txt should not exist on main")
	}
	if _, err := os.Stat(filepath.Join(ws.Dir, "base.txt")); err != nil {
		t.Errorf("base.txt should still exist on main: %v", err)
	}

	// Switch back to feature/cycle: feature.txt should reappear.
	// This also verifies the branch exists on the server.
	if err := ws.Checkout("feature/cycle"); err != nil {
		t.Fatalf("Checkout feature/cycle: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws.Dir, "feature.txt")); err != nil {
		t.Fatalf("feature.txt should reappear on feature/cycle: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws.Dir, "base.txt")); err != nil {
		t.Errorf("base.txt should still exist on feature/cycle: %v", err)
	}

	cfg, _ = ws.loadConfig()
	if cfg.Branch != "feature/cycle" {
		t.Errorf("branch after second checkout = %q, want feature/cycle", cfg.Branch)
	}
}

// TestIntegrationCheckout_DirtyBlocked verifies that checking out a branch
// when the working tree is dirty returns an error.
func TestIntegrationCheckout_DirtyBlocked(t *testing.T) {
	t.Parallel()
	srv := newRealServer(t)

	ws, _ := newTestApp(t, srv)
	if err := ws.Init(srv.URL, "", "alice"); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Create a branch so there is somewhere to switch to.
	if err := ws.CheckoutNew("other-branch"); err != nil {
		t.Fatalf("CheckoutNew: %v", err)
	}
	if err := ws.Checkout("main"); err != nil {
		t.Fatalf("Checkout main: %v", err)
	}

	// Create an uncommitted local file.
	writeFile(t, ws, "dirty.txt", "not committed")

	err := ws.Checkout("other-branch")
	if err == nil {
		t.Fatal("expected error for dirty tree, got nil")
	}
	if !strings.Contains(err.Error(), "uncommitted changes") {
		t.Errorf("expected 'uncommitted changes' error, got: %v", err)
	}
}

// TestIntegrationCheckout_BranchIsolation verifies that two branches with
// different files do not leak files into each other.
func TestIntegrationCheckout_BranchIsolation(t *testing.T) {
	t.Parallel()
	srv := newRealServer(t)

	ws, _ := newTestApp(t, srv)
	if err := ws.Init(srv.URL, "", "alice"); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Base commit on main.
	writeFile(t, ws, "shared.txt", "shared")
	if err := ws.Commit("base"); err != nil {
		t.Fatalf("Commit base: %v", err)
	}

	// Branch-a: add file-a.txt.
	if err := ws.CheckoutNew("branch-a"); err != nil {
		t.Fatalf("CheckoutNew branch-a: %v", err)
	}
	writeFile(t, ws, "file-a.txt", "for branch-a")
	if err := ws.Commit("add file-a"); err != nil {
		t.Fatalf("Commit branch-a: %v", err)
	}

	// Return to main, create branch-b with file-b.txt.
	if err := ws.Checkout("main"); err != nil {
		t.Fatalf("Checkout main: %v", err)
	}
	if err := ws.CheckoutNew("branch-b"); err != nil {
		t.Fatalf("CheckoutNew branch-b: %v", err)
	}
	writeFile(t, ws, "file-b.txt", "for branch-b")
	if err := ws.Commit("add file-b"); err != nil {
		t.Fatalf("Commit branch-b: %v", err)
	}

	// file-a.txt must not exist on branch-b.
	if _, err := os.Stat(filepath.Join(ws.Dir, "file-a.txt")); !os.IsNotExist(err) {
		t.Error("file-a.txt should not exist on branch-b")
	}

	// Switch to branch-a: file-a.txt should exist, file-b.txt should not.
	if err := ws.Checkout("branch-a"); err != nil {
		t.Fatalf("Checkout branch-a: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws.Dir, "file-a.txt")); err != nil {
		t.Fatalf("file-a.txt should exist on branch-a: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws.Dir, "file-b.txt")); !os.IsNotExist(err) {
		t.Error("file-b.txt should not exist on branch-a")
	}
}

// ---------------------------------------------------------------------------
// ds merge
// ---------------------------------------------------------------------------

// TestIntegrationMerge_Success exercises the full branch→commit→merge workflow
// and verifies that the working directory ends up on main with all files.
func TestIntegrationMerge_Success(t *testing.T) {
	t.Parallel()
	srv := newRealServer(t)

	ws, wsOut := newTestApp(t, srv)
	if err := ws.Init(srv.URL, "", "alice"); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Commit a base file on main.
	writeFile(t, ws, "main.txt", "main content")
	if err := ws.Commit("base"); err != nil {
		t.Fatalf("Commit base: %v", err)
	}

	// Create feature branch and add feature.txt.
	if err := ws.CheckoutNew("feature/merge-test"); err != nil {
		t.Fatalf("CheckoutNew: %v", err)
	}
	writeFile(t, ws, "feature.txt", "feature content")
	if err := ws.Commit("add feature"); err != nil {
		t.Fatalf("Commit feature: %v", err)
	}

	// Merge back to main.
	wsOut.Reset()
	if err := ws.Merge(); err != nil {
		t.Fatalf("Merge: %v", err)
	}

	// Config should reflect main.
	cfg, _ := ws.loadConfig()
	if cfg.Branch != "main" {
		t.Errorf("branch after merge = %q, want main", cfg.Branch)
	}

	// Both files should be present locally.
	if _, err := os.Stat(filepath.Join(ws.Dir, "main.txt")); err != nil {
		t.Errorf("main.txt missing after merge: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws.Dir, "feature.txt")); err != nil {
		t.Errorf("feature.txt missing after merge: %v", err)
	}

	if !strings.Contains(wsOut.String(), "Merged") {
		t.Errorf("expected 'Merged' in output: %s", wsOut.String())
	}

	// State should have both files.
	st, _ := ws.loadState()
	if len(st.Files) != 2 {
		t.Errorf("state.files = %d, want 2 after merge", len(st.Files))
	}
}

// TestIntegrationMerge_Conflict verifies that merging two branches that both
// modified the same file reports a conflict error gracefully.
func TestIntegrationMerge_Conflict(t *testing.T) {
	t.Parallel()
	srv := newRealServer(t)

	ws, wsOut := newTestApp(t, srv)
	if err := ws.Init(srv.URL, "", "alice"); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Initial commit: shared.txt on main.
	writeFile(t, ws, "shared.txt", "original")
	if err := ws.Commit("initial"); err != nil {
		t.Fatalf("Commit initial: %v", err)
	}

	// Create feature branch, modify shared.txt.
	if err := ws.CheckoutNew("feature/conflict-merge"); err != nil {
		t.Fatalf("CheckoutNew: %v", err)
	}
	writeFile(t, ws, "shared.txt", "branch version")
	if err := ws.Commit("branch modifies shared"); err != nil {
		t.Fatalf("Commit on branch: %v", err)
	}

	// Advance main past the branch base.
	if err := ws.Checkout("main"); err != nil {
		t.Fatalf("Checkout main: %v", err)
	}
	writeFile(t, ws, "shared.txt", "main version")
	if err := ws.Commit("main advances shared"); err != nil {
		t.Fatalf("Commit on main: %v", err)
	}

	// Switch to feature branch and try to merge → conflict.
	if err := ws.Checkout("feature/conflict-merge"); err != nil {
		t.Fatalf("Checkout feature: %v", err)
	}
	wsOut.Reset()
	err := ws.Merge()
	if err == nil {
		t.Fatal("expected merge conflict error, got nil")
	}
	if !strings.Contains(err.Error(), "conflict") {
		t.Errorf("expected 'conflict' in error, got: %v", err)
	}
	if !strings.Contains(wsOut.String(), "shared.txt") {
		t.Errorf("expected 'shared.txt' in conflict output: %s", wsOut.String())
	}
}

// ---------------------------------------------------------------------------
// ds rebase
// ---------------------------------------------------------------------------

// TestIntegrationRebase_Success verifies that rebase replays branch commits
// onto the current main head and updates the state sequence.
func TestIntegrationRebase_Success(t *testing.T) {
	t.Parallel()
	srv := newRealServer(t)

	ws, wsOut := newTestApp(t, srv)
	if err := ws.Init(srv.URL, "", "alice"); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Base commit on main.
	writeFile(t, ws, "base.txt", "base")
	if err := ws.Commit("base"); err != nil {
		t.Fatalf("Commit base: %v", err)
	}

	// Create feature branch and add feature.txt.
	if err := ws.CheckoutNew("feature/rebase-test"); err != nil {
		t.Fatalf("CheckoutNew: %v", err)
	}
	writeFile(t, ws, "feature.txt", "feature")
	if err := ws.Commit("add feature"); err != nil {
		t.Fatalf("Commit feature: %v", err)
	}

	stBefore, _ := ws.loadState()
	seqBefore := stBefore.Sequence

	// Advance main past the branch base.
	if err := ws.Checkout("main"); err != nil {
		t.Fatalf("Checkout main: %v", err)
	}
	writeFile(t, ws, "main2.txt", "more main")
	if err := ws.Commit("advance main"); err != nil {
		t.Fatalf("Commit advance: %v", err)
	}

	// Rebase the feature branch.
	if err := ws.Checkout("feature/rebase-test"); err != nil {
		t.Fatalf("Checkout feature: %v", err)
	}
	wsOut.Reset()
	if err := ws.Rebase(); err != nil {
		t.Fatalf("Rebase: %v", err)
	}

	if !strings.Contains(wsOut.String(), "Rebased") {
		t.Errorf("expected 'Rebased' in output: %s", wsOut.String())
	}
	if !strings.Contains(wsOut.String(), "feature/rebase-test") {
		t.Errorf("expected branch name in output: %s", wsOut.String())
	}

	// State sequence must be updated beyond what it was before the rebase.
	stAfter, _ := ws.loadState()
	if stAfter.Sequence <= seqBefore {
		t.Errorf("state.sequence = %d should be > %d after rebase",
			stAfter.Sequence, seqBefore)
	}
}

// TestIntegrationRebase_Conflict verifies that a rebase conflict writes
// .main and .branch files to disk with the correct content.
func TestIntegrationRebase_Conflict(t *testing.T) {
	t.Parallel()
	srv := newRealServer(t)

	ws, _ := newTestApp(t, srv)
	if err := ws.Init(srv.URL, "", "alice"); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Initial shared file on main.
	writeFile(t, ws, "conflict.txt", "initial content")
	if err := ws.Commit("initial"); err != nil {
		t.Fatalf("Commit initial: %v", err)
	}

	// Create feature branch, modify conflict.txt.
	if err := ws.CheckoutNew("feature/rebase-conflict"); err != nil {
		t.Fatalf("CheckoutNew: %v", err)
	}
	writeFile(t, ws, "conflict.txt", "branch changes")
	if err := ws.Commit("branch modifies conflict"); err != nil {
		t.Fatalf("Commit on branch: %v", err)
	}

	// Advance main by also modifying conflict.txt.
	if err := ws.Checkout("main"); err != nil {
		t.Fatalf("Checkout main: %v", err)
	}
	writeFile(t, ws, "conflict.txt", "main advances")
	if err := ws.Commit("main modifies conflict"); err != nil {
		t.Fatalf("Commit on main: %v", err)
	}

	// Rebase feature branch — should conflict.
	if err := ws.Checkout("feature/rebase-conflict"); err != nil {
		t.Fatalf("Checkout feature: %v", err)
	}
	err := ws.Rebase()
	if err == nil {
		t.Fatal("expected rebase conflict error, got nil")
	}
	if !strings.Contains(err.Error(), "conflict") {
		t.Errorf("expected 'conflict' in error, got: %v", err)
	}

	// conflict.txt.main and conflict.txt.branch must be written to disk.
	mainContent, err2 := os.ReadFile(filepath.Join(ws.Dir, "conflict.txt.main"))
	if err2 != nil {
		t.Fatalf("conflict.txt.main not written: %v", err2)
	}
	if string(mainContent) != "main advances" {
		t.Errorf("conflict.txt.main = %q, want %q", string(mainContent), "main advances")
	}

	branchContent, err3 := os.ReadFile(filepath.Join(ws.Dir, "conflict.txt.branch"))
	if err3 != nil {
		t.Fatalf("conflict.txt.branch not written: %v", err3)
	}
	if string(branchContent) != "branch changes" {
		t.Errorf("conflict.txt.branch = %q, want %q", string(branchContent), "branch changes")
	}
}

// TestIntegrationRebaseAndMerge verifies the full rebase-then-merge workflow:
// rebase a branch onto advanced main, then merge — all files end up on main.
func TestIntegrationRebaseAndMerge(t *testing.T) {
	t.Parallel()
	srv := newRealServer(t)

	ws, _ := newTestApp(t, srv)
	if err := ws.Init(srv.URL, "", "alice"); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Base commit on main.
	writeFile(t, ws, "main.txt", "main")
	if err := ws.Commit("base"); err != nil {
		t.Fatalf("Commit base: %v", err)
	}

	// Create feature branch and add feature.txt.
	if err := ws.CheckoutNew("feature/rebase-merge"); err != nil {
		t.Fatalf("CheckoutNew: %v", err)
	}
	writeFile(t, ws, "feature.txt", "feature")
	if err := ws.Commit("add feature"); err != nil {
		t.Fatalf("Commit feature: %v", err)
	}

	// Advance main with a new file.
	if err := ws.Checkout("main"); err != nil {
		t.Fatalf("Checkout main: %v", err)
	}
	writeFile(t, ws, "advance.txt", "advance")
	if err := ws.Commit("advance main"); err != nil {
		t.Fatalf("Commit advance: %v", err)
	}

	// Rebase feature branch.
	if err := ws.Checkout("feature/rebase-merge"); err != nil {
		t.Fatalf("Checkout feature: %v", err)
	}
	if err := ws.Rebase(); err != nil {
		t.Fatalf("Rebase: %v", err)
	}

	// Merge into main.
	if err := ws.Merge(); err != nil {
		t.Fatalf("Merge after rebase: %v", err)
	}

	// All three files should be present on main.
	for _, f := range []string{"main.txt", "advance.txt", "feature.txt"} {
		if _, err := os.Stat(filepath.Join(ws.Dir, f)); err != nil {
			t.Errorf("%s missing on main after rebase+merge: %v", f, err)
		}
	}

	cfg, _ := ws.loadConfig()
	if cfg.Branch != "main" {
		t.Errorf("branch after merge = %q, want main", cfg.Branch)
	}
}

// ---------------------------------------------------------------------------
// ds log
// ---------------------------------------------------------------------------

// TestIntegrationLog_MultipleCommits commits three files with distinct messages
// and verifies all three appear in log output in newest-first order.
func TestIntegrationLog_MultipleCommits(t *testing.T) {
	t.Parallel()
	srv := newRealServer(t)

	ws, wsOut := newTestApp(t, srv)
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

	writeFile(t, ws, "c.txt", "ccc")
	if err := ws.Commit("third commit"); err != nil {
		t.Fatalf("Commit 3: %v", err)
	}

	wsOut.Reset()
	if err := ws.Log("", 20); err != nil {
		t.Fatalf("Log: %v", err)
	}

	output := wsOut.String()
	for _, msg := range []string{"first commit", "second commit", "third commit"} {
		if !strings.Contains(output, msg) {
			t.Errorf("expected %q in log output:\n%s", msg, output)
		}
	}

	// Newest first: "third commit" must appear before "first commit".
	if strings.Index(output, "third commit") > strings.Index(output, "first commit") {
		t.Errorf("expected newest commit first in log:\n%s", output)
	}
}

// TestIntegrationLog_FilePath verifies that ds log <path> only shows commits
// that touched the specified file.
func TestIntegrationLog_FilePath(t *testing.T) {
	t.Parallel()
	srv := newRealServer(t)

	ws, wsOut := newTestApp(t, srv)
	if err := ws.Init(srv.URL, "", "alice"); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Commit that touches readme.txt.
	writeFile(t, ws, "readme.txt", "v1")
	if err := ws.Commit("add readme"); err != nil {
		t.Fatalf("Commit readme: %v", err)
	}

	// Commit that does NOT touch readme.txt.
	writeFile(t, ws, "other.txt", "other")
	if err := ws.Commit("add other"); err != nil {
		t.Fatalf("Commit other: %v", err)
	}

	// Commit that touches readme.txt again.
	writeFile(t, ws, "readme.txt", "v2")
	if err := ws.Commit("update readme"); err != nil {
		t.Fatalf("Commit update readme: %v", err)
	}

	wsOut.Reset()
	if err := ws.Log("readme.txt", 20); err != nil {
		t.Fatalf("Log readme.txt: %v", err)
	}

	output := wsOut.String()
	if !strings.Contains(output, "add readme") {
		t.Errorf("expected 'add readme' in file log:\n%s", output)
	}
	if !strings.Contains(output, "update readme") {
		t.Errorf("expected 'update readme' in file log:\n%s", output)
	}
	// "add other" touched only other.txt — must not appear.
	if strings.Contains(output, "add other") {
		t.Errorf("'add other' should not appear in readme.txt log:\n%s", output)
	}
}

// ---------------------------------------------------------------------------
// ds show
// ---------------------------------------------------------------------------

// TestIntegrationShow_Sequence commits a file and verifies that Show for that
// sequence returns the correct commit metadata and file list.
func TestIntegrationShow_Sequence(t *testing.T) {
	t.Parallel()
	srv := newRealServer(t)

	ws, wsOut := newTestApp(t, srv)
	if err := ws.Init(srv.URL, "", "alice"); err != nil {
		t.Fatalf("Init: %v", err)
	}

	writeFile(t, ws, "show-me.txt", "show content")
	if err := ws.Commit("the commit to show"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Retrieve the committed sequence from state.
	st, _ := ws.loadState()
	seq := st.Sequence
	if seq == 0 {
		t.Fatal("expected non-zero sequence after commit")
	}

	wsOut.Reset()
	if err := ws.Show(seq, ""); err != nil {
		t.Fatalf("Show(%d): %v", seq, err)
	}

	output := wsOut.String()
	if !strings.Contains(output, "the commit to show") {
		t.Errorf("expected commit message in Show output:\n%s", output)
	}
	if !strings.Contains(output, "show-me.txt") {
		t.Errorf("expected file name in Show output:\n%s", output)
	}
	if !strings.Contains(output, "author:") {
		t.Errorf("expected author field in Show output:\n%s", output)
	}
}

// TestIntegrationShow_FileContent verifies that Show with a path parameter
// returns the file's content at the given sequence.
func TestIntegrationShow_FileContent(t *testing.T) {
	t.Parallel()
	srv := newRealServer(t)

	ws, wsOut := newTestApp(t, srv)
	if err := ws.Init(srv.URL, "", "alice"); err != nil {
		t.Fatalf("Init: %v", err)
	}

	writeFile(t, ws, "snap.txt", "snapshot content")
	if err := ws.Commit("snapshot"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	st, _ := ws.loadState()
	seq := st.Sequence

	wsOut.Reset()
	if err := ws.Show(seq, "snap.txt"); err != nil {
		t.Fatalf("Show(%d, snap.txt): %v", seq, err)
	}

	if !strings.Contains(wsOut.String(), "snapshot content") {
		t.Errorf("expected file content in Show output:\n%s", wsOut.String())
	}
}

// TestIntegrationBinaryFile verifies that committing a binary file succeeds and
// that ds diff shows [binary] for that file on the branch diff.
func TestIntegrationBinaryFile(t *testing.T) {
	t.Parallel()
	srv := newRealServer(t)

	ws, wsOut := newTestApp(t, srv)
	if err := ws.Init(srv.URL, "", "alice"); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Create a feature branch so the binary file shows up in diff.
	if err := ws.CheckoutNew("feature/binary"); err != nil {
		t.Fatalf("CheckoutNew: %v", err)
	}

	// Write a binary file (non-UTF-8 bytes).
	binContent := []byte{0xFF, 0xFE, 0x00, 0x01, 0x02, 0x03}
	if err := os.WriteFile(filepath.Join(ws.Dir, "image.png"), binContent, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Commit should succeed.
	if err := ws.Commit("add binary file"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Diff should show image.png as [binary].
	wsOut.Reset()
	if err := ws.Diff(); err != nil {
		t.Fatalf("Diff: %v", err)
	}

	output := wsOut.String()
	if !strings.Contains(output, "image.png") {
		t.Errorf("expected image.png in diff output:\n%s", output)
	}
	if !strings.Contains(output, "[binary]") {
		t.Errorf("expected [binary] in diff output:\n%s", output)
	}
}
