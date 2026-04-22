package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/dlorenc/docstore/internal/db"
)

// parseRepoPath parses a /repos/... URL path into the full repo name and the
// endpoint string that follows the "/-/" separator.
//
//	/repos/acme/myrepo/-/tree        → ("acme/myrepo", "tree", true)
//	/repos/acme/team/sub/-/commit    → ("acme/team/sub", "commit", true)
//	/repos/acme/myrepo               → ("acme/myrepo", "", true)  // bare repo
//	/something/else                  → ("", "", false)
//
// NOTE: repoAndSubPath in middleware.go parses the same "/-/" URL format for
// RBAC purposes. Both functions must be kept in sync if the URL structure changes.
func parseRepoPath(path string) (repoName, endpoint string, ok bool) {
	const prefix = "/repos/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	rest := path[len(prefix):]
	if rest == "" {
		return "", "", false
	}
	idx := strings.Index(rest, "/-/")
	if idx == -1 {
		// Bare /repos/:repopath — no endpoint
		return rest, "", true
	}
	return rest[:idx], rest[idx+3:], true
}

// handleReposPrefix is the catch-all handler for all /repos/... paths (except
// the bare GET /repos and POST /repos which are registered as exact matches).
// It parses the repo name and endpoint from the URL using the "/-/" separator,
// sets the "name" path value so existing sub-handlers work unchanged, and
// dispatches to the appropriate handler.
func (s *server) handleReposPrefix(w http.ResponseWriter, r *http.Request) {
	repoName, endpoint, ok := parseRepoPath(r.URL.Path)
	if !ok || repoName == "" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	r.SetPathValue("name", repoName)

	if endpoint == "" {
		// Bare /repos/:reponame
		switch r.Method {
		case http.MethodGet:
			s.handleGetRepo(w, r)
		case http.MethodDelete:
			s.handleDeleteRepo(w, r)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}

	switch {
	case endpoint == "tree":
		if r.Method == http.MethodGet {
			s.handleTree(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "archive":
		if r.Method == http.MethodGet {
			s.handleArchive(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "branches":
		if r.Method == http.MethodGet {
			s.handleBranches(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "diff":
		if r.Method == http.MethodGet {
			s.handleDiff(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "commit":
		if r.Method == http.MethodPost {
			s.handleCommit(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "branch":
		if r.Method == http.MethodPost {
			s.handleCreateBranch(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "merge":
		if r.Method == http.MethodPost {
			s.handleMerge(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "rebase":
		if r.Method == http.MethodPost {
			s.handleRebase(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "review":
		if r.Method == http.MethodPost {
			s.handleReview(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "check":
		if r.Method == http.MethodPost {
			s.handleCheck(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "checks/retry":
		if r.Method == http.MethodPost {
			s.handleRetryChecks(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "comment":
		if r.Method == http.MethodPost {
			s.handleCreateReviewComment(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case strings.HasPrefix(endpoint, "comment/"):
		commentID := strings.TrimPrefix(endpoint, "comment/")
		r.SetPathValue("commentID", commentID)
		if r.Method == http.MethodDelete {
			s.handleDeleteReviewComment(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "purge":
		if r.Method == http.MethodPost {
			s.handlePurge(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "roles":
		if r.Method == http.MethodGet {
			s.handleListRoles(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "chain":
		if r.Method == http.MethodGet {
			s.handleChain(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "releases":
		switch r.Method {
		case http.MethodGet:
			s.handleListReleases(w, r)
		case http.MethodPost:
			s.handleCreateRelease(w, r)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case strings.HasPrefix(endpoint, "releases/"):
		releaseName := strings.TrimPrefix(endpoint, "releases/")
		r.SetPathValue("release", releaseName)
		switch r.Method {
		case http.MethodGet:
			s.handleGetRelease(w, r)
		case http.MethodDelete:
			s.handleDeleteRelease(w, r)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case strings.HasPrefix(endpoint, "commit/"):
		rest := strings.TrimPrefix(endpoint, "commit/")
		if strings.HasSuffix(rest, "/issues") {
			r.SetPathValue("sequence", strings.TrimSuffix(rest, "/issues"))
			if r.Method == http.MethodGet {
				s.handleCommitIssues(w, r)
			} else {
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			}
		} else {
			if r.Method == http.MethodGet {
				r.SetPathValue("sequence", rest)
				s.handleGetCommit(w, r)
			} else {
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			}
		}

	case strings.HasPrefix(endpoint, "file/"):
		if r.Method == http.MethodGet {
			r.SetPathValue("path", strings.TrimPrefix(endpoint, "file/"))
			s.handleFile(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case strings.HasPrefix(endpoint, "roles/"):
		identity := strings.TrimPrefix(endpoint, "roles/")
		r.SetPathValue("identity", identity)
		switch r.Method {
		case http.MethodPut:
			s.handleSetRole(w, r)
		case http.MethodDelete:
			s.handleDeleteRole(w, r)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case strings.HasPrefix(endpoint, "branch/"):
		bpath := strings.TrimPrefix(endpoint, "branch/")
		if strings.HasSuffix(bpath, "/auto-merge") {
			branchName := strings.TrimSuffix(bpath, "/auto-merge")
			r.SetPathValue("bname", branchName)
			switch r.Method {
			case http.MethodPost:
				s.handleEnableAutoMerge(w, r)
			case http.MethodDelete:
				s.handleDisableAutoMerge(w, r)
			default:
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			}
		} else {
			switch r.Method {
			case http.MethodDelete:
				r.SetPathValue("bname", bpath)
				s.handleDeleteBranch(w, r)
			case http.MethodPatch:
				r.SetPathValue("bname", bpath)
				s.handleUpdateBranch(w, r)
			case http.MethodGet:
				if strings.HasSuffix(bpath, "/status") {
					branchName := strings.TrimSuffix(bpath, "/status")
					s.handleBranchStatus(w, r, repoName, branchName)
				} else if strings.HasSuffix(bpath, "/agent-context") {
					branchName := strings.TrimSuffix(bpath, "/agent-context")
					s.handleAgentContext(w, r, repoName, branchName)
				} else {
					r.SetPathValue("branch", bpath)
					s.handleBranchGet(w, r)
				}
			default:
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			}
		}

	case endpoint == "events":
		if r.Method == http.MethodGet {
			s.handleSSERepoEvents(w, r)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case endpoint == "proposals":
		switch r.Method {
		case http.MethodPost:
			s.handleCreateProposal(w, r)
		case http.MethodGet:
			s.handleListProposals(w, r)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case strings.HasPrefix(endpoint, "proposals/"):
		rest := strings.TrimPrefix(endpoint, "proposals/")
		// Check for /proposals/:id/close
		if strings.HasSuffix(rest, "/close") {
			proposalID := strings.TrimSuffix(rest, "/close")
			r.SetPathValue("proposalID", proposalID)
			if r.Method == http.MethodPost {
				s.handleCloseProposal(w, r)
			} else {
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			}
		} else if strings.HasSuffix(rest, "/issues") {
			proposalID := strings.TrimSuffix(rest, "/issues")
			r.SetPathValue("proposalID", proposalID)
			if r.Method == http.MethodGet {
				s.handleProposalIssues(w, r)
			} else {
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			}
		} else {
			r.SetPathValue("proposalID", rest)
			switch r.Method {
			case http.MethodGet:
				s.handleGetProposal(w, r)
			case http.MethodPatch:
				s.handleUpdateProposal(w, r)
			default:
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			}
		}

	case endpoint == "issues":
		switch r.Method {
		case http.MethodGet:
			s.handleListIssues(w, r)
		case http.MethodPost:
			s.handleCreateIssue(w, r)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case strings.HasPrefix(endpoint, "issues/"):
		rest := strings.TrimPrefix(endpoint, "issues/")
		parts := strings.SplitN(rest, "/", 3)
		r.SetPathValue("number", parts[0])
		switch len(parts) {
		case 1:
			// issues/{number}
			switch r.Method {
			case http.MethodGet:
				s.handleGetIssue(w, r)
			case http.MethodPatch:
				s.handleUpdateIssue(w, r)
			default:
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			}
		case 2:
			switch parts[1] {
			case "close":
				if r.Method == http.MethodPost {
					s.handleCloseIssue(w, r)
				} else {
					writeError(w, http.StatusMethodNotAllowed, "method not allowed")
				}
			case "reopen":
				if r.Method == http.MethodPost {
					s.handleReopenIssue(w, r)
				} else {
					writeError(w, http.StatusMethodNotAllowed, "method not allowed")
				}
			case "comments":
				switch r.Method {
				case http.MethodGet:
					s.handleListIssueComments(w, r)
				case http.MethodPost:
					s.handleCreateIssueComment(w, r)
				default:
					writeError(w, http.StatusMethodNotAllowed, "method not allowed")
				}
			case "refs":
				switch r.Method {
				case http.MethodGet:
					s.handleListIssueRefs(w, r)
				case http.MethodPost:
					s.handleAddIssueRef(w, r)
				default:
					writeError(w, http.StatusMethodNotAllowed, "method not allowed")
				}
			default:
				writeError(w, http.StatusNotFound, "not found")
			}
		case 3:
			if parts[1] == "comments" {
				r.SetPathValue("commentID", parts[2])
				switch r.Method {
				case http.MethodPatch:
					s.handleUpdateIssueComment(w, r)
				case http.MethodDelete:
					s.handleDeleteIssueComment(w, r)
				default:
					writeError(w, http.StatusMethodNotAllowed, "method not allowed")
				}
			} else {
				writeError(w, http.StatusNotFound, "not found")
			}
		default:
			writeError(w, http.StatusNotFound, "not found")
		}

	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeAPIError(w, statusToCode(status), status, msg)
}

// validateRepo checks that the named repo exists. It writes a 404 and returns
// false when the repo is not found, so callers can do:
//
//	if !s.validateRepo(w, r, repo) { return }
func (s *server) validateRepo(w http.ResponseWriter, r *http.Request, repo string) bool {
	_, err := s.commitStore.GetRepo(r.Context(), repo)
	if err != nil {
		if errors.Is(err, db.ErrRepoNotFound) {
			writeAPIError(w, ErrCodeRepoNotFound, http.StatusNotFound, "repo not found")
		} else {
			slog.Error("internal error", "op", "validate_repo", "repo", repo, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		}
		return false
	}
	return true
}
