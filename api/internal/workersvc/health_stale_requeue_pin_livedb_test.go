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

// #2705 M3 agreement test: with the affinity ceiling SHORTER than the stale-requeue grace, the
// three consumers of "is this run held for its returning worker" must agree at every instant:
// a peer's ClaimRun, the ephemeral provisioning trigger, and the health detector's pin decision
// (the real ListActiveRunsForHealth projection through staleRequeuePin and queuedReason). Against
// a REAL Postgres; skipped unless UZI_TEST_DATABASE_URL points at a throwaway one
// (e2e/run-store-it.sh provides it). Time never passes for real: status_since / updated_at are
// back-dated with UPDATEs.
func TestStaleRequeuePinAgreesAcrossClaimProvisioningAndHealthLiveDB(t *testing.T) {
	const (
		grace   = 10 * time.Minute
		ceiling = 5 * time.Minute // shorter than the grace
	)
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
	// The unplaceable trigger needs an opted-in user and a capability nothing online provides; the
	// peer's ClaimRun params carry no worker caps and capability_aware=false, so it can still claim.
	e.exec(t, `UPDATE users SET ephemeral_workers_enabled = true WHERE id = $1`, e.userID)
	run := e.seedQueuedRun(t, nil, []string{"gh"})
	t.Cleanup(func() { _, _ = e.pool.Exec(e.ctx, `DELETE FROM runs WHERE id = $1`, run) })

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
			// The state RequeueRunsOfStaleWorkers leaves behind (a previous case's claim moved it).
			e.exec(t, `UPDATE runs SET status = 'queued', worker_id = $2, claim_generation = 1, stale_requeue_generation = 1,
			                  status_since = now() - make_interval(secs => $3), updated_at = now() - make_interval(secs => $4)
			           WHERE id = $1`, run, owner, tc.statusSince.Seconds(), tc.updatedAge.Seconds())
			now := time.Now()

			// Health: the Go pin decision and the reason it produces.
			row := e.healthRowFor(t, run)
			_, healthPinned := svc.staleRequeuePin(now, row)
			if reasonPinned := isStaleRequeuePinReason(svc.queuedReason(e.ctx, now, row)); reasonPinned != healthPinned {
				t.Fatalf("queuedReason pin=%v disagrees with staleRequeuePin=%v", reasonPinned, healthPinned)
			}

			// Provisioning: the run is demand exactly when it is NOT pinned.
			rows, err := e.q.ListUnplaceableQueuedRunsForEphemeral(e.ctx, store.ListUnplaceableQueuedRunsForEphemeralParams{
				BackgroundGraceCutoff:    pgtype.Timestamptz{Time: now.Add(-15 * time.Minute), Valid: true},
				StaleRequeueCutoff:       StaleRequeueCutoff(now, grace),
				AffinityCutoff:           pgtype.Timestamptz{Time: now.Add(-ceiling), Valid: true},
				EphemeralLease:           LeaseInterval(0),
				MaxPerUser:               1000,
				MaxRows:                  10000,
				CrossCheckEvaluatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
				CrossCheckAffinityCutoff: pgtype.Timestamptz{Time: now.Add(-2 * time.Minute), Valid: true},
			})
			if err != nil {
				t.Fatalf("ListUnplaceableQueuedRunsForEphemeral: %v", err)
			}
			demand := false
			for _, r := range rows {
				demand = demand || r.ID == run
			}

			// Claim: a live peer is blocked exactly when the run is pinned. Last, since a claim moves the run.
			p := e.claimParams(peer, []string{}, false)
			p.AffinityCutoff = pgtype.Timestamptz{Time: now.Add(-ceiling), Valid: true}
			p.StaleRequeueCutoff = StaleRequeueCutoff(now, grace)
			_, claimErr := e.q.ClaimRun(e.ctx, p)
			if claimErr != nil && !errors.Is(claimErr, pgx.ErrNoRows) {
				t.Fatalf("ClaimRun: %v", claimErr)
			}
			claimBlocked := errors.Is(claimErr, pgx.ErrNoRows)

			if healthPinned != tc.pinned || claimBlocked != tc.pinned || demand != !tc.pinned {
				t.Fatalf("pinned want %v; health=%v claimBlocked=%v provisioningDemand=%v (demand must be the negation)",
					tc.pinned, healthPinned, claimBlocked, demand)
			}
		})
	}
}
