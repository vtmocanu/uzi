-- +goose Up
-- An unpinned checker's effort is the cross-check default ("high"), independent of the
-- owner's worker effort, so its provenance gets its own value.
ALTER TABLE cross_checks DROP CONSTRAINT cross_checks_checker_effort_source_check;
ALTER TABLE cross_checks ADD CONSTRAINT cross_checks_checker_effort_source_check
    CHECK (checker_effort_source IN ('pin', 'worker default', 'cross-check default'));

-- +goose Down
-- Recorded provenance is history and cross_checks rows are write-once, so a rollback
-- refuses instead of rewriting them; the widened constraint stays in place.
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
