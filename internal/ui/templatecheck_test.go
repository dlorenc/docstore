package ui

import (
	"html/template"
	"testing"

	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/store"
	"github.com/jba/templatecheck"
)

// TestTemplateTypes runs templatecheck against every parsed template to catch
// type errors—missing fields, wrong argument types, invalid slice/index ops—at
// test time rather than at runtime.  The subtests mirror the templateSet fields
// so that a single failing template is easy to identify.
//
// Each page template's "content" sub-template is checked directly with its
// concrete page type so that templatecheck can follow types through the full
// template body. Fragment templates that don't use the layout are checked with
// their own data type directly.
//
// Keep this file alongside the templates as a permanent regression guard:
// template changes that introduce type errors will be caught here first.
func TestTemplateTypes(t *testing.T) {
	tmpl, err := parseTemplates(templatesFS)
	if err != nil {
		t.Fatalf("parseTemplates: %v", err)
	}

	check := func(t *testing.T, tmpl *template.Template, data any) {
		t.Helper()
		if err := templatecheck.CheckHTML(tmpl, data); err != nil {
			t.Error(err)
		}
	}

	// layout checks the layout-level fields (.Title, .Breadcrumbs, .Identity)
	// with an empty pageData. The body is nil/unknown so content is unchecked
	// here; each page's content is checked separately below.
	t.Run("layout", func(t *testing.T) {
		check(t, tmpl.repos, pageData{})
	})

	t.Run("repos", func(t *testing.T) {
		check(t, tmpl.repos.Lookup("content"), reposPage{})
	})
	t.Run("branches", func(t *testing.T) {
		check(t, tmpl.branches.Lookup("content"), branchesPage{})
	})
	t.Run("branch_detail", func(t *testing.T) {
		check(t, tmpl.branchDetail.Lookup("content"), branchDetailPage{
			Ctx: &model.AgentContextResponse{},
		})
	})
	t.Run("branch_checks", func(t *testing.T) {
		check(t, tmpl.branchChecks, &model.AgentContextResponse{
			CheckRuns: []model.CheckRun{},
		})
	})
	t.Run("check_history", func(t *testing.T) {
		check(t, tmpl.checkHistory, []model.CheckRun{})
	})
	t.Run("review_comments", func(t *testing.T) {
		check(t, tmpl.reviewComments, reviewCommentsData{})
	})
	t.Run("file", func(t *testing.T) {
		check(t, tmpl.fileView.Lookup("content"), filePage{})
	})
	t.Run("error", func(t *testing.T) {
		check(t, tmpl.errorPage.Lookup("content"), errorBody{})
	})
	t.Run("commit_log", func(t *testing.T) {
		check(t, tmpl.commitLog.Lookup("content"), logPage{
			Repo: model.Repo{},
		})
	})
	t.Run("log_rows", func(t *testing.T) {
		check(t, tmpl.logRows, logRowsData{})
	})
	t.Run("commit_detail", func(t *testing.T) {
		check(t, tmpl.commitDetail.Lookup("content"), commitDetailPage{
			Commit: &store.CommitDetail{},
		})
	})
	t.Run("issues", func(t *testing.T) {
		check(t, tmpl.issues.Lookup("content"), issuesPage{})
	})
	t.Run("issues_rows", func(t *testing.T) {
		check(t, tmpl.issuesRows, issuesPage{})
	})
	t.Run("issue_detail", func(t *testing.T) {
		check(t, tmpl.issueDetail.Lookup("content"), issueDetailPage{
			Issue: &model.Issue{},
		})
	})
	t.Run("new_issue", func(t *testing.T) {
		check(t, tmpl.newIssue.Lookup("content"), newIssuePage{})
	})
	t.Run("accept_invite", func(t *testing.T) {
		check(t, tmpl.acceptInvite.Lookup("content"), acceptInvitePage{})
	})
	t.Run("org", func(t *testing.T) {
		check(t, tmpl.org.Lookup("content"), orgPage{
			Members: []model.OrgMember{{Role: model.OrgRoleMember}},
			Invites: []model.OrgInvite{{Role: model.OrgRoleMember}},
		})
	})
	t.Run("proposals", func(t *testing.T) {
		check(t, tmpl.proposals.Lookup("content"), proposalsPage{})
	})
	t.Run("proposals_rows", func(t *testing.T) {
		check(t, tmpl.proposalsRows, []*model.Proposal{})
	})
	t.Run("proposal_detail", func(t *testing.T) {
		check(t, tmpl.proposalDetail.Lookup("content"), proposalDetailPage{
			Proposal: &model.Proposal{},
		})
	})
	t.Run("releases", func(t *testing.T) {
		check(t, tmpl.releases.Lookup("content"), releasesPage{})
	})
	t.Run("release_detail", func(t *testing.T) {
		check(t, tmpl.releaseDetail.Lookup("content"), releaseDetailPage{
			Release: &model.Release{},
		})
	})
	t.Run("ci_jobs", func(t *testing.T) {
		check(t, tmpl.ciJobs.Lookup("content"), ciJobsPage{
			Jobs: []model.CIJob{},
		})
	})
	t.Run("ci_job_detail", func(t *testing.T) {
		check(t, tmpl.ciJobDetail.Lookup("content"), ciJobDetailPage{
			Job: &model.CIJob{ID: "abcdefgh-1234"},
		})
	})
	t.Run("create_org", func(t *testing.T) {
		check(t, tmpl.createOrg.Lookup("content"), createOrgPage{})
	})
	t.Run("create_repo", func(t *testing.T) {
		check(t, tmpl.createRepo.Lookup("content"), createRepoPage{})
	})
	t.Run("new_commit", func(t *testing.T) {
		check(t, tmpl.newCommit.Lookup("content"), commitFormPage{})
	})
	t.Run("repo_settings", func(t *testing.T) {
		check(t, tmpl.repoSettings.Lookup("content"), repoSettingsPage{})
	})
	t.Run("user_profile", func(t *testing.T) {
		check(t, tmpl.userProfile.Lookup("content"), userProfilePage{
			Memberships: []model.OrgMember{{Role: model.OrgRoleMember}},
		})
	})
	t.Run("commit_file_diff", func(t *testing.T) {
		check(t, tmpl.commitFileDiff, commitFileDiffData{})
	})
}
