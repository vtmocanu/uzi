package workersvc

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4512 (ADR-4007 T3a): the scheduled once-sweep admission seam in createRun. These tests drive
// the real CreateScheduledAutopilotRun / CreateScheduledRun against a throwaway Postgres: the run, the
// fire header and the ledger row commit together or not at all, and the locks the admission takes
// never deadlock with a repo delete, a secret delete or a schedule edit. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh). Every name ends LiveDB.

type onceFx struct {
	env   codexTestEnv
	svc   *Service
	user  uuid.UUID
	repo  uuid.UUID
	sched store.RunSchedule
	next  int64
}

func newOnceFx(t *testing.T) *onceFx {
	t.Helper()
	env := setupCodexLiveDB(t)
	userID, repoID := seedClaudeOnlyOriginUser(t, env)
	svc := atomicSvc(env)
	svc.SetForges(originDriftForges{f: &originDriftForge{}})
	return newOnceFxFor(t, env, svc, userID, repoID)
}

func newOnceFxFor(t *testing.T, env codexTestEnv, svc *Service, userID, repoID uuid.UUID) *onceFx {
	t.Helper()
	f := &onceFx{env: env, svc: svc, user: userID, repo: repoID, next: 96}
	f.sched = f.newSchedule(t, repoID)
	return f
}

func (f *onceFx) newSchedule(t *testing.T, repo uuid.UUID) store.RunSchedule {
	t.Helper()
	sc, err := f.env.q.CreateRunSchedule(f.env.ctx, store.CreateRunScheduleParams{
		UserID: f.user, RepoID: repo, Target: "sweep", Timing: "once",
		RunAt:      pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true},
		NextFireAt: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true},
		Timezone:   "UTC", AutoApprove: true, Enabled: true, Labels: []byte(`["uzi"]`),
	})
	if err != nil {
		t.Fatalf("create once sweep: %v", err)
	}
	return sc
}

// issue seeds an eligible cached issue and returns its number.
func (f *onceFx) issue(t *testing.T) int64 {
	t.Helper()
	f.next++
	seedEligibleIssue(t, f.env, f.repo, f.next)
	return f.next
}

func (f *onceFx) progress(iid int64, capacity *int) *OnceFireProgress {
	return &OnceFireProgress{
		ScheduleID: f.sched.ID, UserID: f.user, RepoID: f.repo, FireEpoch: 0, IssueIID: iid,
		Title: "origin-drift issue", WebURL: "https://forge.e2e/i", EffectiveCap: capacity,
	}
}

// admit runs one scheduled autopilot admission for iid.
func (f *onceFx) admit(iid int64, p *OnceFireProgress) (store.Run, error) {
	wait := false
	return f.svc.CreateScheduledAutopilotRun(f.env.ctx, f.user, f.repo, iid, "desc", &wait, nil, nil, false, nil, nil, p)
}

func (f *onceFx) runs(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM runs WHERE user_id=$1 AND repo_id=$2`, f.user, f.repo).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *onceFx) ledgerRows(t *testing.T) (headers, rows int) {
	t.Helper()
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT (SELECT count(*) FROM schedule_once_fires WHERE schedule_id=$1),
		(SELECT count(*) FROM schedule_once_fire_runs WHERE schedule_id=$1)`, f.sched.ID).Scan(&headers, &rows); err != nil {
		t.Fatal(err)
	}
	return headers, rows
}

// assertNothing fails unless no run, header or ledger row exists.
func (f *onceFx) assertNothing(t *testing.T, why string) {
	t.Helper()
	if n := f.runs(t); n != 0 {
		t.Fatalf("%s: %d run row(s) exist, want 0", why, n)
	}
	if h, r := f.ledgerRows(t); h != 0 || r != 0 {
		t.Fatalf("%s: headers=%d ledger rows=%d, want 0/0", why, h, r)
	}
}

// waitBlocked waits until a backend whose query matches like is blocked on a lock.
func waitBlocked(t *testing.T, env codexTestEnv, like string) {
	t.Helper()
	waitBlockedPID(t, env, like)
}

