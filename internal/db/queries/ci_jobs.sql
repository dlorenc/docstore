-- name: InsertCIJob :one
INSERT INTO ci_jobs (repo, branch, sequence)
VALUES ($1, $2, $3)
RETURNING *;

-- name: ClaimCIJob :one
-- Atomically claims the oldest queued job. SKIP LOCKED avoids contention between workers.
UPDATE ci_jobs
SET status     = 'claimed',
    claimed_at = now(),
    worker_pod    = $1,
    worker_pod_ip = $2
WHERE id = (
    SELECT id FROM ci_jobs
    WHERE status = 'queued'
    ORDER BY created_at
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING *;

-- name: HeartbeatCIJob :exec
UPDATE ci_jobs
SET last_heartbeat_at = now()
WHERE id = $1;

-- name: CompleteCIJob :exec
UPDATE ci_jobs
SET status        = $2,
    log_url       = $3,
    error_message = $4
WHERE id = $1;

-- name: GetCIJob :one
SELECT * FROM ci_jobs
WHERE id = $1;

-- name: ReapStaleCIJobs :many
-- Reset claimed jobs that have not sent a heartbeat within the last 2 minutes back to queued.
UPDATE ci_jobs
SET status            = 'queued',
    claimed_at        = NULL,
    last_heartbeat_at = NULL,
    worker_pod        = NULL,
    worker_pod_ip     = NULL
WHERE status = 'claimed'
  AND COALESCE(last_heartbeat_at, claimed_at) < now() - interval '2 minutes'
RETURNING *;
