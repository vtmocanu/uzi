package workersvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/secretscrub"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// ErrInvalidSettleRequest is a malformed predecessor-settle request (issue #1582 M1): a
// generation below 1 or a SHA that is not a 40-char lowercase hex commit id. The handler
// maps it to 400.
var ErrInvalidSettleRequest = errors.New("invalid recovery settle request")

// isSettleSHA reports whether s is a full 40-char LOWERCASE hex commit id — the only shape
// the settle surface (and migration 00247's audit CHECKs) accept.
func isSettleSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// validateSettleRequest is the shape gate for a predecessor-settle request. It checks only
// the request's own form; eligibility (predecessor < successor included) is a server-side
// decision answered as retained/not_eligible.
func validateSettleRequest(req apitypes.RecoverySettleRequest) error {
	if req.PredecessorGeneration < 1 || req.SuccessorGeneration < 1 {
		return ErrInvalidSettleRequest
	}
	for _, sha := range []string{req.PushedSha, req.SourceSha, req.AdoptedSha} {
		if !isSettleSHA(sha) {
			return ErrInvalidSettleRequest
		}
	}
	return nil
}

// SettlePredecessorHold releases ONE older-generation custody hold on a run the caller worker
// COMPLETED, on the api's OWN ancestry proof (issue #1582 M1). A same-worker resume adopts the
// predecessor generation's work and completes; the completed-generation backstop deliberately
// never releases the older hold, so it would stay open forever. The worker names the hold and
// candidate SHAs only; the api:
//
//  1. reads the run from its own row: it must be 'completed', at claim_generation ==
//     successor_generation, held by the caller (runs.worker_id), on a non-empty branch. It
//     captures that completion identity (branch, status_since). A run of another owner is
//     ErrRunNotOwned (404); any other mismatch is retained/not_eligible.
//  2. reads the hold by id on that run: generation == predecessor_generation <
//     successor_generation and original_worker_id == caller. A hold already released with
//     'ancestry' evidence and the SAME stored identity is an idempotent released; any other
//     settled hold is retained/not_eligible. Otherwise it must be open.
//  3. proves: reads the branch head H ONCE from the forge, then asks the forge whether each
//     candidate (pushed, source, adopted) is an ancestor of H. Any not_ancestor is
//     retained/not_ancestor; otherwise any unknown or error is retained/ancestry_unknown.
//  4. releases with the single guarded ReleasePredecessorCustodyHoldByAncestry statement,
//     which re-asserts every step-1/step-2 guard. One row is released (FinalHeadSha = H);
//     zero rows re-read the hold and answer released only when the stored identity matches,
//     else retained/state_changed. It never touches a sibling hold.
func (s *Service) SettlePredecessorHold(ctx context.Context, wkr store.Worker, runID, holdID uuid.UUID, req apitypes.RecoverySettleRequest) (apitypes.RecoverySettleResponse, error) {
	if err := validateSettleRequest(req); err != nil {
		return apitypes.RecoverySettleResponse{}, err
	}
	retained := func(reason string) apitypes.RecoverySettleResponse {
		return apitypes.RecoverySettleResponse{
			RunID: runID.String(), HoldID: holdID.String(),
			Outcome: apitypes.RecoverySettleRetained, Reason: reason,
		}
	}
	released := func(finalHead string) apitypes.RecoverySettleResponse {
		return apitypes.RecoverySettleResponse{
			RunID: runID.String(), HoldID: holdID.String(),
			Outcome: apitypes.RecoverySettleReleased, FinalHeadSha: finalHead,
		}
	}

	// 1. The run, from the server's own row only.
	run, err := s.q.GetRunByIDForUser(ctx, store.GetRunByIDForUserParams{ID: runID, UserID: wkr.UserID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apitypes.RecoverySettleResponse{}, ErrRunNotOwned
		}
		return apitypes.RecoverySettleResponse{}, err
	}
	if run.Status != "completed" ||
		run.ClaimGeneration != req.SuccessorGeneration ||
		!run.WorkerID.Valid || uuid.UUID(run.WorkerID.Bytes) != wkr.ID ||
		!run.Branch.Valid || run.Branch.String == "" ||
		!run.StatusSince.Valid {
		return retained(apitypes.RecoverySettleNotEligible), nil
	}
	branch := run.Branch.String
	completedSince := run.StatusSince

	// 2. The exact hold.
	hold, err := s.q.GetCustodyHoldForSettle(ctx, store.GetCustodyHoldForSettleParams{HoldID: holdID, RunID: runID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return retained(apitypes.RecoverySettleNotEligible), nil
		}
		return apitypes.RecoverySettleResponse{}, err
	}
	if hold.Generation != req.PredecessorGeneration ||
		req.PredecessorGeneration >= req.SuccessorGeneration ||
		hold.OriginalWorkerID != wkr.ID {
		return retained(apitypes.RecoverySettleNotEligible), nil
	}
	if hold.State != "open" {
		if settledByAncestry(hold, req, branch) {
			return released(hold.ReleaseFinalHeadSha.String), nil
		}
		return retained(apitypes.RecoverySettleNotEligible), nil
	}

	// 3. The api's own proof, via the forge compare API.
	if s.forges == nil {
		return apitypes.RecoverySettleResponse{}, ErrForgesUnavailable
	}
	rc, err := s.q.GetRunClaimContext(ctx, runID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// No repo/connection behind the run: nothing to prove against.
			return retained(apitypes.RecoverySettleNotEligible), nil
		}
		return apitypes.RecoverySettleResponse{}, err
	}
	logUnknown := func(step string, err error) {
		slog.Warn("recovery settle: ancestry unproven", "run", runID.String(), "hold", holdID.String(),
			"step", step, "error", secretscrub.Scrub(err.Error()))
	}
	f, err := s.forges.ForgeForConnection(rc.ForgeType, rc.BaseUrl, rc.TokenCiphertext)
	if err != nil {
		logUnknown("build forge", err)
		return retained(apitypes.RecoverySettleAncestryUnknown), nil
	}
	head, err := f.BranchHead(ctx, rc.ForgeProjectID, branch)
	if err != nil {
		logUnknown("branch head", err)
		return retained(apitypes.RecoverySettleAncestryUnknown), nil
	}
	if !isSettleSHA(head) {
		logUnknown("branch head", fmt.Errorf("forge returned a malformed branch head"))
		return retained(apitypes.RecoverySettleAncestryUnknown), nil
	}
	notAncestor, unknown := false, false
	asked := map[string]bool{}
	for _, c := range []string{req.PushedSha, req.SourceSha, req.AdoptedSha} {
		if asked[c] {
			continue // one question per distinct candidate
		}
		asked[c] = true
		a, err := f.CompareAncestry(ctx, rc.ForgeProjectID, head, c)
		if err != nil {
			// Any error is unknown, whatever verdict accompanied it (ErrAncestryUnsupported
			// included): only an explicit error-free positive answer is proof.
			logUnknown("compare ancestry", err)
			a = forge.AncestryUnknown
		}
		switch a {
		case forge.AncestryAncestor:
		case forge.AncestryNotAncestor:
			notAncestor = true
		default:
			unknown = true
		}
	}
	if notAncestor {
		return retained(apitypes.RecoverySettleNotAncestor), nil
	}
	if unknown {
		return retained(apitypes.RecoverySettleAncestryUnknown), nil
	}

	// 4. The single guarded release.
	n, err := s.q.ReleasePredecessorCustodyHoldByAncestry(ctx, store.ReleasePredecessorCustodyHoldByAncestryParams{
		PushedSha:             req.PushedSha,
		SourceSha:             req.SourceSha,
		AdoptedSha:            req.AdoptedSha,
		FinalHeadSha:          head,
		SuccessorGeneration:   req.SuccessorGeneration,
		Branch:                branch,
		HoldID:                holdID,
		RunID:                 runID,
		PredecessorGeneration: req.PredecessorGeneration,
		WorkerID:              wkr.ID,
		CompletedSince:        completedSince,
	})
	if err != nil {
		return apitypes.RecoverySettleResponse{}, err
	}
	if n == 1 {
		slog.Info("recovery settle: released predecessor hold by ancestry", "run", runID.String(), "hold", holdID.String(),
			"predecessor_generation", req.PredecessorGeneration, "successor_generation", req.SuccessorGeneration)
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
	if after.Generation == req.PredecessorGeneration && after.OriginalWorkerID == wkr.ID && settledByAncestry(after, req, branch) {
		return released(after.ReleaseFinalHeadSha.String), nil
	}
	return retained(apitypes.RecoverySettleStateChanged), nil
}

// settledByAncestry reports whether hold is already released with 'ancestry' evidence under
// EXACTLY the identity req names (candidate SHAs + successor generation) on branch — the
// idempotent-acknowledgement test. Any difference (another class, another candidate, another
// successor, another branch, a discarded hold) is false.
func settledByAncestry(hold store.RecoveryCustodyHold, req apitypes.RecoverySettleRequest, branch string) bool {
	return hold.State == "released" &&
		hold.ReleaseEvidence.Valid && hold.ReleaseEvidence.String == "ancestry" &&
		hold.ReleasePushedSha.Valid && hold.ReleasePushedSha.String == req.PushedSha &&
		hold.ReleaseSourceSha.Valid && hold.ReleaseSourceSha.String == req.SourceSha &&
		hold.ReleaseAdoptedSha.Valid && hold.ReleaseAdoptedSha.String == req.AdoptedSha &&
		hold.ReleaseSuccessorGeneration.Valid && hold.ReleaseSuccessorGeneration.Int64 == req.SuccessorGeneration &&
		hold.ReleaseBranch.Valid && hold.ReleaseBranch.String == branch &&
		hold.ReleaseFinalHeadSha.Valid
}
