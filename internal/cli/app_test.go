package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if err := app.saveConfig(&Config{Remote: remote, Repo: "default/default", Branch: branch, Author: author}); err != nil {
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
		case r.Method == "GET" && r.URL.Path == "/repos/default/default/-/tree":
			json.NewEncoder(w).Encode([]treeEntry{})
		case r.Method == "GET" && r.URL.Path == "/repos/default/default/-/branches":
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

	if err := app.Init(srv.URL, "", "alice"); err != nil {
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

	if err := app.Init(srv.URL+"/", "", "bob"); err != nil {
		t.Fatal(err)
	}

	cfg, _ := app.loadConfig()
	if cfg.Remote != srv.URL {
		t.Errorf("remote = %q, want trailing slash trimmed", cfg.Remote)
	}
}

// TestInitRepoFlagStripsURLSuffix verifies that when --repo is passed explicitly
// alongside a URL that already contains /repos/:owner/:name, the /repos/... suffix is
// still stripped from baseRemote.  Without the fix this produced doubled paths.
func TestInitRepoFlagStripsURLSuffix(t *testing.T) {
	// The mock server registers the /repos/default/myrepo/-/* paths that Init will call.
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/default/myrepo/-/tree":
			json.NewEncoder(w).Encode([]treeEntry{})
		case r.Method == "GET" && r.URL.Path == "/repos/default/myrepo/-/branches":
			json.NewEncoder(w).Encode([]model.Branch{{Name: "main", HeadSequence: 0}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	app, _ := newTestApp(t, srv)

	// Pass a URL that already embeds the /repos/default/myrepo suffix AND an explicit
	// --repo flag with the same full path.
	embeddedURL := srv.URL + "/repos/default/myrepo"
	if err := app.Init(embeddedURL, "default/myrepo", "carol"); err != nil {
		t.Fatalf("Init: %v", err)
	}

	cfg, err := app.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	// Remote must be the bare host, not .../repos/default/myrepo.
	if cfg.Remote != srv.URL {
		t.Errorf("remote = %q, want %q (suffix must be stripped)", cfg.Remote, srv.URL)
	}
	if cfg.Repo != "default/myrepo" {
		t.Errorf("repo = %q, want %q", cfg.Repo, "default/myrepo")
	}
}

// TestInitCanonicalTrailingDash verifies that Init strips the trailing "/-"
// from the canonical URL form (e.g. https://host/repos/owner/name/-).
func TestInitCanonicalTrailingDash(t *testing.T) {
	// The mock server registers the /repos/default/myrepo/-/* paths that Init will call.
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/default/myrepo/-/tree":
			json.NewEncoder(w).Encode([]treeEntry{})
		case r.Method == "GET" && r.URL.Path == "/repos/default/myrepo/-/branches":
			json.NewEncoder(w).Encode([]model.Branch{{Name: "main", HeadSequence: 0}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	app, _ := newTestApp(t, srv)

	// Pass the canonical trailing-/- URL form with no explicit --repo flag.
	canonicalURL := srv.URL + "/repos/default/myrepo/-"
	if err := app.Init(canonicalURL, "", "carol"); err != nil {
		t.Fatalf("Init: %v", err)
	}

	cfg, err := app.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	// Remote must be the bare host.
	if cfg.Remote != srv.URL {
		t.Errorf("remote = %q, want %q (trailing /- must be stripped)", cfg.Remote, srv.URL)
	}
	if cfg.Repo != "default/myrepo" {
		t.Errorf("repo = %q, want %q", cfg.Repo, "default/myrepo")
	}
}

func TestInitDefaultAuthor(t *testing.T) {
	srv := newEmptyRepoServer(t)
	defer srv.Close()

	app, _ := newTestApp(t, srv)

	if err := app.Init(srv.URL, "", ""); err != nil {
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
		case r.Method == "GET" && r.URL.Path == "/repos/default/default/-/tree":
			json.NewEncoder(w).Encode([]treeEntry{
				{Path: "README.md", VersionID: "v1", ContentHash: HashBytes([]byte("hello"))},
				{Path: "src/main.go", VersionID: "v2", ContentHash: HashBytes([]byte("package main"))},
			})
		case r.Method == "GET" && r.URL.Path == "/repos/default/default/-/file/README.md":
			json.NewEncoder(w).Encode(model.FileResponse{Path: "README.md", Content: []byte("hello")})
		case r.Method == "GET" && r.URL.Path == "/repos/default/default/-/file/src/main.go":
			json.NewEncoder(w).Encode(model.FileResponse{Path: "src/main.go", Content: []byte("package main")})
		case r.Method == "GET" && r.URL.Path == "/repos/default/default/-/branches":
			json.NewEncoder(w).Encode([]model.Branch{{Name: "main", HeadSequence: 7}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	app, out := newTestApp(t, srv)
	if err := app.Init(srv.URL, "", "alice"); err != nil {
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
		if r.Method == "POST" && r.URL.Path == "/repos/default/default/-/commit" {
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
		if r.Method == "POST" && r.URL.Path == "/repos/default/default/-/commit" {
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
		if r.Method == "POST" && r.URL.Path == "/repos/default/default/-/commit" {
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
		if r.Method == "POST" && r.URL.Path == "/repos/default/default/-/branch" {
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
		case r.Method == "GET" && r.URL.Path == "/repos/default/default/-/tree":
			json.NewEncoder(w).Encode([]treeEntry{
				{Path: "main.go", VersionID: "v1", ContentHash: "hash1"},
				{Path: "README.md", VersionID: "v2", ContentHash: "hash2"},
			})
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/repos/default/default/-/file/"):
			path := strings.TrimPrefix(r.URL.Path, "/repos/default/default/-/file/")
			content := map[string]string{
				"main.go":   "package main",
				"README.md": "# Hello",
			}
			json.NewEncoder(w).Encode(model.FileResponse{
				Path:    path,
				Content: []byte(content[path]),
			})
		case r.Method == "GET" && r.URL.Path == "/repos/default/default/-/branches":
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
		case r.Method == "GET" && r.URL.Path == "/repos/default/default/-/tree":
			json.NewEncoder(w).Encode([]treeEntry{})
		case r.Method == "GET" && r.URL.Path == "/repos/default/default/-/branches":
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
		case r.Method == "GET" && r.URL.Path == "/repos/default/default/-/tree":
			if r.URL.Query().Get("branch") != "feature/x" {
				t.Errorf("expected branch=feature/x, got %q", r.URL.Query().Get("branch"))
			}
			json.NewEncoder(w).Encode([]treeEntry{
				{Path: "updated.txt", VersionID: "v3", ContentHash: "newhash"},
			})
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/repos/default/default/-/file/"):
			callCount++
			json.NewEncoder(w).Encode(model.FileResponse{
				Path:    "updated.txt",
				Content: []byte("updated content"),
			})
		case r.Method == "GET" && r.URL.Path == "/repos/default/default/-/branches":
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
		case r.Method == "GET" && r.URL.Path == "/repos/default/default/-/tree":
			json.NewEncoder(w).Encode([]treeEntry{
				{Path: "same.txt", VersionID: "v1", ContentHash: sameHash},
			})
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/repos/default/default/-/file/"):
			fileDownloads++
			json.NewEncoder(w).Encode(model.FileResponse{Path: "same.txt", Content: []byte(sameContent)})
		case r.Method == "GET" && r.URL.Path == "/repos/default/default/-/branches":
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
		case r.Method == "GET" && r.URL.Path == "/repos/default/default/-/tree":
			json.NewEncoder(w).Encode([]treeEntry{})
		case r.Method == "GET" && r.URL.Path == "/repos/default/default/-/branches":
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
		case r.Method == "POST" && r.URL.Path == "/repos/default/default/-/merge":
			var req model.MergeRequest
			json.NewDecoder(r.Body).Decode(&req)
			if req.Branch != "feature/done" {
				t.Errorf("merge branch = %q, want %q", req.Branch, "feature/done")
			}
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(model.MergeResponse{Sequence: 10})
		case r.Method == "GET" && r.URL.Path == "/repos/default/default/-/tree":
			json.NewEncoder(w).Encode([]treeEntry{})
		case r.Method == "GET" && r.URL.Path == "/repos/default/default/-/branches":
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
		if r.Method == "GET" && r.URL.Path == "/repos/default/default/-/diff" {
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
// detectContentType
// ---------------------------------------------------------------------------

func TestDetectContentType(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		content []byte
		want    string
	}{
		{
			name:    "valid UTF-8 text returns empty",
			path:    "hello.txt",
			content: []byte("hello world"),
			want:    "",
		},
		{
			name:    "known binary extension with non-UTF-8 bytes returns image/png",
			path:    "image.png",
			content: []byte{0xFF, 0xFE, 0x00, 0x01},
			want:    "image/png",
		},
		{
			name:    "unknown extension with non-UTF-8 bytes returns application/octet-stream",
			path:    "data.bin",
			content: []byte{0xFF, 0xFE, 0x00, 0x01},
			want:    "application/octet-stream",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectContentType(tt.path, tt.content)
			if got != tt.want {
				t.Errorf("detectContentType(%q, ...) = %q, want %q", tt.path, got, tt.want)
			}
		})
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

// ---------------------------------------------------------------------------
// Log
// ---------------------------------------------------------------------------

func TestLog_FullHistory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/default/default/-/branches":
			json.NewEncoder(w).Encode([]model.Branch{
				{Name: "feature/x", HeadSequence: 3, BaseSequence: 0},
			})
		case r.Method == "GET" && r.URL.Path == "/repos/default/default/-/commit/3":
			json.NewEncoder(w).Encode(commitInfo{
				Sequence: 3, Branch: "feature/x", Author: "alice",
				CreatedAt: mustParseTime("2026-04-14"), Message: "add tests",
			})
		case r.Method == "GET" && r.URL.Path == "/repos/default/default/-/commit/2":
			// commit 2 is on main — should be skipped
			json.NewEncoder(w).Encode(commitInfo{
				Sequence: 2, Branch: "main", Author: "bob",
				CreatedAt: mustParseTime("2026-04-14"), Message: "main commit",
			})
		case r.Method == "GET" && r.URL.Path == "/repos/default/default/-/commit/1":
			json.NewEncoder(w).Encode(commitInfo{
				Sequence: 1, Branch: "feature/x", Author: "alice",
				CreatedAt: mustParseTime("2026-04-14"), Message: "initial",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	app, out := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "feature/x", "alice")

	if err := app.Log("", 20); err != nil {
		t.Fatal(err)
	}

	output := out.String()
	if !strings.Contains(output, "seq 3") {
		t.Errorf("expected seq 3 in output:\n%s", output)
	}
	if !strings.Contains(output, "add tests") {
		t.Errorf("expected message 'add tests' in output:\n%s", output)
	}
	if !strings.Contains(output, "seq 1") {
		t.Errorf("expected seq 1 in output:\n%s", output)
	}
	if strings.Contains(output, "main commit") {
		t.Errorf("main commits should not appear:\n%s", output)
	}
	// newest first: seq 3 before seq 1
	if strings.Index(output, "seq 3") > strings.Index(output, "seq 1") {
		t.Errorf("expected seq 3 before seq 1 (newest first):\n%s", output)
	}
}

func TestLog_FileHistory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" && r.URL.Path == "/repos/default/default/-/file/README.md/history" {
			if r.URL.Query().Get("branch") != "main" {
				t.Errorf("expected branch=main, got %q", r.URL.Query().Get("branch"))
			}
			json.NewEncoder(w).Encode([]fileHistEntry{
				{Sequence: 5, Author: "alice", CreatedAt: mustParseTime("2026-04-14"), Message: "update readme"},
				{Sequence: 1, Author: "alice", CreatedAt: mustParseTime("2026-04-13"), Message: "add readme"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	app, out := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "main", "alice")

	if err := app.Log("README.md", 20); err != nil {
		t.Fatal(err)
	}

	output := out.String()
	if !strings.Contains(output, "seq 5") {
		t.Errorf("expected seq 5 in output:\n%s", output)
	}
	if !strings.Contains(output, "update readme") {
		t.Errorf("expected 'update readme' in output:\n%s", output)
	}
	if !strings.Contains(output, "seq 1") {
		t.Errorf("expected seq 1 in output:\n%s", output)
	}
}

func TestLog_Limit(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/default/default/-/branches":
			json.NewEncoder(w).Encode([]model.Branch{
				{Name: "main", HeadSequence: 3, BaseSequence: 0},
			})
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/repos/default/default/-/commit/"):
			callCount++
			seq := int64(0)
			fmt.Sscanf(r.URL.Path, "/repos/default/default/-/commit/%d", &seq)
			json.NewEncoder(w).Encode(commitInfo{
				Sequence: seq, Branch: "main", Author: "alice",
				CreatedAt: mustParseTime("2026-04-14"), Message: fmt.Sprintf("commit %d", seq),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	app, out := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "main", "alice")

	if err := app.Log("", 1); err != nil {
		t.Fatal(err)
	}

	output := out.String()
	// Should only show seq 3 (limit=1, newest first)
	if !strings.Contains(output, "seq 3") {
		t.Errorf("expected seq 3 in output:\n%s", output)
	}
	if strings.Contains(output, "seq 2") || strings.Contains(output, "seq 1") {
		t.Errorf("limit=1 should only show 1 entry:\n%s", output)
	}
}

// ---------------------------------------------------------------------------
// Show
// ---------------------------------------------------------------------------

func TestShow_Commit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/repos/default/default/-/commit/5" {
			w.Header().Set("Content-Type", "application/json")
			vid := "v1"
			json.NewEncoder(w).Encode(commitInfo{
				Sequence:  5,
				Branch:    "feature/x",
				Author:    "alice",
				CreatedAt: mustParseTime("2026-04-14"),
				Message:   "add test file",
				Files: []commitFileInfo{
					{Path: "src/main_test.go", VersionID: &vid},
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

	if err := app.Show(5, ""); err != nil {
		t.Fatal(err)
	}

	output := out.String()
	if !strings.Contains(output, "commit 5") {
		t.Errorf("expected 'commit 5' in output:\n%s", output)
	}
	if !strings.Contains(output, "src/main_test.go") {
		t.Errorf("expected file path in output:\n%s", output)
	}
	if !strings.Contains(output, "removed.go") {
		t.Errorf("expected deleted file in output:\n%s", output)
	}
	if !strings.Contains(output, "deleted") {
		t.Errorf("expected 'deleted' label in output:\n%s", output)
	}
}

func TestShow_FileAtSequence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/repos/default/default/-/file/README.md" {
			if r.URL.Query().Get("at") != "5" {
				t.Errorf("expected at=5, got %q", r.URL.Query().Get("at"))
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(model.FileResponse{
				Path:    "README.md",
				Content: []byte("hello world"),
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	app, out := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "main", "alice")

	if err := app.Show(5, "README.md"); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), "hello world") {
		t.Errorf("expected file content in output:\n%s", out.String())
	}
}

func TestShow_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(model.ErrorResponse{Error: "commit not found"})
	}))
	defer srv.Close()

	app, _ := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "main", "alice")

	err := app.Show(99, "")
	if err == nil || !strings.Contains(err.Error(), "99") {
		t.Errorf("expected error mentioning sequence 99, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Rebase
// ---------------------------------------------------------------------------

func TestRebase_Success(t *testing.T) {
	var gotReq model.RebaseRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/repos/default/default/-/rebase" {
			json.NewDecoder(r.Body).Decode(&gotReq)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(model.RebaseResponse{
				NewBaseSequence: 5,
				NewHeadSequence: 7,
				CommitsReplayed: 2,
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	app, out := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "feature/x", "alice")

	if err := app.Rebase(); err != nil {
		t.Fatal(err)
	}

	if gotReq.Branch != "feature/x" {
		t.Errorf("rebase branch = %q, want %q", gotReq.Branch, "feature/x")
	}

	// state.json should have updated head sequence.
	st, _ := app.loadState()
	if st.Sequence != 7 {
		t.Errorf("state.sequence = %d, want 7", st.Sequence)
	}

	output := out.String()
	if !strings.Contains(output, "Rebased") {
		t.Errorf("expected 'Rebased' in output:\n%s", output)
	}
	if !strings.Contains(output, "feature/x") {
		t.Errorf("expected branch name in output:\n%s", output)
	}
	if !strings.Contains(output, "2 commits replayed") {
		t.Errorf("expected commit count in output:\n%s", output)
	}
}

func TestRebase_Conflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/repos/default/default/-/rebase":
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(model.RebaseConflictError{
				Conflicts: []model.ConflictEntry{
					{Path: "README.md", MainVersionID: "v-main", BranchVersionID: "v-branch"},
				},
			})
		case r.Method == "GET" && r.URL.Path == "/repos/default/default/-/file/README.md":
			branch := r.URL.Query().Get("branch")
			if branch == "main" {
				json.NewEncoder(w).Encode(model.FileResponse{Path: "README.md", Content: []byte("main version")})
			} else {
				json.NewEncoder(w).Encode(model.FileResponse{Path: "README.md", Content: []byte("branch version")})
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	app, out := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "feature/x", "alice")

	err := app.Rebase()
	if err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Errorf("expected conflict error, got: %v", err)
	}

	// Conflict files should be written to disk.
	mainContent, readErr := os.ReadFile(filepath.Join(app.Dir, "README.md.main"))
	if readErr != nil {
		t.Fatalf("expected README.md.main to be written: %v", readErr)
	}
	if string(mainContent) != "main version" {
		t.Errorf("README.md.main = %q, want %q", string(mainContent), "main version")
	}

	branchContent, readErr := os.ReadFile(filepath.Join(app.Dir, "README.md.branch"))
	if readErr != nil {
		t.Fatalf("expected README.md.branch to be written: %v", readErr)
	}
	if string(branchContent) != "branch version" {
		t.Errorf("README.md.branch = %q, want %q", string(branchContent), "branch version")
	}

	if !strings.Contains(out.String(), "README.md") {
		t.Errorf("expected conflict path in output:\n%s", out.String())
	}
}

func TestRebase_OnMain(t *testing.T) {
	app, _ := newTestApp(t, nil)
	initWorkspace(t, app, "http://test", "main", "alice")

	err := app.Rebase()
	if err == nil || !strings.Contains(err.Error(), "cannot rebase main") {
		t.Errorf("expected 'cannot rebase main' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Resolve
// ---------------------------------------------------------------------------

func TestResolve_CommitsResolvedFile(t *testing.T) {
	var gotReq model.CommitRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/repos/default/default/-/commit" {
			json.NewDecoder(r.Body).Decode(&gotReq)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(model.CommitResponse{Sequence: 8})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	app, out := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "feature/x", "alice")

	// Write conflict files and the resolved file.
	writeFile(t, app, "README.md.main", "main version")
	writeFile(t, app, "README.md.branch", "branch version")
	writeFile(t, app, "README.md", "resolved content")

	if err := app.Resolve("README.md"); err != nil {
		t.Fatal(err)
	}

	if len(gotReq.Files) != 1 || gotReq.Files[0].Path != "README.md" {
		t.Errorf("expected commit for README.md, got: %v", gotReq.Files)
	}
	if string(gotReq.Files[0].Content) != "resolved content" {
		t.Errorf("committed content = %q, want %q", string(gotReq.Files[0].Content), "resolved content")
	}

	// State should be updated.
	st, _ := app.loadState()
	if st.Sequence != 8 {
		t.Errorf("state.sequence = %d, want 8", st.Sequence)
	}
	if _, ok := st.Files["README.md"]; !ok {
		t.Error("expected README.md in state files")
	}

	if !strings.Contains(out.String(), "resolved README.md") {
		t.Errorf("expected resolution confirmation in output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "sequence 8") {
		t.Errorf("expected sequence number in output:\n%s", out.String())
	}
}

func TestResolve_CleansUpConflictFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(model.CommitResponse{Sequence: 9})
	}))
	defer srv.Close()

	app, _ := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "feature/x", "alice")

	writeFile(t, app, "src/file.go.main", "main")
	writeFile(t, app, "src/file.go.branch", "branch")
	writeFile(t, app, "src/file.go", "resolved")

	if err := app.Resolve("src/file.go"); err != nil {
		t.Fatal(err)
	}

	// Conflict files should be removed.
	if _, err := os.Stat(filepath.Join(app.Dir, "src/file.go.main")); !os.IsNotExist(err) {
		t.Error("expected src/file.go.main to be removed")
	}
	if _, err := os.Stat(filepath.Join(app.Dir, "src/file.go.branch")); !os.IsNotExist(err) {
		t.Error("expected src/file.go.branch to be removed")
	}
	// Resolved file should still exist.
	if _, err := os.Stat(filepath.Join(app.Dir, "src/file.go")); err != nil {
		t.Errorf("expected src/file.go to still exist: %v", err)
	}
}

func TestResolve_NoConflictFiles(t *testing.T) {
	app, _ := newTestApp(t, nil)
	initWorkspace(t, app, "http://test", "feature/x", "alice")

	err := app.Resolve("README.md")
	if err == nil || !strings.Contains(err.Error(), "README.md.main") {
		t.Errorf("expected error about missing conflict file, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Branches
// ---------------------------------------------------------------------------

func TestBranches_ListsActive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/repos/default/default/-/branches" {
			if r.URL.Query().Get("status") != "active" {
				t.Errorf("expected status=active, got %q", r.URL.Query().Get("status"))
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]model.Branch{
				{Name: "feature/new-api", HeadSequence: 142, BaseSequence: 130, Status: model.BranchStatusActive},
				{Name: "bugfix/login", HeadSequence: 135, BaseSequence: 128, Status: model.BranchStatusActive},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	app, out := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "main", "alice")

	if err := app.Branches("active"); err != nil {
		t.Fatal(err)
	}

	output := out.String()
	if !strings.Contains(output, "feature/new-api") {
		t.Errorf("expected branch name in output:\n%s", output)
	}
	if !strings.Contains(output, "bugfix/login") {
		t.Errorf("expected second branch in output:\n%s", output)
	}
	if !strings.Contains(output, "142") {
		t.Errorf("expected head sequence in output:\n%s", output)
	}
}

func TestBranches_DefaultStatusActive(t *testing.T) {
	var gotStatus string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotStatus = r.URL.Query().Get("status")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]model.Branch{})
	}))
	defer srv.Close()

	app, _ := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "main", "alice")

	app.Branches("")
	if gotStatus != "active" {
		t.Errorf("expected status=active, got %q", gotStatus)
	}
}

// ---------------------------------------------------------------------------
// Reviews
// ---------------------------------------------------------------------------

func TestReviews_ListsWithStale(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/default/default/-/branches":
			json.NewEncoder(w).Encode([]model.Branch{
				{Name: "feature/x", HeadSequence: 10, BaseSequence: 5, Status: model.BranchStatusActive},
			})
		case r.Method == "GET" && r.URL.Path == "/repos/default/default/-/branch/feature/x/reviews":
			json.NewEncoder(w).Encode([]model.Review{
				{ID: "rev-1", Branch: "feature/x", Reviewer: "alice@example.com", Sequence: 10, Status: model.ReviewApproved, Body: "LGTM"},
				{ID: "rev-2", Branch: "feature/x", Reviewer: "bob@example.com", Sequence: 7, Status: model.ReviewRejected, Body: "needs tests"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	app, out := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "feature/x", "alice")

	if err := app.Reviews("feature/x"); err != nil {
		t.Fatal(err)
	}

	output := out.String()
	if !strings.Contains(output, "alice@example.com") {
		t.Errorf("expected reviewer alice in output:\n%s", output)
	}
	if !strings.Contains(output, "approved") {
		t.Errorf("expected 'approved' in output:\n%s", output)
	}
	if !strings.Contains(output, "[stale]") {
		t.Errorf("expected [stale] marker for old review:\n%s", output)
	}
}

func TestReviews_DefaultsToCurrentBranch(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/reviews") {
			gotPath = r.URL.Path
			json.NewEncoder(w).Encode([]model.Review{})
			return
		}
		json.NewEncoder(w).Encode([]model.Branch{
			{Name: "feature/current", HeadSequence: 5},
		})
	}))
	defer srv.Close()

	app, _ := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "feature/current", "alice")

	app.Reviews("") // empty branch → use current
	if !strings.Contains(gotPath, "feature") {
		t.Errorf("expected reviews for current branch, got path: %s", gotPath)
	}
}

func TestReviews_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/reviews"):
			json.NewEncoder(w).Encode([]model.Review{})
		default:
			json.NewEncoder(w).Encode([]model.Branch{{Name: "main", HeadSequence: 1}})
		}
	}))
	defer srv.Close()

	app, out := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "main", "alice")

	if err := app.Reviews("main"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No reviews") {
		t.Errorf("expected 'No reviews' in output:\n%s", out.String())
	}
}

