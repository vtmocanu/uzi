-- +goose Up
-- Recent dispatching fires per schedule (issue #2519). A jsonb array holding the last 10
-- scheduled fires that did something (matched > 0), newest first, each entry the same shape
-- as last_fire (the serialized outcome: fired_at/matched/started/skips). Only a fire with
-- matched > 0 is appended: a capacity-blocked tick or an examined-0 tick (matched = 0) never
-- is, and a NULL summary (serialization hiccup) leaves the list untouched. The writers are
-- AdvanceSchedule and RecordScheduleHeldFire, which prepend inside the same UPDATE that
-- writes last_fire, so concurrent writers serialize on the row lock and none is lost.
-- last_fire itself is unchanged and may differ from recent_fires[0] (a later tick that
-- matched nothing replaces last_fire but not the history). Existing rows are backfilled
-- below with their last_fire when it matched > 0, else an empty list; fires handled by old
-- api replicas during a rolling deploy write last_fire only and are not appended.
ALTER TABLE run_schedules ADD COLUMN recent_fires jsonb NOT NULL DEFAULT '[]'::jsonb;

UPDATE run_schedules SET recent_fires = jsonb_build_array(last_fire)
 WHERE last_fire IS NOT NULL AND (last_fire->>'matched')::int > 0;

-- +goose Down
ALTER TABLE run_schedules DROP COLUMN recent_fires;
