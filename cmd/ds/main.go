// Command ds is the docstore CLI client.
package main

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/dlorenc/docstore/internal/cli"
)

// defaultRemote is the compiled-in default server URL, injected via -ldflags.
// If empty, ds init requires an explicit remote URL.
var defaultRemote string

const usage = `usage: ds <command> [args]

commands:
  init [<remote-url>] [--author <name>]          Initialize a docstore workspace
  status                                          Show changed files
  commit -m "message"                             Commit all changes
  checkout -b <branch> [--draft]                 Create and switch to a new branch
  checkout <branch>                               Switch to an existing branch
  pull [--skip-verify]                            Sync files from the server
  merge                                           Merge current branch into main
  diff                                            Show branch diff from server
  log [path] [--limit N]                          Show commit history (default limit 20)
  show <sequence> [path]                          Show commit or file at a sequence
  rebase                                          Rebase current branch onto main
  resolve <path>                                  Resolve a merge/rebase conflict
  verify                                          Verify commit chain integrity
  ready                                           Mark current branch as ready (not draft)
  branches [--status active|merged|abandoned] [--draft] [--include-draft]  List branches
  reviews [--branch <name>]                       List reviews for a branch
  review --status approved|rejected [--body "…"] [--branch <name>]  Submit a review
  checks [--branch <name>] [--all]                List check runs for a branch
  check --name <n> --status passed|failed [--branch <name>] [--log-url <url>] [--sequence <n>]  Report a CI check
  comment --path <path> --body <body> [--branch <name>]  Add an inline file comment
  comments [--path <path>] [--branch <name>]      List inline file comments
  import-git <path> [--mode squash|replay]        Import a git repo into docstore main
  tui                                             Launch the terminal UI
  purge --older-than <Nd> [--dry-run]             Purge old merged/abandoned branches

  orgs                                            List organizations
  orgs create <name>                              Create an organization
  orgs get <name>                                 Get details for an organization
  orgs delete <name>                              Delete an organization
  orgs repos <name>                               List repos in an organization

  org members add <org> <identity> --role owner|member  Add a member to an org
  org members remove <org> <identity>                   Remove a member from an org
  org members list <org>                                List org members
  org invites create <org> --email <email> --role owner|member  Create an invite (prints token)
  org invites list <org>                                List pending invites
  org invites accept <org> <token>                      Accept an invite by token
  org invites revoke <org> <invite-id>                  Revoke a pending invite

  repo list                                       List repositories
  repo create <owner/name>                        Create a repository
  repo get <owner/name>                           Get a repository
  repo delete <owner/name>                        Delete a repository

  repos                                           List repositories
  repos create <owner> <name>                     Create a repository
  repos delete <name>                             Delete a repository (e.g. acme/myrepo)

  branch delete <name>                            Delete a branch from the current repo

  roles                                           List roles for the current repo
  roles set <identity> <role>                     Set a role (reader/writer/maintainer/admin)
  roles delete <identity>                         Delete a role assignment

  role list                                       List roles for the current repo
  role set <identity> <role>                      Set a role (reader/writer/maintainer/admin)
  role delete <identity>                          Delete a role assignment

  release create <name> [--sequence N] [--notes 'text']  Create a named release
  release list                                           List releases
  release show <name>                                    Show release metadata and tree
  release delete <name>                                  Delete a release (admin only)

  proposal open --title "..." [--branch <name>] [--base main] [--description "..."]  Open a proposal
  proposal list [--state open|closed|merged]             List proposals
  proposal close <id>                                    Close a proposal`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}

	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}

	app := &cli.App{
		Dir:           dir,
		Out:           os.Stdout,
		HTTP:          http.DefaultClient,
		DefaultRemote: defaultRemote,
	}

	args := os.Args[1:]
	cmd := args[0]

	switch cmd {
	case "init":
		remote := defaultRemote
		flagStart := 1
		if len(args) >= 2 && !strings.HasPrefix(args[1], "--") {
			remote = args[1]
			flagStart = 2
		}
		if remote == "" {
			fmt.Fprintln(os.Stderr, "usage: ds init [<remote-url>] [--author <name>]")
			fmt.Fprintln(os.Stderr, "error: no remote URL provided and no default compiled in")
			os.Exit(1)
		}
		repo := ""
		author := ""
		for i := flagStart; i < len(args); i++ {
			if args[i] == "--author" && i+1 < len(args) {
				author = args[i+1]
				i++
			} else if args[i] == "--repo" && i+1 < len(args) {
				repo = args[i+1]
				i++
			}
		}
		err = app.Init(remote, repo, author)

	case "status":
		err = app.Status()

	case "commit":
		msg := ""
		for i := 1; i < len(args); i++ {
			if args[i] == "-m" && i+1 < len(args) {
				msg = args[i+1]
				i++
			}
		}
		if msg == "" {
			fmt.Fprintln(os.Stderr, "usage: ds commit -m \"message\"")
			os.Exit(1)
		}
		err = app.Commit(msg)

	case "checkout":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: ds checkout [-b] <branch> [--draft]")
			os.Exit(1)
		}
		if args[1] == "-b" {
			if len(args) < 3 {
				fmt.Fprintln(os.Stderr, "usage: ds checkout -b <branch> [--draft]")
				os.Exit(1)
			}
			draft := false
			for i := 3; i < len(args); i++ {
				if args[i] == "--draft" {
					draft = true
				}
			}
			err = app.CheckoutNew(args[2], draft)
		} else {
			err = app.Checkout(args[1])
		}

	case "pull":
		skipVerify := false
		for i := 1; i < len(args); i++ {
			if args[i] == "--skip-verify" {
				skipVerify = true
			}
		}
		err = app.Pull(skipVerify)

	case "merge":
		err = app.Merge()

	case "diff":
		err = app.Diff()

	case "log":
		path := ""
		limit := 20
		for i := 1; i < len(args); i++ {
			if args[i] == "--limit" && i+1 < len(args) {
				n, convErr := strconv.Atoi(args[i+1])
				if convErr != nil || n < 1 {
					fmt.Fprintln(os.Stderr, "usage: ds log [path] [--limit N]")
					os.Exit(1)
				}
				limit = n
				i++
			} else if len(args[i]) > 0 && args[i][0] != '-' {
				path = args[i]
			}
		}
		err = app.Log(path, limit)

	case "show":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: ds show <sequence> [path]")
			os.Exit(1)
		}
		seq, convErr := strconv.ParseInt(args[1], 10, 64)
		if convErr != nil {
			fmt.Fprintln(os.Stderr, "usage: ds show <sequence> [path]")
			os.Exit(1)
		}
		filePath := ""
		if len(args) > 2 {
			filePath = args[2]
		}
		err = app.Show(seq, filePath)

	case "rebase":
		err = app.Rebase()

	case "verify":
		err = app.Verify()

	case "resolve":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: ds resolve <path>")
			os.Exit(1)
		}
		err = app.Resolve(args[1])

	case "branches":
		status := "active"
		onlyDraft := false
		includeDraft := false
		for i := 1; i < len(args); i++ {
			if args[i] == "--status" && i+1 < len(args) {
				status = args[i+1]
				i++
			} else if args[i] == "--draft" {
				onlyDraft = true
			} else if args[i] == "--include-draft" {
				includeDraft = true
			}
		}
		err = app.Branches(status, onlyDraft, includeDraft)

	case "reviews":
		branch := ""
		for i := 1; i < len(args); i++ {
			if args[i] == "--branch" && i+1 < len(args) {
				branch = args[i+1]
				i++
			}
		}
		err = app.Reviews(branch)

	case "review":
		status := ""
		body := ""
		branch := ""
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--status":
				if i+1 < len(args) {
					status = args[i+1]
					i++
				}
			case "--body":
				if i+1 < len(args) {
					body = args[i+1]
					i++
				}
			case "--branch":
				if i+1 < len(args) {
					branch = args[i+1]
					i++
				}
			}
		}
		if status == "" {
			fmt.Fprintln(os.Stderr, "usage: ds review --status approved|rejected [--body \"...\"] [--branch <name>]")
			os.Exit(1)
		}
		err = app.Review(branch, status, body)

	case "checks":
		branch := ""
		showAll := false
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--branch":
				if i+1 < len(args) {
					branch = args[i+1]
					i++
				}
			case "--all":
				showAll = true
			}
		}
		err = app.Checks(branch, showAll)

	case "check":
		name := ""
		status := ""
		branch := ""
		var logURL *string
		var sequence *int64
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--name":
				if i+1 < len(args) {
					name = args[i+1]
					i++
				}
			case "--status":
				if i+1 < len(args) {
					status = args[i+1]
					i++
				}
			case "--branch":
				if i+1 < len(args) {
					branch = args[i+1]
					i++
				}
			case "--log-url":
				if i+1 < len(args) {
					u := args[i+1]
					logURL = &u
					i++
				}
			case "--sequence":
				if i+1 < len(args) {
					n, parseErr := strconv.ParseInt(args[i+1], 10, 64)
					if parseErr != nil {
						fmt.Fprintln(os.Stderr, "ds check: --sequence must be an integer")
						os.Exit(1)
					}
					sequence = &n
					i++
				}
			}
		}
		if name == "" || status == "" {
			fmt.Fprintln(os.Stderr, "usage: ds check --name <check_name> --status passed|failed [--branch <name>] [--log-url <url>] [--sequence <n>]")
			os.Exit(1)
		}
		err = app.Check(branch, name, status, logURL, sequence)

	case "import-git":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: ds import-git <path> [--mode squash|replay]")
			os.Exit(1)
		}
		repoPath := args[1]
		mode := "replay"
		for i := 2; i < len(args); i++ {
			if args[i] == "--mode" && i+1 < len(args) {
				mode = args[i+1]
				i++
			}
		}
		err = app.ImportGit(repoPath, mode)

	case "tui":
		err = app.TUI()

	case "org":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: ds org members|invites ...")
			os.Exit(1)
		}
		switch args[1] {
		case "members":
			if len(args) < 3 {
				fmt.Fprintln(os.Stderr, "usage: ds org members add|remove|list ...")
				os.Exit(1)
			}
			switch args[2] {
			case "add":
				if len(args) < 5 {
					fmt.Fprintln(os.Stderr, "usage: ds org members add <org> <identity> --role owner|member")
					os.Exit(1)
				}
				org := args[3]
				identity := args[4]
				role := ""
				for i := 5; i < len(args); i++ {
					if args[i] == "--role" && i+1 < len(args) {
						role = args[i+1]
						i++
					}
				}
				if role == "" {
					fmt.Fprintln(os.Stderr, "usage: ds org members add <org> <identity> --role owner|member")
					os.Exit(1)
				}
				err = app.OrgMembersAdd(org, identity, role)
			case "remove":
				if len(args) < 5 {
					fmt.Fprintln(os.Stderr, "usage: ds org members remove <org> <identity>")
					os.Exit(1)
				}
				err = app.OrgMembersRemove(args[3], args[4])
			case "list":
				if len(args) < 4 {
					fmt.Fprintln(os.Stderr, "usage: ds org members list <org>")
					os.Exit(1)
				}
				err = app.OrgMembersList(args[3])
			default:
				fmt.Fprintf(os.Stderr, "unknown org members subcommand: %s\n", args[2])
				fmt.Fprintln(os.Stderr, usage)
				os.Exit(1)
			}
		case "invites":
			if len(args) < 3 {
				fmt.Fprintln(os.Stderr, "usage: ds org invites create|list|accept|revoke ...")
				os.Exit(1)
			}
			switch args[2] {
			case "create":
				if len(args) < 4 {
					fmt.Fprintln(os.Stderr, "usage: ds org invites create <org> --email <email> --role owner|member")
					os.Exit(1)
				}
				org := args[3]
				email := ""
				role := ""
				for i := 4; i < len(args); i++ {
					switch args[i] {
					case "--email":
						if i+1 < len(args) {
							email = args[i+1]
							i++
						}
					case "--role":
						if i+1 < len(args) {
							role = args[i+1]
							i++
						}
					}
				}
				if email == "" || role == "" {
					fmt.Fprintln(os.Stderr, "usage: ds org invites create <org> --email <email> --role owner|member")
					os.Exit(1)
				}
				err = app.OrgInvitesCreate(org, email, role)
			case "list":
				if len(args) < 4 {
					fmt.Fprintln(os.Stderr, "usage: ds org invites list <org>")
					os.Exit(1)
				}
				err = app.OrgInvitesList(args[3])
			case "accept":
				if len(args) < 5 {
					fmt.Fprintln(os.Stderr, "usage: ds org invites accept <org> <token>")
					os.Exit(1)
				}
				err = app.OrgInvitesAccept(args[3], args[4])
			case "revoke":
				if len(args) < 5 {
					fmt.Fprintln(os.Stderr, "usage: ds org invites revoke <org> <invite-id>")
					os.Exit(1)
				}
				err = app.OrgInvitesRevoke(args[3], args[4])
			default:
				fmt.Fprintf(os.Stderr, "unknown org invites subcommand: %s\n", args[2])
				fmt.Fprintln(os.Stderr, usage)
				os.Exit(1)
			}
		default:
			fmt.Fprintf(os.Stderr, "unknown org subcommand: %s\n", args[1])
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(1)
		}

	case "orgs":
		if len(args) < 2 {
			err = app.Orgs()
		} else {
			switch args[1] {
			case "create":
				if len(args) < 3 {
					fmt.Fprintln(os.Stderr, "usage: ds orgs create <name>")
					os.Exit(1)
				}
				err = app.OrgsCreate(args[2])
			case "get":
				if len(args) < 3 {
					fmt.Fprintln(os.Stderr, "usage: ds orgs get <name>")
					os.Exit(1)
				}
				err = app.OrgsGet(args[2])
			case "delete":
				if len(args) < 3 {
					fmt.Fprintln(os.Stderr, "usage: ds orgs delete <name>")
					os.Exit(1)
				}
				err = app.OrgsDelete(args[2])
			case "repos":
				if len(args) < 3 {
					fmt.Fprintln(os.Stderr, "usage: ds orgs repos <name>")
					os.Exit(1)
				}
				err = app.OrgsRepos(args[2])
			default:
				fmt.Fprintf(os.Stderr, "unknown orgs subcommand: %s\n", args[1])
				fmt.Fprintln(os.Stderr, usage)
				os.Exit(1)
			}
		}

	case "repos":
		if len(args) < 2 {
			err = app.Repos()
		} else {
			switch args[1] {
			case "create":
				if len(args) < 4 {
					fmt.Fprintln(os.Stderr, "usage: ds repos create <owner> <name>")
					os.Exit(1)
				}
				err = app.ReposCreate(args[2], args[3])
			case "delete":
				if len(args) < 3 {
					fmt.Fprintln(os.Stderr, "usage: ds repos delete <name>")
					os.Exit(1)
				}
				err = app.ReposDelete(args[2])
			default:
				fmt.Fprintf(os.Stderr, "unknown repos subcommand: %s\n", args[1])
				fmt.Fprintln(os.Stderr, usage)
				os.Exit(1)
			}
		}

	case "roles":
		if len(args) < 2 {
			err = app.Roles()
		} else {
			switch args[1] {
			case "set":
				if len(args) < 4 {
					fmt.Fprintln(os.Stderr, "usage: ds roles set <identity> <role>")
					os.Exit(1)
				}
				err = app.RolesSet(args[2], args[3])
			case "delete":
				if len(args) < 3 {
					fmt.Fprintln(os.Stderr, "usage: ds roles delete <identity>")
					os.Exit(1)
				}
				err = app.RolesDelete(args[2])
			default:
				fmt.Fprintf(os.Stderr, "unknown roles subcommand: %s\n", args[1])
				fmt.Fprintln(os.Stderr, usage)
				os.Exit(1)
			}
		}

	case "role":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: ds role list|set|delete ...")
			os.Exit(1)
		}
		switch args[1] {
		case "list":
			err = app.Roles()
		case "set":
			if len(args) < 4 {
				fmt.Fprintln(os.Stderr, "usage: ds role set <identity> <role>")
				os.Exit(1)
			}
			err = app.RolesSet(args[2], args[3])
		case "delete":
			if len(args) < 3 {
				fmt.Fprintln(os.Stderr, "usage: ds role delete <identity>")
				os.Exit(1)
			}
			err = app.RolesDelete(args[2])
		default:
			fmt.Fprintf(os.Stderr, "unknown role subcommand: %s\n", args[1])
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(1)
		}

	case "repo":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: ds repo <list|create|get|delete> [args]")
			os.Exit(1)
		}
		switch args[1] {
		case "list":
			err = app.Repos()
		case "create":
			if len(args) < 3 {
				fmt.Fprintln(os.Stderr, "usage: ds repo create <owner/name>")
				os.Exit(1)
			}
			fullName := args[2]
			slash := strings.Index(fullName, "/")
			if slash <= 0 || slash == len(fullName)-1 {
				fmt.Fprintln(os.Stderr, "error: repo name must be in owner/name format")
				os.Exit(1)
			}
			err = app.ReposCreate(fullName[:slash], fullName[slash+1:])
		case "get":
			if len(args) < 3 {
				fmt.Fprintln(os.Stderr, "usage: ds repo get <owner/name>")
				os.Exit(1)
			}
			err = app.RepoGet(args[2])
		case "delete":
			if len(args) < 3 {
				fmt.Fprintln(os.Stderr, "usage: ds repo delete <owner/name>")
				os.Exit(1)
			}
			err = app.ReposDelete(args[2])
		default:
			fmt.Fprintf(os.Stderr, "unknown repo subcommand: %s\n", args[1])
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(1)
		}

	case "branch":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: ds branch <delete> [args]")
			os.Exit(1)
		}
		switch args[1] {
		case "delete":
			if len(args) < 3 {
				fmt.Fprintln(os.Stderr, "usage: ds branch delete <name>")
				os.Exit(1)
			}
			err = app.BranchDelete(args[2])
		default:
			fmt.Fprintf(os.Stderr, "unknown branch subcommand: %s\n", args[1])
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(1)
		}

	case "comment":
		path := ""
		body := ""
		branch := ""
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--path":
				if i+1 < len(args) {
					path = args[i+1]
					i++
				}
			case "--body":
				if i+1 < len(args) {
					body = args[i+1]
					i++
				}
			case "--branch":
				if i+1 < len(args) {
					branch = args[i+1]
					i++
				}
			}
		}
		if path == "" || body == "" {
			fmt.Fprintln(os.Stderr, "usage: ds comment --path <path> --body <body> [--branch <name>]")
			os.Exit(1)
		}
		err = app.Comment(branch, path, body)

	case "comments":
		path := ""
		branch := ""
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--path":
				if i+1 < len(args) {
					path = args[i+1]
					i++
				}
			case "--branch":
				if i+1 < len(args) {
					branch = args[i+1]
					i++
				}
			}
		}
		err = app.Comments(branch, path)

	case "ready":
		err = app.Ready()

	case "purge":
		olderThan := ""
		dryRun := false
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--older-than":
				if i+1 < len(args) {
					olderThan = args[i+1]
					i++
				}
			case "--dry-run":
				dryRun = true
			}
		}
		if olderThan == "" {
			fmt.Fprintln(os.Stderr, "usage: ds purge --older-than <Nd> [--dry-run]")
			os.Exit(1)
		}
		err = app.Purge(olderThan, dryRun)

	case "release":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: ds release <create|list|show|delete> [args]")
			os.Exit(1)
		}
		switch args[1] {
		case "create":
			if len(args) < 3 {
				fmt.Fprintln(os.Stderr, "usage: ds release create <name> [--sequence N] [--notes 'text']")
				os.Exit(1)
			}
			relName := args[2]
			var sequence int64
			notes := ""
			for i := 3; i < len(args); i++ {
				switch args[i] {
				case "--sequence":
					if i+1 < len(args) {
						n, convErr := strconv.ParseInt(args[i+1], 10, 64)
						if convErr != nil || n < 0 {
							fmt.Fprintln(os.Stderr, "usage: ds release create <name> [--sequence N] [--notes 'text']")
							os.Exit(1)
						}
						sequence = n
						i++
					}
				case "--notes":
					if i+1 < len(args) {
						notes = args[i+1]
						i++
					}
				}
			}
			err = app.ReleaseCreate(relName, sequence, notes)
		case "list":
			err = app.ReleaseList()
		case "show":
			if len(args) < 3 {
				fmt.Fprintln(os.Stderr, "usage: ds release show <name>")
				os.Exit(1)
			}
			err = app.ReleaseShow(args[2])
		case "delete":
			if len(args) < 3 {
				fmt.Fprintln(os.Stderr, "usage: ds release delete <name>")
				os.Exit(1)
			}
			err = app.ReleaseDelete(args[2])
		default:
			fmt.Fprintf(os.Stderr, "unknown release subcommand: %s\n", args[1])
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(1)
		}

	case "proposal":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: ds proposal <open|list|close> [args]")
			os.Exit(1)
		}
		switch args[1] {
		case "open":
			title := ""
			branch := ""
			base := ""
			description := ""
			for i := 2; i < len(args); i++ {
				switch args[i] {
				case "--title":
					if i+1 < len(args) {
						title = args[i+1]
						i++
					}
				case "--branch":
					if i+1 < len(args) {
						branch = args[i+1]
						i++
					}
				case "--base":
					if i+1 < len(args) {
						base = args[i+1]
						i++
					}
				case "--description":
					if i+1 < len(args) {
						description = args[i+1]
						i++
					}
				}
			}
			if title == "" {
				fmt.Fprintln(os.Stderr, "usage: ds proposal open --title \"...\" [--branch <name>] [--base main] [--description \"...\"]")
				os.Exit(1)
			}
			err = app.ProposalOpen(branch, base, title, description)
		case "list":
			state := ""
			for i := 2; i < len(args); i++ {
				if args[i] == "--state" && i+1 < len(args) {
					state = args[i+1]
					i++
				}
			}
			err = app.ProposalList(state)
		case "close":
			if len(args) < 3 {
				fmt.Fprintln(os.Stderr, "usage: ds proposal close <proposal-id>")
				os.Exit(1)
			}
			err = app.ProposalClose(args[2])
		default:
			fmt.Fprintf(os.Stderr, "unknown proposal subcommand: %s\n", args[1])
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(1)
		}

	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
}
