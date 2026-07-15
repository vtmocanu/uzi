-- +goose Up

-- Per-user Claude rate-limit gauge (PRD #53). ONE row per user, no history (D4):
-- a live gauge of the two account-wide Anthropic windows (5-hour, 7-day), polled
-- server-side with the user's own token (D1) and overwritten each poll tick. The
-- token itself never leaves the api container; only percentages + reset epochs
-- land here.
--
-- five_hour_pct / seven_day_pct are the floor+clamped utilization (0..100). They
-- are nullable at the DDL level, but a row is only ever written after a
-- fail-closed reading that carries both (D5), so in practice they are always set;
-- the CHECK bounds them either way. *_resets_at are what Anthropic reported
-- (nullable — a reading without a reset is still valid, D7). source records which
-- path produced the reading. synced_at is the only NOT NULL column: staleness is
-- computed from it server-side (D3).
--
-- Cascade-deletes with the user; the token-delete path additionally runs
-- DeleteRateLimits (D3b) so a token-less user never shows a ghost reading.
--
-- Draft number 00080; renumber to the next free slot above the live head at merge
-- per the goose-numbering convention (live head 00064; 00070 held by PRD #41,
-- 00095 by PRD #35).
CREATE TABLE anthropic_rate_limits (
    user_id             UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    five_hour_pct       SMALLINT CHECK (five_hour_pct BETWEEN 0 AND 100),
    five_hour_resets_at TIMESTAMPTZ,
    seven_day_pct       SMALLINT CHECK (seven_day_pct BETWEEN 0 AND 100),
    seven_day_resets_at TIMESTAMPTZ,
    source              TEXT CHECK (source IN ('usage_endpoint', 'header_probe')),
    synced_at           TIMESTAMPTZ NOT NULL
);

-- +goose Down
DROP TABLE anthropic_rate_limits;
