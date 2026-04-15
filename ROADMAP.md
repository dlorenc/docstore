# DocStore VCS — Roadmap

## P0 — Minimum Viable Server

These must land first. Each is an independent PR unless noted.

- [x] **Project scaffolding**: Go module setup, directory structure (`cmd/`, `internal/`, `pkg/`), Makefile, basic CI (lint + test)
- [x] **Database schema & migrations**: SQL migration files with orgs, repos, branches, commits, documents, file_commits, roles, reviews, check_runs tables with indexes
- [x] **Data model types**: Go types for all tables, plus request/response structs for the API
- [x] **Document store & content dedup**: `POST /repos/{repo}/-/commit` path — hash content, dedup against documents table, insert file_commits with atomic sequence allocation
- [x] **Tree materialization & file reads**: `GET /repos/{repo}/-/tree`, `GET /repos/{repo}/-/file/{path}`, `GET /repos/{repo}/-/file/{path}/history`, `GET /repos/{repo}/-/commit/{sequence}`
- [x] **Branch management**: `POST /repos/{repo}/-/branch`, `DELETE /repos/{repo}/-/branch/{name}`, `GET /repos/{repo}/-/branches`
- [x] **Merge engine**: `POST /repos/{repo}/-/merge` — conflict detection, fast-forward merge into main, branch status update
- [x] **Rebase**: `POST /repos/{repo}/-/rebase` — replay branch commits onto current main head
- [x] **Diff endpoint**: `GET /repos/{repo}/-/diff?branch=X` — files changed on branch vs base_sequence
- [x] **Org support**: `orgs` table, org CRUD endpoints, `repos.owner` FK, `/-/` URL separator for slash-in-name repos

## P1 — Access Control & Reviews

Depends on P0 server being functional.

- [x] **IAP authentication middleware**: Extract and validate identity from `X-Goog-IAP-JWT-Assertion` header
- [x] **Roles table & RBAC middleware**: Role lookup, coarse-grained permission checks (reader/writer/maintainer/admin)
- [x] **Review system**: `POST /repos/{repo}/-/review`, review storage, sequence-scoped approval tracking
- [x] **Check runs**: `POST /repos/{repo}/-/check`, check status storage, reporter authorization
- [ ] **OPA policy engine integration**: Embed OPA, load policies from `.docstore/policy/*.rego`, evaluate on all write endpoints
- [ ] **OWNERS files**: Parse OWNERS from materialized main tree, build owners map for policy input
- [ ] **Branch status endpoint**: `GET /repos/{repo}/-/branch/{name}/status` — evaluate merge policies without merging (currently returns 501)

## P2 — CLI Client

Can be developed in parallel with P1.

- [x] **CLI scaffolding**: `ds` command structure, `.docstore` config file management
- [x] **Core commands**: `init`, `checkout`, `status`, `commit`, `log`, `diff`
- [x] **Merge & rebase commands**: `merge`, `rebase`
- [x] **Conflict resolution**: Write conflict files, `ds resolve` command
- [x] **Local state management**: `.docstore/state.json`, local hash tracking for offline status
