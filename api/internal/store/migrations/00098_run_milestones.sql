-- +goose Up

-- Milestone-structured runs (PRD #122 M1): an optional, human-approved milestone
-- list on a run. A milestone is a small {id, title} object; the two columns hold
-- the same shape at two different stages of its lifecycle, modelled on 00052's
-- agent_exclusions/repo_agents.
--
--   milestones_candidate  the PRE-APPROVAL list a worker proposes on its
--                         awaiting_approval report. REPLACED each revision round
--                         (Decision 2), so a fresh proposal overwrites the prior
--                         one rather than accumulating.
--   milestones_frozen     the IMMUTABLE, approved list. Written once — at the
--                         human approve (candidate copied to frozen) or by an
--                         autopilot run's `running` report — and never overwritten
--                         after, so it is the stable list a resume replays and the
--                         run DTO exposes.
--
-- Both are nullable, and NULL is NOT the same as '[]' (as in 00052):
--   NULL         no list was ever reported/approved — a run with no milestones,
--                which behaves exactly as a pre-feature run.
--   '[]'::jsonb  a list was reported and it was empty.
-- Anything that collapses those two (COALESCE to '[]', NOT NULL DEFAULT '[]')
-- would make every historical run claim it had a milestone list. Don't.
ALTER TABLE runs
    ADD COLUMN milestones_candidate jsonb
        CHECK (milestones_candidate IS NULL OR jsonb_typeof(milestones_candidate) = 'array'),
    ADD COLUMN milestones_frozen    jsonb
        CHECK (milestones_frozen IS NULL OR jsonb_typeof(milestones_frozen) = 'array');

-- +goose Down
ALTER TABLE runs
    DROP COLUMN milestones_candidate,
    DROP COLUMN milestones_frozen;
