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

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/secretscrub"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Terminal preservation sweep (issue #4007), the always-registered sweeper.Pass
// "terminal_preservation". It follows SweepHeldPublications (held_cleanup.go) with its own budget,
// scan cap and backoff, and touches only run_terminal_preservations and
// run_terminal_contributions, never run_held_publications.
//
// EFFECT-INERT WHEN EMPTY: the pass starts with one bounded, indexed due-row query
// (ListTerminalPreservationsDue, idx_run_terminal_preservations_due) and returns at once when it is
// empty. Nothing before that query resolves a credential, calls a forge or writes a row.
//
// Lanes, by the due row's state:
//
//   - publishing (reconcile): lists the ref and NEVER sends. Ref at the contribution's tip: published,
//     live (terminalRecordOutcome). Ref at the prior tip, or absent: publish_unknown with backoff. Ref
//     at any other tip: the contribution is refused foreign_tip and the preservation abandoned; the
//     ref is never touched. A listing error backs off and changes no state.
//   - bound (stale-bound): a bound row with no invoked contribution, after terminalBoundGrace, goes to
//     not_published under the run lock; a bound row whose run no longer exists (repo removal cascades
//     runs) has no run row to lock and goes to not_published under the preservation row lock alone.
//   - live and retiring (retire): PRODUCTION-DISABLED by terminalRetirementEnabled. Disabled, a due
//     live or retiring row is only pushed out (one scheduling write; no forge call, no lifetime read,
//     no delete). Enabled, the ordering is:
//     1. forge observation, outside any transaction: observed_at is read from the DATABASE clock
//     BEFORE the forge read (the earliest, conservative stamp), then the lifetime is re-derived with
//     terminalLifetime (the one classification the route also uses);
//     2. tx-R1: run lock (NOWAIT), then the preservation row; the facts and every independent
//     protection are re-checked; the observation must still be fresh on the database clock; then
//     the irrevocable retiring is committed. A reopen between the forge answer and this commit
//     is not seen: that is the documented behaviour, the decision is final once committed;
//     3. the remote effect, with no transaction open: list, CAS DeleteTerminal at retire_tip, list;
//     4. tx-R2: deleted (absent), abandoned (foreign tip), or stay retiring with backoff (uncertain),
//     in which case the next pass repeats only step 3.
//
// Lock order wherever more than one is taken: run, preservation, contribution. Every state or tip
// write is under the run row lock, with one exception: the run-gone bound row (terminalSweepBound
// via terminalLockPreservationOnly), whose run row no longer exists, is written under the
// preservation row lock alone. A scheduling-only write (terminalSkip, terminalFail, terminalPark) is
// a single statement and takes none.

// terminalRetirementEnabled gates the retire lane in production. It is a constant, not a setting:
// slice A ships the lane implemented and tested but off. Tests enable it with the unexported
// terminalHooks.retireEnabled.
const terminalRetirementEnabled = false

const (
	// terminalSweepPassBudget bounds one whole pass; terminalSweepItemTimeout one item (the smaller of
	// it and what remains of the pass).
	terminalSweepPassBudget  = 30 * time.Second
	terminalSweepItemTimeout = 30 * time.Second
	// terminalSweepMinItem is the least an item needs to be worth starting.
	terminalSweepMinItem      = 8 * time.Second
	terminalSweepLockTimeout  = "2s"
	terminalSweepScanLimit    = 10
	terminalSweepMaxItems     = 8
	terminalSweepSkipBackoff  = 5 * time.Minute
	terminalSweepBackoffBase  = time.Minute
	terminalSweepBackoffMax   = 6 * time.Hour
	terminalSweepWriteTimeout = 5 * time.Second

	// terminalReconcileGrace is how long an invoked contribution is left to its own request before the
	// sweep reconciles it (the route's request budget is far smaller).
	terminalReconcileGrace = 10 * time.Minute
	// terminalBoundGrace is how long a bound row waits before it is declared never published.
	terminalBoundGrace = time.Hour

	// terminalRetireRecheck is when a live row whose lifetime is not PROVEN closed is looked at again,
	// and what a due live or retiring row is pushed out by while retirement is disabled.
	terminalRetireRecheck = 24 * time.Hour
	// terminalRetireKeepRecheck is when a row kept by an independent protection is looked at again.
	terminalRetireKeepRecheck = time.Hour
	// retireObservationMaxAge is how old a forge observation may be, on the database clock, when the
	// retiring decision commits. The observation is stamped from the database clock read BEFORE the
	// forge read, so the bound covers the forge read plus the run-lock wait and is the effective
	// freshness bound. It is below terminalSweepItemTimeout, so a stalled item is re-observed.
	retireObservationMaxAge = 15 * time.Second
)

