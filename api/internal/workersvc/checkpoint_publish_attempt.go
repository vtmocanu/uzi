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

// PRD #1810 D2, residual 2: a checkpoint push whose outcome the api never learned.
//
// Publish routes on the run status it reads at its top. A publish routed LIVE pushes without the
// run's retention lock; if the run turns terminal meanwhile, a newer run may supersede the old
// run's retention record while that push is still on the wire. A push the forge already accepted
// is not undone by the api's client giving up (pushbroker's 60-second ceiling bounds the CLIENT,
// not the forge's receive-pack), so the branch ref can move to the old run's tip at any later
// moment the forge's compare-and-swap still matches (the ref is still at the tip the push
// fetched, or still absent when it fetched none: a create): after the run turned terminal, before
// a supersession of its record or inside one (between its recovery-ref create and its branch
// delete), or after a settle deleted the branch ref. Such a ref is tracked by no retention record
// and would block every later publish on the branch.
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
//     run's retention lock, re-records the tip on the run's record (compare-and-set on the record
//     and runs.checkpoint_tip it read before listing origin, so a newer publish is never
//     overwritten) or, once no custody hold of the run is open, CAS-deletes the branch ref at
//     exactly that tip. A supersession's post-delete list and the stuck exit also treat an
//     attempted tip as the run's own. The publish path's own tip persist and record track are
//     compare-and-set too, on what the push observed immediately before its forge call
//     (publishBase), so writes that arrive late never move either BACKWARDS over what the arm or
//     a newer publish recorded; a push whose writes move nothing keeps its row for the arm.
//
// What the attempt record does NOT cover: a row is dropped when its run row is deleted (the arm
// then has no forge coordinates), and retired once publishAttemptHorizon has passed without origin
// carrying its tip. A push the forge applies after either leaves a branch ref no record tracks,
// which blocks a new run's checkpoint on the branch until a human deletes it.
const (
	// livePublishPrePushBudget: see Service.livePublishPrePushBudget. The claim/free supersessions
	// run detached (withRetentionLock) under retentionOpTimeout and can exceed it.
	livePublishPrePushBudget = 2 * time.Minute
	// checkpointSupersessionCooling: see Service.checkpointSupersessionCooling. It is measured
	// from runs.status_since, which the terminal transaction stamps with its own now(): the
	// transaction's START time, not its commit. A publish routed live read the status before that
	// commit became visible, but its routedAt can still be LATER than status_since, by up to the
	// terminal transaction's own duration (start to commit). The last push such a publish sends
	// starts no later than routedAt + livePublishPrePushBudget (checked after the attempt row is
	// written, immediately before the forge call) and the api stops waiting on it
	// pushbroker.MaxPublishDuration later, so the cooling period covers
	// livePublishPrePushBudget + pushbroker.MaxPublishDuration, and supersessionCoolingSlack covers
	// the terminal transaction's duration plus the push's own tip persist and track. Each duration
	// is measured on one clock (the api's for the budget, the database's for the cooling), so no
	// clock offset enters; only rate differences do, which are negligible at these lengths. This
	// is a delay, not the correctness argument (the attempt record is). The constant below fails
	// to compile when the relation breaks.
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
// attempt record or the push's compare-and-set base could not be written or read). Publish answers it with the benign not_descendant skip:
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
	// alreadyCurrent: that push wrote nothing, because origin already held the declared tip
	// (pushbroker.Result.AlreadyCurrent). publishOutcome counts it as published only when the
	// tip is the run's own.
	alreadyCurrent bool
	// base is what the push observed of the run's tracking state immediately before its forge
	// call: the compare-and-set base of its tip persist and record track (publishOutcome).
	base publishBase
}

