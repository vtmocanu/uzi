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
// read error fails the pass (Sweep logs it and still runs its later passes).

// reconcileRetentionBatch bounds each arm's page in one ReconcileCheckpointRetentions pass: a
// record may cost a few forge round-trips, and the pass runs on the sweeper's tick.
const reconcileRetentionBatch = 10

const (
	// retentionPassBudget: see Service.retentionPassBudget.
	retentionPassBudget = 30 * time.Second
	// retentionSweepOpTimeout: see Service.retentionSweepOpTimeout. It bounds one record's locked
	// operation in the sweeper's pass, and it is deliberately NOT sized to the pushbroker per-call
	// ceilings (pushbroker.MaxDeleteDuration, MaxCreateRefDuration: 30s each) times the longest
	// operation's forge calls: that product (5 x 30s) would let one record hold the sweeper's
	// single goroutine for minutes, every tick such a record is due. The longest locked operation
	// is a superseding record's re-drive (reconcileSuperseding -> driveSupersession): CreateRef,
	// the ErrSourceMissing list (resolveMissingSource), the branch-ref Delete, the post-delete list
	// (branchHeldByOwnPublish) and, with no hold open, the recovery-ref Delete (deleteSettlingRef),
	// five calls; the stuck exit (exitStuckSupersessionLocked) makes three (a list and up to two
	// deletes), the audit and attempts arms two, a settle one.
	//
	// The relation, documented rather than compiled: 60s fits five forge calls of a slow but
	// healthy forge answering in about 10s each, plus the operation's database work. A forge
	// slower than about 12s per call makes a sweeper re-drive time out; the step's failure
	// bookkeeping then backs the record off (last_error set). That does not strand it:
	//
	//   - every step of driveSupersession is idempotent against origin (CreateRef answers a
	//     recovery ref already at the tip with ErrRefExistsAtTip from its own list, before it looks
	//     at the branch ref; a CAS delete of an absent ref succeeds), so what a timed-out attempt
	//     completed on origin stays done, though a re-drive still makes every call;
	//   - with no custody hold open, the next sweeper pass takes the three-call stuck exit instead
	//     of a re-drive (the timeout set last_error);
	//   - the publish path's claim/free re-drive (claimCheckpointSlot, freeCheckpointSlot ->
	//     supersedeRetainedCheckpoint -> driveSupersession) runs under retentionOpTimeout
	//     (2 minutes: about 24s per call for the five), not this bound.
	retentionSweepOpTimeout = 60 * time.Second
)

// Compile-time: one forge call at its own pushbroker ceiling ends inside retentionSweepOpTimeout,
// so a hung forge's first call fails and is recorded before the operation's deadline (a negative
// constant does not convert to uint). This is the only relation to the ceilings that is compiled;
// the five-call relation above is documented, not enforced.
const (
	_ = uint(retentionSweepOpTimeout - pushbroker.MaxDeleteDuration - 1)
	_ = uint(retentionSweepOpTimeout - pushbroker.MaxCreateRefDuration - 1)
)

// Notes persisted on a record's last_error by the M4 arms (never carry forge output).
const (
	retentionGoneNote     = "run, repository or forge connection no longer exists"
	stuckExitNote         = "supersession stopped and no custody hold is open; the run's own refs on origin were deleted"
	auditOtherTipNote     = "recovery ref found at another tip during the post-settlement audit; left in place"
	auditUnverifiableNote = "post-settlement audit could not run: " + retentionGoneNote
	auditReopenedNote     = "recovery ref found at the recorded tip while custody is open; reopened as superseded"
)

