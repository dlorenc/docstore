-- Add a globally monotonic sequence for commit ordering.
-- Used by nextval('commit_sequence') in write transactions.
CREATE SEQUENCE commit_sequence START 1;