// publishBase is the run's tracking state a push observed immediately before its forge call
// (observePublishBase). A push's persist (SetRunCheckpointTipIf) and track
// (TrackTerminalCheckpointPublish, AdvanceCheckpointRetentionTip) are compare-and-set on it, so
// writes that arrive late (a live-routed publish holds no retention lock, and its writes can stall
// past the cooling period) never move runs.checkpoint_tip or the record BACKWARDS over a newer
// publish, or over the sweeper's attempts arm re-recording a newer late push.
type publishBase struct {
	// observed: the base was read (retention wired). Unobserved, the persist is unconditional
	// (SetRunCheckpointTip), as no record, attempt row or attempts arm exists to race with.
	observed bool
	// runTip is runs.checkpoint_tip (invalid: none persisted); recordTip the run's retention
	// record tip (invalid: no record).
	runTip, recordTip pgtype.Text
}

// observePublishBase reads the push's compare-and-set base. Inert when retention is not wired.
func (p *checkpointPush) observePublishBase(ctx context.Context) error {
	p.base = publishBase{}
	if !p.s.retentionWired() {
		return nil
	}
	runTip, err := p.s.q.GetRunCheckpointTipForRetention(ctx, p.runID)
	if err != nil {
		return fmt.Errorf("read run checkpoint tip: %w", err)
	}
	var recordTip pgtype.Text
	rec, err := p.s.q.GetCheckpointRetention(ctx, p.runID)
	switch {
	case err == nil:
		recordTip = pgtype.Text{String: rec.Tip, Valid: true}
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("read checkpoint retention: %w", err)
	}
	p.base = publishBase{observed: true, runTip: runTip, recordTip: recordTip}
	return nil
}

// ownsTip reports whether tip is the run's own persisted checkpoint (runs.checkpoint_tip as the
// push observed it immediately before its forge call). Without an observed base (retention not
// wired) every tip counts, the pre-#1810 behaviour: no retention delete runs then.
func (p *checkpointPush) ownsTip(tip string) bool {
	if !p.base.observed {
		return true
	}
	return p.base.runTip.Valid && p.base.runTip.String == tip
}

