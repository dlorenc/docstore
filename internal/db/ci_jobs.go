package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"

	"github.com/dlorenc/docstore/internal/model"
)

// ErrTokenInvalid is returned by LookupRequestToken when the token is not
// found, is expired, or the associated job is not in the 'claimed' state.
var ErrTokenInvalid = errors.New("request token invalid or expired")

// ciJobColumns is the ordered list of columns returned by SELECT * on ci_jobs.
const ciJobColumns = `id, repo, branch, sequence, status, claimed_at, last_heartbeat_at,
	worker_pod, worker_pod_ip, log_url, error_message, created_at, trigger_type, trigger_branch, trigger_base_branch, trigger_proposal_id,
	request_token, request_token_exp, permissions`

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
	var triggerBaseBranch sql.NullString
	var triggerProposalID sql.NullString
	// requestToken and requestTokenExp are stored in the DB but not exposed on
	// the public api.CIJob wire type; scan them into discard variables.
	var requestToken sql.NullString
	var requestTokenExp sql.NullTime
	var permissions pq.StringArray
	if err := row.Scan(
		&j.ID, &j.Repo, &j.Branch, &j.Sequence, &j.Status,
		&claimedAt, &lastHeartbeatAt, &workerPod, &workerPodIP,
		&logURL, &errorMessage, &j.CreatedAt,
		&triggerType, &triggerBranch, &triggerBaseBranch, &triggerProposalID,
		&requestToken, &requestTokenExp, &permissions,
	); err != nil {
		return nil, err
	}
	_ = requestToken
	_ = requestTokenExp
	if len(permissions) > 0 {
		j.Permissions = []string(permissions)
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
	if triggerBaseBranch.Valid {
		j.TriggerBaseBranch = triggerBaseBranch.String
	}
	if triggerProposalID.Valid {
		j.TriggerProposalID = &triggerProposalID.String
	}
	return &j, nil
}

// StoreRequestToken sets the hashed request token and its expiry on a ci_job.
func (s *Store) StoreRequestToken(ctx context.Context, jobID string, hashedToken string, exp time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE ci_jobs SET request_token = $2, request_token_exp = $3 WHERE id = $1`,
		jobID, hashedToken, exp,
	)
	if err != nil {
		return fmt.Errorf("store request token: %w", err)
	}
	return nil
}

// LookupRequestToken looks up a ci_job by its hashed request token.
// It returns the job if the token exists, is not expired, and the job status
// is 'claimed'. Otherwise it returns ErrTokenInvalid.
func (s *Store) LookupRequestToken(ctx context.Context, hashedToken string) (*model.CIJob, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+ciJobColumns+` FROM ci_jobs
		WHERE request_token = $1
		  AND request_token_exp > now()
		  AND status = 'claimed'`,
		hashedToken,
	)
	j, err := scanCIJob(row)
	if err == sql.ErrNoRows {
		return nil, ErrTokenInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("lookup request token: %w", err)
	}
	return j, nil
}

// InsertCIJob inserts a new ci_job row with status 'queued' and returns it.
// triggerType identifies how the job was triggered (e.g. "push", "manual", "proposal").
// triggerBranch is the branch that caused the trigger (may be empty for some trigger types).
// triggerBaseBranch is the base branch for proposal-triggered jobs (empty string for others).
// triggerProposalID is the proposal ID for proposal-triggered jobs (empty string for others).
// permissions is the list of permission names granted to the job (e.g. "checks", "contents").
// A nil or empty permissions slice defaults to {"checks"} at the DB level.
func (s *Store) InsertCIJob(ctx context.Context, repo, branch string, sequence int64, triggerType, triggerBranch, triggerBaseBranch, triggerProposalID string, permissions []string) (*model.CIJob, error) {
	var proposalID any
	if triggerProposalID != "" {
		proposalID = triggerProposalID
	}
	var baseBranch any
	if triggerBaseBranch != "" {
		baseBranch = triggerBaseBranch
	}
	if len(permissions) == 0 {
		permissions = []string{"checks"}
	}
	row := s.db.QueryRowContext(ctx,
		`INSERT INTO ci_jobs (repo, branch, sequence, trigger_type, trigger_branch, trigger_base_branch, trigger_proposal_id, permissions)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING `+ciJobColumns,
		repo, branch, sequence, triggerType, triggerBranch, baseBranch, proposalID, pq.Array(permissions),
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
		return nil, ErrCIJobNotFound
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
			  AND NOT EXISTS (SELECT 1 FROM ci_jobs c2 WHERE c2.worker_pod = $1 AND c2.status = 'claimed')
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
		`UPDATE ci_jobs SET status = $2, log_url = $3, error_message = $4, request_token = NULL, request_token_exp = NULL WHERE id = $1`,
		id, status, logURL, errorMessage,
	)
	if err != nil {
		return fmt.Errorf("complete ci job: %w", err)
	}
	return nil
}

// ListCIJobs returns ci_jobs for the given repo, optionally filtered by branch
// and status, ordered by created_at DESC, up to limit rows.
func (s *Store) ListCIJobs(ctx context.Context, repo string, branch, status *string, limit int) ([]model.CIJob, error) {
	query := `SELECT ` + ciJobColumns + ` FROM ci_jobs WHERE repo = $1`
	args := []any{repo}

	if branch != nil {
		args = append(args, *branch)
		query += fmt.Sprintf(` AND branch = $%d`, len(args))
	}
	if status != nil {
		args = append(args, *status)
		query += fmt.Sprintf(` AND status = $%d`, len(args))
	}
	args = append(args, limit)
	query += fmt.Sprintf(` ORDER BY created_at DESC LIMIT $%d`, len(args))

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list ci jobs: %w", err)
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
	return jobs, rows.Err()
}

// CountQueuedCIJobs returns the number of ci_jobs with status 'queued'.
func (s *Store) CountQueuedCIJobs(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ci_jobs WHERE status = 'queued'`,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count queued ci jobs: %w", err)
	}
	return n, nil
}

// ReapStaleCIJobs resets claimed jobs that have not sent a heartbeat in the
// last 2 minutes back to 'queued' so another worker can pick them up.
func (s *Store) ReapStaleCIJobs(ctx context.Context) ([]model.CIJob, error) {
	rows, err := s.db.QueryContext(ctx,
		`UPDATE ci_jobs
		SET status              = 'queued',
		    claimed_at          = NULL,
		    last_heartbeat_at   = NULL,
		    worker_pod          = NULL,
		    worker_pod_ip       = NULL,
		    request_token       = NULL,
		    request_token_exp   = NULL
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
