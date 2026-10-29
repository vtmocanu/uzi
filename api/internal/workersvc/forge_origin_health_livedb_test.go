package workersvc

import (
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
)

// Read the real per-run projection: online heartbeat and static capabilities
// must not turn an origin-admission wait into "busy" or a capability gap.
func TestForgeOriginHealthAdmissionLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	at := time.Now().UTC()
	worker := e.seedWorker(t, nil)
	id := e.fpSeedRunning(t, worker, 1)
	e.exec(t, "UPDATE workers SET last_heartbeat_at=$2 WHERE id=$1", worker, at)
	e.exec(t, "UPDATE runs SET status='queued',status_since=$2,updated_at=$2 WHERE id=$1", id, at.Add(-time.Hour))
	e.exec(t, `INSERT INTO forge_origin_cooldowns(user_id,worker_id,origin,cooled_until,last_reason,observed_at)
        VALUES($1,$2,'https://forge.e2e',$3,'transient_clone_fetch_exhausted',$4)`,
		e.userID, worker, at.Add(time.Hour), at)
	svc := e.permitService(t)
	for _, due := range []bool{false, true} {
		if due {
			e.exec(t, "UPDATE forge_origin_cooldowns SET cooled_until=$2 WHERE worker_id=$1", worker, at.Add(-time.Hour))
		}
		counts, err := svc.WorkerEligibilityForHealth(e.ctx, at, id)
		if err != nil {
			t.Fatal(err)
		}
		if due {
			if counts.Claimable != 1 || counts.ForgeBlocked != 0 {
				t.Fatalf("due counts: %+v", counts)
			}
		} else {
			if counts.Claimable != 0 || counts.ForgeBlocked != 1 {
				t.Fatalf("cooled counts: %+v", counts)
			}
			reason := svc.queuedReasonWithEligibility(e.ctx, at, e.healthRowFor(t, id),
				healthThresholds{custodyLimit: testCustodyLimit, custodyLimitOK: true}, false,
				&healthEligibilityResult{counts: counts})
			if reason != reasonForgeOriginWait {
				t.Fatalf("cooldown-only reason: %q", reason)
			}
		}
	}
	// Admission aggregates change without rewriting the worker's liveness.
	n, err := e.q.CountOnlineWorkersForUser(e.ctx, e.userID)
	if err != nil || n != 1 {
		t.Fatalf("heartbeat online count: %d, %v", n, err)
	}
	// A due ordinary claim owns one reservation; another queued run reports a
	// trial wait until existing lifecycle cleanup retires that tuple.
	got, err := e.q.ClaimRun(e.ctx, e.fpRecoveryClaimParams(worker, at.Add(-45*time.Second)))
	if err != nil || got.ID != id {
		t.Fatalf("half-open claim: %s, %v", got.ID, err)
	}
	next := e.fpSeedRunning(t, worker, 1)
	e.exec(t, "UPDATE runs SET status='queued',worker_id=NULL,updated_at=$2,status_since=$2 WHERE id=$1", next, at.Add(-time.Hour))
	counts, err := svc.WorkerEligibilityForHealth(e.ctx, at, next)
	if err != nil || counts.Claimable != 0 || counts.ForgeBlocked != 1 {
		t.Fatalf("reserved counts: %+v, %v", counts, err)
	}
	// Sweep the claimed-never-started flight through the existing query, not a
	// telemetry timeout. It must dispose of the reservation, retaining health.
	e.exec(t, "UPDATE runs SET claimed_at=$2 WHERE id=$1", id, at.Add(-time.Hour))
	if _, err := e.q.SweepClaimedNeverStarted(e.ctx, pgconv.Time(at)); err != nil {
		t.Fatal(err)
	}
	c := e.originCooldown(t, worker, "https://forge.e2e")
	if c.ReservationRunID.Valid || c.ObservationVersion != 1 {
		t.Fatalf("sweep reservation: %+v", c)
	}
}