// terminalRetireMaxAge is retireObservationMaxAge, or the per-service test hook terminal.retireMaxAge
// when non-zero (tests only): a forge read is bounded by terminalLifetimeTimeout, below the real
// bound, so a stale-at-commit observation cannot otherwise be produced by a slow forge read.
func (s *Service) terminalRetireMaxAge() time.Duration {
	if s.terminal.retireMaxAge > 0 {
		return s.terminal.retireMaxAge
	}
	return retireObservationMaxAge
}

// The retire_reason vocabulary the sweep writes (a subset of the 00432 CHECK). no_pr never retires
// in slice A: a run with neither issue nor merge request has no closure signal.
const (
	terminalRetireIssueClosed = "issue_closed"
	terminalRetirePRClosed    = "pr_closed"
)

// terminalSweepQueries is the query surface the pass needs beyond Store, satisfied by *store.Queries.
type terminalSweepQueries interface {
	ListTerminalPreservationsDue(context.Context, int32) ([]store.RunTerminalPreservation, error)
	ListTerminalContributionsForRun(context.Context, uuid.UUID) ([]store.RunTerminalContribution, error)
	DeferTerminalPreservation(context.Context, store.DeferTerminalPreservationParams) (int64, error)
	GetTerminalSweepClock(context.Context) (pgtype.Timestamptz, error)
}

// terminalBackoff is the capped exponential delay after attempts failed or inconclusive attempts.
func terminalBackoff(attempts int32) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	if attempts > 12 {
		attempts = 12
	}
	d := terminalSweepBackoffBase << uint(attempts)
	if d > terminalSweepBackoffMax || d <= 0 {
		return terminalSweepBackoffMax
	}
	return d
}

func (s *Service) terminalRetireOn() bool {
	return terminalRetirementEnabled || s.terminal.retireEnabled
}

