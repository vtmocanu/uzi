package workersvc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// credential_switch.go is the SERVER side of the held-state credential-switch protocol's
// WORKER-facing half (PRD #1247 M5, D3/D4/D14): the generation fence's arm-selection policy and
// the `credential_switch` RELEASE transition a worker reports after its local two-phase release.
// The USER-facing verb (`uzi run set-token`, which writes the override + the switch stamp) lives
// in run_credential.go; the fence's atomic FOR UPDATE wrapper is in SetState (service.go).

// stateUsesGenerationFence reports whether a worker-driven transition PARTICIPATES in the
// released-generation fence (PRD #1247 M5, D3) — i.e. whether a CAPABILITY worker must stamp
// claim_generation on it (fail-closed; SetState rejects an omission with ErrMissingClaimGeneration)
// and whether a STALE (released/superseded) report is rejected. It is TRUE for every
// single-statement mutating arm — running, awaiting_approval, awaiting_input, awaiting_followup,
// paused, failed — for the two multi-step PARK arms — limit_wait, recovery_wait (M5a-1 rework,
// reviewer NB1: EVERY worker-driven mutating transition is fenced) — and for a LEGACY completion
// (a single fenced SetRunCompleted UPDATE).
//
// The fence MECHANISM differs by arm, which is why stateUsesForUpdateFence exists alongside this:
//
//   - running / awaiting_* / paused / failed / legacy-completed: fenced by SetState's FOR UPDATE
//     wrapper (stateUsesForUpdateFence is TRUE for exactly these — its mutation runs through the
//     tx-bound querier while the row lock is held from the generation check through the write).
//   - limit_wait / recovery_wait: fenced PER-QUERY inside SetRunLimitWait / SetRunRecoveryWait —
//     the reported generation is threaded through setLimitWait / setRecoveryWait and added to the
//     WHERE with the same nil-guarded shape as InsertRunMessage. They do NOT use the FOR UPDATE
//     wrapper (stateUsesForUpdateFence is FALSE for them): their candidate-read + pure decision +
//     write is multi-step, and holding the FOR UPDATE lock across it while the helper writes on
//     s.q (a different connection) would self-deadlock on the run row.
//
// It is FALSE for:
//
//   - completed on an INTERLOCKED run: completeRunWithPermitLease runs its OWN permit transaction,
//     which must NOT nest under a FOR UPDATE (self-deadlock on the run row); it is
//     generation-fenced INSIDE that lock instead — including the fail-closed capability check —
//     and service.go's completed arm surfaces its ErrStaleClaim / ErrMissingClaimGeneration.
//   - credential_switch: handled by its own fenced+idempotent ReleaseCredentialSwitch statement
//     (releaseCredentialSwitch), which itself requires a stamped generation and must NOT go
//     through the generic stale check — a redelivered same-generation release after the release
//     applied is idempotent success, not stale.
//   - credential_switch_failed: handled before the fence by its own fenced+idempotent
//     ClearCredentialSwitchByWorker statement (failCredentialSwitch) — the bounded capture-failure
//     give-up (D14) CLEARS a pending switch stamp WITHOUT changing status, so it is not a transition
//     and must not go through the generic stale check (like credential_switch / pause_failed).
//   - pause_failed: handled before the fence (it withdraws a pending pause, not a transition).
//
// Legacy generation-zero chats retain the PRD #1247 exemption, including supplied mismatches.
// Fresh chat claims advance the generation, so supplied generations use the normal fence.
// Chat reports may still omit their generation for compatibility with the chat runner.
func stateUsesGenerationFence(state string, owned store.Run) bool {
	if owned.Kind == runkind.Chat && owned.ClaimGeneration == 0 {
		return false
	}
	switch state {
	case "running", "awaiting_approval", "awaiting_input", "awaiting_followup", "paused", "failed",
		"limit_wait", "recovery_wait":
		return true
	case "completed":
		return !owned.CompletionContractVersion.Valid
	default:
		return false
	}
}

