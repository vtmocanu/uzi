-- Hosted workers (PRD #58) --------------------------------------------------
-- The controller-facing half of the protocol. Every query here is instance-wide
-- (the controller is not a user and reconciles the whole cluster), which is why
-- they live apart from runtime.sql's user-scoped worker queries.

-- name: ListHostedWorkersForController :many
-- Desired state for the controller's poll: every hosted worker, plus its pending
-- sealed join token when one is still awaiting delivery (LEFT JOIN — a worker
-- whose token the controller has already acked, or whose token expired unread,
-- returns NULL here and is simply reconciled without one).
--
-- Deliberately carries NO user-controlled text (Decision 7): workers.name is
-- arbitrary 200-byte user input, and the controller renders k8s object names from
-- what this hands it. Object naming derives from the uuid (uzi-hw-<id>), so the
-- name is not selected here at all, rather than selected and trusted not to be used.
--
-- Deliberately unbounded: the whole hosted fleet is the desired state, and the
-- controller reconciles it as a set — a paged poll could never express "these are
-- all the workers that should exist", which is what makes orphan detection
-- (Decision 9) possible. The per-user quota (M2) bounds the row count.
SELECT w.id,
       w.template_declared,
       w.hosted_size,
       w.hosted_generation,
       t.token_ciphertext
FROM workers w
LEFT JOIN hosted_worker_tokens t ON t.worker_id = w.id
WHERE w.kind = 'hosted'
ORDER BY w.created_at ASC;

-- name: UpsertHostedWorkerToken :exec
-- Park a sealed join-token plaintext for the controller's next poll. Called by the
-- provision path (M2) in the same transaction that inserts the worker row, so a
-- worker can never exist with a token_hash but no pending plaintext to deliver.
--
-- An UPSERT, not a plain INSERT, because this is also the ROTATION path (M2/M3):
-- recovering a stranded worker mints a NEW token — new plaintext, new sha256 on
-- the workers row, bumped hosted_generation — and re-parks it here, which must
-- reset delivered_at so the fresh token reads as pending again. Old plaintext is
-- never resurrected; no query in this file could, since the ciphertext is gone the
-- moment it is delivered or expired.
INSERT INTO hosted_worker_tokens (worker_id, token_ciphertext, delivered_at, created_at)
VALUES (@worker_id, @token_ciphertext, NULL, now())
ON CONFLICT (worker_id) DO UPDATE SET
    token_ciphertext = EXCLUDED.token_ciphertext,
    delivered_at     = NULL,
    created_at       = now();

-- name: MarkHostedWorkerTokenDelivered :execrows
-- The ack: the controller has OBSERVED the token materialized as a k8s Secret, so
-- the api's sealed copy has served its purpose and is destroyed (Decision 3's
-- "never at rest in plaintext server-side"). The row survives with delivered_at
-- stamped, which is what distinguishes a delivery from an expiry.
--
-- Scoped to kind='hosted' so a controller ack can never reach a row that is not
-- its business. Idempotent: a re-ack of an already-delivered worker matches the
-- row, rewrites the same NULL, and re-stamps delivered_at — the controller re-acks
-- every poll for the worker's whole life, since its ack is derived from the Secret
-- it keeps observing.
UPDATE hosted_worker_tokens SET
    token_ciphertext = NULL,
    delivered_at     = now()
WHERE worker_id = @worker_id
  AND worker_id IN (SELECT id FROM workers WHERE kind = 'hosted');

-- name: ExpirePendingHostedWorkerTokens :execrows
-- Bound how long a sealed join token may sit at rest (PRD #58, a residual BEYOND
-- Decision 3's stated one). The documented residual is plaintext in etcd for the
-- worker's lifetime; this is different: the pending copy sits in Postgres sealed
-- under UZI_SECRET_KEY, the master key the vault (PRD #32) exists to stop relying
-- on — per docs/vault-threat-model.md an operator holding the api env plus a DB
-- dump has every master-sealed value in plaintext. If the controller never polls
-- (chart not deployed, controller down, hosting flipped off after provisioning,
-- worker abandoned), "delivered once" would quietly degrade into "at rest
-- indefinitely".
--
-- Expiring STRANDS the worker by design: its token_hash is committed and no
-- plaintext survives, so it can never register. That is the intended trade — a
-- stranded worker is visible (it never comes online) and recoverable (M2/M3 rotate
-- a new token in via UpsertHostedWorkerToken), whereas an unbounded sealed secret
-- is neither. delivered_at stays NULL, which is exactly what marks the row
-- stranded rather than done.
UPDATE hosted_worker_tokens SET token_ciphertext = NULL
WHERE token_ciphertext IS NOT NULL
  AND created_at < @cutoff;
