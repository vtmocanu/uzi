package recovery

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Closure-backed FINAL (issue #3727, C2): a thin capture (one that lists prerequisite shas) is
// released against its pinned prerequisite closure instead of being refused. The request owns
// every step: no goroutine, queue or prerequisiteRuntime slot, and one enforced deadline.
const (
	// finalClosureBudget bounds the whole closure-backed FINAL request.
	finalClosureBudget = 6 * time.Second
	// finalClosureStmtTimeout bounds each database statement or transaction step.
	finalClosureStmtTimeout = 500 * time.Millisecond
	// finalClosureListReserve is the least request time left that still fits the 1s listing
	// window plus the 1.5s outcome reserve and the commit.
	finalClosureListReserve = 3 * time.Second
)

// PrerequisiteFinalObservation is the verdict of one pin listing.
type PrerequisiteFinalObservation int

const (
	// PrerequisiteFinalUnknown: the listing could not be made or trusted. Never absence.
	PrerequisiteFinalUnknown PrerequisiteFinalObservation = iota
	// PrerequisiteFinalPresent: the pin ref is listed at the pinned tip.
	PrerequisiteFinalPresent
	// PrerequisiteFinalAbsent: the pin ref was cleanly listed as absent.
	PrerequisiteFinalAbsent
	// PrerequisiteFinalForeign: the pin ref is listed at a different tip.
	PrerequisiteFinalForeign
)

// PrerequisiteFinalPort is the request-owned persistence and forge seam of a closure-backed
// FINAL. workersvc implements it over its prerequisite binding, listing and transactions; the
// handler adapts it (recovery and workersvc must not import each other).
type PrerequisiteFinalPort interface {
	// Reserve takes a fresh listing pass on the pinned closure under capture then closure
	// locks. false means the fence (state, generation, capture) no longer holds.
	Reserve(ctx context.Context, row store.RecoveryPrerequisiteClosure, pass uuid.UUID) (bool, error)
	// Observe lists the pin ref once, outside every transaction.
	Observe(ctx context.Context, row store.RecoveryPrerequisiteClosure) (PrerequisiteFinalObservation, error)
	// RecordAbsence and RecordForeign persist a definitive listing under the same fences and
	// report the rows moved.
	RecordAbsence(ctx context.Context, row store.RecoveryPrerequisiteClosure, pass uuid.UUID) (int64, error)
	RecordForeign(ctx context.Context, row store.RecoveryPrerequisiteClosure, pass uuid.UUID) (int64, error)
}

// SetPrerequisiteFinal wires the closure-backed FINAL port. Call once at construction.
func (s *Service) SetPrerequisiteFinal(port PrerequisiteFinalPort) { s.prereqFinal = port }

// SetPrerequisiteFinalAllow wires the per-worker admission limiter consulted once per
// closure-backed FINAL that survives the unlocked precheck. An admission is never refunded.
// A nil allow disables the feature.
func (s *Service) SetPrerequisiteFinalAllow(allow func(workerID uuid.UUID) bool) {
	s.prereqFinalAllow = allow
}

// PrerequisiteFinalEnabled is the one predicate for "this server releases thin captures against
// their closure": the GitHub knob is on, and both the port and the limiter are wired.
func (s *Service) PrerequisiteFinalEnabled() bool {
	return slices.Contains(s.prerequisiteForges, "github") && s.prereqFinal != nil && s.prereqFinalAllow != nil
}

func (s *Service) finalClosureStage(stage string) {
	if s.finalClosureHook != nil {
		s.finalClosureHook(stage)
	}
}

// finalStmt runs one database step under its own 500ms bound.
func finalStmt(ctx context.Context, fn func(context.Context) error) error {
	c, cancel := context.WithTimeout(ctx, finalClosureStmtTimeout)
	defer cancel()
	return fn(c)
}

// finalClosureView is what the unlocked precheck proved.
type finalClosureView struct {
	hold store.RecoveryCustodyHold
	cap  store.RecoveryCapture
	row  store.RecoveryPrerequisiteClosure
}

// finalClosureBound reports whether the closure row is the pinned closure of exactly this
// capture and hold: identity, checksum, prerequisites, a frozen GitHub.com binding and a
// recorded pin. It judges nothing about absence.
func finalClosureBound(row store.RecoveryPrerequisiteClosure, cap store.RecoveryCapture, hold store.RecoveryCustodyHold) bool {
	return row.CaptureID == cap.ID && row.HoldID == hold.ID && row.RunID == hold.RunID && row.UserID == hold.UserID &&
		row.State == "pinned" && cap.Checksum.Valid && row.Checksum == cap.Checksum.String &&
		len(cap.PrerequisiteShas) != 0 && slices.Equal(row.PrerequisiteShas, cap.PrerequisiteShas) &&
		row.ConnectionID.Valid && row.ForgeKind.Valid && row.ForgeKind.String == "github" &&
		row.NormalizedBaseUrl.Valid && row.NormalizedBaseUrl.String == "https://github.com" &&
		row.ProjectID.Valid && row.DefaultBranch.Valid && row.DefaultBranch.String != "" &&
		row.PinTip.Valid && row.PinTip.String != "" && row.PinRef != ""
}

// finalClosureCaptureOK is the capture's FINAL identity: declared source and coverage, bound,
// available, unexpired and thin.
func finalClosureCaptureOK(cap store.RecoveryCapture, f *apitypes.RecoveryFinalDisposition) bool {
	return cap.SourceSha == f.SourceSha && cap.CoverageDigest.String == f.CoverageDigest && cap.CoverageDigest.Valid &&
		cap.ManifestBound && len(cap.PrerequisiteShas) != 0 && cap.State == "available" &&
		cap.ExpiresAt.Valid && cap.ExpiresAt.Time.After(time.Now())
}

// finalClosurePrecheck reads the request's mutable state without locks and applies the exact
// refusals a thin FINAL gets before it may spend an admission. A non-nil ack is an exact replay.
// Every refusal here is final for this request except the typed pin_missing.
func (s *Service) finalClosurePrecheck(ctx context.Context, wkr store.Worker, runID uuid.UUID,
	req apitypes.RecoveryReleaseRequest, captureID uuid.UUID) (finalClosureView, *apitypes.RecoveryReleaseResponse, error) {
	f := req.FinalDisposition
	generation := *req.Generation
	capture := pgconv.UUID(captureID)
	q := store.New(s.pool)
	var v finalClosureView
	var run store.Run
	var caps []string
	err := finalStmt(ctx, func(c context.Context) (err error) {
		v.hold, err = q.GetHeldPublicationHold(c, store.GetHeldPublicationHoldParams{RunID: runID, UserID: wkr.UserID, WorkerID: wkr.ID, Generation: generation})
		return err
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return v, nil, ErrNotAuthorized
		}
		return v, nil, err
	}
	if v.hold.State == "released" {
		if finalReplayMatches(v.hold, f, capture, req.ReleaseEvidence) {
			ack := apitypes.RecoveryReleaseResponse{RunID: runID.String(), Released: true, HoldsReleased: 1, Generation: req.Generation}
			return v, &ack, nil
		}
		return v, nil, ErrManifestConflict
	}
	err = finalStmt(ctx, func(c context.Context) (err error) {
		run, err = q.GetRunByIDForUser(c, store.GetRunByIDForUserParams{ID: runID, UserID: wkr.UserID})
		return err
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return v, nil, ErrNotAuthorized
		}
		return v, nil, err
	}
	if !finalInventoryEligible(v.hold, wkr.ID, finalRunFacts{run.Status, run.ClaimGeneration, run.ClaimReleasedAt, run.RecoveryWaitCause}, generation, false) {
		return v, nil, ErrNotAuthorized
	}
	err = finalStmt(ctx, func(c context.Context) (err error) {
		v.cap, err = q.GetCaptureForOwner(c, store.GetCaptureForOwnerParams{ID: captureID, RunID: runID, UserID: wkr.UserID})
		return err
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return v, nil, ErrNotAuthorized
		}
		return v, nil, err
	}
	if v.cap.HoldID != v.hold.ID || v.cap.OriginalWorkerID != pgconv.UUID(wkr.ID) {
		return v, nil, ErrNotAuthorized
	}
	if !finalClosureCaptureOK(v.cap, f) {
		return v, nil, ErrManifestConflict
	}
	if s.limits.ReadyRetention < time.Second {
		return v, nil, ErrBadRequest
	}
	err = finalStmt(ctx, func(c context.Context) (err error) {
		v.row, err = q.GetPrerequisiteClosureForCapture(c, captureID)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return v, nil, ErrManifestConflict
	}
	if err != nil {
		return v, nil, err
	}
	err = finalStmt(ctx, func(c context.Context) error {
		return s.pool.QueryRow(c, "SELECT protocol_capabilities FROM workers WHERE id=$1 AND user_id=$2", wkr.ID, wkr.UserID).Scan(&caps)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return v, nil, ErrNotAuthorized
		}
		return v, nil, err
	}
	if !s.PrerequisiteFinalEnabled() || !finalClosureCapsOK(caps) || !finalClosureBound(v.row, v.cap, v.hold) {
		return v, nil, ErrManifestConflict
	}
	if v.row.AbsentSince.Valid {
		return v, nil, ErrPrerequisitePinMissing
	}
	return v, nil, nil
}

func finalClosureCapsOK(caps []string) bool {
	return slices.Contains(caps, capability.RecoveryInventoryV1) && slices.Contains(caps, capability.RecoveryPrerequisiteClosureV1)
}

// releaseFinalClosure answers an archive FINAL for a thin capture. Order is binding:
//
//  1. the caller already rolled back its transaction: no forge I/O under a lock;
//  2. unlocked precheck (replay, eligibility, capture, closure): every refusal that needs no
//     forge, before any admission is spent or any row reserved;
//  3. the per-worker limiter (one admission per request, never refunded);
//  4. a short transaction reserving a fresh listing pass on the pinned closure;
//  5. one pin listing outside every transaction;
//  6. a present pin releases under the full lock chain (worker, run, hold, capture, closure);
//     an absent or foreign pin is recorded under the same pass fence and refused.
//
// Nothing here deletes, extends or re-pins the closure's ref.
func (s *Service) releaseFinalClosure(ctx context.Context, wkr store.Worker, runID uuid.UUID,
	req apitypes.RecoveryReleaseRequest, captureID uuid.UUID) (apitypes.RecoveryReleaseResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, finalClosureBudget)
	defer cancel()
	view, ack, err := s.finalClosurePrecheck(ctx, wkr, runID, req, captureID)
	if err != nil {
		return apitypes.RecoveryReleaseResponse{}, err
	}
	if ack != nil {
		return *ack, nil
	}
	if !s.prereqFinalAllow(wkr.ID) {
		return apitypes.RecoveryReleaseResponse{}, ErrPrerequisitePinUnavailable
	}
	pass := uuid.New()
	reserved, err := s.prereqFinal.Reserve(ctx, view.row, pass)
	if err != nil {
		slog.Warn("closure final: listing reservation failed", "run", runID)
		return apitypes.RecoveryReleaseResponse{}, ErrPrerequisitePinUnavailable
	}
	s.finalClosureStage("reserved")
	if !reserved {
		// The fence moved: a racing identical FINAL may have released, or the state may no
		// longer be eligible. Answer that truthfully; otherwise the worker retries.
		_, ack, err = s.finalClosurePrecheck(ctx, wkr, runID, req, captureID)
		switch {
		case err != nil:
			return apitypes.RecoveryReleaseResponse{}, err
		case ack != nil:
			return *ack, nil
		}
		return apitypes.RecoveryReleaseResponse{}, ErrPrerequisitePinUnavailable
	}
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) < finalClosureListReserve {
		return apitypes.RecoveryReleaseResponse{}, ErrPrerequisitePinUnavailable
	}
	obs, err := s.prereqFinal.Observe(ctx, view.row)
	if err != nil {
		slog.Warn("closure final: pin listing failed", "run", runID)
		obs = PrerequisiteFinalUnknown
	}
	s.finalClosureStage("observed")
	if ctx.Err() != nil {
		return apitypes.RecoveryReleaseResponse{}, ErrPrerequisitePinUnavailable
	}
	switch obs {
	case PrerequisiteFinalPresent:
		resp, err := s.finalClosureCommit(ctx, wkr, runID, req, view, pass)
		if err != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
			// A timeout before the commit decided nothing: retry, never a 500.
			err = ErrPrerequisitePinUnavailable
		}
		return resp, err
	case PrerequisiteFinalAbsent:
		return apitypes.RecoveryReleaseResponse{}, s.finalClosureRecord(ctx, view.row, pass, s.prereqFinal.RecordAbsence, ErrPrerequisitePinMissing)
	case PrerequisiteFinalForeign:
		return apitypes.RecoveryReleaseResponse{}, s.finalClosureRecord(ctx, view.row, pass, s.prereqFinal.RecordForeign, ErrPrerequisitePinForeign)
	}
	return apitypes.RecoveryReleaseResponse{}, ErrPrerequisitePinUnavailable
}