// stateUsesForUpdateFence reports whether SetState wraps a transition in the FOR UPDATE
// generation-fence transaction. It is the subset of stateUsesGenerationFence that fences through
// that OUTER lock — everything EXCEPT limit_wait / recovery_wait, whose multi-step park helpers
// fence PER-QUERY (see stateUsesGenerationFence) and would self-deadlock under the lock. The
// interlocked-completed, credential_switch and credential_switch_failed arms already return false
// from stateUsesGenerationFence (each is dispatched by its own fenced statement before the generic
// fence), so they need no exclusion here.
func stateUsesForUpdateFence(state string, owned store.Run) bool {
	switch state {
	case "limit_wait", "recovery_wait":
		return false
	default:
		return stateUsesGenerationFence(state, owned)
	}
}

// releaseCredentialSwitch applies the held-state credential-switch RELEASE transition (PRD #1247
// M5, D3/D4/D14): a worker's {status:"credential_switch", claim_generation} report requeues the
// held run so its NEXT claim spends the already-written override. It returns SetState's
// (run, applied, err) triple; the caller (SetState) invokes it BEFORE the generic generation
// fence so an idempotent redelivery is not mistaken for a stale mutation.
//
// The release itself is ONE fenced statement (ReleaseCredentialSwitch): it requires the run be at
// the reported generation, UNRELEASED, owned by this worker, and in a HELD state, then sets
// status='queued', claim_released_at=now(), banks the held gap (Open Question 3), and KEEPS the
// switch stamp (visible as "released, awaiting reclaim", D14).
//
// IDEMPOTENT REDELIVERY (the retained-retry, D14): a lost ack makes the worker resend the same
// release. On a 0-row result we re-read the run and CONVERGE instead of writing a stale rejection:
//
//   - claim_released_at set at the SAME generation → the release already applied → idempotent
//     success; the flight's release is done.
//   - the generation advanced PAST the reported one → already reclaimed → idempotent success.
//   - otherwise (an unexpected state: terminal, or never in a held state at this generation) →
//     applied=false, a 409 that changed nothing, so the worker learns the run's real state.
func (s *Service) releaseCredentialSwitch(ctx context.Context, owned store.Run, wkr store.Worker, req StateRequest) (store.Run, bool, error) {
	if req.ClaimGeneration == nil {
		// A capability worker ALWAYS stamps the generation on a release; without it the release
		// cannot fence to an exact claim, so this is an invalid report (→ 400), not a silent no-op.
		return store.Run{}, false, fmt.Errorf("%w: credential_switch requires claim_generation", ErrInvalidState)
	}
	// Issue #4201 m3a: a worker whose CURRENT registration negotiated in-place apply never releases
	// a claim for a switch; the v1 release is refused with nothing written.
	if retired, err := s.applyNegotiated(ctx, wkr.ID); err != nil {
		return store.Run{}, false, err
	} else if retired {
		return owned, false, ErrCredentialSwitchReleaseRetired
	}
	gen := *req.ClaimGeneration
	rows, err := s.q.ReleaseCredentialSwitch(ctx, store.ReleaseCredentialSwitchParams{
		ID:         owned.ID,
		WorkerID:   pgconv.UUID(wkr.ID),
		Generation: gen,
	})
	if err != nil {
		return store.Run{}, false, err
	}
	if rows == 0 {
		// Classify the 0-row by a worker-scoped re-read. A reclaim by a DIFFERENT worker would
		// already have failed the top-of-SetState ownership read, so a still-owned run here is
		// either already-released (same generation) or reclaimed by THIS worker (generation
		// advanced) — both idempotent successes — or an unexpected state.
		cur, rerr := s.runOwnedByWorker(ctx, owned.ID, wkr)
		if rerr != nil {
			return store.Run{}, false, rerr
		}
		switch {
		case cur.ClaimReleasedAt.Valid && cur.ClaimGeneration == gen:
			// Already released at this generation — a redelivered ack. Idempotent success.
			return cur, true, nil
		case cur.ClaimGeneration > gen:
			// Already reclaimed past this generation — the release completed and a fresh claim
			// opened. Idempotent success.
			return cur, true, nil
		default:
			// Unexpected state (terminal, or never in a held state at this generation): the
			// release neither applied nor is idempotently done. applied=false → 409.
			return cur, false, nil
		}
	}
	run, err := s.runOwnedByWorker(ctx, owned.ID, wkr)
	if err != nil {
		return store.Run{}, false, err
	}
	// The run is back to 'queued'; publish the transition through the same broadcaster/notifier
	// fan-out the other park->queued transitions use so the web and Slack observers see it.
	if s.bcast != nil {
		s.bcast.PublishState(owned.ID, run.Status)
	}
	s.notify(owned.ID, run.Status)
	return run, true, nil
}

