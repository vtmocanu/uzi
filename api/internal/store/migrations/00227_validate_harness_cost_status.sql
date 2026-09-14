-- +goose Up

-- Validate the seven CHECKs added NOT VALID in 00226 (PRD #1332 M5A / C1). VALIDATE
-- CONSTRAINT scans each table to confirm existing rows satisfy the CHECK but takes only a
-- SHARE UPDATE EXCLUSIVE lock (write-compatible: concurrent reads and writes proceed),
-- unlike the ACCESS EXCLUSIVE lock an inline validated ADD CONSTRAINT ... CHECK would hold.
-- Split from 00226 so the adds are lock-cheap and the validation is non-blocking, per the
-- standard two-step pattern (00219/00220, 00216/00217) for a CHECK on a live table.
--
-- The backlog satisfies every CHECK by construction: 00226 backfilled runs.harness from
-- codex_material_revision (so the coherence CHECK holds for every existing row), the new
-- run_usage/users/run_schedules columns took vocabulary-valid defaults/NULL, and every
-- existing run_usage row is 'metered' (so the non-metered→zero CHECK holds vacuously).
ALTER TABLE runs VALIDATE CONSTRAINT runs_harness_check;
ALTER TABLE run_usage VALIDATE CONSTRAINT run_usage_harness_check;
ALTER TABLE run_usage VALIDATE CONSTRAINT run_usage_cost_status_check;
ALTER TABLE run_usage VALIDATE CONSTRAINT run_usage_nonmetered_zero_check;
ALTER TABLE users VALIDATE CONSTRAINT users_default_harness_check;
ALTER TABLE run_schedules VALIDATE CONSTRAINT run_schedules_harness_check;
ALTER TABLE runs VALIDATE CONSTRAINT runs_codex_harness_coherence_check;

-- +goose Down

-- There is no VALIDATE inverse (a validated CHECK simply stays validated), so restore the
-- pre-00227 state — each constraint present but NOT VALID — by dropping and re-adding it
-- NOT VALID. The CHECK bodies match 00226's Up verbatim. 00226's Down then drops them with
-- the columns. Mirrors 00220's / 00217's Down exactly.
ALTER TABLE runs DROP CONSTRAINT runs_harness_check;
ALTER TABLE runs ADD CONSTRAINT runs_harness_check
    CHECK (harness IN ('claude', 'codex')) NOT VALID;
ALTER TABLE run_usage DROP CONSTRAINT run_usage_harness_check;
ALTER TABLE run_usage ADD CONSTRAINT run_usage_harness_check
    CHECK (harness IN ('claude', 'codex')) NOT VALID;
ALTER TABLE run_usage DROP CONSTRAINT run_usage_cost_status_check;
ALTER TABLE run_usage ADD CONSTRAINT run_usage_cost_status_check
    CHECK (cost_status IN ('metered', 'subscription', 'unreported')) NOT VALID;
ALTER TABLE run_usage DROP CONSTRAINT run_usage_nonmetered_zero_check;
ALTER TABLE run_usage ADD CONSTRAINT run_usage_nonmetered_zero_check
    CHECK (cost_status = 'metered' OR cost_usd = 0) NOT VALID;
ALTER TABLE users DROP CONSTRAINT users_default_harness_check;
ALTER TABLE users ADD CONSTRAINT users_default_harness_check
    CHECK (default_harness IS NULL OR default_harness IN ('claude', 'codex')) NOT VALID;
ALTER TABLE run_schedules DROP CONSTRAINT run_schedules_harness_check;
ALTER TABLE run_schedules ADD CONSTRAINT run_schedules_harness_check
    CHECK (harness IS NULL OR harness IN ('claude', 'codex')) NOT VALID;
ALTER TABLE runs DROP CONSTRAINT runs_codex_harness_coherence_check;
ALTER TABLE runs ADD CONSTRAINT runs_codex_harness_coherence_check
    CHECK (NOT (codex_material_revision IS NOT NULL OR codex_secret_id IS NOT NULL) OR harness = 'codex') NOT VALID;
