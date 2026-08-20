-- Canary fixture for scripts/check-migration-additive.sh (PRD #422 M6).
--
-- 🔴 NOT A REAL MIGRATION. It lives under scripts/, NEVER under
-- api/internal/store/migrations/, so goose never applies it. Its only job is to prove
-- the additive-migration guard's detector is live: the guard scans this file and must
-- report EXACTLY ONE finding — the Up-section destructive statement below. The identical
-- statement in the Down section MUST NOT be reported, because Down sections are
-- legitimate rollbacks the guard deliberately skips. Zero findings means the detector is
-- blind; two means it wrongly read the Down section. Either way the self-check exits 2.

-- +goose Up
-- A planted DESTRUCTIVE change to a worker-facing table (workers). The guard MUST catch
-- this: an N-1 worker still reads workers.version during a rolling release, so dropping
-- the column mid-release breaks it. This is the one finding the self-check expects.
ALTER TABLE workers DROP COLUMN version;

-- +goose Down
-- A destructive statement in the Down section, which the guard MUST IGNORE. In a real
-- additive migration this would be the rollback of the Up's ADD COLUMN; here it mirrors
-- the Up statement so that a scanner which wrongly read Down sections would double-count
-- and trip the "exactly one" self-check.
ALTER TABLE workers DROP COLUMN version;