// finalClosureRecord persists a definitive listing under a request-derived 1.5s bound. One row
// moved means the verdict is on record and refused; zero rows or an error means a newer
// reservation or state owns the closure, so nothing was overwritten and the worker retries.
func (s *Service) finalClosureRecord(ctx context.Context, row store.RecoveryPrerequisiteClosure, pass uuid.UUID,
	record func(context.Context, store.RecoveryPrerequisiteClosure, uuid.UUID) (int64, error), verdict error) error {
	w, wc := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer wc()
	n, err := record(w, row, pass)
	if err != nil || n != 1 {
		if err != nil {
			slog.Warn("closure final: listing outcome not recorded", "closure", row.ID)
		}
		return ErrPrerequisitePinUnavailable
	}
	return verdict
}

// finalClosureCommit is the locked release. Lock order: worker, run, hold, capture, closure.
// It re-authenticates and compares the receipt before any mutable re-check, then re-proves every
// fence of the unlocked phase under the locks.
func (s *Service) finalClosureCommit(ctx context.Context, wkr store.Worker, runID uuid.UUID,
	req apitypes.RecoveryReleaseRequest, view finalClosureView, pass uuid.UUID) (apitypes.RecoveryReleaseResponse, error) {
	f := req.FinalDisposition
	generation := *req.Generation
	capture := pgconv.UUID(view.cap.ID)
	var tx pgx.Tx
	if err := finalStmt(ctx, func(c context.Context) (err error) { tx, err = s.pool.Begin(c); return err }); err != nil {
		return apitypes.RecoveryReleaseResponse{}, ErrPrerequisitePinUnavailable
	}
	defer func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalClosureStmtTimeout)
		defer cancel()
		_ = tx.Rollback(c)
	}()
	notFound := func(err error) error {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotAuthorized
		}
		return err
	}
	var caps []string
	if err := finalStmt(ctx, func(c context.Context) error {
		return tx.QueryRow(c, "SELECT protocol_capabilities FROM workers WHERE id=$1 AND user_id=$2 FOR UPDATE", wkr.ID, wkr.UserID).Scan(&caps)
	}); err != nil {
		return apitypes.RecoveryReleaseResponse{}, notFound(err)
	}
	var run finalRunFacts
	if err := finalStmt(ctx, func(c context.Context) error {
		return tx.QueryRow(c, "SELECT status, claim_generation, claim_released_at, recovery_wait_cause FROM runs WHERE id=$1 AND user_id=$2 FOR UPDATE",
			runID, wkr.UserID).Scan(&run.status, &run.gen, &run.released, &run.parkCause)
	}); err != nil {
		return apitypes.RecoveryReleaseResponse{}, notFound(err)
	}
	q := store.New(tx)
	var hold store.RecoveryCustodyHold
	var cap store.RecoveryCapture
	var closure store.RecoveryPrerequisiteClosure
	err := finalStmt(ctx, func(c context.Context) (err error) {
		hold, err = q.GetFinalInventoryHold(c, store.GetFinalInventoryHoldParams{RunID: runID, UserID: wkr.UserID, WorkerID: wkr.ID, Generation: generation})
		return err
	})
	if err != nil {
		return apitypes.RecoveryReleaseResponse{}, notFound(err)
	}
	// The receipt compare precedes every mutable re-check: a racing identical FINAL that
	// committed is answered with its original ACK even after the worker lost its capability.
	if !hold.InventoryGuarded || hold.ID != view.hold.ID {
		return apitypes.RecoveryReleaseResponse{}, ErrNotAuthorized
	}
	if hold.State == "released" {
		if finalReplayMatches(hold, f, capture, req.ReleaseEvidence) {
			return apitypes.RecoveryReleaseResponse{RunID: runID.String(), Released: true, HoldsReleased: 1, Generation: req.Generation}, nil
		}
		return apitypes.RecoveryReleaseResponse{}, ErrManifestConflict
	}
	err = finalStmt(ctx, func(c context.Context) (err error) {
		cap, err = q.GetFinalInventoryCapture(c, store.GetFinalInventoryCaptureParams{ID: view.cap.ID, HoldID: hold.ID, RunID: runID, UserID: wkr.UserID, WorkerID: wkr.ID})
		return err
	})
	if err != nil {
		return apitypes.RecoveryReleaseResponse{}, notFound(err)
	}
	err = finalStmt(ctx, func(c context.Context) (err error) {
		closure, err = q.LockPrerequisiteClosure(c, view.row.ID)
		return err
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apitypes.RecoveryReleaseResponse{}, ErrManifestConflict
		}
		return apitypes.RecoveryReleaseResponse{}, err
	}
	if !finalClosureCapsOK(caps) || !s.PrerequisiteFinalEnabled() {
		return apitypes.RecoveryReleaseResponse{}, ErrManifestConflict
	}
	if !finalInventoryEligible(hold, wkr.ID, run, generation, false) {
		return apitypes.RecoveryReleaseResponse{}, ErrNotAuthorized
	}
	if !finalClosureCaptureOK(cap, f) || !finalClosureBound(closure, cap, hold) {
		return apitypes.RecoveryReleaseResponse{}, ErrManifestConflict
	}
	if closure.AbsentSince.Valid {
		return apitypes.RecoveryReleaseResponse{}, ErrPrerequisitePinMissing
	}
	// A newer reservation (cleanup, audit, another FINAL) owns the closure: this listing
	// proves nothing about the state it would release against.
	if closure.CleanupAttempts != view.row.CleanupAttempts || closure.LastAuditPassID != (pgtype.UUID{Bytes: pass, Valid: true}) ||
		(closure.CleanupDeadline.Valid && closure.CleanupDeadline.Time.After(time.Now())) {
		return apitypes.RecoveryReleaseResponse{}, ErrPrerequisitePinUnavailable
	}
	var n int64
	err = finalStmt(ctx, func(c context.Context) (err error) {
		n, err = q.ProtectFinalClosureCapture(c, store.ProtectFinalClosureCaptureParams{
			ID: cap.ID, HoldID: hold.ID, WorkerID: wkr.ID, RetentionSeconds: int64(s.limits.ReadyRetention.Seconds()),
			SourceSha: f.SourceSha, CoverageDigest: pgconv.TextOrNull(f.CoverageDigest), ClosureID: closure.ID,
		})
		return err
	})
	if err != nil {
		return apitypes.RecoveryReleaseResponse{}, err
	}
	if n != 1 {
		return apitypes.RecoveryReleaseResponse{}, ErrNotAuthorized
	}
	err = finalStmt(ctx, func(c context.Context) (err error) {
		n, err = q.ReleaseFinalClosureHold(c, store.ReleaseFinalClosureHoldParams{
			ID: hold.ID, RunID: runID, UserID: wkr.UserID, WorkerID: wkr.ID, Generation: generation,
			FinalCaptureID: cap.ID, FinalSourceSha: f.SourceSha, FinalCoverageDigest: f.CoverageDigest, ClosureID: closure.ID,
		})
		return err
	})
	if err != nil {
		return apitypes.RecoveryReleaseResponse{}, err
	}
	if n != 1 {
		return apitypes.RecoveryReleaseResponse{}, ErrNotAuthorized
	}
	if err := finalStmt(ctx, tx.Commit); err != nil {
		return apitypes.RecoveryReleaseResponse{}, err
	}
	s.custodySettledAfterCommit(runID)
	return apitypes.RecoveryReleaseResponse{RunID: runID.String(), Released: true, HoldsReleased: 1, Generation: req.Generation}, nil
}
