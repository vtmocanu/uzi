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
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1810 D2, residual 2: a checkpoint push whose outcome the api never learned.
//
// Publish routes on the run status it reads at its top. A publish routed LIVE pushes without the
// run's retention lock; if the run turns terminal meanwhile, a newer run may supersede the old
// run's retention record while that push is still on the wire. A push the forge already accepted
// is not undone by the api's client giving up (pushbroker's 60-second ceiling bounds the CLIENT,
// not the forge's receive-pack), so the branch ref can move to the old run's tip at any later
// moment the forge's compare-and-swap still matches: in particular onto an ABSENT branch ref (a
// create, when the push fetched the branch after a supersession deleted it). Such a ref is tracked
// by no retention record and would block every later publish on the branch.
//
// Three mechanisms, and only the last is the correctness argument:
//
//   - a pre-push budget (livePublishPrePushBudget): a live-routed publish sends no push once that
//     long has passed since it was routed, so the ordinary in-flight window is short;
//   - a cooling period (checkpointSupersessionCooling, in BeginCheckpointSupersession): no
//     supersession intent is recorded until the run has been terminal that long, so an ordinary
//     in-flight push lands and is tracked (its tip advances the record) before any supersession
//     binds the record's tip. Both are DELAYS that make the reconciliation below rare;
//   - a durable attempt record (checkpoint_publish_attempts) written BEFORE every push and removed
//     only once the push's outcome is accounted for, plus the sweeper's attempts arm, which
//     compares origin's branch ref with each outstanding attempt of a terminal run and, under the
//     run's retention lock, re-records the tip on the run's record or CAS-deletes the branch ref at
//     exactly that tip. A supersession's post-delete list and the stuck exit also treat an attempted
//     tip as the run's own.
const (
	// livePublishPrePushBudget: see Service.livePublishPrePushBudget. The claim/free supersessions
	// run detached (withRetentionLock) under retentionOpTimeout and can exceed it.
	livePublishPrePushBudget = 2 * time.Minute
	// checkpointSupersessionCooling: see Service.checkpointSupersessionCooling. It must exceed the
	// latest moment an ordinary live-routed push can still be in flight on the client side:
	// livePublishPrePushBudget + pushbroker.MaxPublishDuration, plus supersessionCoolingSlack for
	// clock skew between api replicas and the database and for the push's own tip persist and
	// track. The constant below fails to compile when the relation breaks.
	checkpointSupersessionCooling = 5 * time.Minute
	supersessionCoolingSlack      = time.Minute
	// publishAttemptHorizon is how long the sweeper keeps comparing an outstanding attempt whose
	// tip origin does not carry before it retires the row. It is the one elapsed-time assumption
	// the reconciliation makes: no forge applies a receive-pack request a week after it was sent.
	publishAttemptHorizon = 7 * 24 * time.Hour
	// reconcileAttemptBatch bounds the attempts arm's page per sweeper pass.
	reconcileAttemptBatch = 10
)

// Compile-time: checkpointSupersessionCooling > livePublishPrePushBudget +
// pushbroker.MaxPublishDuration + supersessionCoolingSlack (a negative constant does not convert
// to uint).
const _ = uint(checkpointSupersessionCooling - livePublishPrePushBudget - pushbroker.MaxPublishDuration - supersessionCoolingSlack - 1)

// errPushRefused is a push Publish refused to send (the live pre-push budget elapsed, or the
// attempt record could not be written). Publish answers it with the benign not_descendant skip:
// no forge call was made, and the worker retries on its next tick.
var errPushRefused = errors.New("checkpoint push refused before the forge call")

// checkpointPush is one Publish's push state: the options, how the publish was routed, and the
// attempt row of the push that succeeded.
type checkpointPush struct {
	s *Service
	// runID is the run Publish was asked about (owned.ID for a real ownership read).
	runID  uuid.UUID
	run    store.Run
	branch string
	ref    string
	opts   pushbroker.Options
	// live: routed on a LIVE status read, without the run's retention lock; every push is then
	// refused once livePublishPrePushBudget has passed since routedAt.
	live     bool
	routedAt time.Time
	// landed is the attempt row of the push that returned success (uuid.Nil when unrecorded).
	landed uuid.UUID
}

