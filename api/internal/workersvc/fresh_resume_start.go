package workersvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The refusal reasons RefuseFreshResumeInTx writes on a transfer row (run_fresh_resumes.refusal_reason).
const (
	FreshRefusalInsufficientHeadroom = "insufficient_custody_headroom"
	FreshRefusalOwnerResume          = "owner_resume"
	FreshRefusalAdmissionDeadline    = "admission_deadline"
	FreshRefusalGateOff              = "gate_off"
	freshRefusalRunMoved             = "run_moved"
)

// FreshRefuseOutcome is what RefuseFreshResumeInTx did to a transfer row.
type FreshRefuseOutcome string

const (
	// FreshRefusedPending: a pending row was closed refused and the hold version bumped; the run was
	// never touched.
	FreshRefusedPending FreshRefuseOutcome = "refused_pending"
	// FreshRefusedRestored: a claimed row's pre-claim tuple was put back on the run, the row closed
	// refused and the hold version bumped.
	FreshRefusedRestored FreshRefuseOutcome = "refused_restored"
	// FreshRefusedRunMoved: a claimed row's run was no longer the destination's claim at G', so the row
	// was closed refused (run_moved) and the run was not written.
	FreshRefusedRunMoved FreshRefuseOutcome = "refused_run_moved"
	// FreshRefuseNoop: the row was already closed (or seeded), or does not exist; nothing was written.
	FreshRefuseNoop FreshRefuseOutcome = "noop"
	// FreshRefuseChanged: the row changed state between the unlocked read and the locks, so the lock
	// order this call took no longer fits it. Nothing was written; the caller retries from its start.
	FreshRefuseChanged FreshRefuseOutcome = "changed"
)

// RefuseOption tunes RefuseFreshResumeInTx.
type RefuseOption func(*refuseConfig)

type refuseConfig struct {
	noWait bool
	// holds, when set, is asked once the run and the transfer row are locked and the row is still in the
	// state the lock order was taken for; false means the condition that justified the refusal no longer
	// holds and nothing is written (FreshRefuseNoop).
	holds func(ctx context.Context, q *store.Queries) (bool, error)
}

// RefuseWhenHolds makes the refusal conditional: holds is evaluated under the run and transfer locks (on
// the same transaction, so each statement sees the committed state after the lock waits), and the row is
// refused only when it returns true. The sweep uses it to revalidate its unlocked candidate read.
func RefuseWhenHolds(holds func(ctx context.Context, q *store.Queries) (bool, error)) RefuseOption {
	return func(c *refuseConfig) { c.holds = holds }
}

// RefuseNoWait is for a caller that already holds a worker row lock (the claim finisher, the abandon
// route): the predecessor lock never waits and a held conflicting lock is SQLSTATE 55P03, which the
// caller retries like the claim finisher's other lock conflicts. The default (a sweep, an owner Resume)
// may block.
func RefuseNoWait() RefuseOption { return func(c *refuseConfig) { c.noWait = true } }

// RefuseFreshResumeInTx refuses one transfer row inside the caller's transaction (issue #4628, plan
// section 5). Every refusing path uses it: start, owner Resume, the claim and its assembly refusals, the
// sweep arms and the abandon route. The caller holds no run or transfer lock on this row yet; the lock
// order is:
//
//  0. A pending row: lock the run, then the transfer, close the transfer with reason, bump
//     runs.fresh_hold_version, stop. The run's tuple was never touched.
//  1. A claimed row: read saved_worker_id from the unlocked row.
//  2. FOR KEY SHARE on that worker BEFORE any run lock (NOWAIT with RefuseNoWait); a worker that is
//     gone is NULL.
//  3. Lock the run. 4. Lock the transfer and re-read it. 5. Opt in for this row.
//  6. The run is no longer the destination's claim at G': close the row refused/run_moved without
//     writing the run.
//  7. Otherwise restore the saved tuple (worker_id the locked worker, or NULL), close the row refused
//     and bump the hold version, then clear the opt-in.
//
// Never restored: claim_generation (it stays G'), codex_claim_epoch, codex_cap_hash and claim
// provenance. A non-nil error leaves the transaction unusable for the caller to roll back.
func RefuseFreshResumeInTx(ctx context.Context, tx pgx.Tx, frid uuid.UUID, reason string, opts ...RefuseOption) (FreshRefuseOutcome, error) {
	var cfg refuseConfig
	for _, o := range opts {
		o(&cfg)
	}
	q := store.New(tx)
	row, err := q.GetFreshResume(ctx, frid)
	if errors.Is(err, pgx.ErrNoRows) {
		return FreshRefuseNoop, nil
	}
	if err != nil {
		return "", err
	}
	switch row.State {
	case "pending":
		return refusePendingFreshResume(ctx, q, row, reason, cfg)
	case "claimed":
		return refuseClaimedFreshResume(ctx, q, row, reason, cfg)
	}
	return FreshRefuseNoop, nil
}

