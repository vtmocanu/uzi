-- +goose Up

-- PRD #5: PAT least-privilege verification. The privilege checker stamps one
-- jsonb report per connection (token findings + per-repo findings embedded), the
-- time it ran, and a denormalized worst-case status for cheap list queries and
-- badges. A normalized findings table would over-model "show the current state":
-- there is no history/audit requirement here.
--
-- All three columns are nullable. A NULL privilege_status means NEVER CHECKED —
-- every connection that predates this migration — and the UI renders that as an
-- explicit "unchecked" badge, never a ✓. The boot sweep back-fills these within
-- seconds of the first deploy, so a grandfathered over-privileged token surfaces
-- immediately rather than one interval later.
ALTER TABLE forge_connections
    ADD COLUMN privilege_report     jsonb,
    ADD COLUMN privilege_checked_at timestamptz,
    ADD COLUMN privilege_status     text;

-- +goose Down
ALTER TABLE forge_connections
    DROP COLUMN privilege_report,
    DROP COLUMN privilege_checked_at,
    DROP COLUMN privilege_status;
