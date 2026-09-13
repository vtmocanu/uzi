-- +goose Up

-- PRD #1332 (parent #1106 M5A / C1): the additive "dark Codex routing foundation"
-- schema. Five new columns plus the CHECKs that close their vocabularies, added in the
-- lock-cheap two-step (ADD COLUMN + ADD CONSTRAINT ... NOT VALID here, VALIDATE
-- CONSTRAINT in the sibling 00225) that 00219/00220 and 00216/00217 established for a
-- CHECK on a populated table: an inline validated CHECK would have PostgreSQL scan every
-- row under the ACCESS EXCLUSIVE lock the ALTER already holds, whereas VALIDATE CONSTRAINT
-- later takes only a write-compatible SHARE UPDATE EXCLUSIVE scan.
--
-- Every column is ADDITIVE and safe for an N-1 worker during a rolling release (the
-- check-migration-additive discipline): a worker that never reads `harness`/`cost_status`
-- keeps working, and the NOT NULL columns carry a DEFAULT so the ALTER rewrites no row.
-- M5A ships DARK: no public origin can select Codex, and the two nullable preference
-- columns (users.default_harness, run_schedules.harness) get no public writer in this
-- child — they exist for the D11 resolver (built in C3) and the M5B schedule writer.

-- runs.harness / run_usage.harness: which agent harness a run (and its usage rows) ran
-- under. NOT NULL DEFAULT 'claude' so every existing row and every current production
-- origin is Claude; the twelve production INSERT INTO runs now list 'claude' explicitly so
-- the default never masks an omitted origin.
ALTER TABLE runs ADD COLUMN harness text NOT NULL DEFAULT 'claude';
ALTER TABLE run_usage ADD COLUMN harness text NOT NULL DEFAULT 'claude';

-- run_usage.cost_status: the closed cost-observability marker (D5). 'metered' rows carry a
-- real provider dollar cost; 'subscription' and 'unreported' rows pin cost_usd to the
-- numeric placeholder 0 (the *_nonmetered_zero_check below), so a reader keys on the status
-- and never presents a placeholder 0 as a real dollar total. Existing rows backfill to
-- 'metered', preserving Claude's provider-reported accounting.
ALTER TABLE run_usage ADD COLUMN cost_status text NOT NULL DEFAULT 'metered';

-- users.default_harness / run_schedules.harness: the two preference columns. NULLABLE with
-- no default (NULL = "no preference / inherit"), and no public writer in M5A.
ALTER TABLE users ADD COLUMN default_harness text;
ALTER TABLE run_schedules ADD COLUMN harness text;

-- Backfill any pre-existing internal Codex-bound run to harness='codex' using M1's
-- deletion-proof sentinel codex_material_revision (00202): it is written non-null at the
-- first FreezeRunCodexBinding and is NEVER nulled by the runs_codex_secret_fk ON DELETE
-- SET NULL (codex_secret_id) cascade (00202:71-73), which nulls only the alias id. Keying
-- on codex_secret_id would MISS a row whose alias was deleted; a Codex row legitimately
-- retains harness='codex' after its alias id becomes NULL (D2). No production path creates
-- a Codex-bound run yet (M5A is dark), so in practice only test-server rows are affected.
UPDATE runs SET harness = 'codex' WHERE codex_material_revision IS NOT NULL;

-- Every new CHECK is added NOT VALID (enforced for new/updated rows immediately; the
-- backlog scan is deferred to 00225's VALIDATE CONSTRAINT).
ALTER TABLE runs ADD CONSTRAINT runs_harness_check
    CHECK (harness IN ('claude', 'codex')) NOT VALID;
ALTER TABLE run_usage ADD CONSTRAINT run_usage_harness_check
    CHECK (harness IN ('claude', 'codex')) NOT VALID;
ALTER TABLE run_usage ADD CONSTRAINT run_usage_cost_status_check
    CHECK (cost_status IN ('metered', 'subscription', 'unreported')) NOT VALID;
-- Non-metered rows are pinned to the numeric placeholder cost_usd=0: a 'subscription' or
-- 'unreported' row can never carry a positive dollar amount, so SUM(cost_usd) over a run's
-- rows already equals the metered-only dollar total and UpsertRunUsage's conflict rule
-- cannot combine an unreported status with a positive number.
ALTER TABLE run_usage ADD CONSTRAINT run_usage_nonmetered_zero_check
    CHECK (cost_status = 'metered' OR cost_usd = 0) NOT VALID;
ALTER TABLE users ADD CONSTRAINT users_default_harness_check
    CHECK (default_harness IS NULL OR default_harness IN ('claude', 'codex')) NOT VALID;
ALTER TABLE run_schedules ADD CONSTRAINT run_schedules_harness_check
    CHECK (harness IS NULL OR harness IN ('claude', 'codex')) NOT VALID;
-- Binding-coherence (D2/D3): a row indicated Codex by EITHER M1 sentinel
-- (codex_material_revision, the deletion-proof one, OR codex_secret_id, the alias id the FK
-- can null) MUST carry harness='codex'. This is what lets claim assembly trust runs.harness
-- and fail closed instead of silently assembling a Claude claim for a Codex row whose alias
-- id was nulled by deletion.
ALTER TABLE runs ADD CONSTRAINT runs_codex_harness_coherence_check
    CHECK (NOT (codex_material_revision IS NOT NULL OR codex_secret_id IS NOT NULL) OR harness = 'codex') NOT VALID;

-- +goose Down

-- Drop the constraints, then the columns (the add migration's Down convention). Dropping a
-- column would cascade-drop the CHECKs that reference it, but they are dropped explicitly
-- first so the intent is legible and the order matches the two-step exemplars. Goose downs
-- are not run in this deployment (store.Migrate only ever goes up); this is the true
-- inverse of the additive Up above.
ALTER TABLE runs DROP CONSTRAINT runs_codex_harness_coherence_check;
ALTER TABLE runs DROP CONSTRAINT runs_harness_check;
ALTER TABLE run_usage DROP CONSTRAINT run_usage_nonmetered_zero_check;
ALTER TABLE run_usage DROP CONSTRAINT run_usage_cost_status_check;
ALTER TABLE run_usage DROP CONSTRAINT run_usage_harness_check;
ALTER TABLE users DROP CONSTRAINT users_default_harness_check;
ALTER TABLE run_schedules DROP CONSTRAINT run_schedules_harness_check;

ALTER TABLE run_schedules DROP COLUMN IF EXISTS harness;
ALTER TABLE users DROP COLUMN IF EXISTS default_harness;
ALTER TABLE run_usage DROP COLUMN IF EXISTS cost_status;
ALTER TABLE run_usage DROP COLUMN IF EXISTS harness;
ALTER TABLE runs DROP COLUMN IF EXISTS harness;
