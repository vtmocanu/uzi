-- +goose Up

-- Swap the seeded `terraform` allowlist row for `opentofu` (PRD #123 M2, Decision T,
-- 2026-08-15). terraform is unfree (BUSL since 1.6) so it is not baked into the worker
-- toolchain; OpenTofu (free, MPL-2.0) is baked instead (agent/devbox-global/devbox.json),
-- so this makes the allowlist match the baked toolchain: opentofu becomes both allowlisted and baked
-- (M3-covered, no exception) while terraform is no longer requestable.
--
-- Data-only: no schema change and no new `-- name:` query, so this needs NO sqlc regen.
-- The 00046 seed that first inserted `terraform` is immutable history and is NOT edited.
--
-- NOTE (goose numbering): drafted as 00124 — the next free number above the live head
-- (00123) at drafting time — and renumbered at the landing merge if another migration
-- lands first, per the CLAUDE.md convention.
DELETE FROM tool_allowlist WHERE name = 'terraform';
INSERT INTO tool_allowlist (name) VALUES ('opentofu') ON CONFLICT (name) DO NOTHING;

-- +goose Down
DELETE FROM tool_allowlist WHERE name = 'opentofu';
INSERT INTO tool_allowlist (name) VALUES ('terraform') ON CONFLICT (name) DO NOTHING;
