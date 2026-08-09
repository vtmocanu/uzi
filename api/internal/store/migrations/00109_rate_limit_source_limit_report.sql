-- +goose Up

-- Widen anthropic_rate_limits.source to admit 'limit_report' (PRD #217 M1).
--
-- The park path now writes the dead credential's exhausted window into the gauge
-- (100% consumed) at usage-limit time, and marks WHERE that reading came from with
-- source = 'limit_report' — an inference drawn off a worker's limit report, not a
-- measurement (usage_endpoint) nor a header probe (header_probe). D6: an operator
-- reading the row must be able to tell a park-time inference from a poll, and — given
-- that M1 deliberately does NOT bump synced_at (D3) — that a 100% reading is newer
-- than the synced_at beside it.
--
-- The constraint being widened is UNNAMED in 00080_rate_limits_per_token.sql:48
-- (declared inline on the column), so Postgres auto-generated its name. The pattern
-- and the verification are 00091's: a wrong DROP CONSTRAINT fails at boot, so the
-- name is checked against a live database rather than assumed:
--   SELECT conname FROM pg_constraint
--     WHERE conrelid='anthropic_rate_limits'::regclass AND contype='c';
--   -> anthropic_rate_limits_source_check
--
-- Bare DROP CONSTRAINT with NO `IF EXISTS`, deliberately, per 00091's reasoning: a
-- skipped drop against a differently-named constraint would let the widened ADD
-- succeed alongside the surviving narrow one, and every park would then raise 23514
-- at runtime on a healthy-looking instance. Failing at boot, naming the constraint,
-- is strictly better than failing later on a user's run.
--
-- Go's half of this vocabulary is anthropic.AllSources(); M4 adds a drift test that
-- parses the list below out of this file and compares, matching 00089's
-- TestSelectReasonVocabularyMatchesCheck and 00091's
-- TestRateLimitTypeVocabularyMatchesCheck.
ALTER TABLE anthropic_rate_limits DROP CONSTRAINT anthropic_rate_limits_source_check;
ALTER TABLE anthropic_rate_limits ADD CONSTRAINT anthropic_rate_limits_source_check
    CHECK (source IN ('usage_endpoint', 'header_probe', 'limit_report'));

-- +goose Down

-- Narrows back to the two-value set. This FAILS LOUDLY if any row currently carries
-- source = 'limit_report', which is correct: a rollback that silently rewrote a
-- park-time reading to some other source would launder away the one signal an
-- operator has that the row is an inference rather than a measurement. Clear or age
-- out the limit_report rows first (the poller overwrites them on its next tick).
ALTER TABLE anthropic_rate_limits DROP CONSTRAINT anthropic_rate_limits_source_check;
ALTER TABLE anthropic_rate_limits ADD CONSTRAINT anthropic_rate_limits_source_check
    CHECK (source IN ('usage_endpoint', 'header_probe'));
