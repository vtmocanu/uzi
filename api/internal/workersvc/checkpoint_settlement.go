package workersvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/secretscrub"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1810 M4 (D3): SETTLEMENT RECONCILIATION. Every custody release/discard writer triggers
// SettleRetainedCheckpoint after its transaction commits; this sweeper pass is the backstop for
// every trigger that was lost, skipped (a busy lock, a full retention slot) or never existed (a
// terminal writer that does not call the retention path at all). Each arm lists its own bounded
// page, and every record is worked under its run's retention lock with try semantics: a busy
// record is skipped this tick, a per-record error is logged and skipped, and only a candidate-list
// read error fails the pass.

// reconcileRetentionBatch bounds each arm's page in one ReconcileCheckpointRetentions pass: a
// record may cost a few forge round-trips, and the pass runs on the sweeper's tick.
const reconcileRetentionBatch = 10

// Notes persisted on a record's last_error by the M4 arms (never carry forge output).
const (
	retentionGoneNote     = "run, repository or forge connection no longer exists"
	stuckExitNote         = "supersession stopped and no custody hold is open; the run's refs at the recorded tip were deleted"
	auditOtherTipNote     = "recovery ref found at another tip during the post-settlement audit; left in place"
	auditUnverifiableNote = "post-settlement audit could not run: " + retentionGoneNote
)

// ReconcileCheckpointRetentions is the sweeper's checkpoint-retention pass (PRD #1810 M3/M4).
// Returns the number of records it drove to a final step (a ref settled or verified, a record
// backfilled, a supersession finished or exited). Inert unless every retention seam is wired.
func (s *Service) ReconcileCheckpointRetentions(ctx context.Context) (int64, error) {
	return s.reconcileCheckpointRetentions(ctx, pgtype.UUID{})
}

// reconcileCheckpointRetentions is the pass body. onlyRun confines every arm's candidate list to
// one run (the LiveDB tests, so a reused database's leftover records are never driven against a
// test's forge); the zero value lists every run, as production does. The arms run in order:
//
//  1. backfill: terminal runs that own a checkpoint ref but have no record get one (retained
//     with an open hold, else settling), so arm 2 settles a new settling record the same pass;
//  2. work: a due `settling` record retries its CAS delete; a due `superseding` record is
//     re-driven, or, when its supersession stopped (last_error set) and no hold is open, exited;
//  3. unheld: a `retained`/`superseded` record whose run has no open hold moves to settling and
//     its ref is CAS-deleted;
//  4. audit: a deleted record that named a recovery ref is re-verified once verify_after passed.
func (s *Service) reconcileCheckpointRetentions(ctx context.Context, onlyRun pgtype.UUID) (int64, error) {
	if !s.supersessionWired() {
		return 0, nil
	}
	var progressed int64

	backfill, err := s.q.ListCheckpointRetentionBackfill(ctx, store.ListCheckpointRetentionBackfillParams{
		OnlyRunID: onlyRun, MaxRows: reconcileRetentionBatch,
	})
	if err != nil {
		return 0, fmt.Errorf("list checkpoint retention backfill: %w", err)
	}
	for _, r := range backfill {
		if inserted, _ := s.recordCheckpointRetention(ctx, r.ID, r.Kind, r.IssueIid); inserted {
			slog.Info("sweeper: checkpoint retention backfilled", "run", r.ID)
			progressed++
		}
	}

	work, err := s.q.ListCheckpointRetentionWork(ctx, store.ListCheckpointRetentionWorkParams{
		States: []string{retentionSuperseding, retentionSettling}, OnlyRunID: onlyRun, MaxRows: reconcileRetentionBatch,
	})
	if err != nil {
		return progressed, fmt.Errorf("list checkpoint retention work: %w", err)
	}
	for _, r := range work {
		var (
			done bool
			err  error
		)
		switch r.State {
		case retentionSuperseding:
			done, _, err = s.reconcileSuperseding(ctx, r.RunID)
		case retentionSettling:
			done, _, err = s.settleRetainedCheckpointOnce(ctx, r.RunID)
		}
		if err != nil {
			slog.Warn("sweeper: checkpoint retention", "run", r.RunID, "state", r.State, "error", secretscrub.Scrub(err.Error()))
			continue
		}
		if done {
			progressed++
		}
	}

	unheld, err := s.q.ListUnheldCheckpointRetentions(ctx, store.ListUnheldCheckpointRetentionsParams{
		OnlyRunID: onlyRun, MaxRows: reconcileRetentionBatch,
	})
	if err != nil {
		return progressed, fmt.Errorf("list unheld checkpoint retentions: %w", err)
	}
	for _, r := range unheld {
		done, _, err := s.settleRetainedCheckpointOnce(ctx, r.RunID)
		if err != nil {
			slog.Warn("sweeper: checkpoint retention settle", "run", r.RunID, "error", secretscrub.Scrub(err.Error()))
			continue
		}
		if done {
			progressed++
		}
	}

	audit, err := s.q.ListCheckpointRetentionAudit(ctx, store.ListCheckpointRetentionAuditParams{
		OnlyRunID: onlyRun, MaxRows: reconcileRetentionBatch,
	})
	if err != nil {
		return progressed, fmt.Errorf("list checkpoint retention audit: %w", err)
	}
	for _, r := range audit {
		done, _, err := s.lockedRetentionStep(ctx, r.RunID, s.auditRecoveryRefLocked)
		if err != nil {
			slog.Warn("sweeper: checkpoint retention audit", "run", r.RunID, "error", secretscrub.Scrub(err.Error()))
			continue
		}
		if done {
			progressed++
		}
	}
	return progressed, nil
}

