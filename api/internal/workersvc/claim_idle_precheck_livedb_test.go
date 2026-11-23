package workersvc

import (
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4602 M2: the idle precheck in claimCheckpointRun (HasQueuedRunForUser) must make an
// idle poll skip ClaimRun entirely, while ClaimRun stays the authoritative claim whenever a
// same-owner queued run exists. These tests count statements with a pgx QueryTracer on the
// pool the claim transaction runs on, so each assertion is an exact count, never a timing.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (run via
// ./e2e/run-store-it.sh).

// claimStmtTracer counts the two sqlc statements the idle precheck is about, keyed on the
// "-- name: X " header sqlc prepends to every query.
type claimStmtTracer struct {
	claimRun atomic.Int64
	precheck atomic.Int64
}

func (c *claimStmtTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	switch {
	case strings.HasPrefix(data.SQL, "-- name: ClaimRun "):
		c.claimRun.Add(1)
	case strings.HasPrefix(data.SQL, "-- name: HasQueuedRunForUser "):
		c.precheck.Add(1)
	}
	return ctx
}

func (c *claimStmtTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// newTracedClaimFixture is a custody-continuation fixture whose Service queries and claim
// transactions both run on a dedicated traced pool, so the tracer sees every statement
// svc.Claim issues. Seeding keeps using the fixture's untraced env.pool.
func newTracedClaimFixture(t *testing.T) (*custodyContinuationFixture, *claimStmtTracer) {
	t.Helper()
	env := setupCodexLiveDB(t)
	f := newCustodyContinuationFixture(t, env)
	tr := &claimStmtTracer{}
	pool, err := store.OpenPool(env.ctx, os.Getenv("UZI_TEST_DATABASE_URL"), store.WithQueryTracer(tr))
	if err != nil {
		t.Fatalf("open traced pool: %v", err)
	}
	t.Cleanup(pool.Close)
	svc := New(store.New(pool), env.box, testParams())
	svc.SetVault(env.vlt)
	svc.SetTxBeginner(pool)
	f.svc = svc
	return f, tr
}

func (f *custodyContinuationFixture) runStatus(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT status FROM runs WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatalf("read status: %v", err)
	}
	return s
}

// seedQueuedChatRun inserts a queued chat run for the owner (chat shape: no repo, iid or branch).
func (f *custodyContinuationFixture) seedQueuedChatRun() uuid.UUID {
	id := uuid.New()
	f.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
	            VALUES ($1, $2, NULL, 'chat', NULL, 't', 'd', 'queued')`, id, f.userID)
	return id
}

// An owner with no claimable queued run polls idle without ClaimRun ever executing, even
// when another owner has a claimable run and this owner has a queued chat run (the chat
// lane's, excluded from the run lane by the precheck exactly as ClaimRun excludes it).
func TestClaimIdlePrecheckSkipsClaimRunLiveDB(t *testing.T) {
	f, tr := newTracedClaimFixture(t)
	other := newCustodyContinuationFixture(t, f.env)
	otherRun := other.seedRun("queued", 0)
	chat := f.seedQueuedChatRun()

	const polls = 5
	for i := range polls {
		p, err := f.svc.Claim(f.env.ctx, f.wkr, nil)
		if err != nil {
			t.Fatalf("poll %d: Claim: %v", i, err)
		}
		if p != nil {
			t.Fatalf("poll %d: claimed run %s, want idle", i, p.RunID)
		}
	}
	if got := tr.claimRun.Load(); got != 0 {
		t.Fatalf("ClaimRun executions = %d, want 0 across %d idle polls", got, polls)
	}
	if got := tr.precheck.Load(); got != polls {
		t.Fatalf("HasQueuedRunForUser executions = %d, want %d (one per poll)", got, polls)
	}
	if s := f.runStatus(t, chat); s != "queued" {
		t.Fatalf("chat run status = %q, want queued", s)
	}
	if s := f.runStatus(t, otherRun); s != "queued" {
		t.Fatalf("other owner's run status = %q, want queued", s)
	}
}

// A same-owner queued run this worker cannot claim leaves the precheck true, so ClaimRun
// stays authoritative: it runs once per poll (no retries) and the run is left queued.
func TestClaimIdlePrecheckIneligibleRowStaysAuthoritativeLiveDB(t *testing.T) {
	f, tr := newTracedClaimFixture(t)
	run := f.seedRun("queued", 0)
	f.env.exec(`UPDATE runs SET required_capabilities = '{capability-the-worker-lacks}' WHERE id = $1`, run)

	const polls = 3
	for i := range polls {
		p, err := f.svc.Claim(f.env.ctx, f.wkr, nil)
		if err != nil {
			t.Fatalf("poll %d: Claim: %v", i, err)
		}
		if p != nil {
			t.Fatalf("poll %d: claimed run %s, want idle (worker lacks the capability)", i, p.RunID)
		}
	}
	if got := tr.claimRun.Load(); got != polls {
		t.Fatalf("ClaimRun executions = %d, want %d (one per poll)", got, polls)
	}
	if got := tr.precheck.Load(); got != polls {
		t.Fatalf("HasQueuedRunForUser executions = %d, want %d", got, polls)
	}
	if s := f.runStatus(t, run); s != "queued" {
		t.Fatalf("ineligible run status = %q, want queued", s)
	}
}

// A false precheck is not cached: after an idle poll, a run enqueued afterwards is claimed
// by the very next poll, with the worker set and the custody hold opened.
func TestClaimIdlePrecheckNoNegativeCachingLiveDB(t *testing.T) {
	f, tr := newTracedClaimFixture(t)

	p, err := f.svc.Claim(f.env.ctx, f.wkr, nil)
	if err != nil || p != nil {
		t.Fatalf("first poll = (%v, %v), want idle", p, err)
	}
	if got := tr.claimRun.Load(); got != 0 {
		t.Fatalf("ClaimRun executions after idle poll = %d, want 0", got)
	}

	run := f.seedRun("queued", 0)
	p, err = f.svc.Claim(f.env.ctx, f.wkr, nil)
	if err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if p == nil {
		t.Fatal("second poll idle, want the newly enqueued run claimed")
	}
	if p.RunID != run.String() {
		t.Fatalf("claimed run = %s, want %s", p.RunID, run)
	}
	if got := tr.claimRun.Load(); got != 1 {
		t.Fatalf("ClaimRun executions = %d, want 1", got)
	}
	if s := f.runStatus(t, run); s != "claimed" {
		t.Fatalf("status = %q, want claimed", s)
	}
	var workerID uuid.UUID
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT worker_id FROM runs WHERE id = $1`, run).Scan(&workerID); err != nil {
		t.Fatalf("read worker_id: %v", err)
	}
	if workerID != f.workerID {
		t.Fatalf("worker_id = %s, want %s", workerID, f.workerID)
	}
	if got := f.openHolds(t); got != 1 {
		t.Fatalf("open custody holds = %d, want 1", got)
	}
}