// ---------------------------------------------------------------------------
// Review (submit)
// ---------------------------------------------------------------------------

func TestReviewSubmit_Approved(t *testing.T) {
	var gotReq model.CreateReviewRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/repos/default/default/-/review" {
			json.NewDecoder(r.Body).Decode(&gotReq)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(model.CreateReviewResponse{ID: "review-abc", Sequence: 10})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	app, out := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "feature/x", "alice")

	if err := app.Review("feature/x", "approved", "LGTM"); err != nil {
		t.Fatal(err)
	}

	if gotReq.Branch != "feature/x" {
		t.Errorf("branch = %q, want %q", gotReq.Branch, "feature/x")
	}
	if gotReq.Status != model.ReviewApproved {
		t.Errorf("status = %q, want %q", gotReq.Status, model.ReviewApproved)
	}
	if gotReq.Body != "LGTM" {
		t.Errorf("body = %q, want %q", gotReq.Body, "LGTM")
	}
	if !strings.Contains(out.String(), "approved") {
		t.Errorf("expected 'approved' in output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "review-abc") {
		t.Errorf("expected review id in output:\n%s", out.String())
	}
}

func TestReviewSubmit_DefaultBranch(t *testing.T) {
	var gotReq model.CreateReviewRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/repos/default/default/-/review" {
			json.NewDecoder(r.Body).Decode(&gotReq)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(model.CreateReviewResponse{ID: "r1", Sequence: 5})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	app, _ := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "feature/current", "alice")

	if err := app.Review("", "rejected", "needs work"); err != nil {
		t.Fatal(err)
	}

	if gotReq.Branch != "feature/current" {
		t.Errorf("branch = %q, want feature/current", gotReq.Branch)
	}
}