// ReconcileCheckpointRetentions is the sweeper's checkpoint-retention pass (PRD #1810 M3/M4).
// Returns the number of records it drove to a final step (a ref settled or verified, a record
// backfilled, a supersession finished or exited). Inert unless every retention seam is wired. A
// returned error (a candidate-list read) ends this pass only: Sweep logs it and continues.
//
// The pass is time-bounded so a slow or hung forge cannot stall Sweep's later passes: it starts no
// record once Service.retentionPassBudget has passed since it began (the rest are left for the
// next tick, and the pass returns without an error), and each record it starts runs under
// Service.retentionSweepOpTimeout rather than the publish path's retentionOpTimeout.
func (s *Service) ReconcileCheckpointRetentions(ctx context.Context) (int64, error) {
	return s.reconcileCheckpointRetentions(ctx, pgtype.UUID{})
}

// retentionPass is one reconciliation pass's time budget. Every arm checks spent BEFORE STARTING
// each record, never inside one (the attempts arm checks it again before the settle a
// re-recorded tip owes, which is a second locked operation), and runs the record's locked
// operation under opTimeout.
//
// The worst case, with forge calls that honour their context: the last operation starts just
// before the budget runs out and runs its full opTimeout. After its deadline come:
//
//  1. its bookkeeping, all of it within ONE retentionRecordTimeout (10s) window past the
//     operation's deadline (retentionBookkeepingCtx caps every bookkeeping context derived from a
//     locked operation at that deadline + 10s): the step's own failure write (recordRetentionFailure,
//     deferRetentionVerify or deferPublishAttempt) after the forge call that hit the deadline, and,
//     when the step still returned an error (a fence or database read on the expired context, or a
//     failed write), lockedRetentionStep's expiry bookkeeping (the fence re-check, then
//     deferExpiredRetentionStep's read and write, or the attempt row's defer);
//  2. the unlock (releaseRetentionLock, its own 10s, retentionUnlockTimeout);
//  3. destroyRetentionConn (its own 10s), only when the unlock failed.
//
// In the attempts arm a row whose step panicked is deferred by the arm itself AFTER the unlock, on
// a fresh 10s context; its expiry bookkeeping then never ran (a recovered panic skips it), so it
// takes the place of 1, never adding to it.
//
// So with the defaults a pass runs for at most retentionPassBudget + retentionSweepOpTimeout +
// 3 x 10s = 30s + 60s + 30s = 120s, below the sweeper health beat's danger line (ten 15s
// intervals, 150s) with the default interval. The attempts arm's owed settle is a second locked
// operation, but it too starts only while the budget is unspent, so it is the pass's last
// operation or none (the same bound); a backfill record (two inserts on one 10s bookkeeping
// context and a re-read) is shorter. Not counted: the candidate-list reads and the backfill arm's
// re-read, which run on the sweeper's own context (a list started just before the budget runs out
// starts none of its records), and Sweep's other passes. With the real pushbroker a hung forge
// ends sooner: its first call fails at its own ceiling (30s), and the step records the failure and
// returns. The LiveDB test TestSweepRetentionPassBudgetLiveDB measures the bound against a forge
// that hangs until its context ends. A nil *retentionPass is unbounded (the direct arm calls in
// tests).
type retentionPass struct {
	deadline  time.Time
	opTimeout time.Duration
	// left counts the listed records the pass did not start; notStarted names the arms it did
	// not list at all. Both only feed the budget log line.
	left       int
	notStarted []string
}

func (s *Service) newRetentionPass() *retentionPass {
	return &retentionPass{deadline: time.Now().Add(s.retentionPassBudget), opTimeout: s.retentionSweepOpTimeout}
}

// spent reports that the pass must start no further record: its budget has passed, or the
// sweeper's own context is done.
func (p *retentionPass) spent(ctx context.Context) bool {
	return p != nil && (ctx.Err() != nil || !time.Now().Before(p.deadline))
}

// leave records n listed records the pass stopped before starting.
func (p *retentionPass) leave(n int) {
	if p != nil {
		p.left += n
	}
}

// timeout is the bound of one record's locked operation.
func (p *retentionPass) timeout() time.Duration {
	if p == nil {
		return retentionOpTimeout
	}
	return p.opTimeout
}

