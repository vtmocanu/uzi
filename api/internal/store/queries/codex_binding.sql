-- Codex per-run claim binding + coordinated-refresh primitives (PRD #1147 M2),
-- STORE/SCHEMA only — ships DARK. The service layer drives the refresh state machine
-- and the authority check; this file only provides the owner-scoped SQL primitives.
-- Companion schema: 00201 (runs.codex_* columns), 00200 (codex_refresh_intent),
-- 00198/00199 (codex_provider_account / codex_credential_state).

-- name: FreezeRunCodexBinding :execrows
-- Freeze a run's Codex binding at creation (PRD #1147 M2), via an internal create path
-- rather than the public CreateRun — the binding is decided once, atomically, when the
-- claim is assembled. Records the alias id, auth mode, snapshotted label and the alias
-- material_revision frozen at creation; the account identity/revision are OPTIONAL here
-- (nullable args) because a subscription run may not have resolved its account yet —
-- SetRunCodexFrozenIdentity freezes those at first link. Owner-scoped; 0 rows for a
-- run this user does not own.
UPDATE runs
SET codex_secret_id         = @secret_id::uuid,
    codex_auth_mode         = @auth_mode::text,
    codex_secret_label      = @secret_label::text,
    codex_material_revision = @material_revision::bigint,
    codex_account_key       = sqlc.narg('account_key'),
    codex_account_revision  = sqlc.narg('account_revision'),
    updated_at              = now()
WHERE id = @id AND user_id = @user_id;

-- name: SetRunCodexFrozenIdentity :execrows
-- Freeze the run's canonical identity + account revision at FIRST link (PRD #1147 M2):
-- once a subscription run resolves its provider account, the account_key (the frozen
-- identity tuple) and account_revision are pinned onto the run so the authority check
-- can later compare run-frozen vs current. Owner-scoped; 0 rows for a foreign run.
UPDATE runs
SET codex_account_key      = @key::text,
    codex_account_revision = @rev::bigint,
    updated_at             = now()
WHERE id = @id AND user_id = @user_id;

-- name: GetRunCodexAuthContext :one
-- The authority-check read (PRD #1147 M2): the run's FROZEN Codex binding alongside the
-- CURRENT alias/account state, so the service can compare the two and decide whether the
-- run still holds authority over the account. A pure read — it mutates nothing. Joined
-- rows are owner-consistent (state and account must belong to the run's own user), the
-- same self-standing-scope idiom SetRunAnthropicSecret documents. The provider account
-- join is LEFT because an 'api_key' (static openai_api_key) alias has a state row but no
-- account behind it, so its current generation/revision/tuple come back NULL. Filtered
-- by run id only, per the M2 read contract.
SELECT
    r.codex_secret_id,
    r.codex_auth_mode,
    r.codex_account_key,
    r.codex_material_revision,
    r.codex_account_revision,
    r.codex_claim_epoch,
    r.codex_cap_hash,
    r.worker_id,
    r.status,
    -- CURRENT alias material revision (the run-frozen one is r.codex_material_revision).
    ccs.material_revision AS current_material_revision,
    -- CURRENT account counters + immutable identity tuple (NULL for an api_key alias).
    cpa.generation          AS current_generation,
    cpa.credential_revision AS current_credential_revision,
    cpa.provider_user_id,
    cpa.workspace_account_id
FROM runs r
JOIN codex_credential_state ccs
    ON ccs.user_secret_id = r.codex_secret_id AND ccs.user_id = r.user_id
LEFT JOIN codex_provider_account cpa
    ON cpa.id = ccs.provider_account_id AND cpa.user_id = r.user_id
WHERE r.id = @id;

-- name: SetRunCodexClaimCapability :execrows
-- Mint (or rotate) the per-claim Codex capability (PRD #1147 M2): store the new hash and
-- bump the epoch so a prior capability is superseded. Guarded on worker_id — only the
-- CURRENTLY-OWNING worker may mint, so a worker that already lost the claim cannot mint a
-- fresh capability. 0 rows when the caller is not the owning worker. The revocation half
-- lives in every claimed→queued path in runtime.sql (the three Requeue* queries plus
-- SweepClaimedNeverStarted), which clear the hash + bump the epoch on ownership loss.
UPDATE runs
SET codex_cap_hash    = @hash,
    codex_claim_epoch = codex_claim_epoch + 1,
    updated_at        = now()
WHERE id = @id AND worker_id = @worker_id;

-- name: AcquireCodexRefreshLease :execrows
-- CAS-acquire the refresh lease on a provider account (PRD #1147 M2): move it to
-- 'in_progress' under this operation with a lease deadline, but only from a state where
-- no live lease or quarantine blocks it ('idle' or a prior 'committed'). 0 rows means a
-- live lease or a quarantine already holds the account — the caller must not proceed.
-- Owner-scoped.
UPDATE codex_provider_account
SET coord_state        = 'in_progress',
    coord_operation_id = @op::uuid,
    lease_deadline     = @deadline::timestamptz,
    updated_at         = now()
WHERE id = @id AND user_id = @user_id AND coord_state IN ('idle', 'committed');

-- name: QuarantineExpiredCodexLease :execrows
-- Reap an expired lease (PRD #1147 M2): an 'in_progress' account whose lease_deadline has
-- passed is moved to 'quarantined' so a survivor can reconcile it rather than steal a
-- possibly-still-live lease. 0 rows when the lease is not expired (or the account is not
-- in_progress). Owner-scoped.
UPDATE codex_provider_account
SET coord_state = 'quarantined', updated_at = now()
WHERE id = @id AND user_id = @user_id
  AND coord_state = 'in_progress' AND lease_deadline < @now::timestamptz;

-- name: CommitCodexRefresh :one
-- CAS-commit a completed refresh (PRD #1147 M2): advance the generation and install the
-- new sealed login ONLY IF the account is still at the generation the refresh started
-- from (@from_generation), which is what makes a lost/duplicate refresher's stale commit
-- a no-op. committed_generation records the generation this commit produced, and the
-- recovery slot is cleared (the commit succeeded, so there is nothing to roll back to).
-- The immutable identity tuple (provider_user_id/workspace_account_id) is deliberately
-- NOT touched. pgx.ErrNoRows means the CAS was lost — EITHER the generation moved OR this
-- operation no longer holds a live lease. The coord_state='in_progress' AND
-- coord_operation_id=@op guard is load-bearing (PRD #1147 M2, B6 §9): a lease that expired
-- and was moved to 'quarantined' must NOT be revivable into a commit by its presumed-dead
-- refresher — that would silently bypass the quarantine (clearing the recovery slot and
-- flipping quarantined→committed) exactly when the rotation outcome is ambiguous. Only the
-- operation that still owns the in_progress lease may commit. Owner-scoped.
UPDATE codex_provider_account
SET generation           = generation + 1,
    sealed_login         = @sealed,
    sealed_with          = @sealed_with::text,
    coord_state          = 'committed',
    committed_generation = generation + 1,
    coord_operation_id   = @op::uuid,
    recovery_sealed      = NULL,
    recovery_generation  = NULL,
    updated_at           = now()
WHERE id = @id AND user_id = @user_id AND generation = @from_generation::bigint
    AND coord_state = 'in_progress' AND coord_operation_id = @op::uuid
RETURNING generation, coord_state, committed_generation;

-- name: SetCodexRecoverySlot :execrows
-- Protect a previously-good login into the recovery slot and quarantine the account (PRD
-- #1147 M2): the commit-or-persistence-failure path preserves the material needed to roll
-- back and parks the account for reconciliation. Owner-scoped.
UPDATE codex_provider_account
SET recovery_sealed     = @sealed,
    recovery_generation = @gen::bigint,
    coord_state         = 'quarantined',
    updated_at          = now()
WHERE id = @id AND user_id = @user_id;

-- name: ResetCodexCoordIdle :execrows
-- Return a 'committed' account to 'idle' (PRD #1147 M2) once the committed token has been
-- distributed, clearing the operation id and lease deadline so the account is ready for
-- the next refresh cycle. Guarded on coord_state='committed' so it only ever advances a
-- settled account. Owner-scoped.
UPDATE codex_provider_account
SET coord_state        = 'idle',
    coord_operation_id = NULL,
    lease_deadline     = NULL,
    updated_at         = now()
WHERE id = @id AND user_id = @user_id AND coord_state = 'committed';

-- name: InsertCodexRefreshIntent :one
-- Record the durable pre-rotation intent (PRD #1147 M2) before touching the provider. The
-- operation_id PK makes a duplicate insert of the same operation fail (23505), so
-- idempotence is enforced by the service reading the existing intent first rather than by
-- blind retry. Born 'rotating'. Owner-scoped via the composite FK to the account.
INSERT INTO codex_refresh_intent (operation_id, user_id, provider_account_id, from_generation, state)
VALUES (@operation_id, @user_id, @provider_account_id, @from_generation::bigint, 'rotating')
RETURNING *;

-- name: GetCodexRefreshIntent :one
-- Fetch one intent by operation id (PRD #1147 M2), owner-scoped so a foreign operation id
-- reads no row. pgx.ErrNoRows means no such intent for this user.
SELECT * FROM codex_refresh_intent
WHERE operation_id = @operation_id AND user_id = @user_id;

-- name: SetCodexRefreshIntentState :execrows
-- Advance an intent's state (PRD #1147 M2) as the service drives the rotation state
-- machine ('rotating' → 'committed'/'unrecoverable'/'reconciled'). Owner-scoped; 0 rows
-- for a foreign or missing operation.
UPDATE codex_refresh_intent
SET state = @state, updated_at = now()
WHERE operation_id = @operation_id AND user_id = @user_id;

-- name: ListUnresolvedCodexRefreshIntents :many
-- The recovery scan (PRD #1147 M2): every still-'rotating' intent for an account, which a
-- survivor reconciles against the account's actual generation. Owner-scoped; backed by
-- the partial index idx_codex_refresh_intent_unresolved (00200).
SELECT * FROM codex_refresh_intent
WHERE user_id = @user_id AND provider_account_id = @provider_account_id AND state = 'rotating';
