-- +goose Up

-- Marks a run-bound throwaway hosted worker the api auto-provisions for a single
-- unplaceable run and later drops (PRD #529 M1). false for every existing and
-- ordinary worker — NOT NULL DEFAULT false so existing rows need no backfill and
-- the whole fleet stays "not ephemeral" until the auto-provisioner (M2) sets it.
ALTER TABLE workers ADD COLUMN ephemeral bool NOT NULL DEFAULT false;

-- The single run this ephemeral worker exists to serve (PRD #529 M1). Nullable —
-- an ordinary worker has none — and REFERENCES runs(id) ON DELETE SET NULL so a
-- future hard-delete of a run can never be blocked by this FK; the SET-NULL just
-- orphans the ephemeral row for the M5 reaper.
ALTER TABLE workers ADD COLUMN ephemeral_run_id uuid REFERENCES runs(id) ON DELETE SET NULL;

-- One ephemeral worker per run, enforced at the schema level so a double-provision
-- is impossible regardless of api replica count (precedent: uq_runs_one_active_per_issue
-- in 00020). Partial on `ephemeral` so it never constrains ordinary workers; Postgres
-- treats NULLs as distinct, so a hard-deleted run's SET-NULL rows never collide.
CREATE UNIQUE INDEX uq_workers_ephemeral_run ON workers (ephemeral_run_id) WHERE ephemeral;

-- +goose Down
DROP INDEX uq_workers_ephemeral_run;
ALTER TABLE workers DROP COLUMN ephemeral_run_id;
ALTER TABLE workers DROP COLUMN ephemeral;
