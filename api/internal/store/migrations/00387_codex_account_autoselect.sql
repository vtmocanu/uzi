-- +goose Up

-- Issue #3719: auto-select a Codex subscription account by limit headroom at run
-- creation. Additive only, so an N-1 worker keeps working: every existing user reads
-- codex_account_mode 'default' (today's behaviour) and every existing run leaves
-- codex_select_reason NULL (a historical row, rendered as "not recorded").
--
-- The migration number is a draft: it is renumbered above the live head at landing.
--
-- users and runs are hot tables, so the statement times out rather than queue behind a
-- long writer and stall everyone behind it. Plain ADD COLUMNs (a constant default needs
-- no rewrite) come first; every CHECK on a populated table is added NOT VALID and
-- VALIDATE-d in the sibling 00388, the lock-cheap two-step 00233/00234 established.
SET LOCAL lock_timeout = '5s';

-- users.codex_account_mode: the per-user opt-in. 'default' keeps today's binding; 'auto'
-- picks among the user's pooled, consented Codex aliases at run creation.
ALTER TABLE users ADD COLUMN codex_account_mode TEXT NOT NULL DEFAULT 'default';
ALTER TABLE users ADD CONSTRAINT users_codex_account_mode_check
    CHECK (codex_account_mode IN ('default', 'auto')) NOT VALID;

-- runs.codex_select_reason: why the creation path bound the run's Codex alias. The
-- closed vocabulary has one Go home, codexselect.AllReasons (a guard test compares it
-- with this list). NULL for a Claude run and for any run created before this migration.
ALTER TABLE runs ADD COLUMN codex_select_reason TEXT;
ALTER TABLE runs ADD CONSTRAINT runs_codex_select_reason_check
    CHECK (codex_select_reason IS NULL OR (
        harness = 'codex' AND codex_select_reason IN (
            'default', 'pool_empty', 'auto', 'best_of_pool', 'pool_stale', 'pool_exhausted'
        )
    )) NOT VALID;

-- user_secrets.auto_eligible: widen the kind CHECK (00087) so a codex_auth alias can be
-- pooled. The two-step DROP + re-ADD NOT VALID keeps the add lock-cheap; an
-- openai_api_key row is still rejected. Existing rows only ever satisfied the narrower
-- form, so the widened form cannot be violated by the backlog.
ALTER TABLE user_secrets DROP CONSTRAINT user_secrets_auto_eligible_kind_check;
ALTER TABLE user_secrets ADD CONSTRAINT user_secrets_auto_eligible_kind_check
    CHECK (NOT auto_eligible OR kind IN ('anthropic_token', 'codex_auth')) NOT VALID;

-- +goose Down

-- LOSSY: this Down permanently erases every Codex pool opt-in (auto_eligible on
-- codex_auth rows), every users.codex_account_mode and every runs.codex_select_reason.
-- goose downs are not run in this deployment (store.Migrate only goes up); the lock below
-- keeps a concurrent opt-in from landing between the UPDATE and the narrower CHECK.
SET LOCAL lock_timeout = '5s';

LOCK TABLE user_secrets IN SHARE ROW EXCLUSIVE MODE;
UPDATE user_secrets SET auto_eligible = false WHERE kind = 'codex_auth' AND auto_eligible;

ALTER TABLE user_secrets DROP CONSTRAINT user_secrets_auto_eligible_kind_check;
ALTER TABLE user_secrets ADD CONSTRAINT user_secrets_auto_eligible_kind_check
    CHECK (NOT auto_eligible OR kind = 'anthropic_token');

ALTER TABLE runs DROP CONSTRAINT runs_codex_select_reason_check;
ALTER TABLE runs DROP COLUMN IF EXISTS codex_select_reason;

ALTER TABLE users DROP CONSTRAINT users_codex_account_mode_check;
ALTER TABLE users DROP COLUMN IF EXISTS codex_account_mode;
