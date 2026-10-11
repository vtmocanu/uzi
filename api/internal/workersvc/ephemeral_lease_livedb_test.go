package workersvc

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #2006: the workersvc half of the ephemeral worker lease, against a REAL Postgres. The lease
// starts inside the transaction that commits a worker-reported terminal state, with the worker row
// locked before the run's (the setState generation fence tx and the interlocked completion's permit
// tx), and a follow-up claim rebinds the leased worker in the claim transaction under a fresh
// database clock. Every ordering here is a lock interleaving a fake store cannot exhibit. Skipped
// unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (run via ./e2e/run-store-it.sh).
//
// The interleavings are forced, not raced: a gated transaction beginner parks a transaction right
// before its COMMIT (every statement already executed, every lock still held), and a side
// transaction stands in for another writer holding one row lock. Each test names the protection it
// pins; removing that protection turns it red.

var leaseIIDSeq int64 = 800000

func nextLeaseIID() int64 { return atomic.AddInt64(&leaseIIDSeq, 1) }

// leaseEnv is one owner, one repo, with claim assembly enabled (a real bot PAT and Anthropic token).
type leaseEnv struct {
	codexTestEnv
	userID, repoID uuid.UUID
}

func newLeaseEnv(t *testing.T) leaseEnv {
	t.Helper()
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	enableClaimAssembly(t, env, userID)
	return leaseEnv{codexTestEnv: env, userID: userID, repoID: repoID}
}

// service builds a Service over the live store, the given transaction beginner and lease.
func (e leaseEnv) service(lease time.Duration, tx TxBeginner) *Service {
	svc := New(e.q, e.box, testParams())
	svc.SetTxBeginner(tx)
	svc.SetBackground(func(func()) {})
	svc.SetEphemeralLease(lease)
	return svc
}

