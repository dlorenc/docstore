package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/dlorenc/docstore/internal/model"
)

const ciJobColumns = `id, repo, branch, sequence, status, claimed_at, last_heartbeat_at,
                      worker_pod, worker_pod_ip, log_url, error_message, created_at`

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
	if err := row.Scan(
		&j.ID, &j.Repo, &j.Branch, &j.Sequence, &j.Status,
		&claimedAt, &lastHeartbeatAt, &workerPod, &workerPodIP,
		&logURL, &errorMessage, &j.CreatedAt,
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
	return &j, nil
}

// InsertCIJob creates a new ci_jobs row in the 'queued' state.
func (s *Store) InsertCIJob(ctx context.Context, repo, branch string, sequence int64) (*model.CIJob, error) {
	row := s.db.QueryRowContext(ctx,
		`INSERT INTO ci_jobs (repo, branch, sequence)
		 VALUES ($1, $2, $3)
		 RETURNING `+ciJobColumns,
		repo, branch, sequence,
	)
	j, err := scanCIJob(row)
	if err != nil {
		return nil, fmt.Errorf("insert ci job: %w", err)
	}
	return j, nil
}

// GetCIJob retrieves a ci_jobs row by UUID. Returns ErrCIJobNotFound if not present.
func (s *Store) GetCIJob(ctx context.Context, id string) (*model.CIJob, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+ciJobColumns+` FROM ci_jobs WHERE id = $1`,
		id,
	)
	j, err := scanCIJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrCIJobNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get ci job: %w", err)
	}
	return j, nil
}

// ReapStaleCIJobs resets claimed jobs that missed heartbeats back to 'queued'.
// Returns the reclaimed jobs.
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
			return nil, fmt.Errorf("scan ci job: %w", err)
		}
		jobs = append(jobs, *j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ci jobs: %w", err)
	}
	return jobs, nil
}
