package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dlorenc/docstore/internal/model"
)

// helper to create an App with a temp dir and optional mock server.
func newTestApp(t *testing.T, srv *httptest.Server) (*App, *strings.Builder) {
	t.Helper()
	out := &strings.Builder{}
	client := http.DefaultClient
	if srv != nil {
		client = srv.Client()
	}
	return &App{
		Dir:  t.TempDir(),
		Out:  out,
		HTTP: client,
	}, out
}

// helper to initialize a workspace in the test app.
func initWorkspace(t *testing.T, app *App, remote, branch, author string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(app.Dir, configDir), 0755); err != nil {
		t.Fatal(err)
	}
	if err := app.saveConfig(&Config{Remote: remote, Branch: branch, Author: author}); err != nil {
		t.Fatal(err)
	}
	if err := app.saveState(&State{Branch: branch, Files: make(map[string]string)}); err != nil {
		t.Fatal(err)
	}
}

// helper to write a file in the test workspace.
func writeFile(t *testing.T, app *App, path, content string) {
	t.Helper()
	full := filepath.Join(app.Dir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func strPtr(s string) *string { return &s }

// newEmptyRepoServer returns a mock server that serves an empty tree and
// a single "main" branch at sequence 0.  Suitable for Init tests.
func newEmptyRepoServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/tree":
			json.NewEncoder(w).Encode([]treeEntry{})
		case r.Method == "GET" && r.URL.Path == "/branches":
			json.NewEncoder(w).Encode([]model.Branch{{Name: "main", HeadSequence: 0}})
		default:
			http.NotFound(w, r)
		}
	}))
}

// ---------------------------------------------------------------------------
// Init
// ---------------------------------------------------------------------------

func TestInit(t *testing.T) {
	srv := newEmptyRepoServer(t)
	defer srv.Close()

	app, out := newTestApp(t, srv)

	if err := app.Init(srv.URL, "alice"); err != nil {
		t.Fatal(err)
	}

	cfg, err := app.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Remote != srv.URL {
		t.Errorf("remote = %q, want %q", cfg.Remote, srv.URL)
	}
	if cfg.Branch != "main" {
		t.Errorf("branch = %q, want %q", cfg.Branch, "main")
	}
	if cfg.Author != "alice" {
		t.Errorf("author = %q, want %q", cfg.Author, "alice")
	}

	st, err := app.loadState()
	if err != nil {
		t.Fatal(err)
	}
	if st.Branch != "main" {
		t.Errorf("state branch = %q, want %q", st.Branch, "main")
	}
	if len(st.Files) != 0 {
		t.Errorf("state files = %d, want 0", len(st.Files))
	}

	if !strings.Contains(out.String(), "Initialized") {
		t.Errorf("expected 'Initialized' in output: %s", out.String())
	}
}

func TestInitTrimsTrailingSlash(t *testing.T) {
	srv := newEmptyRepoServer(t)
	defer srv.Close()

	app, _ := newTestApp(t, srv)

	if err := app.Init(srv.URL+"/", "bob"); err != nil {
		t.Fatal(err)
	}

	cfg, _ := app.loadConfig()
	if cfg.Remote != srv.URL {
		t.Errorf("remote = %q, want trailing slash trimmed", cfg.Remote)
	}
}

func TestInitDefaultAuthor(t *testing.T) {
	srv := newEmptyRepoServer(t)
	defer srv.Close()

	app, _ := newTestApp(t, srv)

	if err := app.Init(srv.URL, ""); err != nil {
		t.Fatal(err)
	}

	cfg, _ := app.loadConfig()
	if cfg.Author == "" {
		t.Error("expected non-empty default author")
	}
}

func TestInitFetchesFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/tree":
			json.NewEncoder(w).Encode([]treeEntry{
				{Path: "README.md", VersionID: "v1", ContentHash: HashBytes([]byte("hello"))},
				{Path: "src/main.go", VersionID: "v2", ContentHash: HashBytes([]byte("package main"))},
			})
		case r.Method == "GET" && r.URL.Path == "/file/README.md":
			json.NewEncoder(w).Encode(model.FileResponse{Path: "README.md", Content: []byte("hello")})
		case r.Method == "GET" && r.URL.Path == "/file/src/main.go":
			json.NewEncoder(w).Encode(model.FileResponse{Path: "src/main.go", Content: []byte("package main")})
		case r.Method == "GET" && r.URL.Path == "/branches":
			json.NewEncoder(w).Encode([]model.Branch{{Name: "main", HeadSequence: 7}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	app, out := newTestApp(t, srv)
	if err := app.Init(srv.URL, "alice"); err != nil {
		t.Fatal(err)
	}

	// Files should be written to disk.
	content, err := os.ReadFile(filepath.Join(app.Dir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "hello" {
		t.Errorf("README.md content = %q, want %q", string(content), "hello")
	}

	// State should reflect the files and correct sequence.
	st, _ := app.loadState()
	if len(st.Files) != 2 {
		t.Errorf("state files = %d, want 2", len(st.Files))
	}
	if st.Sequence != 7 {
		t.Errorf("state sequence = %d, want 7", st.Sequence)
	}

	if !strings.Contains(out.String(), "Initialized docstore workspace (2 files, sequence 7)") {
		t.Errorf("unexpected output: %s", out.String())
	}
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

func TestStatus(t *testing.T) {
	tests := []struct {
		name       string
		stateFiles map[string]string // path -> content_hash
		localFiles map[string]string // path -> content (will be written to disk)
		wantNew    []string
		wantMod    []string
		wantDel    []string
		wantClean  bool
	}{
		{
			name:       "no changes",
			stateFiles: map[string]string{"a.txt": HashBytes([]byte("hello"))},
			localFiles: map[string]string{"a.txt": "hello"},
			wantClean:  true,
		},
		{
			name:       "new file",
			stateFiles: map[string]string{},
			localFiles: map[string]string{"new.txt": "content"},
			wantNew:    []string{"new.txt"},
		},
		{
			name:       "modified file",
			stateFiles: map[string]string{"mod.txt": HashBytes([]byte("old"))},
			localFiles: map[string]string{"mod.txt": "new"},
			wantMod:    []string{"mod.txt"},
		},
		{
			name:       "deleted file",
			stateFiles: map[string]string{"gone.txt": "somehash"},
			localFiles: map[string]string{},
			wantDel:    []string{"gone.txt"},
		},
		{
			name:       "mixed changes",
			stateFiles: map[string]string{"keep.txt": HashBytes([]byte("same")), "mod.txt": HashBytes([]byte("old")), "del.txt": "hash"},
			localFiles: map[string]string{"keep.txt": "same", "mod.txt": "new", "add.txt": "fresh"},
			wantNew:    []string{"add.txt"},
			wantMod:    []string{"mod.txt"},
			wantDel:    []string{"del.txt"},
		},
		{
			name:       "nested new file",
			stateFiles: map[string]string{},
			localFiles: map[string]string{"src/pkg/file.go": "package pkg"},
			wantNew:    []string{"src/pkg/file.go"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, out := newTestApp(t, nil)
			initWorkspace(t, app, "http://test", "main", "test")

			// Set state.
			st := &State{Branch: "main", Files: tt.stateFiles}
			if err := app.saveState(st); err != nil {
				t.Fatal(err)
			}

			// Write local files.
			for path, content := range tt.localFiles {
				writeFile(t, app, path, content)
			}

			if err := app.Status(); err != nil {
				t.Fatal(err)
			}

			output := out.String()

			if tt.wantClean {
				if !strings.Contains(output, "No changes") {
					t.Errorf("expected 'No changes' in output:\n%s", output)
				}
				return
			}

			for _, f := range tt.wantNew {
				if !strings.Contains(output, "new:") || !strings.Contains(output, f) {
					t.Errorf("expected new file %q in output:\n%s", f, output)
				}
			}
			for _, f := range tt.wantMod {
				if !strings.Contains(output, "modified:") || !strings.Contains(output, f) {
					t.Errorf("expected modified file %q in output:\n%s", f, output)
				}
			}
			for _, f := range tt.wantDel {
				if !strings.Contains(output, "deleted:") || !strings.Contains(output, f) {
					t.Errorf("expected deleted file %q in output:\n%s", f, output)
				}
			}
		})
	}
}

func TestStatusNoWorkspace(t *testing.T) {
	app, _ := newTestApp(t, nil)
	err := app.Status()
	if err == nil || !strings.Contains(err.Error(), "not a docstore workspace") {
		t.Errorf("expected workspace error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Commit
// ---------------------------------------------------------------------------

func TestCommit(t *testing.T) {
	var receivedReq model.CommitRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/commit" {
			json.NewDecoder(r.Body).Decode(&receivedReq)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(model.CommitResponse{
				Sequence: 1,
				Files:    []model.CommitFileResult{{Path: "hello.txt", VersionID: strPtr("v1")}},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	app, out := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "main", "alice")
	writeFile(t, app, "hello.txt", "hello world")

	if err := app.Commit("initial commit"); err != nil {
		t.Fatal(err)
	}

	// Verify request sent to server.
	if receivedReq.Branch != "main" {
		t.Errorf("branch = %q, want %q", receivedReq.Branch, "main")
	}
	if receivedReq.Message != "initial commit" {
		t.Errorf("message = %q, want %q", receivedReq.Message, "initial commit")
	}
	if receivedReq.Author != "alice" {
		t.Errorf("author = %q, want %q", receivedReq.Author, "alice")
	}
	if len(receivedReq.Files) != 1 {
		t.Fatalf("files = %d, want 1", len(receivedReq.Files))
	}
	if receivedReq.Files[0].Path != "hello.txt" {
		t.Errorf("file path = %q, want %q", receivedReq.Files[0].Path, "hello.txt")
	}
	if string(receivedReq.Files[0].Content) != "hello world" {
		t.Errorf("file content = %q, want %q", string(receivedReq.Files[0].Content), "hello world")
	}

	// Verify state updated.
	st, _ := app.loadState()
	if st.Sequence != 1 {
		t.Errorf("sequence = %d, want 1", st.Sequence)
	}
	if _, ok := st.Files["hello.txt"]; !ok {
		t.Error("expected hello.txt in state files")
	}

	if !strings.Contains(out.String(), "Committed sequence 1") {
		t.Errorf("expected commit confirmation in output: %s", out.String())
	}
}

func TestCommitNoChanges(t *testing.T) {
	app, _ := newTestApp(t, nil)
	initWorkspace(t, app, "http://test", "main", "alice")

	err := app.Commit("nothing")
	if err == nil || !strings.Contains(err.Error(), "nothing to commit") {
		t.Errorf("expected 'nothing to commit' error, got: %v", err)
	}
}

func TestCommitWithDelete(t *testing.T) {
	var receivedReq model.CommitRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/commit" {
			json.NewDecoder(r.Body).Decode(&receivedReq)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(model.CommitResponse{Sequence: 2})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	app, _ := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "main", "alice")

	// Pre-populate state with a file that will be "deleted".
	st := &State{Branch: "main", Files: map[string]string{"old.txt": "hash123"}}
	app.saveState(st)

	if err := app.Commit("delete old file"); err != nil {
		t.Fatal(err)
	}

	// Should have a delete entry (nil content).
	found := false
	for _, f := range receivedReq.Files {
		if f.Path == "old.txt" && f.Content == nil {
			found = true
		}
	}
	if !found {
		t.Error("expected delete entry for old.txt with nil content")
	}

	// State should no longer have old.txt.
	st, _ = app.loadState()
	if _, ok := st.Files["old.txt"]; ok {
		t.Error("old.txt should be removed from state after delete commit")
	}
}

func TestCommitServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(model.ErrorResponse{Error: "branch not found"})
	}))
	defer srv.Close()

	app, _ := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "feature/x", "alice")
	writeFile(t, app, "file.txt", "content")

	err := app.Commit("test")
	if err == nil || !strings.Contains(err.Error(), "branch not found") {
		t.Errorf("expected 'branch not found' error, got: %v", err)
	}
}

func TestCommitMultipleFiles(t *testing.T) {
	var receivedReq model.CommitRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/commit" {
			json.NewDecoder(r.Body).Decode(&receivedReq)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(model.CommitResponse{Sequence: 1})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	app, _ := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "main", "alice")
	writeFile(t, app, "a.txt", "aaa")
	writeFile(t, app, "b.txt", "bbb")
	writeFile(t, app, "src/c.go", "package c")

	if err := app.Commit("add files"); err != nil {
		t.Fatal(err)
	}

	if len(receivedReq.Files) != 3 {
		t.Fatalf("files = %d, want 3", len(receivedReq.Files))
	}

	// Files should be sorted.
	if receivedReq.Files[0].Path != "a.txt" {
		t.Errorf("first file = %q, want a.txt", receivedReq.Files[0].Path)
	}
	if receivedReq.Files[1].Path != "b.txt" {
		t.Errorf("second file = %q, want b.txt", receivedReq.Files[1].Path)
	}
	if receivedReq.Files[2].Path != "src/c.go" {
		t.Errorf("third file = %q, want src/c.go", receivedReq.Files[2].Path)
	}
}

// ---------------------------------------------------------------------------
// Checkout -b (create branch)
// ---------------------------------------------------------------------------

func TestCheckoutNew(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/branch" {
			var req model.CreateBranchRequest
			json.NewDecoder(r.Body).Decode(&req)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(model.CreateBranchResponse{
				Name:         req.Name,
				BaseSequence: 5,
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	app, out := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "main", "alice")

	// Pre-populate state with existing files.
	app.saveState(&State{Branch: "main", Sequence: 5, Files: map[string]string{"f.txt": "hash1"}})

	if err := app.CheckoutNew("feature/test"); err != nil {
		t.Fatal(err)
	}

	// Config should have new branch.
	cfg, _ := app.loadConfig()
	if cfg.Branch != "feature/test" {
		t.Errorf("branch = %q, want %q", cfg.Branch, "feature/test")
	}

	// State should carry over files from parent branch.
	st, _ := app.loadState()
	if st.Branch != "feature/test" {
		t.Errorf("state branch = %q, want %q", st.Branch, "feature/test")
	}
	if st.Sequence != 5 {
		t.Errorf("state sequence = %d, want 5", st.Sequence)
	}
	if _, ok := st.Files["f.txt"]; !ok {
		t.Error("expected files to carry over from parent branch")
	}

	if !strings.Contains(out.String(), "Switched to new branch 'feature/test'") {
		t.Errorf("unexpected output: %s", out.String())
	}
}

func TestCheckoutNewServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(model.ErrorResponse{Error: "branch already exists"})
	}))
	defer srv.Close()

	app, _ := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "main", "alice")

	err := app.CheckoutNew("existing-branch")
	if err == nil || !strings.Contains(err.Error(), "branch already exists") {
		t.Errorf("expected 'branch already exists' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Checkout (switch branch)
// ---------------------------------------------------------------------------

func TestCheckout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/tree":
			json.NewEncoder(w).Encode([]treeEntry{
				{Path: "main.go", VersionID: "v1", ContentHash: "hash1"},
				{Path: "README.md", VersionID: "v2", ContentHash: "hash2"},
			})
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/file/"):
			path := strings.TrimPrefix(r.URL.Path, "/file/")
			content := map[string]string{
				"main.go":   "package main",
				"README.md": "# Hello",
			}
			json.NewEncoder(w).Encode(model.FileResponse{
				Path:    path,
				Content: []byte(content[path]),
			})
		case r.Method == "GET" && r.URL.Path == "/branches":
			json.NewEncoder(w).Encode([]model.Branch{{Name: "main", HeadSequence: 3}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	app, out := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "feature/old", "alice")

	if err := app.Checkout("main"); err != nil {
		t.Fatal(err)
	}

	// Config should have new branch.
	cfg, _ := app.loadConfig()
	if cfg.Branch != "main" {
		t.Errorf("branch = %q, want %q", cfg.Branch, "main")
	}

	// Files should be written to disk.
	content, err := os.ReadFile(filepath.Join(app.Dir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "package main" {
		t.Errorf("main.go content = %q, want %q", string(content), "package main")
	}

	content, err = os.ReadFile(filepath.Join(app.Dir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "# Hello" {
		t.Errorf("README.md content = %q, want %q", string(content), "# Hello")
	}

	// State should have both files and updated sequence.
	st, _ := app.loadState()
	if len(st.Files) != 2 {
		t.Errorf("state files = %d, want 2", len(st.Files))
	}
	if st.Sequence != 3 {
		t.Errorf("state sequence = %d, want 3", st.Sequence)
	}

	if !strings.Contains(out.String(), "Synced branch 'main'") {
		t.Errorf("unexpected output: %s", out.String())
	}
}

func TestCheckoutRemovesDeletedFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/tree":
			json.NewEncoder(w).Encode([]treeEntry{})
		case r.Method == "GET" && r.URL.Path == "/branches":
			json.NewEncoder(w).Encode([]model.Branch{{Name: "feature/new", HeadSequence: 0}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	app, _ := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "main", "alice")

	// Pre-populate state with a file using its actual hash so the tree is clean.
	app.saveState(&State{Branch: "main", Files: map[string]string{"old.txt": HashBytes([]byte("old content"))}})
	writeFile(t, app, "old.txt", "old content")

	if err := app.Checkout("feature/new"); err != nil {
		t.Fatal(err)
	}

	// old.txt should be removed from disk.
	if _, err := os.Stat(filepath.Join(app.Dir, "old.txt")); !os.IsNotExist(err) {
		t.Error("expected old.txt to be removed from disk")
	}

	// State should be empty.
	st, _ := app.loadState()
	if len(st.Files) != 0 {
		t.Errorf("state files = %d, want 0", len(st.Files))
	}
}

func TestCheckoutDirtyTree(t *testing.T) {
	app, _ := newTestApp(t, nil)
	initWorkspace(t, app, "http://test", "main", "alice")

	// Add a new untracked file — makes the tree dirty.
	writeFile(t, app, "new.txt", "new content")

	err := app.Checkout("feature/x")
	if err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Errorf("expected 'uncommitted changes' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Pull
// ---------------------------------------------------------------------------

func TestPull(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/tree":
			if r.URL.Query().Get("branch") != "feature/x" {
				t.Errorf("expected branch=feature/x, got %q", r.URL.Query().Get("branch"))
			}
			json.NewEncoder(w).Encode([]treeEntry{
				{Path: "updated.txt", VersionID: "v3", ContentHash: "newhash"},
			})
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/file/"):
			callCount++
			json.NewEncoder(w).Encode(model.FileResponse{
				Path:    "updated.txt",
				Content: []byte("updated content"),
			})
		case r.Method == "GET" && r.URL.Path == "/branches":
			json.NewEncoder(w).Encode([]model.Branch{{Name: "feature/x", HeadSequence: 5}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	app, out := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "feature/x", "alice")

	// Pre-populate with old content on disk + matching state hash, so isDirty is clean
	// but the server hash ("newhash") is different → file will be downloaded.
	const oldContent = "old content"
	writeFile(t, app, "updated.txt", oldContent)
	app.saveState(&State{Branch: "feature/x", Files: map[string]string{"updated.txt": HashBytes([]byte(oldContent))}})

	if err := app.Pull(); err != nil {
		t.Fatal(err)
	}

	if callCount != 1 {
		t.Errorf("expected 1 file download, got %d", callCount)
	}

	content, err := os.ReadFile(filepath.Join(app.Dir, "updated.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "updated content" {
		t.Errorf("content = %q, want %q", string(content), "updated content")
	}

	// Sequence should be updated.
	st, _ := app.loadState()
	if st.Sequence != 5 {
		t.Errorf("sequence = %d, want 5", st.Sequence)
	}

	if !strings.Contains(out.String(), "1 downloaded") {
		t.Errorf("unexpected output: %s", out.String())
	}
}

func TestPullSkipsUnchanged(t *testing.T) {
	const sameContent = "same content"
	sameHash := HashBytes([]byte(sameContent))
	fileDownloads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/tree":
			json.NewEncoder(w).Encode([]treeEntry{
				{Path: "same.txt", VersionID: "v1", ContentHash: sameHash},
			})
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/file/"):
			fileDownloads++
			json.NewEncoder(w).Encode(model.FileResponse{Path: "same.txt", Content: []byte(sameContent)})
		case r.Method == "GET" && r.URL.Path == "/branches":
			json.NewEncoder(w).Encode([]model.Branch{{Name: "main", HeadSequence: 1}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	app, out := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "main", "alice")

	// Write the file to disk and store its real hash in state so isDirty is clean.
	writeFile(t, app, "same.txt", sameContent)
	app.saveState(&State{Branch: "main", Files: map[string]string{"same.txt": sameHash}})

	if err := app.Pull(); err != nil {
		t.Fatal(err)
	}

	if fileDownloads != 0 {
		t.Errorf("expected 0 file downloads, got %d", fileDownloads)
	}

	if !strings.Contains(out.String(), "0 downloaded") {
		t.Errorf("unexpected output: %s", out.String())
	}
}

func TestPullDirtyTree(t *testing.T) {
	app, _ := newTestApp(t, nil)
	initWorkspace(t, app, "http://test", "main", "alice")

	// Add a new untracked file — makes the tree dirty.
	writeFile(t, app, "new.txt", "new content")

	err := app.Pull()
	if err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Errorf("expected 'uncommitted changes' error, got: %v", err)
	}
}

func TestSyncTreeUpdatesSequence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/tree":
			json.NewEncoder(w).Encode([]treeEntry{})
		case r.Method == "GET" && r.URL.Path == "/branches":
			json.NewEncoder(w).Encode([]model.Branch{{Name: "main", HeadSequence: 42}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	app, _ := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "main", "alice")

	if err := app.Pull(); err != nil {
		t.Fatal(err)
	}

	st, _ := app.loadState()
	if st.Sequence != 42 {
		t.Errorf("sequence = %d, want 42", st.Sequence)
	}
}

// ---------------------------------------------------------------------------
// Merge
// ---------------------------------------------------------------------------

func TestMerge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/merge":
			var req model.MergeRequest
			json.NewDecoder(r.Body).Decode(&req)
			if req.Branch != "feature/done" {
				t.Errorf("merge branch = %q, want %q", req.Branch, "feature/done")
			}
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(model.MergeResponse{Sequence: 10})
		case r.Method == "GET" && r.URL.Path == "/tree":
			json.NewEncoder(w).Encode([]treeEntry{})
		case r.Method == "GET" && r.URL.Path == "/branches":
			json.NewEncoder(w).Encode([]model.Branch{{Name: "main", HeadSequence: 10}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	app, out := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "feature/done", "alice")

	if err := app.Merge(); err != nil {
		t.Fatal(err)
	}

	// Config should be switched to main.
	cfg, _ := app.loadConfig()
	if cfg.Branch != "main" {
		t.Errorf("branch after merge = %q, want %q", cfg.Branch, "main")
	}

	if !strings.Contains(out.String(), "Merged 'feature/done' into main at sequence 10") {
		t.Errorf("unexpected output: %s", out.String())
	}
}

func TestMergeFromMain(t *testing.T) {
	app, _ := newTestApp(t, nil)
	initWorkspace(t, app, "http://test", "main", "alice")

	err := app.Merge()
	if err == nil || !strings.Contains(err.Error(), "cannot merge main into itself") {
		t.Errorf("expected merge-main error, got: %v", err)
	}
}

func TestMergeConflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(model.MergeConflictError{
			Conflicts: []model.ConflictEntry{
				{Path: "file.txt", MainVersionID: "v1", BranchVersionID: "v2"},
			},
		})
	}))
	defer srv.Close()

	app, out := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "feature/conflict", "alice")

	err := app.Merge()
	if err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Errorf("expected conflict error, got: %v", err)
	}

	if !strings.Contains(out.String(), "file.txt") {
		t.Errorf("expected conflict path in output: %s", out.String())
	}
}

// ---------------------------------------------------------------------------
// Diff
// ---------------------------------------------------------------------------

func TestDiff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/diff" {
			if r.URL.Query().Get("branch") != "feature/x" {
				t.Errorf("diff branch = %q, want %q", r.URL.Query().Get("branch"), "feature/x")
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(model.DiffResponse{
				BranchChanges: []model.DiffEntry{
					{Path: "new.go", VersionID: strPtr("v1")},
					{Path: "removed.go", VersionID: nil},
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	app, out := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "feature/x", "alice")

	if err := app.Diff(); err != nil {
		t.Fatal(err)
	}

	output := out.String()
	if !strings.Contains(output, "changed: new.go") {
		t.Errorf("expected 'changed: new.go' in output:\n%s", output)
	}
	if !strings.Contains(output, "deleted: removed.go") {
		t.Errorf("expected 'deleted: removed.go' in output:\n%s", output)
	}
}

func TestDiffNoChanges(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(model.DiffResponse{})
	}))
	defer srv.Close()

	app, out := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "feature/x", "alice")

	if err := app.Diff(); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), "No changes") {
		t.Errorf("expected 'No changes' in output: %s", out.String())
	}
}

