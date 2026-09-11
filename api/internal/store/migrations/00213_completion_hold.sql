-- +goose Up

-- PRD #1226 M4 (D6): the dedicated completion-HOLD transition's additive schema. Three
-- nullable columns land in ONE migration because they are one feature: the two that
-- annotate WHY and AT WHAT HEAD a run is parked on the completion interlock, and the
-- one-shot served-flag column SetRunCompletionHold clears. All are ADDITIVE (ADD COLUMN
-- only) and PLAIN NULLABLE with NO CHECK and NO default, so an N-1 worker/api reading
-- `runs` during a rolling release is byte-unaffected (NULL is the legacy state for every
-- existing row), there is NO CHECK on a live table, and check-migration-additive needs
-- neither the NOT VALID/VALIDATE two-step 00204/00205 used nor a table rewrite.

-- runs.hold_reason is why the run is held (D6). This milestone sets ONLY 'completion_blocked'
-- (SetRunCompletionHold), but the column is kept UNCONSTRAINED text — no CHECK enum — so #1229
-- can add further hold reasons ADDITIVELY without a schema change. NULL until a hold parks it.
ALTER TABLE runs ADD COLUMN hold_reason text;

-- runs.hold_captured_head is the EXACT head the worker captured at hold time (D6), the
-- structural anchor a later resume/steer reasons about. Nullable — a hold may carry no head
-- (SetRunCompletionHold uses pgconv.TextOrNull). NULL until a hold parks it.
ALTER TABLE runs ADD COLUMN hold_captured_head text;

-- runs.completion_budget_exhausted_at is the ONE-SHOT served-flag (D3): a LATER unit in this
-- milestone sets it from the sweeper when a run's completion budget is exhausted, and the
-- worker acting on that served steer CLEARS it (SetRunCompletionHold sets it back to NULL) so
-- a stale ack cannot re-arm the steer. Here it only needs to EXIST so that clear compiles;
-- nothing sets it yet. NULL is the un-served state.
ALTER TABLE runs ADD COLUMN completion_budget_exhausted_at timestamptz;

-- +goose Down

-- Drop the three columns in reverse add order. Down runs only on an explicit `goose down`,
-- never during a forward rolling release (store.Migrate only goes up), so these drops are
-- out of check-migration-additive's Up-only scope.
ALTER TABLE runs DROP COLUMN completion_budget_exhausted_at;
ALTER TABLE runs DROP COLUMN hold_captured_head;
ALTER TABLE runs DROP COLUMN hold_reason;
