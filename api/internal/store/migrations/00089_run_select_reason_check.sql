-- +goose Up

-- Close runs.anthropic_select_reason to its eight legal values (PRD #111 M4).
--
-- 00086 added the column with NO check and said why: the vocabulary was not closed
-- at M1, because M4 adds five values inside this same PRD, so a CHECK written then
-- would have been rewritten before it ever guarded anything — exactly the churn
-- 00082 had to pay on runs.stop_kind. M4 is where the set stops moving, so this is
-- the migration 00086's comment pointed at.
--
-- The eight, and which layer produces each:
--   default      workersvc — no binding named a credential; the owner's default paid
--   pinned       workersvc — the claiming worker's binding (workers.anthropic_secret_id)
--   judge        workersvc — the owner's JUDGE binding, for judge + self_improve runs.
--                Its own value rather than `pinned` because D20 makes the run view
--                name the MODE, and "pinned" would send a user looking for a worker
--                binding that does not exist
--   auto         autoselect — picked from the eligible set
--   best_of_pool autoselect — D10: every measurable pooled token was below
--                MIN_HEADROOM, so the emptiest of THEM was picked anyway
--   pool_empty   autoselect — no token is opted in; auto fell back to the worker's
--                non-auto binding
--   pool_stale   autoselect — tokens are pooled but none is measurable (never
--                polled, a NULL window, or aged out — which is also what a DISABLED
--                poller produces for every token, R2)
--   open_failed  workersvc — D14: the pick would not decrypt, so the claim retried
--                once on the owner default. Distinct from the others because the
--                credential recorded is NOT the one the selector chose
--
-- NULL stays legal and is not a ninth value. Every run that predates 00086 has one,
-- and so does any run that never reached a claim assemble point; a CHECK that
-- rejected NULL would fail on the existing table at migration time.
--
-- WHY A CHECK AT ALL, given the column is display-only. Precisely because it is
-- display-only: nothing in the state machine, the claim path or any sweep reads it,
-- so a typo'd reason has no failing consumer. It would reach the run view, render as
-- itself, and mean nothing to the user and nothing to a support query — the class of
-- bug that survives every test because nothing depends on the value being right. The
-- database is the only reader positioned to notice.
--
-- Go's half of this contract is autoselect.AllReasons() plus workersvc's
-- staticSelectReasons(), and TestSelectReasonVocabularyMatchesCheck PARSES THE LIST
-- BELOW out of this file and compares. So the two do not merely happen to agree
-- today: adding a value on either side without the other goes red at `go test`,
-- which is where a developer is, rather than at claim time, where a user is.
ALTER TABLE runs
    ADD CONSTRAINT runs_anthropic_select_reason_check
        CHECK (anthropic_select_reason IS NULL OR anthropic_select_reason IN (
            'default', 'pinned', 'judge',
            'auto', 'best_of_pool', 'pool_empty', 'pool_stale', 'open_failed'
        ));

-- +goose Down
ALTER TABLE runs DROP CONSTRAINT runs_anthropic_select_reason_check;
