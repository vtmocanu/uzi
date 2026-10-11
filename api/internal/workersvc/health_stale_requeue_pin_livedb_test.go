package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// pinAgreementEnv is the shared fixture of the #2705 M3 live-DB tests: a stale previous owner, a
// live peer, and two queued runs in the state RequeueRunsOfStaleWorkers leaves behind. gapRun
// requires a capability nothing online provides (the ListUnplaceableQueuedRunsForEphemeral
// shape); satRun needs none and sits behind a capable online worker at its cap (the
// ListSaturationQueuedRunsForEphemeral shape), so one fixture serves both provisioning queries.
type pinAgreementEnv struct {
	e               interlockLiveDB
	svc             *Service
	owner, peer     uuid.UUID
	gapRun, satRun  uuid.UUID
	grace, ceiling  time.Duration
	queuedThreshold time.Duration
}

func setupPinAgreement(t *testing.T, grace, ceiling time.Duration) *pinAgreementEnv {
	t.Helper()
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	svc.SetCapabilitySettings(fakeCapabilitySettings{on: true})
	svc.p.WorkerStaleRequeueGrace = grace
	svc.p.WorkerAffinityCeiling = ceiling

	// The previous owner: heartbeat-stale, so only the pin can hold the run. The peer is live.
	owner := uuid.New()
	e.exec(t, `INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at)
	           VALUES ($1, $2, 'previous-owner', $3, 'offline', now() - interval '2 minutes')`, owner, e.userID, owner[:])
	peer := e.seedHeartbeatWorker(t, nil)
	// The saturation query counts an online worker with a NULL run-lane cap as free room, so the
	// claimant is marked offline: ClaimRun still serves it (it reads no status), and the only
	// online capable worker is the at-cap one seeded below.
	e.exec(t, `UPDATE workers SET status = 'offline' WHERE id = $1`, peer)
	// The unplaceable trigger needs an opted-in user and a capability nothing online provides; the
	// peer's ClaimRun params carry no worker caps and capability_aware=false, so it can still claim.
	e.exec(t, `UPDATE users SET ephemeral_workers_enabled = true WHERE id = $1`, e.userID)
	// The saturation trigger needs a capable online persistent worker at its run-lane cap.
	capped := e.seedSpreadWorker(t, nil, 1)
	running := e.seedActiveRunOwnedBy(t, capped)
	gapRun := e.seedQueuedRun(t, nil, []string{"gh"})
	satRun := e.seedQueuedRun(t, nil, nil)
	t.Cleanup(func() {
		for _, id := range []uuid.UUID{gapRun, satRun, running} {
			if _, err := e.pool.Exec(e.ctx, `DELETE FROM runs WHERE id = $1`, id); err != nil {
				t.Errorf("cleanup run %v: %v", id, err)
			}
		}
	})
	return &pinAgreementEnv{e: e, svc: svc, owner: owner, peer: peer, gapRun: gapRun, satRun: satRun,
		grace: grace, ceiling: ceiling, queuedThreshold: 10 * time.Minute}
}

// requeue sets both runs to the state RequeueRunsOfStaleWorkers leaves behind (a previous case's
// claim moved one), back-dated by the given ages.
func (p *pinAgreementEnv) requeue(t *testing.T, statusSince, updatedAge time.Duration) {
	t.Helper()
	p.e.exec(t, `UPDATE runs SET status = 'queued', worker_id = $2, claim_generation = 1, stale_requeue_generation = 1,
	                    status_since = now() - make_interval(secs => $3), updated_at = now() - make_interval(secs => $4)
	             WHERE id = ANY($1)`, []uuid.UUID{p.gapRun, p.satRun}, p.owner, statusSince.Seconds(), updatedAge.Seconds())
}

