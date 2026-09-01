-- +goose Up
-- Backfill (issue #804): a user's SOLE anthropic_token becomes auto-select eligible, so
-- their auto-mode ephemeral workers can spend it instead of parking in pool_wait. Scoped
-- to users with EXACTLY ONE anthropic_token — the reserved-console-key hazard (00087) is
-- inherently multi-token, so a multi-token user is never touched here (their opt-in stays
-- explicit). Keyed on token COUNT, never on is_default: tying eligibility to the default
-- pointer would silently pool a console key a user later promotes to default (PRD #111 D2).
UPDATE user_secrets s
SET auto_eligible = true
WHERE s.kind = 'anthropic_token'
  AND NOT s.auto_eligible
  AND NOT EXISTS (
      SELECT 1 FROM user_secrets o
      WHERE o.user_id = s.user_id AND o.kind = 'anthropic_token' AND o.id <> s.id
  );

-- +goose Down
-- No-op, deliberately and unavoidably: once applied, a backfilled row is indistinguishable
-- from a genuine owner opt-in, so Down cannot restore false without clobbering real opt-ins.
-- (Mirrors 00088's lossy-down precedent.)
SELECT 1;
