-- +goose Up

-- Per-TOKEN rate-limit gauge (PRD #104 M5, D4). Anthropic's 5-hour and 7-day
-- windows are per-credential, so the moment a user holds two tokens a single
-- per-user row stops describing anything: two credentials race for one row and the
-- meters flip between accounts every tick, rendering confidently while describing
-- an account most of the user's workers are no longer spending against. That is
-- worse than no meter, which is why the table is repointed rather than left alone.
--
-- Keyed on user_secret_id; user_id is RETAINED (not derived at read time) because
-- the admin cross-user view groups by user, and because it is half of the composite
-- FK below. The pair is what makes a gauge row belong to a (user, token) that
-- actually exists: (user_id, user_secret_id) → user_secrets (user_id, id), the same
-- D11 ownership shape 00078 and 00079 use.
--
-- ON DELETE CASCADE, not SET NULL, and so no column list is needed here: a gauge
-- reading for a deleted credential is meaningless, so the row goes with it. This
-- also SUBSUMES PRD #53's D3b app-level cleanup — the "delete the token, then
-- delete its gauge row" pair that existed so a token-less user never showed a ghost
-- reading. The database now guarantees it, and a failed cleanup can no longer leave
-- one behind.
--
-- DROP + CREATE rather than ALTER (R2). The reading is a disposable gauge with no
-- history by design (PRD #53 D4 — see the comment in 00065), every row is
-- overwritten on the next poll tick, and there is no sensible way to attribute an
-- existing per-USER reading to one of that user's tokens. So the rows are dropped
-- and the poller refills them.
--
-- CAVEAT, and it is a real one: a deployment with the poller DISABLED
-- (UZI_USAGE_POLL_INTERVAL=0 — which is what the e2e overlay sets) never refills,
-- so meters there read `unavailable` permanently after this migration rather than
-- showing a stale-but-present reading. That is the honest outcome for a gauge
-- nothing is updating, and `unavailable` is already how the read path renders an
-- absent row (handler/ratelimits.go), so nothing errors — but an operator who
-- disabled polling and still expected numbers should know why they stopped.
--
-- Down restores the per-user shape, likewise empty. It cannot reconstruct which
-- token a reading described, and inventing one would be worse than an empty gauge.
DROP TABLE anthropic_rate_limits;

CREATE TABLE anthropic_rate_limits (
    user_secret_id      UUID PRIMARY KEY,
    user_id             UUID NOT NULL,
    five_hour_pct       SMALLINT CHECK (five_hour_pct BETWEEN 0 AND 100),
    five_hour_resets_at TIMESTAMPTZ,
    seven_day_pct       SMALLINT CHECK (seven_day_pct BETWEEN 0 AND 100),
    seven_day_resets_at TIMESTAMPTZ,
    source              TEXT CHECK (source IN ('usage_endpoint', 'header_probe')),
    synced_at           TIMESTAMPTZ NOT NULL,
    FOREIGN KEY (user_id, user_secret_id)
        REFERENCES user_secrets (user_id, id) ON DELETE CASCADE
);

-- The admin view lists every user's tokens grouped by user, and the D3b-successor
-- cleanup reads by user; neither can use the primary key.
CREATE INDEX idx_anthropic_rate_limits_user ON anthropic_rate_limits (user_id);

-- +goose Down
DROP TABLE anthropic_rate_limits;

CREATE TABLE anthropic_rate_limits (
    user_id             UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    five_hour_pct       SMALLINT CHECK (five_hour_pct BETWEEN 0 AND 100),
    five_hour_resets_at TIMESTAMPTZ,
    seven_day_pct       SMALLINT CHECK (seven_day_pct BETWEEN 0 AND 100),
    seven_day_resets_at TIMESTAMPTZ,
    source              TEXT CHECK (source IN ('usage_endpoint', 'header_probe')),
    synced_at           TIMESTAMPTZ NOT NULL
);
