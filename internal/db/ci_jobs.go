package db

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/dlorenc/docstore/internal/model"
)

// ciJobColumns is the ordered list of columns returned by SELECT * on ci_jobs.
const ciJobColumns = `id, repo, branch, sequence, status, claimed_at, last_heartbeat_at,
	worker_pod, worker_pod_ip, log_url, error_message, created_at, trigger_type, trigger_branch, trigger_proposal_id`

// scanCIJob scans a single ci_jobs row into a model.CIJob.
func scanCIJob(row interface {
	Scan(...any) error
}) (*model.CIJob, error) {
	var j model.CIJob
	var claimedAt sql.NullTime
	var lastHeartbeatAt sql.NullTime
	var workerPod sql.NullString
	var workerPodIP sql.NullString
	var logURL sql.NullString
	var errorMessage sql.NullString
	var triggerType sql.NullString
	var triggerBranch sql.NullString
	var triggerProposalID sql.NullString
	if err := row.Scan(
		&j.ID, &j.Repo, &j.Branch, &j.Sequence, &j.Status,
		&claimedAt, &lastHeartbeatAt, &workerPod, &workerPodIP,
		&logURL, &errorMessage, &j.CreatedAt,
		&triggerType, &triggerBranch, &triggerProposalID,
	); err != nil {
		return nil, err
	}
	if claimedAt.Valid {
		j.ClaimedAt = &claimedAt.Time
	}
	if lastHeartbeatAt.Valid {
		j.LastHeartbeatAt = &lastHeartbeatAt.Time
	}
	if workerPod.Valid {
		j.WorkerPod = &workerPod.String
	}
	if workerPodIP.Valid {
		j.WorkerPodIP = &workerPodIP.String
	}
	if logURL.Valid {
		j.LogURL = &logURL.String
	}
	if errorMessage.Valid {
		j.ErrorMessage = &errorMessage.String
	}
	if triggerType.Valid {
		j.TriggerType = triggerType.String
	}
	if triggerBranch.Valid {
		j.TriggerBranch = triggerBranch.String
	}
	if triggerProposalID.Valid {
		j.TriggerProposalID = &triggerProposalID.String
	}
	return &j, nil
}

// InsertCIJob inserts a new ci_job row with status 'queued' and returns it.
// triggerType identifies how the job was triggered (e.g. "push", "manual", "proposal").
// triggerBranch is the branch that caused the trigger (may be empty for some trigger types).
// triggerProposalID is the proposal ID for proposal-triggered jobs (empty string for others).
func (s *Store) InsertCIJob(ctx context.Context, repo, branch string, sequence int64, triggerType, triggerBranch, triggerProposalID string) (*model.CIJob, error) {
	var proposalID interface{}
	if triggerProposalID != "" {
		proposalID = triggerProposalID
	}
	row := s.db.QueryRowContext(ctx,
		`INSERT INTO ci_jobs (repo, branch, sequence, trigger_type, trigger_branch, trigger_proposal_id)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING `+ciJobColumns,
		repo, branch, sequence, triggerType, triggerBranch, proposalID,
	)
	j, err := scanCIJob(row)
	if err != nil {
		return nil, fmt.Errorf("insert ci job: %w", err)
	}
	return j, nil
}

// GetCIJob fetches a single ci_job by ID.
func (s *Store) GetCIJob(ctx context.Context, id string) (*model.CIJob, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+ciJobColumns+` FROM ci_jobs WHERE id = $1`,
		id,
	)
	j, err := scanCIJob(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get ci job: %w", err)
	}
	return j, nil
}

// ClaimCIJob atomically claims the oldest queued job for this worker pod.
// Returns nil, nil if no queued job is currently available.
func (s *Store) ClaimCIJob(ctx context.Context, podName, podIP string) (*model.CIJob, error) {
	row := s.db.QueryRowContext(ctx,
		`UPDATE ci_jobs
		SET status = 'claimed', claimed_at = now(), worker_pod = $1, worker_pod_ip = $2
		WHERE id = (
			SELECT id FROM ci_jobs
			WHERE status = 'queued'
			ORDER BY created_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING `+ciJobColumns,
		podName, podIP,
	)
	j, err := scanCIJob(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim ci job: %w", err)
	}
	return j, nil
}

// HeartbeatCIJob updates last_heartbeat_at to now() for the given job.
func (s *Store) HeartbeatCIJob(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE ci_jobs SET last_heartbeat_at = now() WHERE id = $1`,
		id,
	)
	if err != nil {
		return fmt.Errorf("heartbeat ci job: %w", err)
	}
	return nil
}

// CompleteCIJob sets a terminal status, log URL, and error message on the job.
func (s *Store) CompleteCIJob(ctx context.Context, id, status string, logURL, errorMessage *string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE ci_jobs SET status = $2, log_url = $3, error_message = $4 WHERE id = $1`,
		id, status, logURL, errorMessage,
	)
	if err != nil {
		return fmt.Errorf("complete ci job: %w", err)
	}
	return nil
}

// ReapStaleCIJobs resets claimed jobs that have not sent a heartbeat in the
// last 2 minutes back to 'queued' so another worker can pick them up.
func (s *Store) ReapStaleCIJobs(ctx context.Context) ([]model.CIJob, error) {
	rows, err := s.db.QueryContext(ctx,
		`UPDATE ci_jobs
		SET status            = 'queued',
		    claimed_at        = NULL,
		    last_heartbeat_at = NULL,
		    worker_pod        = NULL,
		    worker_pod_ip     = NULL
		WHERE status = 'claimed'
		  AND COALESCE(last_heartbeat_at, claimed_at) < now() - interval '2 minutes'
		RETURNING `+ciJobColumns,
	)
	if err != nil {
		return nil, fmt.Errorf("reap stale ci jobs: %w", err)
	}
	defer rows.Close()

	var jobs []model.CIJob
	for rows.Next() {
		j, err := scanCIJob(rows)
		if err != nil {
			return nil, fmt.Errorf("scan reaped ci job: %w", err)
		}
		jobs = append(jobs, *j)
	}
	return jobs, rows.Err()
}