// gateTxBeginner wraps the pool. After arm(), the NEXT Begin returns a transaction whose Commit
// parks: it signals reached, then waits for release before really committing. Nested Begin (a
// SAVEPOINT) is promoted from the real transaction, so only the outer commit is gated.
type gateTxBeginner struct {
	pool    *pgxpool.Pool
	armed   atomic.Bool
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGate(t *testing.T, pool *pgxpool.Pool) *gateTxBeginner {
	g := &gateTxBeginner{pool: pool, reached: make(chan struct{}, 1), release: make(chan struct{})}
	// A failing test must not leave a parked commit holding the pool open.
	t.Cleanup(func() { g.open() })
	return g
}

// open lets the parked commit proceed; idempotent.
func (g *gateTxBeginner) open() { g.once.Do(func() { close(g.release) }) }

func (g *gateTxBeginner) arm() { g.armed.Store(true) }

func (g *gateTxBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := g.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if g.armed.CompareAndSwap(true, false) {
		return &gatedTx{Tx: tx, g: g}, nil
	}
	return tx, nil
}

// waitReached blocks until the gated transaction is parked at its commit.
func (g *gateTxBeginner) waitReached(t *testing.T) {
	t.Helper()
	select {
	case <-g.reached:
	case <-time.After(30 * time.Second):
		t.Fatal("the gated transaction never reached its commit")
	}
}

type gatedTx struct {
	pgx.Tx
	g *gateTxBeginner
}

func (t *gatedTx) Commit(ctx context.Context) error {
	t.g.reached <- struct{}{}
	select {
	case <-t.g.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return t.Tx.Commit(ctx)
}

// ---- seeds ----------------------------------------------------------------

// seedBound inserts an ephemeral hosted worker bound to a fresh issue run owned by it at `status`
// and claim generation `gen`, and returns (worker, run, issue iid). The worker advertises the
// interlock capability so an interlocked run can be completed through it.
func (e leaseEnv) seedBound(t *testing.T, status string, gen int64, interlocked bool) (workerID, runID uuid.UUID, iid int64) {
	t.Helper()
	workerID, runID, iid = uuid.New(), uuid.New(), nextLeaseIID()
	var contract, frozen, completed any
	var cv any
	if interlocked {
		fz := frozenJSON(t)
		c, err := buildCompletionContract(fz)
		if err != nil {
			t.Fatalf("buildCompletionContract: %v", err)
		}
		contract, frozen, completed, cv = c, fz, idsJSON(t), int32(1)
	}
	e.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, claim_generation,
	               completion_contract_version, contract_revision, completion_contract, milestones_frozen, milestones_completed)
	        VALUES ($1, $2, $3, 'issue', $4, 't', 'd', $5, $6, $7, $8, $9, $10, $11)`,
		runID, e.userID, e.repoID, iid, status, gen, cv, nilIf(interlocked, int32(1)), contract, frozen, completed)
	e.exec(`INSERT INTO workers (id, user_id, name, token_hash, template_declared, kind, hosted_size, docker_enabled, ephemeral,
	               ephemeral_run_id, status, online_since, last_heartbeat_at, protocol_capabilities, snapshot_register_nonce, snapshot_epoch)
	        VALUES ($1, $2, $3, $4, 'base', 'hosted', 'm', false, true, $5, 'online', now(), now(), $6, 'nonce-A', 0)`,
		workerID, e.userID, "eph-"+workerID.String()[:8], workerID[:], runID,
		[]string{capability.CompletionInterlockV1})
	e.exec(`UPDATE runs SET worker_id = $2 WHERE id = $1`, runID, workerID)
	return workerID, runID, iid
}

func nilIf(cond bool, v any) any {
	if cond {
		return v
	}
	return nil
}

// openHold opens an OPEN custody hold at generation gen naming workerID/runID as the live holder.
func (e leaseEnv) openHold(t *testing.T, runID uuid.UUID, gen int64, workerID uuid.UUID) uuid.UUID {
	t.Helper()
	holdID := uuid.New()
	e.exec(`INSERT INTO recovery_custody_holds
	          (id, user_id, repo_id, run_id, generation, state, original_worker_id, original_worker_identity, live_worker_id, live_run_id)
	        VALUES ($1, $2, $3, $4, $5, 'open', $6, 'ident', $7, $4)`,
		holdID, e.userID, e.repoID, runID, gen, uuid.New(), workerID)
	return holdID
}

func (e leaseEnv) holdState(t *testing.T, holdID uuid.UUID) string {
	t.Helper()
	var s string
	if err := e.pool.QueryRow(e.ctx, `SELECT state FROM recovery_custody_holds WHERE id = $1`, holdID).Scan(&s); err != nil {
		t.Fatalf("read hold state: %v", err)
	}
	return s
}

func (e leaseEnv) workerExists(t *testing.T, id uuid.UUID) bool {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM workers WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("count workers: %v", err)
	}
	return n > 0
}

// workerRow reads the worker row; the caller must know it exists.
func (e leaseEnv) workerRow(t *testing.T, id uuid.UUID) store.Worker {
	t.Helper()
	w, err := e.q.GetWorkerByID(e.ctx, id)
	if err != nil {
		t.Fatalf("GetWorkerByID %s: %v", id, err)
	}
	return w
}

func (e leaseEnv) leased(t *testing.T, id uuid.UUID) bool {
	t.Helper()
	w := e.workerRow(t, id)
	if w.LeaseSince.Valid != w.LeaseRepoID.Valid || w.LeaseSince.Valid != w.LeaseBranch.Valid {
		t.Fatalf("lease columns set partially: %+v", w)
	}
	return w.LeaseSince.Valid
}

func (e leaseEnv) runStatusOf(t *testing.T, id uuid.UUID) string {
	return statusOf(t, e.codexTestEnv, id)
}

func (e leaseEnv) boundRun(t *testing.T, workerID uuid.UUID) pgtype.UUID {
	t.Helper()
	return e.workerRow(t, workerID).EphemeralRunID
}

// reap runs the real reaper with a deadline cutoff in the past (so only the "no live bound run" arm
// can select a fresh online worker) and the given lease.
func (e leaseEnv) reap(t *testing.T, lease time.Duration) int64 {
	t.Helper()
	iv := pgtype.Interval{}
	if lease > 0 {
		iv = pgtype.Interval{Microseconds: lease.Microseconds(), Valid: true}
	}
	n, err := store.ReapEphemeralWorkers(e.ctx, e.pool, pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}, iv)
	if err != nil {
		t.Fatalf("ReapEphemeralWorkers: %v", err)
	}
	return n
}

// waitLockWaiters blocks until at least n backends in this database are waiting on a lock.
func (e leaseEnv) waitLockWaiters(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var c int
		if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM pg_stat_activity
		        WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&c); err != nil {
			t.Fatalf("read lock waiters: %v", err)
		}
		if c >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d backends waiting on a lock, want >= %d", c, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// sideTx opens a transaction that stands in for another writer, locking the row `sql` selects.
func (e leaseEnv) sideTx(t *testing.T, lockSQL string, args ...any) pgx.Tx {
	t.Helper()
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatalf("begin side tx: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(e.ctx) })
	if _, err := tx.Exec(e.ctx, lockSQL, args...); err != nil {
		t.Fatalf("side tx lock %q: %v", lockSQL, err)
	}
	return tx
}

type stateResult struct {
	run     store.Run
	applied bool
	err     error
}

// reportAsync sends the worker's terminal report in a goroutine.
func (e leaseEnv) reportAsync(svc *Service, workerID, runID uuid.UUID, req StateRequest) <-chan stateResult {
	out := make(chan stateResult, 1)
	wkr := e.workerRow2(workerID)
	go func() {
		run, applied, err := svc.SetState(e.ctx, wkr, runID, req)
		out <- stateResult{run, applied, err}
	}()
	return out
}

// workerRow2 is workerRow without *testing.T (for goroutine setup that already proved the row).
func (e leaseEnv) workerRow2(id uuid.UUID) store.Worker {
	w, err := e.q.GetWorkerByID(e.ctx, id)
	if err != nil {
		panic(fmt.Sprintf("GetWorkerByID %s: %v", id, err))
	}
	return w
}

func awaitState(t *testing.T, ch <-chan stateResult) stateResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(30 * time.Second):
		t.Fatal("the terminal report never returned (deadlock or stuck lock)")
		return stateResult{}
	}
}

// completeReq is the worker's completion report: the generation fence stamp, and the branch the
// run worked (so the lease identity is agent/issue-<iid>). head is only read on the interlocked path.
func completeReq(iid, gen int64) StateRequest {
	return StateRequest{State: "completed", ClaimGeneration: &gen, Branch: strPtr(agentIssueBranch(iid)), Head: strPtr("0123abcd")}
}

// grantPermit issues the completion permit an interlocked run's completion consumes.
func (e leaseEnv) grantPermit(t *testing.T, svc *Service, workerID, runID uuid.UUID, iid int64) {
	t.Helper()
	res, err := svc.RequestCompletionPermit(e.ctx, e.workerRow(t, workerID), runID,
		CompletionPermitRequest{ContractRevision: 1, Branch: agentIssueBranch(iid), Head: "0123abcd"})
	if err != nil || !res.Granted {
		t.Fatalf("permit not granted: res=%+v err=%v", res, err)
	}
}

// ---- (a)/(a2) terminal completion vs the reaper, custody release included -----------------------

// TestEphemeralLeaseTerminalVsReaperLiveDB (a, a2): a completed report on a leased-eligible worker
// holds the worker and run locks with the terminal write, the in-tx custody release and the lease
// entry all done but uncommitted. The reaper runs in that window, then the report commits. The
// worker must survive with a live lease and a released hold, through BOTH terminal transactions
// (the generation fence tx for a legacy-contract run, the permit tx for an interlocked run).
// Moving the lease entry or the custody release back to post-commit leaves the worker to the
// teardown, which deletes it.
func TestEphemeralLeaseTerminalVsReaperLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name        string
		interlocked bool
	}{
		{"fence tx (legacy contract)", false},
		{"permit tx (interlocked)", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newLeaseEnv(t)
			gate := newGate(t, e.pool)
			svc := e.service(2*time.Hour, gate)
			w, run, iid := e.seedBound(t, "running", 1, tc.interlocked)
			hold := e.openHold(t, run, 1, w)
			if tc.interlocked {
				e.grantPermit(t, svc, w, run, iid)
			}

			gate.arm()
			res := e.reportAsync(svc, w, run, completeReq(iid, 1))
			gate.waitReached(t)

			// The reaper runs while the terminal tx is parked at COMMIT. It must not delete the worker.
			e.reap(t, 2*time.Hour)
			if !e.workerExists(t, w) {
				t.Fatal("reaper deleted the worker while the terminal report held it")
			}
			gate.open()
			if r := awaitState(t, res); r.err != nil || !r.applied {
				t.Fatalf("SetState completed: applied=%v err=%v", r.applied, r.err)
			}

			if !e.workerExists(t, w) {
				t.Fatal("the worker was deleted although its terminal transaction should have entered the lease")
			}
			if !e.leased(t, w) {
				t.Fatal("no lease on the worker after the completed report")
			}
			if got := e.workerRow(t, w).LeaseBranch.String; got != agentIssueBranch(iid) {
				t.Fatalf("lease_branch = %q, want %q", got, agentIssueBranch(iid))
			}
			if s := e.holdState(t, hold); s != "released" {
				t.Fatalf("custody hold = %q, want released (in the terminal transaction)", s)
			}
			if s := e.runStatusOf(t, run); s != "completed" {
				t.Fatalf("run = %q, want completed", s)
			}
			// A live lease keeps the worker through later reaper ticks too.
			e.reap(t, 2*time.Hour)
			if !e.workerExists(t, w) {
				t.Fatal("a later reaper tick deleted a live-leased worker")
			}
		})
	}
}

// ---- (a3) the in-tx release blocks behind a recovery Reserve hold lock ---------------------------

// TestEphemeralLeaseReleaseWaitsForReserveLiveDB (a3, and the reaper-in-the-window variant of a):
// recovery Reserve locks the hold row FOR UPDATE and never touches runs or workers. The terminal
// transaction's in-tx release waits behind it, with the terminal write done and uncommitted. The
// reaper runs in that window and must not delete the worker; when Reserve commits the release
// completes: no deadlock, no lost lease, no lost hold.
func TestEphemeralLeaseReleaseWaitsForReserveLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name        string
		interlocked bool
	}{
		{"fence tx (legacy contract)", false},
		{"permit tx (interlocked)", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newLeaseEnv(t)
			svc := e.service(2*time.Hour, e.pool)
			w, run, iid := e.seedBound(t, "running", 1, tc.interlocked)
			hold := e.openHold(t, run, 1, w)
			if tc.interlocked {
				e.grantPermit(t, svc, w, run, iid)
			}

			// What recovery.Reserve does first: lock the exact open hold FOR UPDATE.
			reserve := e.sideTx(t, `SELECT id FROM recovery_custody_holds WHERE id = $1 FOR UPDATE`, hold)
			res := e.reportAsync(svc, w, run, completeReq(iid, 1))
			e.waitLockWaiters(t, 1) // the in-tx release is blocked on the hold row

			e.reap(t, 2*time.Hour)
			if !e.workerExists(t, w) {
				t.Fatal("reaper deleted the worker while the terminal tx was open")
			}
			select {
			case r := <-res:
				t.Fatalf("the report returned while Reserve held the hold lock: %+v", r)
			case <-time.After(300 * time.Millisecond):
			}
			if err := reserve.Commit(e.ctx); err != nil {
				t.Fatalf("commit reserve tx: %v", err)
			}
			if r := awaitState(t, res); r.err != nil || !r.applied {
				t.Fatalf("SetState completed after Reserve: applied=%v err=%v", r.applied, r.err)
			}
			if !e.workerExists(t, w) || !e.leased(t, w) {
				t.Fatalf("worker exists=%v leased: the lease was lost behind the Reserve lock", e.workerExists(t, w))
			}
			if s := e.holdState(t, hold); s != "released" {
				t.Fatalf("hold = %q, want released", s)
			}
		})
	}
}

// TestEphemeralLeaseFailedInTxReleaseFallsBackLiveDB: when the in-tx custody release errors, the
// savepoint rolls back, no lease is entered, the completion still commits, and the post-commit
// release and teardown run as before. With the release failing both times the hold stays open and
// the teardown's custody skip keeps the worker; the reaper (the variant where it runs between the
// committed terminal and the hold release) leaves it too, and once the hold is released the worker
// is reaped exactly as it is today.
func TestEphemeralLeaseFailedInTxReleaseFallsBackLiveDB(t *testing.T) {
	e := newLeaseEnv(t)
	svc := e.service(2*time.Hour, e.pool)
	w, run, iid := e.seedBound(t, "running", 1, false)
	hold := e.openHold(t, run, 1, w)

	suffix := hold.String()[:8]
	e.exec(fmt.Sprintf(`CREATE OR REPLACE FUNCTION u2a_block_release_%[1]s() RETURNS trigger AS $$
	  BEGIN
	    IF NEW.id = '%[2]s'::uuid AND NEW.state = 'released' THEN RAISE EXCEPTION 'u2a: release blocked'; END IF;
	    RETURN NEW;
	  END $$ LANGUAGE plpgsql`, suffix, hold))
	e.exec(fmt.Sprintf(`CREATE TRIGGER u2a_block_release_%[1]s BEFORE UPDATE ON recovery_custody_holds
	  FOR EACH ROW EXECUTE FUNCTION u2a_block_release_%[1]s()`, suffix))
	dropped := false
	drop := func() {
		if dropped {
			return
		}
		dropped = true
		e.exec(fmt.Sprintf(`DROP TRIGGER u2a_block_release_%[1]s ON recovery_custody_holds`, suffix))
		e.exec(fmt.Sprintf(`DROP FUNCTION u2a_block_release_%[1]s()`, suffix))
	}
	t.Cleanup(drop)

	r := awaitState(t, e.reportAsync(svc, w, run, completeReq(iid, 1)))
	if r.err != nil || !r.applied || r.run.Status != "completed" {
		t.Fatalf("a failing in-tx release must not fail the completion: applied=%v status=%q err=%v", r.applied, r.run.Status, r.err)
	}
	if !e.workerExists(t, w) {
		t.Fatal("worker deleted although its custody hold is still open")
	}
	if e.leased(t, w) {
		t.Fatal("a lease was entered although the in-tx release failed")
	}
	if s := e.holdState(t, hold); s != "open" {
		t.Fatalf("hold = %q, want open (the release failed)", s)
	}
	// The reaper between the committed terminal and the hold release: the open hold protects it.
	e.reap(t, 2*time.Hour)
	if !e.workerExists(t, w) {
		t.Fatal("reaper deleted a custody-held worker")
	}
	// Once the hold is released there is no lease to protect it: today's behaviour.
	drop()
	if _, err := e.q.ReleaseCustodyHoldExact(e.ctx, store.ReleaseCustodyHoldExactParams{
		RunID: run, Generation: 1, WorkerID: w, ReleaseEvidence: pgtype.Text{String: "publication", Valid: true},
	}); err != nil {
		t.Fatalf("release hold: %v", err)
	}
	e.reap(t, 2*time.Hour)
	if e.workerExists(t, w) {
		t.Fatal("an unleased, released worker must be reaped as today")
	}
}

// ---- lock order: worker before run ----------------------------------------------------------------

// TestEphemeralLeaseTerminalLocksWorkerBeforeRunLiveDB: a claim from the same worker locks the
// worker row and then the runs its snapshot lists (LockOwnedRunsByIDs). The terminal report must
// therefore take the worker lock BEFORE the run lock; with the run first, the report holds the run
// while it waits for the lease's worker update and the claim holds the worker while it waits for the
// run: a deadlock (40P01). The side transaction plays the claim holding the worker lock; the report
// parks on it without touching the run, so the claim's run lock succeeds.
func TestEphemeralLeaseTerminalLocksWorkerBeforeRunLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name        string
		interlocked bool
	}{
		{"fence tx (legacy contract)", false},
		{"permit tx (interlocked)", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newLeaseEnv(t)
			svc := e.service(2*time.Hour, e.pool)
			w, run, iid := e.seedBound(t, "running", 1, tc.interlocked)
			if tc.interlocked {
				e.grantPermit(t, svc, w, run, iid)
			}
			claim := e.sideTx(t, `SELECT id FROM workers WHERE id = $1 FOR UPDATE`, w)
			res := e.reportAsync(svc, w, run, completeReq(iid, 1))
			e.waitLockWaiters(t, 1) // the report is parked on the worker row

			ctx, cancel := context.WithTimeout(e.ctx, 15*time.Second)
			defer cancel()
			if _, err := claim.Exec(ctx, `SELECT id FROM runs WHERE id = $1 FOR UPDATE`, run); err != nil {
				t.Fatalf("the claim's run lock failed (deadlock between worker->run and run->worker): %v", err)
			}
			if err := claim.Commit(e.ctx); err != nil {
				t.Fatalf("commit claim tx: %v", err)
			}
			if r := awaitState(t, res); r.err != nil || !r.applied {
				t.Fatalf("SetState completed: applied=%v err=%v", r.applied, r.err)
			}
			if !e.leased(t, w) {
				t.Fatal("no lease after the report waited for the worker lock")
			}
		})
	}
}

// ---- lease entry through setState ---------------------------------------------------------------

// TestSetStateEnterLeaseLiveDB: a worker-reported completion enters the lease through both terminal
// transactions at lease > 0, and at lease 0 the path is today's (the post-commit release and the
// teardown delete the worker). A worker-reported cancelled and a nil-generation report get no
// lease, a fenced failed run that holds custody keeps the hold and gets none, and a fenced failed
// run with no hold leases.
func TestSetStateEnterLeaseLiveDB(t *testing.T) {
	type tc struct {
		name        string
		lease       time.Duration
		interlocked bool
		hold        bool
		req         func(iid int64) StateRequest
		wantLease   bool
		wantWorker  bool // the worker row survives
		wantHold    string
	}
	failed := func(iid int64) StateRequest {
		gen := int64(1)
		return StateRequest{State: "failed", ClaimGeneration: &gen}
	}
	nilGen := func(iid int64) StateRequest {
		return StateRequest{State: "completed", Branch: strPtr(agentIssueBranch(iid))}
	}
	nilGenPermit := func(iid int64) StateRequest {
		return StateRequest{State: "completed", Branch: strPtr(agentIssueBranch(iid)), Head: strPtr("0123abcd")}
	}
	cancelled := func(iid int64) StateRequest {
		gen := int64(1)
		return StateRequest{State: "cancelled", ClaimGeneration: &gen}
	}
	cases := []tc{
		{"legacy completed, lease on", 2 * time.Hour, false, true, func(i int64) StateRequest { return completeReq(i, 1) }, true, true, "released"},
		{"interlocked completed, lease on", 2 * time.Hour, true, true, func(i int64) StateRequest { return completeReq(i, 1) }, true, true, "released"},
		{"legacy completed, lease off", 0, false, true, func(i int64) StateRequest { return completeReq(i, 1) }, false, false, "released"},
		{"interlocked completed, lease off", 0, true, true, func(i int64) StateRequest { return completeReq(i, 1) }, false, false, "released"},
		{"nil-generation completed gets no lease", 2 * time.Hour, false, true, nilGen, false, false, "released"},
		{"interlocked nil-generation completed gets no lease", 2 * time.Hour, true, true, nilGenPermit, false, false, "released"},
		{"failed with an open hold keeps the hold, no lease", 2 * time.Hour, false, true, failed, false, true, "open"},
		{"failed without a hold leases", 2 * time.Hour, false, false, failed, true, true, ""},
		{"worker-reported cancelled gets no lease", 2 * time.Hour, false, true, cancelled, false, true, "open"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newLeaseEnv(t)
			svc := e.service(c.lease, e.pool)
			w, run, iid := e.seedBound(t, "running", 1, c.interlocked)
			var hold uuid.UUID
			if c.hold {
				hold = e.openHold(t, run, 1, w)
			}
			if c.interlocked && c.req(iid).State == "completed" {
				e.grantPermit(t, svc, w, run, iid)
			}
			// The cancelled report is not a state a live worker can send: the call may be refused;
			// what matters is that it never leases.
			r := awaitState(t, e.reportAsync(svc, w, run, c.req(iid)))
			if c.req(iid).State != "cancelled" && r.err != nil {
				t.Fatalf("SetState %s: %v", c.req(iid).State, r.err)
			}
			exists := e.workerExists(t, w)
			if exists != c.wantWorker {
				t.Fatalf("worker exists = %v, want %v", exists, c.wantWorker)
			}
			if exists {
				if got := e.leased(t, w); got != c.wantLease {
					t.Fatalf("leased = %v, want %v", got, c.wantLease)
				}
			}
			if c.hold {
				if got := e.holdState(t, hold); got != c.wantHold {
					t.Fatalf("hold = %q, want %q", got, c.wantHold)
				}
			}
		})
	}
}

// ---- claim through the lease ----------------------------------------------------------------------

// seedLeasedWorker creates an ephemeral worker that served a completed issue run on `iid` (branch
// NULL: identity agent/issue-<iid>) and entered its lease through the real EnterEphemeralLease.
func (e leaseEnv) seedLeasedWorker(t *testing.T) (workerID, served uuid.UUID, iid int64) {
	t.Helper()
	workerID, served, iid = e.seedBound(t, "completed", 1, false)
	n, err := e.q.EnterEphemeralLease(e.ctx, store.EnterEphemeralLeaseParams{WorkerID: workerID, RunID: served})
	if err != nil || n != 1 {
		t.Fatalf("EnterEphemeralLease = (%d, %v), want (1, nil)", n, err)
	}
	return workerID, served, iid
}

// seedFollowUp inserts a queued issue run on the same repo and owner. iid 0 allocates a fresh one;
// branch non-empty sets runs.branch (it must be the canonical branch of that same iid, or the run has
// no lease identity: the lease branch is derived from issue_iid, never taken from a reported branch).
func (e leaseEnv) seedFollowUp(t *testing.T, iid int64, branch string) uuid.UUID {
	t.Helper()
	if iid == 0 {
		iid = nextLeaseIID()
	}
	id := uuid.New()
	var br any
	if branch != "" {
		br = branch
	}
	e.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, branch)
	        VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'queued', $5)`, id, e.userID, e.repoID, iid, br)
	return id
}