// lockedRetentionStep runs one locked step for runID under its retention lock (try semantics),
// recovering a panic from the go-git seams into err. done is meaningful only when acquired.
func (s *Service) lockedRetentionStep(ctx context.Context, runID uuid.UUID,
	step func(ctx context.Context, runID uuid.UUID, fence func(context.Context) error) (bool, error),
) (done, acquired bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("checkpoint retention step panicked: %s", secretscrub.Scrub(fmt.Sprint(r)))
		}
	}()
	acquired, err = s.withRetentionLock(ctx, runID, func(ctx context.Context, fence func(context.Context) error) error {
		var ferr error
		done, ferr = step(ctx, runID, fence)
		return ferr
	})
	return done && acquired, acquired, err
}

// reconcileSuperseding is the sweeper's arm for a due `superseding` record. Under the lock it
// re-reads the record; one whose supersession STOPPED (last_error set: the tip lagged a later
// publish, the recovery ref sits at another tip, or a forge step keeps failing) and whose run has
// NO open custody hold is exited (exitStuckSupersessionLocked), since nothing needs its tip any
// more. Every other superseding record is re-driven from step 2 (driveSupersession).
func (s *Service) reconcileSuperseding(ctx context.Context, runID uuid.UUID) (done, acquired bool, err error) {
	return s.lockedRetentionStep(ctx, runID, func(ctx context.Context, runID uuid.UUID, fence func(context.Context) error) (bool, error) {
		row, err := s.q.GetCheckpointRetention(ctx, runID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			return false, fmt.Errorf("read record: %w", err)
		}
		if row.State != retentionSuperseding {
			return false, nil
		}
		if row.LastError.Valid {
			held, err := s.q.RunHasOpenCustodyHold(ctx, runID)
			if err != nil {
				return false, fmt.Errorf("read custody: %w", err)
			}
			if !held {
				return s.exitStuckSupersessionLocked(ctx, row, fence)
			}
		}
		return s.driveSupersession(ctx, row, fence)
	})
}

