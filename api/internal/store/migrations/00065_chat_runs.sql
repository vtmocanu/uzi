-- +goose Up

-- Chat runs (PRD #39). A run is now one of THREE kinds: an issue run, a ci_fix
-- run, or a chat run (a conversational session with the in-app uzi agent). A chat
-- run has NO repo, NO issue, NO branch — it rides the run machinery only for token
-- delivery, message persistence/replay, liveness, and vault gating (Decision 1).
-- So repo_id becomes nullable and the kind-shape CHECK inverts for chat: a chat run
-- MUST have repo_id/issue_iid/branch all NULL, while every existing kind keeps its
-- repo_id NOT NULL invariant (Decision 12).
ALTER TABLE runs ALTER COLUMN repo_id DROP NOT NULL;

-- title is the chat conversation's display title, derived from the first message
-- (NULL for issue/ci_fix runs, which display issue_title). resume_of_run_id points
-- a Continue run (Decision 11) at the ended chat run whose SDK session it resumes;
-- ON DELETE SET NULL so removing the prior run never cascades away the continuation.
ALTER TABLE runs
    ADD COLUMN title            text,
    ADD COLUMN resume_of_run_id uuid REFERENCES runs ON DELETE SET NULL;

-- Widen the kind domain. The inline CHECK from 00043 is named runs_kind_check
-- (Postgres's default single-column-check name); drop and re-add with 'chat'.
ALTER TABLE runs DROP CONSTRAINT runs_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_kind_check CHECK (kind IN ('issue', 'ci_fix', 'chat'));

-- Rework the per-kind shape. Existing kinds keep today's rules AND gain an explicit
-- repo_id IS NOT NULL (preserving the pre-migration invariant now that the column is
-- nullable); chat inverts it — repo-less, issue-less, branch-less.
ALTER TABLE runs DROP CONSTRAINT runs_kind_shape;
ALTER TABLE runs ADD CONSTRAINT runs_kind_shape CHECK (
    (kind = 'issue'  AND repo_id IS NOT NULL AND issue_iid IS NOT NULL)
 OR (kind = 'ci_fix' AND repo_id IS NOT NULL AND pipeline_id IS NOT NULL AND pipeline_ref IS NOT NULL)
 OR (kind = 'chat'   AND repo_id IS NULL AND issue_iid IS NULL AND branch IS NULL));

-- Proposed GitLab issues from a chat (Decision 8). The worker's propose_issue tool
-- (M3) writes a pending row; only the browser's confirmed POST executes the forge
-- CreateIssue, stamping created_issue_iid. Never a forge write from this table itself
-- — it is the human-gated queue between the model's suggestion and the real issue.
CREATE TABLE issue_proposals (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id            uuid NOT NULL REFERENCES runs ON DELETE CASCADE,   -- the chat run that proposed it
    repo_id           uuid NOT NULL REFERENCES repos ON DELETE CASCADE,  -- the issue's target repo
    title             text NOT NULL,
    description       text NOT NULL,
    labels            jsonb NOT NULL DEFAULT '[]'::jsonb,
    status            text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'confirmed', 'dismissed')),
    created_issue_iid bigint,                    -- the forge iid, stamped on confirm
    created_at        timestamptz NOT NULL DEFAULT now(),
    resolved_at       timestamptz                -- when confirmed/dismissed
);

-- Per-chat proposal listing (the UI renders a run's proposal cards) + the
-- confirm/dismiss lookups scope by run.
CREATE INDEX idx_issue_proposals_run ON issue_proposals (run_id);

-- +goose Down
DROP TABLE issue_proposals;

ALTER TABLE runs DROP CONSTRAINT runs_kind_shape;
ALTER TABLE runs ADD CONSTRAINT runs_kind_shape CHECK (
    (kind = 'issue'  AND issue_iid IS NOT NULL)
 OR (kind = 'ci_fix' AND pipeline_id IS NOT NULL AND pipeline_ref IS NOT NULL));

ALTER TABLE runs DROP CONSTRAINT runs_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_kind_check CHECK (kind IN ('issue', 'ci_fix'));

ALTER TABLE runs
    DROP COLUMN resume_of_run_id,
    DROP COLUMN title;

-- Restore the NOT NULL. This fails if any chat run (repo_id NULL) still exists;
-- delete them first, as with any down migration that re-tightens a relaxed column.
ALTER TABLE runs ALTER COLUMN repo_id SET NOT NULL;
