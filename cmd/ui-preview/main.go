// Command ui-preview boots the minimal docstore UI against in-memory fake data.
// Run `go run ./cmd/ui-preview` and visit http://localhost:8090/ui/ to preview
// the UI without a database.
//
// This command is for local preview only and is not wired into production.
package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/dlorenc/docstore/api"
	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/store"
	"github.com/dlorenc/docstore/internal/ui"
)

// ---------------------------------------------------------------------------
// Fake data
// ---------------------------------------------------------------------------

type fakeRead struct{}

func (fakeRead) MaterializeTree(_ context.Context, _ , _ string, _ *int64, _ int, _ string) ([]store.TreeEntry, error) {
	return []store.TreeEntry{
		{Path: "README.md", VersionID: "v-readme-01", ContentHash: "sha256:a1b2c3d4e5f6a7b8c9d0"},
		{Path: "docs/intro.md", VersionID: "v-intro-02", ContentHash: "sha256:abc123def456789abcd0"},
		{Path: "docs/architecture.md", VersionID: "v-arch-03", ContentHash: "sha256:def456abc789123def00"},
		{Path: "docs/guides/onboarding.md", VersionID: "v-onb-04", ContentHash: "sha256:111222333444555666ff"},
		{Path: "policies/reviewers.rego", VersionID: "v-pol-05", ContentHash: "sha256:999888777666aaa0bbb0"},
		{Path: "OWNERS", VersionID: "v-own-06", ContentHash: "sha256:000111222333444555aa"},
	}, nil
}

func (fakeRead) GetFile(_ context.Context, _, _, path string, _ *int64) (*store.FileContent, error) {
	content := map[string][]byte{
		"README.md":                  []byte("# acme/platform\n\nShared configuration and policy for Acme Corp.\n\nSee `docs/intro.md` to get started.\n"),
		"docs/intro.md":              []byte("# Introduction\n\nThis repo stores **versioned structured data**. All writes go\nthrough a proposal workflow with CI checks and reviewer policy gates.\n\nNew here? Read `docs/guides/onboarding.md` next.\n"),
		"docs/architecture.md":       []byte("# Architecture\n\n- Content-addressable blob store\n- Postgres-backed branch/commit metadata\n- OPA-enforced merge policy\n- Agent-friendly JSON + minimal web UI\n"),
		"docs/guides/onboarding.md":  []byte("# Onboarding\n\n1. `ds orgs create acme`\n2. `ds repos create acme/platform`\n3. `ds roles set acme/platform you@example.com writer`\n4. Start branching!\n"),
		"policies/reviewers.rego":    []byte("package docstore.policy\n\n# Require at least one approval before merge.\ndeny[msg] {\n    count({r | r := input.reviews[_]; r.status == \"approved\"}) == 0\n    msg := \"at least one approval required\"\n}\n"),
		"OWNERS":                     []byte("# Path-scoped reviewers.\n* @ajay @sam\n/policies/ @security-team\n/docs/ @docs-team\n"),
	}
	if c, ok := content[path]; ok {
		return &store.FileContent{Path: path, VersionID: "v-" + path, ContentHash: "sha256:preview", Content: c, ContentType: "text/markdown"}, nil
	}
	return nil, nil
}

func (fakeRead) GetBranch(_ context.Context, _, _ string) (*store.BranchInfo, error) { return nil, nil }

func (fakeRead) ListBranches(_ context.Context, repo, _ string, _, _ bool) ([]store.BranchInfo, error) {
	if repo != "acme/platform" {
		return []store.BranchInfo{{Name: "main", HeadSequence: 3, BaseSequence: 0, Status: "active"}}, nil
	}
	return []store.BranchInfo{
		{Name: "main", HeadSequence: 42, BaseSequence: 0, Status: "active"},
		{Name: "add-onboarding-guide", HeadSequence: 47, BaseSequence: 42, Status: "active"},
		{Name: "tighten-reviewer-policy", HeadSequence: 45, BaseSequence: 42, Status: "active", AutoMerge: true},
		{Name: "wip-refactor", HeadSequence: 44, BaseSequence: 42, Status: "active", Draft: true},
		{Name: "bump-arch-doc", HeadSequence: 48, BaseSequence: 42, Status: "merged"},
		{Name: "old-experiment", HeadSequence: 31, BaseSequence: 20, Status: "abandoned"},
	}, nil
}

type fakeWrite struct{}

func (fakeWrite) ListRepos(_ context.Context) ([]model.Repo, error) {
	t := time.Now()
	return []model.Repo{
		{Name: "acme/platform", Owner: "acme", CreatedBy: "ajay@acme", CreatedAt: t.Add(-30 * 24 * time.Hour)},
		{Name: "acme/policies", Owner: "acme", CreatedBy: "sam@acme", CreatedAt: t.Add(-14 * 24 * time.Hour)},
		{Name: "acme/docs", Owner: "acme", CreatedBy: "ajay@acme", CreatedAt: t.Add(-3 * 24 * time.Hour)},
		{Name: "beta/experiments", Owner: "beta", CreatedBy: "lee@beta", CreatedAt: t.Add(-2 * time.Hour)},
	}, nil
}

func (fakeWrite) ListOrgs(_ context.Context) ([]model.Org, error) { return nil, nil }

func (fakeWrite) GetRepo(_ context.Context, name string) (*model.Repo, error) {
	all, _ := (fakeWrite{}).ListRepos(context.Background())
	for _, r := range all {
		if r.Name == name {
			return &r, nil
		}
	}
	return nil, db.ErrRepoNotFound
}

