package workersvc

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4512 (ADR-4007 T3a): WriteOnceFireSummary against a throwaway Postgres. The summary
// transaction takes the SAME schedule row lock every admission holds from its step 7 to its
// commit, re-reads the ledger under it and writes only for the claimed, still-active occurrence.
// These are the (n) same-epoch interleavings of the #4512 M3 list; they use the unexported
// onceFireSummaryAfterReadHook (between the in-lock read and the write) and
// createBeforeCommitHook (an admission holding the lock right before its COMMIT). Skipped
// unless UZI_TEST_DATABASE_URL points at a throwaway Postgres. Every name ends LiveDB.

const summaryLockWait = "%FROM run_schedules WHERE id = $1 AND user_id = $2 FOR UPDATE%"

// partialOf is a compose function that records what it saw and writes a last_fire naming the
// ledger rows it was handed (a stand-in for the scheduler's ledger-derived summary).
func partialOf(seen *[]int64, tag string) func(OnceFireSnapshot) (OnceFireWrite, error) {
	return func(s OnceFireSnapshot) (OnceFireWrite, error) {
		*seen = (*seen)[:0]
		for _, r := range s.Progress {
			*seen = append(*seen, r.IssueIid)
		}
		doc := `{"tag":"` + tag + `","started":[`
		for i, r := range s.Progress {
			if i > 0 {
				doc += ","
			}
			doc += itoa(r.IssueIid)
		}
		return OnceFireWrite{Kind: OnceFireWritePartial, LastFire: []byte(doc + `]}`)}, nil
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

func (f *onceFx) storedLastFire(t *testing.T) (string, string) {
	t.Helper()
	var lf *string
	var status string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT last_fire::text, status FROM run_schedules WHERE id=$1`, f.sched.ID).Scan(&lf, &status); err != nil {
		t.Fatal(err)
	}
	if lf == nil {
		return "", status
	}
	return *lf, status
}

// A summary holding the lock after reading only 96 makes a concurrent admission of 97 wait: the
// admission commits only after the summary, so a summary can never overwrite a newer one with a
// shorter list, and the next summary reads 96 and 97.
func TestOnceFireSummaryBlocksAdmissionUntilCommitLiveDB(t *testing.T) {
	f := newOnceFx(t)
	a, b := f.issue(t), f.issue(t)
	if _, err := f.admit(a, f.progress(a, onceCap(2))); err != nil {
		t.Fatal(err)
	}
	reached, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseHook := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseHook)
	f.svc.onceFireSummaryAfterReadHook = func() { close(reached); <-release }
	var seen []int64
	sumErr := make(chan error, 1)
	go func() {
		_, err := f.svc.WriteOnceFireSummary(f.env.ctx, f.sched.ID, f.user, 0, partialOf(&seen, "delayed"))
		sumErr <- err
	}()
	select {
	case <-reached:
	case <-time.After(30 * time.Second):
		t.Fatal("the summary never reached its hook")
	}
	admitErr := make(chan error, 1)
	go func() {
		_, err := f.admit(b, f.progress(b, onceCap(2)))
		admitErr <- err
	}()
	waitBlocked(t, f.env, summaryLockWait)
	if _, rows := f.ledgerRows(t); rows != 1 {
		t.Fatalf("ledger rows while the summary holds the lock = %d, want 1 (the admission must wait)", rows)
	}
	releaseHook()
	if err := <-sumErr; err != nil {
		t.Fatal(err)
	}
	if err := <-admitErr; err != nil {
		t.Fatalf("the waiting admission must commit after the summary: %v", err)
	}
	lf, _ := f.storedLastFire(t)
	if lf != `{"tag": "delayed", "started": [`+itoa(a)+`]}` {
		t.Fatalf("delayed summary stored %s, want only the start it read", lf)
	}
	f.svc.onceFireSummaryAfterReadHook = nil
	if _, err := f.svc.WriteOnceFireSummary(f.env.ctx, f.sched.ID, f.user, 0, partialOf(&seen, "later")); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("a later summary read %v, want both starts", seen)
	}
	if lf, _ := f.storedLastFire(t); lf != `{"tag": "later", "started": [`+itoa(a)+`, `+itoa(b)+`]}` {
		t.Fatalf("later summary stored %s, want both starts", lf)
	}
}

// An admission that has taken the lock and not yet committed makes a completing summary wait on
// it, so the admission is in the completion's final last_fire.
func TestOnceFireSummaryWaitsForCommittingAdmissionLiveDB(t *testing.T) {
	testOnceFireSummaryWaitsForCommittingAdmission(t, false)
}

// A real pool's session default can be REPEATABLE READ (including through the database's
// default_transaction_isolation). The helper must override it before its first snapshot.
// Without the pin, an admission that commits during the lock wait is absent from the ledger read.
func TestOnceFireSummaryPinsReadCommittedOverPoolDefaultLiveDB(t *testing.T) {
	testOnceFireSummaryWaitsForCommittingAdmission(t, true)
}

func testOnceFireSummaryWaitsForCommittingAdmission(t *testing.T, repeatableReadDefault bool) {
	t.Helper()
	f := newOnceFx(t)
	summaryService := f.svc
	if repeatableReadDefault {
		cfg := f.env.pool.Config()
		params := make(map[string]string, len(cfg.ConnConfig.RuntimeParams)+1)
		for key, value := range cfg.ConnConfig.RuntimeParams {
			params[key] = value
		}
		params["default_transaction_isolation"] = "repeatable read"
		cfg.ConnConfig.RuntimeParams = params
		pool, err := pgxpool.NewWithConfig(f.env.ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		var isolation string
		if err := pool.QueryRow(f.env.ctx, "SHOW default_transaction_isolation").Scan(&isolation); err != nil || isolation != "repeatable read" {
			t.Fatalf("summary pool default isolation = %q, %v, want repeatable read", isolation, err)
		}
		summaryService = atomicSvc(f.env)
		summaryService.txBeginner = pool // production also opens its own transaction through pool.Begin
	}
	a, b := f.issue(t), f.issue(t)
	if _, err := f.admit(a, f.progress(a, onceCap(2))); err != nil {
		t.Fatal(err)
	}
	reached, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseHook := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseHook)
	f.svc.createBeforeCommitHook = func(store.Run) { close(reached); <-release }
	admitErr := make(chan error, 1)
	go func() {
		_, err := f.admit(b, f.progress(b, onceCap(2)))
		admitErr <- err
	}()
	awaitReached(t, reached, admitErr)
	f.svc.createBeforeCommitHook = nil

	var seen []int64
	compose := func(s OnceFireSnapshot) (OnceFireWrite, error) {
		w, err := partialOf(&seen, "complete")(s)
		w.Kind, w.Status = OnceFireWriteAdvance, "fired"
		w.LastFiredAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
		return w, err
	}
	sumErr := make(chan error, 1)
	go func() {
		_, err := summaryService.WriteOnceFireSummary(f.env.ctx, f.sched.ID, f.user, 0, compose)
		sumErr <- err
	}()
	waitBlocked(t, f.env, summaryLockWait)
	releaseHook()
	if err := <-admitErr; err != nil {
		t.Fatal(err)
	}
	if err := <-sumErr; err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("the completion read %v, want the admission that committed while it waited", seen)
	}
	lf, status := f.storedLastFire(t)
	if status != "fired" || lf != `{"tag": "complete", "started": [`+itoa(a)+`, `+itoa(b)+`]}` {
		t.Fatalf("final row status=%q last_fire=%s", status, lf)
	}
}

// A stale occurrence (another epoch, a missing or foreign schedule, a row no longer active) writes
// nothing and never calls compose.
func TestOnceFireSummaryStaleWritesNothingLiveDB(t *testing.T) {
	f := newOnceFx(t)
	a := f.issue(t)
	if _, err := f.admit(a, f.progress(a, onceCap(2))); err != nil {
		t.Fatal(err)
	}
	before, _ := f.storedLastFire(t)
	never := func(OnceFireSnapshot) (OnceFireWrite, error) {
		t.Fatal("compose ran for a stale occurrence")
		return OnceFireWrite{}, nil
	}
	if res, err := f.svc.WriteOnceFireSummary(f.env.ctx, f.sched.ID, f.user, 1, never); err != nil || !res.Stale || res.Wrote {
		t.Fatalf("other epoch: res=%+v err=%v", res, err)
	}
	if res, err := f.svc.WriteOnceFireSummary(f.env.ctx, f.sched.ID, uuid.New(), 0, never); err != nil || !res.Stale {
		t.Fatalf("foreign owner: res=%+v err=%v", res, err)
	}
	f.env.exec(`UPDATE run_schedules SET status='fired' WHERE id=$1`, f.sched.ID)
	if res, err := f.svc.WriteOnceFireSummary(f.env.ctx, f.sched.ID, f.user, 0, never); err != nil || !res.Stale {
		t.Fatalf("fired row: res=%+v err=%v", res, err)
	}
	if after, _ := f.storedLastFire(t); after != before {
		t.Fatalf("a stale summary wrote last_fire: %q -> %q", before, after)
	}
}

// A compose failure writes nothing; a held write with no progress is the epoch-guarded
// RecordScheduleHeldFire; a lock wait past 5s ends in an error (transient) without writing.
func TestOnceFireSummaryComposeHeldAndLockTimeoutLiveDB(t *testing.T) {
	f := newOnceFx(t)
	boom := errors.New("compose failed")
	if _, err := f.svc.WriteOnceFireSummary(f.env.ctx, f.sched.ID, f.user, 0, func(OnceFireSnapshot) (OnceFireWrite, error) {
		return OnceFireWrite{}, boom
	}); !errors.Is(err, boom) {
		t.Fatalf("compose error = %v", err)
	}
	res, err := f.svc.WriteOnceFireSummary(f.env.ctx, f.sched.ID, f.user, 0, func(s OnceFireSnapshot) (OnceFireWrite, error) {
		if len(s.Progress) != 0 {
			t.Fatalf("progress = %+v, want none", s.Progress)
		}
		return OnceFireWrite{Kind: OnceFireWriteHeld, LastFire: []byte(`{"matched":1,"skips":[],"started":[]}`)}, nil
	})
	if err != nil || !res.Wrote {
		t.Fatalf("held write: res=%+v err=%v", res, err)
	}
	var recent int
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT jsonb_array_length(recent_fires) FROM run_schedules WHERE id=$1`, f.sched.ID).Scan(&recent); err != nil || recent != 1 {
		t.Fatalf("recent_fires entries = %d, %v, want the held write's one", recent, err)
	}

	// Another session holds the schedule row lock: the summary gives up after 5s.
	holder, err := f.env.pool.Begin(f.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	if _, err := store.New(holder).LockRunScheduleForUser(f.env.ctx, store.LockRunScheduleForUserParams{ID: f.sched.ID, UserID: f.user}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = f.svc.WriteOnceFireSummary(f.env.ctx, f.sched.ID, f.user, 0, func(OnceFireSnapshot) (OnceFireWrite, error) {
		t.Fatal("compose ran without the lock")
		return OnceFireWrite{}, nil
	})
	// The lock_timeout and the helper's detached context are both 5s, so either ends the wait: the
	// server's 55P03 or the client's context deadline. Both are transient to the scheduler, and
	// neither writes anything.
	var pe *pgconn.PgError
	isLockTimeout := errors.As(err, &pe) && pe.Code == "55P03"
	if !isLockTimeout && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock wait error = %v, want 55P03 or the 5s context deadline", err)
	}
	if d := time.Since(start); d < 4*time.Second || d > 9*time.Second {
		t.Fatalf("lock wait lasted %v, want about 5s", d)
	}
}

// Without a transaction beginner the helper fails closed.
func TestOnceFireSummaryFailsClosedWithoutTx(t *testing.T) {
	s := &Service{}
	if _, err := s.WriteOnceFireSummary(context.Background(), uuid.Nil, uuid.Nil, 0, func(OnceFireSnapshot) (OnceFireWrite, error) {
		t.Fatal("compose ran")
		return OnceFireWrite{}, nil
	}); !errors.Is(err, errOnceFireRequiresTx) {
		t.Fatalf("err = %v, want errOnceFireRequiresTx", err)
	}
}
