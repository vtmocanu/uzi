package workersvc

import (
	"context"
	"fmt"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// credential_switch.go is the SERVER side of the held-state credential-switch protocol's
// WORKER-facing half (PRD #1247 M5, D3/D4/D14): the generation fence's arm-selection policy and
// the `credential_switch` RELEASE transition a worker reports after its local two-phase release.
// The USER-facing verb (`uzi run set-token`, which writes the override + the switch stamp) lives
// in run_credential.go; the fence's atomic FOR UPDATE wrapper is in SetState (service.go).

// stateUsesGenerationFence reports whether SetState wraps a given transition in the FOR UPDATE
// generation fence (PRD #1247 M5, D3). It is TRUE for the single-statement arms whose mutation
// runs through the tx-bound querier — running, awaiting_approval, awaiting_input,
// awaiting_followup, paused, failed — and for a LEGACY completion (a single fenced
// SetRunCompleted UPDATE). It is FALSE for:
//
//   - completed on an INTERLOCKED run: completeRunWithPermit runs its OWN permit transaction,
//     which must NOT nest under this FOR UPDATE (self-deadlock on the run row); it is
//     generation-fenced inside that lock instead (service.go's completed arm surfaces its
//     ErrStaleClaim).
//   - limit_wait / recovery_wait: multi-query park helpers on s.q whose park query already
//     requires status='running'. A released run is 'queued', so SetRunLimitWait /
//     SetRunRecoveryWait match 0 rows; and a reclaim moves ownership while the worker tears down
//     before releasing, so the switch flight never drives these. Not additionally
//     generation-fenced in M5a-1 (documented gap; a later belt-and-braces predicate is cheap).
//   - credential_switch: handled by its own fenced+idempotent ReleaseCredentialSwitch statement
//     (releaseCredentialSwitch), which must NOT go through the generic stale check — a
//     redelivered same-generation release after the release applied is idempotent success, not
//     stale.
//   - pause_failed: handled before the fence (it withdraws a pending pause, not a transition).
func stateUsesGenerationFence(state string, owned store.Run) bool {
	switch state {
	case "running", "awaiting_approval", "awaiting_input", "awaiting_followup", "paused", "failed":
		return true
	case "completed":
		return !owned.CompletionContractVersion.Valid
	default:
		return false
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
