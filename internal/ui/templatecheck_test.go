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

	// Helper to build a pageData wrapper around a concrete body type.
	// The Body field is `any`, so passing the concrete value lets templatecheck
	// follow types through {{with .Body}} blocks.
	page := func(body any) pageData { return pageData{Body: body} }

	t.Run("repos", func(t *testing.T) {
		check(t, tmpl.repos, page(reposPage{}))
	})
	t.Run("branches", func(t *testing.T) {
		check(t, tmpl.branches, page(branchesPage{}))
	})
	t.Run("branch_detail", func(t *testing.T) {
		check(t, tmpl.branchDetail, page(branchDetailPage{
			Ctx: &model.AgentContextResponse{},
		}))
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
		check(t, tmpl.fileView, page(filePage{}))
	})
	t.Run("error", func(t *testing.T) {
		check(t, tmpl.errorPage, pageData{})
	})
	t.Run("commit_log", func(t *testing.T) {
		check(t, tmpl.commitLog, page(logPage{
			Repo: model.Repo{},
		}))
	})
	t.Run("log_rows", func(t *testing.T) {
		check(t, tmpl.logRows, logRowsData{})
	})
	t.Run("commit_detail", func(t *testing.T) {
		check(t, tmpl.commitDetail, page(commitDetailPage{
			Commit: &store.CommitDetail{},
		}))
	})
	t.Run("issues", func(t *testing.T) {
		check(t, tmpl.issues, page(issuesPage{}))
	})
	t.Run("issues_rows", func(t *testing.T) {
		check(t, tmpl.issuesRows, issuesPage{})
	})
	t.Run("issue_detail", func(t *testing.T) {
		check(t, tmpl.issueDetail, page(issueDetailPage{
			Issue: &model.Issue{},
		}))
	})
	t.Run("new_issue", func(t *testing.T) {
		check(t, tmpl.newIssue, page(newIssuePage{}))
	})
	t.Run("accept_invite", func(t *testing.T) {
		check(t, tmpl.acceptInvite, page(acceptInvitePage{}))
	})
	t.Run("org", func(t *testing.T) {
		// org.html passes model.OrgRole to statusClass — fixed by accepting any.
		check(t, tmpl.org, page(orgPage{
			Members: []model.OrgMember{{Role: model.OrgRoleMember}},
			Invites: []model.OrgInvite{{Role: model.OrgRoleMember}},
		}))
	})
	t.Run("proposals", func(t *testing.T) {
		check(t, tmpl.proposals, page(proposalsPage{}))
	})
	t.Run("proposals_rows", func(t *testing.T) {
		check(t, tmpl.proposalsRows, []*model.Proposal{})
	})
	t.Run("proposal_detail", func(t *testing.T) {
		check(t, tmpl.proposalDetail, page(proposalDetailPage{
			Proposal: &model.Proposal{},
		}))
	})
	t.Run("releases", func(t *testing.T) {
		check(t, tmpl.releases, page(releasesPage{}))
	})
	t.Run("release_detail", func(t *testing.T) {
		check(t, tmpl.releaseDetail, page(releaseDetailPage{
			Release: &model.Release{},
		}))
	})
	t.Run("ci_jobs", func(t *testing.T) {
		check(t, tmpl.ciJobs, page(ciJobsPage{
			Jobs: []model.CIJob{},
		}))
	})
	t.Run("ci_job_detail", func(t *testing.T) {
		// ci_job_detail uses {{printf "%.8s" .Job.ID}} — fixed from {{slice .Job.ID 0 8}}
		// which templatecheck couldn't resolve through the CIJob type alias pointer.
		check(t, tmpl.ciJobDetail, page(ciJobDetailPage{
			Job: &model.CIJob{ID: "abcdefgh-1234"},
		}))
	})
	t.Run("create_org", func(t *testing.T) {
		check(t, tmpl.createOrg, page(createOrgPage{}))
	})
	t.Run("create_repo", func(t *testing.T) {
		check(t, tmpl.createRepo, page(createRepoPage{}))
	})
	t.Run("new_commit", func(t *testing.T) {
		check(t, tmpl.newCommit, page(commitFormPage{}))
	})
	t.Run("repo_settings", func(t *testing.T) {
		check(t, tmpl.repoSettings, page(repoSettingsPage{}))
	})
	t.Run("user_profile", func(t *testing.T) {
		// user.html passes model.OrgRole to statusClass — fixed by accepting any.
		check(t, tmpl.userProfile, page(userProfilePage{
			Memberships: []model.OrgMember{{Role: model.OrgRoleMember}},
		}))
	})
}
