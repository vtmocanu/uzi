-- +goose Up

-- Validate the runs_harness_decision_shape CHECK re-added NOT VALID in 00402 (issue #3845).
-- VALIDATE CONSTRAINT takes only a SHARE UPDATE EXCLUSIVE lock (reads and writes proceed),
-- unlike the ACCESS EXCLUSIVE an inline validated ADD would hold for the whole scan; split from
-- 00402 because goose runs each file in one transaction (the 00224/00225 two-step).
SET LOCAL lock_timeout = '5s';

ALTER TABLE runs VALIDATE CONSTRAINT runs_harness_decision_shape;

-- +goose Down

-- There is no VALIDATE inverse; a validated CHECK stays validated and 00402's Down drops it.
SELECT 1;
