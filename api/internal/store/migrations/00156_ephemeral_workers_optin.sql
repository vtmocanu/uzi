-- +goose Up

-- users.ephemeral_workers_enabled: the per-user opt-in to ephemeral auto-provisioning
-- (PRD #529 M2, default OFF). It spends real cluster capacity spinning a run-bound
-- hosted worker on demand, so — exactly like judge_enabled (00061) — it is opt-in and
-- default false, needing no backfill on existing rows. The instance kill-switch lives
-- in app_settings (ephemeral_workers_enabled); the auto-provisioner fires for a run
-- only when BOTH the global toggle is on AND the run owner opted in here.
ALTER TABLE users ADD COLUMN ephemeral_workers_enabled bool NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE users DROP COLUMN ephemeral_workers_enabled;