// Two of one owner's workers poll concurrently for a single queued claimable run: exactly
// one claims it and the other is idle. (The existing concurrent admission test claims two
// runs, so it does not cover one run and two claimants.)
func TestClaimIdlePrecheckConcurrentClaimantsLiveDB(t *testing.T) {
	// Calibration: the hand-built second worker, polling alone, claims a fresh queued run. Only
	// then does "exactly one of the two claims" below mean a real race rather than a second
	// worker that could never have claimed.
	t.Run("second worker alone claims", func(t *testing.T) {
		f, _ := newTracedClaimFixture(t)
		second := f.addSecondWorker()
		run := f.seedRun("queued", 0)
		p, err := f.svc.Claim(f.env.ctx, second, nil)
		if err != nil {
			t.Fatalf("second worker Claim: %v", err)
		}
		if p == nil || p.RunID != run.String() {
			t.Fatalf("second worker claim = %+v, want run %s", p, run)
		}
		if s := f.runStatus(t, run); s != "claimed" {
			t.Fatalf("status = %q, want claimed", s)
		}
	})

	t.Run("one run two claimants", func(t *testing.T) {
		f, _ := newTracedClaimFixture(t)
		run := f.seedRun("queued", 0)
		workers := []store.Worker{f.wkr, f.addSecondWorker()}

		type result struct {
			runID string
			err   error
		}
		results := make([]result, len(workers))
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i, w := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				p, err := f.svc.Claim(f.env.ctx, w, nil)
				results[i].err = err
				if p != nil {
					results[i].runID = p.RunID
				}
			}()
		}
		close(start)
		wg.Wait()

		claimed := 0
		for i, r := range results {
			if r.err != nil {
				t.Fatalf("worker %d: Claim: %v", i, r.err)
			}
			if r.runID != "" {
				claimed++
				if r.runID != run.String() {
					t.Fatalf("worker %d claimed %s, want %s", i, r.runID, run)
				}
			}
		}
		if claimed != 1 {
			t.Fatalf("claimants that got the run = %d, want exactly 1", claimed)
		}
		if s := f.runStatus(t, run); s != "claimed" {
			t.Fatalf("status = %q, want claimed", s)
		}
	})
}