func TestReviewSubmit_InvalidStatus(t *testing.T) {
	app, _ := newTestApp(t, nil)
	initWorkspace(t, app, "http://test", "main", "alice")

	err := app.Review("main", "unknown", "")
	if err == nil || !strings.Contains(err.Error(), "status must be") {
		t.Errorf("expected status validation error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Checks
// ---------------------------------------------------------------------------

func TestChecks_ListsWithStale(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/default/default/-/branches":
			json.NewEncoder(w).Encode([]model.Branch{
				{Name: "feature/x", HeadSequence: 10},
			})
		case r.Method == "GET" && r.URL.Path == "/repos/default/default/-/branch/feature/x/checks":
			json.NewEncoder(w).Encode([]model.CheckRun{
				{ID: "chk-1", Branch: "feature/x", CheckName: "ci/build", Sequence: 10, Status: model.CheckRunPassed, Reporter: "ci-bot"},
				{ID: "chk-2", Branch: "feature/x", CheckName: "ci/lint", Sequence: 8, Status: model.CheckRunFailed, Reporter: "ci-bot"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	app, out := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "feature/x", "alice")

	if err := app.Checks("feature/x"); err != nil {
		t.Fatal(err)
	}

	output := out.String()
	if !strings.Contains(output, "ci/build") {
		t.Errorf("expected ci/build in output:\n%s", output)
	}
	if !strings.Contains(output, "passed") {
		t.Errorf("expected 'passed' in output:\n%s", output)
	}
	if !strings.Contains(output, "[stale]") {
		t.Errorf("expected [stale] marker for old check:\n%s", output)
	}
}

func TestChecks_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/checks"):
			json.NewEncoder(w).Encode([]model.CheckRun{})
		default:
			json.NewEncoder(w).Encode([]model.Branch{{Name: "main", HeadSequence: 1}})
		}
	}))
	defer srv.Close()

	app, out := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "main", "alice")

	if err := app.Checks("main"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No check runs") {
		t.Errorf("expected 'No check runs' in output:\n%s", out.String())
	}
}

