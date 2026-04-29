DROP TABLE IF EXISTS ci_oidc_tokens;
ALTER TABLE ci_jobs
  DROP COLUMN IF EXISTS request_token,
  DROP COLUMN IF EXISTS request_token_exp;
