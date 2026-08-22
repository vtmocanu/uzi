-- +goose Up

-- Task-review runs (PRD #400 M4a): a `--review` handoff. A review is NOT a new run
-- kind — it IS a task (repo-ful, issue-less, report-only) distinguished by a non-null
-- review_target_run_id pointing at the task run it reviews. The worker (M4b) routes such
-- a claim to a diff-review executor that clones the reviewed branch, diffs it against
-- base_branch, runs a reviewer agent, and POSTs structured findings back. Modeling the
-- review as a task (not a seventh-plus kind) means it inherits the task shape/claim
-- machinery for free; the ONLY schema widening is two columns on `runs` and two new
-- result tables mirroring run_reviews / review_recommendations.
--
-- review_target_run_id: set ⇒ THIS task run is a review of that target task. ON DELETE
-- CASCADE so deleting the reviewed target removes its in-flight review run too. NULL for
-- every ordinary task/run (never review a review — maybeEnqueueTaskReview gates on it).
--
-- review_requested: set on a PLAIN task at create when the CLI passed --review; consumed
-- at the task's terminal 'completed' transition, where maybeEnqueueTaskReview auto-creates
-- the review run. NOT NULL DEFAULT false so every existing/non-review row reads false.
ALTER TABLE runs ADD COLUMN review_target_run_id uuid REFERENCES runs(id) ON DELETE CASCADE;
ALTER TABLE runs ADD COLUMN review_requested boolean NOT NULL DEFAULT false;

-- At most ONE active review run may exist per reviewed target: the partial unique index
-- makes maybeEnqueueTaskReview's auto-create idempotent (a duplicate raises 23505, read
-- as "already being reviewed"), the same posture the judge's one-active-per-target index
-- (00057/00058) takes. Terminal review runs (completed/failed/cancelled) are excluded so a
-- re-review after a review finishes is allowed.
CREATE UNIQUE INDEX uq_one_active_task_review_per_target
    ON runs (review_target_run_id)
    WHERE review_target_run_id IS NOT NULL AND status NOT IN ('completed', 'failed', 'cancelled');

-- One row per reviewed task (mirrors run_reviews). target_run_id is UNIQUE so a re-review
-- UPSERTs (replace semantics) rather than 23505-ing a second header. review_run_id records
-- WHICH review run produced it (provenance), SET NULL on delete because the header is
-- anchored to the reviewed task, not the review run. These rows are UNTRUSTED DATA: the
-- worker POST (handler) enum-validates status/severity, caps + control-strips the free
-- text, and secret-scrubs it before it lands here; the DB CHECK is the backstop.
CREATE TABLE task_reviews (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    target_run_id uuid NOT NULL UNIQUE REFERENCES runs ON DELETE CASCADE,
    review_run_id uuid REFERENCES runs ON DELETE SET NULL,
    user_id       uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    status        text NOT NULL DEFAULT 'complete'
        CHECK (status IN ('complete', 'failed')),
    summary_md    text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- The per-user "my reviews" scan scopes by owner.
CREATE INDEX idx_task_reviews_user ON task_reviews (user_id);

-- One structured diff-review finding (mirrors review_recommendations, but the PRD #400
-- schema is file + symbol + line + severity + summary + rationale — deliberately WITH a
-- line number, unlike the judge's symbol-only recommendations, because this is a
-- single-diff review, not cross-run dedup where line numbers drift). severity is a closed
-- enum; line is a non-negative int (0 = whole-file / unanchored).
CREATE TABLE task_review_findings (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    review_id    uuid NOT NULL REFERENCES task_reviews ON DELETE CASCADE,
    file         text NOT NULL DEFAULT '',
    symbol       text NOT NULL DEFAULT '',
    line         int NOT NULL DEFAULT 0,
    severity     text NOT NULL DEFAULT 'info'
        CHECK (severity IN ('info', 'warning', 'error')),
    summary_md   text NOT NULL DEFAULT '',
    rationale_md text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- The findings panel lists a review's rows.
CREATE INDEX idx_task_review_findings_review ON task_review_findings (review_id);

-- +goose Down
DROP TABLE task_review_findings;
DROP TABLE task_reviews;
DROP INDEX uq_one_active_task_review_per_target;
ALTER TABLE runs DROP COLUMN review_requested;
ALTER TABLE runs DROP COLUMN review_target_run_id;
