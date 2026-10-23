-- +goose Up

ALTER TABLE runs ADD COLUMN continued_issue_iid bigint;
ALTER TABLE runs ADD CONSTRAINT runs_continued_issue_shape CHECK (
    continued_issue_iid IS NULL
    OR (kind = 'task' AND continued_issue_iid > 0
        AND repo_id IS NOT NULL AND issue_iid IS NULL)
);

DROP INDEX uq_runs_one_active_per_issue;
CREATE UNIQUE INDEX uq_runs_one_active_per_issue
    ON runs (repo_id, coalesce(issue_iid, continued_issue_iid))
    WHERE kind IN ('issue', 'task')
      AND status NOT IN ('completed', 'failed', 'cancelled', 'pool_wait');

CREATE INDEX idx_runs_continued_issue_lookup
    ON runs (repo_id, continued_issue_iid)
    WHERE kind = 'task' AND continued_issue_iid IS NOT NULL;

-- +goose Down

DROP INDEX uq_runs_one_active_per_issue;
CREATE UNIQUE INDEX uq_runs_one_active_per_issue
    ON runs (repo_id, issue_iid)
    WHERE kind = 'issue' AND status NOT IN ('completed', 'failed', 'cancelled', 'pool_wait');

DROP INDEX idx_runs_continued_issue_lookup;
ALTER TABLE runs DROP CONSTRAINT runs_continued_issue_shape;
ALTER TABLE runs DROP COLUMN continued_issue_iid;
