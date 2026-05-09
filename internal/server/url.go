package server

import "strings"

// splitRepoPath parses a /repos/... URL path into the full repo name and the
// endpoint string that follows the "/-/" separator. It is the single source of
// truth for the "/-/" URL format; both parseRepoPath (handlers.go) and
// repoAndSubPath (middleware.go) delegate here.
//
//	/repos/acme/myrepo/-/tree        → ("acme/myrepo", "tree", true)
//	/repos/acme/team/sub/-/commit    → ("acme/team/sub", "commit", true)
//	/repos/acme/myrepo               → ("acme/myrepo", "", true)  // bare repo, no endpoint
//	/something/else                  → ("", "", false)
func splitRepoPath(path string) (repoName, endpoint string, ok bool) {
	const prefix = "/repos/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	rest := path[len(prefix):]
	if rest == "" {
		return "", "", false
	}
	repoName, endpoint, found := strings.Cut(rest, "/-/")
	if !found {
		return rest, "", true
	}
	return repoName, endpoint, true
}
