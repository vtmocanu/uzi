package handler

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/hostedsvc"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #2006 (the ephemeral worker lease), the hostedsvc half, against a REAL Postgres: the
// provisioner's quota eviction of a leased-idle worker, the reaper's lease handling through
// ReapPass, and two forced interleavings of a claim transaction against the reaper and against
// eviction. It lives in the handler package for the same reason as
// ephemeral_provisioner_livedb_test.go (the store-IT harness only sweeps store and handler).
//
// The claim transaction is SIMULATED with the store queries workersvc.Claim runs, in its order:
// BEGIN, GetWorkerForUpdate, LeaseClockNow, ClaimRun with the lease params, RebindLeasedEphemeralWorker,
// then COMMIT later. Each forced test confirms its ordering by observation (a call that must not
// wait returns promptly) rather than by sleeping and hoping.

// leaseProvisioner builds the real provisioner with the lease configured.
func (fx *ephemeralFixture) leaseProvisioner(maxPerUser int, lease time.Duration) *hostedsvc.EphemeralProvisioner {
	sc := settings.New(&settingsStore{rows: []store.AppSetting{
		{Key: settings.KeyEphemeralWorkersEnabled, Value: "true"},
	}}, time.Minute)
	return hostedsvc.NewEphemeralProvisioner(fx.pool, fx.q, fx.box, sc, hostedsvc.EphemeralConfig{
		MaxPerUser:  maxPerUser,
		DefaultSize: "m",
		Lease:       lease,
	})
}

// leasedIdle is an ephemeral worker that served `served` and entered its lease through the real
// EnterEphemeralLease query.
type leasedIdle struct {
	id     uuid.UUID
	served uuid.UUID
	iid    int64
}

// leasedWorker seeds a leased-idle worker: a completed issue run (iid fx.nextIID()) it served, the
// ephemeral worker bound to it (online, fresh heartbeat), and the lease entered with lease_since
// backdated by leaseAge.
func (fx *ephemeralFixture) leasedWorker(leaseAge time.Duration) leasedIdle {
	fx.t.Helper()
	iid := fx.nextIID()
	served := uuid.New()
	if _, err := fx.pool.Exec(fx.ctx,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
		 VALUES ($1, $2, $3, $4, 't', 'd', 'completed')`, served, fx.userID, fx.repoID, iid); err != nil {
		fx.t.Fatalf("seed served run: %v", err)
	}
	a, b := uuid.New(), uuid.New()
	var w uuid.UUID
	if err := fx.pool.QueryRow(fx.ctx,
		`INSERT INTO workers (user_id, name, token_hash, template_declared, kind, hosted_size, docker_enabled,
		                      ephemeral, ephemeral_run_id, status, last_heartbeat_at, online_since, max_concurrent_runs)
		 VALUES ($1, $2, $3, 'base', 'hosted', 'm', true, true, $4, 'online', now(), now(), 2) RETURNING id`,
		fx.userID, "ephemeral-"+served.String(), append(a[:], b[:]...), served).Scan(&w); err != nil {
		fx.t.Fatalf("seed ephemeral worker: %v", err)
	}
	if _, err := fx.pool.Exec(fx.ctx, `UPDATE runs SET worker_id = $2 WHERE id = $1`, served, w); err != nil {
		fx.t.Fatalf("point served run at worker: %v", err)
	}
	if n, err := fx.q.EnterEphemeralLease(fx.ctx, store.EnterEphemeralLeaseParams{WorkerID: w, RunID: served}); err != nil || n != 1 {
		fx.t.Fatalf("EnterEphemeralLease = (%d, %v), want (1, nil)", n, err)
	}
	if leaseAge > 0 {
		if _, err := fx.pool.Exec(fx.ctx,
			`UPDATE workers SET lease_since = now() - $2::interval WHERE id = $1`, w, fmt.Sprintf("%d microseconds", leaseAge.Microseconds())); err != nil {
			fx.t.Fatalf("backdate lease: %v", err)
		}
	}
	return leasedIdle{id: w, served: served, iid: iid}
}

// followUp inserts a queued issue run on the same repo and issue as the worker's served run: the
// effective branch identity (agent/issue-<iid>) equals the lease's, so the lease admits it.
func (fx *ephemeralFixture) followUp(w leasedIdle) uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	if _, err := fx.pool.Exec(fx.ctx,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
		 VALUES ($1, $2, $3, $4, 't', 'd', 'queued')`, id, fx.userID, fx.repoID, w.iid); err != nil {
		fx.t.Fatalf("seed follow-up run: %v", err)
	}
	return id
}