// failCredentialSwitch applies the bounded capture-failure GIVE-UP (PRD #1247 M5, D3 step 3 / D14):
// after a bounded number of failed verified-restore-point captures the worker abandons the switch,
// reports {status:"credential_switch_failed", claim_generation}, and the server CLEARS the pending
// switch stamp WITHOUT changing status — the run keeps running on its CURRENT token. It is the
// exact analog of pause_failed for a pending SWITCH (a withdrawal, not a transition), so SetState
// dispatches it BEFORE the generic generation fence.
//
// The clear is ONE fenced statement (ClearCredentialSwitchByWorker): it requires the run be at the
// reported generation, UNRELEASED, owned by this worker, and carry a stamp actually pending. A
// stale/superseded/foreign report, a report after release/reclaim, or an idempotent redelivery
// after the clear already applied all match 0 rows.
//
//   - 0 rows → re-read the run owned-by-worker and return (cur, false, nil): a benign no-op that
//     the handler renders as the ordinary 409 (nothing was pending for this claim).
//   - non-zero → re-read, emit a best-effort observability note (the note NEVER fails the report;
//     the stamp CLEAR is the load-bearing behavior), and return (run, true, nil).
//
// The op-carrying form additionally requires the current registration nonce (see the clear below).
//
// ACCEPTED TRADEOFF (matches pause_failed, for the op-less v1 form): the clear carries no per-request discriminator, so a
// lost-ack REDELIVERY of a give-up at the same still-held claim generation can clear a switch the
// owner RE-REQUESTED in the window between the first give-up and the redelivery — the re-request is
// silently withdrawn and the owner re-issues it. This mirrors ClearPauseRequest / pause_failed
// (which fences on worker_id alone), is liveness-only (no auth/tenant/secret impact), and closing
// it would need a stamp nonce (a schema column) out of proportion to the edge. Deliberate, not an
// oversight.
func (s *Service) failCredentialSwitch(ctx context.Context, owned store.Run, wkr store.Worker, req StateRequest) (store.Run, bool, error) {
	if req.ClaimGeneration == nil {
		// A capability worker ALWAYS stamps the generation (the verb targets a specific claim);
		// without it the clear cannot fence to an exact claim, so this is an invalid report (→ 400),
		// not a silent no-op.
		return store.Run{}, false, fmt.Errorf("%w: credential_switch_failed requires claim_generation", ErrInvalidState)
	}
	gen := *req.ClaimGeneration
	opID, err := optionalSwitchOpID(req.OpID, stateCredentialSwitchFailed)
	if err != nil {
		return store.Run{}, false, err
	}
	// The op-carrying form is fenced to the current registration incarnation (issue #4201), like
	// credential_switch_uncertain: a superseded, missing or malformed nonce matches 0 rows below (the
	// ordinary not-applied 409 that writes nothing; a NULL nonce never equals the stored one). The op-less
	// v1 form carries none.
	var registerNonce pgtype.Text
	if opID.Valid && validHoldRegisterNonce(req.RegisterNonce) {
		registerNonce = pgtype.Text{String: req.RegisterNonce, Valid: true}
	}
	rows, err := s.q.ClearCredentialSwitchByWorker(ctx, store.ClearCredentialSwitchByWorkerParams{
		ID:            owned.ID,
		WorkerID:      pgconv.UUID(wkr.ID),
		Generation:    gen,
		OpID:          opID,
		RegisterNonce: registerNonce,
	})
	if err != nil {
		return store.Run{}, false, err
	}
	if rows == 0 {
		// Nothing was pending to clear for this claim — a benign no-op (an idempotent redelivery
		// after the clear already applied, a stale/superseded generation, or a report after the
		// claim was released/reclaimed). Re-read worker-scoped so the worker learns the run's real
		// state; applied=false → the handler's ordinary 409.
		cur, rerr := s.runOwnedByWorker(ctx, owned.ID, wkr)
		if rerr != nil {
			return store.Run{}, false, rerr
		}
		return cur, false, nil
	}
	run, err := s.runOwnedByWorker(ctx, owned.ID, wkr)
	if err != nil {
		return store.Run{}, false, err
	}
	// Best-effort note that the switch was abandoned. This is NOT a status transition — the run
	// stays running on its old token — so it posts no Slack line (like pause_failed); it only logs
	// for observability and re-broadcasts the (unchanged) status so the web drops the "switch
	// requested" chip now that the stamp is cleared. The worker's own credential_switch_failed feed
	// message (D14) carries the human-readable reason to the run's activity feed.
	// The transactional SetState parent logs committed abandonment. This helper
	// cannot announce success before that transaction commits.
	if s.bcast != nil {
		s.bcast.PublishState(owned.ID, run.Status)
	}
	return run, true, nil
}