// pushOnce sends ONE push: the live budget check, the durable attempt record, then the forge call.
// A push the forge definitively refused removes its attempt row at once (nothing landed); an
// error of unknown outcome keeps it for the sweeper.
func (p *checkpointPush) pushOnce(ctx context.Context) error {
	s := p.s
	if p.live {
		if elapsed := time.Since(p.routedAt); elapsed > s.livePublishPrePushBudget {
			slog.Warn("checkpoint: live-routed publish exceeded its pre-push budget; push not sent",
				"run", p.runID, "branch", p.branch, "elapsed", elapsed, "budget", s.livePublishPrePushBudget)
			return errPushRefused
		}
	}
	id, err := s.recordPublishAttempt(ctx, p.runID, p.branch, p.ref, p.opts.DeclaredTip)
	if err != nil {
		slog.Warn("checkpoint: record publish attempt; push not sent", "run", p.runID, "branch", p.branch, "error", err)
		return errPushRefused
	}
	_, perr := s.publishFn(ctx, p.opts)
	switch {
	case perr == nil:
		p.landed = id
	case pushDefinitelyRefused(perr):
		s.clearPublishAttempt(ctx, id)
	}
	return perr
}

// pushDefinitelyRefused reports a broker outcome that proves nothing landed: a local refusal
// before the wire, or the forge's own rejection of the update.
func pushDefinitelyRefused(err error) bool {
	return errors.Is(err, pushbroker.ErrNotDescendant) || errors.Is(err, pushbroker.ErrWorkflowScopeRejected) ||
		errors.Is(err, pushbroker.ErrTipMissing) || errors.Is(err, pushbroker.ErrPackTooLarge) ||
		errors.Is(err, pushbroker.ErrPackInvalid)
}

// recordPublishAttempt writes the attempt row a push needs before it is sent. Inert (uuid.Nil, no
// error) when supersession is not wired: then no supersession, and no reconciliation, ever runs.
func (s *Service) recordPublishAttempt(ctx context.Context, runID uuid.UUID, branch, ref, tip string) (uuid.UUID, error) {
	if !s.supersessionWired() {
		return uuid.Nil, nil
	}
	return s.q.RecordCheckpointPublishAttempt(ctx, store.RecordCheckpointPublishAttemptParams{
		RunID: runID, Branch: branch, Ref: ref, Tip: tip,
	})
}

// clearPublishAttempt removes an accounted-for attempt row. Best-effort: a row left behind is only
// compared and retired by the sweeper.
func (s *Service) clearPublishAttempt(ctx context.Context, id uuid.UUID) {
	if id == uuid.Nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), retentionRecordTimeout)
	defer cancel()
	if _, err := s.q.DeleteCheckpointPublishAttempt(ctx, id); err != nil {
		slog.Warn("checkpoint: clear publish attempt", "attempt", id, "error", err)
	}
}

// ownPublishedTip reports whether a branch ref at tip is provably this run's own publish: the tip
// the run persisted (runs.checkpoint_tip) or one it attempted and has not accounted for.
func (s *Service) ownPublishedTip(ctx context.Context, runID uuid.UUID, tip string) (bool, error) {
	runTip, err := s.q.GetRunCheckpointTipForRetention(ctx, runID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("read run checkpoint tip: %w", err)
	}
	if runTip.Valid && runTip.String == tip {
		return true, nil
	}
	attempted, err := s.q.RunHasCheckpointPublishAttempt(ctx, store.RunHasCheckpointPublishAttemptParams{RunID: runID, Tip: tip})
	if err != nil {
		return false, fmt.Errorf("read publish attempts: %w", err)
	}
	return attempted, nil
}

