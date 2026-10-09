package workersvc

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PRD #2006 review rework: the no-snapshot claim path, the rebind's unique-violation branch, the
// atomicity of the terminal lease entry for a run with no custody hold, and the final-status guard.
// Same harness and conventions as ephemeral_lease_livedb_test.go.

// claimNoSnapshotAsync runs Claim with NO active-run snapshot (UZI_ACTIVE_SNAPSHOT_DISABLED, or an
// agent that has not negotiated active_run_snapshot).
func (e leaseEnv) claimNoSnapshotAsync(svc *Service, workerID uuid.UUID) <-chan claimResult {
	out := make(chan claimResult, 1)
	wkr := e.workerRow2(workerID)
	go func() {
		p, err := svc.Claim(e.ctx, wkr, nil)
		out <- claimResult{p, err}
	}()
	return out
}

// TestClaimNoSnapshotThroughLeaseRebindsLiveDB: a leased worker whose claim carries no snapshot
// still claims its same-branch follow-up and is rebound, ending the lease. Through the old
// no-lease auto-commit path the follow-up sits queued for the whole lease.
func TestClaimNoSnapshotThroughLeaseRebindsLiveDB(t *testing.T) {
	e := newLeaseEnv(t)
	svc := e.service(2*time.Hour, e.pool)
	w, served, iid := e.seedLeasedWorker(t)
	follow := e.seedFollowUp(t, iid, agentIssueBranch(iid))

	r := awaitClaim(t, e.claimNoSnapshotAsync(svc, w))
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

// TestClaimNoSnapshotWorkerDeletedIsIdleLiveDB: the worker row is deleted (reaper) after the
// caller loaded it and before the claim locks it. The no-snapshot lease path reports idle, as the
// old auto-commit ClaimRun path did, rather than a raw pgx.ErrNoRows (a 500 at the handler).
func TestClaimNoSnapshotWorkerDeletedIsIdleLiveDB(t *testing.T) {
	e := newLeaseEnv(t)
	svc := e.service(2*time.Hour, e.pool)
	w, _, _ := e.seedLeasedWorker(t)
	wkr := e.workerRow2(w)
	if _, err := e.pool.Exec(e.ctx, `DELETE FROM workers WHERE id = $1`, w); err != nil {
		t.Fatalf("delete worker: %v", err)
	}
	p, err := svc.Claim(e.ctx, wkr, nil)
	if err != nil || p != nil {
		t.Fatalf("claim = (%+v, %v), want idle (nil, nil)", p, err)
	}
}

// TestClaimNoSnapshotLeaseClocksLiveDB: the clock discipline holds on the no-snapshot path. The claim
// transaction begins while the lease is live and blocks on the worker row; the lease then expires
// (armed inside the lock holder's transaction, see TestClaimStaleTxStartClockLiveDB for the barrier)
// and only afterwards does the claim proceed.
//   - admission clock: admission reads a fresh clock after the wait, so nothing is admitted; the
//     probe still shows the lease path ran (one call, none admitted).
//   - rebind clock: with admission pinned to a pre-expiry database instant the claim is admitted,
//     and the rebind's own clock refuses: the claim rolls back to its savepoint, the follow-up stays
//     queued with no hold and the worker keeps its binding and reports idle.
func TestClaimNoSnapshotLeaseClocksLiveDB(t *testing.T) {
	const lease = 5 * time.Second
	run := func(t *testing.T, pin bool) {
		e := newLeaseEnv(t)
		svc := e.service(lease, e.pool)
		probe := &leaseProbe{}
		svc.leaseClaimProbe = probe.hook
		w, served, follow := e.leaseClockCase(t)

		var pinAt time.Time
		if pin {
			// A database instant taken before the claim starts, hence before the arming below.
			if err := e.pool.QueryRow(e.ctx, `SELECT clock_timestamp()`).Scan(&pinAt); err != nil {
				t.Fatalf("read database clock for the pin: %v", err)
			}
			svc.leaseAtOverride = pgtype.Timestamptz{Time: pinAt, Valid: true} // pre-expiry
		}
		holder, pid := e.lockHolder(t, `SELECT id FROM workers WHERE id = $1 FOR UPDATE`, w)
		res := e.claimNoSnapshotAsync(svc, w)
		xactStart, _ := e.waitBlockedBy(t, pid, "GetWorkerForUpdate")
		_, expiry := e.armLeaseExpiry(t, holder, w, lease, workerLockWindow)
		assertBefore(t, "claim xact_start (its transaction-start now())", xactStart, expiry)
		if pin {
			assertBefore(t, "pinned @lease_at", pinAt, expiry)
		}
		fresh := e.awaitDBClockPast(t, expiry) // the lease expires while the claim waits
		logClockOrdering(t, "claim xact_start", xactStart, expiry, fresh)
		if err := holder.Commit(e.ctx); err != nil {
			t.Fatalf("commit holder: %v", err)
		}
		r := awaitClaim(t, res)
		if r.err != nil || r.payload != nil {
			t.Fatalf("claim = (%+v, %v), want idle: the lease expired", r.payload, r.err)
		}
		if probe.calls.Load() != 1 {
			t.Fatalf("lease-claim outcomes reported = %d, want 1 (the claim must run through the lease path)", probe.calls.Load())
		}
		if pin {
			if probe.admitted.Load() != 1 || probe.rebound.Load() != 0 {
				t.Fatalf("pinned admission: admitted=%d rebound=%d, want 1 and 0 (the rebind's own clock must refuse)",
					probe.admitted.Load(), probe.rebound.Load())
			}
		} else if probe.admitted.Load() != 0 {
			t.Fatal("admitted through a lease that expired while the claim waited (stale admission clock)")
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
	}
	t.Run("admission clock", func(t *testing.T) { run(t, false) })
	t.Run("rebind clock", func(t *testing.T) { run(t, true) })
}

// TestClaimRebindUniqueViolationLiveDB (N2): another ephemeral worker is already bound to the
// follow-up run (the provisioner won the race after ClaimRun read). The leased worker's claim is admitted but its
// rebind hits uq_workers_ephemeral_run: the claim rolls back to its savepoint, the worker reports
// idle (no error), keeps its own binding and lease, and the run stays queued. Both claim paths.
func TestClaimRebindUniqueViolationLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name  string
		claim func(e leaseEnv, svc *Service, w uuid.UUID) <-chan claimResult
	}{
		{"with snapshot", func(e leaseEnv, svc *Service, w uuid.UUID) <-chan claimResult { return e.claimAsync(svc, w, 1) }},
		{"no snapshot", func(e leaseEnv, svc *Service, w uuid.UUID) <-chan claimResult { return e.claimNoSnapshotAsync(svc, w) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newLeaseEnv(t)
			svc := e.service(2*time.Hour, e.pool)
			probe := &leaseProbe{}
			svc.leaseClaimProbe = probe.hook
			w, served, iid := e.seedLeasedWorker(t)
			follow := e.seedFollowUp(t, iid, agentIssueBranch(iid))
			// The provisioner commits another ephemeral worker bound to the follow-up after ClaimRun's
			// statement snapshot but before the rebind (ClaimRun skips a run another ephemeral worker
			// is bound to, so a binding committed earlier would have excluded the run). The rival's
			// own insert takes a FOR KEY SHARE lock on the run through the runs foreign key, which
			// would wait on this claim's lock on the run, so the hook inserts with foreign-key
			// triggers off (session_replication_role = replica, the throwaway database's superuser):
			// it stands in for the insert having committed before the claim locked the run.
			svc.leaseClaimWindow = func() {
				rival := uuid.New()
				conn, err := e.pool.Acquire(e.ctx)
				if err != nil {
					panic(err)
				}
				defer conn.Release()
				if _, err := conn.Exec(e.ctx, `SET session_replication_role = replica`); err != nil {
					panic(err)
				}
				_, err = conn.Exec(e.ctx, `INSERT INTO workers (id, user_id, name, token_hash, template_declared, kind, hosted_size, docker_enabled, ephemeral,
				               ephemeral_run_id, status)
				        VALUES ($1, $2, $3, $4, 'base', 'hosted', 'm', false, true, $5, 'offline')`,
					rival, e.userID, "rival-"+rival.String()[:8], rival[:], follow)
				if _, rerr := conn.Exec(e.ctx, `RESET session_replication_role`); rerr != nil {
					panic(rerr)
				}
				if err != nil {
					panic(err)
				}
			}

			res := tc.claim(e, svc, w)
			r := awaitClaim(t, res)
			if r.err != nil || r.payload != nil {
				t.Fatalf("claim = (%+v, %v), want idle with no error", r.payload, r.err)
			}
			if probe.admitted.Load() != 1 || probe.rebound.Load() != 0 {
				t.Fatalf("admitted=%d rebound=%d, want 1 and 0 (admitted, rebind refused)", probe.admitted.Load(), probe.rebound.Load())
			}
			if s := e.runStatusOf(t, follow); s != "queued" {
				t.Fatalf("follow-up = %q, want queued", s)
			}
			if got := e.boundRun(t, w); got.Bytes != served {
				t.Fatalf("worker bound to %v, want its served run %s", got, served)
			}
			if !e.leased(t, w) {
				t.Fatal("the lease must survive a refused rebind")
			}
			var holds int
			if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM recovery_custody_holds WHERE run_id = $1`, follow).Scan(&holds); err != nil || holds != 0 {
				t.Fatalf("custody holds for the refused claim = %d (%v), want 0", holds, err)
			}
		})
	}
}

// afterFirstBeginGate wraps the pool. Once armed, the FIRST Begin passes through (the terminal
// report's transaction); every later Begin parks before it starts, signalling reached, until
// released. A terminal report that opens a second transaction after the first committed (the
// lease entry moved post-commit) parks there, which is the window the test drives the reaper into.
// The correct code opens one transaction and never parks.
type afterFirstBeginGate struct {
	pool    *pgxpool.Pool
	armed   atomic.Bool
	begins  atomic.Int32
	reached chan struct{}
	release chan struct{}
}

func newAfterFirstBeginGate(t *testing.T, pool *pgxpool.Pool) *afterFirstBeginGate {
	g := &afterFirstBeginGate{pool: pool, reached: make(chan struct{}, 8), release: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-g.release:
		default:
			close(g.release)
		}
	})
	return g
}

func (g *afterFirstBeginGate) Begin(ctx context.Context) (pgx.Tx, error) {
	if g.armed.Load() && g.begins.Add(1) > 1 {
		g.reached <- struct{}{}
		select {
		case <-g.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return g.pool.Begin(ctx)
}

// TestEphemeralLeaseTerminalAtomicNoHoldLiveDB (S1, plan test a): a completed run that has NO
// custody hold (a claim that was not recovery-capable) is made terminal, and the lease must be
// entered in the SAME transaction. With no hold, nothing else keeps the worker alive across a
// split: the moment the terminal state commits without the lease, the reaper deletes the worker.
// The test reports completion and, whenever the report opens a transaction after the first one
// (the lease entry moved to its own, post-commit transaction), runs the reaper in that window.
// The worker must still exist and hold its lease afterwards; a two-transaction implementation
// loses it. Both terminal transactions: the generation fence tx (legacy contract) and the permit
// tx (interlocked).
func TestEphemeralLeaseTerminalAtomicNoHoldLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name        string
		interlocked bool
	}{
		{"fence tx (legacy contract)", false},
		{"permit tx (interlocked)", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newLeaseEnv(t)
			gate := newAfterFirstBeginGate(t, e.pool)
			svc := e.service(2*time.Hour, gate)
			w, run, iid := e.seedBound(t, "running", 1, tc.interlocked)
			if tc.interlocked {
				e.grantPermit(t, svc, w, run, iid)
			}
			var holds int
			if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM recovery_custody_holds WHERE run_id = $1`, run).Scan(&holds); err != nil || holds != 0 {
				t.Fatalf("fixture must have no custody hold: %d (%v)", holds, err)
			}

			gate.armed.Store(true)
			res := e.reportAsync(svc, w, run, completeReq(iid, 1))
			var r stateResult
			select {
			case <-gate.reached:
				// The report committed its terminal state and is about to open another transaction.
				// The run is terminal, unleased and holdless: the reaper takes the worker here.
				e.reap(t, 2*time.Hour)
				close(gate.release)
				r = awaitState(t, res)
			case r = <-res:
			case <-time.After(30 * time.Second):
				t.Fatal("the terminal report neither finished nor opened a second transaction")
			}
			if r.err != nil {
				t.Fatalf("SetState completed: %v", r.err)
			}
			if s := e.runStatusOf(t, run); s != "completed" {
				t.Fatalf("run = %q, want completed", s)
			}
			if !e.workerExists(t, w) {
				t.Fatal("the worker was deleted between the terminal commit and the lease entry: the lease must be entered in the terminal transaction")
			}
			if !e.leased(t, w) {
				t.Fatal("no lease on the worker after the completed report")
			}
		})
	}
}

