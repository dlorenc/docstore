CREATE TYPE ci_job_status AS ENUM ('queued', 'claimed', 'passed', 'failed');

CREATE TABLE ci_jobs (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    repo               TEXT NOT NULL,
    branch             TEXT NOT NULL,
    sequence           BIGINT NOT NULL,
    status             ci_job_status NOT NULL DEFAULT 'queued',
    claimed_at         TIMESTAMPTZ,
    last_heartbeat_at  TIMESTAMPTZ,
    worker_pod         TEXT,
    worker_pod_ip      TEXT,
    log_url            TEXT,
    error_message      TEXT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_ci_jobs_queued ON ci_jobs (created_at)
    WHERE status = 'queued';
