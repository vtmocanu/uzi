-- Hosted workers (PRD #58) --------------------------------------------------
-- The controller-facing half of the protocol. Every query here is instance-wide
-- (the controller is not a user and reconciles the whole cluster), which is why
-- they live apart from runtime.sql's user-scoped worker queries.

-- name: ListHostedWorkersForController :many
-- Desired state for the controller's poll: every hosted worker, plus its pending
-- sealed join token when one is still awaiting delivery (LEFT JOIN — a worker whose
-- pod has already proved it holds its token, or whose token expired unread, returns
-- NULL here and is simply reconciled without one).
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
-- The delivery acknowledgement, driven by PROOF OF POSSESSION: the worker itself
-- registered, and RequireWorker only resolved it by matching sha256(the token it
-- presented) against workers.token_hash. So a pod is demonstrably holding the
-- CURRENT token, and the api's sealed buffer has served its purpose and is
-- destroyed (Decision 3's "never at rest in plaintext server-side"). The row
-- survives with delivered_at stamped, which is what distinguishes a delivery from
-- an expiry.
--
-- It is NOT a controller assertion. A controller ack could only say "a Secret
-- exists for W" without naming the token in it, so a rotation racing an in-flight
-- ack destroyed the fresh plaintext undelivered and recorded the row as delivered
-- steady-state while the pod 401'd forever on the old token. Registration cannot
-- lie about the version: a pod holding T1 after a rotation to T2 fails auth and
-- never reaches this statement.
--
-- The (ciphertext IS NOT NULL OR delivered_at IS NULL) guard is for IDEMPOTENCE on
-- re-registration — a pod rescheduled onto another node presents the same token
-- again, and that must be a no-op, not a re-stamp. It admits exactly two states:
--   * pending  (ciphertext, no delivered_at) -> the real delivery
--   * expired  (neither)                     -> a late but genuine SELF-HEAL: the
--     Secret was written after all and the pod finally booted (a slow image pull,
--     an ImagePullBackOff that cleared), so the row is corrected to the truth
--     rather than left claiming a strand that a rotation would then "fix" for no
--     reason. Only a proof licenses this transition; a report never could.
-- and excludes the already-delivered state, which is where re-registration lands.
--
-- Still scoped to kind='hosted': the handler already checks Kind, so this is
-- defence in depth against a future caller that does not.
UPDATE hosted_worker_tokens SET
    token_ciphertext = NULL,
    delivered_at     = now()
WHERE worker_id = @worker_id
  AND worker_id IN (SELECT id FROM workers WHERE kind = 'hosted')
  AND (token_ciphertext IS NOT NULL OR delivered_at IS NULL);

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
-- Expiring is BENIGN in the normal case, and that is what makes 1h a safe default
-- even though the pending window is now pod-scheduling-plus-image-pull rather than
-- two poll intervals. This clears the api's sealed BUFFER, not the token: if the
-- controller already wrote the Secret, the plaintext is in the cluster and
-- workers.token_hash is untouched, so whenever the pod finally boots it reads the
-- file, authenticates, registers, and MarkHostedWorkerTokenDelivered corrects the
-- row late (its expired-state clause exists for exactly that self-heal).
--
-- Expiry only strands when the Secret was NEVER written — which is precisely the
-- case the TTL exists for. A stranded worker is visible (it never comes online,
-- Decision 10's heartbeat) and recoverable (M2/M3 rotate a new token in via
-- UpsertHostedWorkerToken), whereas an unbounded sealed secret is neither.
-- delivered_at stays NULL, which is what marks the row stranded rather than done.
--
-- So the TTL now means "no pod ever proved it booted", NOT "the controller never
-- picked it up".
UPDATE hosted_worker_tokens SET token_ciphertext = NULL
WHERE token_ciphertext IS NOT NULL
  AND created_at < @cutoff;
