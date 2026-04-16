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
  checkout -b <branch>                            Create and switch to a new branch
  checkout <branch>                               Switch to an existing branch
  pull                                            Sync files from the server
  merge                                           Merge current branch into main
  diff                                            Show branch diff from server
  log [path] [--limit N]                          Show commit history (default limit 20)
  show <sequence> [path]                          Show commit or file at a sequence
  rebase                                          Rebase current branch onto main
  resolve <path>                                  Resolve a merge/rebase conflict
  verify                                          Verify commit chain integrity
  branches [--status active|merged|abandoned]     List branches
  reviews [--branch <name>]                       List reviews for a branch
  review --status approved|rejected [--body "…"] [--branch <name>]  Submit a review
  checks [--branch <name>]                        List check runs for a branch
  check --name <n> --status passed|failed [--branch <name>]  Report a CI check
  import-git <path> [--mode squash|replay]        Import a git repo into docstore main
  tui                                             Launch the terminal UI

  orgs                                            List organizations
  orgs create <name>                              Create an organization
  orgs get <name>                                 Get details for an organization
  orgs delete <name>                              Delete an organization
  orgs repos <name>                               List repos in an organization

  repos                                           List repositories
  repos create <owner> <name>                     Create a repository
  repos delete <name>                             Delete a repository (e.g. acme/myrepo)

  roles                                           List roles for the current repo
  roles set <identity> <role>                     Set a role (reader/writer/maintainer/admin)
  roles delete <identity>                         Delete a role assignment

  role list                                       List roles for the current repo
  role set <identity> <role>                      Set a role (reader/writer/maintainer/admin)
  role delete <identity>                          Delete a role assignment`

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
			fmt.Fprintln(os.Stderr, "usage: ds checkout [-b] <branch>")
			os.Exit(1)
		}
		if args[1] == "-b" {
			if len(args) < 3 {
				fmt.Fprintln(os.Stderr, "usage: ds checkout -b <branch>")
				os.Exit(1)
			}
			err = app.CheckoutNew(args[2])
		} else {
			err = app.Checkout(args[1])
		}

	case "pull":
		err = app.Pull()

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
		for i := 1; i < len(args); i++ {
			if args[i] == "--status" && i+1 < len(args) {
				status = args[i+1]
				i++
			}
		}
		err = app.Branches(status)

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
		for i := 1; i < len(args); i++ {
			if args[i] == "--branch" && i+1 < len(args) {
				branch = args[i+1]
				i++
			}
		}
		err = app.Checks(branch)

	case "check":
		name := ""
		status := ""
		branch := ""
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
			}
		}
		if name == "" || status == "" {
			fmt.Fprintln(os.Stderr, "usage: ds check --name <check_name> --status passed|failed [--branch <name>]")
			os.Exit(1)
		}
		err = app.Check(branch, name, status)

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