// logLeft reports, once per pass, what the spent budget left for the next tick.
func (p *retentionPass) logLeft() {
	if p == nil || (p.left == 0 && len(p.notStarted) == 0) {
		return
	}
	slog.Info("sweeper: checkpoint retention pass budget spent; the rest is left for the next tick",
		"records_left", p.left, "arms_not_started", p.notStarted, "budget_exceeded_by", time.Since(p.deadline).Round(time.Millisecond))
}

// retentionArm is one forge-calling arm of the pass.
type retentionArm struct {
	name string
	run  func(ctx context.Context, onlyRun pgtype.UUID, pass *retentionPass) (int64, error)
}

// retentionArmOrder is the order the pass runs its n forge-calling arms in when its rotation
// cursor reads cursor: the canonical order rotated to start at arm cursor mod n. Successive passes
// start from successive arms, so over any n consecutive passes every arm runs first once, and a
// burst of slow records in one arm (which spends the budget) cannot keep the arms after it from
// ever starting.
func retentionArmOrder(cursor uint32, n int) []int {
	order := make([]int, n)
	if n == 0 {
		return order
	}
	start := int(cursor) % n
	for i := range order {
		order[i] = (start + i) % n
	}
	return order
}

// reconcileCheckpointRetentions is the pass body. onlyRun confines every arm's candidate list to
// one run (the LiveDB tests, so a reused database's leftover records are never driven against a
// test's forge); the zero value lists every run, as production does. The arms:
//
//  1. backfill (always first, and it makes no forge call): terminal runs that own a checkpoint ref
//     but have no record get one (retained with an open hold, else settling), so the work arm
//     settles a new settling record the same pass (budget permitting).
//     Since migration 00266 every terminal transition records its run in the same transaction
//     (the runs.status trigger), so this arm is left with terminal runs whose first checkpoint
//     tip was persisted only AFTER the transition (the trigger saw none) and whose own track
//     insert did not record them: TrackTerminalCheckpointPublish failed, or retention was not
//     wired in the api that served the publish;
//
// then the four forge-calling arms, in the canonical order below rotated by one arm per pass
// (retentionArmOrder, Service.retentionArmCursor). No forge arm depends on another having run
// earlier in the same pass: each settles what it moves to settling itself, and what one leaves
// for another is due on a later pass anyway.
//
//  2. attempts: an outstanding terminal push or ready live push (checkpoint_publish_attempts,
//     written before the push and awaiting confirmation) is compared with origin's branch ref.
//     A ready live attempt only confirms the run's tip, with no retention or forge mutation.
//     A terminal attempt can re-record the run's retention record (compare-and-set on what was
//     read before the list), or CAS-delete an untracked ref when its slot was handed on and no
//     custody hold is open (reconcilePublishAttempts, #1810 residual 2 and #2395);
//  3. work: a due `settling` record retries its CAS delete; a due `superseding` record is
//     re-driven, or, when its supersession stopped (last_error set) and no hold is open, exited;
//  4. unheld: a `retained`/`superseded` record whose run has no open hold moves to settling and
//     its ref is CAS-deleted;
//  5. audit: a deleted record that named a recovery ref is re-verified once verify_after passed
//     (a recovery ref found at the recorded tip while custody is open reopens the record instead
//     of being deleted).
//
// Every arm, backfill included, checks the pass budget (retentionPass) before starting each
// record; once it is spent the remaining records and arms are left for the next tick, logged once,
// and the pass returns what it progressed with a nil error, so Sweep's later passes run.
func (s *Service) reconcileCheckpointRetentions(ctx context.Context, onlyRun pgtype.UUID) (int64, error) {
	if !s.supersessionWired() {
		return 0, nil
	}
	pass := s.newRetentionPass()
	defer pass.logLeft()
	// Only an unconfined pass saw every candidate, so only it may advance the global watermark.
	progressed, err := s.backfillCheckpointRetentions(ctx, onlyRun, !onlyRun.Valid, pass)
	if err != nil {
		return 0, err
	}
	arms := []retentionArm{
		{"attempts", s.reconcilePublishAttempts},
		{"work", s.reconcileRetentionWork},
		{"unheld", s.reconcileUnheldRetentions},
		{"audit", s.reconcileRetentionAudit},
	}
	for _, i := range retentionArmOrder(s.retentionArmCursor.Add(1)-1, len(arms)) {
		if pass.spent(ctx) {
			pass.notStarted = append(pass.notStarted, arms[i].name)
			continue
		}
		n, err := arms[i].run(ctx, onlyRun, pass)
		progressed += n
		if err != nil {
			return progressed, err
		}
	}
	return progressed, nil
}