// waitBlockedPID is waitBlocked returning the blocked backend's pid.
func waitBlockedPID(t *testing.T, env codexTestEnv, like string) int32 {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var pid int32
		err := env.pool.QueryRow(env.ctx, `SELECT pid FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query LIKE $1 AND pid <> pg_backend_pid() LIMIT 1`, like).Scan(&pid)
		if err == nil {
			return pid
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no backend blocked on a lock with a query like %q", like)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// awaitReached waits for a commit hook's reached signal, failing instead of hanging when the admission
// ends first (its error says why) or never gets there.
func awaitReached(t *testing.T, reached <-chan struct{}, admitErr <-chan error) {
	t.Helper()
	select {
	case <-reached:
	case err := <-admitErr:
		t.Fatalf("the admission ended before reaching its commit hook: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the admission never reached its commit hook")
	}
}

func isDeadlock(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "40P01"
}

// TestOnceFireAdmissionRecordsRunAndLedgerLiveDB: an admission writes the run, the header (with the
// passed cap) and a ledger row whose run_uuid is that run, through both scheduled seams; a repeat of the
// same issue and a spent cap create nothing. (nil progress writing no progress rows is asserted in
// TestOnceFireNilProgressWritesNothingLiveDB.)
func TestOnceFireAdmissionRecordsRunAndLedgerLiveDB(t *testing.T) {
	f := newOnceFx(t)

	iid := f.issue(t)
	run, err := f.admit(iid, f.progress(iid, onceCap(2)))
	if err != nil {
		t.Fatalf("admission: %v", err)
	}
	var runUUID uuid.UUID
	var title, url string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT run_uuid, issue_title, web_url FROM schedule_once_fire_runs WHERE schedule_id=$1 AND fire_epoch=0 AND issue_iid=$2`,
		f.sched.ID, iid).Scan(&runUUID, &title, &url); err != nil {
		t.Fatalf("ledger row: %v", err)
	}
	if runUUID != run.ID || title != "origin-drift issue" || url != "https://forge.e2e/i" {
		t.Fatalf("ledger row = %s %q %q, want run %s", runUUID, title, url, run.ID)
	}
	var capVal pgtype.Int4
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT effective_cap FROM schedule_once_fires WHERE schedule_id=$1 AND fire_epoch=0`, f.sched.ID).Scan(&capVal); err != nil || !capVal.Valid || capVal.Int32 != 2 {
		t.Fatalf("header cap = %+v %v, want 2", capVal, err)
	}

	// The same issue again (its first run finished, so no active-run guard applies): recorded, nothing created.
	f.env.exec(`UPDATE runs SET status='completed' WHERE id=$1`, run.ID)
	if _, err := f.admit(iid, f.progress(iid, onceCap(2))); !errors.Is(err, ErrOnceFireCandidateRecorded) {
		t.Fatalf("repeat admission = %v, want ErrOnceFireCandidateRecorded", err)
	}
	if n := f.runs(t); n != 1 {
		t.Fatalf("runs after a repeat admission = %d, want 1", n)
	}

	// The non-auto-approve seam records the same way, and the second slot fills the cap.
	iid2 := f.issue(t)
	wait := false
	run2, err := f.svc.CreateScheduledRun(f.env.ctx, f.user, f.repo, iid2, "desc", &wait, nil, nil, false, nil, nil, nil, f.progress(iid2, onceCap(2)))
	if err != nil {
		t.Fatalf("CreateScheduledRun admission: %v", err)
	}
	if _, rows := f.ledgerRows(t); rows != 2 {
		t.Fatalf("ledger rows = %d, want 2", rows)
	}
	var got uuid.UUID
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT run_uuid FROM schedule_once_fire_runs WHERE schedule_id=$1 AND issue_iid=$2`, f.sched.ID, iid2).Scan(&got); err != nil || got != run2.ID {
		t.Fatalf("CreateScheduledRun ledger run_uuid = %v %v, want %s", got, err, run2.ID)
	}

	// Cap spent: the stored cap (2) wins over a larger passed one.
	iid3 := f.issue(t)
	if _, err := f.admit(iid3, f.progress(iid3, onceCap(50))); !errors.Is(err, ErrOnceFireCapSpent) {
		t.Fatalf("third admission at cap 2 = %v, want ErrOnceFireCapSpent", err)
	}
	if n := f.runs(t); n != 2 {
		t.Fatalf("runs after a spent cap = %d, want 2", n)
	}
}

// TestOnceFireNilCapIsUnlimitedLiveDB: a NULL cap never reads as spent (the nil check is explicit).
func TestOnceFireNilCapIsUnlimitedLiveDB(t *testing.T) {
	f := newOnceFx(t)
	for i := 0; i < 3; i++ {
		iid := f.issue(t)
		if _, err := f.admit(iid, f.progress(iid, nil)); err != nil {
			t.Fatalf("unlimited admission %d: %v", i, err)
		}
	}
	var capVal pgtype.Int4
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT effective_cap FROM schedule_once_fires WHERE schedule_id=$1`, f.sched.ID).Scan(&capVal); err != nil || capVal.Valid {
		t.Fatalf("header cap = %+v %v, want NULL", capVal, err)
	}
	if _, rows := f.ledgerRows(t); rows != 3 {
		t.Fatalf("ledger rows = %d, want 3", rows)
	}
	// A cap of 0 is spent immediately, not unlimited.
	f.sched = f.newSchedule(t, f.repo)
	iid := f.issue(t)
	if _, err := f.admit(iid, f.progress(iid, onceCap(0))); !errors.Is(err, ErrOnceFireCapSpent) {
		t.Fatalf("cap 0 admission = %v, want ErrOnceFireCapSpent", err)
	}
}