// pushOnce sends ONE push: the durable attempt record, the compare-and-set base read, the live
// budget check, then the forge call. The budget is checked AFTER the insert and the read,
// immediately before the forge call, so their latency is inside it; a push refused there (or whose
// base could not be read) removes its row (nothing was sent). A push the forge definitively
// refused removes its attempt row at once (nothing landed); an error of unknown outcome keeps it
// for the sweeper. Every push re-reads its base, so the retry after freeCheckpointSlot binds to
// what it observed, not to the first push's view.
func (p *checkpointPush) pushOnce(ctx context.Context) error {
	s := p.s
	p.alreadyCurrent = false
	id, err := s.recordPublishAttempt(ctx, p.runID, p.branch, p.ref, p.opts.DeclaredTip)
	if err != nil {
		slog.Warn("checkpoint: record publish attempt; push not sent", "run", p.runID, "branch", p.branch, "error", err)
		return errPushRefused
	}
	if err := p.observePublishBase(ctx); err != nil {
		s.clearPublishAttempt(ctx, id)
		slog.Warn("checkpoint: read the publish's compare-and-set base; push not sent", "run", p.runID, "branch", p.branch,
			"error", secretscrub.Scrub(err.Error()))
		return errPushRefused
	}
	if p.live {
		if elapsed := s.now().Sub(p.routedAt); elapsed > s.livePublishPrePushBudget {
			s.clearPublishAttempt(ctx, id)
			slog.Warn("checkpoint: live-routed publish exceeded its pre-push budget; push not sent",
				"run", p.runID, "branch", p.branch, "elapsed", elapsed, "budget", s.livePublishPrePushBudget)
			return errPushRefused
		}
	}
	res, perr := s.publishFn(ctx, p.opts)
	switch {
	case perr == nil:
		p.landed = id
		p.alreadyCurrent = res.AlreadyCurrent
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
	ctx, cancel := retentionBookkeepingCtx(ctx)
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
//
// Every row the arm could not resolve has its next comparison pushed out, whatever stopped it (a
// read or write error, a recovered panic, the lock held elsewhere, the locked step's own deadline,
// including a fence that failed only because it ran on the expired context), so a row that keeps
// failing never keeps its place at the head of the bounded, next_check_at-ordered page. The one
// exception is a lock that is not held: lost while the step's deadline was still live (a fence
// started before the deadline failed), or found gone by the expiry path's fence re-check after the
// deadline (lockedRetentionStep). Another holder may be reconciling the row, so it is left to them,
// as the record arms leave a record.
//
// The arm checks the pass budget before starting each row (and before the settle a row owes); each
// locked step runs under pass.timeout(). A settle the budget stops is left to the work arm, which
// lists the settling record on a later pass.
func (s *Service) reconcilePublishAttempts(ctx context.Context, onlyRun pgtype.UUID, pass *retentionPass) (int64, error) {
	due, err := s.q.ListDueCheckpointPublishAttempts(ctx, store.ListDueCheckpointPublishAttemptsParams{
		Cooling:   pgtype.Interval{Microseconds: s.checkpointSupersessionCooling.Microseconds(), Valid: true},
		OnlyRunID: onlyRun, MaxRows: reconcileAttemptBatch,
	})
	if err != nil {
		return 0, fmt.Errorf("list checkpoint publish attempts: %w", err)
	}
	var progressed int64
	for i, a := range due {
		if pass.spent(ctx) {
			pass.leave(len(due) - i)
			break
		}
		// expiredDeferred: the expiry bookkeeping deferred the row under the lock (its fence
		// re-check passed).
		var settle, expiredDeferred bool
		done, acquired, err := s.lockedRetentionStep(ctx, a.RunID, pass.timeout(), func(ctx context.Context, _ uuid.UUID, fence func(context.Context) error) (bool, error) {
			var (
				d   bool
				err error
			)
			d, settle, err = s.reconcilePublishAttemptLocked(ctx, a.ID, fence)
			return d, err
		}, func(ctx context.Context, _ uuid.UUID, msg string) {
			// The step ran out of time and the lock is still held (re-checked): defer the row now,
			// on the expiry path's bookkeeping context.
			expiredDeferred = true
			s.deferPublishAttemptLogged(ctx, a, "operation timed out: "+msg)
		})
		if err != nil {
			slog.Warn("sweeper: checkpoint publish attempt", "run", a.RunID, "attempt", a.ID, "error", err)
			// Deferred already (expired), or the lock is not held (lost while live, or found gone
			// by the expiry re-check): another holder owns the row.
			if !expiredDeferred && !errors.Is(err, ErrRetentionLockLost) {
				s.deferPublishAttemptLogged(ctx, a, secretscrub.Scrub(err.Error()))
			}
			continue
		}
		if !acquired {
			s.deferPublishAttemptLogged(ctx, a, "retention lock busy")
			continue
		}
		if done {
			progressed++
		}
		if settle && !pass.spent(ctx) {
			if _, _, err := s.settleRetainedCheckpointOnce(ctx, a.RunID, pass.timeout()); err != nil {
				slog.Warn("sweeper: checkpoint retention settle after a reconciled publish", "run", a.RunID, "error", err)
			}
		}
	}
	return progressed, nil
}

// reconcilePublishAttemptLocked compares one outstanding attempt with origin, under its run's
// retention lock. The run is terminal, so every publish of it ROUTED terminal takes the same lock;
// a publish routed while the run was still live does not, and may land, persist and track a NEWER
// tip at any moment. So the run row (runs.checkpoint_tip) and the run's record are read BEFORE
// origin is listed, and every re-record is compare-and-set on exactly what was read: a newer
// publish that moved either meanwhile makes the write move nothing, and the row is re-checked on a
// later pass instead of overwriting it.
//
//   - origin's branch ref is NOT at the attempted tip: nothing of this attempt landed yet (or a
//     newer push already replaced it). The row is checked again after a backoff, and retired once
//     publishAttemptHorizon has passed;
//   - it IS, and another run claims that tip: not provably ours; the row is dropped;
//   - it IS, and the record is superseding: left to the supersession, whose post-delete list and
//     stuck exit treat an attempted tip as the run's own;
//   - it IS, and the run's record can track it (no record, or one naming the branch ref that
//     never named a recovery ref): the tip is re-recorded on the record and persisted on the run,
//     both compare-and-set in one transaction; a settling result is owed a settle (settle true);
//   - it IS, and the run's slot was handed on (the record names a recovery ref, or tracking it is
//     refused): while any custody hold of the run is open the ref is kept (it may be the newest
//     copy of the run's work) and the row deferred; once none is, behind the fence, the branch ref
//     is CAS-deleted at exactly the attempted tip, as the terminal publish of a superseded run
//     would have been refused.
//
// The custody gate has a cost, the same one the supersession's stuck exit accepts: while the old
// run's custody hold is open, the late tip stays on the branch ref, and the NEW run that took the
// slot cannot publish over it (its checkpoints are refused with the benign not_descendant skip)
// until custody releases and the ref is deleted. Custody wins because that ref may hold the only
// copy of work the hold protects; the new run's checkpoints are best-effort and resume after.
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
		return s.deferPublishAttempt(ctx, a, problem)
	}

	// Observed BEFORE the list: the compare-and-set bases of every write below.
	run, err := s.q.GetRunByID(ctx, a.RunID)
	if err != nil {
		return false, false, fmt.Errorf("read run: %w", err)
	}
	rec, err := s.q.GetCheckpointRetention(ctx, a.RunID)
	hasRec := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, false, fmt.Errorf("read record: %w", err)
	}

	tips, lerr := s.listRefTipsFn(ctx, pushbroker.ListRefsOptions{CloneURL: f.cloneURL, Username: f.username, PAT: f.pat}, a.Ref)
	if lerr != nil {
		return s.deferPublishAttempt(ctx, a, "list branch ref: "+scrubForgeError(lerr.Error(), f.pat))
	}
	if tip, ok := tips[a.Ref]; !ok || tip != a.Tip {
		if a.AttemptedAt.Valid && time.Since(a.AttemptedAt.Time) > publishAttemptHorizon {
			return s.dropPublishAttempt(ctx, a.ID)
		}
		return s.deferPublishAttempt(ctx, a, "")
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

	if hasRec && rec.State == retentionSuperseding {
		return s.deferPublishAttempt(ctx, a, "")
	}
	if !hasRec || recordTracksPublish(rec, a.Ref) {
		var expected pgtype.Text
		if hasRec {
			expected = pgtype.Text{String: rec.Tip, Valid: true}
		}
		state, moved, err := s.reRecordPublishedTip(ctx, a, expected, run.CheckpointTip)
		if err != nil {
			return false, false, err
		}
		if !moved {
			return s.deferPublishAttempt(ctx, a, "the run's record or checkpoint tip moved while origin was listed; re-checked later")
		}
		slog.Info("checkpoint: a late publish was re-recorded on the run's retention record",
			"run", a.RunID, "ref", a.Ref, "tip", a.Tip, "state", state)
		d, _, derr := s.dropPublishAttempt(ctx, a.ID)
		return d, state == retentionSettling, derr
	}

	held, err := s.q.RunHasOpenCustodyHold(ctx, a.RunID)
	if err != nil {
		return false, false, fmt.Errorf("read custody: %w", err)
	}
	if held {
		return s.deferPublishAttempt(ctx, a, "the run's slot was handed on but a custody hold is open; the branch ref is kept until custody releases")
	}
	if err := s.beforeRetentionWrite(ctx, a.RunID, "attempt-delete", fence); err != nil {
		return false, false, err
	}
	if derr := s.deleteCheckpointFn(ctx, pushbroker.DeleteOptions{
		CloneURL: f.cloneURL, Branch: a.Branch, Ref: a.Ref, Username: f.username, PAT: f.pat, ExpectedOldTip: a.Tip,
	}); derr != nil {
		return s.deferPublishAttempt(ctx, a, "delete untracked branch ref: "+scrubForgeError(derr.Error(), f.pat))
	}
	slog.Warn("checkpoint: CAS delete sent for a branch ref at a terminal run's late-publish tip (its slot was handed on); "+
		"a no-op if origin had moved on", "run", a.RunID, "ref", a.Ref, "tip", a.Tip)
	return s.dropPublishAttempt(ctx, a.ID)
}

