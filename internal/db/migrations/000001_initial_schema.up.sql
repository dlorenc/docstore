-- DocStore VCS initial schema
-- Six tables per DESIGN.md: documents, file_commits, branches, roles, reviews, check_runs

-- Custom enum types (idempotent via DO blocks)
DO $$ BEGIN CREATE TYPE branch_status AS ENUM ('active', 'merged', 'abandoned'); EXCEPTION WHEN duplicate_object THEN null; END $$;
DO $$ BEGIN CREATE TYPE role_type AS ENUM ('reader', 'writer', 'maintainer', 'admin'); EXCEPTION WHEN duplicate_object THEN null; END $$;
DO $$ BEGIN CREATE TYPE review_status AS ENUM ('approved', 'rejected', 'dismissed'); EXCEPTION WHEN duplicate_object THEN null; END $$;
DO $$ BEGIN CREATE TYPE check_status AS ENUM ('pending', 'passed', 'failed'); EXCEPTION WHEN duplicate_object THEN null; END $$;

-- documents: immutable file versions, content-addressed by content_hash
CREATE TABLE IF NOT EXISTS documents (
    version_id UUID PRIMARY KEY,
    path TEXT NOT NULL,
    content BYTEA NOT NULL,
    content_hash TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_documents_content_hash ON documents (content_hash);

-- branches: named pointers to sequences
CREATE TABLE IF NOT EXISTS branches (
    name TEXT PRIMARY KEY,
    head_sequence BIGINT NOT NULL DEFAULT 0,
    base_sequence BIGINT NOT NULL DEFAULT 0,
    status branch_status NOT NULL DEFAULT 'active'
);

-- file_commits: core event log, one row per file change
-- Rows sharing the same sequence number form one atomic commit
CREATE TABLE IF NOT EXISTS file_commits (
    commit_id UUID PRIMARY KEY,
    sequence BIGINT NOT NULL,
    path TEXT NOT NULL,
    version_id UUID REFERENCES documents (version_id),
    branch TEXT NOT NULL REFERENCES branches (name),
    message TEXT NOT NULL,
    author TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Primary query index: tree materialization and file history
-- Supports DISTINCT ON (path) ... ORDER BY path, sequence DESC
CREATE INDEX IF NOT EXISTS idx_file_commits_branch_path_seq ON file_commits (branch, path, sequence DESC);

-- Atomic commit lookup: all rows in a single commit
CREATE INDEX IF NOT EXISTS idx_file_commits_sequence ON file_commits (sequence);

-- roles: identity to permission mapping
CREATE TABLE IF NOT EXISTS roles (
    identity TEXT PRIMARY KEY,
    role role_type NOT NULL
);

-- reviews: approval/rejection records scoped to branch + sequence
CREATE TABLE IF NOT EXISTS reviews (
    id UUID PRIMARY KEY,
    branch TEXT NOT NULL REFERENCES branches (name),
    reviewer TEXT NOT NULL,
    sequence BIGINT NOT NULL,
    status review_status NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_reviews_branch_sequence ON reviews (branch, sequence);

-- check_runs: external CI status reports scoped to branch + sequence
CREATE TABLE IF NOT EXISTS check_runs (
    id UUID PRIMARY KEY,
    branch TEXT NOT NULL REFERENCES branches (name),
    sequence BIGINT NOT NULL,
    check_name TEXT NOT NULL,
    status check_status NOT NULL,
    reporter TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_check_runs_branch_seq_name ON check_runs (branch, sequence, check_name);

-- Seed the main branch
INSERT INTO branches (name, head_sequence, base_sequence, status)
SELECT 'main', 0, 0, 'active'
WHERE NOT EXISTS (SELECT 1 FROM branches WHERE name = 'main');