// TestEphemeralLeaseOnlyCompletedOrFailedFinalStatusLiveDB: a `failed` report that setState routes
// to cancelled (an operator stop or cancel pre-stamped on the run) ends the run `cancelled`. The
// lease is keyed on the run's FINAL status, so a worker-reported-then-routed cancel gets no lease
// even with no custody hold open. A plan-rejected run is not in this set: it ends failed
// (SetRunFailedPlanRejected) and may lease.
func TestEphemeralLeaseOnlyCompletedOrFailedFinalStatusLiveDB(t *testing.T) {
	for _, stopKind := range []string{"cancelled", "stopped"} {
		t.Run(stopKind, func(t *testing.T) {
			e := newLeaseEnv(t)
			svc := e.service(2*time.Hour, e.pool)
			w, run, _ := e.seedBound(t, "running", 1, false)
			e.exec(`UPDATE runs SET stop_kind = $2 WHERE id = $1`, run, stopKind)

			gen := int64(1)
			r := awaitState(t, e.reportAsync(svc, w, run, StateRequest{State: "failed", ClaimGeneration: &gen}))
			if r.err != nil {
				t.Fatalf("SetState failed: %v", r.err)
			}
			if s := e.runStatusOf(t, run); s != "cancelled" {
				t.Fatalf("run = %q, want cancelled (routed by stop_kind %s)", s, stopKind)
			}
			if e.workerExists(t, w) && e.leased(t, w) {
				t.Fatal("a run that ended cancelled must not enter a lease")
			}
		})
	}
}
