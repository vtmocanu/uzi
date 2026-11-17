package workersvc

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// PRD #4212 m3: the ephemeral lease is the CURRENT admin setting, read through
// EphemeralSettingsReader.EphemeralLease at each decision, never a boot snapshot. Every test below
// builds ONE Service, then changes the setting under it and requires the next decision to follow.
// Against a real Postgres (skipped unless UZI_TEST_DATABASE_URL is set; run via ./e2e/run-store-it.sh).

// mutableLeaseSettings is an EphemeralSettingsReader whose lease and read error can change while the
// Service holds it, and which counts EphemeralLease reads.
type mutableLeaseSettings struct {
	mu    sync.Mutex
	lease time.Duration
	err   error
	calls int
}

func (m *mutableLeaseSettings) EphemeralWorkersEnabled(context.Context) (bool, error) {
	return false, nil
}

func (m *mutableLeaseSettings) EphemeralLease(context.Context) (time.Duration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.err != nil {
		return 0, m.err
	}
	return m.lease, nil
}

func (m *mutableLeaseSettings) set(lease time.Duration, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lease, m.err = lease, err
}

func (m *mutableLeaseSettings) reads() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *mutableLeaseSettings) resetReads() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = 0
}

// liveLeaseService is e.service wired to a mutable reader starting at `lease`.
func (e leaseEnv) liveLeaseService(lease time.Duration) (*Service, *mutableLeaseSettings) {
	svc := e.service(lease, e.pool)
	m := &mutableLeaseSettings{lease: lease}
	svc.SetEphemeralSettings(m)
	return svc, m
}

// TestCompletionLeaseEntryFollowsChangedLeaseLiveDB (5): on one Service, an ephemeral worker's
// completed report enters a lease only while the setting is on at that report. 0 -> report -> torn
// down, no lease; >0 -> next report -> lease entered; and the reverse. A boot-time snapshot answers
// the first value for every later report.
func TestCompletionLeaseEntryFollowsChangedLeaseLiveDB(t *testing.T) {
	for _, interlocked := range []bool{false, true} {
		name := "legacy"
		if interlocked {
			name = "interlocked"
		}
		t.Run(name, func(t *testing.T) {
			e := newLeaseEnv(t)
			svc, m := e.liveLeaseService(0)
			complete := func(lease time.Duration) (leased, exists bool) {
				t.Helper()
				m.set(lease, nil)
				w, run, iid := e.seedBound(t, "running", 1, interlocked)
				if interlocked {
					e.grantPermit(t, svc, w, run, iid)
				}
				if r := awaitState(t, e.reportAsync(svc, w, run, completeReq(iid, 1))); r.err != nil {
					t.Fatalf("SetState completed at lease %v: %v", lease, r.err)
				}
				if exists = e.workerExists(t, w); exists {
					leased = e.leased(t, w)
				}
				return leased, exists
			}
			if leased, exists := complete(0); leased || exists {
				t.Fatalf("lease 0: leased=%v exists=%v, want the worker torn down with no lease", leased, exists)
			}
			if leased, exists := complete(2 * time.Hour); !exists || !leased {
				t.Fatalf("lease raised to 2h: leased=%v exists=%v, want the worker kept in a lease", leased, exists)
			}
			if leased, exists := complete(0); leased || exists {
				t.Fatalf("lease lowered back to 0: leased=%v exists=%v, want the worker torn down with no lease", leased, exists)
			}
		})
	}
}

