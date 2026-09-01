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
       w.docker_enabled,
       -- busy (PRD #422 M3, Decision 5): does this worker hold any non-terminal run? This
       -- reuses the EXACT active-run predicate the DeleteWorker guard uses
       -- (CountWorkerNonTerminalRuns), awaiting_approval included, because rolling a worker
       -- that holds such a run would requeue it. Feeds the controller's cordon/defer-roll
       -- decision (M4).
       (SELECT count(*) FROM runs r
          WHERE r.worker_id = w.id
            AND r.status NOT IN ('completed', 'failed', 'cancelled')) > 0 AS busy,
       -- draining_since (PRD #422 M3/M5): the raw nullable cordon timestamp — WHEN this
       -- worker was cordoned, or NULL if it is not draining (draining == draining_since != nil).
       -- M5 selects the timestamp itself rather than an `IS NOT NULL` boolean because the
       -- controller's reconcile loop is stateless (it remembers nothing across ticks) and must
       -- compute `now - draining_since >= deadline` to enforce the bounded drain deadline; the
       -- bool it replaces could only answer "draining y/n", not "for how long".
       w.draining_since,
       t.token_ciphertext
FROM workers w
LEFT JOIN hosted_worker_tokens t ON t.worker_id = w.id
WHERE w.kind = 'hosted'
ORDER BY w.created_at ASC;

-- name: CountHostedWorkersForUser :one
-- The user's PERSISTENT hosted-worker count, for the standing quota gate (PRD #58
-- Decision 8). HOSTED rows only: the quota bounds what the CLUSTER runs, and an
-- external worker the user runs by hand on their own laptop costs us nothing.
--
-- `AND NOT ephemeral` (PRD #529 M2): ephemeral auto-provisioned workers are capped
-- SEPARATELY (CountEphemeralHostedWorkersForUser, under UZI_EPHEMERAL_MAX_PER_USER),
-- so a run-bound throwaway worker must not consume the user's standing persistent
-- quota — otherwise a burst of ephemeral provisions would block the persistent
-- provisions this quota exists to bound.
--
-- THIS IS A SNAPSHOT READ, AND IT IS MEANINGFUL ONLY UNDER THE LOCK ITS CALLER
-- HOLDS. On its own it is a TOCTOU: READ COMMITTED takes no predicate locks, so two
-- concurrent provisions both read N-1, both conclude they are under quota, and both
-- insert — the user lands at quota+1. No rewrite of this statement fixes that. A
-- guarded `INSERT … WHERE (SELECT count(*) …) < quota` has exactly the same hole
-- (each subselect evaluates against its own snapshot) while LOOKING self-sufficient,
-- which is why it was tried and dropped; FOR UPDATE cannot help either, since it
-- locks rows that exist and the rows that would break the invariant do not exist
-- yet.
--
-- What makes it correct is that its only caller — provisionHostedWorker in
-- handler/hosted_workers.go — holds
-- pg_advisory_xact_lock(HostedProvisionLockClass, objid(user_id)) for the whole
-- transaction, so a second provision's count runs only after the first commits and
-- therefore sees its row. Measured, not assumed: with that lock removed, 8
-- concurrent provisions all pass a quota of 2
-- (TestProvisionHostedWorkerQuotaRaceLiveDB).
--
-- ListAppSettingsForUpdate above closes its own TOCTOU the analogous way. It can use
-- a row lock because it validates the rows it locks; this one cannot, for the reason
-- named above, which is exactly why the mechanism here is an advisory lock.
--
-- Uses idx_workers_user (00020_workers_runs.sql): an index scan filtering kind, not
-- a scan of the hosted fleet.
SELECT count(*) FROM workers WHERE user_id = @user_id AND kind = 'hosted' AND NOT ephemeral;

-- name: CountEphemeralHostedWorkersForUser :one
-- The user's CONCURRENT ephemeral hosted-worker count, for the ephemeral cap gate
-- (PRD #529 M2, UZI_EPHEMERAL_MAX_PER_USER). It is the ephemeral analogue of
-- CountHostedWorkersForUser: a separate cap so ephemeral provisions never draw down
-- the standing persistent quota and vice versa.
--
-- THIS IS A SNAPSHOT READ, AND IT IS MEANINGFUL ONLY UNDER THE LOCK ITS CALLER
-- HOLDS — exactly like CountHostedWorkersForUser. On its own it is a TOCTOU under
-- READ COMMITTED: two concurrent provisions both read N-1 and both insert. What
-- makes it correct is that its only caller — the ephemeral provisioner's tx — holds
-- pg_advisory_xact_lock(HostedProvisionLockClass, objid(user_id)) for the whole
-- transaction (the SAME lock class + key the persistent provisionHostedWorker takes),
-- so ephemeral and persistent provisions for one user serialize and this count runs
-- only after a prior provision in the same lock class commits.
SELECT count(*) FROM workers WHERE user_id = @user_id AND kind = 'hosted' AND ephemeral;

-- name: CreateEphemeralHostedWorker :one
-- Insert a run-bound EPHEMERAL hosted worker (PRD #529 M2). It is CreateHostedWorker
-- with ephemeral = true and ephemeral_run_id set, sharing the same deliberately
-- UNGUARDED shape — the cap decision belongs to the caller, which makes it under the
-- advisory lock using CountEphemeralHostedWorkersForUser above, so the whole safety
-- property reads top-to-bottom in one Go function (mirrors provisionHostedWorker).
--
-- The partial UNIQUE index uq_workers_ephemeral_run (00155) makes this INSERT the
-- one-per-run guard: a second provision for the same run raises 23505, which the
-- caller treats as "already provisioned" rather than an error — the per-run race is
-- closed at the schema level, independent of api replica count.
--
-- Same NOT-NULL hosted metadata as CreateHostedWorker (ck_workers_hosted_metadata
-- requires template_declared/hosted_size NOT NULL on a hosted row, docker_enabled an
-- explicit true/false). ephemeral_run_id is cast to a non-null uuid so the param is a
-- plain uuid.UUID: the provisioner always has a concrete run to bind to.
--
-- anthropic_bind_mode is now caller-supplied (issue #804): the provisioner passes `auto`
-- when the owner has ≥1 auto_eligible anthropic_token (a non-empty auto-select pool) and
-- `default` otherwise, so an auto worker never parks a run in pool_wait on an empty pool.
INSERT INTO workers (user_id, name, token_hash, template_declared, kind, hosted_size, docker_enabled, ephemeral, ephemeral_run_id, anthropic_bind_mode)
VALUES (@user_id, @name, @token_hash, @template_declared, 'hosted', @hosted_size, @docker_enabled, true, @ephemeral_run_id::uuid, @anthropic_bind_mode)
RETURNING *;

-- name: DeleteEphemeralWorkerForRun :execrows
-- Teardown primitive (PRD #529 M4): drop the ephemeral worker bound to a now-terminal
-- run, but ONLY when that worker holds no non-terminal run — the SAME busy definition as
-- CountWorkerNonTerminalRuns and the ListHostedWorkersForController `busy` column
-- (status NOT IN completed/failed/cancelled). The controller reaps the pod on its next
-- poll (a hosted row's absence => teardown). Idempotent: matches nothing when there is no
-- bound ephemeral worker or it is still busy, so a double call or a non-ephemeral run is a
-- harmless no-op. hosted_worker_tokens.worker_id is ON DELETE CASCADE, so the token row
-- goes with it. The FK runs.worker_id is ON DELETE SET NULL, so the terminal run keeps
-- its record (worker_id nulled) — never blocked, never orphaning a non-terminal run
-- (the guard forbids that).
--
-- @run_id is cast to a non-null uuid (like CreateEphemeralHostedWorker's
-- @ephemeral_run_id::uuid) so the generated param is a plain uuid.UUID: the caller always
-- has a concrete run to tear down. A NULL ephemeral_run_id never equals it, so an
-- ephemeral worker whose bound run was already reaped (FK SET NULL) is left alone.
DELETE FROM workers w
WHERE w.ephemeral
  AND w.ephemeral_run_id = @run_id::uuid
  AND NOT EXISTS (
      SELECT 1 FROM runs r
      WHERE r.worker_id = w.id
        AND r.status NOT IN ('completed', 'failed', 'cancelled')
  );

-- name: ReapEphemeralWorkers :execrows
-- Orphan/failure GC backstop (PRD #529 M5, Decision 6). DELETE every ephemeral worker
-- that can no longer make progress. Busy-guarded so a worker holding ANY non-terminal
-- run is never reaped mid-flight (defense-in-depth; M3's claim restriction means only the
-- bound run ever points at it). The controller reaps the pod on its next poll (a hosted
-- row's absence => teardown); the token row goes via ON DELETE CASCADE.
--   (a) no live bound run: NOT EXISTS a non-terminal run at ephemeral_run_id — covers a
--       terminal owning run (the backstop for every terminal writer M4's SetState hook
--       misses), an absent run, and an unlinked ephemeral_run_id (FK SET NULL).
--   (b) never booted past the provision deadline: online_since NULL and created_at old.
--   (c) idle-stolen: online past the deadline and the bound run is claimed by a SIBLING
--       (worker_id set and != this worker), so this worker will never get work.
DELETE FROM workers w
WHERE w.ephemeral
  AND NOT EXISTS (
      SELECT 1 FROM runs br
      WHERE br.worker_id = w.id
        AND br.status NOT IN ('completed', 'failed', 'cancelled')
  )
  AND (
      NOT EXISTS (
          SELECT 1 FROM runs r
          WHERE r.id = w.ephemeral_run_id
            AND r.status NOT IN ('completed', 'failed', 'cancelled')
      )
      OR (w.online_since IS NULL AND w.created_at < @deadline_cutoff)
      OR (w.online_since IS NOT NULL AND w.online_since < @deadline_cutoff
          AND EXISTS (
              SELECT 1 FROM runs r
              WHERE r.id = w.ephemeral_run_id
                AND r.worker_id IS NOT NULL AND r.worker_id <> w.id
          ))
  );

-- name: CreateHostedWorker :one
-- Insert a hosted worker. Deliberately UNGUARDED: the quota decision belongs to the
-- caller, which makes it under the advisory lock using the count above, so the whole
-- safety property reads top-to-bottom in one Go function (the shape of
-- createUserFirstAdmin in handler/auth.go) instead of being split half here and half
-- there.
--
-- kind is hardcoded 'hosted' rather than parameterized: this query exists to create
-- hosted workers, and CreateWorker (runtime.sql) exists to create external ones. A
-- kind parameter would let a caller quota-check one kind while inserting another —
-- a hazard the count and the insert being separate statements makes MORE reachable,
-- not less, since nothing but this hardcoding ties the two to the same kind.
-- hosted_generation takes its column default (0); nothing in M2 bumps it.
--
-- docker_enabled (PRD #83 M3) is the opt-in for the rootless-DinD sidecar. It is a
-- plain parameter here, not defaulted, because the provision handler decides it from
-- the request and passes an explicit true/false — a false is a real "no sidecar",
-- distinct from an external worker's NULL.
INSERT INTO workers (user_id, name, token_hash, template_declared, kind, hosted_size, docker_enabled)
VALUES (@user_id, @name, @token_hash, @template_declared, 'hosted', @hosted_size, @docker_enabled)
RETURNING *;

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
-- lie about the version: a pod holding T1 after a rotation to T2 fails auth
-- outright.
--
-- QUALIFYING THE DESTROY (@proved_token_hash) is what makes that true of the WRITE
-- and not merely of the auth. Auth proves sha256(presented) = token_hash at T0, but
-- a DB round trip (RegisterWorker) sits between T0 and this statement, so matching
-- on worker_id alone would destroy "whatever is parked NOW": a rotation committing
-- in that gap means a request that proved T1 destroys a freshly parked T2,
-- undelivered, stamping delivered_at over it — the original defect, one layer down.
-- Carrying the hash the caller ACTUALLY PROVED into the subquery makes a mid-flight
-- rotation match zero rows, so T2 stays pending and the T2-bearing pod acks it
-- properly when it registers.
--
-- Load-bearing premise: token_hash and the parked ciphertext are always written by
-- the SAME rotation transaction (an M2 requirement, pinned in the PRD), so "the
-- hash I proved is still current" ≡ "the parked ciphertext is the token I proved".
-- Callers MUST pass the hash from the AUTHENTICATED CONTEXT and never a
-- post-register re-read of the row: RegisterWorker's RETURNING * would already
-- carry the rotated value and defeat this predicate entirely.
--
-- The (ciphertext IS NOT NULL OR delivered_at IS NULL) guard is for IDEMPOTENCE on
-- re-registration — a pod rescheduled onto another node presents the same token
-- again, and that must be a no-op, not a re-stamp. It admits exactly two states:
--   * pending  (ciphertext, no delivered_at) -> the real delivery
--   * expired  (neither)                     -> a late but genuine SELF-HEAL: the
--     Secret was written after all and the pod finally booted (a slow image pull,
--     an ImagePullBackOff that cleared), so the row is corrected to the truth
--     rather than left claiming a strand that a rotation would then "fix" for no
--     reason. Only a proof licenses this transition; a report never could. It
--     survives the hash predicate untouched — the prover still holds the current
--     token, it just booted late.
-- and excludes the already-delivered state, which is where re-registration lands.
--
-- Still scoped to kind='hosted': the handler already checks Kind, so this is
-- defence in depth against a future caller that does not.
UPDATE hosted_worker_tokens SET
    token_ciphertext = NULL,
    delivered_at     = now()
WHERE worker_id = @worker_id
  AND worker_id IN (
      SELECT id FROM workers
      WHERE kind = 'hosted'
        AND token_hash = @proved_token_hash
  )
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

-- name: CordonHostedWorker :execrows
-- Controller cordon-write (PRD #422 M4): mark a hosted worker draining so the claim
-- gate (workersvc.Claim) idles it — it finishes its in-flight runs, then the controller
-- rolls it. COALESCE preserves the ORIGINAL cordon time so a repeat cordon is idempotent
-- and does NOT reset the M5 drain-deadline clock. Scoped to kind='hosted': the controller
-- manages only hosted workers and must never cordon an external one. Rows affected = 0
-- means no such hosted worker exists (the handler answers 404).
UPDATE workers
   SET draining_since = COALESCE(draining_since, now()), updated_at = now()
 WHERE id = @id AND kind = 'hosted';

-- name: UncordonHostedWorker :execrows
-- Controller uncordon-write (issue #458): clear draining_since so a worker that was
-- cordoned on drift but whose drift was then REVERTED (nothing to roll) resumes claiming.
-- draining_since is otherwise cleared ONLY by RegisterWorker on an actual roll, so a
-- reverted-drift worker would stay cordoned forever. Idempotent: NULL->NULL is harmless,
-- so no `draining_since IS NOT NULL` guard — that keeps rows-affected=0 meaning
-- unambiguously "no such hosted worker" (clean 404), not "already clear". kind='hosted'
-- mirrors CordonHostedWorker: never touch an external worker.
UPDATE workers
   SET draining_since = NULL, updated_at = now()
 WHERE id = @id AND kind = 'hosted';