// TestOnceFireNilProgressWritesNothingLiveDB: nil progress is today's behaviour: no header, no ledger.
func TestOnceFireNilProgressWritesNothingLiveDB(t *testing.T) {
	f := newOnceFx(t)
	iid := f.issue(t)
	if _, err := f.admit(iid, nil); err != nil {
		t.Fatalf("nil-progress admission: %v", err)
	}
	if h, r := f.ledgerRows(t); h != 0 || r != 0 {
		t.Fatalf("nil progress wrote headers=%d rows=%d", h, r)
	}
}

// TestOnceFireRollbackLiveDB: any failure between the ledger insert and COMMIT leaves no run, header or
// ledger row: an error-returning hook, a Codex-freeze failure and a context ended inside the commit hook.
func TestOnceFireRollbackLiveDB(t *testing.T) {
	t.Run("createBeforeCommitErrHook", func(t *testing.T) {
		f := newOnceFx(t)
		iid := f.issue(t)
		boom := errors.New("injected failure after the ledger insert")
		var sawRun store.Run
		f.svc.createBeforeCommitErrHook = func(run store.Run) error { sawRun = run; return boom }
		if _, err := f.admit(iid, f.progress(iid, onceCap(2))); !errors.Is(err, boom) {
			t.Fatalf("admission = %v, want the injected error", err)
		}
		if sawRun.ID == uuid.Nil {
			t.Fatal("the hook never saw the created run: the failure was not after the writes")
		}
		f.assertNothing(t, "after the hook error")
		f.svc.createBeforeCommitErrHook = nil
		if _, err := f.admit(iid, f.progress(iid, onceCap(2))); err != nil {
			t.Fatalf("retry after the rollback: %v", err)
		}
	})

	t.Run("codex freeze failure", func(t *testing.T) {
		env := setupCodexLiveDB(t)
		userID, repoID := seedCodexOnlyOriginUser(t, env)
		svc := atomicSvc(env)
		svc.SetForges(originDriftForges{f: &originDriftForge{}})
		boom := errors.New("injected codex freeze failure")
		var froze atomic.Bool
		svc.codexFreezeFn = func(context.Context, codexFreezeStore, uuid.UUID, uuid.UUID, uuid.UUID, string) error {
			froze.Store(true)
			return boom
		}
		f := newOnceFxFor(t, env, svc, userID, repoID)
		iid := f.issue(t)
		if _, err := f.admit(iid, f.progress(iid, onceCap(2))); !errors.Is(err, boom) {
			t.Fatalf("admission = %v, want the freeze error", err)
		}
		if !froze.Load() {
			t.Fatal("the Codex freeze never ran: the failure was not after the ledger insert")
		}
		f.assertNothing(t, "after the Codex-freeze failure")
	})

	t.Run("context ended inside createBeforeCommitHook", func(t *testing.T) {
		f := newOnceFx(t)
		iid := f.issue(t)
		ctx, cancel := context.WithCancel(f.env.ctx)
		defer cancel()
		var sawRun store.Run
		f.svc.createBeforeCommitHook = func(run store.Run) { sawRun = run; cancel() }
		wait := false
		if _, err := f.svc.CreateScheduledAutopilotRun(ctx, f.user, f.repo, iid, "desc", &wait, nil, nil, false, nil, nil, f.progress(iid, onceCap(2))); err == nil {
			t.Fatal("a create whose commit ran on an ended context succeeded")
		}
		if sawRun.ID == uuid.Nil {
			t.Fatal("the create never reached its commit")
		}
		f.assertNothing(t, "after the cancelled commit")
	})
}

// lostAckBeginner wraps the pool so the armed transaction's Commit really commits and then reports an
// error: the acknowledgement is lost, the data is not.
type lostAckBeginner struct {
	inner TxBeginner
	armed atomic.Bool
}

var errLostAck = errors.New("injected: commit acknowledgement lost")

func (b *lostAckBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.inner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &lostAckTx{Tx: tx, b: b}, nil
}

type lostAckTx struct {
	pgx.Tx
	b *lostAckBeginner
}

func (t *lostAckTx) Commit(ctx context.Context) error {
	if err := t.Tx.Commit(ctx); err != nil {
		return err
	}
	if t.b.armed.CompareAndSwap(true, false) {
		return errLostAck
	}
	return nil
}

