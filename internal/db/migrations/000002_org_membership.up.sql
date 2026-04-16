-- Org membership and invitation tables.

CREATE TYPE org_role AS ENUM ('owner', 'member');

-- org_members: explicit org-level role assignments
CREATE TABLE org_members (
    org        TEXT NOT NULL REFERENCES orgs(name),
    identity   TEXT NOT NULL,
    role       org_role NOT NULL,
    invited_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org, identity)
);

-- org_invites: pending email invitations to join an org
CREATE TABLE org_invites (
    id          UUID PRIMARY KEY,
    org         TEXT NOT NULL REFERENCES orgs(name),
    email       TEXT NOT NULL,
    role        org_role NOT NULL,
    invited_by  TEXT NOT NULL,
    token       TEXT NOT NULL UNIQUE,
    expires_at  TIMESTAMPTZ NOT NULL,
    accepted_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