// Credential-switch hold hold-reason values (issue #3181). A held-state switch whose local release
// cannot be proven settled is reported as credential_switch_held: the run PAUSES (the claim is not
// released) with hold_reason holdReasonCredentialSwitchSettling while the worker finishes its own
// operations (it does not prove the run's processes stopped). The worker's credential_switch_hold_settled acknowledgement flips it to
// holdReasonCredentialSwitchUnsettled, from which only the owner's resume (or set-token) moves the
// run on.
const (
	holdReasonCredentialSwitchSettling  = "credential_switch_settling"     //nolint:gosec // G101: a run hold-reason VOCABULARY value (the server enum), not a credential.
	holdReasonCredentialSwitchUnsettled = "credential_switch_unsettled"    //nolint:gosec // G101: a run hold-reason VOCABULARY value (the server enum), not a credential.
	stateCredentialSwitchFailed         = "credential_switch_failed"       //nolint:gosec // G101: a worker state-report VOCABULARY value (the wire enum), not a credential.
	stateCredentialSwitchUncertain      = "credential_switch_uncertain"    //nolint:gosec // G101: a worker state-report VOCABULARY value (the wire enum), not a credential.
	stateCredentialSwitchHeld           = "credential_switch_held"         //nolint:gosec // G101: a worker state-report VOCABULARY value (the wire enum), not a credential.
	stateCredentialSwitchHoldSettled    = "credential_switch_hold_settled" //nolint:gosec // G101: a worker state-report VOCABULARY value (the wire enum), not a credential.
)

// ErrRunHoldSettling is the owner-facing refusal of a resume or set-token on a run whose
// credential_switch_settling hold is still fenced: its worker is alive, has not re-registered since
// the hold and is still finishing its own operations for the run (issue #3181). The text is the operator-facing
// 409 message.
var ErrRunHoldSettling = errors.New("the worker is still finishing its own operations for this run; try Resume again shortly, or Cancel the run")

// HeartbeatStaleCutoff is the heartbeat instant before which a worker counts as stale: now minus
// the WorkerHeartbeatStale window the stale-worker requeue sweep uses. A non-positive window yields
// an INVALID (NULL) cutoff, which the settling fence (ResumePausedRun, CredentialSwitchHoldSettling)
// reads as "unknown" and keeps CLOSED rather than treating every worker as stale. The global
// stale-sweep does not consume this value.
func HeartbeatStaleCutoff(now time.Time, window time.Duration) pgtype.Timestamptz {
	if window <= 0 {
		return pgtype.Timestamptz{}
	}
	return pgconv.Time(now.Add(-window))
}

// staleWorkerCutoff is HeartbeatStaleCutoff over the service clock and configured window.
func (s *Service) staleWorkerCutoff() pgtype.Timestamptz {
	return HeartbeatStaleCutoff(s.now(), s.p.WorkerHeartbeatStale)
}