// reconcileRetentionWork is the pass's work arm: due `settling` records retry their CAS delete,
// due `superseding` records are re-driven or exited (reconcileSuperseding).
func (s *Service) reconcileRetentionWork(ctx context.Context, onlyRun pgtype.UUID, pass *retentionPass) (int64, error) {
	work, err := s.q.ListCheckpointRetentionWork(ctx, store.ListCheckpointRetentionWorkParams{
		States: []string{retentionSuperseding, retentionSettling}, OnlyRunID: onlyRun, MaxRows: reconcileRetentionBatch,
	})
	if err != nil {
		return 0, fmt.Errorf("list checkpoint retention work: %w", err)
	}
	var progressed int64
	for i, r := range work {
		if pass.spent(ctx) {
			pass.leave(len(work) - i)
			break
		}
		var (
			done bool
			err  error
		)
		switch r.State {
		case retentionSuperseding:
			done, _, err = s.reconcileSuperseding(ctx, r.RunID, pass.timeout())
		case retentionSettling:
			done, _, err = s.settleRetainedCheckpointOnce(ctx, r.RunID, pass.timeout())
		}
		if err != nil {
			slog.Warn("sweeper: checkpoint retention", "run", r.RunID, "state", r.State, "error", secretscrub.Scrub(err.Error()))
			continue
		}
		if done {
			progressed++
		}
	}
	return progressed, nil
}

// reconcileUnheldRetentions is the pass's unheld arm: a retained/superseded record whose run has
// no open hold is settled.
func (s *Service) reconcileUnheldRetentions(ctx context.Context, onlyRun pgtype.UUID, pass *retentionPass) (int64, error) {
	unheld, err := s.q.ListUnheldCheckpointRetentions(ctx, store.ListUnheldCheckpointRetentionsParams{
		OnlyRunID: onlyRun, MaxRows: reconcileRetentionBatch,
	})
	if err != nil {
		return 0, fmt.Errorf("list unheld checkpoint retentions: %w", err)
	}
	var progressed int64
	for i, r := range unheld {
		if pass.spent(ctx) {
			pass.leave(len(unheld) - i)
			break
		}
		done, _, err := s.settleRetainedCheckpointOnce(ctx, r.RunID, pass.timeout())
		if err != nil {
			slog.Warn("sweeper: checkpoint retention settle", "run", r.RunID, "error", secretscrub.Scrub(err.Error()))
			continue
		}
		if done {
			progressed++
		}
	}
	return progressed, nil
}