// reconcilePublishAttempts is the sweeper's attempts arm (ReconcileCheckpointRetentions): each due
// outstanding attempt of a terminal run is compared with origin under the run's retention lock
// (reconcilePublishAttemptLocked). A settle the reconciliation owes runs after the lock is
// released.
func (s *Service) reconcilePublishAttempts(ctx context.Context, onlyRun pgtype.UUID) (int64, error) {
	due, err := s.q.ListDueCheckpointPublishAttempts(ctx, store.ListDueCheckpointPublishAttemptsParams{
		OnlyRunID: onlyRun, MaxRows: reconcileAttemptBatch,
	})
	if err != nil {
		return 0, fmt.Errorf("list checkpoint publish attempts: %w", err)
	}
	var progressed int64
	for _, a := range due {
		var settle bool
		done, _, err := s.lockedRetentionStep(ctx, a.RunID, func(ctx context.Context, _ uuid.UUID, fence func(context.Context) error) (bool, error) {
			var (
				d   bool
				err error
			)
			d, settle, err = s.reconcilePublishAttemptLocked(ctx, a.ID, fence)
			return d, err
		})
		if err != nil {
			slog.Warn("sweeper: checkpoint publish attempt", "run", a.RunID, "attempt", a.ID, "error", err)
			continue
		}
		if done {
			progressed++
		}
		if settle {
			if _, _, err := s.settleRetainedCheckpointOnce(ctx, a.RunID); err != nil {
				slog.Warn("sweeper: checkpoint retention settle after a reconciled publish", "run", a.RunID, "error", err)
			}
		}
	}
	return progressed, nil
}

