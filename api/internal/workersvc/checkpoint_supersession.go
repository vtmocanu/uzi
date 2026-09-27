package workersvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/secretscrub"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1810 M3 (D2): SUPERSESSION. A new issue run on the same branch needs the checkpoint slot
// a retained ref of an older run still occupies (its first publish is refused not_descendant).
// The old tip is MOVED, never deleted: under the OLD run's retention lock the api
//
//  1. persists the intent (retained -> superseding, recovery_ref = refs/uzi-recovery/<run id>)
//     before any forge write;
//  2. creates the recovery ref at the recorded tip (CAS Old = zero, pack-less);
//  3. CAS-deletes the branch ref at that tip;
//  4. marks the record superseded (or, with no open custody hold, settling) in one statement,
//     and a settling record's recovery ref is CAS-deleted under the same lock.
//
// The whole operation, forge calls included, runs under the one lock, and every forge write is
// preceded by beforeRetentionWrite (the test hook, then the fence). A crash at any point leaves
// the tip under at least one recorded ref; ReconcileCheckpointRetentions re-drives a
// `superseding` record from step 2, and every step is idempotent.

// reconcileRetentionBatch bounds one ReconcileCheckpointRetentions pass: each record may cost a
// few forge round-trips, and the pass runs on the sweeper's tick.
const reconcileRetentionBatch = 10

// supersessionWired reports whether supersession can run: every retention seam plus the ref
// create and list seams.
func (s *Service) supersessionWired() bool {
	return s.retentionWired() && s.createRefFn != nil && s.listRefTipsFn != nil
}

// freeCheckpointSlot is Publish's not_descendant follow-up (PRD #1810 D2): for an issue run it
// looks for OTHER runs' records still holding the branch's checkpoint ref, newest first, and
// supersedes them one at a time until one frees the ref. It reports true only when a record
// freed the branch ref, so the caller retries its publish once. A busy lock (another instance
// or the sweeper is working the record) or a full retention slot stops the search: the caller
// returns today's not_descendant skip and the worker retries on its next tick.
func (s *Service) freeCheckpointSlot(ctx context.Context, run store.Run, branch string) bool {
	if run.Kind != runkind.Issue || !run.RepoID.Valid || !s.supersessionWired() {
		return false
	}
	ref := checkpointRefPrefix + branch
	rows, err := s.q.ListCheckpointRetentionsForBranch(ctx, store.ListCheckpointRetentionsForBranchParams{
		RepoID: uuid.UUID(run.RepoID.Bytes), Branch: branch, ExcludeRunID: run.ID, Ref: ref,
	})
	if err != nil {
		slog.Warn("checkpoint retention: list branch records", "run", run.ID, "branch", branch, "error", err)
		return false
	}
	for _, r := range rows {
		freed, acquired, err := s.supersedeRetainedCheckpoint(ctx, r.RunID)
		if err != nil {
			slog.Warn("checkpoint retention: supersede", "run", r.RunID, "for_run", run.ID, "error", secretscrub.Scrub(err.Error()))
		}
		if !acquired {
			return false
		}
		if freed {
			return true
		}
	}
	return false
}

// supersedeRetainedCheckpoint runs one supersession attempt for runID's record under the run's
// retention lock (try semantics; withRetentionLock detaches from ctx and applies its own
// deadline, so a caller's short request timeout cannot cancel it between steps). freed reports
// that the record no longer holds the branch checkpoint ref; acquired is false when the lock or
// a concurrency slot was busy (nothing ran). A panic from the go-git seams is recovered into err:
// the sweeper and the publish handler must survive a hostile forge response.
func (s *Service) supersedeRetainedCheckpoint(ctx context.Context, runID uuid.UUID) (freed, acquired bool, err error) {
	if !s.supersessionWired() {
		return false, false, nil
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("supersession panicked: %s", secretscrub.Scrub(fmt.Sprint(r)))
		}
	}()
	acquired, err = s.withRetentionLock(ctx, runID, func(ctx context.Context, fence func(context.Context) error) error {
		var ferr error
		freed, ferr = s.supersedeLocked(ctx, runID, fence)
		return ferr
	})
	return freed && acquired, acquired, err
}

