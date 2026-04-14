-- Migration 004: Add body to reviews; add composite FK constraints on reviews and check_runs

-- 1. Add body column to reviews (idempotent)
DO $$ BEGIN
    ALTER TABLE reviews ADD COLUMN body TEXT;
EXCEPTION WHEN duplicate_column THEN null; END $$;

-- 2. Add composite FK from reviews to branches(repo, name)
DO $$ BEGIN
    ALTER TABLE reviews ADD CONSTRAINT reviews_repo_branch_fk
        FOREIGN KEY (repo, branch) REFERENCES branches(repo, name);
EXCEPTION WHEN duplicate_object THEN null; END $$;

-- 3. Add composite FK from check_runs to branches(repo, name)
DO $$ BEGIN
    ALTER TABLE check_runs ADD CONSTRAINT check_runs_repo_branch_fk
        FOREIGN KEY (repo, branch) REFERENCES branches(repo, name);
EXCEPTION WHEN duplicate_object THEN null; END $$;
