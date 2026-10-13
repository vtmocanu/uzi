package workersvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/secretscrub"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Held-work publication cleanup (issue #2545). SweepHeldPublications is a sweeper.Pass with
// three lanes over run_held_publications:
//
//   - reconcile: invoked past its grace, create_unknown and delete_unknown rows. It only LISTS
//     the ref and records an outcome, so it is allowed while a hold is open. A ref at the tip
//     settles a create as created; an absent ref leaves a create unknown (capped exponential
//     backoff, never terminal, never a second create). For a delete_unknown row an absent ref is
//     deleted and a foreign tip is abandoned; a ref still at the tip goes to the deletion
//     protocol below.
//   - expiry: acknowledged rows with expires_at <= now (an owner expiry only moves expires_at).
//   - orphan: created rows that were never acknowledged, whose run has no open hold.
//
// The deletion protocol (expiry, orphan, a delete retry) is the only path that deletes a held ref
// and it NEVER runs while any hold of the run is open, whatever its generation:
//
//  1. the forge connection is derived BEFORE the transaction (heldRemote, SSRF gate first);
//  2. one transaction on its own connection takes the run row FOR NO KEY UPDATE NOWAIT
//     (LockRunForHeldCleanup: it conflicts with ClaimRun, the only place a hold is inserted, and
//     with step B, but not with a heartbeat's FK insert), then lock_timeout 2s, then the
//     publication; lock order is worker, run, hold, publication;
//  3. the lane predicate, the repo identity and "no open hold" are re-validated under the locks;
//  4. the ref is listed (absent: deleted; foreign tip: abandoned, never touched), else deleted
//     with a CAS on the recorded tip and listed again to confirm;
//  5. the outcome is recorded and committed on the SAME transaction, in the final
//     heldCleanupTxReserve of the item deadline (forge calls stop that long before it).
//
// A transaction that dies after a send leaves the row in its prior state, and the next listing
// settles it. A skipped item (busy run, open hold) gets a scheduling-only write in its own short
// transaction (DeferHeldPublication), and the expiry and orphan queries filter on that
// next_attempt_at, so a skipped row never head-of-line blocks the rows behind it.
const (
	// heldCleanupPassBudget bounds one whole pass; heldCleanupItemTimeout bounds one item (the
	// smaller of it and what remains of the pass).
	heldCleanupPassBudget  = 30 * time.Second
	heldCleanupItemTimeout = 30 * time.Second
	// heldCleanupTxReserve is the tail of an item's deadline reserved for recording the outcome and
	// committing; forge calls run on a sub-context that ends that long earlier.
	heldCleanupTxReserve = 5 * time.Second
	// heldCleanupMinItem is the least an item needs to be worth starting.
	heldCleanupMinItem      = heldCleanupTxReserve + 3*time.Second
	heldCleanupLockTimeout  = "2s"
	heldCleanupScanLimit    = 10
	heldCleanupMaxItems     = 8
	heldCleanupSkipBackoff  = 5 * time.Minute
	heldCleanupBackoffBase  = time.Minute
	heldCleanupBackoffMax   = 6 * time.Hour
	heldCleanupWriteTimeout = 5 * time.Second
	// heldReconcileGrace is how long an invoked row with no schedule is left to its own request
	// before the sweeper reconciles it (the step-A request budget is 150s).
	heldReconcileGrace = 10 * time.Minute
)

type heldLane int

const (
	heldLaneReconcile heldLane = iota
	heldLaneExpiry
	heldLaneOrphan
	heldLaneDeleteRetry
)

type heldCleanupItem struct {
	row  store.RunHeldPublication
	lane heldLane
}

// heldCleanupQueries is the query surface the pass needs beyond Store, satisfied by *store.Queries.
type heldCleanupQueries interface {
	ListHeldPublicationsDueReconcile(context.Context, store.ListHeldPublicationsDueReconcileParams) ([]store.RunHeldPublication, error)
	ListHeldPublicationsDueExpiry(context.Context, store.ListHeldPublicationsDueExpiryParams) ([]store.RunHeldPublication, error)
	ListHeldPublicationsDueOrphan(context.Context, store.ListHeldPublicationsDueOrphanParams) ([]store.RunHeldPublication, error)
	DeferHeldPublication(context.Context, store.DeferHeldPublicationParams) (int64, error)
	RecordHeldDeleteOutcome(context.Context, store.RecordHeldDeleteOutcomeParams) (int64, error)
}

