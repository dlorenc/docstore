-- releases: named immutable snapshots tied to a commit sequence.

CREATE TABLE releases (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    repo       TEXT NOT NULL REFERENCES repos(name),
    name       TEXT NOT NULL,
    sequence   BIGINT NOT NULL,
    body       TEXT,
    created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (repo, name)
);