// exitStuckSupersessionLocked settles a stopped `superseding` record whose run has no open hold
// (PRD #1810 M4): under the run's lock it lists both refs once, then, each behind the fence,
// CAS-deletes the recovery ref if origin holds it at the recorded tip (only then is it ours) and
// the branch ref if origin holds it at the recorded tip, and closes the record as deleted (with a
// verify_after, since it names a recovery ref). A ref at any other tip is left alone.
//
// Residual, by design: in the tip-lag case the branch ref sits at a LATER tip of the same run
// that no record binds to; it stays on origin until a human or a later run on the branch clears
// it. The Warn names the branch so an operator can.
func (s *Service) exitStuckSupersessionLocked(ctx context.Context, row store.CheckpointRetention, fence func(context.Context) error) (bool, error) {
	runID := row.RunID
	branchRef := checkpointRefPrefix + row.Branch
	recoveryRef := pushbroker.RecoveryRefPrefix + runID.String()
	if !row.RecoveryRef.Valid || row.RecoveryRef.String != recoveryRef {
		return false, fmt.Errorf("superseding record names recovery ref %q, want %q", row.RecoveryRef.String, recoveryRef)
	}
	f, problem, gone := s.forgeForRetention(ctx, runID)
	if gone {
		if _, err := s.q.SetCheckpointRetentionAbandoned(ctx, store.SetCheckpointRetentionAbandonedParams{
			RunID: runID, LastError: retentionGoneNote, ExpectedState: retentionSuperseding,
		}); err != nil {
			return false, fmt.Errorf("mark abandoned: %w", err)
		}
		return false, nil
	}
	if problem != "" {
		return false, s.recordRetentionFailure(ctx, row, retentionSuperseding, problem)
	}
	tips, lerr := s.listRefTipsFn(ctx, pushbroker.ListRefsOptions{CloneURL: f.cloneURL, Username: f.username, PAT: f.pat}, branchRef, recoveryRef)
	if lerr != nil {
		return false, s.recordRetentionFailure(ctx, row, retentionSuperseding, "list refs: "+scrubForgeError(lerr.Error(), f.pat))
	}

	for _, step := range []struct{ op, ref string }{{"exit-recovery", recoveryRef}, {"exit-branch", branchRef}} {
		tip, ok := tips[step.ref]
		if !ok {
			continue
		}
		if tip != row.Tip {
			slog.Warn("checkpoint retention: stuck supersession exit leaves a ref at another tip", "run", runID,
				"branch", row.Branch, "ref", step.ref, "recorded_tip", row.Tip, "origin_tip", tip)
			continue
		}
		if err := s.beforeRetentionWrite(ctx, runID, step.op, fence); err != nil {
			return false, err
		}
		if derr := s.deleteCheckpointFn(ctx, pushbroker.DeleteOptions{
			CloneURL: f.cloneURL, Branch: row.Branch, Ref: step.ref, Username: f.username, PAT: f.pat, ExpectedOldTip: row.Tip,
		}); derr != nil {
			return false, s.recordRetentionFailure(ctx, row, retentionSuperseding,
				"delete "+step.ref+": "+scrubForgeError(derr.Error(), f.pat))
		}
	}
	n, err := s.q.SetCheckpointSupersessionExited(ctx, store.SetCheckpointSupersessionExitedParams{RunID: runID, LastError: stuckExitNote})
	if err != nil {
		return false, fmt.Errorf("mark exited: %w", err)
	}
	if n == 0 {
		return false, nil // a hold appeared or the record moved on
	}
	slog.Info("checkpoint retention: stuck supersession exited", "run", runID, "branch", row.Branch)
	return true, nil
}