// runHoldSettling reports whether the run's credential_switch_settling fence is up (the predicate
// ResumePausedRun refuses on).
func (s *Service) runHoldSettling(ctx context.Context, runID uuid.UUID) (bool, error) {
	return s.q.CredentialSwitchHoldSettling(ctx, store.CredentialSwitchHoldSettlingParams{
		RunID: runID, StaleCutoff: s.staleWorkerCutoff(),
	})
}

// ErrCredentialSwitchReleaseRetired refuses a v1 credential_switch release, or an un-fenced
// credential_switch_held, from a worker whose current registration negotiated in-place apply
// (issue #4201 m3a). Nothing was written. The handler answers 409 with reason
// "credential_switch_release_retired". A negotiated agent never sends either: for a switch it cannot
// apply in place it takes the owner pause or the op-fenced hold instead.
var ErrCredentialSwitchReleaseRetired = errors.New("credential switch release is retired for an in-place apply worker")

// CredentialSwitchApplyFeature is the /protocol feature this api advertises for in-place apply.
const CredentialSwitchApplyFeature = "credential_switch_apply_v1" //nolint:gosec // G101: a protocol feature VOCABULARY name, not a credential.

// stampApplyNonce stamps workers.credential_switch_apply_nonce with the registration nonce when the
// worker advertised credential_switch_in_place_v1 and this api serves CredentialSwitchApplyFeature
// (always, so only the capability is checked). It returns the column value for the registered row.
// An older api replica's registration rewrites snapshot_register_nonce without stamping, which
// leaves the two differing, so applyNegotiated reads false and the v1 reports stay accepted.
func (s *Service) stampApplyNonce(ctx context.Context, q interface {
	StampWorkerCredentialSwitchApplyNonce(ctx context.Context, arg store.StampWorkerCredentialSwitchApplyNonceParams) error
}, workerID uuid.UUID, protocolCaps []string, nonce string) (pgtype.Text, error) {
	if !slices.Contains(protocolCaps, capability.CredentialSwitchInPlaceV1) {
		return pgtype.Text{}, nil
	}
	v := pgconv.TextOrNull(nonce)
	if err := q.StampWorkerCredentialSwitchApplyNonce(ctx, store.StampWorkerCredentialSwitchApplyNonceParams{ID: workerID, Nonce: v}); err != nil {
		return pgtype.Text{}, err
	}
	return v, nil
}

// applyNegotiated reports whether the worker's CURRENT registration negotiated in-place apply:
// credential_switch_apply_nonce equals snapshot_register_nonce.
func (s *Service) applyNegotiated(ctx context.Context, workerID uuid.UUID) (bool, error) {
	w, err := s.q.GetWorkerByID(ctx, workerID)
	if err != nil {
		return false, err
	}
	return w.CredentialSwitchApplyNonce.Valid && w.SnapshotRegisterNonce.Valid &&
		w.CredentialSwitchApplyNonce.String == w.SnapshotRegisterNonce.String, nil
}