// reconcilePublishAttemptLocked compares one outstanding attempt with origin, under its run's
// retention lock (the run is terminal, so every publish of it now takes the same lock):
//
//   - origin's branch ref is NOT at the attempted tip: nothing of this attempt landed yet. The row
//     is checked again after a backoff, and retired once publishAttemptHorizon has passed;
//   - it IS, and another run claims that tip: not provably ours; the row is dropped;
//   - it IS, and the run's record can track it (no record, or one that never named a recovery
//     ref): the tip is persisted on the run and re-recorded (TrackTerminalCheckpointPublish:
//     insert, advance or reopen); a settling result is owed a settle (settle true);
//   - it IS, and the record is superseding: left to the supersession, whose post-delete list and
//     stuck exit treat an attempted tip as the run's own;
//   - it IS, and the run's slot was handed on (the record names a recovery ref, or tracking it
//     was refused): behind the fence, the branch ref is CAS-deleted at exactly the attempted tip,
//     as the terminal publish of a superseded run would have been refused.
//
// done reports the row was resolved (re-recorded, deleted, dropped or retired).
func (s *Service) reconcilePublishAttemptLocked(ctx context.Context, id uuid.UUID, fence func(context.Context) error) (done, settle bool, err error) {
	a, err := s.q.GetCheckpointPublishAttempt(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("read publish attempt: %w", err)
	}
	f, problem, gone := s.forgeForRetention(ctx, a.RunID)
	if gone {
		slog.Warn("checkpoint: publish attempt dropped; its run, repository or forge connection no longer exists",
			"run", a.RunID, "ref", a.Ref, "tip", a.Tip)
		return s.dropPublishAttempt(ctx, a.ID)
	}
	if problem != "" {
		return false, false, s.deferPublishAttempt(ctx, a, problem)
	}
	tips, lerr := s.listRefTipsFn(ctx, pushbroker.ListRefsOptions{CloneURL: f.cloneURL, Username: f.username, PAT: f.pat}, a.Ref)
	if lerr != nil {
		return false, false, s.deferPublishAttempt(ctx, a, "list branch ref: "+scrubForgeError(lerr.Error(), f.pat))
	}
	if tip, ok := tips[a.Ref]; !ok || tip != a.Tip {
		if a.AttemptedAt.Valid && time.Since(a.AttemptedAt.Time) > publishAttemptHorizon {
			return s.dropPublishAttempt(ctx, a.ID)
		}
		return false, false, s.deferPublishAttempt(ctx, a, "")
	}

	run, err := s.q.GetRunByID(ctx, a.RunID)
	if err != nil {
		return false, false, fmt.Errorf("read run: %w", err)
	}
	claimed, err := s.q.CheckpointTipClaimedByOtherRun(ctx, store.CheckpointTipClaimedByOtherRunParams{
		RepoID: run.RepoID, RunID: a.RunID, Tip: a.Tip,
	})
	if err != nil {
		return false, false, fmt.Errorf("read tip claims: %w", err)
	}
	if claimed {
		slog.Warn("checkpoint: a publish attempt's tip is claimed by another run; left to that run",
			"run", a.RunID, "ref", a.Ref, "tip", a.Tip)
		return s.dropPublishAttempt(ctx, a.ID)
	}

	rec, err := s.q.GetCheckpointRetention(ctx, a.RunID)
	hasRec := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, false, fmt.Errorf("read record: %w", err)
	}
	if hasRec && rec.State == retentionSuperseding {
		return false, false, s.deferPublishAttempt(ctx, a, "")
	}
	if !hasRec || !rec.RecoveryRef.Valid {
		if _, perr := s.q.SetRunCheckpointTip(ctx, store.SetRunCheckpointTipParams{
			CheckpointTip: pgtype.Text{String: a.Tip, Valid: true}, ID: a.RunID,
		}); perr != nil {
			return false, false, fmt.Errorf("persist reconciled tip: %w", perr)
		}
		state, terr := s.q.TrackTerminalCheckpointPublish(ctx, store.TrackTerminalCheckpointPublishParams{
			RunID: a.RunID, Branch: a.Branch, Ref: a.Ref, Tip: a.Tip,
		})
		switch {
		case terr == nil:
			slog.Info("checkpoint: a late publish was re-recorded on the run's retention record",
				"run", a.RunID, "ref", a.Ref, "tip", a.Tip, "state", state)
			d, _, derr := s.dropPublishAttempt(ctx, a.ID)
			return d, state == retentionSettling, derr
		case !errors.Is(terr, pgx.ErrNoRows):
			return false, false, fmt.Errorf("track reconciled publish: %w", terr)
		}
		// No row moved: the record names another ref or state tracking refuses; fall through.
	}

	if err := s.beforeRetentionWrite(ctx, a.RunID, "attempt-delete", fence); err != nil {
		return false, false, err
	}
	if derr := s.deleteCheckpointFn(ctx, pushbroker.DeleteOptions{
		CloneURL: f.cloneURL, Branch: a.Branch, Ref: a.Ref, Username: f.username, PAT: f.pat, ExpectedOldTip: a.Tip,
	}); derr != nil {
		return false, false, s.deferPublishAttempt(ctx, a, "delete untracked branch ref: "+scrubForgeError(derr.Error(), f.pat))
	}
	slog.Warn("checkpoint: deleted a branch ref a terminal run's late publish left untracked (its slot was handed on)",
		"run", a.RunID, "ref", a.Ref, "tip", a.Tip)
	return s.dropPublishAttempt(ctx, a.ID)
}

// dropPublishAttempt removes a resolved attempt row under the lock.
func (s *Service) dropPublishAttempt(ctx context.Context, id uuid.UUID) (done, settle bool, err error) {
	n, err := s.q.DeleteCheckpointPublishAttempt(ctx, id)
	if err != nil {
		return false, false, fmt.Errorf("delete publish attempt: %w", err)
	}
	return n > 0, false, nil
}

// deferPublishAttempt pushes the attempt's next comparison out by the retry backoff; a non-empty
// (already scrubbed) note replaces last_error.
func (s *Service) deferPublishAttempt(ctx context.Context, a store.CheckpointPublishAttempt, note string) error {
	var n pgtype.Text
	if note != "" {
		n = pgtype.Text{String: note, Valid: true}
	}
	next := time.Now().Add(retentionBackoff(a.Checks))
	if _, err := s.q.DeferCheckpointPublishAttempt(ctx, store.DeferCheckpointPublishAttemptParams{
		ID: a.ID, NextCheckAt: pgtype.Timestamptz{Time: next, Valid: true}, Note: n,
	}); err != nil {
		return fmt.Errorf("defer publish attempt: %w", err)
	}
	return nil
}