// claimAsync runs Claim for the worker with a fresh claim snapshot at `epoch` listing `listed`.
func (e leaseEnv) claimAsync(svc *Service, workerID uuid.UUID, epoch int64, listed ...uuid.UUID) <-chan claimResult {
	out := make(chan claimResult, 1)
	wkr := e.workerRow2(workerID)
	snap := &ActiveSnapshot{SnapshotEpoch: epoch, RegisterNonce: "nonce-A", Active: []ActiveRunEntry{}}
	for _, r := range listed {
		snap.Active = append(snap.Active, entry(r, 1, "running", false))
	}
	go func() {
		p, err := svc.Claim(e.ctx, wkr, snap)
		out <- claimResult{p, err}
	}()
	return out
}

type claimResult struct {
	payload *ClaimPayload
	err     error
}

func awaitClaim(t *testing.T, ch <-chan claimResult) claimResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(30 * time.Second):
		t.Fatal("the claim never returned")
		return claimResult{}
	}
}

// probe records the lease-claim outcomes the service reports to its test hook.
type leaseProbe struct{ calls, admitted, rebound atomic.Int32 }

func (p *leaseProbe) hook(admitted, rebound bool) {
	p.calls.Add(1)
	if admitted {
		p.admitted.Add(1)
	}
	if rebound {
		p.rebound.Add(1)
	}
}

