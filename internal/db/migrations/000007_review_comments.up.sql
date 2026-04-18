-- review_comments: inline file annotations on a branch.
-- version_id is NOT NULL because comments on deleted files are not supported.
-- review_id is nullable — comments may exist independently of a formal review.
-- sequence records the branch head_sequence at the time the comment was created.

CREATE TABLE review_comments (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    review_id  UUID REFERENCES reviews(id),
    repo       TEXT NOT NULL REFERENCES repos(name),
    branch     TEXT NOT NULL,
    path       TEXT NOT NULL,
    version_id UUID NOT NULL REFERENCES documents(version_id),
    body       TEXT NOT NULL,
    author     TEXT NOT NULL,
    sequence   BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT review_comments_repo_branch_fk FOREIGN KEY (repo, branch) REFERENCES branches(repo, name)
);

CREATE INDEX idx_review_comments_repo_branch ON review_comments (repo, branch);
CREATE INDEX idx_review_comments_repo_branch_path ON review_comments (repo, branch, path);
