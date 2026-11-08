-- Issue #4079: validate the NOT VALID runs_ci_fix_mr_iid_check added by 00417.

-- +goose Up
SET LOCAL lock_timeout = '2s';
ALTER TABLE runs VALIDATE CONSTRAINT runs_ci_fix_mr_iid_check;

-- +goose Down
ALTER TABLE runs DROP CONSTRAINT runs_ci_fix_mr_iid_check;
ALTER TABLE runs ADD CONSTRAINT runs_ci_fix_mr_iid_check
    CHECK (ci_fix_mr_iid IS NULL OR (kind = 'ci_fix' AND ci_fix_mr_iid > 0 AND ci_fix_mr_iid <= 9007199254740991)) NOT VALID;
