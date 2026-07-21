-- +goose Up
-- PRD #99: per-instance activity lanes. Two nullable, denormalized columns on
-- run_messages, carried per-frame exactly as `agent` already is (no worker-side
-- correlation state, no backfill):
--   agent_instance = the SDK's per-frame `parent_tool_use_id` — the subagent
--     INVOCATION id, so two parallel same-role subagents stay distinguishable.
--   agent_label    = the SDK's per-frame `task_description` — what that
--     invocation was asked to do, used as the lane's title.
-- Both are NULL whenever the frame carries no `parent_tool_use_id` -- the
-- orchestrator's own turns, infra/worker frames, and every pre-migration message.
-- NOT the same as `agent = 'lead'`: a repo may legitimately ship an agent NAMED
-- lead, which is a real subagent and does carry an instance id.
-- The web falls back to the role name.
ALTER TABLE run_messages ADD COLUMN agent_instance text;
ALTER TABLE run_messages ADD COLUMN agent_label text;

-- +goose Down
ALTER TABLE run_messages DROP COLUMN agent_label;
ALTER TABLE run_messages DROP COLUMN agent_instance;
