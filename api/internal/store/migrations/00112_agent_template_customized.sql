-- +goose Up
-- PRD #275 (M4b): a delete-proof "pristine vs admin-customized" discriminator for
-- builtin agent templates so the boot reconciler can re-apply shipped builtin
-- prompt improvements to pristine rows only. customized=false <=> pristine (tracks
-- the embedded body on boot); an admin edit sets it true; Reset returns it to false.
-- Chosen over `updated_by IS NULL`, which the `updated_by ... ON DELETE SET NULL`
-- FK (00011) would silently corrupt — an edited row whose editing admin is later
-- deleted would read as pristine and get overwritten on the next boot — and over
-- overloading the UI-visible `updated_at`.
ALTER TABLE agent_templates
    ADD COLUMN customized BOOLEAN NOT NULL DEFAULT false;

-- Backfill: a pre-existing builtin whose updated_at has moved past created_at was
-- edited or reset by an admin at least once, so mark it customized (conservative —
-- a later Reset returns it to tracking). A never-touched seed keeps them equal.
UPDATE agent_templates
SET customized = true
WHERE scope = 'builtin' AND updated_at > created_at;

-- +goose Down
ALTER TABLE agent_templates DROP COLUMN customized;
