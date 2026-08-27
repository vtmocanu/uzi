-- Self-improvement default scheduled job (PRD #590 M1), schema half. This admits a
-- fourth run_schedules target, 'self_improve', so the self-improvement orchestration
-- can be driven by a catalog-enabled schedule (relocated into schedsvc) alongside the
-- bespoke engine that still runs this milestone.
--
-- A 'self_improve' row is promptless and label-less: the whole directive is worker-side,
-- and the tracking issue is resolved at fire time. So its shape arm forbids issue_iid,
-- prompt AND labels. The arm is origin-agnostic (a catalog-enable produces origin='default'
-- while a clone produces origin='user'; both are valid), so it carries no origin clause.
--
-- BOTH constraints are dropped and recreated because a CHECK cannot be altered in place.
-- run_schedules_target_shape's issue/sweep/prompt arms are copied VERBATIM from
-- 00152_run_schedules_default_origin.sql (including the prompt arm's origin='default'
-- relaxation, which MUST be preserved or PRD #589 regresses).

-- +goose Up

ALTER TABLE run_schedules DROP CONSTRAINT run_schedules_target_check;
ALTER TABLE run_schedules ADD CONSTRAINT run_schedules_target_check
    CHECK (target IN ('issue','sweep','prompt','self_improve'));

ALTER TABLE run_schedules DROP CONSTRAINT run_schedules_target_shape;
ALTER TABLE run_schedules ADD CONSTRAINT run_schedules_target_shape CHECK (
    (target = 'issue'  AND issue_iid IS NOT NULL AND prompt IS NULL)
 OR (target = 'sweep'  AND issue_iid IS NULL AND prompt IS NULL)
 OR (target = 'prompt' AND issue_iid IS NULL AND (prompt IS NOT NULL OR origin = 'default'))
 OR (target = 'self_improve' AND issue_iid IS NULL AND prompt IS NULL AND labels IS NULL)
);

-- +goose Down

ALTER TABLE run_schedules DROP CONSTRAINT run_schedules_target_shape;
ALTER TABLE run_schedules ADD CONSTRAINT run_schedules_target_shape CHECK (
    (target = 'issue'  AND issue_iid IS NOT NULL AND prompt IS NULL)
 OR (target = 'sweep'  AND issue_iid IS NULL AND prompt IS NULL)
 OR (target = 'prompt' AND issue_iid IS NULL AND (prompt IS NOT NULL OR origin = 'default'))
);

ALTER TABLE run_schedules DROP CONSTRAINT run_schedules_target_check;
ALTER TABLE run_schedules ADD CONSTRAINT run_schedules_target_check
    CHECK (target IN ('issue','sweep','prompt'));
