-- +goose Up
-- Validates the one-claim Fresh shape check that migration 418 added NOT VALID (issue #3607 Mf).
ALTER TABLE runs VALIDATE CONSTRAINT runs_policy_fresh_generation_shape;

-- +goose Down
SET LOCAL lock_timeout = '30s';
ALTER TABLE runs DROP CONSTRAINT runs_policy_fresh_generation_shape;
ALTER TABLE runs ADD CONSTRAINT runs_policy_fresh_generation_shape CHECK (
    (policy_fresh_consumed_generation IS NULL OR (policy_fresh_consumed_generation > 0 AND policy_fresh_consumed_generation <= 9007199254740991))
    AND (policy_fresh_confirmed_generation IS NULL
         OR (policy_fresh_consumed_generation IS NOT NULL AND policy_fresh_confirmed_generation = policy_fresh_consumed_generation))
) NOT VALID;