func TestDiffWithConflicts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(model.DiffResponse{
			BranchChanges: []model.DiffEntry{{Path: "a.txt", VersionID: strPtr("v1")}},
			Conflicts: []model.ConflictEntry{
				{Path: "shared.txt", MainVersionID: "m1", BranchVersionID: "b1"},
			},
		})
	}))
	defer srv.Close()

	app, out := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "feature/x", "alice")

	if err := app.Diff(); err != nil {
		t.Fatal(err)
	}

	output := out.String()
	if !strings.Contains(output, "Conflicts:") {
		t.Errorf("expected 'Conflicts:' in output:\n%s", output)
	}
	if !strings.Contains(output, "shared.txt") {
		t.Errorf("expected 'shared.txt' in output:\n%s", output)
	}
}

// ---------------------------------------------------------------------------
// Identity header
// ---------------------------------------------------------------------------

func TestIdentityHeaderSentOnCommit(t *testing.T) {
	var gotIdentity string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIdentity = r.Header.Get("X-DocStore-Identity")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(model.CommitResponse{Sequence: 1})
	}))
	defer srv.Close()

	app, _ := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "main", "alice")
	writeFile(t, app, "f.txt", "content")

	app.Commit("test")

	if gotIdentity != "alice" {
		t.Errorf("X-DocStore-Identity = %q, want %q", gotIdentity, "alice")
	}
}

