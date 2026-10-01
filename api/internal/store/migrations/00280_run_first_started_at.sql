-- +goose Up

-- Issue #2004: runs.first_started_at is the run's FIRST running stamp and is NEVER reset by any
-- writer. runs.started_at is the budget/timeout anchor and is deliberately NULLed by the
-- limit/recovery/pool/codex promotions and some requeue writers so a resumed leg gets a fresh RUN_TIMEOUT wall
-- (Decision 6d / #783); duration displays that subtract started_at therefore only showed the last
-- leg. SetRunRunning stamps this column once (COALESCE) and nothing else writes it. The migration
-- number is a draft: it is renumbered above the live head at landing.
ALTER TABLE runs ADD COLUMN first_started_at timestamptz NULL;
COMMENT ON COLUMN runs.first_started_at IS
    'Issue #2004: when the run first reached running; stamped once by SetRunRunning and never reset (display anchor). started_at stays the budget/timeout anchor and is reset by resume paths that grant a fresh wall.';

-- Legacy backfill: a started row seeds from started_at (the best surviving signal, possibly the
-- last leg only); a never-started or parked row (started_at NULL) stays NULL. created_at is
-- never read: it would put queue time into a duration.
UPDATE runs SET first_started_at = started_at WHERE started_at IS NOT NULL;

-- +goose Down
ALTER TABLE runs DROP COLUMN first_started_at;