// refusePendingFreshResume is step 0: run lock, transfer lock, close, bump.
func refusePendingFreshResume(ctx context.Context, q *store.Queries, row store.RunFreshResume, reason string, cfg refuseConfig) (FreshRefuseOutcome, error) {
	if _, err := q.GetRunByIDForUpdate(ctx, row.RunID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return FreshRefuseNoop, nil
		}
		return "", err
	}
	locked, err := q.GetFreshResumeForUpdate(ctx, row.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return FreshRefuseNoop, nil
	}
	if err != nil {
		return "", err
	}
	switch locked.State {
	case "pending":
	case "claimed":
		// A claim committed between the unlocked read and our run lock; the worker lock the claimed
		// branch needs comes first, so this call cannot continue in order.
		return FreshRefuseChanged, nil
	default:
		return FreshRefuseNoop, nil
	}
	if ok, err := refuseHolds(ctx, q, cfg); err != nil || !ok {
		return FreshRefuseNoop, err
	}
	if _, err := q.CloseFreshResume(ctx, store.CloseFreshResumeParams{ID: locked.ID, State: "refused", Reason: reason}); err != nil {
		return "", err
	}
	if _, err := q.BumpFreshHoldVersion(ctx, locked.RunID); err != nil {
		return "", err
	}
	slog.Info("fresh resume refused", "transfer", locked.ID.String(), "run", locked.RunID.String(), "state", "pending", "reason", reason)
	return FreshRefusedPending, nil
}

// refuseHolds evaluates the optional RefuseWhenHolds condition; no condition holds trivially.
func refuseHolds(ctx context.Context, q *store.Queries, cfg refuseConfig) (bool, error) {
	if cfg.holds == nil {
		return true, nil
	}
	return cfg.holds(ctx, q)
}

// refuseClaimedFreshResume is steps 1 to 7.
func refuseClaimedFreshResume(ctx context.Context, q *store.Queries, row store.RunFreshResume, reason string, cfg refuseConfig) (FreshRefuseOutcome, error) {
	if row.SavedWorkerID.Valid {
		lock := q.LockWorkerKeyShare
		if cfg.noWait {
			lock = q.LockWorkerKeyShareNoWait
		}
		if _, err := lock(ctx, uuid.UUID(row.SavedWorkerID.Bytes)); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", err
		}
	}
	if _, err := q.GetRunByIDForUpdate(ctx, row.RunID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return FreshRefuseNoop, nil
		}
		return "", err
	}
	locked, err := q.GetFreshResumeForUpdate(ctx, row.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return FreshRefuseNoop, nil
	}
	if err != nil {
		return "", err
	}
	if locked.State != "claimed" {
		return FreshRefuseNoop, nil
	}
	if ok, err := refuseHolds(ctx, q, cfg); err != nil || !ok {
		return FreshRefuseNoop, err
	}
	if err := q.OptInFreshResume(ctx, locked.ID.String()); err != nil {
		return "", err
	}
	n, err := q.RestoreFreshResumeTuple(ctx, locked.ID)
	if err != nil {
		return "", err
	}
	outcome, closeReason := FreshRefusedRestored, reason
	if n == 0 {
		outcome, closeReason = FreshRefusedRunMoved, freshRefusalRunMoved
	}
	if _, err := q.CloseFreshResume(ctx, store.CloseFreshResumeParams{ID: locked.ID, State: "refused", Reason: closeReason}); err != nil {
		return "", err
	}
	if err := q.ClearFreshResumeOptIn(ctx); err != nil {
		return "", err
	}
	slog.Info("fresh resume refused", "transfer", locked.ID.String(), "run", locked.RunID.String(), "state", "claimed",
		"reason", closeReason, "outcome", string(outcome))
	return outcome, nil
}