// TestClaimLeaseArmFollowsChangedLeaseLiveDB (6): a worker leased 30 minutes ago cannot claim a
// follow-up through the lease while the setting is 10m, can once it is 2h, and cannot again at 0 or
// 10m, all on one Service and one worker. (The spread-peer mirror of the same single claim read is
// not separately exercised: it needs a second claimant deferring to a leased peer, which no existing
// fixture builds.)
func TestClaimLeaseArmFollowsChangedLeaseLiveDB(t *testing.T) {
	for _, short := range []time.Duration{10 * time.Minute, 0} {
		t.Run("short="+short.String(), func(t *testing.T) {
			e := newLeaseEnv(t)
			svc, m := e.liveLeaseService(short)
			w, _, iid := e.seedLeasedWorker(t)
			e.exec(`UPDATE workers SET lease_since = now() - interval '30 minutes' WHERE id = $1`, w)
			follow := e.seedFollowUp(t, iid, agentIssueBranch(iid))

			r := awaitClaim(t, e.claimAsync(svc, w, 1))
			if r.err != nil || r.payload != nil {
				t.Fatalf("claim at lease %v = (%+v, %v), want idle: the 30m-old lease is expired", short, r.payload, r.err)
			}
			if s := e.runStatusOf(t, follow); s != "queued" {
				t.Fatalf("follow-up = %q, want queued", s)
			}

			m.set(2*time.Hour, nil)
			r = awaitClaim(t, e.claimAsync(svc, w, 2))
			if r.err != nil || r.payload == nil || r.payload.RunID != follow.String() {
				t.Fatalf("claim after raising to 2h = (%+v, %v), want the follow-up %s", r.payload, r.err, follow)
			}
			if got := e.boundRun(t, w); !got.Valid || got.Bytes != follow {
				t.Fatalf("worker bound to %v, want the follow-up", got)
			}
		})
	}
	t.Run("lowered", func(t *testing.T) {
		e := newLeaseEnv(t)
		svc, m := e.liveLeaseService(2 * time.Hour)
		w, _, iid := e.seedLeasedWorker(t)
		e.exec(`UPDATE workers SET lease_since = now() - interval '30 minutes' WHERE id = $1`, w)
		follow := e.seedFollowUp(t, iid, agentIssueBranch(iid))
		m.set(10*time.Minute, nil)
		r := awaitClaim(t, e.claimAsync(svc, w, 1))
		if r.err != nil || r.payload != nil {
			t.Fatalf("claim after lowering to 10m = (%+v, %v), want idle", r.payload, r.err)
		}
		if s := e.runStatusOf(t, follow); s != "queued" {
			t.Fatalf("follow-up = %q, want queued", s)
		}
	})
}

// TestHealthEligibilityFollowsChangedLeaseLiveDB (7): the health detector's eligibility count
// (workerEligibilityForHealth -> CountOnlineWorkersClaimableForRun) counts a leased ephemeral peer
// only while the setting keeps its lease live, on one Service. The released-worker queued-reason
// path (queuedReasonAfterPrerequisites) calls the same query with the same single read; it is not
// separately driven here.
func TestHealthEligibilityFollowsChangedLeaseLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	m := &mutableLeaseSettings{lease: 2 * time.Hour}
	svc.SetEphemeralSettings(m)
	run := e.seedQueuedRun(t, nil, nil)
	bound := e.seedQueuedRun(t, nil, nil)
	worker := e.seedHeartbeatWorker(t, nil)
	e.exec(t, "UPDATE runs SET worker_id=$2,branch=NULL WHERE id=$1", run, worker)
	e.exec(t, `UPDATE workers SET ephemeral=true,ephemeral_run_id=$2,
        lease_since=now(),lease_repo_id=(SELECT repo_id FROM runs WHERE id=$3),
        lease_branch=(SELECT 'agent/issue-' || issue_iid::text FROM runs WHERE id=$3) WHERE id=$1`, worker, bound, run)

	count := func() int64 {
		t.Helper()
		c, err := svc.workerEligibilityForHealth(e.ctx, time.Now(), run, true)
		if err != nil {
			t.Fatalf("workerEligibilityForHealth: %v", err)
		}
		return c.Claimable
	}
	for i, step := range []struct {
		lease time.Duration
		want  int64
	}{{2 * time.Hour, 1}, {0, 0}, {2 * time.Hour, 1}, {0, 0}} {
		m.set(step.lease, nil)
		if got := count(); got != step.want {
			t.Fatalf("step %d lease %v: claimable = %d, want %d", i, step.lease, got, step.want)
		}
	}
	// A failed read fails the detector's call rather than counting against a guessed lease.
	m.set(0, errors.New("settings unavailable"))
	if _, err := svc.workerEligibilityForHealth(e.ctx, time.Now(), run, true); err == nil {
		t.Fatal("workerEligibilityForHealth succeeded with the lease read failing")
	}
}