// holdCredentialSwitch applies the credential_switch_held report (issue #3181): the worker could
// not prove its local release settled, so instead of releasing the claim the run is PAUSED with
// hold_reason credential_switch_settling. One fenced statement (HoldCredentialSwitchUnsettled)
// requires the reported generation, an unreleased claim, the reporting worker, a running run and a
// switch stamp pending at that generation. The stamp is cleared; the override, worker_id and
// session are kept; no budget is banked (the later resume banks the paused gap once).
//
// IDEMPOTENT REDELIVERY: a 0-row result re-reads the run and converges when it is already paused
// in either credential-switch hold at this generation and worker; anything else is applied=false
// (the handler's ordinary 409 that changed nothing).
func (s *Service) holdCredentialSwitch(ctx context.Context, owned store.Run, wkr store.Worker, req StateRequest) (store.Run, bool, error) {
	if req.ClaimGeneration == nil {
		return store.Run{}, false, fmt.Errorf("%w: %s requires claim_generation", ErrInvalidState, stateCredentialSwitchHeld)
	}
	if !validHoldRegisterNonce(req.RegisterNonce) {
		return store.Run{}, false, fmt.Errorf("%w: %s requires register_nonce", ErrInvalidState, stateCredentialSwitchHeld)
	}
	gen := *req.ClaimGeneration
	opID, err := optionalSwitchOpID(req.OpID, stateCredentialSwitchHeld)
	if err != nil {
		return store.Run{}, false, err
	}
	// Issue #4201 m3a: for an apply-negotiated registration only an op-fenced hold is accepted. The
	// fenced statement below admits it only when op_id is the pending runs.credential_switch_op_id,
	// and the replay branch only when the persisted hold identity matches (holdIdentityMatches), so
	// the one shape left to refuse here is the un-fenced v1 hold. Nothing is written.
	if !opID.Valid {
		if retired, err := s.applyNegotiated(ctx, wkr.ID); err != nil {
			return store.Run{}, false, err
		} else if retired {
			return owned, false, ErrCredentialSwitchReleaseRetired
		}
	}
	rows, err := s.q.HoldCredentialSwitchUnsettled(ctx, store.HoldCredentialSwitchUnsettledParams{
		ID: owned.ID, WorkerID: pgconv.UUID(wkr.ID), Generation: gen, RegisterNonce: req.RegisterNonce, OpID: opID,
	})
	if err != nil {
		return store.Run{}, false, err
	}
	cur, err := s.runOwnedByWorker(ctx, owned.ID, wkr)
	if err != nil {
		return store.Run{}, false, err
	}
	if rows == 0 {
		// Replay converges only for the CURRENT incarnation: a superseded nonce is refused even
		// when the hold it would replay is already in place.
		current, err := s.currentRegisterNonce(ctx, wkr.ID, req.RegisterNonce)
		if err != nil {
			return store.Run{}, false, err
		}
		if !current {
			return cur, false, nil
		}
		if !opID.Valid {
			// No op id: the pre-#4201 replay, exactly as before.
			return cur, isCredentialSwitchHold(cur, gen), nil
		}
		// With an op id a replay must prove the hold on record was written for THIS operation,
		// generation, worker and incarnation. Without that, a hold from another operation (or an
		// owner pause that happens to share the hold reason) would answer a report it never saw.
		return cur, isCredentialSwitchHold(cur, gen) && holdIdentityMatches(cur, opID, gen, wkr.ID, req.RegisterNonce), nil
	}
	if s.bcast != nil {
		s.bcast.PublishState(owned.ID, cur.Status)
	}
	return cur, true, nil
}

// holdIdentityMatches reports whether the run's recorded credential-switch hold identity (the four
// credential_switch_hold_* columns HoldCredentialSwitchUnsettled writes) names exactly this
// operation, generation, worker and registration nonce, and the run's own claim is at that generation.
func holdIdentityMatches(r store.Run, op pgtype.UUID, gen int64, workerID uuid.UUID, nonce string) bool {
	return r.CredentialSwitchHoldOpID.Valid && r.CredentialSwitchHoldOpID.Bytes == op.Bytes &&
		r.CredentialSwitchHoldGeneration.Valid && r.CredentialSwitchHoldGeneration.Int64 == gen &&
		r.ClaimGeneration == gen &&
		r.CredentialSwitchHoldWorkerID.Valid && uuid.UUID(r.CredentialSwitchHoldWorkerID.Bytes) == workerID &&
		r.CredentialSwitchHoldRegisterNonce.Valid && r.CredentialSwitchHoldRegisterNonce.String == nonce
}

// optionalSwitchOpID parses the optional op_id a credential-switch report carries. Absent is the
// pre-#4201 unfenced form (an invalid pgtype.UUID, which the fenced queries read as NULL); a present
// value must be a UUID or the report is invalid (a 400, not a silent unfenced fallback).
func optionalSwitchOpID(op *string, state string) (pgtype.UUID, error) {
	if op == nil {
		return pgtype.UUID{}, nil
	}
	id, err := uuid.Parse(*op)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("%w: %s op_id is not a UUID", ErrInvalidState, state)
	}
	return pgconv.UUID(id), nil
}

