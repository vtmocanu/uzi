package workersvc

import (
	"context"
	"errors"
	"log/slog"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The fresh claim (issue #4628, plan section 4): inside the claim transaction of an eligible hosted
// worker (W2), before the ordinary claim, a pending fresh resume of the same owner is taken for W2. The
// claim and its assembly refusals live here; the start, the refusal and the evidence selection are in
// fresh_resume_start.go and fresh_resume_select.go.

const (
	// freshClaimCandidates bounds the pending rows one poll examines (plan section 4, step 1).
	freshClaimCandidates = 3
	// freshTransferSeconds is the window a claimed transfer has to seed before the sweep may refuse it.
	freshTransferSeconds = 2 * 60 * 60
	// freshRefusedSettle is finishRunClaimTx's settle value when an assembly arm refused a fresh
	// transfer: the run is back at its held recovery_wait, so the only publication is that state, never a
	// failed notify or a paused broadcast.
	freshRefusedSettle = "fresh_refused"
)

// The reasons an assembly arm gives when it refuses a claimed fresh transfer (run_fresh_resumes.refusal_reason).
const (
	freshRefusalCredentialDisabled = "credential_disabled" //nolint:gosec // G101: a refusal-reason VOCABULARY value, not a credential.
	freshRefusalOverrideUnavail    = "run_override_unavailable"
	freshRefusalCodexAccount       = "codex_account_unavailable"
	freshRefusalPoolWait           = "pool_wait"
	freshRefusalAssemblyRequeue    = "claim_assembly_requeue"
	freshRefusalAssemblyFailed     = "claim_assembly_failed"
)

// FreshResumePayload is the claim payload's fresh_resume object: the transfer the claim took and the
// evidence the destination seeds from. CaptureID is set for a capture, CheckpointRef for a checkpoint ref.
type FreshResumePayload struct {
	ID                 string `json:"id"`
	Kind               string `json:"kind"`
	CaptureID          string `json:"capture_id,omitempty"`
	CheckpointRef      string `json:"checkpoint_ref,omitempty"`
	Sha                string `json:"sha"`
	EvidenceGeneration int64  `json:"evidence_generation"`
}

// freshResumePayload builds the payload object from a claimed transfer row; nil for a non-fresh claim.
func freshResumePayload(row *store.RunFreshResume) *FreshResumePayload {
	if row == nil {
		return nil
	}
	p := &FreshResumePayload{ID: row.ID.String(), Kind: row.EvidenceKind, Sha: row.Sha, EvidenceGeneration: row.EvidenceGeneration}
	if row.CaptureID.Valid {
		p.CaptureID = uuid.UUID(row.CaptureID.Bytes).String()
	}
	if row.CheckpointRef.Valid {
		p.CheckpointRef = row.CheckpointRef.String
	}
	return p
}

// freshClaimEligible reports whether the locked claimant may take a fresh claim: the gate is on and the
// worker is a hosted, persistent destination that advertises fresh_resume_v1 and recovery_inventory_v1
// and accepts new intake (not draining, fenced, cordoned or hibernating). The same destination facts as
// CountFreshResumeDestinations, judged on the row this transaction holds locked.
func (s *Service) freshClaimEligible(w store.Worker, lane string) bool {
	return s.p.FreshResume && lane != "cross_check" &&
		w.Kind == "hosted" && !w.Ephemeral &&
		slices.Contains(w.ProtocolCapabilities, capability.FreshResumeV1) &&
		slices.Contains(w.ProtocolCapabilities, capability.RecoveryInventoryV1) &&
		store.WorkerAcceptsNewIntake(w)
}

// claimFreshResume is plan section 4, steps 1 to 3, for the claimant whose row the caller holds locked
// (reread) in tx. params is the claim's ClaimRun input with the locked-row overlay applied. It reports
// the claimed run and its transfer row (claimed, G', destination), or false when no candidate was
// taken, in which case the transaction is exactly as it was before (a refusal written for an owner
// without headroom persists on purpose).
func (s *Service) claimFreshResume(ctx context.Context, tx pgx.Tx, qtx *store.Queries, reread store.Worker, params store.ClaimRunParams) (store.Run, store.RunFreshResume, bool, error) {
	if !s.freshClaimEligible(reread, params.Lane) || reread.MaintenanceFenced {
		return store.Run{}, store.RunFreshResume{}, false, nil
	}
	cands, err := qtx.ListPendingFreshResumeCandidates(ctx, store.ListPendingFreshResumeCandidatesParams{
		UserID: reread.UserID, WorkerID: reread.ID,
		IsDockerWorker: params.IsDockerWorker, DockerRepoAllowlist: params.DockerRepoAllowlist,
		WorkerCaps: params.WorkerCaps, CapabilityAware: params.CapabilityAware,
		SnapshotFreshCutoff: params.SnapshotFreshCutoff, RowLimit: freshClaimCandidates,
		// ClaimRun's weekly-reserve gate, from the same ClaimRunParams.
		ReserveOn: params.ReserveOn, ReserveNow: params.ReserveNow, ReserveOverrideEnded: params.ReserveOverrideEnded,
		ReservePauseOn: params.ReservePauseOn, ReserveBlockedCodex: params.ReserveBlockedCodex,
		ReserveJudgeMode: params.ReserveJudgeMode, ReservePermitted: params.ReservePermitted,
		ReservePooled: params.ReservePooled, ReserveOwnerDefaultBlocked: params.ReserveOwnerDefaultBlocked,
		ReserveJudgeStaticBlocked: params.ReserveJudgeStaticBlocked, ReserveBlockedClaude: params.ReserveBlockedClaude,
		ReserveWorkerMode: params.ReserveWorkerMode, ReserveWorkerStaticBlocked: params.ReserveWorkerStaticBlocked,
	})
	if err != nil {
		return store.Run{}, store.RunFreshResume{}, false, err
	}
	if afterFreshCandidateRead != nil {
		afterFreshCandidateRead(cands)
	}
	for _, cand := range cands {
		run, row, claimed, err := s.claimFreshCandidate(ctx, tx, reread, params, cand)
		if err != nil {
			return store.Run{}, store.RunFreshResume{}, false, err
		}
		if claimed {
			return run, row, true, nil
		}
	}
	return store.Run{}, store.RunFreshResume{}, false, nil
}

// afterFreshCandidateRead is a test seam: called with the candidate list between the unlocked candidate
// read and the per-candidate locked step. Nil in production.
var afterFreshCandidateRead func([]store.ListPendingFreshResumeCandidatesRow)

// claimFreshCandidate runs step 2 for one candidate under SAVEPOINT fr. A skip rolls the savepoint back
// (releasing every lock and write of the attempt) and reports not claimed; a headroom refusal and a claim
// release it so their writes persist. A statement error rolls back to the savepoint and skips the
// candidate, so one bad row never blocks the ordinary claim behind it. Errors returned: a cancelled
// context, and a failed Begin or RELEASE SAVEPOINT.
func (s *Service) claimFreshCandidate(ctx context.Context, tx pgx.Tx, w2 store.Worker, params store.ClaimRunParams, cand store.ListPendingFreshResumeCandidatesRow) (run store.Run, row store.RunFreshResume, claimed bool, err error) {
	sp, err := tx.Begin(ctx) // SAVEPOINT fr
	if err != nil {
		return run, row, false, err
	}
	keep := false
	defer func() {
		if keep {
			if cerr := sp.Commit(ctx); cerr != nil { // RELEASE SAVEPOINT fr
				run, row, claimed, err = store.Run{}, store.RunFreshResume{}, false, cerr
			}
			return
		}
		_ = sp.Rollback(ctx) // ROLLBACK TO SAVEPOINT fr
	}()
	q := store.New(sp)
	skip := func(why string, cause error) (store.Run, store.RunFreshResume, bool, error) {
		if ctx.Err() != nil {
			return store.Run{}, store.RunFreshResume{}, false, ctx.Err()
		}
		if cause != nil {
			slog.Warn("fresh resume claim: candidate skipped", "transfer", cand.ID.String(), "run", cand.RunID.String(), "why", why, "error", cause)
		}
		return store.Run{}, store.RunFreshResume{}, false, nil
	}

	// 2.1: the owner's headroom lock, never waited for: Service.Claim already holds the claimant row, and
	// Start and Resume take this lock first.
	got, lerr := store.TryLockFreshResumeOwner(ctx, sp, w2.UserID)
	if lerr != nil {
		return skip("owner lock", lerr)
	}
	if !got {
		return skip("owner lock busy", nil)
	}

	// 2.2: the predecessor W1, FOR KEY SHARE SKIP LOCKED. No row is either a conflicting holder (contention:
	// skip, never a refusal) or a deleted worker (the transfer proceeds with no saved worker).
	w1, w1Deleted := cand.W1ID, false
	if w1 != uuid.Nil {
		if _, lerr := q.LockWorkerKeyShareSkipLocked(ctx, w1); lerr != nil {
			if !errors.Is(lerr, pgx.ErrNoRows) {
				return skip("predecessor lock", lerr)
			}
			present, perr := q.FreshResumeWorkerExists(ctx, w1)
			if perr != nil {
				return skip("predecessor probe", perr)
			}
			if present {
				return skip("predecessor lock contended", nil)
			}
			w1Deleted = true
		}
	}

	// 2.3: the run and the transfer, SKIP LOCKED, then the predecessor recheck on the locked versions.
	tgt, lerr := q.LockFreshClaimTarget(ctx, store.LockFreshClaimTargetParams{
		ID: cand.ID, UserID: w2.UserID, WorkerID: w2.ID, SnapshotFreshCutoff: params.SnapshotFreshCutoff,
	})
	if lerr != nil {
		if errors.Is(lerr, pgx.ErrNoRows) {
			return skip("row locked or no longer claimable", nil)
		}
		return skip("run lock", lerr)
	}
	if tgt.W1ID != w1 && (!w1Deleted || tgt.W1ID != uuid.Nil) {
		return skip("predecessor changed", nil)
	}

	// 2.4: the capture FOR SHARE NOWAIT, rechecked on the locked version.
	if tgt.EvidenceKind == apitypes.FreshResumeKindCapture && tgt.CaptureID.Valid {
		if _, lerr := q.LockCaptureForFreshClaim(ctx, store.LockCaptureForFreshClaimParams{
			ID: uuid.UUID(tgt.CaptureID.Bytes), RunID: tgt.RunID, UserID: w2.UserID,
			EvidenceGeneration: tgt.EvidenceGeneration, Sha: tgt.Sha,
		}); lerr != nil {
			var pgErr *pgconn.PgError
			if errors.Is(lerr, pgx.ErrNoRows) || (errors.As(lerr, &pgErr) && pgErr.Code == "55P03") {
				return skip("capture busy or no longer available", nil)
			}
			return skip("capture lock", lerr)
		}
	}

	// 2.5: R1, the owner's headroom at claim time, a statement of its own after every lock above. The
	// pending reservation is already one unit of the owner count, so the count may equal the limit; the
	// run's own open holds must leave room for this attempt's generation hold and the next one.
	if limit := params.CustodyHoldLimit; limit > 0 {
		adm, aerr := q.GetCustodyAdmissionForRun(ctx, store.GetCustodyAdmissionForRunParams{
			UserID: w2.UserID, RunID: tgt.RunID, CustodyHoldLimit: limit, HeartbeatCutoff: params.HeartbeatCutoff,
		})
		if aerr != nil {
			return skip("headroom read", aerr)
		}
		if adm.OwnOpenHolds+2 > int64(limit) || adm.AdmissionCountedHolds > int64(limit) {
			// The refusal is the owner's answer to "no room", not a skipped try: it persists (RELEASE) and the
			// next candidate is examined.
			outcome, rerr := RefuseFreshResumeInTx(ctx, sp, tgt.ID, FreshRefusalInsufficientHeadroom)
			if rerr != nil {
				return skip("headroom refusal", rerr)
			}
			keep = outcome == FreshRefusedPending
			return store.Run{}, store.RunFreshResume{}, false, nil
		}
	}

	// 2.6 to 2.8: the opt-in for this row, the saved tuple and the claim itself.
	if oerr := q.OptInFreshResume(ctx, tgt.ID.String()); oerr != nil {
		return skip("opt-in", oerr)
	}
	prepared, perr := q.PrepareFreshResumeClaim(ctx, store.PrepareFreshResumeClaimParams{
		RunID: tgt.RunID, ID: tgt.ID, WorkerID: pgconv.UUID(w2.ID),
	})
	if perr != nil {
		if errors.Is(perr, pgx.ErrNoRows) {
			return skip("run no longer held", nil)
		}
		return skip("prepare", perr)
	}
	if prepared.UserID != w2.UserID {
		return skip("owner mismatch", errors.New("prepared run is not the claimant's"))
	}
	claimParams := params
	claimParams.OnlyRunID = pgconv.UUID(tgt.RunID)
	claimedRun, cerr := s.claimCheckpointRun(ctx, sp, claimParams)
	if cerr != nil {
		// 2.9: no row is the ordinary "this worker cannot take it now" (a gate of ClaimRun's own); try the
		// next candidate with everything of this one rolled back.
		if errors.Is(cerr, pgx.ErrNoRows) {
			return skip("claim gated", nil)
		}
		return skip("claim", cerr)
	}
	if claimedRun.ID != tgt.RunID {
		return skip("claimed another run", errors.New("claim returned a run other than the transfer's"))
	}

	// 2.10: record the claim, then end the opt-in.
	marked, merr := q.MarkFreshResumeClaimed(ctx, store.MarkFreshResumeClaimedParams{
		ID: tgt.ID, WorkerID: pgconv.UUID(w2.ID), ClaimGeneration: pgtype.Int8{Int64: claimedRun.ClaimGeneration, Valid: true}, TransferSeconds: freshTransferSeconds,
	})
	if merr != nil {
		if errors.Is(merr, pgx.ErrNoRows) {
			return skip("transfer no longer pending", nil)
		}
		return skip("mark claimed", merr)
	}
	if eerr := q.ClearFreshResumeOptIn(ctx); eerr != nil {
		return skip("clear opt-in", eerr)
	}
	keep = true
	slog.Info("fresh resume claimed", "transfer", marked.ID.String(), "run", marked.RunID.String(), "destination", w2.ID.String(), "claim_generation", claimedRun.ClaimGeneration)
	return claimedRun, marked, true, nil
}

// lockFreshSavedWorkerNoWait takes the claimed transfer's saved predecessor FOR KEY SHARE NOWAIT, before
// the run lock of the claim finisher. A transfer that is not claimed, has no saved worker, or whose worker
// is gone locks nothing; 55P03 is the caller's retry.
func lockFreshSavedWorkerNoWait(ctx context.Context, q *store.Queries, frid uuid.UUID) error {
	row, err := q.GetFreshResume(ctx, frid)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if row.State != "claimed" || !row.SavedWorkerID.Valid {
		return nil
	}
	if _, err := q.LockWorkerKeyShareNoWait(ctx, uuid.UUID(row.SavedWorkerID.Bytes)); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	return nil
}

// freshAssemblyReason names the assembly arm finishRunClaimTx would have taken, as the transfer's
// refusal reason. The arms are the ones finishRunClaimTx's switch writes the run in.
func freshAssemblyReason(overrideUnavailable, credDisabled, holdClass, transient bool, decision error) string {
	switch {
	case overrideUnavailable:
		return freshRefusalOverrideUnavail
	case credDisabled:
		return freshRefusalCredentialDisabled
	case holdClass:
		return freshRefusalCodexAccount
	case transient && errors.Is(decision, errAutoPoolEmpty):
		return freshRefusalPoolWait
	case transient:
		return freshRefusalAssemblyRequeue
	}
	return freshRefusalAssemblyFailed
}

// refuseFreshAtAssembly is V1: the assembly of a fresh claim found it cannot deliver (a disabled
// credential, an unhonorable override, a Codex account hold, a pool wait, a terminal or transient
// failure), so the claimed transfer is refused and the held run restored instead of the ordinary write.
// The caller holds the run lock (after the predecessor's key-share lock) and has already released W2's
// generation hold when it opened one. RefuseNoWait because the claim finisher may hold W2's worker row
// FOR UPDATE (finishRunClaimTx takes it only for checkpoint-coordinate or publication-enrolled runs, see
// optInRunFences), and the refusal must not wait on a worker lock behind it.
// expected is the number of holds that release freed, for the retained-checkpoint settlement.
func (s *Service) refuseFreshAtAssembly(ctx context.Context, q claimFinishTx, run store.Run, identity claimRecoveryIdentity, reason string, expected int) (*ClaimPayload, string, error) {
	live, ok := q.(pgxClaimFinishTx)
	if !ok {
		return nil, "", errClaimRecoveryNoTx
	}
	outcome, err := RefuseFreshResumeInTx(ctx, live.tx, identity.freshID, reason, RefuseNoWait())
	if err != nil {
		return nil, "", err
	}
	if outcome != FreshRefusedRestored {
		// The transfer was not this claim's to restore (already closed, or the run moved): the exact-claim
		// fence this finisher is built on failed, so write nothing.
		return nil, "", errClaimRecoveryStale
	}
	if err := q.Commit(ctx); err != nil {
		return nil, "", err
	}
	if expected == 1 {
		s.SettleRetainedCheckpoint(run.ID)
	}
	return nil, freshRefusedSettle, nil
}