// SweepTerminalPreservations runs one pass and returns the number of state transitions it made
// (preservation or contribution; a retire that decides and then finishes counts 2, a
// contribution-only change counts 1). It is always registered; see the file comment for the empty-pass contract.
func (s *Service) SweepTerminalPreservations(ctx context.Context) (int64, error) {
	tq, ok := s.q.(terminalSweepQueries)
	if !ok {
		return 0, errors.New("terminal sweep: queries are not wired")
	}
	ctx, cancel := context.WithTimeout(ctx, terminalSweepPassBudget)
	defer cancel()
	rows, err := tq.ListTerminalPreservationsDue(ctx, terminalSweepScanLimit)
	if err != nil {
		return 0, fmt.Errorf("terminal sweep: list due: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	var touched int64
	var errs []error
	for i, row := range rows {
		if i >= terminalSweepMaxItems || ctx.Err() != nil {
			break // the rest waits for the next tick
		}
		n, err := s.terminalSweepOne(ctx, row)
		touched += n
		if err != nil {
			slog.Error("terminal sweep: item failed", "run", row.RunID, "state", row.State, "error", sanitizeHeldText(err.Error()))
			errs = append(errs, err)
		}
	}
	return touched, errors.Join(errs...)
}

// terminalSweepOne handles one row under its own deadline, recovering a panic (go-git has nil
// dereference paths on malformed forge responses) into a backed-off, error-stamped row.
func (s *Service) terminalSweepOne(ctx context.Context, row store.RunTerminalPreservation) (n int64, err error) {
	ictx, cancel := context.WithTimeout(ctx, terminalSweepItemTimeout)
	defer cancel()
	if dl, ok := ictx.Deadline(); ok && time.Until(dl) < terminalSweepMinItem {
		return 0, nil // too little of the pass left to finish a transaction safely
	}
	defer func() {
		if r := recover(); r != nil {
			msg := sanitizeHeldText(fmt.Sprintf("terminal sweep: panic: %v", r))
			slog.Error("terminal sweep: item panicked", "run", row.RunID, "panic", msg)
			n = 0
			err = s.terminalFail(ictx, row, msg)
		}
	}()
	n, err = s.terminalSweepLane(ictx, row)
	var nb *terminalNoBackoff
	if err != nil && !errors.As(err, &nb) {
		// A per-item error must never leave next_check_at unchanged: the row would stay at the head of
		// the due list and be retried every tick. Every lane has rolled its transaction back by now.
		if werr := s.terminalFail(ictx, row, heldErrText(err, "")); werr != nil {
			return n, errors.Join(err, werr)
		}
	}
	return n, err
}

// terminalNoBackoff marks an error the per-item backoff must not wrap: a failed backoff write itself
// or an injected crash that leaves the row exactly as it is.
type terminalNoBackoff struct{ error }

func (e *terminalNoBackoff) Unwrap() error { return e.error }

// errTerminalRunGone is terminalLockRun's result when the run row no longer exists.
var errTerminalRunGone = errors.New("terminal sweep: the run no longer exists")

func (s *Service) terminalSweepLane(ictx context.Context, row store.RunTerminalPreservation) (int64, error) {
	switch row.State {
	case terminalStateBound:
		return s.terminalSweepBound(ictx, row)
	case terminalStatePublishing:
		return s.terminalSweepReconcile(ictx, row)
	case "live":
		return s.terminalSweepLive(ictx, row)
	case "retiring":
		if !s.terminalRetireOn() {
			return 0, s.terminalPark(ictx, row, terminalRetireRecheck)
		}
		return s.terminalRetireEffect(ictx, row)
	}
	return 0, nil
}

// terminalDeferWrite is the scheduling-only write on a context detached from ctx's cancellation. It
// changes no state and no tip, so it takes no run lock.
func (s *Service) terminalDeferWrite(ctx context.Context, row store.RunTerminalPreservation, delay time.Duration, msg string, bump int32) error {
	tq, ok := s.q.(terminalSweepQueries)
	if !ok {
		return errors.New("terminal sweep: queries are not wired")
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminalSweepWriteTimeout)
	defer cancel()
	_, err := tq.DeferTerminalPreservation(wctx, store.DeferTerminalPreservationParams{
		NextCheckAt:   pgtype.Timestamptz{Time: s.now().Add(delay), Valid: true},
		LastError:     pgtype.Text{String: msg, Valid: msg != ""},
		AttemptBump:   bump,
		ID:            row.ID,
		ExpectedState: row.State,
	})
	if err != nil {
		return &terminalNoBackoff{fmt.Errorf("terminal sweep: defer: %s", secretscrub.Scrub(err.Error()))}
	}
	return nil
}

// terminalSkip defers a busy or not-yet-due row without counting an attempt.
func (s *Service) terminalSkip(ctx context.Context, row store.RunTerminalPreservation) error {
	return s.terminalDeferWrite(ctx, row, terminalSweepSkipBackoff, "", 0)
}

// terminalPark pushes a row out by delay without counting an attempt.
func (s *Service) terminalPark(ctx context.Context, row store.RunTerminalPreservation, delay time.Duration) error {
	return s.terminalDeferWrite(ctx, row, delay, "", 0)
}

// terminalFail backs a row off after a failed or inconclusive attempt and counts it.
func (s *Service) terminalFail(ctx context.Context, row store.RunTerminalPreservation, msg string) error {
	return s.terminalDeferWrite(ctx, row, terminalBackoff(row.Attempts), msg, 1)
}

// terminalSweepTx is one open transaction of the sweep, rolled back unless committed.
type terminalSweepTx struct {
	ctx  context.Context
	tx   pgx.Tx
	qt   *store.Queries
	done bool
}

func (t *terminalSweepTx) rollback() {
	if !t.done {
		t.done = true
		rctx, c := context.WithTimeout(context.WithoutCancel(t.ctx), terminalSweepWriteTimeout)
		defer c()
		_ = t.tx.Rollback(rctx)
	}
}

func (t *terminalSweepTx) commit() error {
	t.done = true // a failed commit ends the transaction as well
	return t.tx.Commit(t.ctx)
}

// terminalLockRun begins a transaction and takes the locks in order: the run (FOR NO KEY UPDATE
// NOWAIT, so a busy run is skipped and never queued behind), the lock timeout, then the preservation
// row FOR UPDATE. busy is true when the run (or the row) is held by someone else; the transaction is
// then already rolled back. The run's repository must still be the row's.
func (s *Service) terminalLockRun(ctx context.Context, row store.RunTerminalPreservation) (tt *terminalSweepTx, pres store.RunTerminalPreservation, busy bool, err error) {
	if s.txBeginner == nil {
		return nil, pres, false, errors.New("terminal sweep: transactions are not wired")
	}
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return nil, pres, false, fmt.Errorf("terminal sweep: begin: %s", secretscrub.Scrub(err.Error()))
	}
	tt = &terminalSweepTx{ctx: ctx, tx: tx, qt: store.New(tx)}
	fail := func(e error) (*terminalSweepTx, store.RunTerminalPreservation, bool, error) {
		tt.rollback()
		if isLockNotAvailable(e) {
			return nil, pres, true, nil
		}
		return nil, pres, false, e
	}
	repo, err := tt.qt.LockRunForHeldCleanup(ctx, row.RunID)
	if errors.Is(err, pgx.ErrNoRows) {
		tt.rollback()
		return nil, pres, false, errTerminalRunGone
	}
	if err != nil {
		return fail(fmt.Errorf("terminal sweep: run lock: %w", err))
	}
	if !repo.Valid || repo.Bytes != [16]byte(row.RepoID) {
		tt.rollback()
		return nil, pres, false, errors.New("terminal sweep: the run's repository no longer matches the preservation")
	}
	if _, err = tx.Exec(ctx, "SET LOCAL lock_timeout = '"+terminalSweepLockTimeout+"'"); err != nil {
		return fail(fmt.Errorf("terminal sweep: lock timeout: %w", err))
	}
	pres, err = tt.qt.GetTerminalPreservationForUpdate(ctx, row.RunID)
	if err != nil {
		return fail(fmt.Errorf("terminal sweep: preservation lock: %w", err))
	}
	return tt, pres, false, nil
}

// terminalLockPreservationOnly is terminalLockRun for a run that no longer exists: there is no run row
// to lock, so only the preservation row is (FOR UPDATE, under the lock timeout).
func (s *Service) terminalLockPreservationOnly(ctx context.Context, row store.RunTerminalPreservation) (tt *terminalSweepTx, pres store.RunTerminalPreservation, busy bool, err error) {
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return nil, pres, false, fmt.Errorf("terminal sweep: begin: %s", secretscrub.Scrub(err.Error()))
	}
	tt = &terminalSweepTx{ctx: ctx, tx: tx, qt: store.New(tx)}
	if _, err = tx.Exec(ctx, "SET LOCAL lock_timeout = '"+terminalSweepLockTimeout+"'"); err == nil {
		pres, err = tt.qt.GetTerminalPreservationForUpdate(ctx, row.RunID)
	}
	if err != nil {
		tt.rollback()
		if isLockNotAvailable(err) {
			return nil, pres, true, nil
		}
		return nil, pres, false, fmt.Errorf("terminal sweep: preservation lock: %w", err)
	}
	return tt, pres, false, nil
}

