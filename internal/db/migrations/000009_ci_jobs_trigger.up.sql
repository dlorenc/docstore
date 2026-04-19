ALTER TABLE ci_jobs
    ADD COLUMN IF NOT EXISTS trigger_type   TEXT,
    ADD COLUMN IF NOT EXISTS trigger_branch TEXT;