// reconcileRetentionAudit is the pass's audit arm: a deleted record that named a recovery ref is
// re-verified (auditRecoveryRefLocked).
func (s *Service) reconcileRetentionAudit(ctx context.Context, onlyRun pgtype.UUID, pass *retentionPass) (int64, error) {
	audit, err := s.q.ListCheckpointRetentionAudit(ctx, store.ListCheckpointRetentionAuditParams{
		OnlyRunID: onlyRun, MaxRows: reconcileRetentionBatch,
	})
	if err != nil {
		return 0, fmt.Errorf("list checkpoint retention audit: %w", err)
	}
	var progressed int64
	for i, r := range audit {
		if pass.spent(ctx) {
			pass.leave(len(audit) - i)
			break
		}
		done, _, err := s.lockedRetentionStep(ctx, r.RunID, pass.timeout(), s.auditRecoveryRefLocked, s.deferExpiredRetentionStep)
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

// backfillCheckpointRetentions is the pass's backfill arm: it records every listed terminal run
// that has no record (recordCheckpointRetention) and, when advance is set, moves the persisted
// watermark (checkpoint_retention_meta.backfilled_through) to what the page proved
// (backfillWatermark). The list starts 10 minutes below the watermark, so a run whose transaction
// committed after a pass moved the watermark past its backfill key (the later of its terminal
// transition and its last publish) is still listed.
func (s *Service) backfillCheckpointRetentions(ctx context.Context, onlyRun pgtype.UUID, advance bool, pass *retentionPass) (int64, error) {
	// The database clock BEFORE the list: a page that was not full proves only up to here.
	listedAt, err := s.q.GetCheckpointRetentionBackfillNow(ctx)
	if err != nil {
		return 0, fmt.Errorf("read backfill clock: %w", err)
	}
	page, err := s.q.ListCheckpointRetentionBackfill(ctx, store.ListCheckpointRetentionBackfillParams{
		OnlyRunID: onlyRun, MaxRows: reconcileRetentionBatch,
	})
	if err != nil {
		return 0, fmt.Errorf("list checkpoint retention backfill: %w", err)
	}
	var progressed int64
	recorded := make([]bool, len(page))
	for i, r := range page {
		if pass.spent(ctx) {
			// Not started: recorded[i:] stay false, so the watermark stops below the first of
			// them and the next pass lists them again.
			pass.leave(len(page) - i)
			break
		}
		inserted, _ := s.recordCheckpointRetention(ctx, r.ID, r.Kind, r.IssueIid)
		if inserted {
			slog.Info("sweeper: checkpoint retention backfilled", "run", r.ID)
			progressed++
			recorded[i] = true
			continue
		}
		// Not inserted now: recorded only if a record exists (a racing terminal writer wrote it).
		// A failed insert, or a hold that appeared between the two inserts, leaves no record, and
		// the watermark must not pass the run.
		_, gerr := s.q.GetCheckpointRetention(ctx, r.ID)
		recorded[i] = gerr == nil
	}
	if !advance {
		return progressed, nil
	}
	through := backfillWatermark(page, recorded, len(page) >= reconcileRetentionBatch, listedAt)
	if _, err := s.q.AdvanceCheckpointRetentionBackfillWatermark(ctx, through); err != nil {
		// Not fatal: the watermark only bounds the scan; the next pass re-lists from the old one.
		slog.Warn("sweeper: checkpoint retention backfill watermark", "error", err)
	}
	return progressed, nil
}

// backfillWatermark is the watermark a backfill page proves: every candidate whose backfill key
// (GREATEST(status_since, checkpoint_tip_at)) is below the returned instant, and that was committed
// when the page was listed, has a record. A page that was not full held every such candidate, so
// it proves up to listedAt, the database clock read immediately before the list (not the later
// moment the watermark is written). A full page proves only up to its last row's key (later
// candidates, and ties with it, were not listed). A run left unrecorded caps it at that run's key,
// so a failing run is re-listed every pass and is never skipped. The page is ordered by the key.
func backfillWatermark(page []store.ListCheckpointRetentionBackfillRow, recorded []bool, full bool, listedAt pgtype.Timestamptz) pgtype.Timestamptz {
	through := listedAt
	if full && len(page) > 0 {
		through = page[len(page)-1].BackfillKey
	}
	for i, r := range page {
		if !recorded[i] {
			if !through.Valid || r.BackfillKey.Time.Before(through.Time) {
				through = r.BackfillKey
			}
			break // ordered: the first unrecorded run is the earliest
		}
	}
	return through
}

// lockedRetentionStep runs one locked step for runID under its retention lock (try semantics), the
// whole operation bounded by timeout (withRetentionLockTimeout), recovering a panic from the go-git
// seams into err. done is meaningful only when acquired.
//
// expired, when non-nil, is the step's failure bookkeeping for an operation that ran out of time:
// it runs, still under the lock and on the pinned session, when the step returned an error after
// the operation's own deadline passed. A forge call that returns just before the deadline leaves
// the step's next fence or database read to run on the expired context; that error is not a forge
// failure, so the step records nothing for it, and without this the record would keep its place at
// the head of its arm's due-ordered page on every later pass.
//
// It never runs when the lock is not held:
//
//   - a fence failed while the deadline was still live (the context not yet done when the fence
//     returned): the lock was genuinely lost (the session ended, or the lock was released under
//     it) and another holder may own the record;
//   - the lock was lost at any other moment, a forge call that ran into the deadline included, and
//     the step's next fence then failed only on the expired context: so before expired runs, the
//     fence is re-checked on a fresh bookkeeping context (retentionBookkeepingCtx, the pinned
//     connection is still held here), and when that re-check fails (the lock is gone, or the
//     re-check itself could not run) nothing is written, and the returned error wraps
//     ErrRetentionLockLost as a failed live fence's does.
//
// expired receives that bookkeeping context (its writes share its deadline) and the scrubbed
// error.
func (s *Service) lockedRetentionStep(ctx context.Context, runID uuid.UUID, timeout time.Duration,
	step func(ctx context.Context, runID uuid.UUID, fence func(context.Context) error) (bool, error),
	expired func(ctx context.Context, runID uuid.UUID, msg string),
) (done, acquired bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("checkpoint retention step panicked: %s", secretscrub.Scrub(fmt.Sprint(r)))
		}
	}()
	acquired, err = s.withRetentionLockTimeout(ctx, runID, timeout, func(ctx context.Context, fence func(context.Context) error) error {
		// lostLive: a fence failed while the deadline was still live (checked when the fence
		// returned, so a fence the deadline cut short mid-query counts as expired, not lost).
		lostLive := false
		watched := func(fctx context.Context) error {
			ferr := fence(fctx)
			if ferr != nil && fctx.Err() == nil {
				lostLive = true
			}
			return ferr
		}
		var ferr error
		done, ferr = step(ctx, runID, watched)
		if ferr != nil && expired != nil && !lostLive && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			if lerr := s.expiredStepBookkeeping(ctx, runID, fence, expired, secretscrub.Scrub(ferr.Error())); lerr != nil {
				// The lock is not held: the caller must treat the step as a lost lock too (the
				// attempts arm then leaves the row to whoever holds it).
				ferr = fmt.Errorf("%w; re-checked after the deadline: %w", ferr, lerr)
			}
		}
		return ferr
	})
	return done && acquired, acquired, err
}

