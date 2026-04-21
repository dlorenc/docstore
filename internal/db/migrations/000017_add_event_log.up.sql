CREATE TABLE event_log (
    seq        BIGSERIAL PRIMARY KEY,
    repo       TEXT NOT NULL,
    type       TEXT NOT NULL,
    payload    BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Index for repo-scoped SSE polling (WHERE repo = $1 AND seq > $2).
CREATE INDEX event_log_repo_seq ON event_log (repo, seq);

-- Index for wildcard-repo queries (WHERE seq > $1, no repo filter).
CREATE INDEX event_log_seq ON event_log (seq);