// recordTracksPublish reports whether TrackTerminalCheckpointPublish's conflict arm can move rec
// to a publish of ref: it names that ref, never named a recovery ref, and is in a state tracking
// advances or reopens.
func recordTracksPublish(rec store.CheckpointRetention, ref string) bool {
	if rec.RecoveryRef.Valid || rec.Ref != ref {
		return false
	}
	switch rec.State {
	case retentionRetained, retentionSettling, "deleted", "abandoned":
		return true
	}
	return false
}

// reRecordPublishedTip re-records a late-landed tip: the run's record (insert, advance or reopen)
// and runs.checkpoint_tip, each compare-and-set on the value observed before origin was listed
// (expectedRecordTip NULL: no record; expectedRunTip NULL: no tip persisted). Both statements run
// in one transaction, so either both move or neither does; moved false means one of them found a
// newer value and nothing was written. Without a wired transaction beginner (fake-store tests) the
// record is written first and the run tip second, with no rollback.
func (s *Service) reRecordPublishedTip(ctx context.Context, a store.CheckpointPublishAttempt, expectedRecordTip, expectedRunTip pgtype.Text) (state string, moved bool, err error) {
	q := s.q
	if s.txBeginner != nil {
		tx, berr := s.txBeginner.Begin(ctx)
		if berr != nil {
			return "", false, fmt.Errorf("begin re-record: %w", berr)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		q = store.New(tx)
		defer func() {
			if err == nil && moved {
				if cerr := tx.Commit(ctx); cerr != nil {
					state, moved, err = "", false, fmt.Errorf("commit re-record: %w", cerr)
				}
			}
		}()
	}
	state, terr := q.TrackReconciledCheckpointPublish(ctx, store.TrackReconciledCheckpointPublishParams{
		RunID: a.RunID, Branch: a.Branch, Ref: a.Ref, Tip: a.Tip, ExpectedTip: expectedRecordTip,
	})
	if errors.Is(terr, pgx.ErrNoRows) {
		return "", false, nil
	}
	if terr != nil {
		return "", false, fmt.Errorf("track reconciled publish: %w", terr)
	}
	n, perr := q.SetRunCheckpointTipIf(ctx, store.SetRunCheckpointTipIfParams{
		CheckpointTip: a.Tip, ID: a.RunID, ExpectedTip: expectedRunTip,
	})
	if perr != nil {
		return "", false, fmt.Errorf("persist reconciled tip: %w", perr)
	}
	if n == 0 {
		return "", false, nil
	}
	return state, true, nil
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
// (already scrubbed) note replaces last_error. It runs on retentionBookkeepingCtx, so the
// bookkeeping still lands when the locked step's own context has expired (within the one window
// past the operation's deadline that context shares).
func (s *Service) deferPublishAttempt(ctx context.Context, a store.CheckpointPublishAttempt, note string) (done, settle bool, err error) {
	var n pgtype.Text
	if note != "" {
		n = pgtype.Text{String: note, Valid: true}
	}
	ctx, cancel := retentionBookkeepingCtx(ctx)
	defer cancel()
	next := time.Now().Add(retentionBackoff(a.Checks))
	if _, err := s.q.DeferCheckpointPublishAttempt(ctx, store.DeferCheckpointPublishAttemptParams{
		ID: a.ID, NextCheckAt: pgtype.Timestamptz{Time: next, Valid: true}, Note: n,
	}); err != nil {
		return false, false, fmt.Errorf("defer publish attempt: %w", err)
	}
	return false, false, nil
}

// deferPublishAttemptLogged is deferPublishAttempt for a row the locked step did not resolve or
// defer itself (an error, a recovered panic, the lock held elsewhere); a failure is only logged.
func (s *Service) deferPublishAttemptLogged(ctx context.Context, a store.CheckpointPublishAttempt, note string) {
	if _, _, err := s.deferPublishAttempt(ctx, a, note); err != nil {
		slog.Warn("sweeper: checkpoint publish attempt bookkeeping", "run", a.RunID, "attempt", a.ID, "error", err)
	}
}
