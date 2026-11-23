-- +goose Up
-- PRD #4123 M3: the weekly-limit warning ledger. One row per credential (an Anthropic token OR a
-- Codex provider account) records the newest accepted observation instant, the anchored weekly
-- period (reset boundary and utilization, plus the Codex bucket and its sticky multi-window flag)
-- and whether this period's warning DM has been claimed. It is a bounded flag ledger: no history,
-- no queue, no pruning. A parent delete (the secret, the Codex account, the user) cascades the row.
--
-- Additive: a new table only. Adding the foreign keys takes a brief SHARE ROW EXCLUSIVE lock on the
-- parent tables, so the statement times out rather than queue behind a long transaction.
--
-- The migration number is a draft: it is renumbered above the live head at landing.
SET LOCAL lock_timeout = '5s';

CREATE TABLE weekly_limit_alerts (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id             uuid NOT NULL,
    anthropic_secret_id uuid NULL,
    codex_account_id    uuid NULL,
    observed_at         timestamptz NOT NULL,
    anchor_reset        timestamptz NULL,
    anchor_pct          double precision NULL,
    anchor_bucket       text NULL,
    anchor_multi        boolean NOT NULL DEFAULT false,
    warned              boolean NOT NULL DEFAULT false,
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT weekly_limit_alerts_secret_fk
        FOREIGN KEY (user_id, anthropic_secret_id) REFERENCES user_secrets (user_id, id) ON DELETE CASCADE,
    CONSTRAINT weekly_limit_alerts_codex_account_fk
        FOREIGN KEY (user_id, codex_account_id) REFERENCES codex_provider_account (user_id, id) ON DELETE CASCADE,
    CONSTRAINT weekly_limit_alerts_one_credential_check
        CHECK (num_nonnulls(anthropic_secret_id, codex_account_id) = 1),
    CONSTRAINT weekly_limit_alerts_codex_only_check
        CHECK (codex_account_id IS NOT NULL OR (anchor_bucket IS NULL AND NOT anchor_multi))
);

CREATE UNIQUE INDEX weekly_limit_alerts_user_secret_key
    ON weekly_limit_alerts (user_id, anthropic_secret_id);
CREATE UNIQUE INDEX weekly_limit_alerts_user_codex_account_key
    ON weekly_limit_alerts (user_id, codex_account_id);

-- +goose Down
SET LOCAL lock_timeout = '5s';

DROP TABLE weekly_limit_alerts;