// TestClaimThroughLeaseRebindsLiveDB: a leased worker claims a same-owner, same-repo, same-branch
// follow-up; the claim rebinds the worker to the new run and ends the lease in the same transaction.
func TestClaimThroughLeaseRebindsLiveDB(t *testing.T) {
	e := newLeaseEnv(t)
	svc := e.service(2*time.Hour, e.pool)
	w, served, iid := e.seedLeasedWorker(t)
	follow := e.seedFollowUp(t, iid, agentIssueBranch(iid))

	r := awaitClaim(t, e.claimAsync(svc, w, 1))
	if r.err != nil || r.payload == nil || r.payload.RunID != follow.String() {
		t.Fatalf("claim = (%+v, %v), want the follow-up %s", r.payload, r.err, follow)
	}
	if got := e.boundRun(t, w); !got.Valid || got.Bytes != follow || got.Bytes == served {
		t.Fatalf("worker bound to %v, want the follow-up %s", got, follow)
	}
	if e.leased(t, w) {
		t.Fatal("the lease must end when the worker is rebound")
	}
	if s := e.runStatusOf(t, follow); s != "claimed" {
		t.Fatalf("follow-up = %q, want claimed", s)
	}
}

// TestClaimIssueRerunCreatedThroughServiceLiveDB: an issue re-run created through the real create
// service has runs.branch NULL; its effective identity (agent/issue-<iid>) matches the lease of the
// worker that served the earlier run on that issue's branch, so the leased worker claims it.
func TestClaimIssueRerunCreatedThroughServiceLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, repoID := seedClaudeOnlyOriginUser(t, env)
	enableClaimAssembly(t, env, userID)
	e := leaseEnv{codexTestEnv: env, userID: userID, repoID: repoID}
	svc := e.service(2*time.Hour, e.pool)

	const n int64 = 20061
	// The earlier run on issue n completed on agent/issue-n and its worker leased.
	w, served, _ := e.seedBound(t, "completed", 1, false)
	e.exec(`UPDATE runs SET issue_iid = $2, branch = $3 WHERE id = $1`, served, n, agentIssueBranch(n))
	if c, err := e.q.EnterEphemeralLease(e.ctx, store.EnterEphemeralLeaseParams{WorkerID: w, RunID: served}); err != nil || c != 1 {
		t.Fatalf("EnterEphemeralLease = (%d, %v)", c, err)
	}

	seedEligibleIssue(t, env, repoID, n)
	rerun, err := svc.CreateRun(env.ctx, userID, repoID, n, "desc", nil, nil, true /*force*/, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if rerun.Branch.Valid {
		t.Fatalf("a created issue run keeps runs.branch NULL, got %q", rerun.Branch.String)
	}
	r := awaitClaim(t, e.claimAsync(svc, w, 1))
	if r.err != nil || r.payload == nil || r.payload.RunID != rerun.ID.String() {
		t.Fatalf("claim = (%+v, %v), want the re-run %s", r.payload, r.err, rerun.ID)
	}
	if got := e.boundRun(t, w); !got.Valid || got.Bytes != rerun.ID {
		t.Fatalf("worker bound to %v, want the re-run", got)
	}
}

