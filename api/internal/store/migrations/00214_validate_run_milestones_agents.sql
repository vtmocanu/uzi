-- +goose Up

-- Validate the CHECK added NOT VALID in 00211 (PRD #1224 M2). VALIDATE CONSTRAINT scans the
-- table to confirm existing rows satisfy the CHECK but takes only a SHARE UPDATE EXCLUSIVE
-- lock (write-compatible), unlike the ACCESS EXCLUSIVE an inline validated ADD would hold.
-- Split from 00211 per the standard two-step pattern for a CHECK on a live table.
ALTER TABLE runs VALIDATE CONSTRAINT runs_milestones_agents_is_array;

-- +goose Down
-- No VALIDATE inverse; restore the pre-00212 state (constraint present but NOT VALID) by
-- dropping and re-adding it NOT VALID. 00211's Down then drops it.
ALTER TABLE runs DROP CONSTRAINT runs_milestones_agents_is_array;
ALTER TABLE runs ADD CONSTRAINT runs_milestones_agents_is_array
    CHECK (milestones_agents IS NULL OR jsonb_typeof(milestones_agents) = 'array') NOT VALID;
