package store_test

import "testing"

func TestForgeOriginPlacementDueOwnerTrialLiveDB(t *testing.T) {
	f := newForgePlacementFixture(t)
	f.history("forge_no_output")
	f.exec("UPDATE forge_origin_cooldowns SET cooled_until=now()-interval '1 day' WHERE worker_id=$1", f.owner)
	got, err := forgeAdmissionClaim(f.rollAffinityFixture, f.q, f.owner)
	if err != nil || got.ID != f.run || got.ClaimGeneration != 2 {
		t.Fatalf("due custodian lost ordinary trial eligibility: %s G=%d err=%v", got.ID, got.ClaimGeneration, err)
	}
	var reserved bool
	if err := f.pool.QueryRow(f.ctx, `SELECT reservation_run_id=$1 AND reservation_generation=2
		AND reservation_observation_version=observation_version AND observation_version=1
		FROM forge_origin_cooldowns WHERE worker_id=$2`, f.run, f.owner).Scan(&reserved); err != nil || !reserved {
		t.Fatalf("owner trial lost reservation or invented success: %v, %v", reserved, err)
	}
}

func TestForgeOriginPlacementDueUnsafeStaysPinnedLiveDB(t *testing.T) {
	for _, guarded := range []bool{true, false} {
		name := "legacy_history"
		if guarded {
			name = "open_source"
		}
		t.Run(name, func(t *testing.T) {
			f := newForgePlacementFixture(t)
			hold := f.open(1, guarded)
			if !guarded {
				f.exec(`UPDATE recovery_custody_holds SET state='released',live_worker_id=NULL,live_run_id=NULL,
					release_evidence='no_adopted_source',released_at=now() WHERE id=$1`, hold)
			}
			f.queue()
			f.exec("UPDATE forge_origin_cooldowns SET cooled_until=now()-interval '1 day' WHERE worker_id=$1", f.owner)
			f.safe(false)
			f.peerClaim(false)
		})
	}
}

// Timer expiry permits a trial; it does not positively restore forge health.
// A source-proven run must still be available to a healthy peer when the run's
// retry stamp and the failing worker's origin cooldown become due together.
func TestForgeOriginPlacementDueUnverifiedPeerLiveDB(t *testing.T) {
	for _, proof := range []string{"forge_no_output", "no_adopted_source"} {
		t.Run(proof, func(t *testing.T) {
			f := newForgePlacementFixture(t)
			f.history(proof)
			f.exec("UPDATE forge_origin_cooldowns SET cooled_until=now()-interval '1 day' WHERE worker_id=$1", f.owner)
			f.safe(true)
			var due bool
			if err := f.pool.QueryRow(f.ctx, `SELECT fn_forge_origin_eligible(w,r,now()) AND c.reservation_run_id IS NULL
                FROM workers w JOIN forge_origin_cooldowns c ON c.worker_id=w.id
                JOIN runs r ON r.id=$2 WHERE w.id=$1`, f.owner, f.run).Scan(&due); err != nil || !due {
				t.Fatalf("due worker should be trial-eligible: %v, %v", due, err)
			}
			f.peerClaim(true)
		})
	}
}