// heldBackoff is the capped exponential delay after attempts failed or inconclusive attempts.
func heldBackoff(attempts int32) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	if attempts > 12 {
		attempts = 12
	}
	d := heldCleanupBackoffBase << uint(attempts)
	if d > heldCleanupBackoffMax || d <= 0 {
		return heldCleanupBackoffMax
	}
	return d
}

// SweepHeldPublications runs one cleanup pass and returns the number of publication rows whose
// state it changed. It is always registered: UZI_HELD_PUBLICATION gates only new step A and B work,
// never cleanup.
func (s *Service) SweepHeldPublications(ctx context.Context) (int64, error) {
	hq, ok := s.q.(heldCleanupQueries)
	if !ok {
		return 0, errors.New("held cleanup: queries are not wired")
	}
	ctx, cancel := context.WithTimeout(ctx, heldCleanupPassBudget)
	defer cancel()
	now := s.now()
	ts := pgtype.Timestamptz{Time: now, Valid: true}

	reconcile, err := hq.ListHeldPublicationsDueReconcile(ctx, store.ListHeldPublicationsDueReconcileParams{
		GraceSeconds: heldReconcileGrace.Seconds(), Now: ts, Lim: heldCleanupScanLimit,
	})
	if err != nil {
		return 0, fmt.Errorf("held cleanup: list reconcile: %w", err)
	}
	expiry, err := hq.ListHeldPublicationsDueExpiry(ctx, store.ListHeldPublicationsDueExpiryParams{Now: ts, Lim: heldCleanupScanLimit})
	if err != nil {
		return 0, fmt.Errorf("held cleanup: list expiry: %w", err)
	}
	orphan, err := hq.ListHeldPublicationsDueOrphan(ctx, store.ListHeldPublicationsDueOrphanParams{Now: ts, Lim: heldCleanupScanLimit})
	if err != nil {
		return 0, fmt.Errorf("held cleanup: list orphan: %w", err)
	}

	var touched int64
	var errs []error
	for _, it := range roundRobinHeld(heldCleanupMaxItems, reconcile, expiry, orphan) {
		if ctx.Err() != nil {
			break // budget spent: the rest waits for the next tick
		}
		n, err := s.heldCleanupOne(ctx, it)
		touched += n
		if err != nil {
			slog.Error("held cleanup: item failed", "run", it.row.RunID, "error", sanitizeHeldText(err.Error()))
			errs = append(errs, err)
		}
	}
	return touched, errors.Join(errs...)
}

// roundRobinHeld interleaves the three lanes' rows, at most limit, skipping a row already taken.
func roundRobinHeld(limit int, reconcile, expiry, orphan []store.RunHeldPublication) []heldCleanupItem {
	lanes := [][]store.RunHeldPublication{reconcile, expiry, orphan}
	laneOf := []heldLane{heldLaneReconcile, heldLaneExpiry, heldLaneOrphan}
	seen := map[[16]byte]bool{}
	out := make([]heldCleanupItem, 0, limit)
	for i := 0; len(out) < limit; i++ {
		more := false
		for l, rows := range lanes {
			if i >= len(rows) {
				continue
			}
			more = true
			if seen[rows[i].ID] || len(out) >= limit {
				continue
			}
			seen[rows[i].ID] = true
			out = append(out, heldCleanupItem{row: rows[i], lane: laneOf[l]})
		}
		if !more {
			break
		}
	}
	return out
}

