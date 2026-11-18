-- +goose Up
SET LOCAL lock_timeout = '5s';

-- PRD #4320 M6: a provider-bound run that hits a persistent gateway 429/529 parks as limit_wait with
-- rate_limit_type 'provider_gateway_limit' (no subscription window exists to name). This widens the
-- runs_rate_limit_type_check of 00091 by that one value, for runs whose model_provider_key_mode is set.
-- Existing rows satisfy the wider form; the new constraint is NOT VALID here (no full scan of the busy
-- runs table under the lock) and validated in 00479. This file touches runs only. The CHECK does not
-- tie the value to model_provider_key_mode: only the provider park path writes it, and the vocabulary
-- is what the constraint enforces.
ALTER TABLE runs DROP CONSTRAINT runs_rate_limit_type_check;
ALTER TABLE runs ADD CONSTRAINT runs_rate_limit_type_check
    CHECK (rate_limit_type IS NULL OR rate_limit_type IN (
        'five_hour', 'seven_day', 'seven_day_opus', 'seven_day_sonnet',
        'seven_day_overage_included', 'overage', 'unknown', 'provider_gateway_limit'
    )) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- The value gates Resume (a provider gateway limit_wait with a NULL retry_not_before waits for its
-- owner) and the park sweep. Coercing it to 'unknown' on a run still waiting would strand it beyond
-- what either can match, so refuse while any run is a limit_wait with this type (the 00198
-- convention) until the operator resumes or cancels it. Resume and cancel leave rate_limit_type in
-- place as history, so every other row carrying the value (terminal or resumed) is only a record and
-- is mapped to 'unknown' before the narrower CHECK is re-added.
-- ACCESS EXCLUSIVE up front, taken before the guard reads, so no concurrent park lands the value
-- between the check and the constraint swap and no lock upgrade (a deadlock source) happens.
LOCK TABLE runs IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM runs WHERE status = 'limit_wait' AND rate_limit_type = 'provider_gateway_limit') THEN
        RAISE EXCEPTION 'refusing to roll back 00478: run(s) are still waiting (limit_wait) on a provider gateway limit; resume or cancel those waiting runs before downgrading';
    END IF;
END
$$;
-- +goose StatementEnd
UPDATE runs SET rate_limit_type = 'unknown' WHERE rate_limit_type = 'provider_gateway_limit';
ALTER TABLE runs DROP CONSTRAINT runs_rate_limit_type_check;
ALTER TABLE runs ADD CONSTRAINT runs_rate_limit_type_check
    CHECK (rate_limit_type IS NULL OR rate_limit_type IN (
        'five_hour', 'seven_day', 'seven_day_opus', 'seven_day_sonnet',
        'seven_day_overage_included', 'overage', 'unknown'
    ));
