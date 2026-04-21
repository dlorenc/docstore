ALTER TABLE check_runs DROP CONSTRAINT check_runs_unique;
CREATE INDEX idx_check_runs_repo_branch_seq_name ON check_runs (repo, branch, sequence, check_name);
ALTER TABLE check_runs DROP COLUMN attempt;
