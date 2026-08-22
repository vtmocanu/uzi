-- +goose Up
-- Issue #552 M1: the park-scoped follow_up watermark for the awaiting_followup → running
-- wake guard. Before this column that guard admitted the transition whenever ANY consumed
-- `follow_up` input existed, so after the first follow-up cycle it was always satisfied and
-- a delayed/duplicate PRE-PARK `running` report could un-park an idle run (SetRunRunning,
-- runtime.sql). open_followup_id records the highest already-CONSUMED follow_up id the run
-- had taken delivery of AT THE MOMENT IT PARKED; the guard then requires a consumed
-- follow_up NEWER than the watermark, which is the follow_up that should wake THIS park.
--
-- run_user_inputs.id is a monotone bigserial PRIMARY KEY (00020), so a plain bigint holds
-- it. Nullable, no default, no backfill: the setter's COALESCE(MAX(id),0) floor stamps 0
-- (never NULL) on EVERY park, including a first park with nothing consumed. The column is
-- genuinely NULL only for a run that has never parked at all (incl. rows parked before this
-- migration, since there is no backfill); the guard reads that NULL as 0 via COALESCE.
ALTER TABLE runs ADD COLUMN open_followup_id bigint;

-- +goose Down
ALTER TABLE runs DROP COLUMN open_followup_id;
