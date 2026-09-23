-- +goose Up

-- Validate the two CHECKs added NOT VALID in 00244 (issue #1562): run_usage_usage_basis_check
-- (usage_basis IN ('per_leg','session_cumulative')) and run_usage_lineage_index_check
-- (lineage_index >= 0). VALIDATE CONSTRAINT scans run_usage to confirm existing rows satisfy
-- each CHECK but takes only a SHARE UPDATE EXCLUSIVE lock (write-compatible: concurrent reads
-- and writes proceed), unlike the ACCESS EXCLUSIVE lock an inline validated ADD CONSTRAINT
-- would hold. Split from 00244 so the adds are lock-cheap and the validation non-blocking, per
-- the two-step pattern (00226/00227, 00242/00243) for a CHECK on a live table.
--
-- The backlog satisfies both CHECKs by construction: usage_basis was added NOT NULL DEFAULT
-- 'per_leg' (an allowed value) and lineage_index NOT NULL DEFAULT 0 (>= 0), so every existing
-- row already conforms.
ALTER TABLE run_usage VALIDATE CONSTRAINT run_usage_usage_basis_check;
ALTER TABLE run_usage VALIDATE CONSTRAINT run_usage_lineage_index_check;

-- +goose Down

-- There is no VALIDATE inverse (a validated CHECK simply stays validated), so restore the
-- pre-00245 state — each constraint present but NOT VALID — by dropping and re-adding it
-- NOT VALID. The CHECK bodies match 00244's Up verbatim; 00244's Down then drops the columns
-- (which drops these constraints with them). Mirrors 00243's Down exactly.
ALTER TABLE run_usage DROP CONSTRAINT run_usage_usage_basis_check;
ALTER TABLE run_usage ADD CONSTRAINT run_usage_usage_basis_check
    CHECK (usage_basis IN ('per_leg', 'session_cumulative')) NOT VALID;
ALTER TABLE run_usage DROP CONSTRAINT run_usage_lineage_index_check;
ALTER TABLE run_usage ADD CONSTRAINT run_usage_lineage_index_check
    CHECK (lineage_index >= 0) NOT VALID;
