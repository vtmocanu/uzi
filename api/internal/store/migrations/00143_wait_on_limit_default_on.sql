-- +goose Up

-- Issue #520: make wait-on-limit the DEFAULT-ON behaviour. uzi is a
-- single-operator self-host, so nobody should have to opt in to a run PARKING
-- (rather than failing) when the owner's Anthropic usage window is exhausted.
-- This flips the two per-user / per-schedule column defaults and backfills
-- existing users, so the whole instance moves to the safe-by-default behaviour.
--
-- NUMBER ASSIGNED AT LANDING. Drafted as 00143 against a live head of
-- 00142_capability_scheduling.sql; renumber above the live head on the landing
-- rebase if another migration merged first. Per CLAUDE.md that renumber is
-- mandatory, not tidy: the boot runner is strict goose (no allow-missing, see
-- 00091's header), so landing a version BELOW an already-applied head makes
-- every upgraded instance refuse to boot.
--
-- 🔴 DO NOT TOUCH runs.wait_on_limit. Judge runs deliberately rely on
-- runs.wait_on_limit DEFAULT false staying false, so a judge run is never
-- parked on a usage limit. Only the users and run_schedules COLUMN defaults
-- change here — the per-run stamp is left exactly as 00091 set it.

-- users --------------------------------------------------------------------
--
-- Flip the per-user default a new run inherits (00091 set it false). New-user
-- creates (CreateUser / CreateUserOIDC) do not stamp this column, so they pick
-- up this new default automatically.
ALTER TABLE users ALTER COLUMN wait_on_limit SET DEFAULT true;

-- Backfill existing users to the new default (issue #520 decision:
-- single-operator self-host, nobody should have to opt in). This is a ONE-WAY
-- data change: once a row is set true here it is indistinguishable from a row
-- the operator set true by choice, so the Down section deliberately does NOT
-- reverse it (see below).
UPDATE users SET wait_on_limit = true WHERE wait_on_limit = false;

-- run_schedules ------------------------------------------------------------
--
-- Align the DB column default with the create handler's applyCreateDefaults,
-- which already fills wait_on_limit = true. Cosmetic today (the handler always
-- supplies a value), but it closes a future silent-opt-out gap where a new
-- insert path that omits the column would otherwise land false.
ALTER TABLE run_schedules ALTER COLUMN wait_on_limit SET DEFAULT true;

-- +goose Down

ALTER TABLE run_schedules ALTER COLUMN wait_on_limit SET DEFAULT false;
ALTER TABLE users ALTER COLUMN wait_on_limit SET DEFAULT false;

-- The Up backfill (UPDATE users SET wait_on_limit = true) is intentionally
-- one-way and is NOT reversed here: a row set true by the backfill is
-- indistinguishable from a row the operator set true by choice, so restoring
-- every user to false on rollback would silently discard real opt-in choices.
-- Down therefore restores only the column DEFAULTs.
