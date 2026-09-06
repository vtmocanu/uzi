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
--
-- SECURITY HARDENING (PRD #1147 audit): write-once freeze. The guard
-- `codex_secret_id IS NULL OR codex_secret_id = @secret_id` makes the binding
-- immutable once set — the FIRST freeze (NULL) and an identical retry (equal
-- secret) affect the row, but a CONFLICTING second freeze with a DIFFERENT secret
-- affects 0 rows and cannot silently re-point a run at another credential. The audit
-- flagged the unguarded UPDATE: it let a late/duplicate freeze overwrite a run's
-- already-frozen binding, defeating the "decided once, atomically" guarantee.
UPDATE runs
SET codex_secret_id         = @secret_id::uuid,
    codex_auth_mode         = @auth_mode::text,
    codex_secret_label      = @secret_label::text,
    codex_material_revision = @material_revision::bigint,
    -- A late/duplicate freeze replay must NOT clobber an already-frozen account
    -- identity back to NULL: COALESCE keeps the prior non-null value when this call
    -- passes NULL (e.g. a subscription run whose account was frozen by a later
    -- SetRunCodexFrozenIdentity and then a stale binding freeze is re-delivered).
    -- Explicit ::text/::bigint casts on every narg: under pgx's extended protocol the
    -- parameter type of a bare sqlc.narg is undeterminable (it appears only inside
    -- COALESCE / an OR guard, never against a typed column alone), which fails at prepare
    -- with 42P08 ("could not determine data type of parameter"). The casts pin the types
    -- (codex_account_key is TEXT, codex_account_revision is BIGINT) without altering the
    -- write-once semantics.
    codex_account_key       = COALESCE(codex_account_key, sqlc.narg('account_key')::text),
    codex_account_revision  = COALESCE(codex_account_revision, sqlc.narg('account_revision')::bigint),
    updated_at              = now()
WHERE id = @id AND user_id = @user_id
    AND (codex_secret_id IS NULL OR codex_secret_id = @secret_id::uuid)
    -- Write-once identity guard: a replay carrying a DIFFERENT account_key than the
    -- one already frozen affects 0 rows (a NULL incoming key, or an equal one, is
    -- allowed), so a frozen identity can never be silently re-pointed.
    AND (codex_account_key IS NULL OR sqlc.narg('account_key')::text IS NULL OR codex_account_key = sqlc.narg('account_key')::text);

-- name: SetRunCodexFrozenIdentity :execrows
-- Freeze the run's canonical identity + account revision at FIRST link (PRD #1147 M2):
-- once a subscription run resolves its provider account, the account_key (the frozen
-- identity tuple) and account_revision are pinned onto the run so the authority check
-- can later compare run-frozen vs current. Owner-scoped; 0 rows for a foreign run.
--
-- SECURITY HARDENING (PRD #1147 audit): write-once identity freeze. The guard
-- `codex_account_key IS NULL OR codex_account_key = @key` pins the frozen identity
-- tuple immutably at first link — the FIRST freeze (NULL) and an identical retry
-- (equal tuple) succeed, but a conflicting freeze presenting a DIFFERENT identity
-- affects 0 rows, so the run's canonical identity can never be silently repointed to
-- another account after it was frozen. The audit flagged the unguarded UPDATE as the
-- symmetric hole to FreezeRunCodexBinding's.
UPDATE runs
SET codex_account_key      = @key::text,
    codex_account_revision = @rev::bigint,
    updated_at             = now()
WHERE id = @id AND user_id = @user_id
    AND (codex_account_key IS NULL OR codex_account_key = @key::text);

-- name: GetRunCodexAuthContext :one
-- The authority-check read (PRD #1147 M2): the run's FROZEN Codex binding alongside the
-- CURRENT alias/account state, so the service can compare the two and decide whether the
-- run still holds authority over the account. A pure read — it mutates nothing. Joined
-- rows are owner-consistent (state and account must belong to the run's own user), the
-- same self-standing-scope idiom SetRunAnthropicSecret documents. The provider account
-- join is LEFT because an 'api_key' (static openai_api_key) alias has a state row but no
-- account behind it, so its current generation/revision/tuple come back NULL. Filtered
-- by run id only, per the M2 read contract.
--
-- SECURITY HARDENING (PRD #1147 audit): two additive columns feed bind-time checks the
-- source audit found missing.
--   current_coord_state — the account's live coordination state (NULL for an api_key
--   alias, which has no account behind the LEFT JOIN). The release predicate must NOT
--   hand a run authority over an account parked in 'quarantined'/'in_progress'; without
--   this column the release decision could not see the quarantine and could release a
--   possibly-ambiguous account.
--   bound_kind — the alias's own user_secrets.kind, joined owner-scoped (us.user_id =
--   r.user_id) so it can never read a foreign secret's kind. The bind-time kind↔auth-mode
--   check needs the actual kind to reject a binding whose auth_mode contradicts the
--   credential kind (an openai_api_key alias frozen 'subscription', or vice versa). The
--   join is INNER, not LEFT: a run carrying a codex binding always has its alias row (the
--   codex_secret_id FK targets user_secrets) and the existing INNER join to
--   codex_credential_state already requires a state row, so this adds no new NULL case.
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
    cpa.workspace_account_id,
    -- CURRENT account coordination state (NULL for an api_key alias): the release
    -- predicate refuses a run authority over a quarantined/in-progress account.
    cpa.coord_state AS current_coord_state,
    -- The bound alias's own kind, for the bind-time kind↔auth-mode check.
    us.kind AS bound_kind
FROM runs r
JOIN codex_credential_state ccs
    ON ccs.user_secret_id = r.codex_secret_id AND ccs.user_id = r.user_id
JOIN user_secrets us
    ON us.id = r.codex_secret_id AND us.user_id = r.user_id
LEFT JOIN codex_provider_account cpa
    ON cpa.id = ccs.provider_account_id AND cpa.user_id = r.user_id
WHERE r.id = @id;

-- name: SetRunCodexClaimCapability :one
-- Mint (or rotate) the per-claim Codex capability (PRD #1147 M2): store the new hash and
-- bump the epoch so a prior capability is superseded. Guarded on worker_id — only the
-- CURRENTLY-OWNING worker may mint, so a worker that already lost the claim cannot mint a
-- fresh capability. RETURNING codex_claim_epoch hands back the PERSISTED post-bump epoch
-- so the caller wires the capability off the value actually stored, not a re-derived one;
-- a caller that no longer owns the run matches no row → pgx.ErrNoRows. The revocation half
-- lives in every claimed→queued path in runtime.sql (the three Requeue* queries plus
-- SweepClaimedNeverStarted), which clear the hash + bump the epoch on ownership loss.
-- PRD #1147 F7 (defense-in-depth) extends the same revoke to the park/promote paths that
-- likewise leave a run without a live owner: SetRunPoolWait (claimed→pool_wait hold),
-- PromotePoolWaitRun (pool_wait→queued), and PromoteLimitWaitRuns (limit_wait→queued).
-- SetRunLimitWait is INTENTIONALLY EXCLUDED: limit_wait is an actively-claimed status that
-- keeps its live capability by design (persist-before-park), so revoking there would strip
-- a run that still legitimately holds its claim.
UPDATE runs
SET codex_cap_hash    = @hash,
    codex_claim_epoch = codex_claim_epoch + 1,
    updated_at        = now()
WHERE id = @id AND worker_id = @worker_id
RETURNING codex_claim_epoch;

-- name: AcquireCodexRefreshLease :execrows
-- CAS-acquire the refresh lease on a provider account (PRD #1147 M2): move it to
-- 'in_progress' under this operation with a lease deadline, but only from a state where
-- no live lease or quarantine blocks it ('idle' or a prior 'committed') AND the account is
-- STILL at the generation the caller intends to advance from (@from_generation). 0 rows
-- means a live lease or a quarantine already holds the account, OR the generation has
-- already moved — the caller must not proceed.
--
-- The generation guard (PRD #1147 M4) closes the stale-generation redundant-exchange
-- window: without it, a loser op that snapshotted an old generation could acquire the
-- freshly-idle lease AFTER a winner completed a full exchange→commit→reset cycle and then
-- perform a SECOND provider exchange with its now-stale refresh token (its commit would be
-- CAS-rejected, but the provider was already called twice — violating the "exactly ONE
-- rotation per stale-generation burst" invariant D5 and wasting a single-use rotating
-- refresh token). Requiring generation = @from_generation makes that loser's acquire fail
-- (0 rows), so the service re-reads, sees the account already advanced, and reconciles to
-- the committed token with NO provider call. Owner-scoped.
UPDATE codex_provider_account
SET coord_state        = 'in_progress',
    coord_operation_id = @op::uuid,
    lease_deadline     = @deadline::timestamptz,
    updated_at         = now()
WHERE id = @id AND user_id = @user_id AND coord_state IN ('idle', 'committed')
    AND generation = @from_generation::bigint;

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
    -- 00198's CHECK requires (recovery_sealed IS NULL) = (recovery_sealed_with IS NULL),
    -- so clearing the recovery blob MUST also clear its key discriminator or an account
    -- with a populated recovery slot would violate the CHECK (23514) on commit.
    recovery_sealed_with = NULL,
    updated_at           = now()
WHERE id = @id AND user_id = @user_id AND generation = @from_generation::bigint
    AND coord_state = 'in_progress' AND coord_operation_id = @op::uuid
RETURNING generation, coord_state, committed_generation;

-- name: SetCodexRecoverySlot :execrows
-- Protect a previously-good login into the recovery slot and quarantine the account (PRD
-- #1147 M2): the commit-or-persistence-failure path preserves the material needed to roll
-- back and parks the account for reconciliation. Owner-scoped.
--
-- SECURITY HARDENING (PRD #1147 audit): operation-identity guard (coord_operation_id=@op)
-- across the in_progress/quarantined states. The identity guard is what prevents a
-- stale/foreign op from writing: a different op that committed sets coord_operation_id to
-- itself, and RefreshCodexAccountLogin nulls it, so a dead op matches 0 rows either way.
-- Allowing coord_state IN ('in_progress','quarantined') lets the operation that owns the
-- lease protect its own material even after its lease expired and was reaped to
-- 'quarantined' (QuarantineExpiredCodexLease keeps coord_operation_id) — precisely the
-- commit-failure recovery path (handleCodexCommitFailure/codexRetainUnverifiedMaterial).
-- This does NOT weaken the identity guard: without the quarantined state the protect write
-- matched 0 rows after a mid-flight quarantine and silently dropped freshly-rotated
-- material. recovery_sealed_with records which key sealed the protected blob (PRD #1147
-- F14), so a later promotion can open it with the correct key even if the live sealed_login
-- has since migrated master→dek. (Callers in workersvc gain the @op param in a later unit.)
UPDATE codex_provider_account
SET recovery_sealed      = @sealed,
    recovery_generation  = @gen::bigint,
    recovery_sealed_with = @recovery_sealed_with,
    coord_state          = 'quarantined',
    updated_at           = now()
WHERE id = @id AND user_id = @user_id
    AND coord_operation_id = @op::uuid
    AND coord_state IN ('in_progress', 'quarantined');

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

-- name: PromoteCodexRecovery :one
-- Install the protected recovery material as the live login (SECURITY HARDENING, PRD
-- #1147 audit): the reconcile path's roll-forward for a quarantined account whose
-- recovery slot holds a known-good login. It promotes recovery_sealed into sealed_login,
-- advances the generation (and committed_generation) so any stale CAS-guarded writer
-- loses, returns the account to 'idle', and clears the coordination + recovery slots.
--
-- Guarded on coord_state='quarantined' AND recovery_sealed IS NOT NULL AND
-- recovery_generation = @from_generation so ONLY a quarantined account carrying the
-- expected recovery generation is promoted, and the promotion is IDEMPOTENT: a second
-- call finds coord_state != 'quarantined' (already promoted to 'idle') and matches 0
-- rows → pgx.ErrNoRows, so a retried reconcile cannot double-advance the generation. A
-- wrong @from_generation (the recovery slot moved under the caller) likewise matches 0
-- rows. The audit flagged that without a dedicated roll-forward primitive the reconcile
-- path had no CAS-safe way to make the recovery material live.
--
-- sealed_with ADOPTS recovery_sealed_with on promotion (PRD #1147 F14): the recovery
-- blob may have been sealed under a DIFFERENT key than the current sealed_login — a
-- master→dek migration can advance sealed_login's key while the protected recovery blob
-- still carries the older discriminator (or vice versa). Promoting recovery_sealed into
-- the live login therefore MUST also install its recovery_sealed_with, or a later open of
-- the promoted login would try the wrong key and fail to decrypt. The prior assertion
-- that the discriminator is invariant across promotion was wrong for exactly the
-- cross-key-migration window; recovery_sealed_with is cleared alongside the rest of the
-- recovery slot since the material has been consumed.
UPDATE codex_provider_account
SET sealed_login         = recovery_sealed,
    sealed_with          = recovery_sealed_with,
    generation           = generation + 1,
    committed_generation = generation + 1,
    coord_state          = 'idle',
    coord_operation_id   = NULL,
    lease_deadline       = NULL,
    recovery_sealed      = NULL,
    recovery_generation  = NULL,
    recovery_sealed_with = NULL,
    updated_at           = now()
WHERE id = @id AND user_id = @user_id
    AND coord_state = 'quarantined'
    AND recovery_sealed IS NOT NULL
    AND recovery_generation = @from_generation::bigint
RETURNING generation;

-- name: RefreshCodexAccountLogin :execrows
-- Install a freshly-sealed login on an account after a VERIFIED re-login (SECURITY
-- HARDENING, PRD #1147 audit): the recovery path when the recovery slot is empty or
-- untrusted and the service has re-authenticated the subscription out of band. It writes
-- the new sealed_login + sealed_with, advances the generation (and committed_generation)
-- so stale CAS writers lose, returns the account to 'idle', and clears the coordination +
-- recovery slots. Owner-scoped; 0 rows for a foreign account. Unlike PromoteCodexRecovery
-- this is a fresh install (new sealed_with may differ), so sealed_with IS written.
UPDATE codex_provider_account
SET sealed_login         = @sealed,
    sealed_with          = @sealed_with::text,
    generation           = generation + 1,
    committed_generation = generation + 1,
    coord_state          = 'idle',
    coord_operation_id   = NULL,
    lease_deadline       = NULL,
    recovery_sealed      = NULL,
    recovery_generation  = NULL,
    -- 00198's CHECK requires (recovery_sealed IS NULL) = (recovery_sealed_with IS NULL),
    -- so clearing the recovery blob MUST also clear its key discriminator or an account
    -- with a populated recovery slot (e.g. a quarantine re-login) would violate the
    -- CHECK (23514) on this install.
    recovery_sealed_with = NULL,
    updated_at           = now()
WHERE id = @id AND user_id = @user_id;

-- name: QuarantineCodexAccount :execrows
-- Quarantine an in-progress account WITHOUT writing recovery material (SECURITY
-- HARDENING, PRD #1147 audit): the identity-mismatch path parks the account when the
-- provider returned a login for a DIFFERENT identity tuple than expected, so there is no
-- trustworthy prior-good login to protect into the recovery slot (that is
-- SetCodexRecoverySlot's job on the ordinary failure path). Guarded on
-- coord_state='in_progress' AND coord_operation_id=@op so only the operation that still
-- holds the live lease may quarantine it — a presumed-dead or wrong operation matches 0
-- rows and cannot flip a settled account into quarantine. Owner-scoped.
UPDATE codex_provider_account
SET coord_state = 'quarantined', updated_at = now()
WHERE id = @id AND user_id = @user_id
    AND coord_state = 'in_progress' AND coord_operation_id = @op::uuid;

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
