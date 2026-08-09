-- +goose Up

-- The credential a run's claim must exclude on its NEXT selection, because the run
-- just parked on it for a usage limit (PRD #217 M2, D2).
--
-- WHY A NEW COLUMN AND NOT runs.limit_wait_count. The per-run signal the claim needs
-- is "the credential the last park spent", and it must be a value the FIRST successful
-- claim clears — otherwise the exclusion outlives its one claim and a single-token
-- user's run, which has no alternative, is parked forever. limit_wait_count is the
-- wrong shape: it is sticky (SetRunLimitWait only ever increments it, and
-- PromoteLimitWaitRuns deliberately never clears it, runtime.sql), so it can say "this
-- run has parked" but never "clear the exclusion now".
--
-- 🔴 THE LIFETIME, STATED HONESTLY BECAUSE THE COMMENT IS WHERE THE NEXT READER LOOKS.
-- SetRunLimitWait SETS this to the credential the run was spending (runs.anthropic_secret_id
-- at park time). SetRunAnthropicSecret CLEARS it to NULL — unconditionally, on the
-- first claim that successfully RECORDS a credential. So it is cleared on the first
-- claim that gets as far as recording, which means it SURVIVES a claim that dies before
-- recording (at GetRunClaimContext, at box.Open, or inside openAnthropic): "at least
-- one claim", not "exactly one". That is the correct bound — a claim that never opened
-- a credential never spent the dead one, so the exclusion must still stand for the
-- retry.
--
-- The composite FK is 00086's shape (runs_anthropic_secret_fk) and its comment applies
-- verbatim: referencing (user_id, id) — legal because 00077 added UNIQUE (user_id, id)
-- as a legal FK target — makes the DATABASE reject a run naming another user's
-- credential rather than leaving that to a Go check.
--
-- 🔴 THE COLUMN LIST ON SET NULL IS LOAD-BEARING, exactly as 00086:48-55 records. A
-- bare ON DELETE SET NULL on a COMPOSITE FK nulls EVERY referencing column, which here
-- includes runs.user_id (NOT NULL) — so deleting a token any run had recorded here
-- would fail the NOT NULL constraint and abort the DELETE, silently until the first
-- such delete. SET NULL (limit_dead_secret_id) (Postgres 15+) nulls only this record.
--
-- Deliberately NO CHECK requiring this non-null for any kind. A SET-NULL FK and a
-- NOT-NULL CHECK are jointly unsatisfiable: the cascade would null the column and then
-- the CHECK would abort the very DELETE the SET NULL exists to allow (repo memory).
-- The column is nullable and a NULL means "no exclusion", which is the common case and
-- the state every pre-feature run is in.
ALTER TABLE runs
    ADD COLUMN limit_dead_secret_id UUID,
    ADD CONSTRAINT runs_limit_dead_secret_fk
        FOREIGN KEY (user_id, limit_dead_secret_id)
        REFERENCES user_secrets (user_id, id) ON DELETE SET NULL (limit_dead_secret_id);

-- Partial, like 00086's idx_runs_anthropic_secret: Postgres creates no index for the
-- referencing side of a foreign key, so without this every DELETE of a user_secrets
-- row would seq-scan `runs` to find referencing rows. The column is NULL for all but a
-- currently-parked run, so a partial index is a handful of rows rather than the whole
-- table.
CREATE INDEX idx_runs_limit_dead_secret ON runs (limit_dead_secret_id)
    WHERE limit_dead_secret_id IS NOT NULL;

-- +goose Down
DROP INDEX idx_runs_limit_dead_secret;
ALTER TABLE runs
    DROP CONSTRAINT runs_limit_dead_secret_fk,
    DROP COLUMN limit_dead_secret_id;
