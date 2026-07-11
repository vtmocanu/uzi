-- +goose Up

-- Per-run agent selection (PRD #37): which subagent roster a run used, and what
-- the worker found in the repo it cloned.
--
--   repo_agents       the roster the worker parsed out of the clone's
--                     .claude/agents/ — names + descriptions ONLY (the prompt
--                     bodies never leave the worker). Reported on the first
--                     `running` state report after checkout, so an autopilot run
--                     records it too (Decision 6).
--   agent_source      which roster the run's SUBAGENTS come from: the repo's own
--                     agents, or the owner's uzi templates. The `lead`
--                     orchestrator is always uzi's builtin under either source and
--                     is never selectable (Decision 3).
--   agent_exclusions  names removed from the chosen source's roster. At least one
--                     subagent must survive (validated server-side, never here:
--                     the roster it is checked against lives in another column or
--                     another table).
--
-- All three are nullable, and the two states are NOT the same (Decision 6/7):
--   NULL           a pre-feature run, or one whose worker never reported.
--   '[]'::jsonb    detection ran and found nothing — the gate's repo card goes
--                  inert and the owner's templates are the only choice.
-- Anything that collapses those two (COALESCE to '[]', NOT NULL DEFAULT '[]')
-- would make every historical run claim it was scanned. Don't.
--
-- The selection is a record of what ran, not a cache the worker reads back:
-- reproducibility is deliberately partial (Decision 8) because prompt bodies are
-- not persisted, so a rerun re-reads the repo, possibly at a different commit. A
-- requeue/resume re-enters plan → gate and OVERWRITES agent_source/agent_exclusions
-- with the latest approval.
ALTER TABLE runs
    ADD COLUMN agent_source     text CHECK (agent_source IN ('repo', 'own')),
    ADD COLUMN agent_exclusions jsonb
        CHECK (agent_exclusions IS NULL OR jsonb_typeof(agent_exclusions) = 'array'),
    ADD COLUMN repo_agents      jsonb
        CHECK (repo_agents IS NULL OR jsonb_typeof(repo_agents) = 'array');

-- +goose Down
ALTER TABLE runs
    DROP COLUMN agent_source,
    DROP COLUMN agent_exclusions,
    DROP COLUMN repo_agents;