// terminalSweepBound is the stale-bound lane: a bound row with no invoked contribution, after the
// grace, is a run that never had a ref. Under the run lock it goes to not_published; when the run row
// is gone it goes there under the preservation row lock alone (no run row exists to lock).
func (s *Service) terminalSweepBound(ctx context.Context, row store.RunTerminalPreservation) (int64, error) {
	if due := row.CreatedAt.Time.Add(terminalBoundGrace); s.now().Before(due) {
		return 0, s.terminalPark(ctx, row, due.Sub(s.now()))
	}
	tt, pres, busy, err := s.terminalLockRun(ctx, row)
	if errors.Is(err, errTerminalRunGone) {
		// Repo removal cascades runs but a bound row holds no live pointer, so it survives. No ref was
		// ever created and no run row exists to lock: the transition needs only the preservation lock.
		tt, pres, busy, err = s.terminalLockPreservationOnly(ctx, row)
	}
	if err != nil {
		return 0, err
	}
	if busy {
		return 0, s.terminalSkip(ctx, row)
	}
	defer tt.rollback()
	if pres.State != terminalStateBound || pres.Tip.Valid {
		return 0, nil // another writer moved the row on
	}
	cons, err := tt.qt.ListTerminalContributionsForRun(ctx, row.RunID)
	if err != nil {
		return 0, fmt.Errorf("terminal sweep: contributions: %w", err)
	}
	for _, c := range cons {
		if c.State == terminalContributionInvoked || c.State == terminalContributionUnknown || c.State == "published" {
			tt.rollback()
			return 0, s.terminalFail(ctx, row, "bound with a contribution that may have created the ref")
		}
	}
	if _, err = tt.qt.MarkTerminalPreservationNotPublished(ctx, pres.ID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, terminalWriteMiss(ctx, tt.qt, "sweep mark not_published", row.RunID, 0, false, terminalStateBound)
		}
		return 0, fmt.Errorf("terminal sweep: not_published: %w", err)
	}
	if err = tt.commit(); err != nil {
		return 0, fmt.Errorf("terminal sweep: commit: %s", secretscrub.Scrub(err.Error()))
	}
	return 1, nil
}