// heldCleanupOne handles one item under its own deadline, recovering a panic (go-git has nil
// dereference paths on malformed forge responses) into a deferred, error-stamped row.
func (s *Service) heldCleanupOne(ctx context.Context, it heldCleanupItem) (n int64, err error) {
	ictx, cancel := context.WithTimeout(ctx, heldCleanupItemTimeout)
	defer cancel()
	if dl, ok := ictx.Deadline(); ok && time.Until(dl) < heldCleanupMinItem {
		return 0, nil // too little of the pass left to finish a transaction safely
	}
	defer func() {
		if r := recover(); r != nil {
			msg := sanitizeHeldText(fmt.Sprintf("held cleanup: panic: %v", r))
			slog.Error("held cleanup: item panicked", "run", it.row.RunID, "panic", msg)
			n = 0
			err = s.heldDefer(ictx, it.row, msg, 1)
		}
	}()
	switch it.lane {
	case heldLaneReconcile:
		return s.heldReconcileItem(ictx, it.row)
	default:
		return s.heldDeleteItem(ictx, it.row, it.lane)
	}
}

// heldErrText makes err safe to store and log: heldRedact deletes unsafe characters, replaces
// the connection PAT (also when an invisible or control rune split it), scrubs recognised secret families and
// bounds the length.
func heldErrText(err error, pat string) string { return heldRedact(err.Error(), pat) }

// heldDefer is the scheduling-only write of a skipped or failed item: next_attempt_at moves out
// and, when msg is not empty, last_error is recorded. It runs on its own short transaction (a
// single statement) detached from ctx's cancellation. attemptBump is 0 for a plain skip.
func (s *Service) heldDefer(ctx context.Context, row store.RunHeldPublication, msg string, attemptBump int32) error {
	hq, ok := s.q.(heldCleanupQueries)
	if !ok {
		return errors.New("held cleanup: queries are not wired")
	}
	delay := heldCleanupSkipBackoff
	if attemptBump > 0 {
		delay = heldBackoff(row.Attempts)
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), heldCleanupWriteTimeout)
	defer cancel()
	_, err := hq.DeferHeldPublication(wctx, store.DeferHeldPublicationParams{
		NextAttemptAt: pgtype.Timestamptz{Time: s.now().Add(delay), Valid: true},
		LastError:     pgtype.Text{String: msg, Valid: msg != ""},
		AttemptBump:   attemptBump,
		ID:            row.ID,
		ExpectedState: row.State,
	})
	if err != nil {
		return fmt.Errorf("held cleanup: defer: %s", secretscrub.Scrub(err.Error()))
	}
	return nil
}

// heldRecordDelete records a delete outcome on q (a transaction's queries, or the pool's) for a
// row expected in fromState. It returns whether the CAS matched a row.
func heldRecordDelete(ctx context.Context, q interface {
	RecordHeldDeleteOutcome(context.Context, store.RecordHeldDeleteOutcomeParams) (int64, error)
}, row store.RunHeldPublication, state, lastErr string, next time.Time) (bool, error) {
	n, err := q.RecordHeldDeleteOutcome(ctx, store.RecordHeldDeleteOutcomeParams{
		State:         state,
		LastError:     pgtype.Text{String: lastErr, Valid: lastErr != ""},
		NextAttemptAt: pgtype.Timestamptz{Time: next, Valid: !next.IsZero()},
		ID:            row.ID,
		FromStates:    []string{row.State},
	})
	return n > 0, err
}