// ErrCredentialSwitchUncertainParkPending refuses a credential_switch_uncertain report because the
// run has a pending wall or reserve pause (issue #4201): that park belongs to the worker's own park
// path, so the worker takes it instead of the uncertain hold. Nothing was written. The handler answers
// 409 with reason "park_pending".
var ErrCredentialSwitchUncertainParkPending = errors.New("a wall or reserve pause is pending for this run")

// uncertainCredentialSwitch applies the credential_switch_uncertain report (issue #4201): the worker
// cannot prove whether an in-place credential apply took effect, so the run PAUSES for the owner to
// decide (one fenced statement, SetRunCredentialSwitchUncertainHold: reporting worker, generation,
// unreleased claim, live status, current registration nonce, and the named operation either still
// pending or journaled as a child epoch). hold_reason stays NULL, the worker, session and open
// question are kept, a pending owner pause is left alone and nothing is banked.
//
// A 0-row result re-reads the run. A pending wall/reserve pause is the distinct
// ErrCredentialSwitchUncertainParkPending. An idempotent replay (paused, no hold reason, same
// generation, worker and current incarnation, operation still pending or journaled) converges to
// applied; anything else is applied=false (the handler's ordinary 409).
//
// The `credential_switch_uncertain` feed message is NOT written here: the worker emits it itself
// through its own batcher, at a seq it owns, so it cannot collide with the worker's live frames.
func (s *Service) uncertainCredentialSwitch(ctx context.Context, owned store.Run, wkr store.Worker, req StateRequest) (store.Run, bool, error) {
	if req.ClaimGeneration == nil {
		return store.Run{}, false, fmt.Errorf("%w: %s requires claim_generation", ErrInvalidState, stateCredentialSwitchUncertain)
	}
	opID, err := optionalSwitchOpID(req.OpID, stateCredentialSwitchUncertain)
	if err != nil {
		return store.Run{}, false, err
	}
	if !opID.Valid {
		return store.Run{}, false, fmt.Errorf("%w: %s requires op_id", ErrInvalidState, stateCredentialSwitchUncertain)
	}
	if !validHoldRegisterNonce(req.RegisterNonce) {
		// A missing or malformed nonce matches no registration: nothing is written and the worker gets the
		// ordinary not-applied 409 (re-read worker-scoped so it learns the run's real state).
		cur, rerr := s.runOwnedByWorker(ctx, owned.ID, wkr)
		return cur, false, rerr
	}
	gen := *req.ClaimGeneration
	op := uuid.UUID(opID.Bytes)
	rows, err := s.q.SetRunCredentialSwitchUncertainHold(ctx, store.SetRunCredentialSwitchUncertainHoldParams{
		ID: owned.ID, WorkerID: pgconv.UUID(wkr.ID), Generation: gen, OpID: op, RegisterNonce: req.RegisterNonce,
	})
	if err != nil {
		return store.Run{}, false, err
	}
	cur, err := s.runOwnedByWorker(ctx, owned.ID, wkr)
	if err != nil {
		return store.Run{}, false, err
	}
	if rows > 0 {
		if s.bcast != nil {
			s.bcast.PublishState(owned.ID, cur.Status)
		}
		return cur, true, nil
	}
	current, err := s.currentRegisterNonce(ctx, wkr.ID, req.RegisterNonce)
	if err != nil {
		return store.Run{}, false, err
	}
	if !current || cur.ClaimGeneration != gen || cur.ClaimReleasedAt.Valid {
		return cur, false, nil
	}
	if cur.PauseRequestedAt.Valid && (cur.PauseMode.String == "wall" || cur.PauseMode.String == "reserve") &&
		slices.Contains([]string{"running", "awaiting_approval", "awaiting_input", "awaiting_followup"}, cur.Status) &&
		!cur.HoldReason.Valid {
		return cur, false, ErrCredentialSwitchUncertainParkPending
	}
	if cur.Status != "paused" || cur.HoldReason.Valid {
		return cur, false, nil
	}
	stampPending := cur.CredentialSwitchRequestedAt.Valid && cur.CredentialSwitchGeneration.Valid &&
		cur.CredentialSwitchGeneration.Int64 == gen && cur.CredentialSwitchOpID.Valid &&
		uuid.UUID(cur.CredentialSwitchOpID.Bytes) == op
	if stampPending {
		return cur, true, nil
	}
	child, err := s.q.GetRunCredentialSwitchEpochByOp(ctx, store.GetRunCredentialSwitchEpochByOpParams{OpID: op, RunID: owned.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return cur, false, nil
	}
	if err != nil {
		return store.Run{}, false, err
	}
	return cur, child.ClaimGeneration == gen, nil
}

// settleCredentialSwitchHold applies the credential_switch_hold_settled acknowledgement (issue
// #3181): the worker finished joining its own operations (lane disposal, deps install). That does
// NOT prove the run's processes stopped, so credential_switch_settling becomes
// credential_switch_unsettled (settlement unproven, work and custody kept on the worker; the next
// claim re-proves quiescence fail closed). SettleCredentialSwitchHold fences
// on the worker, the generation and the incarnation (the worker must not have re-registered since
// the hold was written). A 0-row result converges only when the hold is already unsettled at this
// generation and worker; otherwise applied=false.
func (s *Service) settleCredentialSwitchHold(ctx context.Context, owned store.Run, wkr store.Worker, req StateRequest) (store.Run, bool, error) {
	if req.ClaimGeneration == nil {
		return store.Run{}, false, fmt.Errorf("%w: %s requires claim_generation", ErrInvalidState, stateCredentialSwitchHoldSettled)
	}
	if !validHoldRegisterNonce(req.RegisterNonce) {
		return store.Run{}, false, fmt.Errorf("%w: %s requires register_nonce", ErrInvalidState, stateCredentialSwitchHoldSettled)
	}
	gen := *req.ClaimGeneration
	rows, err := s.q.SettleCredentialSwitchHold(ctx, store.SettleCredentialSwitchHoldParams{
		ID: owned.ID, WorkerID: pgconv.UUID(wkr.ID), Generation: gen, RegisterNonce: req.RegisterNonce,
	})
	if err != nil {
		return store.Run{}, false, err
	}
	cur, err := s.runOwnedByWorker(ctx, owned.ID, wkr)
	if err != nil {
		return store.Run{}, false, err
	}
	if rows == 0 {
		current, err := s.currentRegisterNonce(ctx, wkr.ID, req.RegisterNonce)
		if err != nil {
			return store.Run{}, false, err
		}
		return cur, current && cur.Status == "paused" && cur.ClaimGeneration == gen &&
			cur.HoldReason.Valid && cur.HoldReason.String == holdReasonCredentialSwitchUnsettled, nil
	}
	if s.bcast != nil {
		s.bcast.PublishState(owned.ID, cur.Status)
	}
	return cur, true, nil
}

// validHoldRegisterNonce is the shape check both hold reports apply to the registration nonce they
// carry: present and bounded. Whether it is the CURRENT nonce is currentRegisterNonce's question.
func validHoldRegisterNonce(nonce string) bool { return nonce != "" && len(nonce) <= 128 }

// currentRegisterNonce reports whether nonce is the worker's current registration nonce
// (workers.snapshot_register_nonce), read after the claim mutation's worker lock.
func (s *Service) currentRegisterNonce(ctx context.Context, workerID uuid.UUID, nonce string) (bool, error) {
	w, err := s.q.GetWorkerByID(ctx, workerID)
	if err != nil {
		return false, err
	}
	return w.SnapshotRegisterNonce.Valid && w.SnapshotRegisterNonce.String == nonce, nil
}

// isCredentialSwitchHold reports whether the run is paused in a credential-switch hold at gen
// (either the settling or the unsettled reason, which is what the hold report's replay admits).
func isCredentialSwitchHold(r store.Run, gen int64) bool {
	if r.Status != "paused" || r.ClaimGeneration != gen || r.ClaimReleasedAt.Valid || !r.HoldReason.Valid {
		return false
	}
	switch r.HoldReason.String {
	case holdReasonCredentialSwitchUnsettled, holdReasonCredentialSwitchSettling:
		return true
	}
	return false
}