// TestClaimLeaseReadErrorLiveDB (9): an ephemeral claimant whose lease read fails gets an error and
// claims nothing; a persistent claimant proceeds as at lease 0 and still claims.
func TestClaimLeaseReadErrorLiveDB(t *testing.T) {
	boom := errors.New("settings unavailable")
	t.Run("ephemeral claimant fails and claims nothing", func(t *testing.T) {
		e := newLeaseEnv(t)
		svc, m := e.liveLeaseService(2 * time.Hour)
		w, served, iid := e.seedLeasedWorker(t)
		follow := e.seedFollowUp(t, iid, agentIssueBranch(iid))
		m.set(0, boom)
		r := awaitClaim(t, e.claimAsync(svc, w, 1))
		if r.err == nil || !errors.Is(r.err, boom) || r.payload != nil {
			t.Fatalf("claim = (%+v, %v), want a payload-less error wrapping the read failure", r.payload, r.err)
		}
		if m.reads() == 0 {
			t.Fatal("the lease reader was never called: the error path was not exercised")
		}
		if s := e.runStatusOf(t, follow); s != "queued" {
			t.Fatalf("follow-up = %q, want queued", s)
		}
		if got := e.boundRun(t, w); !got.Valid || got.Bytes != served {
			t.Fatalf("worker bound to %v, want still the served run", got)
		}
		if !e.leased(t, w) {
			t.Fatal("the lease must be untouched by a failed claim")
		}
		// The read recovers: the same Service claims normally.
		m.set(2*time.Hour, nil)
		r = awaitClaim(t, e.claimAsync(svc, w, 2))
		if r.err != nil || r.payload == nil || r.payload.RunID != follow.String() {
			t.Fatalf("claim after recovery = (%+v, %v), want the follow-up", r.payload, r.err)
		}
	})
	t.Run("persistent claimant still claims", func(t *testing.T) {
		e := newLeaseEnv(t)
		svc, m := e.liveLeaseService(2 * time.Hour)
		w, _, iid := e.seedBound(t, "completed", 1, false)
		e.exec(`UPDATE workers SET ephemeral = false, ephemeral_run_id = NULL WHERE id = $1`, w)
		follow := e.seedFollowUp(t, iid, "")
		m.set(0, boom)
		r := awaitClaim(t, e.claimAsync(svc, w, 1))
		if r.err != nil || r.payload == nil || r.payload.RunID != follow.String() {
			t.Fatalf("claim = (%+v, %v), want the queued run claimed despite the failing lease read", r.payload, r.err)
		}
		if m.reads() == 0 {
			t.Fatal("the lease reader was never called: the error path was not exercised")
		}
		if s := e.runStatusOf(t, follow); s != "claimed" {
			t.Fatalf("run = %q, want claimed", s)
		}
	})
}

// TestStatusReportLeaseReadsLiveDB (10): a routine status report never calls the lease reader; an
// ephemeral worker's terminal report calls it exactly once; and a terminal report whose read fails
// is refused with the run and the worker unchanged.
func TestStatusReportLeaseReadsLiveDB(t *testing.T) {
	t.Run("routine report does not read the lease", func(t *testing.T) {
		e := newLeaseEnv(t)
		svc, m := e.liveLeaseService(2 * time.Hour)
		w, run, _ := e.seedBound(t, "running", 1, false)
		gen := int64(1)
		for _, state := range []string{"running", "awaiting_input"} {
			r := awaitState(t, e.reportAsync(svc, w, run, StateRequest{State: state, ClaimGeneration: &gen, OpenQuestionID: strPtr("q-1")}))
			if r.err != nil {
				t.Fatalf("SetState %s: %v", state, r.err)
			}
			if got := e.runStatusOf(t, run); got != state {
				t.Fatalf("after %s report run = %q", state, got)
			}
		}
		if n := m.reads(); n != 0 {
			t.Fatalf("lease reader called %d times by routine reports, want 0", n)
		}
	})
	for _, c := range []struct {
		name        string
		lease       time.Duration
		interlocked bool
	}{
		{"legacy completed, lease on", 2 * time.Hour, false},
		{"legacy completed, lease off", 0, false},
		{"interlocked completed, lease on", 2 * time.Hour, true},
		{"interlocked completed, lease off", 0, true},
	} {
		t.Run("terminal report reads once: "+c.name, func(t *testing.T) {
			e := newLeaseEnv(t)
			svc, m := e.liveLeaseService(c.lease)
			w, run, iid := e.seedBound(t, "running", 1, c.interlocked)
			if c.interlocked {
				e.grantPermit(t, svc, w, run, iid)
			}
			m.resetReads()
			if r := awaitState(t, e.reportAsync(svc, w, run, completeReq(iid, 1))); r.err != nil {
				t.Fatalf("SetState completed: %v", r.err)
			}
			if n := m.reads(); n != 1 {
				t.Fatalf("lease reader called %d times by one terminal report, want exactly 1", n)
			}
			if got := e.runStatusOf(t, run); got != "completed" {
				t.Fatalf("run = %q, want completed", got)
			}
		})
	}
	for _, interlocked := range []bool{false, true} {
		name := "legacy"
		if interlocked {
			name = "interlocked"
		}
		t.Run("terminal report with a failing read changes nothing: "+name, func(t *testing.T) {
			e := newLeaseEnv(t)
			svc, m := e.liveLeaseService(2 * time.Hour)
			w, run, iid := e.seedBound(t, "running", 1, interlocked)
			if interlocked {
				e.grantPermit(t, svc, w, run, iid)
			}
			boom := errors.New("settings unavailable")
			m.set(0, boom)
			r := awaitState(t, e.reportAsync(svc, w, run, completeReq(iid, 1)))
			if r.err == nil || !errors.Is(r.err, boom) {
				t.Fatalf("SetState err = %v, want one wrapping the read failure", r.err)
			}
			if got := e.runStatusOf(t, run); got != "running" {
				t.Fatalf("run = %q, want unchanged (running)", got)
			}
			if !e.workerExists(t, w) || e.leased(t, w) {
				t.Fatal("the worker must be untouched (present, no lease)")
			}
			// The worker's retry succeeds once the read recovers, on the same Service.
			m.set(2*time.Hour, nil)
			if r := awaitState(t, e.reportAsync(svc, w, run, completeReq(iid, 1))); r.err != nil {
				t.Fatalf("retry after recovery: %v", r.err)
			}
			if got := e.runStatusOf(t, run); got != "completed" {
				t.Fatalf("run after retry = %q, want completed", got)
			}
		})
	}
}