// ---------------------------------------------------------------------------
// Check (submit)
// ---------------------------------------------------------------------------

func TestCheckSubmit_Passed(t *testing.T) {
	var gotReq model.CreateCheckRunRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/repos/default/default/-/check" {
			json.NewDecoder(r.Body).Decode(&gotReq)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(model.CreateCheckRunResponse{ID: "check-xyz", Sequence: 10})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	app, out := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "feature/x", "alice")

	if err := app.Check("feature/x", "ci/build", "passed"); err != nil {
		t.Fatal(err)
	}

	if gotReq.Branch != "feature/x" {
		t.Errorf("branch = %q, want %q", gotReq.Branch, "feature/x")
	}
	if gotReq.CheckName != "ci/build" {
		t.Errorf("check_name = %q, want %q", gotReq.CheckName, "ci/build")
	}
	if gotReq.Status != model.CheckRunPassed {
		t.Errorf("status = %q, want %q", gotReq.Status, model.CheckRunPassed)
	}
	if !strings.Contains(out.String(), "ci/build") {
		t.Errorf("expected check name in output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "check-xyz") {
		t.Errorf("expected check id in output:\n%s", out.String())
	}
}

func TestCheckSubmit_DefaultBranch(t *testing.T) {
	var gotReq model.CreateCheckRunRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/repos/default/default/-/check" {
			json.NewDecoder(r.Body).Decode(&gotReq)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(model.CreateCheckRunResponse{ID: "c1", Sequence: 5})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	app, _ := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "feature/current", "alice")

	if err := app.Check("", "ci/test", "failed"); err != nil {
		t.Fatal(err)
	}

	if gotReq.Branch != "feature/current" {
		t.Errorf("branch = %q, want feature/current", gotReq.Branch)
	}
}

