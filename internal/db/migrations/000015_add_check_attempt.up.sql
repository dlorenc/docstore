ALTER TABLE check_runs ADD COLUMN attempt SMALLINT NOT NULL DEFAULT 1;
DROP INDEX idx_check_runs_repo_branch_seq_name;
-- Remove duplicate rows that would violate the upcoming unique constraint.
-- Before this migration there was no unique index, so multiple status-update
-- rows could exist for the same (repo, branch, sequence, check_name). Keep
-- only the most-recently created row per tuple; all get attempt=1 above.
DELETE FROM check_runs
WHERE id NOT IN (
    SELECT DISTINCT ON (repo, branch, sequence, check_name) id
    FROM check_runs
    ORDER BY repo, branch, sequence, check_name, created_at DESC
);
ALTER TABLE check_runs ADD CONSTRAINT check_runs_unique UNIQUE (repo, branch, sequence, check_name, attempt);
