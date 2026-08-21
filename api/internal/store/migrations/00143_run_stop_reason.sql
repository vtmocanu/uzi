-- +goose Up

-- PRD #503 M3 (REC B): a nullable column for the reason an operator gave when
-- cancelling a run. A cancel is not a failure, so this is a dedicated column rather
-- than an overload of failure_reason (which stays the reject/failure-class field, M2).
-- No CHECK — it is free operator text. Persisted on both cancel paths: the server-side
-- CancelRunServerSide and the live path's CreateStopVerdictInput stamp.
ALTER TABLE runs ADD COLUMN stop_reason text;

-- +goose Down
ALTER TABLE runs DROP COLUMN stop_reason;
