-- Issue #4079: a ci_fix run is bound at creation to the single open pull request of
-- its branch, so publication does not fail association_unknown when an older merged
-- PR's source run recorded the same reused branch with a different mr_iid. NULL means
-- unbound (no open PR on the branch). The CHECK is added NOT VALID and validated by
-- the next migration so this one holds only a brief lock.

-- +goose Up
SET LOCAL lock_timeout = '2s';
ALTER TABLE runs ADD COLUMN ci_fix_mr_iid bigint;
ALTER TABLE runs ADD CONSTRAINT runs_ci_fix_mr_iid_check
    CHECK (ci_fix_mr_iid IS NULL OR (kind = 'ci_fix' AND ci_fix_mr_iid > 0 AND ci_fix_mr_iid <= 9007199254740991)) NOT VALID;

-- +goose Down
ALTER TABLE runs DROP CONSTRAINT IF EXISTS runs_ci_fix_mr_iid_check;
ALTER TABLE runs DROP COLUMN IF EXISTS ci_fix_mr_iid;
