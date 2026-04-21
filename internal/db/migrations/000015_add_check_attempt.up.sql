ALTER TABLE check_runs ADD COLUMN attempt SMALLINT NOT NULL DEFAULT 1;
DROP INDEX idx_check_runs_repo_branch_seq_name;
ALTER TABLE check_runs ADD CONSTRAINT check_runs_unique UNIQUE (repo, branch, sequence, check_name, attempt);