// terminalSweepReconcile is the reconcile lane for a publishing row. It never sends and never
// deletes.
func (s *Service) terminalSweepReconcile(ctx context.Context, row store.RunTerminalPreservation) (int64, error) {
	tq, ok := s.q.(terminalSweepQueries)
	if !ok {
		return 0, errors.New("terminal sweep: queries are not wired")
	}
	cons, err := tq.ListTerminalContributionsForRun(ctx, row.RunID)
	if err != nil {
		return 0, fmt.Errorf("terminal sweep: contributions: %w", err)
	}
	var con *store.RunTerminalContribution
	for i := range cons {
		if cons[i].State == terminalContributionInvoked || cons[i].State == terminalContributionUnknown {
			con = &cons[i]
			break
		}
	}
	if con == nil {
		return 0, s.terminalFail(ctx, row, "publishing without an unresolved contribution")
	}
	if con.State == terminalContributionInvoked && con.CreateInvokedAt.Valid {
		if due := con.CreateInvokedAt.Time.Add(terminalReconcileGrace); s.now().Before(due) {
			return 0, s.terminalPark(ctx, row, due.Sub(s.now())) // still the route's own request
		}
	}
	f, err := s.heldRemote(ctx, row.RunID)
	if err != nil {
		msg := "forge connection is gone"
		if !errors.Is(err, ErrHeldRemoteGone) {
			msg = heldErrText(err, "")
		}
		slog.Warn("terminal sweep: forge connection unavailable", "run", row.RunID, "error", msg)
		return 0, s.terminalFail(ctx, row, msg)
	}
	lctx, cancel := context.WithTimeout(ctx, terminalListTimeout)
	defer cancel()
	tip, present, err := s.terminal.listFn()(lctx, f.ListOptions(), row.RunID)
	if err != nil {
		msg := heldErrText(err, f.PAT)
		slog.Warn("terminal sweep: reconcile listing failed", "run", row.RunID, "error", msg)
		return 0, s.terminalFail(ctx, row, msg)
	}
	switch {
	case present && tip == con.Tip:
		_, con2, err := s.terminalRecordOutcome(ctx, row.RunID, con.Generation, terminalOutcomeRecord{outcome: pushbroker.TerminalApplied})
		if err != nil {
			return 0, s.terminalFail(ctx, row, heldErrText(fmt.Errorf("record reconciled publication: %w", err), f.PAT))
		}
		return boolToInt(con2.State == "published"), nil
	case present && (!con.PriorTip.Valid || tip != con.PriorTip.String):
		return s.terminalSweepForeign(ctx, row, con.Generation)
	case present:
		return s.terminalSweepUnknown(ctx, row, con.Generation, "ref still at the prior tip")
	default:
		return s.terminalSweepUnknown(ctx, row, con.Generation, "ref not present yet")
	}
}

