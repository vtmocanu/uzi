-- +goose Up

-- PRD #1247 M1: the per-run Anthropic credential override foundation seam. Additive
-- only, so an N-1 worker during a rolling release keeps working: every existing run
-- and schedule leaves the two override columns NULL, which resolves as "inherit the
-- worker binding" — today's behaviour, back-compat by construction (D1).
--
-- Every CHECK on a POPULATED table is added NOT VALID here and VALIDATE-d in the
-- sibling 00234, the lock-cheap two-step 00226/00227 established for a CHECK on a live
-- table: an inline validated CHECK would scan every row under the ACCESS EXCLUSIVE lock
-- the ALTER already holds, whereas VALIDATE CONSTRAINT later takes only a
-- write-compatible SHARE UPDATE EXCLUSIVE scan. The new run_credential_epochs table is
-- empty at creation, so its constraints are inline (an empty table validates instantly).

-- runs: the two override columns (D1) plus the held-state switch/release bookkeeping
-- (D3, D4). credential_override_mode NULL = inherit the worker binding; a 'pinned' mode
-- whose id is later nulled by a token delete also resolves as inherit. The FK is
-- ON DELETE SET NULL so a deleted token never leaves a dangling id — the claim inherits
-- rather than failing (D1, matching 00078's worker-binding rule).
ALTER TABLE runs ADD COLUMN credential_override_mode TEXT;
ALTER TABLE runs ADD COLUMN credential_override_secret_id UUID REFERENCES user_secrets(id) ON DELETE SET NULL;
-- claim_released_at: the released-claim fence (D3). Set by the held-state release
-- transition, cleared by ClaimRun; NULL for a run that has never released a claim.
ALTER TABLE runs ADD COLUMN claim_released_at TIMESTAMPTZ;
-- The pending held-state switch stamp (D4): when a switch was requested and the claim
-- generation it targets. Independent of the pause columns (D11).
ALTER TABLE runs ADD COLUMN credential_switch_requested_at TIMESTAMPTZ;
ALTER TABLE runs ADD COLUMN credential_switch_generation BIGINT;

-- run_schedules: the same two override columns so a fired run inherits the schedule's
-- choice (D5). NULL = inherit, exactly like the runs columns.
ALTER TABLE run_schedules ADD COLUMN credential_override_mode TEXT;
ALTER TABLE run_schedules ADD COLUMN credential_override_secret_id UUID REFERENCES user_secrets(id) ON DELETE SET NULL;

-- run_messages / run_usage: the originating claim generation on every canonical frame
-- and derived usage row (D7). Nullable, DEFAULT-less, additive — a hot-path column with
-- no table rewrite (the accepted cost in D7). The value is stamped server-side by the
-- fenced append (M5/M9); it is only the column here in M1.
ALTER TABLE run_messages ADD COLUMN claim_generation BIGINT;
ALTER TABLE run_usage ADD COLUMN claim_generation BIGINT;

-- run_credential_epochs: the attribution journal (D7, D14) — one row per claim, written
-- by recordRunCredential after a successful open, keyed by the run's existing
-- claim_generation (00223, PRD #1349; this PRD adds no generation and never increments
-- it outside ClaimRun). The composite PRIMARY KEY (run_id, claim_generation) gives one
-- row per claim and makes a same-generation re-record idempotent (ON CONFLICT DO UPDATE).
-- secret_id FK is ON DELETE SET NULL so a deleted token leaves the epoch's label/reason
-- intact while the id goes NULL, mirroring the run's own snapshot columns. There is no
-- CHECK on select_reason here: the journal records whatever reason the claim path chose,
-- and the closed vocabulary lives on runs.anthropic_select_reason.
CREATE TABLE run_credential_epochs (
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    claim_generation BIGINT NOT NULL,
    secret_id UUID NULL REFERENCES user_secrets(id) ON DELETE SET NULL,
    label TEXT,
    select_reason TEXT,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, claim_generation)
);

-- Close the override-mode vocabulary to its three legal values on both tables. Added
-- NOT VALID (enforced for new/updated rows immediately; the backlog scan deferred to
-- 00234). NULL stays legal on both — it is the inherit default every existing row carries.
ALTER TABLE runs ADD CONSTRAINT runs_credential_override_mode_check
    CHECK (credential_override_mode IS NULL OR credential_override_mode IN ('pinned', 'auto', 'default')) NOT VALID;
ALTER TABLE run_schedules ADD CONSTRAINT run_schedules_credential_override_mode_check
    CHECK (credential_override_mode IS NULL OR credential_override_mode IN ('pinned', 'auto', 'default')) NOT VALID;

-- Widen runs.anthropic_select_reason to admit the two per-run override reasons
-- (run_pinned = a per-run override named a token; run_default = a per-run override of
-- mode 'default'). The two-step DROP + re-ADD NOT VALID is required because 00089 added
-- this CHECK validated: re-adding NOT VALID keeps the add lock-cheap and defers the
-- backlog scan to 00234. The IN-list is 00089's eight (see 00089's comment) plus
-- run_pinned and run_default; an `auto` override reuses the selector's own reasons, so
-- no auto-specific value is added.
ALTER TABLE runs DROP CONSTRAINT runs_anthropic_select_reason_check;
ALTER TABLE runs ADD CONSTRAINT runs_anthropic_select_reason_check
    CHECK (anthropic_select_reason IS NULL OR anthropic_select_reason IN (
        'default', 'pinned', 'judge',
        'auto', 'best_of_pool', 'pool_empty', 'pool_stale', 'open_failed',
        'run_pinned', 'run_default'
    )) NOT VALID;

-- +goose Down

-- True inverse of the additive Up (goose downs are not run in this deployment;
-- store.Migrate only ever goes up). Drop the constraints, then the columns, then the
-- table; the select-reason CHECK is restored to 00089's validated eight-value form.
ALTER TABLE runs DROP CONSTRAINT runs_anthropic_select_reason_check;
ALTER TABLE runs ADD CONSTRAINT runs_anthropic_select_reason_check
    CHECK (anthropic_select_reason IS NULL OR anthropic_select_reason IN (
        'default', 'pinned', 'judge',
        'auto', 'best_of_pool', 'pool_empty', 'pool_stale', 'open_failed'
    ));
ALTER TABLE run_schedules DROP CONSTRAINT run_schedules_credential_override_mode_check;
ALTER TABLE runs DROP CONSTRAINT runs_credential_override_mode_check;

DROP TABLE run_credential_epochs;

ALTER TABLE run_usage DROP COLUMN IF EXISTS claim_generation;
ALTER TABLE run_messages DROP COLUMN IF EXISTS claim_generation;

ALTER TABLE run_schedules DROP COLUMN IF EXISTS credential_override_secret_id;
ALTER TABLE run_schedules DROP COLUMN IF EXISTS credential_override_mode;

ALTER TABLE runs DROP COLUMN IF EXISTS credential_switch_generation;
ALTER TABLE runs DROP COLUMN IF EXISTS credential_switch_requested_at;
ALTER TABLE runs DROP COLUMN IF EXISTS claim_released_at;
ALTER TABLE runs DROP COLUMN IF EXISTS credential_override_secret_id;
ALTER TABLE runs DROP COLUMN IF EXISTS credential_override_mode;
