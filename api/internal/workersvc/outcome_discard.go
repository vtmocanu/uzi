package workersvc

// Owner-confirmed discard of a worker-held `run_not_owned` terminal outcome (issue #2984).
//
// The holding worker REPORTS its blocked journals (ReplaceBlockedOutcomes). A principal who owns
// BOTH the run and that worker authorizes one exact journal (AuthorizeOutcomeDiscard). The worker
// pulls the directive, takes lease-fenced one-shot authority (BeginOutcomeDiscard), recomputes
// its loss manifest and unlinks exactly that journal, then reports (CompleteOutcomeDiscard). A
// separate non-deleting path (ReconcileOutcomeDiscard) settles directives whose journal is gone.
// Nothing the worker reports back is proof the journal is gone: completed and completed_unknown
// both read as "outcome unknown" to the owner.
//
// LOCK ORDER (queries/worker_outcome_discards.sql repeats it): L0 workers row FOR KEY SHARE, L1
// advisory lock (WorkerOutcomeDiscardLockClass, worker), L2 reported-outcome rows, L3 directive
// rows. Run and user rows are only read, never locked. No network I/O runs inside these
// transactions. Every transaction opens through outcomeDiscardTx so L0 and L1 are always first.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/outcomeloss"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Timing of a discard directive.
const (
	// OutcomeDiscardAuthorizationTTL is how long an owner's confirmation stays usable.
	OutcomeDiscardAuthorizationTTL = 15 * time.Minute
	// OutcomeDiscardLease is the one-shot execution lease /begin grants.
	OutcomeDiscardLease = 120 * time.Second
	// OutcomeDiscardGrace is how long past lease_until an executing row survives before it is
	// expired. The worker's own deadline is lease - 30s, so the grace absorbs a late /complete.
	OutcomeDiscardGrace = 30 * time.Second
	// OutcomeDiscardRetention is how long final directives and unreported outcome rows are kept.
	OutcomeDiscardRetention = 30 * 24 * time.Hour

	outcomeDiscardSweepBatches = 20
)

// Owner refusal codes of AuthorizeOutcomeDiscard (409).
const (
	DiscardRefusalNotBlocked          = "not_blocked"
	DiscardRefusalReasonNotDiscard    = "reason_not_discardable"
	DiscardRefusalAttributed          = "attributed"
	DiscardRefusalJournalChanged      = "journal_changed"
	DiscardRefusalLossChanged         = "loss_changed"
	DiscardRefusalManifestIncomplete  = "manifest_incomplete"
	DiscardRefusalCustodyHoldOpen     = "custody_hold_open"
	DiscardRefusalExecutionUnresolved = "execution_unresolved"
)

var (
	// ErrInvalidBlockedOutcome is a report that failed validation (400).
	ErrInvalidBlockedOutcome = errors.New("invalid blocked outcome report")
	// ErrOutcomeDiscardNotFound is an unknown directive, or one of another worker or owner (404).
	ErrOutcomeDiscardNotFound = errors.New("outcome discard not found")
	// ErrOutcomeDiscardConflict is a transition the directive's state does not allow (409).
	ErrOutcomeDiscardConflict = errors.New("outcome discard state conflict")
	// ErrInvalidOutcomeDiscardRequest is a malformed owner or worker request body (400).
	ErrInvalidOutcomeDiscardRequest = errors.New("invalid outcome discard request")
	errOutcomeDiscardUnavailable    = errors.New("outcome discard: transactions are not wired")
	// ErrOutcomeWorkerGone is a worker row deleted while a request was in flight (404).
	ErrOutcomeWorkerGone = errors.New("outcome discard: worker no longer exists")
)

// OutcomeDiscardRefusal is a 409 from AuthorizeOutcomeDiscard with a closed machine code.
type OutcomeDiscardRefusal struct{ Code string }

func (e *OutcomeDiscardRefusal) Error() string { return "outcome discard refused: " + e.Code }

func discardRefusal(code string) error { return &OutcomeDiscardRefusal{Code: code} }

