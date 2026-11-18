-- +goose Up
SET LOCAL lock_timeout = '5s';

-- PRD #4320 M6: a provider-bound run that hits a persistent gateway 429/529 parks as limit_wait with
-- rate_limit_type 'provider_gateway_limit' (no subscription window exists to name). This widens the
-- runs_rate_limit_type_check of 00091 by that one value, for runs whose model_provider_key_mode is set.
-- Existing rows satisfy the wider form; the new constraint is NOT VALID here (no full scan of the busy
-- runs table under the lock) and validated in 00479. This file touches runs only.
ALTER TABLE runs DROP CONSTRAINT runs_rate_limit_type_check;
ALTER TABLE runs ADD CONSTRAINT runs_rate_limit_type_check
    CHECK (rate_limit_type IS NULL OR rate_limit_type IN (
        'five_hour', 'seven_day', 'seven_day_opus', 'seven_day_sonnet',
        'seven_day_overage_included', 'overage', 'unknown', 'provider_gateway_limit'
    )) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- Rows carrying the new member would violate the narrower CHECK. rate_limit_type is display-only
-- history, so map them to 'unknown' (a real member of the 00091 vocabulary) rather than refuse. The
-- table is locked first so a concurrent park cannot land the new value between the rewrite and the swap.
LOCK TABLE runs IN SHARE ROW EXCLUSIVE MODE;
UPDATE runs SET rate_limit_type = 'unknown' WHERE rate_limit_type = 'provider_gateway_limit';
ALTER TABLE runs DROP CONSTRAINT runs_rate_limit_type_check;
ALTER TABLE runs ADD CONSTRAINT runs_rate_limit_type_check
    CHECK (rate_limit_type IS NULL OR rate_limit_type IN (
        'five_hour', 'seven_day', 'seven_day_opus', 'seven_day_sonnet',
        'seven_day_overage_included', 'overage', 'unknown'
    ));