// TestHealthEligibilityFollowsShortenedPositiveLeaseLiveDB: a positive-to-positive change on one
// Service. A leased peer 30m into its lease is counted claimable at 2h and not at 10m, in either
// order of change, so a 2h boot value is not remembered.
func TestHealthEligibilityFollowsShortenedPositiveLeaseLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	m := &mutableLeaseSettings{lease: 2 * time.Hour}
	svc.SetEphemeralSettings(m)
	run := e.seedQueuedRun(t, nil, nil)
	bound := e.seedQueuedRun(t, nil, nil)
	worker := e.seedHeartbeatWorker(t, nil)
	e.exec(t, "UPDATE runs SET worker_id=$2,branch=NULL WHERE id=$1", run, worker)
	e.exec(t, `UPDATE workers SET ephemeral=true,ephemeral_run_id=$2,
        lease_since=now() - interval '30 minutes',lease_repo_id=(SELECT repo_id FROM runs WHERE id=$3),
        lease_branch=(SELECT 'agent/issue-' || issue_iid::text FROM runs WHERE id=$3) WHERE id=$1`, worker, bound, run)

	for i, step := range []struct {
		lease time.Duration
		want  int64
	}{{2 * time.Hour, 1}, {10 * time.Minute, 0}, {2 * time.Hour, 1}, {10 * time.Minute, 0}} {
		m.set(step.lease, nil)
		c, err := svc.workerEligibilityForHealth(e.ctx, time.Now(), run, true)
		if err != nil {
			t.Fatalf("step %d workerEligibilityForHealth: %v", i, err)
		}
		if c.Claimable != step.want {
			t.Fatalf("step %d lease %v (30m into the lease): claimable = %d, want %d", i, step.lease, c.Claimable, step.want)
		}
	}
}

// TestClaimSpreadPeerFollowsShortenedLeaseLiveDB: the spread-peer mirror of the claim's lease read.
// A busy persistent claimant defers a fresh queued run to an idle leased ephemeral peer 30m into its
// lease while the setting is 2h; once the SAME Service sees 10m the peer's lease is expired, it is
// no deferral target, and the claimant claims the run.
func TestClaimSpreadPeerFollowsShortenedLeaseLiveDB(t *testing.T) {
	e := newLeaseEnv(t)
	svc, m := e.liveLeaseService(2 * time.Hour)
	peer, _, iid := e.seedLeasedWorker(t)
	e.exec(`UPDATE workers SET max_concurrent_runs = 2, lease_since = now() - interval '30 minutes' WHERE id = $1`, peer)

	claimant := uuid.New()
	e.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, max_concurrent_runs, snapshot_register_nonce)
	        VALUES ($1, $2, 'persistent', $3, 'online', now(), 2, 'nonce-A')`, claimant, e.userID, claimant[:])
	e.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id, claim_generation)
	        VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'running', $5, 1)`, uuid.New(), e.userID, e.repoID, nextLeaseIID(), claimant)
	follow := e.seedFollowUp(t, iid, agentIssueBranch(iid))

	if r := awaitClaim(t, e.claimNoSnapshotAsync(svc, claimant)); r.err != nil || r.payload != nil {
		t.Fatalf("lease 2h: claim = (%+v, %v), want idle (deferred to the live-leased peer)", r.payload, r.err)
	}
	if s := e.runStatusOf(t, follow); s != "queued" {
		t.Fatalf("lease 2h: follow-up = %q, want still queued", s)
	}

	m.set(10*time.Minute, nil)
	r := awaitClaim(t, e.claimNoSnapshotAsync(svc, claimant))
	if r.err != nil || r.payload == nil || r.payload.RunID != follow.String() {
		t.Fatalf("lease shortened to 10m: claim = (%+v, %v), want the follow-up %s", r.payload, r.err, follow)
	}
}
