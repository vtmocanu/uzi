package workersvc

import (
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// switch_refusal.go holds the pure refusal predicates of `uzi run set-token` (SetRunCredential and
// requestCodexCredentialSwitch): every check that depends only on the run row, the holding worker
// row and a handful of database-backed facts. The verbs call the same steps in the same order, at
// the points where their reads and writes interleave, so their error values and ordering are the
// ones they always had. The next-claim preview calls the composite predicates
// (anthropicSwitchRefusal, codexSwitchRefusal) with already-read facts to answer "could the owner
// switch this run right now" without writing anything. The verb stays the authority: its
// in-transaction locks re-check everything the preview can only estimate.

// switchStage tells codexSwitchRefusal which of the verb's three call points it is serving.
type switchStage int

const (
	// switchStageFull runs every check in the verb's order against one row, for the preview.
	switchStageFull switchStage = iota
	// switchStageObserved is the verb's pre-transaction call on the observed row: only the checks
	// that run before the verb's own request validation (claimed, live status, lane).
	switchStageObserved
	// switchStageLocked is the verb's in-transaction call on the re-read row and the locked worker:
	// the observed-row comparison, lane, policy episode and worker capability.
	switchStageLocked
)

// switchFacts are the inputs the predicates cannot derive from the run and worker rows.
type switchFacts struct {
	// Settling is the paused run's credential-switch settling fence (runHoldSettling).
	Settling bool
	// Stage selects the Codex call point; the zero value is the full preview check.
	Stage switchStage
	// Observed is the row the Codex verb observed before its transaction; the locked stage
	// compares the re-read row against it (ErrCredentialSwitchRaced). Nil compares nothing.
	Observed *store.Run
	// StaleCutoff is the heartbeat instant before which a worker counts as stale.
	StaleCutoff time.Time
}

// anthropicBoundRefusal refuses a provider-bound run: its credential is the frozen provider key.
func anthropicBoundRefusal(run store.Run) error {
	if providerBoundRun(run) {
		return ErrCredentialOverrideProviderSource
	}
	return nil
}

// anthropicHoldRefusal refuses a run on a hold no token switch can release: a base-attestation
// hold never runs again (issue #3194), and credentials_reset is released only by the promoter
// (PRD #3989 M3).
func anthropicHoldRefusal(run store.Run) error {
	if IsBaseAttestationHeld(run) {
		return ErrBaseAttestationHeld
	}
	if run.Status == "paused" && run.HoldReason.Valid && run.HoldReason.String == holdCredentialsReset {
		return ErrCredentialsResetHeld
	}
	return nil
}

// heldReviewRefusal refuses a held-state switch on a diff-review advice run, whose switch would be
// accepted but never honored.
func heldReviewRefusal(run store.Run) error {
	if run.ReviewTargetRunID.Valid {
		return fmt.Errorf("%w: held-state review (review_target_run_id set)", ErrCredentialOverrideLaneNotSwitchable)
	}
	return nil
}

// heldWorkerRefusal is the held-state worker check: the run needs a live holding worker (a zero
// worker.ID is "no row") that advertises credential_switch_v1.
func heldWorkerRefusal(run store.Run, w store.Worker) error {
	if !run.WorkerID.Valid || w.ID == uuid.Nil {
		return ErrCredentialSwitchNoLiveWorker
	}
	if !slices.Contains(w.ProtocolCapabilities, capability.CredentialSwitchV1) {
		return &CredentialSwitchWorkerUnsupportedError{WorkerID: w.ID, WorkerName: w.Name}
	}
	return nil
}

// lockedParkRefusal is the check the queued/parked set-token transaction makes on the row it locked,
// in order: the three per-cause retry-stamp sentinels, the observed-row comparison, then the owner hold.
func lockedParkRefusal(locked store.LockRunForCredentialSwitchRow, observed store.Run) error {
	if locked.Status == "recovery_wait" && locked.RecoveryWaitCause.String == recoveryCauseDataVolumeFull && !locked.RecoveryRetryNotBefore.Valid {
		return ErrDataVolumeFullHeld
	}
	if locked.Status == "recovery_wait" && locked.RecoveryWaitCause.Valid && locked.RecoveryWaitCause.String == "forge_unreachable" && !locked.RecoveryRetryNotBefore.Valid {
		return ErrForgeUnreachableHeld
	}
	if locked.Status == "recovery_wait" && locked.RecoveryWaitCause.String == recoveryCauseCodexIdleStall && locked.RecoveryWaitCause.Valid && !locked.RecoveryRetryNotBefore.Valid {
		return ErrCodexIdleStallHeld
	}
	if locked.Status != observed.Status || locked.ClaimGeneration != observed.ClaimGeneration ||
		locked.StatusSince.Valid != observed.StatusSince.Valid || !locked.StatusSince.Time.Equal(observed.StatusSince.Time) {
		return ErrCredentialSwitchRaced
	}
	if label, held := recoveryWaitOwnerHold(locked); held {
		return &RunOwnerHeldError{Label: label}
	}
	return nil
}

// lockedRowOf is the row the queued/parked transaction would lock, read from the run row instead.
func lockedRowOf(run store.Run) store.LockRunForCredentialSwitchRow {
	return store.LockRunForCredentialSwitchRow{
		ID: run.ID, Status: run.Status, StatusSince: run.StatusSince, ClaimGeneration: run.ClaimGeneration,
		RecoveryWaitCause: run.RecoveryWaitCause, RecoveryRetryNotBefore: run.RecoveryRetryNotBefore,
		HoldReason: run.HoldReason, PolicyOutcomeID: run.PolicyOutcomeID, PolicyResolvedAt: run.PolicyResolvedAt,
	}
}

// anthropicStatusRefusal is the status dispatch of SetRunCredential reduced to its refusals. A
// paused run on the credential_disabled hold is reassigned (never refused); any other paused run is
// refused only while its settling fence is up. A queued or parked run is checked as its locked
// transaction would; a held run needs a live, capable worker; claimed and terminal runs always refuse.
func anthropicStatusRefusal(run store.Run, worker store.Worker, facts switchFacts) error {
	switch run.Status {
	case "paused":
		if run.HoldReason.Valid && run.HoldReason.String == "credential_disabled" {
			return nil
		}
		if facts.Settling {
			return ErrRunHoldSettling
		}
		return nil
	case "queued", "limit_wait", "pool_wait", "recovery_wait":
		return lockedParkRefusal(lockedRowOf(run), run)
	case "awaiting_approval", "awaiting_input", "awaiting_followup", "running":
		if err := heldReviewRefusal(run); err != nil {
			return err
		}
		return heldWorkerRefusal(run, worker)
	case "claimed":
		return ErrCredentialSwitchClaimAssembling
	}
	return ErrCredentialSwitchRunTerminal
}

// anthropicSwitchRefusal reports why the owner could not switch this Claude run's token right now,
// nil when the verb's pure gates would let it through. It is the composite of the steps
// SetRunCredential runs, in its order, with the lane check standing in for the override validator
// (a preview has no target to validate).
func anthropicSwitchRefusal(run store.Run, worker store.Worker, facts switchFacts) error {
	if err := anthropicBoundRefusal(run); err != nil {
		return err
	}
	if err := credentialOverrideLaneRefusal(run.Kind); err != nil {
		return err
	}
	if err := anthropicHoldRefusal(run); err != nil {
		return err
	}
	return anthropicStatusRefusal(run, worker, facts)
}

// codexSwitchRefusal is the pure half of requestCodexCredentialSwitch, in its order. facts.Stage
// picks the verb's pre-transaction call (observed row: claimed, live status, lane), its
// in-transaction call (re-read row and locked worker) or, by default, the whole sequence for a preview.
func codexSwitchRefusal(run store.Run, worker store.Worker, facts switchFacts) error {
	if facts.Stage != switchStageLocked {
		if run.Status == "claimed" {
			return ErrCredentialSwitchClaimAssembling
		}
		if !applyLiveStatus(run.Status) {
			return ErrCodexSwitchIneligible
		}
		if !codexSwitchLaneEligible(run) {
			return ErrCredentialOverrideLaneNotSwitchable
		}
		if facts.Stage == switchStageObserved {
			return nil
		}
		if !run.WorkerID.Valid {
			return ErrCredentialSwitchNoLiveWorker
		}
	}
	obs := run
	if facts.Observed != nil {
		obs = *facts.Observed
	}
	if worker.UserID != obs.UserID || run.UserID != obs.UserID || run.ClaimGeneration != obs.ClaimGeneration ||
		run.ClaimReleasedAt.Valid || !applyLiveStatus(run.Status) || run.Kind == "job" || run.Kind == "cross_check" {
		return ErrCredentialSwitchRaced
	}
	if !codexSwitchLaneEligible(run) {
		return ErrCredentialOverrideLaneNotSwitchable
	}
	if codexSwitchPolicyBlocked(run) {
		return &RunOwnerHeldError{Label: "policy hold"}
	}
	if !codexSwitchNegotiated(worker) || !slices.Contains(worker.ProtocolCapabilities, capability.CodexSwitchStoppedV1) ||
		!worker.LastHeartbeatAt.Valid || worker.LastHeartbeatAt.Time.Before(facts.StaleCutoff) {
		return &CredentialSwitchWorkerUnsupportedError{WorkerID: worker.ID, WorkerName: worker.Name}
	}
	return nil
}