// heldReconcileItem is the reconcile lane for one row. It never deletes and never creates.
func (s *Service) heldReconcileItem(ctx context.Context, row store.RunHeldPublication) (int64, error) {
	f, err := s.heldRemote(ctx, row.RunID)
	if err != nil {
		return 0, s.heldReconcileFailed(ctx, row, err, "")
	}
	lctx, cancel := context.WithTimeout(ctx, heldListTimeout)
	defer cancel()
	tips, err := s.heldListFn(lctx, f.ListOptions(), row.Ref)
	if err != nil {
		return 0, s.heldReconcileFailed(ctx, row, err, f.PAT)
	}
	tip, present := tips[row.Ref]
	switch row.State {
	case "invoked", "create_unknown":
		if present && tip == row.Tip {
			rec, err := s.heldRecordOutcome(ctx, row.ID, "created", "", "", time.Time{})
			if err != nil {
				return 0, fmt.Errorf("held cleanup: record reconciled create: %s", secretscrub.Scrub(err.Error()))
			}
			return boolToInt(rec.State == "created" && row.State != "created"), nil
		}
		// Absent (or not yet at the tip) is never terminal: stay unknown, check again later.
		_, err := s.heldRecordOutcome(ctx, row.ID, "create_unknown", "", "ref not present at the recorded tip yet",
			s.now().Add(heldBackoff(row.Attempts)))
		if err != nil {
			return 0, fmt.Errorf("held cleanup: record unknown create: %s", secretscrub.Scrub(err.Error()))
		}
		return 0, nil
	case "delete_unknown":
		state, msg := "", ""
		switch {
		case !present:
			state = "deleted"
		case tip != row.Tip:
			state, msg = "abandoned", "ref is at a foreign tip"
		default:
			// Still at the tip: the delete is retried, which needs the run to have no open hold.
			return s.heldDeleteItem(ctx, row, heldLaneDeleteRetry)
		}
		wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), heldCleanupWriteTimeout)
		defer wcancel()
		hq, ok := s.q.(heldCleanupQueries)
		if !ok {
			return 0, errors.New("held cleanup: queries are not wired")
		}
		ok, err := heldRecordDelete(wctx, hq, row, state, msg, time.Time{})
		if err != nil {
			return 0, fmt.Errorf("held cleanup: record %s: %s", state, secretscrub.Scrub(err.Error()))
		}
		return boolToInt(ok), nil
	}
	return 0, nil
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// heldReconcileFailed records a reconcile step that could not list the ref: a create stays
// unknown (its next check scheduled), a delete is deferred. Nothing becomes terminal.
func (s *Service) heldReconcileFailed(ctx context.Context, row store.RunHeldPublication, cause error, pat string) error {
	msg := "forge connection is gone"
	if !errors.Is(cause, ErrHeldRemoteGone) {
		msg = heldErrText(cause, pat)
	}
	slog.Warn("held cleanup: reconcile listing failed", "run", row.RunID, "state", row.State, "error", msg)
	if row.State == "delete_unknown" {
		return s.heldDefer(ctx, row, msg, 1)
	}
	if _, err := s.heldRecordOutcome(ctx, row.ID, "create_unknown", "", msg, s.now().Add(heldBackoff(row.Attempts))); err != nil {
		return fmt.Errorf("held cleanup: record unknown create: %s", secretscrub.Scrub(err.Error()))
	}
	return nil
}

