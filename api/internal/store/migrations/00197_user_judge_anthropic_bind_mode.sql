-- +goose Up

-- A user's JUDGE-lane Anthropic BIND MODE (PRD #1140 M2), three-valued exactly like
-- workers.anthropic_bind_mode (00088): 'default' spends the owner's default
-- credential, 'pinned' spends the one judge_anthropic_secret_id (00079) names, and
-- 'auto' lets the SAME selector the run lane uses choose from the owner's opted-in
-- pool at claim time. The column default is 'auto'.
--
-- D5: default 'auto' reaches EXISTING users too, because unlike a worker row the judge
-- pointer is resolved at claim time, so the column default changes what a claim does
-- rather than only what a freshly-created row holds; a user with a pinned judge token
-- is backfilled to 'pinned' below and keeps spending it, unchanged.
-- D4: judge-lane 'auto' on a GENUINELY EMPTY pool spends the owner's default token,
-- recorded 'pool_empty' (the run lane still HOLDS in pool_wait instead) — the one
-- deliberate asymmetry between the two lanes; see the PRD for the full rationale.
--
-- The backfill runs in the SAME migration as the ADD (not in application code) so
-- there is no window in which a bound row carries the DEFAULT 'auto' while holding a
-- pointer — a window in which the judge lane would silently stop honouring the pin.
--
-- No index: the column is read only for a user already fetched by id on the judge
-- claim path, never used as a search predicate.
--
-- No CHECK couples the mode to the pointer, and one is not possible (D6, exactly as
-- 00088 explains for workers): 00079's FK is ON DELETE SET NULL
-- (judge_anthropic_secret_id), so a legal token delete nulls the pointer and leaves
-- the mode, and a coupling CHECK would reject that DELETE. The rule lives in
-- resolution instead: 'pinned' with a NULL pointer resolves as 'default', and the API
-- reports the EFFECTIVE mode (effectiveBindMode, handler/workers.go) so no client
-- re-derives it.
ALTER TABLE users
    ADD COLUMN judge_anthropic_bind_mode TEXT NOT NULL DEFAULT 'auto'
        CHECK (judge_anthropic_bind_mode IN ('default', 'pinned', 'auto'));

UPDATE users SET judge_anthropic_bind_mode = 'pinned' WHERE judge_anthropic_secret_id IS NOT NULL;

-- +goose Down
--
-- LOSSY FOR 'auto', deliberately and unavoidably (same shape as 00088's down): down
-- drops the column and the Up re-derives it from judge_anthropic_secret_id, so
-- 'pinned' and 'default' round-trip exactly, but an 'auto' user holds NO pointer by
-- construction and returns to 'default'. The pointer cannot express 'auto', so nothing
-- here could preserve it.
ALTER TABLE users DROP COLUMN judge_anthropic_bind_mode;
