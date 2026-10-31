-- Read-only admission check for rolling back gated Plan cross-check (PRD #3708).
--
-- Run it with the upgraded api STOPPED, as the final step before either a schema rollback
-- (goose down to the version below 00387) or a binary-only downgrade:
--
--   psql -v ON_ERROR_STOP=1 -1 -f scripts/gated-cross-check-drain-check.sql "$DATABASE_URL"
--
-- It raises (and psql exits non-zero) while any gated run is not terminal. It writes
-- nothing. The statements below are identical to the guard at the top of the Down of
-- 00387 and 00388; a test keeps the three copies equal. Procedure: docs/cross-check.md,
-- "Rolling back Plan cross-check before the human gate".
SET LOCAL lock_timeout = '5s';
LOCK TABLE runs IN SHARE ROW EXCLUSIVE MODE;
DO $$
DECLARE n bigint;
BEGIN
    SELECT count(*) INTO n FROM runs
    WHERE plan_cross_check_gated AND status NOT IN ('completed', 'failed', 'cancelled');
    IF n > 0 THEN
        RAISE EXCEPTION 'uzi: % non-terminal gated plan cross-check runs exist; abort the rollback', n;
    END IF;
END
$$;