// outcomeDiscardTx runs fn in a transaction that holds L0 and L1 for workerID before anything else.
func (s *Service) outcomeDiscardTx(ctx context.Context, workerID uuid.UUID, fn func(q *store.Queries) error) error {
	if s.txBeginner == nil {
		return errOutcomeDiscardUnavailable
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	if _, err := q.LockWorkerForOutcomeDiscard(ctx, workerID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrOutcomeWorkerGone
		}
		return err
	}
	if err := q.LockWorkerOutcomeDiscards(ctx, workerID); err != nil {
		return err
	}
	if err := fn(q); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ReplaceBlockedOutcomes validates a worker's whole report set, then replaces the stored set
// wholesale (L0, L1, L2; never a directive row).
func (s *Service) ReplaceBlockedOutcomes(ctx context.Context, wkr store.Worker, reports []apitypes.BlockedOutcomeReport) error {
	if len(reports) > outcomeloss.MaxOutcomesPerPut {
		return ErrInvalidBlockedOutcome
	}
	type key struct {
		run uuid.UUID
		gen int64
	}
	seen := map[key]bool{}
	losses := make([][]byte, len(reports))
	for i := range reports {
		r := &reports[i]
		if _, err := outcomeloss.ValidateReport(wkr.ID, r); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidBlockedOutcome, err)
		}
		k := key{uuid.MustParse(r.RunID), r.ClaimGeneration} // ValidateReport proved the uuid
		if seen[k] {
			return fmt.Errorf("%w: duplicate run and generation", ErrInvalidBlockedOutcome)
		}
		seen[k] = true
		if r.Loss != nil {
			b, err := json.Marshal(r.Loss)
			if err != nil {
				return err
			}
			losses[i] = b
		}
	}
	now := s.now()
	return s.outcomeDiscardTx(ctx, wkr.ID, func(q *store.Queries) error {
		if err := q.DeleteWorkerBlockedOutcomes(ctx, wkr.ID); err != nil {
			return err
		}
		for i, r := range reports {
			if err := q.InsertWorkerBlockedOutcome(ctx, store.InsertWorkerBlockedOutcomeParams{
				WorkerID: wkr.ID, RunID: uuid.MustParse(r.RunID), ClaimGeneration: r.ClaimGeneration,
				JournalSha256: r.JournalSHA256, BlockedReason: r.BlockedReason, LossComplete: r.LossComplete,
				LossSha256: r.LossSHA256, IncompleteReason: r.IncompleteReason, Loss: losses[i],
				ReportedAt: pgconv.Time(now),
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

// outcomeDiscardView renders a directive row at the server's clock.
func outcomeDiscardView(d store.WorkerOutcomeDiscard, now time.Time) apitypes.OutcomeDiscard {
	v := apitypes.OutcomeDiscard{
		ID: d.ID.String(), WorkerID: d.WorkerID.String(), RunID: d.RunID.String(),
		ClaimGeneration: d.ClaimGeneration, JournalSHA256: d.JournalSha256, LossSHA256: d.LossSha256,
		State: d.State, Reason: d.ResultReason.String, ExpiresAt: d.ExpiresAt.Time.UTC(),
		CreatedAt: d.CreatedAt.Time.UTC(), UpdatedAt: d.UpdatedAt.Time.UTC(),
	}
	if d.State == apitypes.OutcomeDiscardExecuting {
		lease := d.LeaseUntil.Time.UTC()
		v.LeaseUntil = &lease
		if rem := lease.Sub(now).Milliseconds(); rem > 0 {
			v.RemainingMS = rem
		}
	}
	return v
}

// dueExpiry reports whether a live directive is past its deadline at now, and the reason.
func dueExpiry(d store.WorkerOutcomeDiscard, now time.Time) (string, bool) {
	switch d.State {
	case apitypes.OutcomeDiscardAuthorized:
		if !now.Before(d.ExpiresAt.Time) {
			return "authorization_expired", true
		}
	case apitypes.OutcomeDiscardExecuting:
		if !now.Before(d.LeaseUntil.Time.Add(OutcomeDiscardGrace)) {
			return "lease_expired", true
		}
	}
	return "", false
}

func finishDiscard(ctx context.Context, q *store.Queries, d store.WorkerOutcomeDiscard, state, reason string, now time.Time, from ...string) (store.WorkerOutcomeDiscard, error) {
	out, err := q.FinishOutcomeDiscard(ctx, store.FinishOutcomeDiscardParams{
		ID: d.ID, State: state, Reason: pgtype.Text{String: reason, Valid: true}, Now: pgconv.Time(now), FromStates: from,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return d, ErrOutcomeDiscardConflict // cannot happen under L3; fail closed rather than guess
	}
	return out, err
}

// expireIfDue applies lazy expiry to a locked directive row and returns the row as it now stands.
func expireIfDue(ctx context.Context, q *store.Queries, d store.WorkerOutcomeDiscard, now time.Time) (store.WorkerOutcomeDiscard, error) {
	reason, due := dueExpiry(d, now)
	if !due {
		return d, nil
	}
	return finishDiscard(ctx, q, d, apitypes.OutcomeDiscardExpired, reason, now, d.State)
}

// ListWorkerOutcomeDiscards returns the worker's authorized and executing directives, after lazy
// expiry of the ones past their deadline.
func (s *Service) ListWorkerOutcomeDiscards(ctx context.Context, wkr store.Worker) ([]apitypes.OutcomeDiscard, error) {
	now := s.now()
	var rows []store.WorkerOutcomeDiscard
	err := s.outcomeDiscardTx(ctx, wkr.ID, func(q *store.Queries) error {
		if _, err := q.ExpireOutcomeDiscardsForWorker(ctx, store.ExpireOutcomeDiscardsForWorkerParams{
			Now: pgconv.Time(now), WorkerID: wkr.ID, GraceSeconds: int32(OutcomeDiscardGrace.Seconds()),
		}); err != nil {
			return err
		}
		var err error
		rows, err = q.ListLiveOutcomeDiscardsForWorker(ctx, wkr.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := make([]apitypes.OutcomeDiscard, 0, len(rows))
	for _, d := range rows {
		out = append(out, outcomeDiscardView(d, now))
	}
	return out, nil
}

// lockedDirectiveForWorker loads the worker's own directive under L2 (when the journal's report
// row exists) and L3. A directive of another worker or an unknown id is not found.
func lockedDirectiveForWorker(ctx context.Context, q *store.Queries, workerID, id uuid.UUID, lockReport bool) (store.WorkerOutcomeDiscard, *store.WorkerBlockedOutcome, error) {
	peek, err := q.GetOutcomeDiscard(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.WorkerOutcomeDiscard{}, nil, ErrOutcomeDiscardNotFound
		}
		return store.WorkerOutcomeDiscard{}, nil, err
	}
	if peek.WorkerID != workerID {
		return store.WorkerOutcomeDiscard{}, nil, ErrOutcomeDiscardNotFound
	}
	var report *store.WorkerBlockedOutcome
	if lockReport { // L2 strictly before L3
		row, err := q.GetWorkerBlockedOutcomeForUpdate(ctx, store.GetWorkerBlockedOutcomeForUpdateParams{
			WorkerID: workerID, RunID: peek.RunID, ClaimGeneration: peek.ClaimGeneration,
		})
		switch {
		case err == nil:
			report = &row
		case !errors.Is(err, pgx.ErrNoRows):
			return store.WorkerOutcomeDiscard{}, nil, err
		}
	}
	d, err := q.GetOutcomeDiscardForUpdate(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.WorkerOutcomeDiscard{}, nil, ErrOutcomeDiscardNotFound
		}
		return store.WorkerOutcomeDiscard{}, nil, err
	}
	return d, report, nil
}

// BeginOutcomeDiscard grants the worker one-shot, lease-fenced authority to delete the journal.
// Unknown or foreign ids are ErrOutcomeDiscardNotFound. Every other outcome is a view of the
// directive: state "executing" is the only grant; a repeated call while the lease runs returns
// the same tuple and remaining_ms and never extends it; a re-check failure refuses the directive.
func (s *Service) BeginOutcomeDiscard(ctx context.Context, wkr store.Worker, id uuid.UUID) (apitypes.OutcomeDiscard, error) {
	now := s.now()
	var out apitypes.OutcomeDiscard
	err := s.outcomeDiscardTx(ctx, wkr.ID, func(q *store.Queries) error {
		d, report, err := lockedDirectiveForWorker(ctx, q, wkr.ID, id, true)
		if err != nil {
			return err
		}
		if d, err = expireIfDue(ctx, q, d, now); err != nil {
			return err
		}
		switch d.State {
		case apitypes.OutcomeDiscardExecuting:
			out = outcomeDiscardView(d, now) // idempotent: same tuple, lease never extended
			return nil
		case apitypes.OutcomeDiscardAuthorized:
		default:
			out = outcomeDiscardView(d, now)
			return nil
		}
		if code := s.recheckDiscard(ctx, q, wkr, d, report); code != "" {
			d, err = finishDiscard(ctx, q, d, apitypes.OutcomeDiscardRefused, code, now, apitypes.OutcomeDiscardAuthorized)
			out = outcomeDiscardView(d, now)
			return err
		}
		d, err = q.BeginOutcomeDiscard(ctx, store.BeginOutcomeDiscardParams{
			ID: d.ID, LeaseUntil: pgconv.Time(now.Add(OutcomeDiscardLease)), Now: pgconv.Time(now),
		})
		if err != nil {
			return err
		}
		out = outcomeDiscardView(d, now)
		return nil
	})
	return out, err
}

// recheckDiscard re-verifies, at /begin, every fact the owner's confirmation depended on. It
// returns a refusal reason, or "" when the directive may execute.
func (s *Service) recheckDiscard(ctx context.Context, q *store.Queries, wkr store.Worker, d store.WorkerOutcomeDiscard, report *store.WorkerBlockedOutcome) string {
	if _, err := q.GetWorkerByIDForUser(ctx, store.GetWorkerByIDForUserParams{ID: wkr.ID, UserID: wkr.UserID}); err != nil {
		return "worker_not_owned"
	}
	run, err := q.GetRunByIDForUser(ctx, store.GetRunByIDForUserParams{ID: d.RunID, UserID: wkr.UserID})
	if err != nil {
		return "run_not_owned_by_worker_owner"
	}
	if run.WorkerID.Valid && uuid.UUID(run.WorkerID.Bytes) == wkr.ID {
		return DiscardRefusalAttributed
	}
	if held, err := q.OpenCustodyHoldForWorkerRun(ctx, store.OpenCustodyHoldForWorkerRunParams{
		RunID: d.RunID, WorkerID: pgconv.UUID(wkr.ID),
	}); err != nil || held {
		return DiscardRefusalCustodyHoldOpen
	}
	switch {
	case report == nil:
		return DiscardRefusalNotBlocked
	case report.JournalSha256 != d.JournalSha256:
		return DiscardRefusalJournalChanged
	case report.BlockedReason != outcomeloss.ReasonRunNotOwned:
		return DiscardRefusalReasonNotDiscard
	case !report.LossComplete:
		return DiscardRefusalManifestIncomplete
	case report.LossSha256 != d.LossSha256:
		return DiscardRefusalLossChanged
	}
	return ""
}

// CompleteOutcomeDiscard records the worker's report of an execution. Only an executing directive
// of this worker accepts it; the same result again is idempotent; a different result is a conflict.
// The report is never proof the journal is gone.
func (s *Service) CompleteOutcomeDiscard(ctx context.Context, wkr store.Worker, id uuid.UUID, req apitypes.OutcomeDiscardCompleteRequest) (apitypes.OutcomeDiscard, error) {
	var target, reason string
	switch req.Result {
	case apitypes.OutcomeDiscardResultDeleted:
		if req.Reason != "" {
			return apitypes.OutcomeDiscard{}, ErrInvalidOutcomeDiscardRequest
		}
		target, reason = apitypes.OutcomeDiscardCompleted, "deleted"
	case apitypes.OutcomeDiscardResultRefused:
		target, reason = apitypes.OutcomeDiscardRefused, req.Reason
	case apitypes.OutcomeDiscardResultUnsynced:
		target, reason = apitypes.OutcomeDiscardCompletedUnknown, req.Reason
	default:
		return apitypes.OutcomeDiscard{}, ErrInvalidOutcomeDiscardRequest
	}
	if req.Result != apitypes.OutcomeDiscardResultDeleted && !outcomeloss.ValidReasonCode(req.Reason) {
		return apitypes.OutcomeDiscard{}, ErrInvalidOutcomeDiscardRequest
	}
	now := s.now()
	var out apitypes.OutcomeDiscard
	err := s.outcomeDiscardTx(ctx, wkr.ID, func(q *store.Queries) error {
		d, _, err := lockedDirectiveForWorker(ctx, q, wkr.ID, id, false)
		if err != nil {
			return err
		}
		if d.State == target {
			out = outcomeDiscardView(d, now)
			return nil
		}
		if d.State != apitypes.OutcomeDiscardExecuting {
			return ErrOutcomeDiscardConflict
		}
		d, err = finishDiscard(ctx, q, d, target, reason, now, apitypes.OutcomeDiscardExecuting)
		out = outcomeDiscardView(d, now)
		return err
	})
	return out, err
}

// ReconcileOutcomeDiscard settles a directive whose journal the worker observed absent or
// unverifiable. It never requires a reported row and never carries deletion authority: absent
// after /begin is completed_unknown, absent before it is refused, unverifiable is refused.
func (s *Service) ReconcileOutcomeDiscard(ctx context.Context, wkr store.Worker, id uuid.UUID, req apitypes.OutcomeDiscardReconcileRequest) (apitypes.OutcomeDiscard, error) {
	if req.Observation != apitypes.OutcomeDiscardObservationAbsent && req.Observation != apitypes.OutcomeDiscardObservationPresentUnverified {
		return apitypes.OutcomeDiscard{}, ErrInvalidOutcomeDiscardRequest
	}
	now := s.now()
	var out apitypes.OutcomeDiscard
	err := s.outcomeDiscardTx(ctx, wkr.ID, func(q *store.Queries) error {
		d, _, err := lockedDirectiveForWorker(ctx, q, wkr.ID, id, false)
		if err != nil {
			return err
		}
		var state, reason string
		switch {
		case req.Observation == apitypes.OutcomeDiscardObservationPresentUnverified:
			state, reason = apitypes.OutcomeDiscardRefused, "journal_unverifiable"
		case d.State == apitypes.OutcomeDiscardExecuting:
			state, reason = apitypes.OutcomeDiscardCompletedUnknown, "journal_absent_after_begin"
		default:
			state, reason = apitypes.OutcomeDiscardRefused, "journal_absent"
		}
		if d.State != apitypes.OutcomeDiscardExecuting && d.State != apitypes.OutcomeDiscardAuthorized {
			// Already final: idempotent only when this observation maps to the recorded outcome.
			if req.Observation == apitypes.OutcomeDiscardObservationPresentUnverified {
				if d.State == state && d.ResultReason.String == reason {
					out = outcomeDiscardView(d, now)
					return nil
				}
				return ErrOutcomeDiscardConflict
			}
			if (d.State == apitypes.OutcomeDiscardCompletedUnknown && d.ResultReason.String == "journal_absent_after_begin") ||
				(d.State == apitypes.OutcomeDiscardRefused && d.ResultReason.String == "journal_absent") {
				out = outcomeDiscardView(d, now)
				return nil
			}
			return ErrOutcomeDiscardConflict
		}
		d, err = finishDiscard(ctx, q, d, state, reason, now, d.State)
		out = outcomeDiscardView(d, now)
		return err
	})
	return out, err
}

// ownerWorker proves userID owns workerID (plain read). Anything else is ErrWorkerNotFound.
func (s *Service) ownerWorker(ctx context.Context, q interface {
	GetWorkerByIDForUser(context.Context, store.GetWorkerByIDForUserParams) (store.Worker, error)
}, userID, workerID uuid.UUID) error {
	if _, err := q.GetWorkerByIDForUser(ctx, store.GetWorkerByIDForUserParams{ID: workerID, UserID: userID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrWorkerNotFound
		}
		return err
	}
	return nil
}

// ListBlockedOutcomesForOwner returns the worker's reported outcomes of runs the caller owns,
// with each digest recomputed from the stored manifest. ErrWorkerNotFound when the caller does
// not own the worker.
func (s *Service) ListBlockedOutcomesForOwner(ctx context.Context, userID, workerID uuid.UUID) ([]apitypes.OwnerBlockedOutcome, error) {
	if s.txBeginner == nil {
		return nil, errOutcomeDiscardUnavailable
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	if err := s.ownerWorker(ctx, q, userID, workerID); err != nil {
		return nil, err
	}
	rows, err := q.ListWorkerBlockedOutcomesForOwner(ctx, store.ListWorkerBlockedOutcomesForOwnerParams{UserID: userID, WorkerID: workerID})
	if err != nil {
		return nil, err
	}
	out := make([]apitypes.OwnerBlockedOutcome, 0, len(rows))
	for _, r := range rows {
		o := apitypes.OwnerBlockedOutcome{
			RunID: r.RunID.String(), ClaimGeneration: r.ClaimGeneration, JournalSHA256: r.JournalSha256,
			BlockedReason: r.BlockedReason, LossComplete: r.LossComplete, IncompleteReason: r.IncompleteReason,
			ReportedAt: r.ReportedAt.Time.UTC(),
		}
		if r.LossComplete {
			var m apitypes.OutcomeLossManifest
			if err := json.Unmarshal(r.Loss, &m); err == nil {
				if _, sum, serr := outcomeloss.Sum(&m); serr == nil {
					o.Loss, o.LossSHA256 = &m, sum
				}
			}
			if o.Loss == nil { // a stored manifest that no longer validates is shown as incomplete
				o.LossComplete, o.IncompleteReason = false, "stored_manifest_invalid"
			}
		}
		out = append(out, o)
	}
	return out, nil
}

// ValidateOutcomeDiscardRequest checks the owner request grammar (no database access).
func ValidateOutcomeDiscardRequest(req apitypes.OutcomeDiscardRequest) error {
	id, err := uuid.Parse(req.RunID)
	if err != nil || id.String() != req.RunID || req.ClaimGeneration < 0 || req.ClaimGeneration > outcomeloss.MaxSafeInt ||
		!outcomeloss.ValidHex64(req.JournalSHA256) || !outcomeloss.ValidHex64(req.LossSHA256) {
		return ErrInvalidOutcomeDiscardRequest
	}
	return nil
}

// AuthorizeOutcomeDiscard records the owner's confirmation of one exact journal. The caller must
// own BOTH the run and the worker (an admin who owns neither is refused: ErrRunNotFound /
// ErrWorkerNotFound). evidence must be "informed_decision"; the handler enforces confirm=discard.
// Refusals are *OutcomeDiscardRefusal in the spec's order.
func (s *Service) AuthorizeOutcomeDiscard(ctx context.Context, userID, workerID uuid.UUID, req apitypes.OutcomeDiscardRequest) (apitypes.OutcomeDiscard, error) {
	if err := ValidateOutcomeDiscardRequest(req); err != nil {
		return apitypes.OutcomeDiscard{}, err
	}
	if req.Evidence != apitypes.OutcomeDiscardEvidenceInformed {
		return apitypes.OutcomeDiscard{}, ErrInvalidOutcomeDiscardRequest
	}
	runID := uuid.MustParse(req.RunID)
	// Cheap owner pre-checks before any lock is taken on a worker the caller may not own.
	if _, err := s.GetRun(ctx, userID, runID); err != nil {
		return apitypes.OutcomeDiscard{}, err
	}
	if err := s.ownerWorker(ctx, s.q, userID, workerID); err != nil {
		return apitypes.OutcomeDiscard{}, err
	}
	now := s.now()
	var out apitypes.OutcomeDiscard
	err := s.outcomeDiscardTx(ctx, workerID, func(q *store.Queries) error {
		// Re-read under L0/L1: the pre-checks above are not a lock.
		run, err := q.GetRunByIDForUser(ctx, store.GetRunByIDForUserParams{ID: runID, UserID: userID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrRunNotFound
			}
			return err
		}
		if err := s.ownerWorker(ctx, q, userID, workerID); err != nil {
			return err
		}
		report, err := q.GetWorkerBlockedOutcomeForUpdate(ctx, store.GetWorkerBlockedOutcomeForUpdateParams{
			WorkerID: workerID, RunID: runID, ClaimGeneration: req.ClaimGeneration,
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return discardRefusal(DiscardRefusalNotBlocked)
		case err != nil:
			return err
		}
		switch {
		case report.BlockedReason != outcomeloss.ReasonRunNotOwned:
			return discardRefusal(DiscardRefusalReasonNotDiscard)
		case run.WorkerID.Valid && uuid.UUID(run.WorkerID.Bytes) == workerID:
			return discardRefusal(DiscardRefusalAttributed)
		case report.JournalSha256 != req.JournalSHA256:
			return discardRefusal(DiscardRefusalJournalChanged)
		case !report.LossComplete:
			return discardRefusal(DiscardRefusalManifestIncomplete)
		case report.LossSha256 != req.LossSHA256:
			return discardRefusal(DiscardRefusalLossChanged)
		}
		held, err := q.OpenCustodyHoldForWorkerRun(ctx, store.OpenCustodyHoldForWorkerRunParams{RunID: runID, WorkerID: pgconv.UUID(workerID)})
		if err != nil {
			return err
		}
		if held {
			return discardRefusal(DiscardRefusalCustodyHoldOpen)
		}
		grace := int32(OutcomeDiscardGrace.Seconds())
		if err := q.ExpireOutcomeDiscardsForKey(ctx, store.ExpireOutcomeDiscardsForKeyParams{
			Now: pgconv.Time(now), WorkerID: workerID, RunID: runID, ClaimGeneration: req.ClaimGeneration, GraceSeconds: grace,
		}); err != nil {
			return err
		}
		if err := q.SupersedeAuthorizedOutcomeDiscards(ctx, store.SupersedeAuthorizedOutcomeDiscardsParams{
			Now: pgconv.Time(now), WorkerID: workerID, RunID: runID, ClaimGeneration: req.ClaimGeneration,
			JournalSha256: req.JournalSHA256, LossSha256: req.LossSHA256,
		}); err != nil {
			return err
		}
		live, err := q.GetActiveOutcomeDiscardForKey(ctx, store.GetActiveOutcomeDiscardForKeyParams{
			WorkerID: workerID, RunID: runID, ClaimGeneration: req.ClaimGeneration,
		})
		switch {
		case err == nil && live.State == apitypes.OutcomeDiscardExecuting:
			return discardRefusal(DiscardRefusalExecutionUnresolved)
		case err == nil: // the same tuple is already authorized: confirming again is idempotent
			out = outcomeDiscardView(live, now)
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		d, err := q.InsertOutcomeDiscard(ctx, store.InsertOutcomeDiscardParams{
			WorkerID: workerID, RunID: runID, ClaimGeneration: req.ClaimGeneration, JournalSha256: req.JournalSHA256,
			LossSha256: req.LossSHA256, ConfirmedBy: userID, Evidence: req.Evidence,
			ExpiresAt: pgconv.Time(now.Add(OutcomeDiscardAuthorizationTTL)), Now: pgconv.Time(now),
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) { // lost a race the locks should have excluded: fail closed
				return discardRefusal(DiscardRefusalExecutionUnresolved)
			}
			return err
		}
		out = outcomeDiscardView(d, now)
		return nil
	})
	return out, err
}

// ownedDirective proves userID owns the directive's worker AND run (plain reads), returning the
// directive id's row. Anything else is ErrOutcomeDiscardNotFound.
func (s *Service) ownedDirective(ctx context.Context, userID, workerID, id uuid.UUID) error {
	if err := s.ownerWorker(ctx, s.q, userID, workerID); err != nil {
		return ErrOutcomeDiscardNotFound
	}
	if s.txBeginner == nil {
		return errOutcomeDiscardUnavailable
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	d, err := q.GetOutcomeDiscard(ctx, id)
	if err != nil || d.WorkerID != workerID {
		if err == nil || errors.Is(err, pgx.ErrNoRows) {
			return ErrOutcomeDiscardNotFound
		}
		return err
	}
	if _, err := q.GetRunByIDForUser(ctx, store.GetRunByIDForUserParams{ID: d.RunID, UserID: userID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrOutcomeDiscardNotFound
		}
		return err
	}
	return nil
}

// GetOutcomeDiscardForOwner returns a directive's status to the principal who owns both its
// worker and its run, after lazy expiry.
func (s *Service) GetOutcomeDiscardForOwner(ctx context.Context, userID, workerID, id uuid.UUID) (apitypes.OutcomeDiscard, error) {
	if err := s.ownedDirective(ctx, userID, workerID, id); err != nil {
		return apitypes.OutcomeDiscard{}, err
	}
	now := s.now()
	var out apitypes.OutcomeDiscard
	err := s.outcomeDiscardTx(ctx, workerID, func(q *store.Queries) error {
		d, _, err := lockedDirectiveForWorker(ctx, q, workerID, id, false)
		if err != nil {
			return err
		}
		if d, err = expireIfDue(ctx, q, d, now); err != nil {
			return err
		}
		out = outcomeDiscardView(d, now)
		return nil
	})
	return out, err
}

// CancelOutcomeDiscard cancels an authorized directive. An executing directive cannot be
// cancelled (ErrOutcomeDiscardConflict): the worker may already be deleting.
func (s *Service) CancelOutcomeDiscard(ctx context.Context, userID, workerID, id uuid.UUID) (apitypes.OutcomeDiscard, error) {
	if err := s.ownedDirective(ctx, userID, workerID, id); err != nil {
		return apitypes.OutcomeDiscard{}, err
	}
	now := s.now()
	var out apitypes.OutcomeDiscard
	err := s.outcomeDiscardTx(ctx, workerID, func(q *store.Queries) error {
		d, _, err := lockedDirectiveForWorker(ctx, q, workerID, id, false)
		if err != nil {
			return err
		}
		if d, err = expireIfDue(ctx, q, d, now); err != nil {
			return err
		}
		switch d.State {
		case apitypes.OutcomeDiscardCancelled:
		case apitypes.OutcomeDiscardAuthorized:
			if d, err = finishDiscard(ctx, q, d, apitypes.OutcomeDiscardCancelled, "owner_cancelled", now, apitypes.OutcomeDiscardAuthorized); err != nil {
				return err
			}
		default:
			return ErrOutcomeDiscardConflict
		}
		out = outcomeDiscardView(d, now)
		return nil
	})
	return out, err
}

// sweepOutcomeDiscards is Sweep's retention hook: it expires directives past their deadline and
// deletes final directives and unreported outcome rows older than OutcomeDiscardRetention. Each
// statement is its own short transaction with FOR UPDATE SKIP LOCKED batches of at most 500 rows
// and at most outcomeDiscardSweepBatches batches per tick; it never updates worker_id. An error
// is logged and ends only that statement's loop, so one failure does not block the others or the
// rest of the sweep.
func (s *Service) sweepOutcomeDiscards(ctx context.Context, now time.Time) {
	cutoff := pgconv.Time(now.Add(-OutcomeDiscardRetention))
	steps := []struct {
		name string
		run  func() (int64, error)
	}{
		{"expire", func() (int64, error) {
			return s.q.SweepExpireOutcomeDiscards(ctx, store.SweepExpireOutcomeDiscardsParams{
				Now: pgconv.Time(now), GraceSeconds: int32(OutcomeDiscardGrace.Seconds()),
			})
		}},
		{"delete final directives", func() (int64, error) { return s.q.SweepDeleteFinalOutcomeDiscards(ctx, cutoff) }},
		{"delete stale outcomes", func() (int64, error) { return s.q.SweepDeleteStaleBlockedOutcomes(ctx, cutoff) }},
	}
	for _, step := range steps {
		for i := 0; i < outcomeDiscardSweepBatches && ctx.Err() == nil; i++ {
			n, err := step.run()
			if err != nil {
				slog.Warn("sweeper: outcome discard retention failed", "step", step.name, "error", err)
				break
			}
			if n < 500 {
				break
			}
		}
	}
}