// auditRecoveryRefLocked is the post-settlement audit of one deleted record that named a
// recovery ref (PRD #1810 M4). It closes the residual window the fence cannot: an attempt that
// passed its fence, lost its session, and whose recovery-ref create landed on origin AFTER another
// instance settled the record. Under the run's lock it lists the recovery ref once:
//
//   - absent: verified;
//   - at the recorded tip: the stray is ours; behind the fence, CAS-delete it, then verified;
//   - at another tip: not ours to delete; left in place, logged, verified with a note;
//   - a forge or connection failure: the verification is deferred with backoff (verified_at
//     stays NULL).
func (s *Service) auditRecoveryRefLocked(ctx context.Context, runID uuid.UUID, fence func(context.Context) error) (bool, error) {
	row, err := s.q.GetCheckpointRetention(ctx, runID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("read record: %w", err)
	}
	recoveryRef := pushbroker.RecoveryRefPrefix + runID.String()
	if row.State != "deleted" || row.VerifiedAt.Valid || !row.RecoveryRef.Valid || row.RecoveryRef.String != recoveryRef {
		return false, nil
	}
	f, problem, gone := s.forgeForRetention(ctx, runID)
	if gone {
		return s.markRetentionVerified(ctx, runID, auditUnverifiableNote)
	}
	if problem != "" {
		return false, s.deferRetentionVerify(ctx, row, problem)
	}
	tips, lerr := s.listRefTipsFn(ctx, pushbroker.ListRefsOptions{CloneURL: f.cloneURL, Username: f.username, PAT: f.pat}, recoveryRef)
	if lerr != nil {
		return false, s.deferRetentionVerify(ctx, row, "list refs: "+scrubForgeError(lerr.Error(), f.pat))
	}
	tip, ok := tips[recoveryRef]
	switch {
	case !ok:
		return s.markRetentionVerified(ctx, runID, "")
	case tip != row.Tip:
		slog.Warn("checkpoint retention: audit found the recovery ref at another tip; left in place", "run", runID,
			"ref", recoveryRef, "recorded_tip", row.Tip, "origin_tip", tip)
		return s.markRetentionVerified(ctx, runID, auditOtherTipNote)
	}
	if err := s.beforeRetentionWrite(ctx, runID, "audit-delete", fence); err != nil {
		return false, err
	}
	if derr := s.deleteCheckpointFn(ctx, pushbroker.DeleteOptions{
		CloneURL: f.cloneURL, Branch: row.Branch, Ref: recoveryRef, Username: f.username, PAT: f.pat, ExpectedOldTip: row.Tip,
	}); derr != nil {
		return false, s.deferRetentionVerify(ctx, row, "delete stray recovery ref: "+scrubForgeError(derr.Error(), f.pat))
	}
	slog.Warn("checkpoint retention: audit deleted a stray recovery ref created after settlement", "run", runID, "ref", recoveryRef)
	return s.markRetentionVerified(ctx, runID, "")
}

// markRetentionVerified stamps a deleted record's audit as done; a non-empty note replaces
// last_error.
func (s *Service) markRetentionVerified(ctx context.Context, runID uuid.UUID, note string) (bool, error) {
	var n pgtype.Text
	if note != "" {
		n = pgtype.Text{String: note, Valid: true}
	}
	rows, err := s.q.SetCheckpointRetentionVerified(ctx, store.SetCheckpointRetentionVerifiedParams{RunID: runID, Note: n})
	if err != nil {
		return false, fmt.Errorf("mark verified: %w", err)
	}
	return rows > 0, nil
}

// deferRetentionVerify pushes a deleted record's audit out by the retry backoff, recording the
// already-scrubbed msg.
func (s *Service) deferRetentionVerify(ctx context.Context, row store.CheckpointRetention, msg string) error {
	next := time.Now().Add(retentionBackoff(row.Attempts))
	if _, err := s.q.DeferCheckpointRetentionVerify(ctx, store.DeferCheckpointRetentionVerifyParams{
		RunID: row.RunID, LastError: msg, VerifyAfter: pgtype.Timestamptz{Time: next, Valid: true},
	}); err != nil {
		return fmt.Errorf("defer verify (%s): %w", msg, err)
	}
	slog.Warn("checkpoint retention: audit deferred", "run", row.RunID, "attempt", row.Attempts+1, "reason", msg)
	return nil
}
