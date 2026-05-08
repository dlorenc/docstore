CREATE TABLE repo_secrets (
    id            TEXT PRIMARY KEY,
    repo          TEXT NOT NULL,
    name          TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    ciphertext    BYTEA NOT NULL,
    nonce         BYTEA NOT NULL,
    encrypted_dek BYTEA NOT NULL,
    kms_key_name  TEXT NOT NULL,
    size_bytes    INT NOT NULL,
    created_by    TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL,
    updated_by    TEXT,
    updated_at    TIMESTAMPTZ,
    last_used_at  TIMESTAMPTZ,
    UNIQUE (repo, name)
);

CREATE INDEX repo_secrets_by_repo ON repo_secrets (repo);