// addSecondWorker inserts a second online worker for the fixture's owner, mirroring how the
// fixture's own worker is seeded, and returns its claim-time row.
func (f *custodyContinuationFixture) addSecondWorker() store.Worker {
	second := uuid.New()
	f.env.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at)
	            VALUES ($1, $2, 'second', $3, 'online', now())`, second, f.userID, second[:])
	f.env.exec(`UPDATE workers SET last_heartbeat_at = now() WHERE id = $1`, f.workerID)
	return store.Worker{ID: second, UserID: f.userID, Name: "second", Status: "online", ProtocolCapabilities: f.wkr.ProtocolCapabilities}
}

// newTracedLeaseEnv is a lease-env (2h lease) whose Service queries and claim transactions run on a
// traced pool, so the tracer sees every statement a Claim issues. Seeding keeps using e.pool.
func newTracedLeaseEnv(t *testing.T) (leaseEnv, *Service, *claimStmtTracer) {
	t.Helper()
	e := newLeaseEnv(t)
	tr := &claimStmtTracer{}
	pool, err := store.OpenPool(e.ctx, os.Getenv("UZI_TEST_DATABASE_URL"), store.WithQueryTracer(tr))
	if err != nil {
		t.Fatalf("open traced pool: %v", err)
	}
	t.Cleanup(pool.Close)
	svc := New(store.New(pool), e.box, testParams())
	svc.SetVault(e.vlt)
	svc.SetTxBeginner(pool)
	svc.SetBackground(func(func()) {})
	svc.SetEphemeralSettings(fakeEphemeralSettings{lease: 2 * time.Hour})
	return e, svc, tr
}

// The snapshot idle path with a false precheck: a valid snapshot is replaced in the claim
// transaction, ClaimRun never executes, and the replacement still commits.
func TestClaimIdlePrecheckSnapshotIdleCommitsReplaceLiveDB(t *testing.T) {
	e, svc, tr := newTracedLeaseEnv(t)
	w, served, _ := e.seedBound(t, "running", 1, false)

	r := awaitClaim(t, e.claimAsync(svc, w, 1, served))
	if r.err != nil || r.payload != nil {
		t.Fatalf("claim = (%+v, %v), want idle", r.payload, r.err)
	}
	if got := tr.claimRun.Load(); got != 0 {
		t.Fatalf("ClaimRun executions = %d, want 0", got)
	}
	if got := tr.precheck.Load(); got != 1 {
		t.Fatalf("HasQueuedRunForUser executions = %d, want 1", got)
	}
	if got := e.workerRow(t, w).SnapshotEpoch; got != 1 {
		t.Fatalf("snapshot_epoch = %d, want 1 (the replacement must persist)", got)
	}
	if s := e.runStatusOf(t, served); s != "running" {
		t.Fatalf("served run = %q, want running", s)
	}
}

// The leased idle path with a false precheck: no ClaimRun, the lease and binding are untouched,
// the claim reports lease-claim (false, false), and a lease-admissible run enqueued afterwards is
// claimed by the next poll.
func TestClaimIdlePrecheckLeasedIdleLiveDB(t *testing.T) {
	e, svc, tr := newTracedLeaseEnv(t)
	probe := &leaseProbe{}
	svc.leaseClaimProbe = probe.hook
	w, served, iid := e.seedLeasedWorker(t)
	before := e.workerRow(t, w)

	r := awaitClaim(t, e.claimAsync(svc, w, 1))
	if r.err != nil || r.payload != nil {
		t.Fatalf("claim = (%+v, %v), want idle", r.payload, r.err)
	}
	if got := tr.claimRun.Load(); got != 0 {
		t.Fatalf("ClaimRun executions = %d, want 0", got)
	}
	if got := tr.precheck.Load(); got != 1 {
		t.Fatalf("HasQueuedRunForUser executions = %d, want 1", got)
	}
	if probe.calls.Load() != 1 || probe.admitted.Load() != 0 || probe.rebound.Load() != 0 {
		t.Fatalf("lease probe calls=%d admitted=%d rebound=%d, want 1/0/0",
			probe.calls.Load(), probe.admitted.Load(), probe.rebound.Load())
	}
	after := e.workerRow(t, w)
	if !after.LeaseSince.Valid || after.LeaseSince != before.LeaseSince ||
		after.LeaseRepoID != before.LeaseRepoID || after.LeaseBranch != before.LeaseBranch {
		t.Fatalf("lease changed: before=(%v %v %v) after=(%v %v %v)",
			before.LeaseSince, before.LeaseRepoID, before.LeaseBranch,
			after.LeaseSince, after.LeaseRepoID, after.LeaseBranch)
	}
	if got := e.boundRun(t, w); got.Bytes != served {
		t.Fatalf("worker bound to %v after an idle claim, want %s", got, served)
	}
	if after.SnapshotEpoch != 1 {
		t.Fatalf("snapshot_epoch = %d, want 1 (the replacement must persist)", after.SnapshotEpoch)
	}

	follow := e.seedFollowUp(t, iid, agentIssueBranch(iid))
	r = awaitClaim(t, e.claimAsync(svc, w, 2))
	if r.err != nil || r.payload == nil || r.payload.RunID != follow.String() {
		t.Fatalf("next claim = (%+v, %v), want the follow-up %s", r.payload, r.err, follow)
	}
	if got := e.boundRun(t, w); got.Bytes != follow {
		t.Fatalf("worker bound to %v, want the follow-up", got)
	}
	if got := tr.claimRun.Load(); got != 1 {
		t.Fatalf("ClaimRun executions = %d after the follow-up claim, want 1", got)
	}
}