// heldDeleteItem is the deletion protocol for one row (see the file comment).
func (s *Service) heldDeleteItem(ctx context.Context, row store.RunHeldPublication, lane heldLane) (n int64, err error) {
	if s.txBeginner == nil {
		return 0, errors.New("held cleanup: transactions are not wired")
	}
	// 1. The forge connection, before BEGIN.
	f, ferr := s.heldRemote(ctx, row.RunID)
	if ferr != nil {
		msg := "forge connection is gone"
		if !errors.Is(ferr, ErrHeldRemoteGone) {
			msg = heldErrText(ferr, "")
		}
		slog.Warn("held cleanup: forge connection unavailable", "run", row.RunID, "error", msg)
		return 0, s.heldDefer(ctx, row, msg, 1)
	}
	dl, _ := ctx.Deadline()
	forgeCtx, cancelForge := context.WithDeadline(ctx, dl.Add(-heldCleanupTxReserve))
	defer cancelForge()

	// 2. One transaction on its own connection.
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("held cleanup: begin: %s", secretscrub.Scrub(err.Error()))
	}
	done := false
	rollback := func() {
		if !done {
			done = true
			rctx, c := context.WithTimeout(context.WithoutCancel(ctx), heldCleanupWriteTimeout)
			defer c()
			_ = tx.Rollback(rctx)
		}
	}
	defer rollback()
	// skip abandons the transaction first, so the scheduling write cannot queue behind its locks.
	skip := func(msg string, bump int32) (int64, error) {
		rollback()
		return 0, s.heldDefer(ctx, row, msg, bump)
	}
	qt := store.New(tx)

	repo, err := qt.LockRunForHeldCleanup(ctx, row.RunID)
	if err != nil {
		if isLockNotAvailable(err) {
			return skip("", 0) // the run is busy (a claim, step B or an owner write)
		}
		return skip(heldErrText(fmt.Errorf("run lock: %w", err), f.PAT), 1)
	}
	if _, err = tx.Exec(ctx, "SET LOCAL lock_timeout = '"+heldCleanupLockTimeout+"'"); err != nil {
		return 0, fmt.Errorf("held cleanup: lock timeout: %s", secretscrub.Scrub(err.Error()))
	}
	if s.heldCleanupHook != nil {
		s.heldCleanupHook("run_locked")
	}
	cur, err := qt.LockHeldPublicationByRunGeneration(ctx, store.LockHeldPublicationByRunGenerationParams{RunID: row.RunID, Generation: row.Generation})
	if err != nil {
		if isLockNotAvailable(err) {
			return skip("", 0)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return skip(heldErrText(fmt.Errorf("publication lock: %w", err), f.PAT), 1)
	}

	// 3. Re-validate under the locks.
	if cur.ID != row.ID || !repo.Valid || repo.Bytes != [16]byte(cur.RepoID) {
		return skip("run repository no longer matches the publication", 1)
	}
	if !s.heldLanePredicate(cur, lane) {
		return 0, nil // another worker already moved the row on
	}
	open, err := qt.CountOpenHoldsForRun(ctx, row.RunID)
	if err != nil {
		return skip(heldErrText(fmt.Errorf("open holds: %w", err), f.PAT), 1)
	}
	if open > 0 {
		return skip("", 0) // never delete while any hold of the run is open
	}

	// 4. List, then CAS delete and re-list. Forge errors leave the row as it was.
	tips, err := s.heldListFn(forgeCtx, f.ListOptions(), cur.Ref)
	if err != nil {
		return skip(heldErrText(err, f.PAT), 1)
	}
	tip, present := tips[cur.Ref]
	state, msg := "", ""
	var next time.Time
	switch {
	case !present:
		state = "deleted"
	case tip != cur.Tip:
		state, msg = "abandoned", "ref is at a foreign tip"
	default:
		delErr := s.heldDeleteFn(forgeCtx, f.DeleteOptions(cur.Ref, cur.Tip))
		after, lerr := s.heldListFn(forgeCtx, f.ListOptions(), cur.Ref)
		atip, apresent := after[cur.Ref]
		switch {
		case lerr == nil && !apresent:
			state = "deleted"
		case lerr == nil && atip != cur.Tip:
			state, msg = "abandoned", "ref is at a foreign tip"
		default:
			state, next = "delete_unknown", s.now().Add(heldBackoff(cur.Attempts))
			switch {
			case delErr != nil:
				msg = heldErrText(delErr, f.PAT)
			case lerr != nil:
				msg = heldErrText(lerr, f.PAT)
			default:
				msg = "ref still at the recorded tip after the delete"
			}
		}
	}

	// 5. Record and commit on the same transaction.
	ok, err := heldRecordDelete(ctx, qt, cur, state, msg, next)
	if err != nil {
		return 0, fmt.Errorf("held cleanup: record %s: %s", state, secretscrub.Scrub(err.Error()))
	}
	if !ok {
		return 0, nil
	}
	if err := tx.Commit(ctx); err != nil {
		done = true // a failed commit ends the transaction; the next listing settles the row
		return 0, fmt.Errorf("held cleanup: commit: %s", secretscrub.Scrub(err.Error()))
	}
	done = true
	return boolToInt(state != "delete_unknown" || cur.State != "delete_unknown"), nil
}

// heldLanePredicate re-checks, on the locked row, the condition that put it in its lane.
func (s *Service) heldLanePredicate(cur store.RunHeldPublication, lane heldLane) bool {
	switch lane {
	case heldLaneOrphan:
		return cur.State == "created" && !cur.AcknowledgedAt.Valid
	case heldLaneExpiry:
		return cur.State == "acknowledged" && cur.ExpiresAt.Valid && !cur.ExpiresAt.Time.After(s.now())
	case heldLaneDeleteRetry:
		return cur.State == "delete_unknown"
	}
	return false
}