func TestCheckSubmit_InvalidStatus(t *testing.T) {
	app, _ := newTestApp(t, nil)
	initWorkspace(t, app, "http://test", "main", "alice")

	err := app.Check("main", "ci/build", "unknown")
	if err == nil || !strings.Contains(err.Error(), "status must be") {
		t.Errorf("expected status validation error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// --branch override flag (ISSUE-9)
// ---------------------------------------------------------------------------

func TestReviews_ExplicitBranchOverride(t *testing.T) {
	var gotReviewsPath, gotBranchesPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			gotReviewsPath = r.URL.EscapedPath()
			json.NewEncoder(w).Encode([]model.Review{})
		case strings.HasSuffix(r.URL.Path, "/branches"):
			gotBranchesPath = r.URL.EscapedPath()
			json.NewEncoder(w).Encode([]model.Branch{
				{Name: "feature/other", HeadSequence: 3},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	app, _ := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "main", "alice") // workspace is on "main"

	app.Reviews("feature/other") // explicit --branch different from workspace branch

	wantReviewsPath := "/repos/default/default/-/branch/feature%2Fother/reviews"
	if gotReviewsPath != wantReviewsPath {
		t.Errorf("reviews URL = %q, want %q", gotReviewsPath, wantReviewsPath)
	}
	wantBranchesPath := "/repos/default/default/-/branches"
	if gotBranchesPath != wantBranchesPath {
		t.Errorf("branches URL = %q, want %q", gotBranchesPath, wantBranchesPath)
	}
}

func TestChecks_ExplicitBranchOverride(t *testing.T) {
	var gotChecksPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/checks"):
			gotChecksPath = r.URL.EscapedPath()
			json.NewEncoder(w).Encode([]model.CheckRun{})
		default:
			json.NewEncoder(w).Encode([]model.Branch{
				{Name: "feature/other", HeadSequence: 3},
			})
		}
	}))
	defer srv.Close()

	app, _ := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "main", "alice")

	app.Checks("feature/other")

	wantChecksPath := "/repos/default/default/-/branch/feature%2Fother/checks"
	if gotChecksPath != wantChecksPath {
		t.Errorf("checks URL = %q, want %q", gotChecksPath, wantChecksPath)
	}
}

