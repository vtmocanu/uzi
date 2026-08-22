-- +goose Up

-- PRD #576 M4: async Adopt/Resync/Provision seeding. Item-seeding (dozens of forge
-- calls, ~27s live) now runs in a background goroutine so the write handler returns
-- immediately (fixing the cosmetic 502). While a seed is in flight the reverse poller
-- must NOT run for the repo — a reverse tick against a partially-seeded board could
-- backfill/mis-move issues the seed has not written yet (F-F / R1). This column is the
-- per-repo suppression signal ReverseSync checks.
--
-- It is a LEASE, not a boolean: it records WHEN seeding started, not merely "seeding".
-- A nullable timestamp means a process crash mid-seed cannot suppress reverse sync
-- forever — ReverseSync only honors the suppression while the lease is younger than
-- seedSuppressLease (10m); past that the poller reconciles on its next tick (PRD M4
-- "converges on next tick"). NULL = "not seeding" (the steady state), so existing rows
-- need no backfill and the normal clear path just nulls it.
ALTER TABLE github_project_links ADD COLUMN seeding_started_at timestamptz;

-- +goose Down
ALTER TABLE github_project_links DROP COLUMN seeding_started_at;
