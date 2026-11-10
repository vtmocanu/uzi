-- name: ApplyPolicyHold :one
-- Caller holds the worker/run mutation fence and has reconciled the recorded identity.
WITH held AS (
    UPDATE runs SET status='paused', status_since=now(), updated_at=now(),
        budget_paused_seconds = budget_paused_seconds + CASE
            WHEN status IN ('awaiting_approval','awaiting_input')
            THEN GREATEST(0,EXTRACT(EPOCH FROM (now()-status_since))::int) ELSE 0 END,
        hold_reason='codex_policy_hold',
        -- An existing constrained conversation can only be cleared by owner consent.
        -- Empty/omitted preservation evidence never replaces an established identity.
        session_id=COALESCE(
            CASE WHEN policy_preserve_required OR policy_pending_fresh
                OR (policy_outcome_id IS NOT NULL AND policy_resolved_at IS NULL)
                THEN NULLIF(session_id,'') END,
            NULLIF(sqlc.narg('session_id')::text,''),session_id),
        hold_captured_head=sqlc.narg('head')::text,
        -- Required-recovery identity (#3607 M2a). Every expression below reads the PRE-update row.
        -- Initial set: the first nonempty reported head becomes R. Supersede: R is replaced by the
        -- reported head only with publication evidence AND a verification record bound to THIS claim
        -- generation and to the current R; anything else keeps R.
        policy_required_head=CASE
            WHEN policy_required_head IS NULL AND NULLIF(sqlc.narg('head')::text,'') IS NOT NULL
                THEN sqlc.narg('head')::text
            WHEN @published::boolean AND COALESCE(sqlc.narg('checkpoint_contains_latest')::boolean,false)
                AND NULLIF(sqlc.narg('head')::text,'') IS NOT NULL
                AND policy_recovery_verified_generation = @claim_generation
                AND policy_recovery_verified_head = policy_required_head
                THEN sqlc.narg('head')::text
            ELSE policy_required_head END,
        policy_recovery_verified_head=CASE
            WHEN policy_required_head IS NOT NULL AND @published::boolean
                AND COALESCE(sqlc.narg('checkpoint_contains_latest')::boolean,false)
                AND NULLIF(sqlc.narg('head')::text,'') IS NOT NULL
                AND policy_recovery_verified_generation = @claim_generation
                AND policy_recovery_verified_head = policy_required_head
                THEN sqlc.narg('head')::text
            ELSE policy_recovery_verified_head END,
        policy_hold_cause= @cause::text,
        checkpoint_contains_latest=sqlc.narg('checkpoint_contains_latest')::boolean,
        policy_hold_published= @published::boolean,
        policy_preserve_required=true,policy_pending_fresh=false,
        -- A new hold retires any one-claim Fresh history: the owner decides again from this episode.
        policy_fresh_consumed_generation=NULL,policy_fresh_confirmed_generation=NULL,
        policy_outcome_id= @outcome_id,policy_continuation_mode= @continuation_mode,
        policy_resolved_at=NULL,policy_applying_worker_id= @worker_id,
        policy_applying_generation= @claim_generation,policy_source_generation= @source_generation,
        policy_refusal_count=CASE WHEN @actual_refusal::boolean
            THEN LEAST(policy_refusal_count::bigint+1,2147483647)::integer ELSE policy_refusal_count END,
        policy_refusal_tag=CASE WHEN @actual_refusal::boolean THEN sqlc.narg('policy_tag')::text ELSE policy_refusal_tag END,
        policy_refusal_phase=CASE WHEN @actual_refusal::boolean THEN sqlc.narg('phase')::text ELSE policy_refusal_phase END,
        policy_refusal_correlation_id=CASE WHEN @actual_refusal::boolean THEN sqlc.narg('correlation_id')::text ELSE policy_refusal_correlation_id END,
        pause_requested_at=NULL,pause_mode=NULL,pause_after_count=NULL,
        health='ok',health_reason=NULL,health_since=NULL
    WHERE runs.id= @id AND runs.worker_id= @worker_id AND runs.user_id= @user_id
      AND status IN ('claimed','running') AND claim_released_at IS NULL
      AND claim_generation= @claim_generation AND stop_kind IS NULL
      AND kind IN ('issue','task','prompt','self_improve') AND NOT interactive AND harness='codex'
      AND (policy_applying_generation IS NULL OR policy_applying_generation < @claim_generation)
    RETURNING runs.*
), settled AS (
    UPDATE run_user_inputs SET consumed_at=COALESCE(consumed_at,now()),applied_at=now()
    WHERE run_id IN (SELECT id FROM held) AND kind IN ('pause','pause_cancel') AND applied_at IS NULL
)
SELECT * FROM held;

-- name: ResumePolicyHold :one
-- Owner resume of ONE unresolved codex_policy_hold episode (#3607 M2a). The caller holds the run
-- row lock in a read-committed transaction; every predicate is repeated here so a stale request
-- matches zero rows. Parked-time and queue accounting mirror ResumePausedRun (gate-park
-- accounting: started_at kept, parked wall time banked, affinity and generation kept, codex
-- capability revoked, health reset).
-- fresh_from_saved_work clears the active session, ends the preserving constraint for this
-- episode (the old session id survives in the caller's audit row) and the recorded verification
-- pair, but KEEPS policy_required_head: the next claim must still prove the saved work contains it
-- (policy_saved_work_head on the claim) before the head is cleared by RecordPolicyRecoveryVerified's
-- non-preserving arm. preserve keeps the session, preserve_required, the required head and the
-- verification pair. Outcome, applying and source facts stay in both.
-- fresh_one_claim (same_conversation episodes only, #3607 Mf) clears the active session like
-- fresh_from_saved_work but KEEPS preserve_required, the required head and the verification pair,
-- and arms policy_pending_fresh: ClaimRun consumes it exactly once. Every mode retires prior
-- one-claim history (both generation stamps NULL).
UPDATE runs SET
    status = 'queued',
    status_since = now(),
    budget_paused_seconds = runs.budget_paused_seconds
        + GREATEST(0, EXTRACT(EPOCH FROM (now() - runs.status_since))::int),
    worker_id = CASE WHEN runs.claim_released_at IS NOT NULL THEN NULL ELSE runs.worker_id END,
    hold_reason = NULL,
    session_id = CASE WHEN @mode::text IN ('fresh_from_saved_work','fresh_one_claim') THEN NULL ELSE runs.session_id END,
    policy_preserve_required = CASE WHEN @mode::text = 'fresh_from_saved_work' THEN false ELSE runs.policy_preserve_required END,
    policy_pending_fresh = (@mode::text = 'fresh_one_claim'),
    policy_fresh_consumed_generation = NULL,
    policy_fresh_confirmed_generation = NULL,
    policy_recovery_verified_generation = CASE WHEN @mode::text = 'fresh_from_saved_work' THEN NULL ELSE runs.policy_recovery_verified_generation END,
    policy_recovery_verified_head = CASE WHEN @mode::text = 'fresh_from_saved_work' THEN NULL ELSE runs.policy_recovery_verified_head END,
    policy_resolved_at = now(),
    codex_cap_hash = NULL, codex_claim_epoch = runs.codex_claim_epoch + 1,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE runs.id = @id AND runs.user_id = @user_id
  AND runs.status = 'paused'
  AND runs.hold_reason = 'codex_policy_hold'
  AND runs.stop_kind IS NULL
  AND runs.policy_outcome_id = @expected_episode_id
  AND runs.policy_resolved_at IS NULL
  AND ((@mode::text = 'preserve' AND runs.policy_continuation_mode = 'same_conversation')
    OR (@mode::text = 'fresh_one_claim' AND runs.policy_continuation_mode = 'same_conversation')
    OR (@mode::text = 'fresh_from_saved_work' AND runs.policy_continuation_mode = 'fresh_from_saved_work'))
RETURNING runs.*;

-- name: RecordPolicyRecoveryVerified :one
-- Record the worker's local verification of the required recovery head for ONE claim generation
-- (#3607 M2a). The caller holds the worker then run locks. This never changes policy_required_head;
-- it only lets a later same-generation published report supersede it.
UPDATE runs SET
    policy_recovery_verified_generation = @claim_generation,
    policy_recovery_verified_head = @required_head,
    updated_at = now()
WHERE runs.id = @id AND runs.user_id = @user_id AND runs.worker_id = @worker_id
  AND runs.claim_released_at IS NULL
  AND runs.claim_generation = @claim_generation
  AND runs.status IN ('claimed','running')
  AND runs.policy_preserve_required
  AND runs.policy_required_head = @required_head
RETURNING runs.id;

-- name: ClearPolicySavedWorkHead :one
-- Non-preserving arm of the worker's verification of the required recovery head (#3607). A run whose
-- episode was resolved with fresh_from_saved_work is no longer preserve_required but still carries
-- policy_required_head; once the worker holding the exact current claim has verified locally that the
-- saved work contains that head, the requirement and the verification pair are cleared in one locked
-- UPDATE. The caller holds the worker then run locks; every fence is repeated here.
UPDATE runs SET
    policy_required_head = NULL,
    policy_recovery_verified_generation = NULL,
    policy_recovery_verified_head = NULL,
    updated_at = now()
WHERE runs.id = @id AND runs.user_id = @user_id AND runs.worker_id = @worker_id
  AND runs.claim_released_at IS NULL
  AND runs.claim_generation = @claim_generation
  AND runs.status IN ('claimed','running')
  AND NOT runs.policy_preserve_required
  AND runs.policy_required_head = @required_head
RETURNING runs.id;

-- name: PolicyResumeAuditExists :one
-- True when the run already carries a resume audit row whose body starts with the exact
-- policy-resume prefix (mode and episode id). Used only to recognise a lost-response retry.
SELECT EXISTS (
    SELECT 1 FROM run_user_inputs
    WHERE run_id = @run_id AND kind = 'resume' AND body IS NOT NULL
      AND starts_with(body, @body_prefix::text)
)::boolean;
