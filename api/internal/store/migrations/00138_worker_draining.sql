-- +goose Up

-- Worker drain/cordon signal (PRD #422 Decision 7). draining_since is an ORTHOGONAL
-- cordon column, NOT a workers.status value. status is heartbeat-derived: RegisterWorker
-- and HeartbeatWorker both write status='online' on every call, so a drain encoded in
-- status would be clobbered back to 'online' by the very next heartbeat and never
-- observed by the claim gate. A draining worker MUST keep heartbeating (so it stays
-- live and its in-flight runs are not requeued as stale) and finish its in-flight runs,
-- but claims nothing new — the controller rolls it once it is idle (M4).
--
-- NULL = not draining. A timestamp = cordoned-at (when the drain was requested). A
-- draining worker keeps status='online' and keeps heartbeating; it finishes its
-- in-flight runs but claims nothing new. Cleared on the worker's next RegisterWorker
-- (after its roll), which re-enables claiming.
ALTER TABLE workers ADD COLUMN draining_since timestamptz;

-- +goose Down
ALTER TABLE workers DROP COLUMN draining_since;
