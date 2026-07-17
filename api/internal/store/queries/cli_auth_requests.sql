-- name: CreateCLIAuthRequest :one
-- Start a browser-brokered login (PRD #64 M5). The client sends the PKCE
-- code_challenge (base64url(S256(verifier))) and a client_desc; the server assigns a
-- unique user_code and a ~5min expiry. scope defaults to 'user' and is chosen on the
-- consent screen at approve, so it is NOT set here. user_code is UNIQUE, so a
-- collision is a loud insert failure the handler retries with a fresh code, never a
-- silent cross-wire.
INSERT INTO cli_auth_requests (code_challenge, client_desc, user_code, expires_at)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetCLIAuthRequest :one
-- Consent-screen metadata (GET /api/auth/cli/request/{id}) and the approve/deny
-- pre-check. Keyed on the request_id (the URL-carried lookup key), never on
-- user_code — that invariant is what keeps the user_code's ~40 bits sufficient. The
-- handler exposes only client_desc + status + expires_at; code_challenge and the
-- user_code never leave the server (the human TYPES the code from their terminal).
SELECT * FROM cli_auth_requests WHERE id = $1;

-- name: ApproveCLIAuthRequest :execrows
-- Mark a pending, unexpired request approved, binding it to the session user and the
-- consent-screen scope. Mints NOTHING — the token is minted claim-first in the poll
-- tx. Guarded on status='pending' AND expires_at>now(): a second approve, or an
-- approve of a denied/consumed/expired request, touches zero rows (the handler maps
-- that to 409). The user_code the human typed is validated in the handler before
-- this runs.
UPDATE cli_auth_requests
   SET status = 'approved', user_id = $2, scope = $3
 WHERE id = $1 AND status = 'pending' AND expires_at > now();

-- name: DenyCLIAuthRequest :execrows
-- Mark a pending, unexpired request denied (the consent screen's Deny button).
-- Guarded like approve; a non-pending request touches zero rows.
UPDATE cli_auth_requests
   SET status = 'denied'
 WHERE id = $1 AND status = 'pending' AND expires_at > now();

-- name: ClaimCLIAuthRequest :one
-- The claim-first mint guard (PRD #64), run INSIDE the poll transaction. Under READ
-- COMMITTED two concurrent polls could both SELECT status='approved' and both mint,
-- yielding two tokens from one approval; the atomic UPDATE...RETURNING makes the
-- claim ITSELF the guard, so exactly one poll flips approved→consumed and gets the
-- row. Mint ONLY on a returned row; zero rows (pgx.ErrNoRows) means not-yet-approved
-- / denied / already-consumed / expired, which the handler disambiguates via
-- GetCLIAuthRequest for the poll status. On a code_challenge mismatch the handler
-- rolls the whole tx back, leaving the row 'approved' (never 'denied') so a junk poll
-- from someone who learned the non-secret request_id cannot kill a live login.
UPDATE cli_auth_requests
   SET status = 'consumed'
 WHERE id = $1 AND status = 'approved' AND expires_at > now()
RETURNING code_challenge, user_id, scope, client_desc;

-- name: DeleteExpiredCLIAuthRequests :execrows
-- Sweep expired rows. cli_auth_requests are short-lived but nothing else deletes
-- them; this runs opportunistically in the start handler and on the shared sweeper
-- ticker (the repo's precedent for stale rows). Returns the count for the sweeper log.
DELETE FROM cli_auth_requests WHERE expires_at < now();
