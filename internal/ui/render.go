package ui

import (
	"bytes"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// templateSet holds the parsed template tree. Each page has its own *Template
// so it can define its own "content" block against the shared layout.
type templateSet struct {
	repos          *template.Template
	branches       *template.Template
	branchDetail   *template.Template
	branchChecks   *template.Template
	checkHistory   *template.Template
	reviewComments *template.Template
	fileView       *template.Template
	errorPage      *template.Template
	commitLog      *template.Template
	logRows        *template.Template
	commitDetail   *template.Template
	proposals      *template.Template
	proposalsRows  *template.Template
	proposalDetail *template.Template
	releases       *template.Template
	releaseDetail  *template.Template
}

func parseTemplates(root fs.FS) (*templateSet, error) {
	load := func(page string) (*template.Template, error) {
		return template.New("layout.html").
			Funcs(funcMap()).
			ParseFS(root, "templates/layout.html", "templates/"+page)
	}
	loadFragment := func(name string) (*template.Template, error) {
		return template.New(name).
			Funcs(funcMap()).
			ParseFS(root, "templates/"+name)
	}

	repos, err := load("repos.html")
	if err != nil {
		return nil, fmt.Errorf("parse repos: %w", err)
	}
	branches, err := load("branches.html")
	if err != nil {
		return nil, fmt.Errorf("parse branches: %w", err)
	}
	// branch_detail pulls in _diff.html and branch_checks.html as partials so
	// the initial render shows the checks table without a separate HTMX call.
	branchDetail, err := template.New("layout.html").
		Funcs(funcMap()).
		ParseFS(root,
			"templates/layout.html",
			"templates/branch_detail.html",
			"templates/_diff.html",
			"templates/branch_checks.html")
	if err != nil {
		return nil, fmt.Errorf("parse branch_detail: %w", err)
	}
	branchChecks, err := loadFragment("branch_checks.html")
	if err != nil {
		return nil, fmt.Errorf("parse branch_checks: %w", err)
	}
	checkHistory, err := loadFragment("check_history.html")
	if err != nil {
		return nil, fmt.Errorf("parse check_history: %w", err)
	}
	reviewComments, err := loadFragment("review_comments.html")
	if err != nil {
		return nil, fmt.Errorf("parse review_comments: %w", err)
	}
	fileView, err := load("file.html")
	if err != nil {
		return nil, fmt.Errorf("parse file: %w", err)
	}
	errorPage, err := load("error.html")
	if err != nil {
		return nil, fmt.Errorf("parse error: %w", err)
	}
	// log.html pulls in log_rows.html so the initial render shows the first
	// page of rows inline, matching the same pattern as branch_detail.
	commitLog, err := template.New("layout.html").
		Funcs(funcMap()).
		ParseFS(root,
			"templates/layout.html",
			"templates/log.html",
			"templates/log_rows.html")
	if err != nil {
		return nil, fmt.Errorf("parse log: %w", err)
	}
	logRows, err := loadFragment("log_rows.html")
	if err != nil {
		return nil, fmt.Errorf("parse log_rows: %w", err)
	}
	commitDetail, err := load("commit.html")
	if err != nil {
		return nil, fmt.Errorf("parse commit: %w", err)
	}
	// proposals full page pulls in proposals_rows.html as a partial.
	proposals, err := template.New("layout.html").
		Funcs(funcMap()).
		ParseFS(root,
			"templates/layout.html",
			"templates/proposals.html",
			"templates/proposals_rows.html")
	if err != nil {
		return nil, fmt.Errorf("parse proposals: %w", err)
	}
	proposalsRows, err := loadFragment("proposals_rows.html")
	if err != nil {
		return nil, fmt.Errorf("parse proposals_rows: %w", err)
	}
	proposalDetail, err := load("proposal_detail.html")
	if err != nil {
		return nil, fmt.Errorf("parse proposal_detail: %w", err)
	}
	releases, err := load("releases.html")
	if err != nil {
		return nil, fmt.Errorf("parse releases: %w", err)
	}
	releaseDetail, err := load("release_detail.html")
	if err != nil {
		return nil, fmt.Errorf("parse release_detail: %w", err)
	}
	return &templateSet{
		repos:          repos,
		branches:       branches,
		branchDetail:   branchDetail,
		branchChecks:   branchChecks,
		checkHistory:   checkHistory,
		reviewComments: reviewComments,
		fileView:       fileView,
		errorPage:      errorPage,
		commitLog:      commitLog,
		logRows:        logRows,
		commitDetail:   commitDetail,
		proposals:      proposals,
		proposalsRows:  proposalsRows,
		proposalDetail: proposalDetail,
		releases:       releases,
		releaseDetail:  releaseDetail,
	}, nil
}

// render executes the named template against data and writes it to w as HTML.
// On execution error it logs and emits a 500; templates must not panic.
func (h *Handler) render(w http.ResponseWriter, t *template.Template, name string, data any) {
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		slog.Error("ui render error", "template", name, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

// renderError renders the error page with the given status.
func (h *Handler) renderError(w http.ResponseWriter, status int, message string) {
	var buf bytes.Buffer
	data := pageData{
		Title: fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Err: errorInfo{
			Status:  status,
			Message: message,
		},
	}
	if err := h.tmpl.errorPage.ExecuteTemplate(&buf, "layout.html", data); err != nil {
		http.Error(w, message, status)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// ---------------------------------------------------------------------------
// Template data structures
// ---------------------------------------------------------------------------

type pageData struct {
	Title       string
	Breadcrumbs []crumb
	Body        any
	Err         errorInfo
}

type crumb struct {
	Label string
	Href  string
}

type errorInfo struct {
	Status  int
	Message string
}

// ---------------------------------------------------------------------------
// Template FuncMap
// ---------------------------------------------------------------------------

func funcMap() template.FuncMap {
	return template.FuncMap{
		"relTime":    relTime,
		"shortHash":  shortHash,
		"statusClass": statusClass,
		"diffClass":  diffClass,
		"joinPath":   joinPath,
		"safeContent": safeContent,
		"hasPrefix":  strings.HasPrefix,
		"lines":      splitLines,
		"dict":       dict,
		"add":        func(a, b int) int { return a + b },
	}
}

// dict builds a map from alternating key/value pairs for template composition:
//   {{template "x" (dict "Title" "Foo" "Rows" .Bar)}}
func dict(kv ...any) map[string]any {
	m := make(map[string]any, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			continue
		}
		m[k] = kv[i+1]
	}
	return m
}

func relTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("2006-01-02")
	}
}

func shortHash(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}

// statusClass maps domain status strings to stable CSS class names.
// "ok" is reserved for genuine success states (passed, approved); lifecycle
// states like "active" and "merged" map to "neutral" so they don't compete
// visually with the mergeable verdict on the branch detail page.
func statusClass(s string) string {
	switch strings.ToLower(s) {
	case "passed", "approved", "open":
		return "ok"
	case "failed", "rejected", "abandoned":
		return "bad"
	case "pending", "dismissed", "draft":
		return "warn"
	case "active", "merged", "closed":
		return "neutral"
	default:
		return "neutral"
	}
}

// diffClass returns a CSS class for a diff entry based on whether it is an add,
// delete, or modify. An entry with a nil VersionID is a deletion.
func diffClass(versionID *string) string {
	if versionID == nil {
		return "del"
	}
	return "add"
}

func joinPath(parts ...string) string {
	out := strings.Builder{}
	for i, p := range parts {
		if i > 0 {
			out.WriteByte('/')
		}
		out.WriteString(p)
	}
	return out.String()
}

// safeContent returns the text as a safe HTML fragment if it looks like text,
// else a placeholder. Used to avoid blowing up on binary files.
func safeContent(b []byte) template.HTML {
	if !isProbablyText(b) {
		return template.HTML("<em>binary file (not shown)</em>")
	}
	return template.HTML(template.HTMLEscapeString(string(b)))
}

func isProbablyText(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	// If any byte in the first 512 is null, treat as binary.
	n := 512
	if len(b) < n {
		n = len(b)
	}
	for _, c := range b[:n] {
		if c == 0 {
			return false
		}
	}
	return true
}

// splitLines returns the file content split into lines, each annotated with
// its 1-indexed line number, for numbered rendering.
type numberedLine struct {
	Num  int
	Text string
}

func splitLines(b []byte) []numberedLine {
	if len(b) == 0 {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	out := make([]numberedLine, len(lines))
	for i, l := range lines {
		out[i] = numberedLine{Num: i + 1, Text: l}
	}
	return out
}