func fakeAssemble(_ context.Context, _, branch string) (*model.AgentContextResponse, error) {
	t := time.Now()
	vid := func(s string) *string { return &s }

	switch branch {
	case "add-onboarding-guide":
		logURL := "https://ci.example/logs/1"
		return &model.AgentContextResponse{
			Branch: model.Branch{Name: branch, HeadSequence: 47, BaseSequence: 42, Status: model.BranchStatusActive},
			Diff: model.DiffResponse{
				BranchChanges: []model.DiffEntry{
					{Path: "docs/guides/onboarding.md", VersionID: vid("v-onb-new")},
					{Path: "docs/intro.md", VersionID: vid("v-intro-02b")},
					{Path: "docs/legacy.md", VersionID: nil},
				},
				MainChanges: []model.DiffEntry{
					{Path: "README.md", VersionID: vid("v-readme-02")},
				},
			},
			Reviews: []model.Review{
				{ID: "r1", Reviewer: "sam@acme", Status: model.ReviewApproved, Sequence: 47, CreatedAt: t.Add(-40 * time.Minute), Body: "LGTM, nice guide."},
				{ID: "r2", Reviewer: "lee@acme", Status: model.ReviewDismissed, Sequence: 46, CreatedAt: t.Add(-2 * time.Hour), Body: "stale after new commits"},
			},
			CheckRuns: []model.CheckRun{
				{ID: "c1", CheckName: "markdown-lint", Status: model.CheckRunPassed, Reporter: "ci", Sequence: 47, CreatedAt: t.Add(-30 * time.Minute)},
				{ID: "c2", CheckName: "link-check", Status: model.CheckRunPending, Reporter: "ci", Sequence: 47, CreatedAt: t.Add(-10 * time.Minute), LogURL: &logURL},
				{ID: "c3", CheckName: "spell-check", Status: model.CheckRunFailed, Reporter: "ci", Sequence: 47, CreatedAt: t.Add(-25 * time.Minute), LogURL: &logURL},
			},
			Proposals: []model.Proposal{
				{ID: "p1", Branch: branch, BaseBranch: "main", Title: "docs: add onboarding guide", Description: "Fills the gap new contributors keep asking about in slack.", Author: "ajay@acme", State: model.ProposalOpen, CreatedAt: t.Add(-3 * time.Hour), UpdatedAt: t.Add(-30 * time.Minute)},
			},
			LinkedIssues: []model.Issue{
				{ID: "i1", Number: 128, Title: "New contributor onboarding is unclear", Author: "new-dev@acme", State: model.IssueStateOpen, CreatedAt: t.Add(-5 * 24 * time.Hour)},
			},
			Policies: []model.PolicyResult{
				{Name: "at-least-one-approval", Pass: true, Reason: "sam@acme approved"},
				{Name: "all-checks-passed", Pass: false, Reason: "spell-check failed, link-check pending"},
				{Name: "owner-review-required", Pass: true, Reason: "docs-team approved via sam@acme"},
			},
			RecentCommits: []api.ChainEntry{
				{Sequence: 47, Branch: branch, Author: "ajay@acme", Message: "address review comments", CreatedAt: t.Add(-30 * time.Minute)},
				{Sequence: 46, Branch: branch, Author: "ajay@acme", Message: "add architecture cross-link", CreatedAt: t.Add(-2 * time.Hour)},
				{Sequence: 45, Branch: branch, Author: "ajay@acme", Message: "initial onboarding guide", CreatedAt: t.Add(-3 * time.Hour)},
			},
			Mergeable: false,
		}, nil

	case "tighten-reviewer-policy":
		return &model.AgentContextResponse{
			Branch: model.Branch{Name: branch, HeadSequence: 45, BaseSequence: 42, Status: model.BranchStatusActive, AutoMerge: true},
			Diff: model.DiffResponse{
				BranchChanges: []model.DiffEntry{{Path: "policies/reviewers.rego", VersionID: vid("v-pol-new")}},
			},
			Reviews: []model.Review{
				{ID: "r3", Reviewer: "security@acme", Status: model.ReviewApproved, Sequence: 45, CreatedAt: t.Add(-1 * time.Hour)},
			},
			CheckRuns: []model.CheckRun{
				{ID: "c4", CheckName: "rego-syntax", Status: model.CheckRunPassed, Reporter: "ci", Sequence: 45, CreatedAt: t.Add(-50 * time.Minute)},
				{ID: "c5", CheckName: "policy-sim", Status: model.CheckRunPassed, Reporter: "ci", Sequence: 45, CreatedAt: t.Add(-40 * time.Minute)},
			},
			Policies: []model.PolicyResult{
				{Name: "owner-review-required", Pass: true, Reason: "security-team approved"},
				{Name: "all-checks-passed", Pass: true, Reason: "all 2 checks passed"},
			},
			Mergeable: true,
		}, nil
	}
	return nil, &notFoundErr{}
}

type notFoundErr struct{}

func (e *notFoundErr) Error() string { return "branch not found" }

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	h, err := ui.NewHandler(fakeRead{}, fakeWrite{}, fakeAssemble)
	if err != nil {
		log.Fatalf("ui init: %v", err)
	}
	mux := http.NewServeMux()
	h.Register(mux)

	// Redirect bare "/" to /ui/ so the preview URL is short.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusFound)
	})

	addr := "127.0.0.1:8090"
	log.Printf("docstore UI preview: http://%s/ui/", addr)
	log.Printf("try: http://%s/ui/r/acme/platform", addr)
	log.Printf("try: http://%s/ui/r/acme/platform/b/add-onboarding-guide", addr)
	log.Printf("try: http://%s/ui/r/acme/platform/f/docs/intro.md?branch=main", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
