CREATE TYPE proposal_state AS ENUM ('open', 'closed', 'merged');

CREATE TABLE proposals (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    repo        TEXT NOT NULL REFERENCES repos(name),
    branch      TEXT NOT NULL,
    base_branch TEXT NOT NULL DEFAULT 'main',
    title       TEXT NOT NULL,
    description TEXT,
    author      TEXT NOT NULL,
    state       proposal_state NOT NULL DEFAULT 'open',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT proposals_repo_branch_fk FOREIGN KEY (repo, branch) REFERENCES branches(repo, name)
);

CREATE UNIQUE INDEX proposals_one_open_per_branch
    ON proposals (repo, branch)
    WHERE state = 'open';