// terminalSweepUnknown records an unproven outcome: an invoked contribution becomes publish_unknown
// (an already-unknown one stays), and the preservation, which stays publishing, is backed off with
// the attempt counted. Under the run lock; never a second send.
func (s *Service) terminalSweepUnknown(ctx context.Context, row store.RunTerminalPreservation, generation int64, msg string) (int64, error) {
	tt, pres, busy, err := s.terminalLockRun(ctx, row)
	if err != nil {
		return 0, err
	}
	if busy {
		return 0, s.terminalSkip(ctx, row)
	}
	defer tt.rollback()
	if pres.State != terminalStatePublishing {
		return 0, nil
	}
	cur, err := tt.qt.GetTerminalContributionByRunGeneration(ctx, store.GetTerminalContributionByRunGenerationParams{RunID: row.RunID, Generation: generation})
	if err != nil {
		return 0, fmt.Errorf("terminal sweep: contribution: %w", err)
	}
	var n int64
	switch cur.State {
	case terminalContributionInvoked:
		if _, err = tt.qt.RecordTerminalContributionUnknown(ctx, cur.ID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return 0, nil // decided by the route meanwhile
			}
			return 0, fmt.Errorf("terminal sweep: unknown: %w", err)
		}
		n = 1
	case terminalContributionUnknown:
	default:
		return 0, nil // decided meanwhile
	}
	rows, err := tt.qt.DeferTerminalPreservation(ctx, store.DeferTerminalPreservationParams{
		NextCheckAt: pgtype.Timestamptz{Time: s.now().Add(terminalBackoff(pres.Attempts)), Valid: true},
		LastError:   pgtype.Text{String: msg, Valid: msg != ""}, AttemptBump: 1, ID: pres.ID, ExpectedState: terminalStatePublishing,
	})
	if err != nil {
		return 0, fmt.Errorf("terminal sweep: backoff: %w", err)
	}
	if rows == 0 {
		return 0, terminalWriteMiss(ctx, tt.qt, "sweep backoff", row.RunID, generation, false, terminalStatePublishing)
	}
	if err = tt.commit(); err != nil {
		return 0, fmt.Errorf("terminal sweep: commit: %s", secretscrub.Scrub(err.Error()))
	}
	return n, nil
}

// terminalSweepForeign handles a ref at a tip that is neither the contribution's nor its prior tip:
// the contribution is refused foreign_tip and the preservation abandoned. The ref is never touched.
func (s *Service) terminalSweepForeign(ctx context.Context, row store.RunTerminalPreservation, generation int64) (int64, error) {
	tt, pres, busy, err := s.terminalLockRun(ctx, row)
	if err != nil {
		return 0, err
	}
	if busy {
		return 0, s.terminalSkip(ctx, row)
	}
	defer tt.rollback()
	if pres.State != terminalStatePublishing {
		return 0, nil
	}
	cur, err := tt.qt.GetTerminalContributionByRunGeneration(ctx, store.GetTerminalContributionByRunGenerationParams{RunID: row.RunID, Generation: generation})
	if err != nil {
		return 0, fmt.Errorf("terminal sweep: contribution: %w", err)
	}
	if cur.State != terminalContributionInvoked && cur.State != terminalContributionUnknown {
		return 0, nil // decided meanwhile
	}
	if _, err = tt.qt.RecordTerminalContributionRefused(ctx, store.RecordTerminalContributionRefusedParams{
		ID: cur.ID, ExpectedState: cur.State, RefusalReason: pgtype.Text{String: apitypes.TerminalReasonForeignTip, Valid: true},
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, terminalWriteMiss(ctx, tt.qt, "sweep record refused", row.RunID, generation, true, cur.State)
		}
		return 0, fmt.Errorf("terminal sweep: refused: %w", err)
	}
	if _, err = tt.qt.MarkTerminalPreservationAbandoned(ctx, store.MarkTerminalPreservationAbandonedParams{
		ID: pres.ID, ExpectedState: terminalStatePublishing, LastError: pgtype.Text{String: "ref is at a foreign tip", Valid: true},
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, terminalWriteMiss(ctx, tt.qt, "sweep abandon", row.RunID, generation, false, terminalStatePublishing)
		}
		return 0, fmt.Errorf("terminal sweep: abandon: %w", err)
	}
	if err = tt.commit(); err != nil {
		return 0, fmt.Errorf("terminal sweep: commit: %s", secretscrub.Scrub(err.Error()))
	}
	return 1, nil
}

