-- +goose Up
-- included_at: the worker reported the follow-up was included in an executor prompt.
-- inclusion_reported: the worker that ACKed the row advertises input_inclusion_v1 (or the inclusion
-- receipt itself set it), so a NULL included_at means "not yet included" rather than "this worker
-- cannot say".
-- No backfill: applied_at means the input was routed by steering, not that an executor prompt
-- carried it, so deriving included_at from it would claim something never observed.
ALTER TABLE run_user_inputs ADD COLUMN included_at TIMESTAMPTZ;
ALTER TABLE run_user_inputs ADD COLUMN inclusion_reported BOOLEAN NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE run_user_inputs DROP COLUMN inclusion_reported;
ALTER TABLE run_user_inputs DROP COLUMN included_at;