// supersedeLocked re-reads the record under the lock and acts only on the state it finds.
func (s *Service) supersedeLocked(ctx context.Context, runID uuid.UUID, fence func(context.Context) error) (bool, error) {
	row, err := s.q.GetCheckpointRetention(ctx, runID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("read record: %w", err)
	}
	branchRef := checkpointRefPrefix + row.Branch
	switch row.State {
	case retentionRetained:
		if row.Ref != branchRef {
			return false, nil
		}
		n, err := s.q.BeginCheckpointSupersession(ctx, store.BeginCheckpointSupersessionParams{
			RunID: runID, RecoveryRef: pushbroker.RecoveryRefPrefix + runID.String(), Tip: row.Tip,
		})
		if err != nil {
			return false, fmt.Errorf("record supersession intent: %w", err)
		}
		if n == 0 {
			// The tip advanced (a late publish) or the record moved on: the next trigger re-reads.
			return false, nil
		}
		if row, err = s.q.GetCheckpointRetention(ctx, runID); err != nil {
			return false, fmt.Errorf("re-read record: %w", err)
		}
		if row.State != retentionSuperseding {
			return false, nil
		}
	case retentionSuperseding:
	case retentionSettling:
		if row.Ref != branchRef {
			return true, nil // settling a recovery ref: the branch slot is already free
		}
		// No hold is open: settle it the ordinary way, deleting the branch ref CAS on its tip.
		// No recovery ref is created for a record nobody needs any more.
		return s.deleteSettlingRef(ctx, row, fence)
	default:
		return row.Ref != branchRef, nil
	}
	return s.driveSupersession(ctx, row, fence)
}

// driveSupersession runs steps 2-4 for a `superseding` record. Every step is idempotent, so a
// re-drive after a crash repeats the completed ones harmlessly.
func (s *Service) driveSupersession(ctx context.Context, row store.CheckpointRetention, fence func(context.Context) error) (bool, error) {
	runID := row.RunID
	branchRef := checkpointRefPrefix + row.Branch
	recoveryRef := pushbroker.RecoveryRefPrefix + runID.String()
	if !row.RecoveryRef.Valid || row.RecoveryRef.String != recoveryRef {
		return false, fmt.Errorf("superseding record names recovery ref %q, want %q", row.RecoveryRef.String, recoveryRef)
	}
	f, problem, gone := s.forgeForRetention(ctx, runID)
	if gone {
		problem = "run, repository or forge connection no longer exists"
	}
	if problem != "" {
		return false, s.recordRetentionFailure(ctx, row, retentionSuperseding, problem)
	}

	// Step 2: the recovery ref at the recorded tip.
	if err := s.beforeRetentionWrite(ctx, runID, "create", fence); err != nil {
		return false, err
	}
	cerr := s.createRefFn(ctx, pushbroker.CreateRefOptions{
		CloneURL: f.cloneURL, Username: f.username, PAT: f.pat, Ref: recoveryRef, Tip: row.Tip, SourceRef: branchRef,
	})
	switch {
	case cerr == nil, errors.Is(cerr, pushbroker.ErrRefExistsAtTip):
	case errors.Is(cerr, pushbroker.ErrRefExists):
		return false, s.recordRetentionFailure(ctx, row, retentionSuperseding,
			"recovery ref exists at another tip; branch ref left in place")
	case errors.Is(cerr, pushbroker.ErrSourceMissing):
		proceed, freed, err := s.resolveMissingSource(ctx, row, f, branchRef, recoveryRef)
		if !proceed {
			return freed, err
		}
	default:
		return false, s.recordRetentionFailure(ctx, row, retentionSuperseding,
			"create recovery ref: "+scrubForgeError(cerr.Error(), f.pat))
	}

	// Step 3: free the branch ref, CAS on the tip. Absent or advanced by another run is done.
	if err := s.beforeRetentionWrite(ctx, runID, "delete-branch", fence); err != nil {
		return false, err
	}
	if derr := s.deleteCheckpointFn(ctx, pushbroker.DeleteOptions{
		CloneURL: f.cloneURL, Branch: row.Branch, Ref: branchRef, Username: f.username, PAT: f.pat, ExpectedOldTip: row.Tip,
	}); derr != nil {
		return false, s.recordRetentionFailure(ctx, row, retentionSuperseding,
			"delete branch ref: "+scrubForgeError(derr.Error(), f.pat))
	}

	// Step 4: superseded (a hold is open) or straight to settling (none is), one statement.
	state, err := s.q.MarkCheckpointSuperseded(ctx, runID)
	if err != nil {
		// The branch ref is free either way; a record left superseding is re-driven.
		return true, fmt.Errorf("mark superseded: %w", err)
	}
	slog.Info("checkpoint retention: superseded", "run", runID, "branch", row.Branch, "recovery_ref", recoveryRef, "state", state)
	if state != retentionSettling {
		return true, nil
	}
	// No hold is open: the recovery ref is owed its CAS delete now, under the same lock.
	settling, err := s.q.GetCheckpointRetention(ctx, runID)
	if err != nil {
		return true, fmt.Errorf("re-read record: %w", err)
	}
	if settling.State != retentionSettling {
		return true, nil
	}
	if _, err := s.deleteSettlingRef(ctx, settling, fence); err != nil {
		return true, err
	}
	return true, nil
}

