-- +goose Up

-- The judge's structured output (PRD #46 Decision 5). A judge run reviews one
-- finished run and writes exactly one run_reviews row plus zero or more
-- review_recommendations. These rows are UNTRUSTED DATA: the worker is a
-- user-controlled container, so the review POST (M3) validates the enums, caps +
-- strips control chars on the free text, and scrubs it through the secret-family
-- redactor before it lands here. Nothing downstream treats a recommendation as an
-- instruction (Decision 5 + the self-improve untrusted-framing in Decision 10).

-- One review per reviewed run. target_run_id is UNIQUE so a re-judge UPSERTs
-- (replace semantics, Decision 8) rather than 23505-ing against a second row, and
-- ON DELETE CASCADE so deleting the reviewed run removes its review (Decision 8).
-- judge_run_id records WHICH judge run produced it (provenance / trace link); it is
-- SET NULL on delete because the review is anchored to the reviewed run, not the
-- judge run. user_id is the shared owner of both runs (a judge is never cross-user,
-- Decision 2). status distinguishes a real LLM verdict from the deterministic
-- fallback written when the model call fails (Decision 4) — recommendations still
-- land in the latter case.
CREATE TABLE run_reviews (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    target_run_id uuid NOT NULL UNIQUE REFERENCES runs ON DELETE CASCADE,
    judge_run_id  uuid REFERENCES runs ON DELETE SET NULL,
    user_id       uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    verdict       text NOT NULL CHECK (verdict IN ('ideal', 'ok', 'issues')),
    summary_md    text NOT NULL DEFAULT '',
    judge_model   text NOT NULL DEFAULT '',
    status        text NOT NULL DEFAULT 'complete'
        CHECK (status IN ('complete', 'failed')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- The admin all-reviews and the per-user "my reviews" listings scope by owner.
CREATE INDEX idx_run_reviews_user ON run_reviews (user_id);

-- One structured recommendation. category is the user's verbatim taxonomy
-- (specs/human.md); target names the agent/tool/repo the recommendation is about.
-- Provenance (Decision 5) — produced_by_run_id/user — is carried independently of
-- review_id so a recommendation stays attributable to its source even if surfaced
-- apart from its review. addressed_by_run_id is stamped when a self_improve run
-- folds an improve_uzi recommendation into an MR (Decision 11); NULL until then and
-- only ever set on improve_uzi rows.
CREATE TABLE review_recommendations (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    review_id           uuid NOT NULL REFERENCES run_reviews ON DELETE CASCADE,
    category            text NOT NULL CHECK (category IN (
                            'enable_tool', 'install_worker_tool', 'adjust_template',
                            'improve_agent', 'add_agent', 'improve_uzi')),
    target              text NOT NULL DEFAULT '',
    rationale_md        text NOT NULL DEFAULT '',
    confidence          text NOT NULL DEFAULT ''
        CHECK (confidence IN ('', 'low', 'medium', 'high')),
    produced_by_run_id  uuid REFERENCES runs ON DELETE SET NULL,
    produced_by_user_id uuid REFERENCES users ON DELETE SET NULL,
    addressed_by_run_id uuid REFERENCES runs ON DELETE SET NULL,
    created_at          timestamptz NOT NULL DEFAULT now()
);

-- The recommendations panel lists a review's rows.
CREATE INDEX idx_review_recommendations_review ON review_recommendations (review_id);

-- The self-improvement engine (M5) consumes the unaddressed improve_uzi backlog;
-- a partial index keeps that scan cheap and documents the exact working set.
CREATE INDEX idx_review_recommendations_improve_uzi_open
    ON review_recommendations (created_at)
    WHERE category = 'improve_uzi' AND addressed_by_run_id IS NULL;

-- +goose Down
DROP TABLE review_recommendations;
DROP TABLE run_reviews;
