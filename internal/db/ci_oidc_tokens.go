package db

import (
	"context"
	"fmt"
	"time"
)

// RecordOIDCToken inserts an audit record for an issued OIDC JWT.
// jti is the JWT ID (UUID), jobID is the ci_jobs.id, audience is the requested
// audience, and exp is the token expiry time.
func (s *Store) RecordOIDCToken(ctx context.Context, jti, jobID, audience string, exp time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO ci_oidc_tokens (jti, job_id, audience, expires_at)
		 VALUES ($1, $2, $3, $4)`,
		jti, jobID, audience, exp,
	)
	if err != nil {
		return fmt.Errorf("record oidc token: %w", err)
	}
	return nil
}
