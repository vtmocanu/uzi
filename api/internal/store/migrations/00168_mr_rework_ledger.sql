-- +goose Up

-- PRD #700 M3 (MR review watcher loop guard). The per-(repo, ref) ledger the poller
-- detector (poller/mr_review_watch.go) reads/writes to bound how many AUTOMATIC
-- mr_rework cycles a watched MR may spend, remember which review comments were already
-- consumed, and latch the comment-once halt notice. It mirrors ci_autofix_attempts
-- (00117): INDEPENDENT of the runs table (an mr_rework run is terminal-and-evicted long
-- before the next review comment on the same ref), so a durable "cycles spent on this
-- MR" guarantee lives here, not on the runs rows. Reconcile eviction
-- (DeleteMRReworkLedgerNotIn) keeps it from outliving the ref it guards — a merged or
-- closed MR drops out of the opened-only candidate set and its row is evicted — so a
-- reused agent/issue-N branch never inherits a stale count.
--
-- ref is the agent/issue-N branch (the same key ci_autofix_attempts uses).
--
-- high_water is the MONOTONIC comment id (MRComment.ID) of the newest review comment
-- already consumed; the detector fires only on a kept comment with id > high_water and,
-- on proceed, advances high_water to max(id) over the current kept comments
-- (advance-only, GREATEST in the upsert). It is a SCALAR mark.
--
-- 🔴 KNOWN LIMITATION (documented decision, NOT an oversight — mirrored verbatim in the
-- detector). GitHub/Forgejo source review-comment ids from DISTINCT sequences
-- (issue-comment / inline-review / review-summary), so a single scalar high-water can
-- SKIP a genuinely-new comment whose id is numerically BELOW a previously-consumed
-- comment from another sequence. This is FAIL-SAFE: a skipped comment falls back to
-- human review (the pre-feature baseline) and never causes a wrong write; re-processing
-- is bounded by the per-MR cap. A PER-SEQUENCE high-water is the robust follow-up; the
-- scalar mark is what PRD #700 Decision 2 + SC3 specify.
CREATE TABLE mr_rework_ledger (
    repo_id       uuid   NOT NULL REFERENCES repos ON DELETE CASCADE,
    ref           text   NOT NULL,
    attempt_count int    NOT NULL DEFAULT 0,   -- AUTO rework cycles spent (cap default 5)
    high_water    bigint NOT NULL DEFAULT 0,   -- max review-comment id consumed (advance-only)
    halt_notified boolean NOT NULL DEFAULT false, -- comment-once latch after the cap halt
    updated_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (repo_id, ref)
);

-- +goose Down
DROP TABLE mr_rework_ledger;