// healthPinned reports whether the real health path (the ListActiveRunsForHealth projection
// through healthTargetFor, so the below-threshold bypass is included) shows the pin reason.
func (p *pinAgreementEnv) healthPinned(t *testing.T, now time.Time, run uuid.UUID) bool {
	t.Helper()
	row := p.e.healthRowFor(t, run)
	_, rawPinned := p.svc.staleRequeuePin(now, row)
	flag, reason := p.svc.healthTargetFor(p.e.ctx, now, row, healthThresholds{queued: p.queuedThreshold})
	shown := flag == healthWaitingWorker && isStaleRequeuePinReason(reason)
	if shown != rawPinned {
		t.Fatalf("healthTargetFor pin=%v (%q, %q) disagrees with staleRequeuePin=%v", shown, flag, reason, rawPinned)
	}
	return shown
}

// demand returns which of the two runs each provisioning query lists, at one evaluation instant.
func (p *pinAgreementEnv) demand(t *testing.T, now time.Time) (gap, sat bool) {
	t.Helper()
	ts := func(d time.Duration) pgtype.Timestamptz { return pgtype.Timestamptz{Time: now.Add(-d), Valid: true} }
	unplaceable, err := p.e.q.ListUnplaceableQueuedRunsForEphemeral(p.e.ctx, store.ListUnplaceableQueuedRunsForEphemeralParams{
		BackgroundGraceCutoff:    ts(15 * time.Minute),
		StaleRequeueCutoff:       StaleRequeueCutoff(now, p.grace),
		AffinityCutoff:           ts(p.ceiling),
		EphemeralLease:           LeaseInterval(0),
		MaxPerUser:               1000,
		MaxRows:                  10000,
		CrossCheckEvaluatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
		CrossCheckAffinityCutoff: ts(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("ListUnplaceableQueuedRunsForEphemeral: %v", err)
	}
	for _, r := range unplaceable {
		gap = gap || r.ID == p.gapRun
	}
	saturated, err := p.e.q.ListSaturationQueuedRunsForEphemeral(p.e.ctx, store.ListSaturationQueuedRunsForEphemeralParams{
		BackgroundGraceCutoff:    ts(15 * time.Minute),
		StaleRequeueCutoff:       StaleRequeueCutoff(now, p.grace),
		AffinityCutoff:           ts(p.ceiling),
		SaturationDelay:          LeaseInterval(30 * time.Second), // below the youngest status_since (1m)
		EphemeralLease:           LeaseInterval(0),
		MaxPerUser:               1000,
		MaxRows:                  10000,
		CrossCheckEvaluatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
		CrossCheckAffinityCutoff: ts(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("ListSaturationQueuedRunsForEphemeral: %v", err)
	}
	for _, r := range saturated {
		sat = sat || r.ID == p.satRun
	}
	return gap, sat
}

// peerBlocked reports whether a live peer's ClaimRun finds nothing to claim. Last in a case,
// since a successful claim moves a run.
func (p *pinAgreementEnv) peerBlocked(t *testing.T, now time.Time) bool {
	t.Helper()
	cp := p.e.claimParams(p.peer, []string{}, false)
	cp.AffinityCutoff = pgtype.Timestamptz{Time: now.Add(-p.ceiling), Valid: true}
	cp.StaleRequeueCutoff = StaleRequeueCutoff(now, p.grace)
	_, err := p.e.q.ClaimRun(p.e.ctx, cp)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ClaimRun: %v", err)
	}
	return errors.Is(err, pgx.ErrNoRows)
}

// #2705 M3 agreement test: with the affinity ceiling SHORTER than the stale-requeue grace, the
// consumers of "is this run held for its returning worker" must agree at every instant: a peer's
// ClaimRun, BOTH ephemeral provisioning triggers, and the health detector's decision (the real
// ListActiveRunsForHealth projection through healthTargetFor). Against a REAL Postgres; skipped
// unless UZI_TEST_DATABASE_URL points at a throwaway one (e2e/run-store-it.sh provides it). Time
// never passes for real: status_since / updated_at are back-dated with UPDATEs.
func TestStaleRequeuePinAgreesAcrossClaimProvisioningAndHealthLiveDB(t *testing.T) {
	const (
		grace   = 10 * time.Minute
		ceiling = 5 * time.Minute // shorter than the grace
	)
	p := setupPinAgreement(t, grace, ceiling)

	cases := []struct {
		name                    string
		statusSince, updatedAge time.Duration
		pinned                  bool
	}{
		{"inside both windows", time.Minute, time.Minute, true},
		{"past the ceiling, inside the grace", time.Minute, 6 * time.Minute, false},
		{"past the grace, inside the ceiling", 11 * time.Minute, time.Minute, false},
		{"past both", 11 * time.Minute, 11 * time.Minute, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p.requeue(t, tc.statusSince, tc.updatedAge)
			now := time.Now()

			gapPinned := p.healthPinned(t, now, p.gapRun)
			satPinned := p.healthPinned(t, now, p.satRun)
			gapDemand, satDemand := p.demand(t, now)
			claimBlocked := p.peerBlocked(t, now)

			if gapPinned != tc.pinned || satPinned != tc.pinned || claimBlocked != tc.pinned ||
				gapDemand != !tc.pinned || satDemand != !tc.pinned {
				t.Fatalf("pinned want %v; health gap=%v sat=%v claimBlocked=%v demand gap=%v sat=%v (demand must be the negation)",
					tc.pinned, gapPinned, satPinned, claimBlocked, gapDemand, satDemand)
			}
		})
	}
}

// #2705 M3: the NON-time half of the pin (owner row exists, stale_requeue_generation equals
// claim_generation) must agree across the same consumers. Inside both windows a control run is
// pinned; then each non-time condition is broken in turn and every consumer must release the run:
// the projection is not pinnable, health shows no pin reason, both triggers list it as demand, and
// a peer can claim. The EXISTS owner-row arm of the projection is not separately observable here:
// runs.worker_id is ON DELETE SET NULL, so a deleted owner always arrives as worker_id IS NULL.
func TestStaleRequeuePinNonTimeHalfAgreesLiveDB(t *testing.T) {
	const (
		grace   = 10 * time.Minute
		ceiling = 30 * time.Minute
	)
	cases := []struct {
		name    string
		breakIt func(p *pinAgreementEnv, t *testing.T)
	}{
		{"owner row deleted", func(p *pinAgreementEnv, t *testing.T) {
			p.e.exec(t, `DELETE FROM workers WHERE id = $1`, p.owner)
		}},
		{"stale_requeue_generation mismatched", func(p *pinAgreementEnv, t *testing.T) {
			p.e.exec(t, `UPDATE runs SET claim_generation = 2 WHERE id = ANY($1)`, []uuid.UUID{p.gapRun, p.satRun})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := setupPinAgreement(t, grace, ceiling)
			p.requeue(t, time.Minute, time.Minute)
			now := time.Now()
			if !p.healthPinned(t, now, p.gapRun) || !p.healthPinned(t, now, p.satRun) {
				t.Fatal("control: the intact stale-requeued runs are not pinned")
			}
			if g, s := p.demand(t, now); g || s {
				t.Fatalf("control: pinned runs are provisioning demand (gap=%v sat=%v)", g, s)
			}

			tc.breakIt(p, t)
			now = time.Now()
			for name, run := range map[string]uuid.UUID{"gap": p.gapRun, "sat": p.satRun} {
				if row := p.e.healthRowFor(t, run); row.StaleRequeuePinnable {
					t.Fatalf("%s run still projects stale_requeue_pinnable", name)
				}
				if p.healthPinned(t, now, run) {
					t.Fatalf("%s run still shows the pin reason", name)
				}
			}
			if g, s := p.demand(t, now); !g || !s {
				t.Fatalf("released runs are not provisioning demand (gap=%v sat=%v)", g, s)
			}
			if p.peerBlocked(t, now) {
				t.Fatal("a peer cannot claim a run whose pin condition is broken")
			}
		})
	}
}
