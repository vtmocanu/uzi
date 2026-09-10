-- +goose Up

-- PRD #1224 M2: per-milestone agent attribution. A nullable jsonb column holding the
-- lead-declared, server-validated attribution subset for the run's in-progress milestones:
-- an array of {id, agent, agent_label?} objects (agent = the subagent_type identifier the
-- server joins byte-exact against current_activity.agent). NULLABLE, NO DEFAULT, so every
-- existing row is byte-unchanged (no rewrite, no NOT NULL). Written COUPLED to
-- milestones_in_progress (SetRunRunning, Decision 6) and cleared on every terminal
-- transition beside milestones_in_progress (Decision 7).
ALTER TABLE runs ADD COLUMN milestones_agents jsonb;

-- Shape guard: the column is either NULL or a JSON array (never an object/scalar). Added
-- NOT VALID to skip the validating table scan (and the ACCESS EXCLUSIVE lock it would hold);
-- new/updated rows are still enforced. 00212 runs VALIDATE CONSTRAINT under a lock-cheap scan.
ALTER TABLE runs ADD CONSTRAINT runs_milestones_agents_is_array
    CHECK (milestones_agents IS NULL OR jsonb_typeof(milestones_agents) = 'array') NOT VALID;

-- +goose Down
ALTER TABLE runs DROP CONSTRAINT IF EXISTS runs_milestones_agents_is_array;
ALTER TABLE runs DROP COLUMN milestones_agents;