// TestOnceFireLostAckLiveDB: a real commit whose acknowledgement is lost leaves the run and the ledger
// row; the retry reads them (recorded, nothing re-created).
func TestOnceFireLostAckLiveDB(t *testing.T) {
	f := newOnceFx(t)
	beginner := &lostAckBeginner{inner: f.env.pool}
	f.svc.txBeginner = beginner
	iid := f.issue(t)

	beginner.armed.Store(true)
	if _, err := f.admit(iid, f.progress(iid, onceCap(2))); !errors.Is(err, errLostAck) {
		t.Fatalf("admission = %v, want the lost acknowledgement", err)
	}
	if n := f.runs(t); n != 1 {
		t.Fatalf("runs after the lost ack = %d, want 1 (the commit happened)", n)
	}
	if h, r := f.ledgerRows(t); h != 1 || r != 1 {
		t.Fatalf("headers=%d rows=%d after the lost ack, want 1/1", h, r)
	}
	// The run is now live; the existing active-run guards would refuse a second create before the
	// ledger is read, so the retry reads the ledger once that run has finished.
	f.env.exec(`UPDATE runs SET status='completed' WHERE user_id=$1 AND repo_id=$2`, f.user, f.repo)
	if _, err := f.admit(iid, f.progress(iid, onceCap(2))); !errors.Is(err, ErrOnceFireCandidateRecorded) {
		t.Fatalf("retry = %v, want ErrOnceFireCandidateRecorded", err)
	}
	if n := f.runs(t); n != 1 {
		t.Fatalf("runs after the retry = %d, want 1", n)
	}
}

// TestOnceFireConcurrentAdmissionsAtCapOneLiveDB: a second admission at cap 1, blocked on the schedule row
// lock while the first holds it (ledger row written, not yet committed), is refused ErrOnceFireCapSpent once
// the first commits, so exactly one run starts. The ordering is forced, not raced.
func TestOnceFireConcurrentAdmissionsAtCapOneLiveDB(t *testing.T) {
	f := newOnceFx(t)
	iid1, iid2 := f.issue(t), f.issue(t)
	reached, proceed := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(proceed) }) }
	defer release()
	f.svc.createBeforeCommitHook = func(store.Run) {
		once.Do(func() { close(reached) })
		<-proceed
	}
	admitErr1 := make(chan error, 1)
	go func() { _, err := f.admit(iid1, f.progress(iid1, onceCap(1))); admitErr1 <- err }()
	awaitReached(t, reached, admitErr1) // admission 1 holds the repo share and schedule row locks, ledger row written

	admitErr2 := make(chan error, 1)
	go func() { _, err := f.admit(iid2, f.progress(iid2, onceCap(1))); admitErr2 <- err }()
	waitBlocked(t, f.env, "%FROM run_schedules WHERE id = $1 AND user_id = $2 FOR UPDATE%")

	release()
	if err := <-admitErr1; err != nil {
		t.Fatalf("admission 1: %v", err)
	}
	if err := <-admitErr2; !errors.Is(err, ErrOnceFireCapSpent) {
		t.Fatalf("admission 2 = %v, want ErrOnceFireCapSpent", err)
	}
	if n := f.runs(t); n != 1 {
		t.Fatalf("runs = %d, want exactly 1", n)
	}
	if _, rows := f.ledgerRows(t); rows != 1 {
		t.Fatalf("ledger rows = %d, want 1", rows)
	}
}

// TestOnceFireConcurrentSameIssueLiveDB is a smoke test of concurrent same-issue creates: exactly one
// run and one ledger row result. The branch lock serializes the creates and losers are normally
// refused by the ledger read (ErrOnceFireCandidateRecorded); with that check removed the
// one-active-run-per-issue gate (ErrActiveRunExists) still yields one run, so this test cannot
// pin ledger dedupe.
// Ledger dedupe is pinned by TestOnceFireAdmissionRecordsRunAndLedgerLiveDB (repeat of a recorded issue)
// and TestOnceFireLedgerPkeyConflictMapsToRecordedLiveDB (the primary-key race).
func TestOnceFireConcurrentSameIssueLiveDB(t *testing.T) {
	f := newOnceFx(t)
	iid := f.issue(t)
	const workers = 3
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = f.admit(iid, f.progress(iid, nil))
		}()
	}
	close(start)
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrOnceFireCandidateRecorded), errors.Is(err, ErrBranchInUse), errors.Is(err, ErrActiveRunExists):
		default:
			t.Fatalf("unexpected admission error: %v", err)
		}
	}
	if ok != 1 || f.runs(t) != 1 {
		t.Fatalf("started %d, runs %d, want exactly 1", ok, f.runs(t))
	}
	if _, rows := f.ledgerRows(t); rows != 1 {
		t.Fatalf("ledger rows = %d, want 1", rows)
	}
}