func TestReviewSubmit_ExplicitBranchOverride(t *testing.T) {
	var gotReq model.CreateReviewRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/repos/default/default/-/review" {
			json.NewDecoder(r.Body).Decode(&gotReq)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(model.CreateReviewResponse{ID: "r-override", Sequence: 7})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	app, _ := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "main", "alice") // workspace is on "main"

	if err := app.Review("feature/other", "approved", "LGTM"); err != nil {
		t.Fatal(err)
	}
	if gotReq.Branch != "feature/other" {
		t.Errorf("branch in request body = %q, want %q", gotReq.Branch, "feature/other")
	}
}

func TestCheckSubmit_ExplicitBranchOverride(t *testing.T) {
	var gotReq model.CreateCheckRunRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/repos/default/default/-/check" {
			json.NewDecoder(r.Body).Decode(&gotReq)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(model.CreateCheckRunResponse{ID: "c-override", Sequence: 7})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	app, _ := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "main", "alice") // workspace is on "main"

	if err := app.Check("feature/other", "ci/build", "passed"); err != nil {
		t.Fatal(err)
	}
	if gotReq.Branch != "feature/other" {
		t.Errorf("branch in request body = %q, want %q", gotReq.Branch, "feature/other")
	}
}

// mustParseTime parses a date string for use in tests.
func mustParseTime(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

// ---------------------------------------------------------------------------
// ImportGit
// ---------------------------------------------------------------------------

// initTestGitRepo creates a minimal git repo in dir with the given commits.
// Each commit is a map of filename -> content strings.
func initTestGitRepo(t *testing.T, commits []map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test User",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test User",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test User")

	for i, files := range commits {
		for name, content := range files {
			full := filepath.Join(dir, name)
			if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(content), 0644); err != nil {
				t.Fatal(err)
			}
			run("add", name)
		}
		run("commit", "-m", fmt.Sprintf("Commit %d", i+1))
	}
	return dir
}

