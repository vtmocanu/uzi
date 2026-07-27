-- +goose Up

-- A worker's Anthropic BIND MODE (PRD #111 M3, D1). Three values, and the third is
-- the feature: 'default' spends the owner's default credential, 'pinned' spends the
-- one named by anthropic_secret_id (00078), and 'auto' lets the selector choose from
-- the owner's opted-in pool (00087) at claim time.
--
-- It composes with the existing binding rather than replacing it, which is what lets
-- a user auto-balance most workers while keeping the retrospective one pinned. A
-- pinned worker always wins over any heuristic, because the mode is read first.
--
-- The backfill is what makes this a no-op for every existing worker: a worker with a
-- binding becomes 'pinned', everything else 'default'. Both are exactly what
-- workerSecretID already does today (a NULL binding resolves the owner's default),
-- so no worker changes behaviour on the deploy. It runs in the SAME migration as the
-- ADD rather than in application code, so there is no window in which live rows
-- carry the DEFAULT 'default' while holding a binding — a window in which the run
-- lane would silently stop honouring pins.
--
-- 🔴 NO CHECK COUPLES THE MODE TO THE ID, AND ONE IS NOT POSSIBLE. The obvious
-- constraint — "mode = 'pinned' implies anthropic_secret_id IS NOT NULL" — would
-- make a LEGAL token delete fail: 00078's FK is ON DELETE SET NULL, so deleting a
-- credential nulls the binding of every worker pinned to it while leaving the mode
-- alone, and a coupling CHECK would reject that DELETE. The database cannot express
-- the invariant without breaking the cascade that PRD #104 D5 requires.
--
-- So the rule lives in resolution instead: **'pinned' with a NULL id resolves as
-- 'default'** (D9). That is not a new rule and not a compromise — it is precisely
-- what workerSecretID does today for an unset binding, kept true under the new
-- column. The id is read ONLY in 'pinned' mode; 'default' and 'auto' never consult
-- it, so a stale id left behind by a mode change cannot leak into a claim.
--
-- The UI must render such a worker HONESTLY as using the default rather than showing
-- a pin to a token that no longer exists. The API answers that by reporting the
-- EFFECTIVE mode, so no client has to re-derive the rule (see effectiveBindMode in
-- handler/workers.go).
ALTER TABLE workers
    ADD COLUMN anthropic_bind_mode TEXT NOT NULL DEFAULT 'default'
        CHECK (anthropic_bind_mode IN ('default', 'pinned', 'auto'));

UPDATE workers SET anthropic_bind_mode = 'pinned' WHERE anthropic_secret_id IS NOT NULL;

-- No index: the column is read only for a worker already fetched by id on its claim
-- path, never used as a search predicate.

-- +goose Down
ALTER TABLE workers DROP COLUMN anthropic_bind_mode;