// TestClaimLeaseOffNeverClaimsForeignLiveDB: with the lease at 0 a worker that still carries lease
// columns (a lowered setting) claims nothing beyond its bound run.
func TestClaimLeaseOffNeverClaimsForeignLiveDB(t *testing.T) {
	e := newLeaseEnv(t)
	svc := e.service(0, e.pool)
	w, _, iid := e.seedLeasedWorker(t)
	follow := e.seedFollowUp(t, iid, agentIssueBranch(iid))
	r := awaitClaim(t, e.claimAsync(svc, w, 1))
	if r.err != nil || r.payload != nil {
		t.Fatalf("claim = (%+v, %v), want idle with the lease off", r.payload, r.err)
	}
	if s := e.runStatusOf(t, follow); s != "queued" {
		t.Fatalf("follow-up = %q, want queued", s)
	}
}

// TestClaimTwoFollowUpsForOneLeasedWorkerLiveDB (b): two follow-ups are queued for one leased
// worker, and two claims for it race. The first claim holds the worker lock through its rebind; the
// second waits on that lock, then re-reads the worker: the lease is gone, so it claims nothing.
// Exactly one follow-up is claimed. Claiming off the stale pre-lock row (not the re-read one) would
// admit the second through a lease that no longer exists, which the probe reports.
func TestClaimTwoFollowUpsForOneLeasedWorkerLiveDB(t *testing.T) {
	e := newLeaseEnv(t)
	gate := newGate(t, e.pool)
	svc := e.service(2*time.Hour, gate)
	probe := &leaseProbe{}
	svc.leaseClaimProbe = probe.hook
	w, served, iid := e.seedLeasedWorker(t)
	r1 := e.seedFollowUp(t, iid, agentIssueBranch(iid))
	// uq_runs_one_active_per_issue allows one active issue run per issue, so the second lease-eligible
	// follow-up is an mr_rework on the same branch (its identity is its pipeline_ref).
	r2 := uuid.New()
	e.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_title, issue_description, status, pipeline_ref, target_run_id, mr_iid)
	        VALUES ($1, $2, $3, 'mr_rework', 't', 'd', 'queued', $4, $5, $6)`,
		r2, e.userID, e.repoID, agentIssueBranch(iid), served, nextLeaseIID()+2000)

	gate.arm()
	first := e.claimAsync(svc, w, 1)
	gate.waitReached(t)
	second := e.claimAsync(svc, w, 2)
	e.waitLockWaiters(t, 1) // the second claim waits for the worker row
	gate.open()

	a, b := awaitClaim(t, first), awaitClaim(t, second)
	if a.err != nil || b.err != nil {
		t.Fatalf("claims errored: %v / %v", a.err, b.err)
	}
	claimed := 0
	for _, p := range []*ClaimPayload{a.payload, b.payload} {
		if p != nil {
			claimed++
		}
	}
	if claimed != 1 {
		t.Fatalf("%d follow-ups claimed, want exactly 1 (first=%v second=%v)", claimed, a.payload, b.payload)
	}
	if a.payload == nil || a.payload.RunID != r1.String() {
		t.Fatalf("first claim = %+v, want the oldest follow-up %s", a.payload, r1)
	}
	if s := e.runStatusOf(t, r2); s != "queued" {
		t.Fatalf("second follow-up = %q, want queued", s)
	}
	if got := probe.admitted.Load(); got != 1 {
		t.Fatalf("lease admissions = %d, want 1 (the second claim must not see a lease)", got)
	}
}

// TestClaimVsCordonLiveDB (e): a cordon ends the lease. (1) The cordon holds the worker row while a
// claim, whose worker row was read BEFORE the cordon, waits for it; when the cordon commits the
// claim re-reads the row under its lock, sees no lease and claims nothing (the probe never fires: the
// stale row's lease columns are not used). (2) The claim holds the worker lock through its rebind;
// the cordon waits and then applies to the rebound worker.
func TestClaimVsCordonLiveDB(t *testing.T) {
	t.Run("cordon first", func(t *testing.T) {
		e := newLeaseEnv(t)
		svc := e.service(2*time.Hour, e.pool)
		probe := &leaseProbe{}
		svc.leaseClaimProbe = probe.hook
		w, _, iid := e.seedLeasedWorker(t)
		follow := e.seedFollowUp(t, iid, agentIssueBranch(iid))

		cordon := e.sideTx(t, `UPDATE workers SET draining_since = COALESCE(draining_since, now()),
		        lease_since = NULL, lease_repo_id = NULL, lease_branch = NULL WHERE id = $1`, w)
		res := e.claimAsync(svc, w, 1) // worker row read now: still carries the lease columns
		e.waitLockWaiters(t, 1)
		if err := cordon.Commit(e.ctx); err != nil {
			t.Fatalf("commit cordon: %v", err)
		}
		r := awaitClaim(t, res)
		if r.err != nil || r.payload != nil {
			t.Fatalf("claim = (%+v, %v), want idle after the cordon ended the lease", r.payload, r.err)
		}
		if probe.calls.Load() != 0 {
			t.Fatal("the claim used the stale lease columns instead of the re-read row")
		}
		if s := e.runStatusOf(t, follow); s != "queued" {
			t.Fatalf("follow-up = %q, want queued", s)
		}
	})
	t.Run("claim first", func(t *testing.T) {
		e := newLeaseEnv(t)
		gate := newGate(t, e.pool)
		svc := e.service(2*time.Hour, gate)
		w, _, iid := e.seedLeasedWorker(t)
		follow := e.seedFollowUp(t, iid, agentIssueBranch(iid))

		gate.arm()
		res := e.claimAsync(svc, w, 1)
		gate.waitReached(t)
		cordonDone := make(chan error, 1)
		go func() {
			_, err := e.q.CordonHostedWorker(e.ctx, w)
			cordonDone <- err
		}()
		e.waitLockWaiters(t, 1)
		gate.open()
		r := awaitClaim(t, res)
		if r.err != nil || r.payload == nil || r.payload.RunID != follow.String() {
			t.Fatalf("claim = (%+v, %v), want the follow-up", r.payload, r.err)
		}
		if err := <-cordonDone; err != nil {
			t.Fatalf("cordon: %v", err)
		}
		got := e.workerRow(t, w)
		if !got.DrainingSince.Valid || got.LeaseSince.Valid || got.EphemeralRunID.Bytes != follow {
			t.Fatalf("worker after claim+cordon = draining %v lease %v bound %v, want draining, no lease, bound to the follow-up",
				got.DrainingSince.Valid, got.LeaseSince.Valid, got.EphemeralRunID)
		}
	})
}

// TestClaimVsDeleteEphemeralWorkerForRunLiveDB (f): a teardown of the run the leased worker served
// is issued while a claim has the worker's rebind done but uncommitted, and again after it commits.
// The live lease makes the first delete match nothing (it never reaches the row lock), and the
// rebind moves the binding off the served run so the second matches nothing either; the worker
// survives bound to the claimed run. Without the rebind it would stay bound to the served run, which
// the assertion on the binding catches.
func TestClaimVsDeleteEphemeralWorkerForRunLiveDB(t *testing.T) {
	e := newLeaseEnv(t)
	gate := newGate(t, e.pool)
	svc := e.service(2*time.Hour, gate)
	w, served, iid := e.seedLeasedWorker(t)
	follow := e.seedFollowUp(t, iid, agentIssueBranch(iid))
	del := func() int64 {
		n, err := e.q.DeleteEphemeralWorkerForRun(e.ctx, store.DeleteEphemeralWorkerForRunParams{
			RunID: served, EphemeralLease: LeaseInterval(svc.ephemeralLease)})
		if err != nil {
			t.Fatalf("DeleteEphemeralWorkerForRun: %v", err)
		}
		return n
	}

	gate.arm()
	res := e.claimAsync(svc, w, 1)
	gate.waitReached(t)
	if n := del(); n != 0 {
		t.Fatalf("teardown during the claim deleted %d workers, want 0 (live lease)", n)
	}
	gate.open()
	r := awaitClaim(t, res)
	if r.err != nil || r.payload == nil || r.payload.RunID != follow.String() {
		t.Fatalf("claim = (%+v, %v), want the follow-up", r.payload, r.err)
	}
	if n := del(); n != 0 {
		t.Fatalf("teardown after the claim deleted %d workers, want 0 (rebound)", n)
	}
	if !e.workerExists(t, w) {
		t.Fatal("the worker was deleted although it had been rebound to the follow-up")
	}
	if got := e.boundRun(t, w); !got.Valid || got.Bytes != follow {
		t.Fatalf("worker bound to %v, want the follow-up %s", got, follow)
	}
}

// ---- fresh admission clock ----------------------------------------------------------------------

// The fresh-clock tests below need the lease to expire WHILE the claim is blocked on a lock, and the
// claim's earlier clock reads to be provably before that expiry. Sleeping for a fixed span after
// "a lock waiter exists" proves neither: the claim's transaction can begin late, and a pre-expired
// fixture would let a stale-clock bug pass. So the lease is armed from ONE database clock read, with
// expiry a known positive window after it, and the test waits on the database's own clock until that
// expiry has passed before releasing the lock. When the claim blocks on the WORKER lock, the arming
// runs inside the holder's transaction after the claim is observed blocked; when it blocks on a RUN
// lock (the claim then holds the worker row), the arming runs before the holder and the claim start,
// and the blocked statement's query_start is asserted to be before the expiry.

// workerLockWindow is how long after arming the lease expires when the claim is blocked on the WORKER
// lock. The claim's transaction began before the arming (the arming runs inside the lock holder's
// transaction, after the claim is seen blocked), so its transaction-start now() is before any
// expiry for every positive window; the value only has to be positive and is kept small to stay fast.
const workerLockWindow = 250 * time.Millisecond

// runLockWindow is how long after arming the lease expires when the claim is blocked on a RUN lock.
// An early admission clock (the bug this case catches) would be read right after the worker lock, so
// the window must cover the span from arming to the claim reaching LockWorkerRecoveryParents; that
// is asserted (the blocked statement's query_start must be before the expiry) rather than assumed.
const runLockWindow = time.Second

// leaseClockCase seeds a worker with a FRESH lease (never pre-expired: a lease that already expired
// would be refused by every clock, so it could not tell a stale clock from a fresh one) and a queued
// same-branch follow-up. The test then arms the expiry with armLeaseExpiry.
func (e leaseEnv) leaseClockCase(t *testing.T) (w, served uuid.UUID, follow uuid.UUID) {
	t.Helper()
	w, served, iid := e.seedLeasedWorker(t)
	return w, served, e.seedFollowUp(t, iid, agentIssueBranch(iid))
}

// rowQuerier is the QueryRow seam shared by a pool (autocommit) and a transaction.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// armLeaseExpiry rewrites the worker's lease_since from a single database clock read c so the lease
// expires exactly `window` after c (lease_since = c - (lease - window)), and returns c and the
// resulting expiry. window must be positive, so the lease is never pre-expired when armed.
func (e leaseEnv) armLeaseExpiry(t *testing.T, q rowQuerier, w uuid.UUID, lease, window time.Duration) (armedAt, expiry time.Time) {
	t.Helper()
	if window <= 0 || window >= lease {
		t.Fatalf("lease expiry window %v must be in (0, %v)", window, lease)
	}
	err := q.QueryRow(e.ctx, `WITH c AS (SELECT clock_timestamp() AS t)
	        UPDATE workers SET lease_since = c.t - $2::interval FROM c WHERE workers.id = $1
	        RETURNING c.t, workers.lease_since + $3::interval`,
		w, LeaseInterval(lease-window), LeaseInterval(lease)).Scan(&armedAt, &expiry)
	if err != nil {
		t.Fatalf("arm lease expiry (exactly one worker row expected): %v", err)
	}
	return armedAt, expiry
}

// lockHolder is sideTx that also reports its backend pid, so a waiter can be identified by the lock
// it waits behind (pg_blocking_pids).
func (e leaseEnv) lockHolder(t *testing.T, lockSQL string, args ...any) (pgx.Tx, int32) {
	t.Helper()
	tx := e.sideTx(t, lockSQL, args...)
	var pid int32
	if err := tx.QueryRow(e.ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read holder backend pid: %v", err)
	}
	return tx, pid
}

// waitBlockedBy waits until exactly one backend is blocked by holderPid and its current statement is
// the sqlc query `stmt` (sqlc keeps "-- name: <stmt> :kind" at the start of the query text). It
// returns that backend's transaction start and the start of the blocked statement. More than one
// blocked backend, or a different blocked statement that never gives way, is fatal.
func (e leaseEnv) waitBlockedBy(t *testing.T, holderPid int32, stmt string) (xactStart, queryStart time.Time) {
	t.Helper()
	marker := "name: " + stmt
	deadline := time.Now().Add(20 * time.Second)
	last := "no backend blocked by the holder"
	for {
		rows, err := e.pool.Query(e.ctx, `SELECT pid, xact_start, query_start, query FROM pg_stat_activity
		        WHERE $1 = ANY(pg_blocking_pids(pid))`, holderPid)
		if err != nil {
			t.Fatalf("read blocked backends: %v", err)
		}
		type blocked struct {
			pid         int32
			xact, query *time.Time
			text        string
		}
		var got []blocked
		for rows.Next() {
			var b blocked
			if err := rows.Scan(&b.pid, &b.xact, &b.query, &b.text); err != nil {
				rows.Close()
				t.Fatalf("scan blocked backend: %v", err)
			}
			got = append(got, b)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("read blocked backends: %v", err)
		}
		if len(got) > 1 {
			t.Fatalf("%d backends blocked by holder pid %d, want exactly 1", len(got), holderPid)
		}
		if len(got) == 1 {
			b := got[0]
			if strings.Contains(b.text, marker) && b.xact != nil && b.query != nil {
				return *b.xact, *b.query
			}
			last = fmt.Sprintf("blocked backend %d runs %q", b.pid, b.text)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no backend blocked by holder pid %d on %q: %s", holderPid, stmt, last)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// awaitDBClockPast waits until the database's clock_timestamp() is at or past expiry, comparing on the
// server, and returns the first server clock reading that satisfied it. That reading is what a
// claim running afterwards can only exceed.
func (e leaseEnv) awaitDBClockPast(t *testing.T, expiry time.Time) time.Time {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var now time.Time
		var past bool
		if err := e.pool.QueryRow(e.ctx, `SELECT t, t >= $1 FROM (SELECT clock_timestamp() AS t) c`, expiry).Scan(&now, &past); err != nil {
			t.Fatalf("read database clock: %v", err)
		}
		if past {
			return now
		}
		if time.Now().After(deadline) {
			t.Fatalf("database clock %v never reached lease expiry %v", now, expiry)
		}
		wait := expiry.Sub(now) + 10*time.Millisecond
		if wait < 5*time.Millisecond {
			wait = 5 * time.Millisecond
		}
		time.Sleep(wait)
	}
}

// assertBefore fails the test when an earlier clock reading is not strictly before the lease expiry:
// then the fixture could not tell a stale clock from a fresh one.
func assertBefore(t *testing.T, what string, initial, expiry time.Time) {
	t.Helper()
	if !initial.Before(expiry) {
		t.Fatalf("fixture cannot discriminate: %s %v is not before lease expiry %v (window too short)", what, initial, expiry)
	}
}

// logClockOrdering records the ordering the test established, for diagnosing a window failure.
func logClockOrdering(t *testing.T, what string, initial, expiry, fresh time.Time) {
	t.Helper()
	const f = time.RFC3339Nano
	t.Logf("clock ordering: %s %s < expiry %s <= fresh %s", what, initial.Format(f), expiry.Format(f), fresh.Format(f))
}

// TestClaimStaleTxStartClockLiveDB (g): the claim transaction begins while the lease is live and
// blocks on the worker row; the lease then expires, and only afterwards does the claim proceed.
// Admission uses a clock read after the wait, so the follow-up is not claimed and the lease is
// never applied. Reading the transaction-start now() instead admits the follow-up through the
// expired lease (the probe reports it), and only the rebind's own clock then refuses.
//
// The barrier: the claim is seen blocked on GetWorkerForUpdate behind the holder (so its transaction
// has started), the lease is armed inside the holder's transaction to expire workerLockWindow later,
// the claim's pg_stat_activity xact_start is asserted before that expiry, and the holder commits only
// once the database clock has passed the expiry.
func TestClaimStaleTxStartClockLiveDB(t *testing.T) {
	const lease = 5 * time.Second
	e := newLeaseEnv(t)
	svc := e.service(lease, e.pool)
	probe := &leaseProbe{}
	svc.leaseClaimProbe = probe.hook
	w, served, follow := e.leaseClockCase(t)

	holder, pid := e.lockHolder(t, `SELECT id FROM workers WHERE id = $1 FOR UPDATE`, w)
	res := e.claimAsync(svc, w, 1)
	xactStart, _ := e.waitBlockedBy(t, pid, "GetWorkerForUpdate")
	_, expiry := e.armLeaseExpiry(t, holder, w, lease, workerLockWindow)
	assertBefore(t, "claim xact_start (its transaction-start now())", xactStart, expiry)
	fresh := e.awaitDBClockPast(t, expiry) // the lease expires while the claim waits
	logClockOrdering(t, "claim xact_start", xactStart, expiry, fresh)
	if err := holder.Commit(e.ctx); err != nil {
		t.Fatalf("commit holder: %v", err)
	}
	r := awaitClaim(t, res)
	if r.err != nil || r.payload != nil {
		t.Fatalf("claim = (%+v, %v), want idle: the lease expired", r.payload, r.err)
	}
	if probe.admitted.Load() != 0 {
		t.Fatal("the claim was admitted through an expired lease (a stale transaction-start clock)")
	}
	if s := e.runStatusOf(t, follow); s != "queued" {
		t.Fatalf("follow-up = %q, want queued", s)
	}
	if got := e.boundRun(t, w); got.Bytes != served {
		t.Fatalf("worker rebound to %v through an expired lease", got)
	}
}

// TestClaimRunLockWaitCrossesExpiryLiveDB (g2): the claim takes the worker lock while the lease is
// live, then waits on a run lock a side transaction holds (LockWorkerRecoveryParents on a run its
// snapshot lists) across the lease's expiry. (1) Admission reads its clock AFTER that wait, so it
// does not admit; reading it right after the worker lock admits through a lease that has since
// expired. (2) With admission forced by pinning @lease_at to the arming instant (before the expiry,
// the test hook), the rebind's own clock refuses: the claim rolls back to its savepoint, the run
// stays queued with no hold, and the worker reports idle. A rebind that reused the admission
// instant would apply.
//
// The barrier: the lease is armed (expiry runLockWindow after one database clock read) right before
// the run lock is taken; the claim is seen blocked on LockWorkerRecoveryParents behind it, and that
// statement's query_start (after the worker lock and any clock read taken right after it) is asserted
// before the expiry; the run-lock holder commits only once the database clock has passed the expiry.
func TestClaimRunLockWaitCrossesExpiryLiveDB(t *testing.T) {
	const lease = 5 * time.Second
	run := func(t *testing.T, pin bool) {
		e := newLeaseEnv(t)
		svc := e.service(lease, e.pool)
		probe := &leaseProbe{}
		svc.leaseClaimProbe = probe.hook
		w, served, follow := e.leaseClockCase(t)

		armedAt, expiry := e.armLeaseExpiry(t, e.pool, w, lease, runLockWindow)
		if pin {
			svc.leaseAtOverride = pgtype.Timestamptz{Time: armedAt, Valid: true} // pre-expiry
		}
		runLock, pid := e.lockHolder(t, `SELECT id FROM runs WHERE id = $1 FOR UPDATE`, served)
		res := e.claimAsync(svc, w, 1, served)                                // the snapshot lists the worker's served run
		_, queryStart := e.waitBlockedBy(t, pid, "LockWorkerRecoveryParents") // worker lock held, run lock awaited
		assertBefore(t, "blocked run-lock statement query_start (after any clock read taken right after the worker lock)", queryStart, expiry)
		if pin {
			assertBefore(t, "pinned @lease_at", armedAt, expiry)
		}
		fresh := e.awaitDBClockPast(t, expiry)
		logClockOrdering(t, "run-lock query_start", queryStart, expiry, fresh)
		if err := runLock.Commit(e.ctx); err != nil {
			t.Fatalf("commit run-lock tx: %v", err)
		}
		r := awaitClaim(t, res)
		if r.err != nil || r.payload != nil {
			t.Fatalf("claim = (%+v, %v), want idle", r.payload, r.err)
		}
		if pin {
			if probe.admitted.Load() != 1 || probe.rebound.Load() != 0 {
				t.Fatalf("pinned admission: admitted=%d rebound=%d, want 1 and 0 (the rebind's own clock must refuse)",
					probe.admitted.Load(), probe.rebound.Load())
			}
		} else if probe.admitted.Load() != 0 {
			t.Fatal("admitted through a lease that expired during the run-lock wait (admission clock read too early)")
		}
		if s := e.runStatusOf(t, follow); s != "queued" {
			t.Fatalf("follow-up = %q, want queued", s)
		}
		var holds int
		if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM recovery_custody_holds WHERE run_id = $1`, follow).Scan(&holds); err != nil || holds != 0 {
			t.Fatalf("custody holds for the refused claim = %d (%v), want 0", holds, err)
		}
		if got := e.boundRun(t, w); got.Bytes != served {
			t.Fatalf("worker rebound to %v after a refused claim", got)
		}
		// The snapshot replacement is committed even though the claim was refused.
		if got := e.workerRow(t, w).SnapshotEpoch; got != 1 {
			t.Fatalf("snapshot_epoch = %d, want 1 (the replacement must persist)", got)
		}
	}
	t.Run("admission clock", func(t *testing.T) { run(t, false) })
	t.Run("rebind clock", func(t *testing.T) { run(t, true) })
}
