-- PRD #66 M1 (D5): the per-repo privilege findings were reshaped from parallel
-- free-text Violations/Warnings string slices to a coded Finding{Code, Severity,
-- Message} set. That changes the jsonb shape of forge_connections.privilege_report,
-- so old blobs (which carry `"violations"`/`"warnings"` arrays per repo) no longer
-- unmarshal into the new RepoReport and must not linger. The report is a derived
-- cache, not a source of truth — the next privilege sweep and the boot stamp
-- re-write it in seconds — so this NULLs the cache rather than attempting a jsonb
-- rewrite. privilege_status is denormalized from the same report, so it is NULLed
-- too (NULL status renders as an explicit "unchecked" badge until the re-stamp).
-- privilege_checked_at is intentionally left alone: it is a plain timestamp, not
-- reshaped, and clearing it would only lose harmless provenance.
--
-- Data-only, no schema change and no `-- name:` query change, so this needs no
-- sqlc regen (00030_privilege_report.sql owns the columns; the models are
-- unchanged). Draft migration number — renumber above the live head at merge per
-- repo convention.

-- +goose Up
UPDATE forge_connections SET privilege_report = NULL, privilege_status = NULL;

-- +goose Down
-- No-op: the report is derived cache re-stamped by the next sweep / boot stamp;
-- the pre-reshape free-text blobs cannot (and need not) be restored.
