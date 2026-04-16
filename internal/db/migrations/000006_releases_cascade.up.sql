-- Add ON DELETE CASCADE to the releases → repos foreign key.
-- Without this, deleting a repo leaves orphaned release rows.

ALTER TABLE releases DROP CONSTRAINT releases_repo_fkey;
ALTER TABLE releases ADD CONSTRAINT releases_repo_fkey
    FOREIGN KEY (repo) REFERENCES repos(name) ON DELETE CASCADE;
