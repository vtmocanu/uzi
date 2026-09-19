-- Guardrail override requests (PRD #1432 M1) ------------------------------

-- name: UpsertGuardrailOverrideRequest :one
-- Open or refresh a member's request that an admin allow this repo through the
-- #66 guardrail. The ON CONFLICT arbiter is the partial unique index
-- guardrail_override_requests_one_pending (repo_id WHERE status = 'pending'), so a
-- SECOND request for the same repo while one is still pending UPDATEs that open row
-- in place (new reason/findings, created_at bumped to now()) rather than piling up
-- a duplicate. A decided request has dropped out of the partial index, so it does
-- not conflict and the next refusal opens a fresh pending row. findings is the
-- audit/display snapshot only — no gate reads it back.
INSERT INTO guardrail_override_requests (repo_id, requested_by, reason, findings)
VALUES (@repo_id, @requested_by, @reason, @findings)
ON CONFLICT (repo_id) WHERE status = 'pending'
DO UPDATE SET reason     = EXCLUDED.reason,
              findings   = EXCLUDED.findings,
              created_at = now()
RETURNING *;

-- name: GetGuardrailOverrideRequest :one
-- One request by id (the decide handler's preflight; an unknown id returns no rows).
SELECT * FROM guardrail_override_requests WHERE id = $1;

-- name: DecideGuardrailOverrideRequest :one
-- Settle a PENDING request as approved or rejected, stamping the deciding admin and
-- an optional note. The `AND status = 'pending'` guard is what makes a decision
-- single-shot: a second decide on the same id (already decided, or unknown) matches
-- no row and returns pgx.ErrNoRows, so the handler cannot double-decide or race two
-- admins into conflicting outcomes. decided_at is now(), never from the request body.
-- decision_note is optional (sqlc.narg → nullable). This M1 seam only records the
-- decision; applying an approval to repos.guardrail_override_* is a later milestone.
UPDATE guardrail_override_requests
SET status        = @status,
    decided_by    = @decided_by::uuid,
    decided_at    = now(),
    decision_note = sqlc.narg('decision_note')
WHERE id = @id AND status = 'pending'
RETURNING *;

-- name: ListGuardrailOverrideRequestsForUser :many
-- The requesting member's own view: the LATEST request per repo they own, scoped
-- through the repo's connection to the caller (c.user_id = $1) so it can never leak
-- another tenant's request. DISTINCT ON (gor.repo_id) with the matching ORDER BY
-- picks the newest request per repo (a repo may have an old decided request plus a
-- fresh pending one). The connection join is the tenant boundary — a request whose
-- repo belongs to a different user's connection is never returned.
SELECT DISTINCT ON (gor.repo_id) gor.*
FROM guardrail_override_requests gor
JOIN repos r ON r.id = gor.repo_id
JOIN forge_connections c ON c.id = r.connection_id
WHERE c.user_id = $1
ORDER BY gor.repo_id, gor.created_at DESC;

-- name: ListPendingGuardrailOverrideRequests :many
-- The admin cross-user queue (PRD #1432): every PENDING request across ALL users,
-- joined to its repo (path), owning user (id + email), and connection (forge_type)
-- so the admin can triage without a per-row fan-out. UNSCOPED — admin-only, gated in
-- the handler (precedent AdminListReposWithPrivilege). Ordered by owner email then
-- repo path for a stable, human-scannable queue.
SELECT gor.id, gor.repo_id, gor.requested_by, gor.reason, gor.findings, gor.status, gor.created_at,
       r.path_with_namespace, u.id AS owner_id, u.email AS owner_email, c.forge_type
FROM guardrail_override_requests gor
JOIN repos r ON r.id = gor.repo_id
JOIN forge_connections c ON c.id = r.connection_id
JOIN users u ON u.id = c.user_id
WHERE gor.status = 'pending'
ORDER BY u.email ASC, r.path_with_namespace ASC;
