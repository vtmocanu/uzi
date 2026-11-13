-- +goose Up
SET LOCAL lock_timeout = '5s';

-- PRD #4205 M1: first-wake memory, one row per run that has already woken a
-- worker, so a run wakes a sleeping worker at most once. Split from 00435 so
-- the runs lock needed by the FK is never requested while workers is held
-- ACCESS EXCLUSIVE.
CREATE TABLE hibernation_wakes (
 run_id uuid PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
 worker_id uuid NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
 first_woken_at timestamptz NOT NULL
);
CREATE INDEX hibernation_wakes_worker_idx ON hibernation_wakes (worker_id);

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- Wake rows are transient demand memory (a row lives only while its run is queued), so Down does
-- not refuse on them: refusing would stall a downgrade behind an idle queue. Down of 00435 still
-- refuses while any worker is not awake. The table lock makes a concurrent wake insert wait.
LOCK TABLE hibernation_wakes IN ACCESS EXCLUSIVE MODE;
DROP TABLE hibernation_wakes;
