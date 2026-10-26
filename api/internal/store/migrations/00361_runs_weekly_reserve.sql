-- +goose Up
-- PRD #3522 M1: run-side state of the weekly usage reserve, in ONE ALTER so runs is locked
-- once. The five columns are unused until the later milestones (a persisted hold reason for a
-- queued run, and the manual-resume exemption scoped to one credential and one observed
-- 7-day reset); landing them with the settings keeps the schema change a single additive step.
--   reserve_hold_reason          the visible reason a queued run is held
--   reserve_exempt_until         the exemption's expiry (the next observed 7-day reset)
--   reserve_exempt_granted_at    when the owner's manual resume granted it
--   reserve_exempt_credential_id the credential the exemption was granted against
--   reserve_exempt_window_reset  the 7-day reset the exemption was granted under
-- All nullable with no default (NULL = none), so no existing row changes.
--
-- runs_pause_mode_check is widened to a FOURTH value 'reserve', the system-authored pause the
-- reserve files. The three existing values are carried VERBATIM from the live constraint
-- (00242's Up: 'milestone', 'now', 'wall'). Added NOT VALID, validated in 00362.
--
-- The migration number is a draft: renumber the trio together above the live head at landing.
SET LOCAL lock_timeout = '5s';

ALTER TABLE runs DROP CONSTRAINT IF EXISTS runs_pause_mode_check;
ALTER TABLE runs
    ADD COLUMN reserve_hold_reason text,
    ADD COLUMN reserve_exempt_until timestamptz,
    ADD COLUMN reserve_exempt_granted_at timestamptz,
    ADD COLUMN reserve_exempt_credential_id uuid,
    ADD COLUMN reserve_exempt_window_reset timestamptz,
    ADD CONSTRAINT runs_pause_mode_check
        CHECK (pause_mode IS NULL OR pause_mode IN ('milestone', 'now', 'wall', 'reserve')) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';

-- Narrowing is DATA-DEPENDENT, as in 00242's Down: the three-value CHECK is re-added VALIDATED
-- (not NOT VALID), so it FAILS while any row still holds pause_mode = 'reserve' and this
-- migration then refuses to come down. Drain or resume the reserve-paused runs first.
ALTER TABLE runs DROP CONSTRAINT runs_pause_mode_check;
ALTER TABLE runs
    DROP COLUMN reserve_exempt_window_reset,
    DROP COLUMN reserve_exempt_credential_id,
    DROP COLUMN reserve_exempt_granted_at,
    DROP COLUMN reserve_exempt_until,
    DROP COLUMN reserve_hold_reason;
ALTER TABLE runs ADD CONSTRAINT runs_pause_mode_check
    CHECK (pause_mode IS NULL OR pause_mode IN ('milestone', 'now', 'wall'));
