-- +goose Up
-- PRD #66 M8 (D8): the admin per-repo guardrail override — a per-repo, admin-only,
-- audited exception that lets ONE named repo through the #66 guardrail with a reason.
-- NOT a global switch (R6), and it NEVER waives the fail-closed protection_unreadable
-- case (D3/R8) — the evaluator downgrades only the six "bot is too strong" codes,
-- post-evaluation, on an overridden repo.
--
-- Three columns on repos:
--   * guardrail_override_reason — NULL = no override. This NULL is THE ACTIVE
--     DISCRIMINATOR the gates read (guardrail_override_reason IS NOT NULL ⇒ overridden);
--     Revoke NULLs all three.
--   * guardrail_override_by — the admin actor (audit). ON DELETE RESTRICT, NOT the
--     agent-template updated_by ON DELETE SET NULL trap (D8): nulling the actor while
--     the override stays live is an audit gap. There is no production user-deletion
--     flow today (no DeleteUser query/handler; only raw test DELETEs), so RESTRICT
--     wedges nothing real; the PRD's fallback if one ever appears is re-attestation,
--     not SET NULL.
--   * guardrail_override_at — when the override was set.
--
-- No backfill: every repo starts with no override (all three NULL).
--
-- Draft migration number 00121 (head is 00120) — renumber above the live head at
-- merge per repo convention.
ALTER TABLE repos
    ADD COLUMN guardrail_override_reason text,
    ADD COLUMN guardrail_override_by uuid REFERENCES users(id) ON DELETE RESTRICT,
    ADD COLUMN guardrail_override_at timestamptz;

-- +goose Down
ALTER TABLE repos
    DROP COLUMN guardrail_override_reason,
    DROP COLUMN guardrail_override_by,
    DROP COLUMN guardrail_override_at;
