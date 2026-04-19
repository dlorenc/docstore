ALTER TABLE ci_jobs
    DROP COLUMN IF EXISTS trigger_type,
    DROP COLUMN IF EXISTS trigger_branch;
