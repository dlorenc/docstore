ALTER TABLE ci_jobs
    ADD COLUMN permissions TEXT[] NOT NULL DEFAULT '{}';
