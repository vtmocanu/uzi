-- +goose Up
-- PRD #634 M1: run scope steering. An operator can bound an in-flight milestone run
-- with a "scope ceiling" — the count of milestones the run is permitted to complete
-- over the immutable milestones_frozen list — and the worker settles each steering
-- input's outcome as a disposition. All four changes ride ONE migration because they
-- are one feature and must land atomically: the column that bounds the run, the column
-- that records how an input was acted on, and the two kinds (run_user_inputs.kind='scope',
-- stop_kind='scope_capped') the feature writes.

-- runs.scope_ceiling: the ceiling. NULLABLE with NO DEFAULT (NULL = unbounded), which is
-- the common case, so every existing row is byte-unchanged (no rewrite, no NOT NULL).
-- It is the count of milestones the run is permitted to complete over the immutable
-- milestones_frozen list; NULL = unbounded.
ALTER TABLE runs ADD COLUMN scope_ceiling INT;

-- run_user_inputs.disposition: how the worker settled a delivered steering input. NULLABLE
-- with NO DEFAULT — NULL until the worker settles it, so existing rows are byte-unchanged.
-- consumed_at stays the DELIVERY marker; disposition is the ACTED-ON marker. The CHECK is
-- written `disposition IS NULL OR disposition IN (...)` so existing NULL rows pass.
ALTER TABLE run_user_inputs ADD COLUMN disposition TEXT
    CONSTRAINT run_user_inputs_disposition_check
    CHECK (disposition IS NULL OR disposition IN ('applied', 'declined', 'superseded', 'advisory'));

-- run_user_inputs.kind: an EIGHTH steering-input kind, 'scope' — the audit row a
-- 'uzi run scope' writes. The seven existing values are carried VERBATIM from the LIVE
-- constraint (last widened in 00146, which added 'stop'); 'stop' is ALREADY present and is
-- NOT re-added. Re-deriving the list from anything but the live constraint silently deletes
-- whatever it forgets (00092 documents exactly that failure).
ALTER TABLE run_user_inputs DROP CONSTRAINT run_user_inputs_kind_check;
ALTER TABLE run_user_inputs ADD CONSTRAINT run_user_inputs_kind_check
    CHECK (kind IN ('follow_up', 'approve_plan', 'reject_plan', 'cancel', 'revise_plan', 'answer', 'stop', 'scope'));

-- runs.stop_kind: a FIFTH value, 'scope_capped' — the finalize disposition for a
-- scope-truncated COMPLETED run, distinct from #517's 'stopped' (a graceful task-stop) so
-- the two are not conflated. The four existing values are carried verbatim from the LIVE
-- constraint (last widened in 00146). NOTE: scope_capped is a 'completed'-status disposition,
-- NOT a failed transition, so it is deliberately NOT added to the runs_fail_origin_check
-- domain — 00126/00137/00139 enumerate stop_kind literals (plan_rejected, auto_stopped) into
-- fail_origin only for FAILED transitions, and a scope-capped run is a success.
ALTER TABLE runs DROP CONSTRAINT runs_stop_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_stop_kind_check
    CHECK (stop_kind IN ('cancelled', 'plan_rejected', 'auto_stopped', 'stopped', 'scope_capped'));

-- +goose Down
-- Each narrowing is best-effort and DATA-DEPENDENT, mirroring 00146's honesty: a re-added
-- narrower CHECK FAILS if any row already holds the new value (a kind='scope' input, or a
-- stop_kind='scope_capped' run), and this migration then refuses to come down. That is the
-- correct outcome — a down that silently stranded rows violating the constraint it just
-- installed would be worse. Goose downs are not run in this deployment (store.Migrate only
-- ever goes up); drain first if you must.
ALTER TABLE run_user_inputs DROP CONSTRAINT run_user_inputs_disposition_check;
ALTER TABLE run_user_inputs DROP COLUMN disposition;

ALTER TABLE run_user_inputs DROP CONSTRAINT run_user_inputs_kind_check;
ALTER TABLE run_user_inputs ADD CONSTRAINT run_user_inputs_kind_check
    CHECK (kind IN ('follow_up', 'approve_plan', 'reject_plan', 'cancel', 'revise_plan', 'answer', 'stop'));

ALTER TABLE runs DROP CONSTRAINT runs_stop_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_stop_kind_check
    CHECK (stop_kind IN ('cancelled', 'plan_rejected', 'auto_stopped', 'stopped'));

ALTER TABLE runs DROP COLUMN scope_ceiling;
