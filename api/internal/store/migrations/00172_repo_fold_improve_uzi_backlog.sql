-- +goose Up

-- PRD #686 M1: a per-repo capability flag gating the "fold improve-uzi backlog"
-- behaviour. NOT NULL DEFAULT false so every existing and future repo starts opted
-- out; the toggle is flipped per-repo through the PatchRepo owner/admin path.
ALTER TABLE repos ADD COLUMN fold_improve_uzi_backlog boolean NOT NULL DEFAULT false;

-- Backfill (PRD #686 D3): any repo that already has a self_improve schedule is
-- actively dogfooding uzi's own backlog, so preserve that by opting it in on
-- landing rather than silently resetting it to the false default.
UPDATE repos SET fold_improve_uzi_backlog = true
WHERE id IN (SELECT DISTINCT repo_id FROM run_schedules WHERE target = 'self_improve');

-- +goose Down

ALTER TABLE repos DROP COLUMN fold_improve_uzi_backlog;