func TestImportGitReplay(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	gitDir := initTestGitRepo(t, []map[string]string{
		{"hello.txt": "hello world\n"},
		{"world.txt": "goodbye\n"},
	})

	var capturedReqs []model.CommitRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/commit") {
			var req model.CommitRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			capturedReqs = append(capturedReqs, req)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(model.CommitResponse{Sequence: int64(len(capturedReqs))})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	app, out := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "main", "importer")

	if err := app.ImportGit(gitDir, "replay"); err != nil {
		t.Fatalf("ImportGit replay: %v", err)
	}

	// Should have 2 commits (one per git commit).
	if len(capturedReqs) != 2 {
		t.Fatalf("expected 2 commit requests, got %d", len(capturedReqs))
	}

	// First commit: hello.txt added.
	if !strings.Contains(capturedReqs[0].Message, "[git-author: test@example.com]") {
		t.Errorf("message[0] = %q, want [git-author: ...] prefix", capturedReqs[0].Message)
	}
	if capturedReqs[0].Branch != "main" {
		t.Errorf("branch[0] = %q, want main", capturedReqs[0].Branch)
	}
	found := false
	for _, f := range capturedReqs[0].Files {
		if f.Path == "hello.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected hello.txt in first commit files")
	}

	// Second commit: world.txt added.
	found = false
	for _, f := range capturedReqs[1].Files {
		if f.Path == "world.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected world.txt in second commit files")
	}

	outStr := out.String()
	if !strings.Contains(outStr, "Importing 2 commits") {
		t.Errorf("output missing 'Importing 2 commits': %s", outStr)
	}
	if !strings.Contains(outStr, "Done. 2 commits imported.") {
		t.Errorf("output missing 'Done.' line: %s", outStr)
	}
}

func TestImportGitSquash(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	gitDir := initTestGitRepo(t, []map[string]string{
		{"a.txt": "aaa\n", "b.txt": "bbb\n"},
	})

	var capturedReqs []model.CommitRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/commit") {
			var req model.CommitRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			capturedReqs = append(capturedReqs, req)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(model.CommitResponse{Sequence: 1})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	app, out := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "main", "importer")

	if err := app.ImportGit(gitDir, "squash"); err != nil {
		t.Fatalf("ImportGit squash: %v", err)
	}

	// Should have exactly 1 commit.
	if len(capturedReqs) != 1 {
		t.Fatalf("expected 1 commit request, got %d", len(capturedReqs))
	}

	req := capturedReqs[0]
	if !strings.Contains(req.Message, "[git-import] Squashed import of") {
		t.Errorf("squash message = %q, want [git-import] Squashed import of...", req.Message)
	}
	if req.Branch != "main" {
		t.Errorf("branch = %q, want main", req.Branch)
	}
	if len(req.Files) != 2 {
		t.Errorf("expected 2 files, got %d", len(req.Files))
	}

	outStr := out.String()
	if !strings.Contains(outStr, "Collecting files from HEAD") {
		t.Errorf("output missing 'Collecting files' line: %s", outStr)
	}
	if !strings.Contains(outStr, "Done. 1 commit imported") {
		t.Errorf("output missing 'Done.' line: %s", outStr)
	}
}

func TestImportGitInvalidMode(t *testing.T) {
	app, _ := newTestApp(t, nil)
	initWorkspace(t, app, "http://localhost", "main", "user")
	err := app.ImportGit(t.TempDir(), "badmode")
	if err == nil || !strings.Contains(err.Error(), "mode must be") {
		t.Errorf("expected mode error, got %v", err)
	}
}

func TestImportGitNotARepo(t *testing.T) {
	app, _ := newTestApp(t, nil)
	initWorkspace(t, app, "http://localhost", "main", "user")
	dir := t.TempDir() // no .git
	err := app.ImportGit(dir, "replay")
	if err == nil || !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("expected 'not a git repository' error, got %v", err)
	}
}

func TestImportGitDeletedFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	// First commit adds a file, second commit deletes it.
	gitDir := initTestGitRepo(t, []map[string]string{
		{"toDelete.txt": "gone\n"},
	})
	// Add a delete commit.
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = gitDir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test User",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test User",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.Remove(filepath.Join(gitDir, "toDelete.txt")); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-m", "Delete toDelete.txt")

	var capturedReqs []model.CommitRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/commit") {
			var req model.CommitRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			capturedReqs = append(capturedReqs, req)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(model.CommitResponse{Sequence: int64(len(capturedReqs))})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	app, _ := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "main", "importer")

	if err := app.ImportGit(gitDir, "replay"); err != nil {
		t.Fatalf("ImportGit replay: %v", err)
	}

	if len(capturedReqs) != 2 {
		t.Fatalf("expected 2 commit requests, got %d", len(capturedReqs))
	}

	// Second commit should have a delete (nil content after JSON unmarshal = empty/missing).
	deleteReq := capturedReqs[1]
	foundDelete := false
	for _, f := range deleteReq.Files {
		if f.Path == "toDelete.txt" && f.Content == nil {
			foundDelete = true
		}
	}
	if !foundDelete {
		t.Errorf("expected deleted file with nil content in second commit; got %+v", deleteReq.Files)
	}
}

func TestImportGitReplayBinaryFile(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test User",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test User",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test User")

	binData := []byte{0xFF, 0xFE, 0x00, 0x01}
	if err := os.WriteFile(filepath.Join(dir, "image.png"), binData, 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "image.png")
	run("commit", "-m", "Add binary image")

	var capturedReqs []model.CommitRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/commit") {
			var req model.CommitRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			capturedReqs = append(capturedReqs, req)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(model.CommitResponse{Sequence: int64(len(capturedReqs))})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	app, _ := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "main", "importer")

	if err := app.ImportGit(dir, "replay"); err != nil {
		t.Fatalf("ImportGit replay: %v", err)
	}

	if len(capturedReqs) != 1 {
		t.Fatalf("expected 1 commit request, got %d", len(capturedReqs))
	}

	foundPNG := false
	for _, f := range capturedReqs[0].Files {
		if f.Path == "image.png" && f.ContentType == "image/png" {
			foundPNG = true
		}
	}
	if !foundPNG {
		t.Errorf("expected image.png with ContentType image/png; got %+v", capturedReqs[0].Files)
	}
}

func TestImportGitDefaultMode(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	gitDir := initTestGitRepo(t, []map[string]string{
		{"readme.txt": "hello\n"},
	})

	var capturedReqs []model.CommitRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/commit") {
			var req model.CommitRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			capturedReqs = append(capturedReqs, req)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(model.CommitResponse{Sequence: int64(len(capturedReqs))})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	app, _ := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "main", "importer")

	// Empty string mode should default to "replay".
	if err := app.ImportGit(gitDir, ""); err != nil {
		t.Fatalf("ImportGit default mode: %v", err)
	}

	if len(capturedReqs) != 1 {
		t.Fatalf("expected 1 commit request, got %d", len(capturedReqs))
	}
}

