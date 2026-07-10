-- +goose Up
-- Agent template allocations (PRD #18 M7): which templates ride each user's
-- claim. Same shape as agent_skill_allocations (00040) — a shared/global default
-- layer (user_id NULL, admin-managed) plus a per-user overlay — but the overlay
-- carries an `enabled` flag so a user can both ADD a template (their own, or
-- re-enable a global default) and REMOVE a global default from their own runs.
CREATE TABLE agent_template_allocations (
    template_id UUID NOT NULL REFERENCES agent_templates (id) ON DELETE CASCADE,
    user_id     UUID REFERENCES users (id) ON DELETE CASCADE,  -- NULL = global default (admin); else that user's overlay
    enabled     BOOLEAN NOT NULL DEFAULT true,                 -- shared rows are always true (presence = default-on); a user overlay may be false (force-off)
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Row identity (no surrogate PK): a NULL (shared) user_id folds to a fixed
-- sentinel so a shared default row and a user overlay for the same template
-- coexist while duplicates cannot.
CREATE UNIQUE INDEX uq_agent_template_allocations ON agent_template_allocations
    (template_id, COALESCE(user_id, '00000000-0000-0000-0000-000000000000'));

-- No empty-means-all cliff (review m5): seed an explicit global-default row for
-- every existing builtin/global template. On a fresh DB this table starts empty
-- (builtins are reconciler-seeded AFTER migrations); the builtin reconciler seeds
-- each builtin's default row when it first inserts the builtin, and the global
-- create handler seeds a new global's row. So absence of a shared row is always a
-- deliberate admin removal, never an unseeded default — and absence of a user
-- overlay simply means "follow the global default set".
INSERT INTO agent_template_allocations (template_id, user_id, enabled)
SELECT id, NULL, true FROM agent_templates WHERE scope <> 'user';

-- +goose Down
DROP TABLE agent_template_allocations;
