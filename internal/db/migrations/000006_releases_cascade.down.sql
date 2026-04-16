-- Revert ON DELETE CASCADE from the releases → repos foreign key.

ALTER TABLE releases DROP CONSTRAINT releases_repo_fkey;
ALTER TABLE releases ADD CONSTRAINT releases_repo_fkey
    FOREIGN KEY (repo) REFERENCES repos(name);
