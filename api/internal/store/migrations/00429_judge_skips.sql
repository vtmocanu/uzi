-- +goose Up

-- Issue #3257: judge_skips records why the automatic judge was NOT created for a finished
-- run because the judge's harness had no usable credential, so the run page and the CLI can
-- say so and offer a retry. One row per target run (the primary key); a later refusal
-- overwrites it, and creating a judge for the target deletes it in the same transaction.
-- Additive only: an N-1 api never reads or writes the table.
--
-- The migration number is a draft: it is renumbered above the live head at landing.
SET LOCAL lock_timeout = '5s';

CREATE TABLE judge_skips (
    target_run_id uuid PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
    harness       text NOT NULL CHECK (harness IN ('claude', 'codex')),
    reason        text NOT NULL CHECK (reason IN ('no_usable_credential', 'credential_disabled', 'codex_pool_unavailable')),
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- +goose Down

-- LOSSY: this Down permanently erases every recorded judge skip. goose downs are not run
-- in this deployment (store.Migrate only goes up).
SET LOCAL lock_timeout = '5s';

DROP TABLE IF EXISTS judge_skips;
