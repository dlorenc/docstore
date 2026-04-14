-- Reverse of initial schema — drop in reverse dependency order

DROP TABLE IF EXISTS check_runs;
DROP TABLE IF EXISTS reviews;
DROP TABLE IF EXISTS roles;
DROP TABLE IF EXISTS file_commits;
DROP TABLE IF EXISTS commits;
DROP TABLE IF EXISTS branches;
DROP TABLE IF EXISTS documents;
DROP TABLE IF EXISTS repos;
DROP TABLE IF EXISTS orgs;

DROP TYPE IF EXISTS check_status;
DROP TYPE IF EXISTS review_status;
DROP TYPE IF EXISTS role_type;
DROP TYPE IF EXISTS branch_status;