// FreshResumeStartResult is the disposition of StartFreshResume: the transfer row, and whether it is the
// row an earlier identical request already created.
type FreshResumeStartResult struct {
	Row    store.RunFreshResume
	Replay bool
}

// StartFreshResume is POST /api/runs/{id}/fresh-resume (issue #4628, plan section 4 "Start"). The
// forge checks run outside the transaction; then, inside one READ COMMITTED transaction:
//
//	owner advisory lock (first), run FOR UPDATE, then the capture FOR SHARE with its availability facts
//	in the same statement. A known (run, request_id) is resolved BEFORE eligibility: an active request
//	returns its row, a completed one is request_completed, and neither creates anything. Otherwise the
//	gates, the token against the locked run, the candidate facts and the owner's headroom are rechecked
//	and one pending row is inserted, its user_id taken from the locked run row. No runs column is
//	written: the run stays held and its worker_recovery_episode is untouched.
//
// Refusals are *FreshResumeRefused: disabled, stale_selection, request_completed, in_progress,
// ineligible, no_destination, custody_limit and the evidence reasons.
func (s *Service) StartFreshResume(ctx context.Context, userID, runID uuid.UUID, selectionToken string, requestID uuid.UUID) (FreshResumeStartResult, error) {
	refuse := func(reason string) (FreshResumeStartResult, error) {
		return FreshResumeStartResult{}, &FreshResumeRefused{Reason: reason}
	}
	if !s.p.FreshResume {
		return refuse(apitypes.FreshResumeStatusDisabled)
	}
	if s.txBeginner == nil {
		return FreshResumeStartResult{}, ErrFreshResumeUnavailable
	}

	// Step 1: the facts and the forge proofs, with no transaction open. A request id this run already
	// knows skips the proofs: it is resolved from its row below.
	var sel freshSelection
	known, err := s.freshRequestKnown(ctx, userID, runID, requestID)
	if err != nil {
		return FreshResumeStartResult{}, err
	}
	if !known {
		facts, err := s.freshLoad(ctx, userID, runID)
		if err != nil {
			return FreshResumeStartResult{}, err
		}
		sel = s.decideFreshEvidence(ctx, facts)
	}

	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return FreshResumeStartResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)

	// Step 2: the owner lock, before any row lock.
	if err := store.LockFreshResumeOwner(ctx, tx, userID); err != nil {
		return FreshResumeStartResult{}, err
	}
	// Step 3: the run, then (below) the capture.
	run, err := q.GetRunByIDForUpdate(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && run.UserID != userID) {
		return FreshResumeStartResult{}, ErrRunNotFound
	}
	if err != nil {
		return FreshResumeStartResult{}, err
	}

	// A known request resolves before eligibility, so a retry after the transfer was claimed returns the
	// existing disposition instead of failing the gates.
	switch existing, err := q.GetFreshResumeByRequest(ctx, store.GetFreshResumeByRequestParams{RunID: runID, RequestID: requestID}); {
	case err == nil:
		switch existing.State {
		case "pending", "claimed", "seeded", "superseded":
			return FreshResumeStartResult{Row: existing, Replay: true}, nil
		default:
			return refuse(apitypes.FreshResumeReasonRequestCompleted)
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return FreshResumeStartResult{}, err
	}
	if known {
		// The row existed when the proofs were skipped, so it can only have been removed with its run.
		return refuse(apitypes.FreshResumeReasonStaleSelection)
	}

	// Step 4: eligibility, the candidate's facts, the token and the headroom, under the locks.
	status, err := s.freshGates(ctx, q, run)
	if err != nil {
		return FreshResumeStartResult{}, err
	}
	if status != "" {
		return refuse(status)
	}
	if sel.Status != apitypes.FreshResumeStatusAvailable {
		return refuse(sel.Status)
	}
	// The token binds the run's latest claim generation and hold version, both read under the lock.
	if sel.G != run.ClaimGeneration || sel.Version != run.FreshHoldVersion ||
		freshToken(run.ID, run.ClaimGeneration, run.FreshHoldVersion, sel.Ge, sel.Chosen) != selectionToken {
		return refuse(apitypes.FreshResumeReasonStaleSelection)
	}
	if reason, err := s.freshLockCandidate(ctx, q, run, sel); err != nil {
		return FreshResumeStartResult{}, err
	} else if reason != "" {
		return refuse(reason)
	}

	// Step 5: the pending row. user_id comes from the locked run row, never the request.
	params := store.InsertFreshResumePendingParams{
		RunID: run.ID, UserID: run.UserID, RequestID: requestID,
		EvidenceGeneration: sel.Ge, EvidenceKind: sel.Chosen.kind, Sha: sel.Chosen.sha, CandidateToken: selectionToken,
	}
	if sel.Chosen.kind == apitypes.FreshResumeKindCapture {
		params.CaptureID = pgtype.UUID{Bytes: sel.Chosen.captureID, Valid: true}
	} else {
		params.CheckpointRef = pgconv.TextOrNull(sel.Chosen.ref)
	}
	row, err := q.InsertFreshResumePending(ctx, params)
	if err != nil {
		if store.IsFreshResumeRequestConflict(err) {
			return refuse(apitypes.FreshResumeReasonRequestCompleted)
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "run_fresh_resumes_one_active_idx" {
			return refuse(apitypes.FreshResumeStatusInProgress)
		}
		return FreshResumeStartResult{}, err
	}
	// The transfer row is the owner action's audit record (who, when, which evidence, which request);
	// run_user_inputs has no kind for it and run_messages appends are fenced on an unreleased claim,
	// which a held run does not have. The log line names it for the operator trail.
	slog.Info("fresh resume started", "transfer", row.ID.String(), "run", run.ID.String(), "user", run.UserID.String(),
		"request", requestID.String(), "evidence_kind", row.EvidenceKind, "evidence_generation", row.EvidenceGeneration, "sha", row.Sha)
	// Step 6 is the absence of a runs write; step 7 is the absence of any supersede: a chained start
	// leaves the seeded row in place until the new row's seeded receipt.
	if err := tx.Commit(ctx); err != nil {
		return FreshResumeStartResult{}, err
	}
	return FreshResumeStartResult{Row: row}, nil
}

// freshLockCandidate re-verifies the chosen evidence under the run lock and, for a capture, locks it
// FOR SHARE with its availability facts in the same statement. It returns a refusal reason, or "".
func (s *Service) freshLockCandidate(ctx context.Context, q *store.Queries, run store.Run, sel freshSelection) (string, error) {
	c := sel.Chosen
	if c.kind == apitypes.FreshResumeKindCapture {
		_, err := q.LockCaptureForFreshResume(ctx, store.LockCaptureForFreshResumeParams{
			ID: c.captureID, RunID: run.ID, UserID: run.UserID, EvidenceGeneration: sel.Ge, Sha: c.sha,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return apitypes.FreshResumeStatusExpired, nil
		}
		return "", err
	}
	tipOK := run.CheckpointTip.Valid && run.CheckpointTip.String == c.sha &&
		run.CheckpointTipGeneration.Valid && run.CheckpointTipGeneration.Int64 == sel.Ge &&
		run.CheckpointTipGenerationTip.Valid && run.CheckpointTipGenerationTip.String == c.sha
	if !tipOK {
		return apitypes.FreshResumeReasonStaleSelection, nil
	}
	holds, err := q.CountOpenHoldsForFreshRun(ctx, store.CountOpenHoldsForFreshRunParams{RunID: run.ID, UserID: run.UserID})
	if err != nil {
		return "", err
	}
	if holds == 0 {
		// The same status the preview gives a stamped tip at Ge on a run with no open hold.
		return apitypes.FreshResumeStatusNoEvidence, nil
	}
	return "", nil
}

// freshRequestKnown reports whether (run, request_id) already has a transfer row, from a short unlocked
// read. It lets a replay skip the forge proofs; the locked read below stays authoritative.
func (s *Service) freshRequestKnown(ctx context.Context, userID, runID, requestID uuid.UUID) (bool, error) {
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	if _, err := q.GetRunByIDForUser(ctx, store.GetRunByIDForUserParams{ID: runID, UserID: userID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrRunNotFound
		}
		return false, err
	}
	_, err = q.GetFreshResumeByRequest(ctx, store.GetFreshResumeByRequestParams{RunID: runID, RequestID: requestID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// freshLoad loads the selection facts in one short read transaction.
func (s *Service) freshLoad(ctx context.Context, userID, runID uuid.UUID) (freshFacts, error) {
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return freshFacts{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return loadFreshFacts(ctx, store.New(tx), userID, runID)
}

// ResumeWorkerRecoveryFresh is the owner Resume of a worker-exhaustion recovery_wait through the fresh
// resume lock order (plan section 4 "Owner lock" and "Owner Resume on a pending row"). handled is false
// when the plain autocommit path applies: the feature is off and the owner has no outstanding transfer.
// Otherwise, in one READ COMMITTED transaction: the owner lock; the worker FOR UPDATE from the unlocked
// read of runs.worker_id; the run, rechecking worker_id; the transfer; then ResumeWorkerRecoveryEpisode.
// A pending transfer is refused first (owner_resume, which also bumps the hold version) and the Resume
// proceeds; a claimed transfer is a *FreshResumeRefused in_progress.
func (s *Service) ResumeWorkerRecoveryFresh(ctx context.Context, userID uuid.UUID, observed store.Run, timeout int32) (handled bool, err error) {
	if s.txBeginner == nil {
		return false, nil
	}
	// Only the run kinds fn_fresh_resume_run_base admits can carry a transfer row (the guard trigger is
	// the backstop). Any other kind, a cross_check checker above all, takes the plain path: this
	// function locks the run before ResumeWorkerRecoveryEpisode locks a checker's parent lead, and
	// parents lock before checkers.
	switch observed.Kind {
	case "issue", "mr_rework", "self_improve":
	default:
		return false, nil
	}
	if !s.p.FreshResume {
		// Fail closed: an unreadable probe is handled=true with the error, so the caller answers 500
		// rather than taking the plain path past a possible pending transfer.
		probe, err := s.beginReadCommitted(ctx)
		if err != nil {
			slog.Error("fresh resume: owner resume probe", "error", err, "run", observed.ID.String())
			return true, err
		}
		outstanding, err := store.New(probe).OwnerHasOutstandingFreshResume(ctx, userID)
		_ = probe.Rollback(ctx)
		if err != nil {
			slog.Error("fresh resume: owner resume probe", "error", err, "run", observed.ID.String())
			return true, err
		}
		if !outstanding {
			return false, nil
		}
	}
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return true, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	if err := store.LockFreshResumeOwner(ctx, tx, userID); err != nil {
		return true, err
	}
	if observed.WorkerID.Valid {
		if _, err := q.GetWorkerForUpdate(ctx, uuid.UUID(observed.WorkerID.Bytes)); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return true, err
		}
	}
	run, err := q.GetRunByIDForUpdate(ctx, observed.ID)
	if err != nil {
		return true, err
	}
	if run.UserID != userID || run.WorkerID != observed.WorkerID {
		// The run moved to another worker after the unlocked read: the worker lock we hold is not its
		// worker's. Fail closed as a changed hold.
		return true, pgx.ErrNoRows
	}
	switch row, err := q.GetActiveFreshResumeForRunForUpdate(ctx, run.ID); {
	case err == nil:
		if row.State != "pending" {
			return true, &FreshResumeRefused{Reason: apitypes.FreshResumeStatusInProgress}
		}
		outcome, err := RefuseFreshResumeInTx(ctx, tx, row.ID, FreshRefusalOwnerResume)
		if err != nil {
			return true, err
		}
		if outcome != FreshRefusedPending {
			return true, &FreshResumeRefused{Reason: apitypes.FreshResumeStatusInProgress}
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return true, err
	}
	if _, err := q.ResumeWorkerRecoveryEpisode(ctx, store.ResumeWorkerRecoveryEpisodeParams{ID: observed.ID, UserID: userID, GlobalTimeoutSeconds: timeout}); err != nil {
		return true, err
	}
	if err := tx.Commit(ctx); err != nil {
		return true, fmt.Errorf("commit owner resume: %w", err)
	}
	return true, nil
}
