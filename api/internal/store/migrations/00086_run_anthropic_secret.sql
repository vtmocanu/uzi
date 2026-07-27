-- +goose Up

-- Which Anthropic credential a run's claim actually spent (PRD #111 M1, D8).
--
-- run_usage (PRD #40, 00062) records what a run SPENT but not which credential
-- spent it. That was tautological while a user could hold one token; since
-- PRD #104 they can hold several, and once PRD #111 M4 picks between them
-- automatically "which account paid for this run?" has no answer in the data at
-- all. These columns are that answer, written at the claim assemble points after
-- a SUCCESSFUL open, so the recorded id is provably the id that was opened.
--
-- Four columns, two of which nothing writes yet (D19). anthropic_select_reason
-- and anthropic_headroom_pct belong to M5's rendering, but M5 has no schema
-- change of its own and "on fallback, why" is not renderable without somewhere to
-- persist it. This migration is already altering `runs`; folding them in costs one
-- ALTER instead of a fourth migration whose number would have to be re-derived at
-- landing. M1 writes reason ('default' | 'pinned' — the mode that named the
-- credential, which is all that is knowable before auto exists) and leaves
-- headroom NULL; M4 adds 'auto', 'best_of_pool', 'pool_empty', 'pool_stale' and
-- 'open_failed' and is the first writer of headroom.
--
-- Deliberately NO CHECK on anthropic_select_reason, unlike runs.stop_kind
-- (00050/00082). The vocabulary is NOT closed at M1 — M4 adds five values inside
-- this same PRD — so a CHECK here would be rewritten before it ever guarded
-- anything, which is exactly the churn 00082 had to pay. The column is
-- display-only: nothing in the state machine, the claim path or any sweep gate
-- reads it. Once M4 has settled the vocabulary a CHECK is worth adding, and that
-- is the right migration to add it in.
--
-- WHY THE LABEL IS SNAPSHOTTED RATHER THAN JOINED. The FK below nulls the id when
-- the token is deleted, and a rename rewrites user_secrets.label in place. A join
-- would therefore make a run's history read "unknown" the moment a user tidies up
-- their credentials, and would silently re-label historical runs on a rename. The
-- snapshot is what keeps "which account was this?" answerable afterwards; the id
-- is what keeps it JOINable while the token still exists. Both, on purpose.
--
-- The composite FK is 00078's shape and its comment applies verbatim here, so it
-- is not repeated in full: referencing (user_id, id) — legal because 00077 added
-- UNIQUE (user_id, id) solely to be a legal FK target — makes the DATABASE reject
-- a run recording another user's credential, rather than leaving that to a Go
-- check one refactor away from being bypassed.
--
-- 🔴 THE COLUMN LIST ON SET NULL IS LOAD-BEARING. A bare `ON DELETE SET NULL` on a
-- COMPOSITE foreign key nulls EVERY referencing column, which here means
-- runs.user_id as well as the binding. runs.user_id is NOT NULL
-- (00020_workers_runs.sql:31), so deleting a token that any run had recorded would
-- fail the NOT NULL constraint and the DELETE would error out — and it would be
-- silent until the first user deleted a token that happened to have run history.
-- `SET NULL (anthropic_secret_id)` (Postgres 15+) nulls only the record and leaves
-- the run's owner alone. 00078 and 00079 hit the same trap.
--
-- Not declared ON UPDATE, for 00078's reason: user_secrets.id and user_id are both
-- immutable in every write path, so there is no update to cascade.
--
-- A NULL record satisfies the FK under the default MATCH SIMPLE, which is exactly
-- what every pre-feature run needs.
ALTER TABLE runs
    ADD COLUMN anthropic_secret_id     UUID,
    ADD COLUMN anthropic_secret_label  TEXT,
    ADD COLUMN anthropic_select_reason TEXT,
    ADD COLUMN anthropic_headroom_pct  SMALLINT,
    ADD CONSTRAINT runs_anthropic_headroom_pct_check
        CHECK (anthropic_headroom_pct IS NULL OR anthropic_headroom_pct BETWEEN 0 AND 100),
    ADD CONSTRAINT runs_anthropic_secret_fk
        FOREIGN KEY (user_id, anthropic_secret_id)
        REFERENCES user_secrets (user_id, id) ON DELETE SET NULL (anthropic_secret_id);

-- Partial, like 00078's. It earns its keep in M1 already: Postgres creates NO
-- index for the referencing side of a foreign key, so without this every DELETE
-- of a user_secrets row would seq-scan `runs` to find referencing rows — on the
-- one table that grows without bound. It also backs M4's in-flight rollup
-- (count of currently-spending runs per credential) and the per-token cost join
-- against run_usage_totals, both of which key on exactly this column.
CREATE INDEX idx_runs_anthropic_secret ON runs (anthropic_secret_id)
    WHERE anthropic_secret_id IS NOT NULL;

-- +goose Down
DROP INDEX idx_runs_anthropic_secret;
ALTER TABLE runs
    DROP CONSTRAINT runs_anthropic_secret_fk,
    DROP CONSTRAINT runs_anthropic_headroom_pct_check,
    DROP COLUMN anthropic_headroom_pct,
    DROP COLUMN anthropic_select_reason,
    DROP COLUMN anthropic_secret_label,
    DROP COLUMN anthropic_secret_id;