// resolveMissingSource disambiguates CreateRef's ErrSourceMissing (origin does not advertise the
// branch ref at the recorded tip) by listing both refs once:
//
//   - the recovery ref at the tip: an earlier attempt created it and crashed; proceed to step 3;
//   - the recovery ref at another tip: stop, branch ref untouched;
//   - the branch ref at a different tip (checkpoint_tip lagged a later publish of the same run,
//     or another writer moved it): stop, the record kept for a later attempt;
//   - neither ref: nothing uzi owns holds the tip any more; the record closes as deleted.
//
// freed is meaningful only when proceed is false.
func (s *Service) resolveMissingSource(ctx context.Context, row store.CheckpointRetention, f retentionForge, branchRef, recoveryRef string) (proceed, freed bool, err error) {
	tips, lerr := s.listRefTipsFn(ctx, pushbroker.ListRefsOptions{CloneURL: f.cloneURL, Username: f.username, PAT: f.pat}, branchRef, recoveryRef)
	if lerr != nil {
		return false, false, s.recordRetentionFailure(ctx, row, retentionSuperseding, "list refs: "+scrubForgeError(lerr.Error(), f.pat))
	}
	rec, recOK := tips[recoveryRef]
	_, brOK := tips[branchRef]
	switch {
	case recOK && rec == row.Tip:
		return true, false, nil
	case recOK:
		return false, false, s.recordRetentionFailure(ctx, row, retentionSuperseding,
			"recovery ref exists at another tip; branch ref left in place")
	case brOK:
		return false, false, s.recordRetentionFailure(ctx, row, retentionSuperseding,
			"branch ref is not at the recorded tip (checkpoint tip lag or another writer); left in place")
	}
	const note = "origin holds the recorded tip under neither the branch ref nor the recovery ref"
	if _, err := s.q.SetCheckpointSupersessionTipGone(ctx, store.SetCheckpointSupersessionTipGoneParams{RunID: row.RunID, LastError: note}); err != nil {
		return false, true, fmt.Errorf("mark tip gone: %w", err)
	}
	slog.Warn("checkpoint retention: "+note+"; record closed", "run", row.RunID, "branch", row.Branch, "tip", row.Tip)
	return false, true, nil
}

// ReconcileCheckpointRetentions is the sweeper's checkpoint-retention pass (PRD #1810). It
// re-drives records whose work was interrupted, each under its run's retention lock (try
// semantics: a record whose lock is busy is skipped this tick). Best-effort like
// ReconcileCustodyReleases: a candidate-list read error fails the pass, a per-record error is
// logged and skipped. Returns the number of records driven to a final step this pass.
//
// M3 arm: `superseding` records (a crash or failure between the persisted intent and the
// superseded mark) are re-driven through supersession. M4 adds its own arms (settling retries,
// unheld retained/superseded records moved to settling, the backfill and the audit) as separate
// cases here, each naming the states it lists.
func (s *Service) ReconcileCheckpointRetentions(ctx context.Context) (int64, error) {
	if !s.supersessionWired() {
		return 0, nil
	}
	rows, err := s.q.ListCheckpointRetentionWork(ctx, store.ListCheckpointRetentionWorkParams{
		States: []string{retentionSuperseding}, MaxRows: reconcileRetentionBatch,
	})
	if err != nil {
		return 0, fmt.Errorf("list checkpoint retention work: %w", err)
	}
	var progressed int64
	for _, r := range rows {
		switch r.State {
		case retentionSuperseding:
			freed, acquired, err := s.supersedeRetainedCheckpoint(ctx, r.RunID)
			if err != nil {
				slog.Warn("sweeper: checkpoint supersession re-drive", "run", r.RunID, "error", secretscrub.Scrub(err.Error()))
				continue
			}
			if acquired && freed {
				progressed++
			}
		}
	}
	return progressed, nil
}
