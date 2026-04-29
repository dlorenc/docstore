ALTER TABLE ci_jobs
  ADD COLUMN request_token     TEXT UNIQUE,
  ADD COLUMN request_token_exp TIMESTAMPTZ;

CREATE TABLE ci_oidc_tokens (
    jti        UUID PRIMARY KEY,
    job_id     UUID NOT NULL REFERENCES ci_jobs(id),
    audience   TEXT NOT NULL,
    issued_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);
