# DocStore VCS — Roadmap

## P0 — Minimum Viable Server

These must land first. Each is an independent PR unless noted.

- [ ] **Project scaffolding**: Go module setup, directory structure (`cmd/`, `internal/`, `pkg/`), Makefile, basic CI (lint + test)
- [ ] **Database schema & migrations**: SQL migration files for all six tables (documents, file_commits, branches, roles, reviews, check_runs) with indexes per DESIGN.md
- [ ] **Data model types**: Go types for all tables, plus request/response structs for the API
- [ ] **Document store & content dedup**: `POST /commit` path — hash content, dedup against documents table, insert file_commits with atomic sequence allocation
- [ ] **Tree materialization & file reads**: `GET /tree`, `GET /file/:path`, `GET /file/:path/history`, `GET /commit/:sequence`
- [ ] **Branch management**: `POST /branch`, `DELETE /branch/:name`, `GET /branches`
- [ ] **Merge engine**: `POST /merge` — conflict detection, fast-forward merge into main, branch status update
- [ ] **Rebase**: `POST /rebase` — replay branch commits onto current main head
- [ ] **Diff endpoint**: `GET /diff?branch=X` — files changed on branch vs base_sequence

## P1 — Access Control & Reviews

Depends on P0 server being functional.

- [ ] **IAP authentication middleware**: Extract and validate identity from `X-Goog-IAP-JWT-Assertion` header
- [ ] **Roles table & RBAC middleware**: Role lookup, coarse-grained permission checks (reader/writer/maintainer/admin)
- [ ] **Review system**: `POST /review`, review storage, sequence-scoped approval tracking
- [ ] **Check runs**: `POST /check`, check status storage, reporter authorization
- [ ] **OPA policy engine integration**: Embed OPA, load policies from `.docstore/policy/*.rego`, evaluate on all write endpoints
- [ ] **OWNERS files**: Parse OWNERS from materialized main tree, build owners map for policy input
- [ ] **Branch status endpoint**: `GET /branch/:name/status` — evaluate merge policies without merging

## P2 — CLI Client

Can be developed in parallel with P1.

- [ ] **CLI scaffolding**: `ds` command structure, `.docstore` config file management
- [ ] **Core commands**: `init`, `checkout`, `status`, `commit`, `log`, `diff`
- [ ] **Merge & rebase commands**: `merge`, `rebase`
- [ ] **Conflict resolution**: Write conflict files, `ds resolve` command
- [ ] **Local state management**: `.docstore/state.json`, local hash tracking for offline status
