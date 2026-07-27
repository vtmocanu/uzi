-- +goose Up

-- PRD #35 M-SEAM: the schema half of "park a run until the Anthropic usage limit
-- resets, then resume it". This migration adds storage and widens the status
-- domain; NOTHING reads or writes any of it yet. The worker-side detection (M1)
-- and the server park machinery (M2) land on top.
--
-- DRAFT NUMBER. Live head at authoring is 00089_run_select_reason_check.sql.
-- Per CLAUDE.md, renumber to the next free number above the live head on the
-- landing rebase: the boot runner is strict goose (no allow-missing,
-- store/migrate.go), so landing a version BELOW an already-applied head makes
-- every upgraded instance refuse to boot.

-- runs ---------------------------------------------------------------------
--
-- wait_on_limit is the per-run opt-in (Decision 7), stamped at creation from the
-- owner's users.wait_on_limit default or an explicit request override. DEFAULT
-- false makes every existing run, and every creation path not yet taught to stamp
-- it, opt OUT — the safe direction, since an un-opted run keeps today's behaviour
-- exactly (it fails, now with a better reason). M3 teaches all four creation paths.
--
-- limit_resets_at is when the exhausted window reopens, as the worker read it off
-- the SDK frame. It is REPORTED DATA, kept for display and for the Slack line; it
-- is deliberately NOT the promotion gate, because a compromised worker must not be
-- able to park a run for years.
--
-- retry_not_before IS the promotion gate, and it is computed server-side (M2):
-- cross-checked against this user's own anthropic_rate_limits gauge, made
-- pool-aware (Decision 6e), jittered to avoid a stampede across one user's parked
-- runs, and clamped to RUN_LIMIT_MAX_PARK. Nullable because every pre-feature row
-- has no park.
--
-- limit_wait_count counts parks for THIS run and is capped by RUN_LIMIT_MAX_WAITS.
-- It is deliberately distinct from requeue_count rather than folded into it
-- (Decision 5, mirroring the vault lock-race precedent in specs/ai.md §139): the
-- two count different failures — requeue_count is worker deaths, limit_wait_count
-- is limit parks — and a shared counter would let one exhaust the other's budget.
--
-- rate_limit_type is the SDK's rateLimitType for the window that rejected us,
-- after the server has allowlisted it against the SDK union (anything unrecognised
-- is coerced to 'unknown' before it reaches this column). It lives on the ROW, not
-- only in the feed message, because GetSlackRunContext (slack.sql) is an explicit
-- column list selected FROM runs and cannot reach a run_messages payload — the
-- "paused: usage limit (five_hour)" line is unbuildable without it.
--
-- NO CHECK on rate_limit_type, deliberately, and this is the same argument
-- 00086_run_anthropic_secret.sql made for anthropic_select_reason: the vocabulary
-- is the SDK's, not ours, so an SDK pin bump can add a member and a CHECK written
-- today "would have been rewritten before it ever guarded anything". 00089 closed
-- that column only once its set stopped moving. The Go allowlist is the guard here,
-- and a test pins it against the installed SDK typings.
ALTER TABLE runs
    ADD COLUMN wait_on_limit    boolean     NOT NULL DEFAULT false,
    ADD COLUMN limit_resets_at  timestamptz,
    ADD COLUMN retry_not_before timestamptz,
    ADD COLUMN limit_wait_count int         NOT NULL DEFAULT 0,
    ADD COLUMN rate_limit_type  text;

-- users --------------------------------------------------------------------
--
-- The per-user default a new run inherits (Decision 7), an exact clone of the
-- users.autopilot_enabled precedent (00037_autopilot_mapping.sql): per-user consent
-- set from the user's own Settings page, default false. app_settings is NOT used —
-- it is admin/instance-wide only ("there is no user_settings table",
-- 00044_slack.sql) — and the caps are env config like the other run clocks.
ALTER TABLE users
    ADD COLUMN wait_on_limit boolean NOT NULL DEFAULT false;

-- status domain ------------------------------------------------------------
--
-- 'limit_wait' is a NON-TERMINAL status: a parked run still holds its issue (the
-- uq_runs_one_active_per_issue partial index is a negative guard and therefore
-- already counts it as active), still cancels server-side (CancelRunServerSide is
-- likewise a negative guard), and is invisible to every sweeper filter and to the
-- PRD #47 health detector because each of those is a POSITIVE allowlist. None of
-- them needed editing; see PRD #35 Decision 3 for the site-by-site argument.
--
-- The constraint is unnamed in 00020_workers_runs.sql (declared inline on the
-- column), so Postgres auto-generated its name. Verified against a live database
-- rather than assumed, since a wrong DROP CONSTRAINT fails at boot:
--   SELECT conname FROM pg_constraint WHERE conrelid='runs'::regclass AND contype='c';
--   -> runs_status_check
ALTER TABLE runs DROP CONSTRAINT runs_status_check;
ALTER TABLE runs ADD CONSTRAINT runs_status_check
    CHECK (status IN ('queued', 'claimed', 'running', 'awaiting_approval',
                      'limit_wait', 'completed', 'failed', 'cancelled'));

-- The promotion pass (M2) sweeps `status = 'limit_wait' AND retry_not_before <= now()`
-- on every tick. Partial on the status so the index holds only parked runs — a set
-- that is empty on a healthy instance — rather than one entry per run ever created.
-- Cheap to add now and awkward to retrofit once the table is large.
CREATE INDEX idx_runs_limit_wait_retry
    ON runs (retry_not_before)
    WHERE status = 'limit_wait';

-- +goose Down

-- The status CHECK narrows on the way down, so this Down FAILS LOUDLY if any run is
-- currently parked. That is correct and is not worth "fixing" with a pre-emptive
-- UPDATE: silently rewriting a user's parked run to some other status on a rollback
-- would lose work that the up-migration promised to resume. Cancel or let the parked
-- runs drain first.
DROP INDEX idx_runs_limit_wait_retry;

ALTER TABLE runs DROP CONSTRAINT runs_status_check;
ALTER TABLE runs ADD CONSTRAINT runs_status_check
    CHECK (status IN ('queued', 'claimed', 'running', 'awaiting_approval',
                      'completed', 'failed', 'cancelled'));

ALTER TABLE users DROP COLUMN wait_on_limit;

ALTER TABLE runs
    DROP COLUMN rate_limit_type,
    DROP COLUMN limit_wait_count,
    DROP COLUMN retry_not_before,
    DROP COLUMN limit_resets_at,
    DROP COLUMN wait_on_limit;