// expiredStepBookkeeping runs expired for an operation that ran out of time, but only once the
// fence, re-checked on a fresh bookkeeping context, shows the lock is still held. It returns the
// fence's error (wrapping ErrRetentionLockLost) when it wrote nothing for that reason. A re-check
// that could not run in the bookkeeping window counts as a lost lock too: nothing is written.
func (s *Service) expiredStepBookkeeping(ctx context.Context, runID uuid.UUID, fence func(context.Context) error,
	expired func(ctx context.Context, runID uuid.UUID, msg string), msg string,
) error {
	bctx, cancel := retentionBookkeepingCtx(ctx)
	defer cancel()
	if err := fence(bctx); err != nil {
		slog.Warn("checkpoint retention: expired step bookkeeping skipped; the retention lock is not held",
			"run", runID, "error", secretscrub.Scrub(err.Error()))
		return err
	}
	expired(bctx, runID, msg)
	return nil
}

// deferExpiredRetentionStep is lockedRetentionStep's expiry bookkeeping for the record arms: on
// ctx (the expiry path's bookkeeping context, whose deadline every write below shares) it re-reads
// the run's record and pushes its next attempt out by the retry backoff, guarded on the state it
// read (recordRetentionFailure), or, for a deleted record whose audit is pending, pushes the audit
// out (deferRetentionVerify). A record in any other state, or none, is left alone; a failure is
// only logged.
func (s *Service) deferExpiredRetentionStep(ctx context.Context, runID uuid.UUID, msg string) {
	bctx, cancel := retentionBookkeepingCtx(ctx)
	defer cancel()
	row, err := s.q.GetCheckpointRetention(bctx, runID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("checkpoint retention: expired step bookkeeping: read record", "run", runID, "error", secretscrub.Scrub(err.Error()))
		}
		return
	}
	msg = "operation timed out: " + msg
	switch row.State {
	case retentionRetained, retentionSuperseding, retentionSuperseded, retentionSettling:
		err = s.recordRetentionFailure(bctx, row, row.State, msg)
	case "deleted":
		if row.RecoveryRef.Valid && !row.VerifiedAt.Valid {
			err = s.deferRetentionVerify(bctx, row, msg)
		}
	}
	if err != nil {
		slog.Warn("checkpoint retention: expired step bookkeeping", "run", runID, "error", secretscrub.Scrub(err.Error()))
	}
}

