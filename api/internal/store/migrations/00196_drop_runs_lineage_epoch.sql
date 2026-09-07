-- +goose Up

-- Issue #1087: the CONTRACT step of the #1079 expand/contract change. #1079 retired the
-- mutable per-run counter in code (BumpRunLineageEpoch and the resume_lineage_break bump
-- loop are gone; the fold derives the per-leg epoch position-absolutely), so
-- runs.lineage_epoch has had no reader or writer since. Its migration (00188) deliberately
-- KEPT the column because check:migration-additive forbids dropping an api-facing column in
-- a forward migration during a rolling release. Now, a release later, the column is dropped.
--
-- Why this is safe here: the api tier is a HARD SINGLETON deployed with strategy: Recreate
-- (deploy/chart/templates/api-deployment.yaml, "never RollingUpdate"; values.yaml
-- replicaCount: 1), so the old api pod is fully terminated before the new one boots and runs
-- this migration -- there is never an N-1/N api replica overlap that would SELECT
-- runs.lineage_epoch by name after the drop. The worker/controller never read the column
-- (it was server-internal). The allow-drop marker below exempts exactly this one column from
-- check:migration-additive's worker-facing-table rule (see that script's header).
--
-- NOTE: this drops runs.lineage_epoch ONLY. run_usage.lineage_epoch is a DIFFERENT, live
-- column -- the per-leg fold key added to the run_usage primary key in 00188 -- and is left
-- untouched.

-- migration-additive:allow-drop runs.lineage_epoch
ALTER TABLE runs DROP COLUMN lineage_epoch;

-- +goose Down
ALTER TABLE runs ADD COLUMN lineage_epoch integer NOT NULL DEFAULT 0;
