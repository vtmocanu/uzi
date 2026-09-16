package workersvc

import (
	"context"
	"fmt"
	"log/slog"

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
//   - completed on an INTERLOCKED run: completeRunWithPermit runs its OWN permit transaction,
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
// A CHAT run is EXEMPT regardless of state (PRD #1247 M5 rework): chat has no claim-generation
// contract (its batcher sends generation 0 and the run may omit it entirely), so fencing a
// capability worker's chat report would 409 it. The chat guard below returns false for every
// state so no chat transition is ever fenced.
func stateUsesGenerationFence(state string, owned store.Run) bool {
	if owned.Kind == runkind.Chat {
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
// ACCEPTED TRADEOFF (matches pause_failed): the clear carries no per-request discriminator, so a
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
	rows, err := s.q.ClearCredentialSwitchByWorker(ctx, store.ClearCredentialSwitchByWorkerParams{
		ID:         owned.ID,
		WorkerID:   pgconv.UUID(wkr.ID),
		Generation: gen,
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
	slog.Info("credential switch abandoned: bounded capture-failure give-up cleared the pending switch stamp; run continues on its current token",
		"run", owned.ID, "worker", wkr.ID, "generation", gen)
	if s.bcast != nil {
		s.bcast.PublishState(owned.ID, run.Status)
	}
	return run, true, nil
}