// terminalSweepLive is retire steps 1-2 for a live row: the forge observation, then the decision.
func (s *Service) terminalSweepLive(ctx context.Context, row store.RunTerminalPreservation) (int64, error) {
	if !s.terminalRetireOn() {
		return 0, s.terminalPark(ctx, row, terminalRetireRecheck)
	}
	tq, ok := s.q.(terminalSweepQueries)
	if !ok {
		return 0, errors.New("terminal sweep: queries are not wired")
	}
	run, err := s.q.GetRunByIDForUser(ctx, store.GetRunByIDForUserParams{ID: row.RunID, UserID: row.UserID})
	if err != nil {
		return 0, s.terminalFail(ctx, row, heldErrText(fmt.Errorf("run: %w", err), ""))
	}
	// 1. Forge observation, outside any transaction. The lifetime is re-derived at every observation,
	// so a run that gained a merge request is classified by it.
	// The clock is read BEFORE the forge read, so observedAt is the conservative (earliest) stamp.
	clock, err := tq.GetTerminalSweepClock(ctx)
	if err != nil {
		return 0, s.terminalFail(ctx, row, heldErrText(fmt.Errorf("clock: %w", err), ""))
	}
	lt := s.terminalLifetime(ctx, run)
	if !lt.ClosedProven {
		// Not proven closed (open, unknown, a forge error, or a run with no closure signal): keep.
		return 0, s.terminalPark(ctx, row, terminalRetireRecheck)
	}
	reason := terminalRetirePRClosed
	if lt.Class == terminalLifetimeIssue {
		reason = terminalRetireIssueClosed
	}
	observedAt := clock.Time
	if s.terminal.afterObserve != nil {
		s.terminal.afterObserve(&observedAt)
	}
	return s.terminalRetireDecide(ctx, row, run, reason, observedAt)
}

