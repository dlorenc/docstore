-- DocStore VCS — single clean initial schema with org support.
-- Repos are owned by an org. Repo names may contain slashes (e.g. "acme/team/subrepo").
-- The full repo identifier is stored as the primary key.

CREATE TYPE branch_status AS ENUM ('active', 'merged', 'abandoned');
CREATE TYPE role_type AS ENUM ('reader', 'writer', 'maintainer', 'admin');
CREATE TYPE review_status AS ENUM ('approved', 'rejected', 'dismissed');
CREATE TYPE check_status AS ENUM ('pending', 'passed', 'failed');

-- orgs: top-level namespace owner
CREATE TABLE orgs (
    name       TEXT PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by TEXT NOT NULL DEFAULT 'system'
);

-- repos: named tenants owned by an org.
-- name is the full path (e.g. "acme/myrepo" or "acme/team/subrepo").
-- owner must equal the first path segment of name.
CREATE TABLE repos (
    name       TEXT PRIMARY KEY,
    owner      TEXT NOT NULL REFERENCES orgs(name),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by TEXT NOT NULL DEFAULT 'system',
    CHECK (owner = split_part(name, '/', 1))
);

-- documents: immutable file versions, content-addressed by content_hash
CREATE TABLE documents (
    version_id   UUID PRIMARY KEY,
    path         TEXT NOT NULL,
    content      BYTEA NOT NULL,
    content_hash TEXT NOT NULL,
    content_type TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by   TEXT NOT NULL,
    repo         TEXT NOT NULL DEFAULT 'default/default' REFERENCES repos (name)
);

CREATE INDEX idx_documents_repo_content_hash ON documents (repo, content_hash);

-- branches: named pointers scoped to a repo
CREATE TABLE branches (
    repo          TEXT NOT NULL DEFAULT 'default/default' REFERENCES repos (name),
    name          TEXT NOT NULL,
    head_sequence BIGINT NOT NULL DEFAULT 0,
    base_sequence BIGINT NOT NULL DEFAULT 0,
    status        branch_status NOT NULL DEFAULT 'active',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by    TEXT NOT NULL DEFAULT 'system',
    PRIMARY KEY (repo, name)
);

-- commits: global monotonic sequence allocation, one row per atomic commit
CREATE TABLE commits (
    sequence         BIGSERIAL PRIMARY KEY,
    branch           TEXT NOT NULL,
    message          TEXT NOT NULL,
    author           TEXT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    repo             TEXT NOT NULL DEFAULT 'default/default' REFERENCES repos (name),
    commit_hash      TEXT,
    rekor_uuid       TEXT,
    signature_bundle JSONB
);

-- file_commits: core event log, one row per file change within a commit
CREATE TABLE file_commits (
    commit_id  UUID PRIMARY KEY,
    sequence   BIGINT NOT NULL REFERENCES commits (sequence),
    path       TEXT NOT NULL,
    version_id UUID REFERENCES documents (version_id),
    branch     TEXT NOT NULL,
    repo       TEXT NOT NULL DEFAULT 'default/default' REFERENCES repos (name)
);

CREATE INDEX idx_file_commits_repo_branch_path_seq ON file_commits (repo, branch, path, sequence DESC);
CREATE INDEX idx_file_commits_repo_sequence ON file_commits (repo, sequence);

-- roles: identity-to-permission mapping scoped to a repo
CREATE TABLE roles (
    repo     TEXT NOT NULL DEFAULT 'default/default' REFERENCES repos (name),
    identity TEXT NOT NULL,
    role     role_type NOT NULL,
    PRIMARY KEY (repo, identity)
);

-- reviews: approval/rejection records scoped to repo + branch + sequence
CREATE TABLE reviews (
    id         UUID PRIMARY KEY,
    repo       TEXT NOT NULL DEFAULT 'default/default' REFERENCES repos (name),
    branch     TEXT NOT NULL,
    reviewer   TEXT NOT NULL,
    sequence   BIGINT NOT NULL,
    status     review_status NOT NULL,
    body       TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT reviews_repo_branch_fk FOREIGN KEY (repo, branch) REFERENCES branches (repo, name)
);

CREATE INDEX idx_reviews_repo_branch_sequence ON reviews (repo, branch, sequence);

-- check_runs: external CI status reports scoped to repo + branch + sequence
CREATE TABLE check_runs (
    id         UUID PRIMARY KEY,
    repo       TEXT NOT NULL DEFAULT 'default/default' REFERENCES repos (name),
    branch     TEXT NOT NULL,
    sequence   BIGINT NOT NULL,
    check_name TEXT NOT NULL,
    status     check_status NOT NULL,
    reporter   TEXT NOT NULL,
    log_url    TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT check_runs_repo_branch_fk FOREIGN KEY (repo, branch) REFERENCES branches (repo, name)
);

CREATE INDEX idx_check_runs_repo_branch_seq_name ON check_runs (repo, branch, sequence, check_name);

-- event_subscriptions: webhook (and future Pub/Sub) delivery targets
-- repo NULL means all repos; event_types NULL means all event types.
CREATE TABLE event_subscriptions (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    repo          TEXT REFERENCES repos(name),
    event_types   TEXT[],
    backend       TEXT NOT NULL CHECK (backend IN ('webhook', 'pubsub')),
    config        JSONB NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by    TEXT NOT NULL,
    suspended_at  TIMESTAMPTZ,
    failure_count INT NOT NULL DEFAULT 0
);

-- event_outbox: pending and delivered outbox rows for webhook delivery.
-- Rows are cleaned up after 7 days.
CREATE TABLE event_outbox (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event           JSONB NOT NULL,
    subscription_id UUID REFERENCES event_subscriptions(id) ON DELETE CASCADE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    attempts        INT NOT NULL DEFAULT 0,
    next_attempt    TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at    TIMESTAMPTZ,
    last_error      TEXT
);

CREATE INDEX idx_event_outbox_pending ON event_outbox (next_attempt)
    WHERE delivered_at IS NULL AND attempts < 10;

CREATE INDEX idx_event_subscriptions_active_webhook ON event_subscriptions (backend)
    WHERE suspended_at IS NULL AND backend = 'webhook';

-- Seed the default org, default repo, and main branch
INSERT INTO orgs (name, created_by) VALUES ('default', 'system');
INSERT INTO repos (name, owner, created_by) VALUES ('default/default', 'default', 'system');
INSERT INTO branches (repo, name, head_sequence, base_sequence, status) VALUES ('default/default', 'main', 0, 0, 'active');