func (fx *ephemeralFixture) workerExists(id uuid.UUID) bool {
	fx.t.Helper()
	var n int
	if err := fx.pool.QueryRow(fx.ctx, `SELECT count(*) FROM workers WHERE id = $1`, id).Scan(&n); err != nil {
		fx.t.Fatalf("count worker: %v", err)
	}
	return n == 1
}

func (fx *ephemeralFixture) openHold(w leasedIdle) {
	fx.t.Helper()
	if _, err := fx.pool.Exec(fx.ctx,
		`INSERT INTO recovery_custody_holds
		   (id, user_id, repo_id, run_id, generation, state, original_worker_id, original_worker_identity, live_worker_id, live_run_id)
		 VALUES ($1, $2, $3, $4, 1, 'open', $5, 'ident', $6, $4)`,
		uuid.New(), fx.userID, fx.repoID, w.served, uuid.New(), w.id); err != nil {
		fx.t.Fatalf("seed open custody hold: %v", err)
	}
}

// busyRunOn attaches a running run to the worker (the busy guard keys on runs.worker_id).
func (fx *ephemeralFixture) busyRunOn(w leasedIdle) {
	fx.t.Helper()
	if _, err := fx.pool.Exec(fx.ctx,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id)
		 VALUES ($1, $2, $3, $4, 't', 'd', 'running', $5)`, uuid.New(), fx.userID, fx.repoID, fx.nextIID(), w.id); err != nil {
		fx.t.Fatalf("seed busy run: %v", err)
	}
}

func (fx *ephemeralFixture) runState(id uuid.UUID) (status string, worker pgtype.UUID) {
	fx.t.Helper()
	if err := fx.pool.QueryRow(fx.ctx, `SELECT status, worker_id FROM runs WHERE id = $1`, id).Scan(&status, &worker); err != nil {
		fx.t.Fatalf("read run: %v", err)
	}
	return status, worker
}

// ---------------------------------------------------------------------------------------------
// Provisioning: quota eviction of a leased-idle worker.
// ---------------------------------------------------------------------------------------------

// TestEphemeralLeaseProvisionEvictsOldestLiveDB: an owner at the cap whose cap is held only by
// leased-idle workers gets the OLDEST lease evicted and a new worker provisioned for the queued
// run that no leased worker may claim (acceptance example 3).
func TestEphemeralLeaseProvisionEvictsOldestLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, true)
	young := fx.leasedWorker(10 * time.Minute)
	oldest := fx.leasedWorker(90 * time.Minute)
	// A queued run on ANOTHER issue: no leased worker's branch identity matches, so the leases
	// cannot serve it and it needs a new worker.
	x := fx.queuedRun([]string{"docker"})

	if _, err := fx.leaseProvisioner(2, 2*time.Hour).ProvisionPass(fx.ctx); err != nil {
		t.Fatalf("ProvisionPass: %v", err)
	}
	if fx.workerExists(oldest.id) {
		t.Fatal("the oldest leased-idle worker was not evicted")
	}
	if !fx.workerExists(young.id) {
		t.Fatal("the younger leased-idle worker was evicted; only the oldest may be")
	}
	rows := fx.ephemeralRows()
	if len(rows) != 2 {
		t.Fatalf("ephemeral rows = %d, want 2 (the surviving lease and the new worker)", len(rows))
	}
	var bound bool
	for _, r := range rows {
		if r.runID == x {
			bound = true
		}
	}
	if !bound {
		t.Fatalf("no ephemeral worker bound to the queued run %s: %+v", x, rows)
	}
}

// TestEphemeralLeaseProvisionOverCapRefusesLiveDB: an owner OVER the cap (the cap was lowered
// across a restart: 3 leased-idle workers, cap 2) is refused outright. Evicting one lease and
// inserting would leave the owner at 3 again, still over the cap, so nothing is evicted and
// nothing is inserted.
func TestEphemeralLeaseProvisionOverCapRefusesLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, true)
	a := fx.leasedWorker(30 * time.Minute)
	b := fx.leasedWorker(60 * time.Minute)
	c := fx.leasedWorker(90 * time.Minute)
	x := fx.queuedRun([]string{"docker"})

	created, err := fx.leaseProvisioner(2, 2*time.Hour).ProvisionPass(fx.ctx)
	if err != nil {
		t.Fatalf("ProvisionPass: %v", err)
	}
	if created != 0 {
		t.Fatalf("created = %d, want 0: an over-cap owner must not be provisioned", created)
	}
	for name, w := range map[string]leasedIdle{"a": a, "b": b, "c": c} {
		if !fx.workerExists(w.id) {
			t.Fatalf("leased worker %s was evicted although the provision was refused", name)
		}
	}
	for _, r := range fx.ephemeralRows() {
		if r.runID == x {
			t.Fatalf("an ephemeral worker was inserted for the queued run %s although the owner is over the cap", x)
		}
	}
	if n := len(fx.ephemeralRows()); n != 3 {
		t.Fatalf("ephemeral rows = %d, want 3", n)
	}
}

// TestEphemeralLeaseProvisionLeaseOffNeverEvictsLiveDB: with the lease off (Lease 0) an owner
// exactly at the cap is refused and nothing is evicted, even though releasable leased-idle rows
// exist (left over from before the lease was turned off).
func TestEphemeralLeaseProvisionLeaseOffNeverEvictsLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, true)
	a := fx.leasedWorker(30 * time.Minute)
	b := fx.leasedWorker(90 * time.Minute)
	x := fx.queuedRun([]string{"docker"})

	created, err := fx.leaseProvisioner(2, 0).ProvisionPass(fx.ctx)
	if err != nil {
		t.Fatalf("ProvisionPass: %v", err)
	}
	if created != 0 {
		t.Fatalf("created = %d, want 0: at the cap with the lease off nothing may be provisioned", created)
	}
	if !fx.workerExists(a.id) || !fx.workerExists(b.id) {
		t.Fatal("a leased-idle worker was evicted with the lease off")
	}
	for _, r := range fx.ephemeralRows() {
		if r.runID == x {
			t.Fatalf("an ephemeral worker was inserted for the queued run %s", x)
		}
	}
	if n := len(fx.ephemeralRows()); n != 2 {
		t.Fatalf("ephemeral rows = %d, want 2", n)
	}
}

// TestEphemeralLeaseProvisionNeverEvictsBusyOrHeldLiveDB: the eviction candidate is the oldest
// RELEASABLE lease. A busy leased worker and a custody-held leased worker, both older than the
// releasable one, are never evicted. With nothing releasable left the next run is refused.
func TestEphemeralLeaseProvisionNeverEvictsBusyOrHeldLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, true)
	busy := fx.leasedWorker(100 * time.Minute)
	fx.busyRunOn(busy)
	held := fx.leasedWorker(99 * time.Minute)
	fx.openHold(held)
	releasable := fx.leasedWorker(5 * time.Minute)
	fx.queuedRun([]string{"docker"})
	prov := fx.leaseProvisioner(3, 2*time.Hour)

	if _, err := prov.ProvisionPass(fx.ctx); err != nil {
		t.Fatalf("ProvisionPass: %v", err)
	}
	if fx.workerExists(releasable.id) {
		t.Fatal("the releasable leased worker was not evicted")
	}
	if !fx.workerExists(busy.id) || !fx.workerExists(held.id) {
		t.Fatalf("a busy (%v) or custody-held (%v) leased worker was evicted", fx.workerExists(busy.id), fx.workerExists(held.id))
	}
	if n := len(fx.ephemeralRows()); n != 3 {
		t.Fatalf("ephemeral rows = %d, want 3 (busy, held, new)", n)
	}

	// Nothing is releasable now (busy, held, and the new worker bound to a live queued run):
	// another unplaceable run is refused and nothing is evicted.
	fx.queuedRun([]string{"docker"})
	if _, err := prov.ProvisionPass(fx.ctx); err != nil {
		t.Fatalf("second ProvisionPass: %v", err)
	}
	if n := len(fx.ephemeralRows()); n != 3 {
		t.Fatalf("ephemeral rows after the refused pass = %d, want 3", n)
	}
	if !fx.workerExists(busy.id) || !fx.workerExists(held.id) {
		t.Fatal("a non-releasable leased worker was evicted by the refused pass")
	}
}

// ---------------------------------------------------------------------------------------------
// Reaper: lease handling through ReapPass.
// ---------------------------------------------------------------------------------------------

// TestEphemeralLeaseReapPassLiveDB: lease 0 reaps a finished worker at once, exactly as before
// the lease; with the lease configured a live lease is spared and an expired one is reaped.
func TestEphemeralLeaseReapPassLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, true)
	const deadline = 10 * time.Minute

	reap := func(lease time.Duration) {
		t.Helper()
		sc := settings.New(&settingsStore{}, time.Minute)
		p := hostedsvc.NewEphemeralProvisioner(fx.pool, fx.q, fx.box, sc, hostedsvc.EphemeralConfig{
			MaxPerUser: 2, DefaultSize: "m", ProvisionDeadline: deadline, Lease: lease,
		})
		if _, err := p.ReapPass(fx.ctx); err != nil {
			t.Fatalf("ReapPass: %v", err)
		}
	}

	// Lease 0 (disabled): a worker whose lease columns are set (a stale row from an earlier
	// lease-enabled boot) is reaped like any finished ephemeral worker.
	w0 := fx.leasedWorker(1 * time.Minute)
	reap(0)
	if fx.workerExists(w0.id) {
		t.Fatal("lease 0: a finished ephemeral worker survived the reaper")
	}

	// Lease 30m: a lease started 10m ago is live and spared; one started 31m ago is expired and reaped.
	live := fx.leasedWorker(10 * time.Minute)
	expired := fx.leasedWorker(31 * time.Minute)
	reap(30 * time.Minute)
	if !fx.workerExists(live.id) {
		t.Fatal("a live lease was reaped")
	}
	if fx.workerExists(expired.id) {
		t.Fatal("an expired lease was not reaped")
	}

	// The lease ends when the configured interval shrinks past the entry time (re-read per pass).
	reap(5 * time.Minute)
	if fx.workerExists(live.id) {
		t.Fatal("a lease older than the configured interval survived")
	}
}

// ---------------------------------------------------------------------------------------------
// Forced interleavings: a claim transaction held open while the reaper / provisioner run.
// ---------------------------------------------------------------------------------------------

// simClaim is the claim transaction of workersvc.Claim, driven step by step.
type simClaim struct {
	fx  *ephemeralFixture
	w   leasedIdle
	tx  pgx.Tx
	qtx *store.Queries
}

// beginClaim opens the transaction and takes the worker row lock (GetWorkerForUpdate), the first
// step of the snapshot claim path.
func (fx *ephemeralFixture) beginClaim(w leasedIdle) *simClaim {
	fx.t.Helper()
	tx, err := fx.pool.Begin(fx.ctx)
	if err != nil {
		fx.t.Fatalf("begin claim tx: %v", err)
	}
	fx.t.Cleanup(func() { _ = tx.Rollback(fx.ctx) })
	c := &simClaim{fx: fx, w: w, tx: tx, qtx: fx.q.WithTx(tx)}
	if _, err := c.qtx.GetWorkerForUpdate(fx.ctx, w.id); err != nil {
		fx.t.Fatalf("GetWorkerForUpdate: %v", err)
	}
	return c
}

func (c *simClaim) leaseClock() time.Time {
	c.fx.t.Helper()
	at, err := c.qtx.LeaseClockNow(c.fx.ctx)
	if err != nil {
		c.fx.t.Fatalf("LeaseClockNow: %v", err)
	}
	return at.Time
}

// claimThroughLease runs ClaimRun with the lease params read from the worker row (as Claim reads
// them from its re-read), admission instant leaseAt, then the rebind with rebindLease. It returns the
// claimed run id. A forced test passes a rebindLease wider than the claim's lease to stand in for a
// claim that won at the expiry edge: the claim half is not what these tests measure, the other
// transaction's handling of a worker that JUST rebound is.
func (c *simClaim) claimThroughLease(lease time.Duration, leaseAt time.Time, rebindLease time.Duration) uuid.UUID {
	c.fx.t.Helper()
	fx := c.fx
	row, err := c.qtx.GetWorkerByID(fx.ctx, c.w.id)
	if err != nil {
		fx.t.Fatalf("GetWorkerByID: %v", err)
	}
	run, err := c.qtx.ClaimRun(fx.ctx, store.ClaimRunParams{
		WorkerID:            pgtype.UUID{Bytes: c.w.id, Valid: true},
		UserID:              fx.userID,
		AffinityCutoff:      pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Minute), Valid: true},
		SpreadCutoff:        pgtype.Timestamptz{Time: time.Now().Add(-9 * time.Second), Valid: true},
		HeartbeatCutoff:     pgtype.Timestamptz{Time: time.Now().Add(-45 * time.Second), Valid: true},
		IsDockerWorker:      true,
		DockerRepoAllowlist: []uuid.UUID{fx.repoID},
		IsEphemeral:         true,
		EphemeralRunID:      pgtype.UUID{Bytes: c.w.served, Valid: true},
		LeaseSince:          row.LeaseSince,
		LeaseRepoID:         row.LeaseRepoID,
		LeaseBranch:         row.LeaseBranch,
		EphemeralLease:      pgtype.Interval{Microseconds: lease.Microseconds(), Valid: true},
		LeaseAt:             pgtype.Timestamptz{Time: leaseAt, Valid: true},
	})
	if err != nil {
		fx.t.Fatalf("ClaimRun through the lease: %v (the follow-up must be admitted)", err)
	}
	n, err := c.qtx.RebindLeasedEphemeralWorker(fx.ctx, store.RebindLeasedEphemeralWorkerParams{
		NewRunID:       run.ID,
		WorkerID:       c.w.id,
		OldRunID:       c.w.served,
		EphemeralLease: pgtype.Interval{Microseconds: rebindLease.Microseconds(), Valid: true},
	})
	if err != nil || n != 1 {
		fx.t.Fatalf("RebindLeasedEphemeralWorker = (%d, %v), want (1, nil)", n, err)
	}
	return run.ID
}

func (c *simClaim) commit() {
	c.fx.t.Helper()
	if err := c.tx.Commit(c.fx.ctx); err != nil {
		c.fx.t.Fatalf("commit claim tx: %v", err)
	}
}

type passResult struct {
	n   int64
	err error
}

// asyncPass runs fn on its own goroutine and returns its result channel.
func asyncPass(fn func() (int64, error)) <-chan passResult {
	ch := make(chan passResult, 1)
	go func() {
		n, err := fn()
		ch <- passResult{n, err}
	}()
	return ch
}

// returnsPromptly reports whether the pass finished within d. A pass that must NOT wait on a row
// the claim holds (the reaper and the eviction both lock with SKIP LOCKED) finishes in
// milliseconds; one that is still running after d is parked on the claim's lock.
func returnsPromptly(ch <-chan passResult, d time.Duration) (passResult, bool) {
	select {
	case r := <-ch:
		return r, true
	case <-time.After(d):
		return passResult{}, false
	}
}

// TestEphemeralLeaseReaperVsClaimLiveDB (PRD #2006, forced interleaving c): a claim transaction
// holds the worker row lock while the lease EXPIRES and the reaper runs, and the follow-up run is
// inserted AFTER the reaper's snapshot. The reaper must skip the locked worker (not wait on it, not
// delete it on its stale view), and the worker that just rebound must survive.
//
// Timeline: lease 2s, lease_since 1s ago. The claim takes the worker lock and reads the admission
// clock while the lease is live, then the lease expires; ReapPass starts (its snapshot has no
// follow-up and sees an expired lease); only then is the follow-up inserted, claimed through the
// lease at the pre-expiry admission instant, rebound, and committed.
//
// Mutations that must redden it (verified): drop FOR UPDATE SKIP LOCKED from the reaper's lock
// statement (the delete then blocks on the claim and deletes on its stale snapshot, or the pass
// parks behind the claim), or remove SKIP LOCKED alone (the pass parks behind the claim).
func TestEphemeralLeaseReaperVsClaimLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, true)
	const lease = 2 * time.Second
	w := fx.leasedWorker(1 * time.Second)

	c := fx.beginClaim(w)
	admitAt := c.leaseClock()
	time.Sleep(1200 * time.Millisecond) // the lease (2s, started 1s ago) is now expired

	prov := fx.leaseProvisioner(2, lease)
	reaped := asyncPass(func() (int64, error) { return prov.ReapPass(fx.ctx) })
	res, prompt := returnsPromptly(reaped, 2*time.Second)

	// The follow-up exists only after the reaper's snapshot.
	f := fx.followUp(w)
	claimed := c.claimThroughLease(lease, admitAt, 2*time.Hour)
	if claimed != f {
		t.Fatalf("claimed %s, want the follow-up %s", claimed, f)
	}
	c.commit()

	if !prompt {
		res = <-reaped
		t.Errorf("the reaper parked behind the claim's worker lock instead of skipping it (reaped %d, err %v)", res.n, res.err)
	}
	if res.err != nil {
		t.Fatalf("ReapPass: %v", res.err)
	}
	if !fx.workerExists(w.id) {
		t.Fatal("the reaper deleted a worker that just rebound to a follow-up run")
	}
	status, worker := fx.runState(f)
	if worker != (pgtype.UUID{Bytes: w.id, Valid: true}) || status == "queued" {
		t.Fatalf("follow-up run state = (%s, %v), want claimed by the surviving worker", status, worker)
	}

	// Once committed the worker is bound to the follow-up and busy: a later pass leaves it alone too.
	if _, err := prov.ReapPass(fx.ctx); err != nil {
		t.Fatalf("second ReapPass: %v", err)
	}
	if !fx.workerExists(w.id) {
		t.Fatal("a later reaper pass deleted the busy rebound worker")
	}
}

// TestEphemeralLeaseEvictionVsClaimLiveDB (PRD #2006, forced interleaving d): a claim transaction
// holds a leased worker's row lock (it has claimed the follow-up and rebound) while the provisioner
// is at the cap and wants to evict. Eviction must skip the locked worker (not wait, not delete it)
// and refuse the provision; the worker survives bound to the follow-up.
//
// Mutation that must redden it (verified): drop SKIP LOCKED from LockOldestReleasableLeasedEphemeralWorker
// (the provisioner then parks behind the claim).
func TestEphemeralLeaseEvictionVsClaimLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, true)
	w := fx.leasedWorker(10 * time.Minute)
	x := fx.queuedRun([]string{"docker"}) // another issue: only a new worker can serve it
	f := fx.followUp(w)

	c := fx.beginClaim(w)
	admitAt := c.leaseClock()
	if claimed := c.claimThroughLease(2*time.Hour, admitAt, 2*time.Hour); claimed != f {
		t.Fatalf("claimed %s, want the follow-up %s", claimed, f)
	}

	prov := fx.leaseProvisioner(1, 2*time.Hour)
	provisioned := asyncPass(func() (int64, error) { return prov.ProvisionPass(fx.ctx) })
	res, prompt := returnsPromptly(provisioned, 3*time.Second)
	c.commit()

	if !prompt {
		res = <-provisioned
		t.Errorf("the provisioner parked behind the claim's worker lock instead of skipping it (created %d, err %v)", res.n, res.err)
	}
	if res.err != nil {
		t.Fatalf("ProvisionPass: %v", res.err)
	}
	if !fx.workerExists(w.id) {
		t.Fatal("eviction deleted a worker that just claimed a follow-up")
	}
	for _, r := range fx.ephemeralRows() {
		if r.runID == x {
			t.Fatal("a worker was provisioned for the queued run although the only slot was held by the claiming worker")
		}
	}
	if status, worker := fx.runState(f); worker != (pgtype.UUID{Bytes: w.id, Valid: true}) || status == "queued" {
		t.Fatalf("follow-up run state = (%s, %v), want claimed by the surviving worker", status, worker)
	}

	// The worker is now busy with the follow-up, so it is not releasable: a later pass still refuses.
	if _, err := prov.ProvisionPass(fx.ctx); err != nil {
		t.Fatalf("second ProvisionPass: %v", err)
	}
	if !fx.workerExists(w.id) {
		t.Fatal("a later provision pass evicted the busy worker")
	}
}