// terminalRetireDecide is tx-R1, then (when the decision commits) the remote effect.
func (s *Service) terminalRetireDecide(ctx context.Context, row store.RunTerminalPreservation, run store.Run, reason string, observedAt time.Time) (int64, error) {
	tt, pres, busy, err := s.terminalLockRun(ctx, row)
	if err != nil {
		return 0, err
	}
	if busy {
		return 0, s.terminalSkip(ctx, row)
	}
	defer tt.rollback()
	if pres.State != "live" || !pres.Tip.Valid || pres.Tip != row.Tip {
		return 0, nil // the row moved on (or its tip did): a later observation decides
	}
	blocker, err := tt.qt.GetTerminalRetireBlocker(ctx, row.RunID)
	if errors.Is(err, pgx.ErrNoRows) {
		blocker, err = "run_missing", nil
	}
	if err != nil {
		return 0, fmt.Errorf("terminal sweep: protections: %w", err)
	}
	if blocker == "" && run.IssueIid.Valid && run.RepoID.Valid {
		consumed, cerr := tt.qt.HasCrossRunLiveConsumer(ctx, store.HasCrossRunLiveConsumerParams{
			OwnerID: run.UserID, RepoID: run.RepoID, IssueIid: run.IssueIid,
		})
		if cerr != nil {
			return 0, fmt.Errorf("terminal sweep: consumer: %w", cerr)
		}
		if consumed {
			blocker = "cross_run_consumer"
		}
	}
	if blocker != "" {
		if err = terminalSchedule(ctx, tt.qt, store.ScheduleTerminalPreservationCheckParams{
			ID: pres.ID, NextCheckAt: pgtype.Timestamptz{Time: s.now().Add(terminalRetireKeepRecheck), Valid: true},
			LastError: pgtype.Text{String: "retire kept: " + blocker, Valid: true},
		}); err != nil {
			return 0, err
		}
		if err = tt.commit(); err != nil {
			return 0, fmt.Errorf("terminal sweep: commit: %s", secretscrub.Scrub(err.Error()))
		}
		return 0, nil
	}
	retiring, err := tt.qt.MarkTerminalPreservationRetiring(ctx, store.MarkTerminalPreservationRetiringParams{
		ID: pres.ID, Tip: pres.Tip.String, RetireReason: reason,
		ObservedAt: pgtype.Timestamptz{Time: observedAt, Valid: true}, MaxAgeSeconds: s.terminalRetireMaxAge().Seconds(),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// A stale observation (or a guard refusal): the decision is discarded without a state write and
		// the row is backed off (scheduling only, no attempt counted) so it is not re-observed every tick.
		slog.Warn("terminal sweep: retire decision discarded", "run", row.RunID)
		tt.rollback() // release the row lock before the scheduling write
		return 0, s.terminalSkip(ctx, row)
	}
	if err != nil {
		return 0, fmt.Errorf("terminal sweep: retiring: %w", err)
	}
	if err = tt.commit(); err != nil {
		return 0, fmt.Errorf("terminal sweep: commit: %s", secretscrub.Scrub(err.Error()))
	}
	// The decision is committed and irrevocable. 3. and 4. follow with no transaction open.
	n, err := s.terminalRetireEffect(ctx, retiring)
	var nb *terminalNoBackoff
	if err != nil && !errors.As(err, &nb) {
		// The row is retiring now, but terminalSweepOne holds the live row it was handed, and
		// DeferTerminalPreservation matches on the expected state: backing off with that row would
		// match nothing and the retiring row (due now) would loop every tick. Back off with the
		// retiring row, then tell the caller it need not.
		if werr := s.terminalFail(ctx, retiring, heldErrText(err, "")); werr != nil {
			return 1 + n, errors.Join(err, werr)
		}
		return 1 + n, &terminalNoBackoff{err}
	}
	return 1 + n, err
}

// terminalRetireEffect is retire steps 3-4 for a retiring row: list, CAS-delete at retire_tip (which
// lists again to confirm), then record the outcome in tx-R2. An uncertain outcome leaves the row
// retiring with backoff; the next pass repeats only this step. The decision is never re-observed.
func (s *Service) terminalRetireEffect(ctx context.Context, row store.RunTerminalPreservation) (int64, error) {
	if !row.RetireTip.Valid {
		return 0, s.terminalFail(ctx, row, "retiring without a retire tip")
	}
	f, err := s.heldRemote(ctx, row.RunID)
	if err != nil {
		msg := "forge connection is gone"
		if !errors.Is(err, ErrHeldRemoteGone) {
			msg = heldErrText(err, "")
		}
		return 0, s.terminalFail(ctx, row, msg)
	}
	lctx, cancel := context.WithTimeout(ctx, terminalListTimeout)
	tip, present, err := s.terminal.listFn()(lctx, f.ListOptions(), row.RunID)
	cancel()
	if err != nil {
		return 0, s.terminalFail(ctx, row, heldErrText(err, f.PAT))
	}
	final, msg := "", ""
	switch {
	case !present:
		final = "deleted"
	case tip != row.RetireTip.String:
		final, msg = "abandoned", "ref is at a foreign tip"
	default:
		out, derr := s.terminal.deleteFn()(ctx, pushbroker.DeleteTerminalOptions{
			CloneURL: f.CloneURL, Username: f.Username, PAT: f.PAT, RunID: row.RunID, ExpectedTip: row.RetireTip.String,
		})
		if s.terminal.afterDelete != nil {
			if herr := s.terminal.afterDelete(); herr != nil {
				return 0, &terminalNoBackoff{herr} // a crash after the remote effect: the row stays retiring
			}
		}
		switch out {
		case pushbroker.TerminalDeleteGone:
			final = "deleted"
		case pushbroker.TerminalDeleteMoved:
			final, msg = "abandoned", "ref is at a foreign tip"
		default:
			text := "ref still at the retire tip after the delete"
			if derr != nil {
				text = heldErrText(derr, f.PAT)
			}
			return 0, s.terminalFail(ctx, row, text)
		}
	}
	return s.terminalRetireFinish(ctx, row, final, msg)
}

// terminalRetireFinish is tx-R2, on a context detached from ctx's cancellation: a delete the item
// outlived is still recorded. A busy run leaves the row retiring and due; step 3 then repeats.
func (s *Service) terminalRetireFinish(ctx context.Context, row store.RunTerminalPreservation, final, msg string) (int64, error) {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminalSweepWriteTimeout)
	defer cancel()
	tt, pres, busy, err := s.terminalLockRun(wctx, row)
	if err != nil {
		return 0, err
	}
	if busy {
		return 0, s.terminalSkip(wctx, row)
	}
	defer tt.rollback()
	if pres.State != "retiring" {
		return 0, nil
	}
	switch final {
	case "deleted":
		_, err = tt.qt.MarkTerminalPreservationDeleted(wctx, pres.ID)
	default:
		_, err = tt.qt.MarkTerminalPreservationAbandoned(wctx, store.MarkTerminalPreservationAbandonedParams{
			ID: pres.ID, ExpectedState: "retiring", LastError: pgtype.Text{String: msg, Valid: msg != ""},
		})
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, terminalWriteMiss(wctx, tt.qt, "retire "+final, row.RunID, 0, false, "retiring")
	}
	if err != nil {
		return 0, fmt.Errorf("terminal sweep: %s: %w", final, err)
	}
	if err = tt.commit(); err != nil {
		return 0, fmt.Errorf("terminal sweep: commit: %s", secretscrub.Scrub(err.Error()))
	}
	return 1, nil
}
