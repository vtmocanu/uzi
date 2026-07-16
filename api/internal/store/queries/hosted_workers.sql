-- Hosted workers (PRD #58) --------------------------------------------------
-- The controller-facing half of the protocol. Every query here is instance-wide
-- (the controller is not a user and reconciles the whole cluster), which is why
-- they live apart from runtime.sql's user-scoped worker queries.

-- name: ListHostedWorkersForController :many
-- Desired state for the controller's poll: every hosted worker, plus its pending
-- sealed join token when one is still awaiting delivery (LEFT JOIN — a worker
-- whose token the controller has already acked returns NULL here and is simply
-- reconciled without one).
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

-- name: CreateHostedWorkerToken :exec
-- Park the sealed join-token plaintext for the controller's next poll. Called by
-- the provision path (M2) in the same transaction that inserts the worker row, so
-- a worker can never exist with a token_hash but no pending plaintext to deliver.
-- ON CONFLICT DO NOTHING keeps a retried provision idempotent rather than 23505-ing.
INSERT INTO hosted_worker_tokens (worker_id, token_ciphertext)
VALUES (@worker_id, @token_ciphertext)
ON CONFLICT (worker_id) DO NOTHING;

-- name: DeleteHostedWorkerToken :execrows
-- The ack: the controller has OBSERVED the token materialized as a k8s Secret, so
-- the api's sealed copy has served its purpose and is destroyed (Decision 3's
-- "never at rest in plaintext server-side"). Scoped to kind='hosted' so a
-- controller ack can never reach a row that is not its business. Idempotent — a
-- re-acked worker deletes 0 rows, which is a no-op, not an error.
DELETE FROM hosted_worker_tokens
WHERE worker_id = @worker_id
  AND worker_id IN (SELECT id FROM workers WHERE kind = 'hosted');
