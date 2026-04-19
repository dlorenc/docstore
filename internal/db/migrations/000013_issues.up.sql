CREATE TYPE issue_state AS ENUM ('open', 'closed');
CREATE TYPE issue_close_reason AS ENUM ('completed', 'not_planned', 'duplicate');
CREATE TYPE issue_ref_type AS ENUM ('proposal', 'commit');

CREATE TABLE issues (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    repo         TEXT NOT NULL REFERENCES repos(name),
    number       BIGINT NOT NULL,
    title        TEXT NOT NULL,
    body         TEXT,
    author       TEXT NOT NULL,
    state        issue_state NOT NULL DEFAULT 'open',
    close_reason issue_close_reason,
    closed_by    TEXT,
    labels       TEXT[] NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (repo, number)
);

CREATE TABLE issue_comments (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    issue_id   UUID NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    repo       TEXT NOT NULL REFERENCES repos(name),
    body       TEXT NOT NULL,
    author     TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    edited_at  TIMESTAMPTZ
);

CREATE TABLE issue_refs (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    issue_id   UUID NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    repo       TEXT NOT NULL REFERENCES repos(name),
    ref_type   issue_ref_type NOT NULL,
    ref_id     TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (issue_id, ref_type, ref_id)
);

CREATE INDEX issues_repo_state   ON issues (repo, state);
CREATE INDEX issues_repo_number  ON issues (repo, number);
CREATE INDEX issue_comments_issue_id ON issue_comments (issue_id);
CREATE INDEX issue_refs_issue_id ON issue_refs (issue_id);
CREATE INDEX issue_refs_repo_ref ON issue_refs (repo, ref_type, ref_id);
