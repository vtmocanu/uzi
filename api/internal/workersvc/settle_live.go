package workersvc

import (
	"context"
	"errors"
	"log/slog"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// liveSettleStatuses is the allowlist of run statuses a LIVE predecessor settle accepts
// (issue #1751 M2): the successor generation holds a live claim and is working or parked at a
// gate. ReleasePredecessorCustodyHoldByLiveAncestry re-asserts the same set in SQL; any other
// status (queued, a wait park, a terminal status) is retained/not_eligible.
var liveSettleStatuses = []string{"claimed", "running", "awaiting_approval", "awaiting_input"}

// checkpointRefPrefix is the forge namespace a run's checkpoint is published under, the same
// prefix PublishCheckpoint pushes to.
const checkpointRefPrefix = "refs/uzi-checkpoints/"

// validateLiveSettleRequest is the shape gate for a live settle request: generations >= 1,
// every SHA a 40-char lowercase hex commit id, and a known target. Eligibility is a
// server-side decision answered as retained/not_eligible.
func validateLiveSettleRequest(req apitypes.RecoveryLiveSettleRequest) error {
	if req.PredecessorGeneration < 1 || req.SuccessorGeneration < 1 {
		return ErrInvalidSettleRequest
	}
	for _, sha := range []string{req.PublishedSha, req.SourceSha, req.AdoptedSha} {
		if !isSettleSHA(sha) {
			return ErrInvalidSettleRequest
		}
	}
	if req.Target != apitypes.RecoverySettleTargetCheckpoint && req.Target != apitypes.RecoverySettleTargetBranch {
		return ErrInvalidSettleRequest
	}
	return nil
}

// SettlePredecessorHoldLive releases ONE older-generation custody hold while the SUCCESSOR
// generation of the same run is still LIVE on the caller worker (issue #1751 M2), on the api's
// OWN forge proof against the target the successor published. The worker names the hold, the
// target (its forge checkpoint ref or its run branch) and three candidate SHAs: published_sha,
// the head the successor published to that target; source_sha, the predecessor generation's
// journaled source; adopted_sha, the tip the successor adopted. The api:
//
//	a. validates the request's shape (ErrInvalidSettleRequest, 400).
//	b. reads the run (a run of another owner is ErrRunNotOwned, 404) and the hold by id on that
//	   run; the hold must belong to the caller's owner. Else retained/not_eligible.
//	c. answers an ALREADY-settled hold first: a 'live_ancestry' release with exactly this
//	   identity (settledByLiveAncestry) is released from the stored row with no forge call and
//	   no write, whatever the run did since (it may have completed or been reclaimed after an
//	   ACK was lost). Any other settled hold is retained/not_eligible.
//	d. checks live eligibility: the run is in liveSettleStatuses, at claim_generation ==
//	   successor_generation, held by the caller with its claim unreleased; the hold is open, at
//	   predecessor_generation < successor_generation, originally taken by the caller.
//	e. captures the target coordinates BEFORE the forge: runs.branch as read, and for a
//	   checkpoint target the branch derived from the run row (checkpointBranch over kind, run
//	   id, issue iid); for a branch target runs.branch itself, which must be a valid branch
//	   name. An underivable or invalid branch is retained/not_eligible.
//	f. binds source_sha to the server's captures exactly as the completed path does
//	   (retained/candidate_mismatch).
//	g. proves: reads the target head H ONCE (RefHead of refs/uzi-checkpoints/<branch>, or
//	   BranchHead), a 404 is retained/branch_missing, then asks whether each distinct
//	   candidate is an ancestor of H (not_ancestor wins over ancestry_unknown).
//	h. releases with the single guarded ReleasePredecessorCustodyHoldByLiveAncestry, which
//	   re-asserts every step-b/d/e guard and the capture binding.
//	i. one row is released (FinalHeadSha = H); zero rows re-read the hold and answer released
//	   only when the stored identity matches, else retained/state_changed.
//
// Steps a-f read only the server's own rows, so every not_eligible or candidate_mismatch
// answer is given without any forge call.
//
// Durability backstop. A checkpoint proof is time-limited: the api deletes the run's checkpoint
// ref best-effort on terminal transitions (deleteCheckpointBestEffort, PRD #1030 M4), so the
// commits H covers may never reach a durable public ref. The release relies on two things
// instead: (a) it is same-worker only (step d), so the predecessor's work is already in the
// successor's clone on this worker; and (b) the successor generation's OWN claim-time hold is
// not touched by this settle and stays open, and its recovery archive computes prerequisites
// only against a FRESH, verified forge default-branch tip (agent/src/git.ts, the D5
// prerequisite resolution), never against the checkpoint ref. Checkpoint-only commits are
// therefore never treated as public and remain in the successor's custody.
func (s *Service) SettlePredecessorHoldLive(ctx context.Context, wkr store.Worker, runID, holdID uuid.UUID, req apitypes.RecoveryLiveSettleRequest) (apitypes.RecoverySettleResponse, error) {
	// a. Shape.
	if err := validateLiveSettleRequest(req); err != nil {
		return apitypes.RecoverySettleResponse{}, err
	}
	retained := func(reason string) apitypes.RecoverySettleResponse { return settleRetained(runID, holdID, reason) }
	released := func(finalHead string) apitypes.RecoverySettleResponse {
		return settleReleased(runID, holdID, finalHead)
	}

	// b. The run and the exact hold, from the server's own rows.
	run, err := s.q.GetRunByIDForUser(ctx, store.GetRunByIDForUserParams{ID: runID, UserID: wkr.UserID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apitypes.RecoverySettleResponse{}, ErrRunNotOwned
		}
		return apitypes.RecoverySettleResponse{}, err
	}
	hold, err := s.q.GetCustodyHoldForSettle(ctx, store.GetCustodyHoldForSettleParams{HoldID: holdID, RunID: runID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return retained(apitypes.RecoverySettleNotEligible), nil
		}
		return apitypes.RecoverySettleResponse{}, err
	}
	if hold.UserID != wkr.UserID {
		return retained(apitypes.RecoverySettleNotEligible), nil
	}

	// c. Idempotency BEFORE live eligibility.
	if hold.State != "open" {
		if settledByLiveAncestry(hold, req, wkr) {
			return released(hold.ReleaseFinalHeadSha.String), nil
		}
		return retained(apitypes.RecoverySettleNotEligible), nil
	}

	// d. Live eligibility.
	if !slices.Contains(liveSettleStatuses, run.Status) ||
		run.ClaimGeneration != req.SuccessorGeneration ||
		!run.WorkerID.Valid || uuid.UUID(run.WorkerID.Bytes) != wkr.ID ||
		run.ClaimReleasedAt.Valid ||
		hold.Generation != req.PredecessorGeneration ||
		req.PredecessorGeneration >= req.SuccessorGeneration ||
		hold.OriginalWorkerID != wkr.ID {
		return retained(apitypes.RecoverySettleNotEligible), nil
	}

	// e. The target coordinates, captured before the forge is asked.
	capturedBranch := run.Branch
	kind, issueIid := run.Kind, run.IssueIid
	var provenBranch string
	switch req.Target {
	case apitypes.RecoverySettleTargetBranch:
		if !capturedBranch.Valid || !isSettleBranchName(capturedBranch.String) {
			return retained(apitypes.RecoverySettleNotEligible), nil
		}
		provenBranch = capturedBranch.String
	default: // apitypes.RecoverySettleTargetCheckpoint (validated above)
		cp, ok := checkpointBranch(kind, runID, issueIid)
		if !ok || !isSettleBranchName(cp) {
			return retained(apitypes.RecoverySettleNotEligible), nil
		}
		provenBranch = cp
	}

	// f. The server-held source binding.
	if reason, err := s.settleCaptureBinding(ctx, runID, holdID, req.SuccessorGeneration, req.SourceSha); err != nil || reason != "" {
		if err != nil {
			return apitypes.RecoverySettleResponse{}, err
		}
		return retained(reason), nil
	}

	// g. The api's own proof against the published target.
	logUnknown := settleLogUnknown(runID, holdID)
	f, projectID, reason, err := s.settleForge(ctx, runID, logUnknown)
	if err != nil {
		return apitypes.RecoverySettleResponse{}, err
	}
	if reason != "" {
		return retained(reason), nil
	}
	var head string
	if req.Target == apitypes.RecoverySettleTargetCheckpoint {
		head, err = f.RefHead(ctx, projectID, checkpointRefPrefix+provenBranch)
		if reason := settleHeadReason(head, err, "checkpoint ref head", logUnknown); reason != "" {
			return retained(reason), nil
		}
	} else {
		head, err = f.BranchHead(ctx, projectID, provenBranch)
		if reason := settleHeadReason(head, err, "branch head", logUnknown); reason != "" {
			return retained(reason), nil
		}
	}
	if reason := proveSettleCandidates(ctx, f, projectID, head, []string{req.PublishedSha, req.SourceSha, req.AdoptedSha}, logUnknown); reason != "" {
		return retained(reason), nil
	}

	// h. The single guarded release.
	n, err := s.q.ReleasePredecessorCustodyHoldByLiveAncestry(ctx, store.ReleasePredecessorCustodyHoldByLiveAncestryParams{
		PublishedSha:          req.PublishedSha,
		SourceSha:             req.SourceSha,
		AdoptedSha:            req.AdoptedSha,
		FinalHeadSha:          head,
		SuccessorGeneration:   req.SuccessorGeneration,
		ProvenBranch:          provenBranch,
		Target:                req.Target,
		HoldID:                holdID,
		RunID:                 runID,
		UserID:                wkr.UserID,
		PredecessorGeneration: req.PredecessorGeneration,
		WorkerID:              wkr.ID,
		CapturedBranch:        capturedBranch,
		Kind:                  kind,
		IssueIid:              issueIid,
	})
	if err != nil {
		return apitypes.RecoverySettleResponse{}, err
	}

	// i. The answer.
	if n == 1 {
		slog.Info("recovery settle: released predecessor hold by live ancestry", "run", runID.String(), "hold", holdID.String(),
			"predecessor_generation", req.PredecessorGeneration, "successor_generation", req.SuccessorGeneration,
			"target", req.Target)
		return released(head), nil
	}
	// Zero rows: something moved between the proof and the write. Released ONLY when a
	// concurrent identical settle already stamped this exact identity; never otherwise.
	after, err := s.q.GetCustodyHoldForSettle(ctx, store.GetCustodyHoldForSettleParams{HoldID: holdID, RunID: runID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return retained(apitypes.RecoverySettleStateChanged), nil
		}
		return apitypes.RecoverySettleResponse{}, err
	}
	if settledByLiveAncestry(after, req, wkr) {
		return released(after.ReleaseFinalHeadSha.String), nil
	}
	return retained(apitypes.RecoverySettleStateChanged), nil
}

// settledByLiveAncestry reports whether hold is already released with 'live_ancestry'
// evidence under EXACTLY the identity req names — predecessor and successor generations, the
// caller as the hold's original worker, all three candidate SHAs and the target — the live
// path's idempotent-acknowledgement test. Any difference (another class, candidate, successor,
// target or worker, a discarded hold, an unusable stored branch or head) is false.
func settledByLiveAncestry(hold store.RecoveryCustodyHold, req apitypes.RecoveryLiveSettleRequest, wkr store.Worker) bool {
	return hold.State == "released" &&
		hold.ReleaseEvidence.Valid && hold.ReleaseEvidence.String == "live_ancestry" &&
		hold.Generation == req.PredecessorGeneration &&
		hold.ReleaseSuccessorGeneration.Valid && hold.ReleaseSuccessorGeneration.Int64 == req.SuccessorGeneration &&
		hold.OriginalWorkerID == wkr.ID &&
		hold.ReleasePushedSha.Valid && hold.ReleasePushedSha.String == req.PublishedSha &&
		hold.ReleaseSourceSha.Valid && hold.ReleaseSourceSha.String == req.SourceSha &&
		hold.ReleaseAdoptedSha.Valid && hold.ReleaseAdoptedSha.String == req.AdoptedSha &&
		hold.ReleaseTarget.Valid && hold.ReleaseTarget.String == req.Target &&
		hold.ReleaseBranch.Valid && isSettleBranchName(hold.ReleaseBranch.String) &&
		hold.ReleaseFinalHeadSha.Valid && isSettleSHA(hold.ReleaseFinalHeadSha.String)
}