// TestOnceFireStaleLiveDB: a stale epoch, a deleted schedule, a no-longer-active or no-longer-once row and a
// schedule of another owner refuse with ErrOnceFireStale and create nothing.
func TestOnceFireStaleLiveDB(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, f *onceFx, p *OnceFireProgress)
	}{
		{"epoch moved by a re-arm", func(t *testing.T, f *onceFx, _ *OnceFireProgress) {
			f.env.exec(`UPDATE run_schedules SET run_at = now() + interval '3 hours', status='active' WHERE id=$1`, f.sched.ID)
		}},
		{"claimed epoch ahead of the row", func(t *testing.T, f *onceFx, p *OnceFireProgress) { p.FireEpoch = 5 }},
		{"schedule deleted", func(t *testing.T, f *onceFx, _ *OnceFireProgress) {
			f.env.exec(`DELETE FROM run_schedules WHERE id=$1`, f.sched.ID)
		}},
		{"schedule fired", func(t *testing.T, f *onceFx, _ *OnceFireProgress) {
			f.env.exec(`UPDATE run_schedules SET status='fired', next_fire_at=NULL WHERE id=$1`, f.sched.ID)
		}},
		{"schedule parked at error", func(t *testing.T, f *onceFx, _ *OnceFireProgress) {
			f.env.exec(`UPDATE run_schedules SET status='error' WHERE id=$1`, f.sched.ID)
		}},
		{"converted to recurring", func(t *testing.T, f *onceFx, _ *OnceFireProgress) {
			f.env.exec(`UPDATE run_schedules SET timing='recurring', cron_expr='*/5 * * * *', run_at=NULL WHERE id=$1`, f.sched.ID)
			// The conversion bumped the epoch, so even the old epoch is stale by epoch alone; also
			// prove the timing recheck: put the epoch back.
			f.env.exec(`UPDATE run_schedules SET fire_epoch=0 WHERE id=$1`, f.sched.ID)
		}},
		{"retargeted away from sweep", func(t *testing.T, f *onceFx, _ *OnceFireProgress) {
			f.env.exec(`UPDATE run_schedules SET target='prompt', prompt='p', labels=NULL WHERE id=$1`, f.sched.ID)
			f.env.exec(`UPDATE run_schedules SET fire_epoch=0 WHERE id=$1`, f.sched.ID)
		}},
		{"owned by another user", func(t *testing.T, f *onceFx, p *OnceFireProgress) {
			// The progress and the create arguments both name f.user (so the identity compare passes), but
			// the locked row belongs to someone else: LockRunScheduleForUser filters user_id.
			other := f.env.seedUser(t)
			f.env.exec(`DELETE FROM run_schedules WHERE id=$1`, f.sched.ID)
			f.env.exec(`INSERT INTO run_schedules (id, user_id, repo_id, target, labels, timing, run_at, timezone, next_fire_at)
				VALUES ($1, $2, $3, 'sweep', '["uzi"]', 'once', now() - interval '1 hour', 'UTC', now() - interval '1 hour')`, f.sched.ID, other, f.repo)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newOnceFx(t)
			iid := f.issue(t)
			p := f.progress(iid, onceCap(2))
			tc.setup(t, f, p)
			if _, err := f.admit(iid, p); !errors.Is(err, ErrOnceFireStale) {
				t.Fatalf("admission = %v, want ErrOnceFireStale", err)
			}
			if n := f.runs(t); n != 0 {
				t.Fatalf("%d run row(s) after a stale admission", n)
			}
			var headers int
			if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM schedule_once_fires WHERE schedule_id=$1`, f.sched.ID).Scan(&headers); err != nil || headers != 0 {
				t.Fatalf("headers after a stale admission = %d %v", headers, err)
			}
		})
	}
}

// TestOnceFireBusyGateCreatesNothingLiveDB: a busy consumer gate refuses before any ledger write.
func TestOnceFireBusyGateCreatesNothingLiveDB(t *testing.T) {
	f := newOnceFx(t)
	f.svc.consumerGateWait = shortGateWait
	iid := f.issue(t)
	release := holdGateExclusive(f.env.ctx, t, f.env.pool, f.repo)
	if _, err := f.admit(iid, f.progress(iid, onceCap(2))); !errors.Is(err, ErrTerminalConsumerGateBusy) {
		t.Fatalf("admission behind a held gate = %v, want ErrTerminalConsumerGateBusy", err)
	}
	f.assertNothing(t, "after the busy gate")
	release()
	if _, err := f.admit(iid, f.progress(iid, onceCap(2))); err != nil {
		t.Fatalf("admission after the holder ended: %v", err)
	}
}

// TestOnceFireIdentityLiveDB: a progress context that disagrees with the create arguments (checked before
// any lock) or with the locked schedule's repo fails closed with ErrOnceFireIdentity and creates nothing.
func TestOnceFireIdentityLiveDB(t *testing.T) {
	t.Run("create-argument mismatch, before any lock", func(t *testing.T) {
		f := newOnceFx(t)
		f.svc.consumerGateWait = shortGateWait
		iid := f.issue(t)
		// An exclusive gate holder: a mismatch that were checked after the gate would report Busy.
		release := holdGateExclusive(f.env.ctx, t, f.env.pool, f.repo)
		defer release()
		for name, mutate := range map[string]func(p *OnceFireProgress){
			"user":  func(p *OnceFireProgress) { p.UserID = uuid.New() },
			"repo":  func(p *OnceFireProgress) { p.RepoID = uuid.New() },
			"issue": func(p *OnceFireProgress) { p.IssueIID = iid + 1000 },
		} {
			p := f.progress(iid, onceCap(2))
			mutate(p)
			if _, err := f.admit(iid, p); !errors.Is(err, ErrOnceFireIdentity) {
				t.Fatalf("%s mismatch = %v, want ErrOnceFireIdentity", name, err)
			}
		}
		f.assertNothing(t, "after the identity mismatches")
	})

	t.Run("locked-row repo mismatch", func(t *testing.T) {
		f := newOnceFx(t)
		// A second repo of the same user: the schedule belongs to it, the create and progress name f.repo.
		otherRepo := uuid.New()
		var conn uuid.UUID
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT connection_id FROM repos WHERE id=$1`, f.repo).Scan(&conn); err != nil {
			t.Fatal(err)
		}
		f.env.exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
			VALUES ($1, $2, 2, $3, $4, 'main', true)`, otherRepo, conn, "g/"+otherRepo.String(), "https://forge.e2e/g/"+otherRepo.String())
		f.env.exec(`UPDATE run_schedules SET repo_id=$2 WHERE id=$1`, f.sched.ID, otherRepo)
		f.env.exec(`UPDATE run_schedules SET fire_epoch=0 WHERE id=$1`, f.sched.ID) // the repo change bumped it
		iid := f.issue(t)
		if _, err := f.admit(iid, f.progress(iid, onceCap(2))); !errors.Is(err, ErrOnceFireIdentity) {
			t.Fatalf("locked-row repo mismatch = %v, want ErrOnceFireIdentity", err)
		}
		if n := f.runs(t); n != 0 {
			t.Fatalf("%d run row(s) after the mismatch", n)
		}
		var headers int
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM schedule_once_fires WHERE schedule_id=$1`, f.sched.ID).Scan(&headers); err != nil || headers != 0 {
			t.Fatalf("headers after the mismatch = %d %v", headers, err)
		}
	})
}

