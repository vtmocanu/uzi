package workersvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

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
// the settle surface (and migration 00251's audit CHECKs) accept.
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

// maxSettleBranchLen bounds the run branch the settle surface will send to a forge; it is
// also migration 00251's release_branch CHECK ceiling.
const maxSettleBranchLen = 255

// isSettleBranchName reports whether b is a well-formed git branch name, checked BEFORE the
// worker-reported runs.branch reaches any forge URL (issue #1582 M1 rework). It applies
// git-check-ref-format's rules: non-empty and at most maxSettleBranchLen bytes; no "..",
// "//", "@{", or a component starting with "."; no leading "/" or "."; no trailing "/",
// "." or ".lock"; not the lone "@"; and no control character, DEL, space, or any of
// ~ ^ : ? * [ \ .
func isSettleBranchName(b string) bool {
	if b == "" || len(b) > maxSettleBranchLen || b == "@" {
		return false
	}
	if strings.Contains(b, "..") || strings.Contains(b, "//") || strings.Contains(b, "@{") || strings.Contains(b, "/.") {
		return false
	}
	if strings.HasPrefix(b, "/") || strings.HasPrefix(b, ".") ||
		strings.HasSuffix(b, "/") || strings.HasSuffix(b, ".") || strings.HasSuffix(b, ".lock") {
		return false
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c < 0x20 || c == 0x7f || strings.IndexByte(" ~^:?*[\\", c) >= 0 {
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
// three candidate SHAs only: pushed_sha, the SUCCESSOR generation's acknowledged pushed head
// (the head the completing generation pushed and landed, persisted by the worker before its
// terminal report); source_sha, the PREDECESSOR generation's journaled source; and
// adopted_sha, the tip the successor adopted. The api:
//
//  0. reads the run (ownership only: a run of another owner is ErrRunNotOwned, 404) and the
//     hold by id on that run (none is retained/not_eligible). A hold this caller already
//     released for EXACTLY this identity answers released from the stored row BEFORE any
//     run-state check, with no forge call and no write (issue #1751 M2): an 'ancestry'
//     release on the run's current branch (settledByAncestry), or a 'live_ancestry' release
//     taken while the run was live whose ACK was lost before it completed
//     (settledByLiveAncestryForCompleted).
//  1. reads the run from its own row: it must be 'completed', at claim_generation ==
//     successor_generation, held by the caller (runs.worker_id), on a branch that is a valid
//     git branch name (isSettleBranchName). It captures that completion identity (branch,
//     status_since). A run of another owner is ErrRunNotOwned (404); any other mismatch is
//     retained/not_eligible.
//  2. checks the hold: generation == predecessor_generation < successor_generation,
//     original_worker_id == caller, and still open (any settled hold step 0 did not
//     recognise is retained/not_eligible).
//  3. binds the candidates to the facts the server already holds, where they exist: when any
//     recovery capture registered under the hold was created BEFORE the successor generation
//     claimed (its hold's created_at, else runs.claimed_at; a later capture is ignored
//     entirely, so one reserved after completion cannot plant a source), source_sha must equal
//     one of those captures' source_sha; when the run is interlocked, pushed_sha must equal
//     the head of the completion permit its completion consumed. A mismatch is
//     retained/candidate_mismatch (an interlocked run with no consumed permit, or one whose
//     consumed permit head is not a 40-char lowercase hex commit id, is retained/not_eligible).
//     The worker process holds the custody, but these candidates must not be free choices
//     where the server knows the answer.
//  4. proves: reads the branch head H ONCE from the forge (a 404 is retained/branch_missing),
//     then asks the forge whether each distinct candidate is an ancestor of H. Any
//     not_ancestor is retained/not_ancestor; otherwise any unknown or error is
//     retained/ancestry_unknown.
//  5. releases with the single guarded ReleasePredecessorCustodyHoldByAncestry statement,
//     which re-asserts every step-1/step-2 guard and the capture binding. One row is released
//     (FinalHeadSha = H); zero rows re-read the hold and answer released only when the stored
//     identity matches, else retained/state_changed. It never touches a sibling hold.
//
// Steps 1-3 read only the server's own rows, so every not_eligible or candidate_mismatch
// answer is given without any forge call.
func (s *Service) SettlePredecessorHold(ctx context.Context, wkr store.Worker, runID, holdID uuid.UUID, req apitypes.RecoverySettleRequest) (apitypes.RecoverySettleResponse, error) {
	if err := validateSettleRequest(req); err != nil {
		return apitypes.RecoverySettleResponse{}, err
	}
	retained := func(reason string) apitypes.RecoverySettleResponse { return settleRetained(runID, holdID, reason) }
	released := func(finalHead string) apitypes.RecoverySettleResponse {
		return settleReleased(runID, holdID, finalHead)
	}

	// The run (ownership only, for the 404) and the exact hold, from the server's own rows.
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

	// Idempotent acknowledgement FIRST (issue #1751 M2): a hold this caller already settled for
	// this exact identity answers released from the stored row, with no forge call and no
	// write, whatever the run did since. That covers an 'ancestry' settle whose ACK was lost
	// (on the run's current branch, as before) and a 'live_ancestry' settle whose ACK was lost
	// before the run completed (settledByLiveAncestryForCompleted).
	if hold.State == "released" && hold.Generation == req.PredecessorGeneration && hold.OriginalWorkerID == wkr.ID &&
		((run.Branch.Valid && settledByAncestry(hold, req, run.Branch.String)) || settledByLiveAncestryForCompleted(hold, req, wkr)) {
		return released(hold.ReleaseFinalHeadSha.String), nil
	}

	// 1. The run's completion identity.
	if run.Status != "completed" ||
		run.ClaimGeneration != req.SuccessorGeneration ||
		!run.WorkerID.Valid || uuid.UUID(run.WorkerID.Bytes) != wkr.ID ||
		!run.Branch.Valid || !isSettleBranchName(run.Branch.String) ||
		!run.StatusSince.Valid {
		return retained(apitypes.RecoverySettleNotEligible), nil
	}
	branch := run.Branch.String
	completedSince := run.StatusSince

	// 2. The exact hold. An already-settled hold was answered above when its identity matched.
	if hold.Generation != req.PredecessorGeneration ||
		req.PredecessorGeneration >= req.SuccessorGeneration ||
		hold.OriginalWorkerID != wkr.ID ||
		hold.State != "open" {
		return retained(apitypes.RecoverySettleNotEligible), nil
	}

	// 3. The server-held candidate binding.
	if reason, err := s.settleCaptureBinding(ctx, runID, holdID, req.SuccessorGeneration, req.SourceSha); err != nil || reason != "" {
		if err != nil {
			return apitypes.RecoverySettleResponse{}, err
		}
		return retained(reason), nil
	}
	if run.CompletionContractVersion.Valid {
		if !run.ContractRevision.Valid {
			return retained(apitypes.RecoverySettleNotEligible), nil
		}
		permitHead, err := s.q.GetSettleCompletionPermitHead(ctx, store.GetSettleCompletionPermitHeadParams{
			RunID: runID, ContractRevision: run.ContractRevision.Int32, WorkerID: wkr.ID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return retained(apitypes.RecoverySettleNotEligible), nil
			}
			return apitypes.RecoverySettleResponse{}, err
		}
		if !isSettleSHA(permitHead) {
			// Not a commit id this surface could ever match or stamp: there is no usable
			// server fact to bind to, which is ineligibility, not a worker mismatch.
			return retained(apitypes.RecoverySettleNotEligible), nil
		}
		if permitHead != req.PushedSha {
			return retained(apitypes.RecoverySettleCandidateMismatch), nil
		}
	}

	// 4. The api's own proof, via the forge compare API.
	logUnknown := settleLogUnknown(runID, holdID)
	f, projectID, reason, err := s.settleForge(ctx, runID, logUnknown)
	if err != nil {
		return apitypes.RecoverySettleResponse{}, err
	}
	if reason != "" {
		return retained(reason), nil
	}
	head, err := f.BranchHead(ctx, projectID, branch)
	if reason := settleHeadReason(head, err, "branch head", logUnknown); reason != "" {
		return retained(reason), nil
	}
	if reason := proveSettleCandidates(ctx, f, projectID, head, []string{req.PushedSha, req.SourceSha, req.AdoptedSha}, logUnknown); reason != "" {
		return retained(reason), nil
	}

	// 5. The single guarded release.
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

// settleRetained / settleReleased build the two settle answers (issue #1582 M1), shared by
// the completed-run and live settle paths.
func settleRetained(runID, holdID uuid.UUID, reason string) apitypes.RecoverySettleResponse {
	return apitypes.RecoverySettleResponse{
		RunID: runID.String(), HoldID: holdID.String(),
		Outcome: apitypes.RecoverySettleRetained, Reason: reason,
	}
}

func settleReleased(runID, holdID uuid.UUID, finalHead string) apitypes.RecoverySettleResponse {
	return apitypes.RecoverySettleResponse{
		RunID: runID.String(), HoldID: holdID.String(),
		Outcome: apitypes.RecoverySettleReleased, FinalHeadSha: finalHead,
	}
}

// settleLogUnknown returns the logger both settle paths use for a proof step that could not
// be completed; the error is secret-scrubbed.
func settleLogUnknown(runID, holdID uuid.UUID) func(step string, err error) {
	return func(step string, err error) {
		slog.Warn("recovery settle: ancestry unproven", "run", runID.String(), "hold", holdID.String(),
			"step", step, "error", secretscrub.Scrub(err.Error()))
	}
}

// settleCaptureBinding is the server-held source binding both settle paths apply (issue #1582
// M1 rework): when any capture registered under the hold before the successor generation
// claimed exists, source must equal one of their source_sha values, else candidate_mismatch.
// It returns "" when the binding holds (or there is nothing to bind to).
func (s *Service) settleCaptureBinding(ctx context.Context, runID, holdID uuid.UUID, successor int64, source string) (string, error) {
	sources, err := s.q.ListCaptureSourceShasForHold(ctx, store.ListCaptureSourceShasForHoldParams{
		HoldID: holdID, RunID: runID, SuccessorGeneration: successor,
	})
	if err != nil {
		return "", err
	}
	if len(sources) > 0 && !slices.Contains(sources, source) {
		return apitypes.RecoverySettleCandidateMismatch, nil
	}
	return "", nil
}

// settleForge resolves the forge a settle proof asks, from the run's claim context. A nil
// forge seam is ErrForgesUnavailable; a run with no repo/connection behind it is not_eligible
// (nothing to prove against); a forge that cannot be built is ancestry_unknown. A non-empty
// reason is the retained answer.
func (s *Service) settleForge(ctx context.Context, runID uuid.UUID, logUnknown func(string, error)) (forge.Forge, int64, string, error) {
	if s.forges == nil {
		return nil, 0, "", ErrForgesUnavailable
	}
	rc, err := s.q.GetRunClaimContext(ctx, runID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, 0, apitypes.RecoverySettleNotEligible, nil
		}
		return nil, 0, "", err
	}
	f, err := s.forges.ForgeForConnection(rc.ForgeType, rc.BaseUrl, rc.TokenCiphertext)
	if err != nil {
		logUnknown("build forge", err)
		return nil, 0, apitypes.RecoverySettleAncestryUnknown, nil
	}
	return f, rc.ForgeProjectID, "", nil
}

// settleHeadReason classifies the target-head read both settle paths make: a 404
// (forge.ErrRefNotFound) is branch_missing; any other error, or a head that is not a 40-char
// lowercase hex commit id, is ancestry_unknown. "" means head is usable.
func settleHeadReason(head string, err error, step string, logUnknown func(string, error)) string {
	if errors.Is(err, forge.ErrRefNotFound) {
		return apitypes.RecoverySettleBranchMissing
	}
	if err != nil {
		logUnknown(step, err)
		return apitypes.RecoverySettleAncestryUnknown
	}
	if !isSettleSHA(head) {
		logUnknown(step, fmt.Errorf("forge returned a malformed %s", step))
		return apitypes.RecoverySettleAncestryUnknown
	}
	return ""
}

// proveSettleCandidates asks the forge, once per DISTINCT candidate, whether it is an ancestor
// of (or equal to) head. Any explicit not_ancestor wins (not_ancestor); otherwise any error or
// inconclusive answer is ancestry_unknown — only an error-free positive answer is proof. ""
// means every candidate is proven.
func proveSettleCandidates(ctx context.Context, f forge.Forge, projectID int64, head string, candidates []string, logUnknown func(string, error)) string {
	notAncestor, unknown := false, false
	asked := map[string]bool{}
	for _, c := range candidates {
		if asked[c] {
			continue // one question per distinct candidate
		}
		asked[c] = true
		a, err := f.CompareAncestry(ctx, projectID, head, c)
		if err != nil {
			// Any error is unknown, whatever verdict accompanied it: only an explicit
			// error-free positive answer is proof.
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
		return apitypes.RecoverySettleNotAncestor
	}
	if unknown {
		return apitypes.RecoverySettleAncestryUnknown
	}
	return ""
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

// settledByLiveAncestryForCompleted reports whether hold was already released by a LIVE settle
// (issue #1751 M2, 'live_ancestry') for the identity a COMPLETED-path request names: the
// same predecessor generation, the same successor generation, the caller as the hold's
// original worker, and the same source and adopted SHAs. It is the completed path's
// idempotent acknowledgement for a live settle whose ACK was lost before the run completed.
// pushed_sha is deliberately NOT compared: the live settle stamped the head the successor had
// PUBLISHED to its checkpoint ref or branch at settle time, while the completed request's
// pushed_sha is the successor's FINAL pushed head, which legitimately moved on since. The
// predecessor's work (source, adopted) is what the hold protects, and that is what must match.
func settledByLiveAncestryForCompleted(hold store.RecoveryCustodyHold, req apitypes.RecoverySettleRequest, wkr store.Worker) bool {
	return hold.State == "released" &&
		hold.ReleaseEvidence.Valid && hold.ReleaseEvidence.String == "live_ancestry" &&
		hold.Generation == req.PredecessorGeneration &&
		hold.OriginalWorkerID == wkr.ID &&
		hold.ReleaseSuccessorGeneration.Valid && hold.ReleaseSuccessorGeneration.Int64 == req.SuccessorGeneration &&
		hold.ReleaseSourceSha.Valid && hold.ReleaseSourceSha.String == req.SourceSha &&
		hold.ReleaseAdoptedSha.Valid && hold.ReleaseAdoptedSha.String == req.AdoptedSha &&
		hold.ReleaseFinalHeadSha.Valid && isSettleSHA(hold.ReleaseFinalHeadSha.String)
}
