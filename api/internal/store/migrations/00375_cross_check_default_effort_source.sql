-- +goose Up
-- An unpinned checker's effort is the cross-check default ("high"), independent of the
-- owner's worker effort, so its provenance gets its own value. The widened CHECK is added
-- NOT VALID and validated in 00376, the 00367/00368 two-step: no full-table scan runs
-- under this migration's ACCESS EXCLUSIVE lock. New writes are checked immediately.
SET LOCAL lock_timeout = '5s';
ALTER TABLE cross_checks DROP CONSTRAINT cross_checks_checker_effort_source_check;
ALTER TABLE cross_checks ADD CONSTRAINT cross_checks_checker_effort_source_check
    CHECK (checker_effort_source IN ('pin', 'worker default', 'cross-check default')) NOT VALID;

-- +goose Down
-- Recorded provenance is history and cross_checks rows are write-once, so a rollback
-- refuses instead of rewriting them. The guard names the reason; the two-value CHECK is
-- re-added VALIDATED, so a row written after the guard also makes this Down fail and roll
-- back rather than slip under a NOT VALID constraint.
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM cross_checks WHERE checker_effort_source = 'cross-check default') THEN
        RAISE EXCEPTION 'cross_checks rows record checker_effort_source ''cross-check default''; refusing to narrow the constraint';
    END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE cross_checks DROP CONSTRAINT cross_checks_checker_effort_source_check;
ALTER TABLE cross_checks ADD CONSTRAINT cross_checks_checker_effort_source_check
    CHECK (checker_effort_source IN ('pin', 'worker default'));
