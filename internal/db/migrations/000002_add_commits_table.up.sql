-- Migration 002: Add commits table for globally monotonic sequence allocation.
-- The commits table provides a single BIGSERIAL counter shared across all
-- branches, replacing the broken per-branch headSeq+1 approach.

-- Global commit log: one row per atomic commit, BIGSERIAL gives global order.
CREATE TABLE commits (
    sequence   BIGSERIAL PRIMARY KEY,
    branch     TEXT NOT NULL,
    message    TEXT NOT NULL,
    author     TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Remove denormalized columns from file_commits and add FK to commits.
-- message/author/created_at belong in commits, not duplicated per file row.
ALTER TABLE file_commits
    DROP COLUMN message,
    DROP COLUMN author,
    DROP COLUMN created_at,
    ADD CONSTRAINT fk_file_commits_sequence
        FOREIGN KEY (sequence) REFERENCES commits(sequence);

-- Add created_at and created_by to branches per MVP.md spec.
ALTER TABLE branches
    ADD COLUMN created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN created_by TEXT NOT NULL DEFAULT 'system';
