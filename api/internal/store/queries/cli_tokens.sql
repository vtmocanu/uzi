-- name: GetCLITokenByHash :one
-- The auth lookup, carrying the NULL trap verbatim: a bare `expires_at > now()`
-- evaluates to NULL — hence false — for the never-expiring webui-minted uzc_, i.e.
-- it would silently reject the agent/CI token this table exists to serve. Fail
-- closed on revoked/expired, never on a NULL expiry.
SELECT * FROM cli_tokens
WHERE token_hash = $1
  AND NOT revoked
  AND (expires_at IS NULL OR expires_at > now());

-- name: TouchCLIToken :exec
-- Coarse (≤1/min per token) last-used stamp: ONE update sets BOTH last_used_at and
-- last_used_ip, skipped when the row was touched within the last minute — so a
-- single-replica api does not take a DB write on every CLI call. last_used_ip is
-- written for free on the same statement (the only detection control the design
-- has). An empty ip string becomes NULL rather than being cast to inet, so a
-- non-IP RemoteAddr fallback can never error the update.
UPDATE cli_tokens
   SET last_used_at = now(),
       last_used_ip = NULLIF(sqlc.arg(client_ip)::text, '')::inet
 WHERE id = sqlc.arg(id)
   AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute');

-- name: CreateCLIToken :one
-- Mint a token (both acquisition paths land here). The SERVER sets expires_at per
-- the expiry matrix (NULL = never); the client never proposes a lifetime.
INSERT INTO cli_tokens (user_id, name, token_hash, token_prefix, scope, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: ListCLITokens :many
-- The per-user token list (metadata only — the value is never stored, so it can
-- never be listed). Newest first.
SELECT * FROM cli_tokens WHERE user_id = $1 ORDER BY created_at DESC;

-- name: RevokeCLIToken :execrows
-- Soft-delete one of the CALLER'S tokens. Owner-scoped by user_id, so a foreign or
-- unknown id touches zero rows (the handler maps that to 404) — never a cross-user
-- revoke. Returns the affected-row count so the handler can distinguish
-- not-found/not-yours from a real revoke.
UPDATE cli_tokens SET revoked = true
WHERE id = $1 AND user_id = $2 AND NOT revoked;

-- name: RevokeAllCLITokens :exec
-- The panic button: revoke every un-revoked token of one user in a single query
-- over idx_cli_tokens_user. Idempotent — a second call touches zero rows. Scoped to
-- $1, so it revokes the caller's tokens and nobody else's.
UPDATE cli_tokens SET revoked = true WHERE user_id = $1 AND NOT revoked;

-- name: ListAllCLITokensForAdmin :many
-- The factory-wide standing-credential inventory (admin read-only). The question it
-- exists to answer is "who holds credentials to this instance, and are any stale or
-- unexpected?" — which nothing could answer before: every other query in this file is
-- scoped to one user_id or to one token_hash, so a token was visible to its owner and
-- to nobody else, while `workers` has had an admin-wide view since PRD #42.
--
-- COLUMNS ARE PROJECTED EXPLICITLY, AND THAT IS A SECURITY BOUNDARY, NOT A STYLE
-- CHOICE. token_hash is omitted here rather than dropped in the DTO, so it is absent
-- from the generated Go struct and a future DTO edit CANNOT leak it — the sha256 of a
-- credential is offline-crackable, and an admin-wide list of them would turn a
-- visibility fix into a credential-disclosure surface. Contrast ListCLITokens above,
-- which is `SELECT *` and relies on cliTokenDTO to drop the hash: that is safe today
-- because the row never leaves the handler, but it is one careless marshal away from
-- not being. Do not "simplify" this to SELECT * or sqlc.embed.
--
-- Revoked rows are INCLUDED: a revoked token is the incident trail (the whole reason
-- this table soft-deletes), so an audit view that hid them would hide exactly what an
-- investigation needs. They sort last.
SELECT t.id,
       t.user_id,
       u.email AS owner_email,
       t.name,
       t.token_prefix,
       t.scope,
       t.revoked,
       t.created_at,
       t.last_used_at,
       t.last_used_ip,
       t.expires_at
  FROM cli_tokens t
  JOIN users u ON u.id = t.user_id
 ORDER BY t.revoked ASC, u.email ASC, t.created_at DESC, t.id ASC;
