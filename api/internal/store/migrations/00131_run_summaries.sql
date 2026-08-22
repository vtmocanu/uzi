-- +goose Up
-- PRD #362 M1: plain-English run summaries. Three nullable columns on runs, all
-- additive (every existing row reads NULL = "no summary", the fallback the web/CLI
-- render as the issue title). summary_intent is "what this run will implement";
-- summary_plan is "what the proposed plan will do"; summary_deltas is the tagged
-- list of how the plan diverged from the original ask
-- ([{ "kind": "added"|"changed"|"dropped", "text": string }, ...]). Deltas are
-- validated-and-rejected on persist and tolerated-with-fallback on read (Decision 6).
ALTER TABLE runs ADD COLUMN summary_intent text NULL;
ALTER TABLE runs ADD COLUMN summary_plan text NULL;
ALTER TABLE runs ADD COLUMN summary_deltas jsonb NULL;

-- +goose Down
ALTER TABLE runs DROP COLUMN IF EXISTS summary_deltas;
ALTER TABLE runs DROP COLUMN IF EXISTS summary_plan;
ALTER TABLE runs DROP COLUMN IF EXISTS summary_intent;