// ---------------------------------------------------------------------------
// Chain verification / pull tests
// ---------------------------------------------------------------------------

// buildChainHash replicates the server-side hash formula for tests.
func buildChainHash(prevHash string, seq int64, repo, branch, author, message string, createdAt time.Time, files []chainFile) string {
	return computeChainHash(prevHash, seq, repo, branch, author, message, createdAt, files)
}

// newChainEntry builds a minimal chainEntry with a correct commit_hash for testing.
func newChainEntry(prevHash string, seq int64, repo, branch, author, msg string, ts time.Time) chainEntry {
	hash := buildChainHash(prevHash, seq, repo, branch, author, msg, ts, nil)
	return chainEntry{
		Sequence:   seq,
		Branch:     branch,
		Author:     author,
		Message:    msg,
		CreatedAt:  ts,
		CommitHash: &hash,
		Files:      []chainFile{},
	}
}

// newPullServer builds a mock HTTP server that handles the standard pull flow:
// tree listing, branches (with headSeq), and a chain endpoint returning entries.
func newPullServer(t *testing.T, branch string, headSeq int64, chainEntries []chainEntry) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/default/default/-/tree":
			json.NewEncoder(w).Encode([]treeEntry{})
		case r.Method == "GET" && r.URL.Path == "/repos/default/default/-/branches":
			json.NewEncoder(w).Encode([]model.Branch{{Name: branch, HeadSequence: headSeq}})
		case r.Method == "GET" && r.URL.Path == "/repos/default/default/-/chain":
			json.NewEncoder(w).Encode(chainEntries)
		default:
			http.NotFound(w, r)
		}
	}))
}

// TestPull_TOFU verifies that the first pull (no stored CommitHash) stores the tip hash.
func TestPull_TOFU(t *testing.T) {
	ts := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	e1 := newChainEntry(genesisHash, 1, "default/default", "main", "alice", "first", ts)

	srv := newPullServer(t, "main", 1, []chainEntry{e1})
	defer srv.Close()

	app, _ := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "main", "alice")

	if err := app.Pull(); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	st, err := app.loadState()
	if err != nil {
		t.Fatal(err)
	}
	if st.CommitHash == "" {
		t.Fatal("expected CommitHash to be set after TOFU pull")
	}
	if st.CommitHash != *e1.CommitHash {
		t.Errorf("expected CommitHash %q, got %q", *e1.CommitHash, st.CommitHash)
	}
}

// TestPull_VerificationSuccess verifies that a subsequent pull with a valid chain passes.
func TestPull_VerificationSuccess(t *testing.T) {
	ts := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	e1 := newChainEntry(genesisHash, 1, "default/default", "main", "alice", "first", ts)
	ts2 := ts.Add(time.Second)
	e2 := newChainEntry(*e1.CommitHash, 2, "default/default", "main", "alice", "second", ts2)

	srv := newPullServer(t, "main", 2, []chainEntry{e1, e2})
	defer srv.Close()

	app, _ := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "main", "alice")

	// Pre-seed state as if we already did a TOFU pull at seq 1.
	st := &State{Branch: "main", Sequence: 1, Files: make(map[string]string), CommitHash: *e1.CommitHash}
	if err := app.saveState(st); err != nil {
		t.Fatal(err)
	}

	if err := app.Pull(); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	st2, _ := app.loadState()
	if st2.CommitHash != *e2.CommitHash {
		t.Errorf("expected updated CommitHash %q, got %q", *e2.CommitHash, st2.CommitHash)
	}
}

// TestPull_VerificationFailure verifies that a wrong hash causes an error.
func TestPull_VerificationFailure(t *testing.T) {
	ts := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	e1 := newChainEntry(genesisHash, 1, "default/default", "main", "alice", "first", ts)
	ts2 := ts.Add(time.Second)
	// Build a second entry with a wrong hash.
	badHash := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	e2Wrong := chainEntry{
		Sequence:   2,
		Branch:     "main",
		Author:     "alice",
		Message:    "second",
		CreatedAt:  ts2,
		CommitHash: &badHash,
		Files:      []chainFile{},
	}

	srv := newPullServer(t, "main", 2, []chainEntry{e1, e2Wrong})
	defer srv.Close()

	app, _ := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "main", "alice")

	st := &State{Branch: "main", Sequence: 1, Files: make(map[string]string), CommitHash: *e1.CommitHash}
	if err := app.saveState(st); err != nil {
		t.Fatal(err)
	}

	err := app.Pull()
	if err == nil {
		t.Fatal("expected chain integrity error, got nil")
	}
	if !strings.Contains(err.Error(), "chain integrity error") {
		t.Errorf("expected 'chain integrity error' in error, got: %v", err)
	}
}

// TestVerify_HappyPath verifies ds verify prints OK for a valid chain.
func TestVerify_HappyPath(t *testing.T) {
	ts := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	e1 := newChainEntry(genesisHash, 1, "default/default", "main", "alice", "first", ts)
	ts2 := ts.Add(time.Second)
	e2 := newChainEntry(*e1.CommitHash, 2, "default/default", "main", "alice", "second", ts2)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/default/default/-/chain":
			json.NewEncoder(w).Encode([]chainEntry{e1, e2})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	app, out := newTestApp(t, srv)
	initWorkspace(t, app, srv.URL, "main", "alice")
	st := &State{Branch: "main", Sequence: 2, Files: make(map[string]string)}
	if err := app.saveState(st); err != nil {
		t.Fatal(err)
	}

	if err := app.Verify(); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	output := out.String()
	// First entry is the anchor (no recomputation); second is verified.
	if !strings.Contains(output, "OK") {
		t.Errorf("expected OK in output: %s", output)
	}
}