// ---------------------------------------------------------------------------
// Config / State persistence
// ---------------------------------------------------------------------------

func TestConfigRoundTrip(t *testing.T) {
	app, _ := newTestApp(t, nil)
	os.MkdirAll(filepath.Join(app.Dir, configDir), 0755)

	cfg := &Config{Remote: "http://example.com", Branch: "dev", Author: "bob"}
	if err := app.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	loaded, err := app.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Remote != cfg.Remote || loaded.Branch != cfg.Branch || loaded.Author != cfg.Author {
		t.Errorf("config mismatch: got %+v, want %+v", loaded, cfg)
	}
}

func TestStateRoundTrip(t *testing.T) {
	app, _ := newTestApp(t, nil)
	os.MkdirAll(filepath.Join(app.Dir, configDir), 0755)

	st := &State{
		Branch:   "feature/test",
		Sequence: 42,
		Files:    map[string]string{"a.txt": "hash1", "b.txt": "hash2"},
	}
	if err := app.saveState(st); err != nil {
		t.Fatal(err)
	}

	loaded, err := app.loadState()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Branch != st.Branch || loaded.Sequence != st.Sequence {
		t.Errorf("state mismatch: got branch=%q seq=%d, want branch=%q seq=%d",
			loaded.Branch, loaded.Sequence, st.Branch, st.Sequence)
	}
	if len(loaded.Files) != 2 {
		t.Errorf("state files = %d, want 2", len(loaded.Files))
	}
}

func TestLoadStateMissing(t *testing.T) {
	app, _ := newTestApp(t, nil)

	st, err := app.loadState()
	if err != nil {
		t.Fatal(err)
	}
	if st.Files == nil {
		t.Error("expected non-nil Files map for missing state")
	}
}

// ---------------------------------------------------------------------------
// HashBytes
// ---------------------------------------------------------------------------

func TestHashBytes(t *testing.T) {
	h := HashBytes([]byte("hello"))
	// SHA256("hello") = 2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824
	want := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if h != want {
		t.Errorf("hash = %q, want %q", h, want)
	}
}