// TestOnceFireRepoDeleteDuringAdmissionLiveDB: a repo delete overlapping an admission, in either order,
// never deadlocks: the repo share lock is taken before the schedule row lock.
func TestOnceFireRepoDeleteDuringAdmissionLiveDB(t *testing.T) {
	t.Run("admission holds the repo share lock while waiting for the schedule row, delete queues behind it", func(t *testing.T) {
		f := newOnceFx(t)
		f.env.exec(`UPDATE repos SET enabled=false WHERE id=$1`, f.repo) // DeleteRepoForUser only deletes a disabled repo
		iid := f.issue(t)

		// Session 1 holds the schedule row.
		hold, err := f.env.pool.Begin(f.env.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = hold.Rollback(context.Background()) }()
		if _, err := hold.Exec(f.env.ctx, `SELECT id FROM run_schedules WHERE id=$1 FOR UPDATE`, f.sched.ID); err != nil {
			t.Fatal(err)
		}

		// The admission takes the repo FOR SHARE and then blocks on the schedule row.
		admitErr := make(chan error, 1)
		go func() { _, err := f.admit(iid, f.progress(iid, onceCap(2))); admitErr <- err }()
		admitPID := waitBlockedPID(t, f.env, "%FROM run_schedules WHERE id = $1 AND user_id = $2 FOR UPDATE%")

		// The repo delete blocks on the repo row the admission share-locked.
		delErr := make(chan error, 1)
		go func() {
			_, err := f.env.q.DeleteRepoForUser(f.env.ctx, store.DeleteRepoForUserParams{ID: f.repo, UserID: f.user})
			delErr <- err
		}()
		delPID := waitBlockedPID(t, f.env, "%WITH deletion_scope AS%")
		var blockedByAdmission bool
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT $2 = ANY(pg_blocking_pids($1))`, delPID, admitPID).Scan(&blockedByAdmission); err != nil {
			t.Fatal(err)
		}
		if !blockedByAdmission {
			t.Fatalf("the repo delete (pid %d) is not blocked by the admission (pid %d): the admission did not take the repo lock before the schedule row", delPID, admitPID)
		}
		// pg_blocking_pids also lists backends queued ahead on the same lock, so pin the direct cause: the
		// delete waits on the admission's own transaction (the repo row share lock), not on session 1's.
		var waitsOnAdmissionTx bool
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks w JOIN pg_locks h
				ON h.locktype = 'transactionid' AND h.transactionid = w.transactionid AND h.granted
			WHERE w.pid = $1 AND w.locktype = 'transactionid' AND NOT w.granted AND h.pid = $2)`, delPID, admitPID).Scan(&waitsOnAdmissionTx); err != nil {
			t.Fatal(err)
		}
		if !waitsOnAdmissionTx {
			t.Fatalf("the repo delete (pid %d) does not wait on the admission's transaction (pid %d)", delPID, admitPID)
		}

		if err := hold.Rollback(f.env.ctx); err != nil {
			t.Fatal(err)
		}
		// Shipped order: the admission already holds the repo share lock, so it proceeds and commits, and
		// the delete (queued behind it) then removes the repo with its schedule and ledger.
		if err := <-admitErr; err != nil {
			t.Fatalf("admission (deadlock=%v): %v", isDeadlock(err), err)
		}
		if err := <-delErr; err != nil {
			t.Fatalf("repo delete (deadlock=%v): %v", isDeadlock(err), err)
		}
		var n int
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT (SELECT count(*) FROM repos WHERE id=$1) + (SELECT count(*) FROM run_schedules WHERE id=$2)
			+ (SELECT count(*) FROM schedule_once_fires WHERE schedule_id=$2) + (SELECT count(*) FROM schedule_once_fire_runs WHERE schedule_id=$2)`, f.repo, f.sched.ID).Scan(&n); err != nil || n != 0 {
			t.Fatalf("rows left after the repo delete: %d %v", n, err)
		}
	})

	t.Run("delete holds the repo row first, admission waits at step 6 and finds it gone", func(t *testing.T) {
		f := newOnceFx(t)
		iid := f.issue(t)
		tx, err := f.env.pool.Begin(f.env.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if _, err := tx.Exec(f.env.ctx, `DELETE FROM repos WHERE id=$1`, f.repo); err != nil {
			t.Fatalf("delete: %v", err)
		}
		admitErr := make(chan error, 1)
		go func() { _, err := f.admit(iid, f.progress(iid, onceCap(2))); admitErr <- err }()
		waitBlocked(t, f.env, "%FROM repos WHERE id = $1 FOR SHARE%")
		if err := tx.Commit(f.env.ctx); err != nil {
			t.Fatalf("commit delete: %v", err)
		}
		if err := <-admitErr; !errors.Is(err, ErrOnceFireStale) {
			t.Fatalf("admission = %v (deadlock=%v), want ErrOnceFireStale", err, isDeadlock(err))
		}
		var n int
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM runs WHERE repo_id=$1`, f.repo).Scan(&n); err != nil || n != 0 {
			t.Fatalf("runs for the deleted repo: %d %v", n, err)
		}
	})
}

