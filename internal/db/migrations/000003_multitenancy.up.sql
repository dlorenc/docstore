-- Migration 003: Multitenancy — add repos table and scope all data tables.
-- Strategy: add repo column with DEFAULT 'default', seed 'default' repo,
-- change branches PK to (repo, name), update indexes.

-- 1. Create repos table
CREATE TABLE IF NOT EXISTS repos (
    name       TEXT PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by TEXT NOT NULL DEFAULT 'system'
);

-- 2. Seed the default repo
INSERT INTO repos (name, created_by)
VALUES ('default', 'system')
ON CONFLICT (name) DO NOTHING;

-- 3. Add repo column to branches
DO $$ BEGIN
    ALTER TABLE branches ADD COLUMN repo TEXT NOT NULL DEFAULT 'default';
EXCEPTION WHEN duplicate_column THEN null; END $$;

DO $$ BEGIN
    ALTER TABLE branches ADD CONSTRAINT branches_repo_fk
        FOREIGN KEY (repo) REFERENCES repos(name);
EXCEPTION WHEN duplicate_object THEN null; END $$;

-- Drop FK constraints from dependent tables before changing branches PK
DO $$ BEGIN
    ALTER TABLE file_commits DROP CONSTRAINT file_commits_branch_fkey;
EXCEPTION WHEN undefined_object THEN null; END $$;

DO $$ BEGIN
    ALTER TABLE reviews DROP CONSTRAINT reviews_branch_fkey;
EXCEPTION WHEN undefined_object THEN null; END $$;

DO $$ BEGIN
    ALTER TABLE check_runs DROP CONSTRAINT check_runs_branch_fkey;
EXCEPTION WHEN undefined_object THEN null; END $$;

-- Drop old PK on branches (name alone) and add composite PK
DO $$ BEGIN
    ALTER TABLE branches DROP CONSTRAINT branches_pkey;
EXCEPTION WHEN undefined_object THEN null; END $$;

DO $$ BEGIN
    ALTER TABLE branches ADD PRIMARY KEY (repo, name);
EXCEPTION WHEN duplicate_object THEN null; END $$;

-- 4. Add repo column to documents
DO $$ BEGIN
    ALTER TABLE documents ADD COLUMN repo TEXT NOT NULL DEFAULT 'default';
EXCEPTION WHEN duplicate_column THEN null; END $$;

DO $$ BEGIN
    ALTER TABLE documents ADD CONSTRAINT documents_repo_fk
        FOREIGN KEY (repo) REFERENCES repos(name);
EXCEPTION WHEN duplicate_object THEN null; END $$;

-- 5. Add repo column to file_commits
DO $$ BEGIN
    ALTER TABLE file_commits ADD COLUMN repo TEXT NOT NULL DEFAULT 'default';
EXCEPTION WHEN duplicate_column THEN null; END $$;

DO $$ BEGIN
    ALTER TABLE file_commits ADD CONSTRAINT file_commits_repo_fk
        FOREIGN KEY (repo) REFERENCES repos(name);
EXCEPTION WHEN duplicate_object THEN null; END $$;

-- 6. Add repo column to commits
DO $$ BEGIN
    ALTER TABLE commits ADD COLUMN repo TEXT NOT NULL DEFAULT 'default';
EXCEPTION WHEN duplicate_column THEN null; END $$;

DO $$ BEGIN
    ALTER TABLE commits ADD CONSTRAINT commits_repo_fk
        FOREIGN KEY (repo) REFERENCES repos(name);
EXCEPTION WHEN duplicate_object THEN null; END $$;

-- 7. Add repo column to roles, change PK to (repo, identity)
DO $$ BEGIN
    ALTER TABLE roles ADD COLUMN repo TEXT NOT NULL DEFAULT 'default';
EXCEPTION WHEN duplicate_column THEN null; END $$;

DO $$ BEGIN
    ALTER TABLE roles ADD CONSTRAINT roles_repo_fk
        FOREIGN KEY (repo) REFERENCES repos(name);
EXCEPTION WHEN duplicate_object THEN null; END $$;

DO $$ BEGIN
    ALTER TABLE roles DROP CONSTRAINT roles_pkey;
EXCEPTION WHEN undefined_object THEN null; END $$;

DO $$ BEGIN
    ALTER TABLE roles ADD PRIMARY KEY (repo, identity);
EXCEPTION WHEN duplicate_object THEN null; END $$;

-- 8. Add repo column to reviews
DO $$ BEGIN
    ALTER TABLE reviews ADD COLUMN repo TEXT NOT NULL DEFAULT 'default';
EXCEPTION WHEN duplicate_column THEN null; END $$;

DO $$ BEGIN
    ALTER TABLE reviews ADD CONSTRAINT reviews_repo_fk
        FOREIGN KEY (repo) REFERENCES repos(name);
EXCEPTION WHEN duplicate_object THEN null; END $$;

-- 9. Add repo column to check_runs
DO $$ BEGIN
    ALTER TABLE check_runs ADD COLUMN repo TEXT NOT NULL DEFAULT 'default';
EXCEPTION WHEN duplicate_column THEN null; END $$;

DO $$ BEGIN
    ALTER TABLE check_runs ADD CONSTRAINT check_runs_repo_fk
        FOREIGN KEY (repo) REFERENCES repos(name);
EXCEPTION WHEN duplicate_object THEN null; END $$;

-- 10. Update indexes to include repo as leading column
DROP INDEX IF EXISTS idx_file_commits_branch_path_seq;
DROP INDEX IF EXISTS idx_file_commits_sequence;
DROP INDEX IF EXISTS idx_documents_content_hash;
DROP INDEX IF EXISTS idx_reviews_branch_sequence;
DROP INDEX IF EXISTS idx_check_runs_branch_seq_name;

CREATE INDEX IF NOT EXISTS idx_file_commits_repo_branch_path_seq
    ON file_commits (repo, branch, path, sequence DESC);
CREATE INDEX IF NOT EXISTS idx_file_commits_repo_sequence
    ON file_commits (repo, sequence);
CREATE INDEX IF NOT EXISTS idx_documents_repo_content_hash
    ON documents (repo, content_hash);
CREATE INDEX IF NOT EXISTS idx_reviews_repo_branch_sequence
    ON reviews (repo, branch, sequence);
CREATE INDEX IF NOT EXISTS idx_check_runs_repo_branch_seq_name
    ON check_runs (repo, branch, sequence, check_name);

-- 11. Re-seed main branch in default repo (idempotent)
INSERT INTO branches (repo, name, head_sequence, base_sequence, status)
VALUES ('default', 'main', 0, 0, 'active')
ON CONFLICT (repo, name) DO NOTHING;