// reconcileSuperseding is the sweeper's arm for a due `superseding` record. Under the lock it
// re-reads the record; one whose supersession STOPPED (last_error set: the tip lagged a later
// publish, the recovery ref sits at another tip, or a forge step keeps failing) and whose run has
// NO open custody hold is exited (exitStuckSupersessionLocked), since nothing needs its tip any
// more. Every other superseding record is re-driven from step 2 (driveSupersession). The whole
// operation is bounded by timeout.
func (s *Service) reconcileSuperseding(ctx context.Context, runID uuid.UUID, timeout time.Duration) (done, acquired bool, err error) {
	return s.lockedRetentionStep(ctx, runID, timeout, func(ctx context.Context, runID uuid.UUID, fence func(context.Context) error) (bool, error) {
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
	}, s.deferExpiredRetentionStep)
}

// exitStuckSupersessionLocked settles a stopped `superseding` record whose run has no open hold
// (PRD #1810 M4): under the run's lock it lists both refs once, then, each behind the fence,
// CAS-deletes the recovery ref if origin holds it at the recorded tip (only then is it ours) and
// the branch ref if origin holds it at a tip provably this run's own publish, and closes the
// record as deleted (with a verify_after, since it names a recovery ref).
//
// The branch ref is this run's at the recorded tip, and also at a tip provably the run's own
// (ownPublishedTip): runs.checkpoint_tip of THIS run (the tip-lag case, where the record's tip lags
// a later publish of the same run), or a tip the run attempted to push and has not accounted for
// (checkpoint_publish_attempts: a publish whose tip persist failed, or whose outcome the api never
// learned). A branch ref at any other tip, and a recovery ref at any tip but the recorded one, is
// left alone.
//
// Residual, by design: a branch ref at a tip that is none of those (another writer's, outside
// Publish) stays on origin, untracked. It blocks a new run's checkpoint on the branch (that run's
// publishes keep getting the not_descendant skip) until a human deletes it; nothing in uzi clears
// it later. The Warn names the branch and both tips so an operator can.
func (s *Service) exitStuckSupersessionLocked(ctx context.Context, row store.CheckpointRetention, fence func(context.Context) error) (bool, error) {
	runID := row.RunID
	branchRef := checkpointRefPrefix + row.Branch
	recoveryRef := pushbroker.RecoveryRefPrefix + runID.String()
	if !row.RecoveryRef.Valid || row.RecoveryRef.String != recoveryRef {
		return false, fmt.Errorf("superseding record names recovery ref %q, want %q", row.RecoveryRef.String, recoveryRef)
	}
	f, problem, gone := s.forgeForRetention(ctx, runID)
	if gone {
		bctx, cancel := retentionBookkeepingCtx(ctx)
		defer cancel()
		if _, err := s.q.SetCheckpointRetentionAbandoned(bctx, store.SetCheckpointRetentionAbandonedParams{
			RunID: runID, LastError: retentionGoneNote, ExpectedState: retentionSuperseding,
		}); err != nil {
			return false, fmt.Errorf("mark abandoned: %w", err)
		}
		return false, nil
	}
	if problem != "" {
		return false, s.recordRetentionFailure(ctx, row, retentionSuperseding, problem)
	}
	runTip, err := s.q.GetRunCheckpointTipForRetention(ctx, runID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("read run checkpoint tip: %w", err)
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
		ours := tip == row.Tip
		if !ours && step.ref == branchRef {
			if ours, err = s.ownPublishedTip(ctx, runID, tip); err != nil {
				return false, err
			}
		}
		if !ours {
			slog.Warn("checkpoint retention: stuck supersession exit leaves a ref at a tip that is not this run's; "+
				"a human must delete it (a new run's checkpoint on the branch stays skipped until then)", "run", runID,
				"branch", row.Branch, "ref", step.ref, "recorded_tip", row.Tip, "run_tip", runTip.String, "origin_tip", tip)
			continue
		}
		if err := s.beforeRetentionWrite(ctx, runID, step.op, fence); err != nil {
			return false, err
		}
		if derr := s.deleteCheckpointFn(ctx, pushbroker.DeleteOptions{
			CloneURL: f.cloneURL, Branch: row.Branch, Ref: step.ref, Username: f.username, PAT: f.pat, ExpectedOldTip: tip,
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
// instance closed the record. Under the run's lock it lists the recovery ref once:
//
//   - absent: verified;
//   - at the recorded tip while the run STILL has an open custody hold: the ref is the only copy
//     of the tip custody protects (the record may have closed as tip-gone with the hold open), so
//     it is never deleted here. The record is REOPENED as superseded naming it
//     (ReopenCheckpointRetentionSupersededIfHeld, the open-hold predicate in the same statement),
//     and ordinary settlement deletes it once the last hold settles;
//   - at the recorded tip with no open hold: the stray is ours; behind the fence, CAS-delete it,
//     then verified;
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
	reopened, err := s.q.ReopenCheckpointRetentionSupersededIfHeld(ctx, store.ReopenCheckpointRetentionSupersededIfHeldParams{
		RunID: runID, RecoveryRef: recoveryRef, Tip: row.Tip, LastError: auditReopenedNote,
	})
	if err != nil {
		return false, fmt.Errorf("reopen held record: %w", err)
	}
	if reopened > 0 {
		slog.Warn("checkpoint retention: audit found the recovery ref at the recorded tip while custody is open; record reopened as superseded",
			"run", runID, "ref", recoveryRef, "tip", row.Tip)
		return true, nil
	}
	// Zero rows: no hold of the run is open (the record's own guards held under this lock, and no
	// writer outside the lock touches a record that names a recovery ref).
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
// already-scrubbed msg. The write runs on retentionBookkeepingCtx, so it lands after the
// operation's own deadline too.
func (s *Service) deferRetentionVerify(ctx context.Context, row store.CheckpointRetention, msg string) error {
	ctx, cancel := retentionBookkeepingCtx(ctx)
	defer cancel()
	next := time.Now().Add(retentionBackoff(row.Attempts))
	if _, err := s.q.DeferCheckpointRetentionVerify(ctx, store.DeferCheckpointRetentionVerifyParams{
		RunID: row.RunID, LastError: msg, VerifyAfter: pgtype.Timestamptz{Time: next, Valid: true},
	}); err != nil {
		return fmt.Errorf("defer verify (%s): %w", msg, err)
	}
	slog.Warn("checkpoint retention: audit deferred", "run", row.RunID, "attempt", row.Attempts+1, "reason", msg)
	return nil
}