// TestOnceFireThreeSessionInterleavingLiveDB: a transaction holding the schedule row FOR UPDATE (as
// lockScheduleForConfig does), a once admission pinned to a secret, and a delete of that secret complete
// without a 40P01, and the admission ends stale because the edit re-armed the schedule.
func TestOnceFireThreeSessionInterleavingLiveDB(t *testing.T) {
	f := newOnceFx(t)
	var secret uuid.UUID
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT id FROM user_secrets WHERE user_id=$1 AND kind='anthropic_token'`, f.user).Scan(&secret); err != nil {
		t.Fatal(err)
	}
	f.env.exec(`UPDATE run_schedules SET credential_override_mode='pinned', credential_override_secret_id=$2 WHERE id=$1`, f.sched.ID, secret)
	iid := f.issue(t)

	// Session 1: the schedule edit, holding the row lock first.
	edit, err := f.env.pool.Begin(f.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = edit.Rollback(context.Background()) }()
	if _, err := store.New(edit).LockRunScheduleForUser(f.env.ctx, store.LockRunScheduleForUserParams{ID: f.sched.ID, UserID: f.user}); err != nil {
		t.Fatal(err)
	}

	// Session 2: the admission, pinned to the secret: it share-locks the secret, then the repo, then waits on the schedule.
	override := &CredentialOverride{Mode: CredentialOverrideModePinned, SecretID: &secret}
	wait := false
	admitErr := make(chan error, 1)
	go func() {
		_, err := f.svc.CreateScheduledAutopilotRun(f.env.ctx, f.user, f.repo, iid, "desc", &wait, nil, nil, false, override, nil, f.progress(iid, onceCap(2)))
		admitErr <- err
	}()
	waitBlocked(t, f.env, "%FROM run_schedules WHERE id = $1 AND user_id = $2 FOR UPDATE%")

	// Session 3: the secret delete, queued behind the admission's share lock on the secret row.
	delErr := make(chan error, 1)
	go func() {
		_, err := f.env.pool.Exec(f.env.ctx, `DELETE FROM user_secrets WHERE id=$1`, secret)
		delErr <- err
	}()
	waitBlocked(t, f.env, "%DELETE FROM user_secrets%")

	// The edit re-arms the schedule (bumping the epoch) and commits.
	if _, err := edit.Exec(f.env.ctx, `UPDATE run_schedules SET run_at = now() + interval '5 hours' WHERE id=$1`, f.sched.ID); err != nil {
		t.Fatal(err)
	}
	if err := edit.Commit(f.env.ctx); err != nil {
		t.Fatal(err)
	}

	if err := <-admitErr; !errors.Is(err, ErrOnceFireStale) {
		t.Fatalf("admission = %v (deadlock=%v), want ErrOnceFireStale", err, isDeadlock(err))
	}
	if err := <-delErr; err != nil {
		t.Fatalf("secret delete (deadlock=%v): %v", isDeadlock(err), err)
	}
	f.assertNothing(t, "after the interleaving")
}

// TestOnceFireProgressFailsClosedWithoutTx: a non-nil progress with no transaction beginner, or on a store
// that is not *store.Queries, fails closed before anything is read.
func TestOnceFireProgressFailsClosedWithoutTx(t *testing.T) {
	user, repo := uuid.New(), uuid.New()
	p := &OnceFireProgress{ScheduleID: uuid.New(), UserID: user, RepoID: repo, IssueIID: 4, FireEpoch: 0}
	for name, svc := range map[string]*Service{
		"fake store, no beginner": New(&fakeStore{}, newBox(t), testParams()),
		"live store, no beginner": {q: &store.Queries{}},
		"fake store with beginner": func() *Service {
			s := New(&fakeStore{}, newBox(t), testParams())
			s.txBeginner = failingBeginner{}
			return s
		}(),
	} {
		if _, err := svc.CreateScheduledAutopilotRun(context.Background(), user, repo, 4, "d", nil, nil, nil, false, nil, nil, p); !errors.Is(err, errOnceFireRequiresTx) {
			t.Fatalf("%s: autopilot = %v, want errOnceFireRequiresTx", name, err)
		}
		if _, err := svc.CreateScheduledRun(context.Background(), user, repo, 4, "d", nil, nil, nil, false, nil, nil, nil, p); !errors.Is(err, errOnceFireRequiresTx) {
			t.Fatalf("%s: scheduled = %v, want errOnceFireRequiresTx", name, err)
		}
	}
}

type failingBeginner struct{}

func (failingBeginner) Begin(context.Context) (pgx.Tx, error) {
	return nil, errors.New("must not begin")
}

func onceCap(n int) *int { return &n }

// TestOnceFireLedgerPkeyConflictMapsToRecordedLiveDB: a 23505 on the ledger's primary key (a row for the
// same issue committing after the admission's existence read but before its own insert) is
// ErrOnceFireCandidateRecorded, not the generic unique-violation ErrActiveRunExists, and rolls the run back.
func TestOnceFireLedgerPkeyConflictMapsToRecordedLiveDB(t *testing.T) {
	f := newOnceFx(t)
	iid := f.issue(t)
	// The header exists and is committed, so the admission's own header insert is a no-op.
	f.env.exec(`INSERT INTO schedule_once_fires (schedule_id, fire_epoch, effective_cap) VALUES ($1, 0, 5)`, f.sched.ID)

	// A competing writer's ledger row for the same issue, uncommitted: invisible to the admission's
	// READ COMMITTED existence read, but the admission's insert waits on its unique key.
	rival, err := f.env.pool.Begin(f.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rival.Rollback(context.Background()) }()
	if _, err := rival.Exec(f.env.ctx, `INSERT INTO schedule_once_fire_runs (schedule_id, fire_epoch, repo_id, issue_iid, issue_title, web_url, run_uuid)
		VALUES ($1, 0, $2, $3, 'rival', 'u', gen_random_uuid())`, f.sched.ID, f.repo, iid); err != nil {
		t.Fatal(err)
	}
	admitErr := make(chan error, 1)
	go func() { _, err := f.admit(iid, f.progress(iid, onceCap(5))); admitErr <- err }()
	waitBlocked(t, f.env, "%INSERT INTO schedule_once_fire_runs%")
	if err := rival.Commit(f.env.ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-admitErr; !errors.Is(err, ErrOnceFireCandidateRecorded) {
		t.Fatalf("admission = %v, want ErrOnceFireCandidateRecorded", err)
	}
	if n := f.runs(t); n != 0 {
		t.Fatalf("%d run row(s) survived the conflicted admission, want 0", n)
	}
}
