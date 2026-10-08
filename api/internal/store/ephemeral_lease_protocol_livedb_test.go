package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestEphemeralLeasePlacementProtocolMirrorLiveDB (PRD #2006, review finding): the lease arm of the
// two provisioning triggers must apply the protocol requirements ClaimRun enforces, or a leased
// worker that matches a run's repository and branch but cannot claim it (no codex_harness_v1 for a
// Codex run) reads as a placement and no replacement is provisioned until the lease ends.
// CountOnlineWorkersClaimableForRun already applies them to every worker; it is asserted here too.
func TestEphemeralLeasePlacementProtocolMirrorLiveDB(t *testing.T) {
	iv := leaseInterval(leaseTwoHours)
	docker := []string{"docker"}

	// seed makes a leased worker plus a same-branch queued Codex run, the worker advertising
	// `protos`. withBusy adds a saturated persistent worker that could otherwise take the run, which
	// the saturation trigger needs and the capability-gap trigger must not have.
	seed := func(t *testing.T, iid int64, protos []string, withBusy bool) (*fleetFixture, uuid.UUID) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		if withBusy {
			busy := fx.worker("persistent", capOf(1), true)
			mustExec(fx.ctx, fx.t, fx.pool, `UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`,
				busy, []string{"codex_harness_v1", "codex_runtime_v2", "codex_custom_model_v1"})
			fx.holdActive(busy, 1)
		}
		w := seedLeasedWorker(fx, leaseRun{iid: i64p(iid)}, 0)
		run := insertLeaseRun(fx, leaseRun{iid: i64p(iid)})
		mustExec(fx.ctx, fx.t, fx.pool,
			`UPDATE runs SET harness = 'codex', status_since = now() - interval '1 hour', required_capabilities = $2 WHERE id = $1`, run, docker)
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, w.id, protos)
		return fx, run
	}

	for _, tc := range []struct {
		name   string
		protos []string
		placed bool
	}{
		{"without codex_harness_v1 is not a placement", []string{}, false},
		{"without codex_runtime_v2 is not a placement", []string{"codex_harness_v1"}, false},
		{"with codex_harness_v1 is a placement", []string{"codex_harness_v1", "codex_runtime_v2"}, true},
	} {
		t.Run("a leased worker "+tc.name+" for a Codex run", func(t *testing.T) {
			fx, run := seed(t, 190, tc.protos, false)
			if got := listedUnplaceable(fx, 1000, iv); got[run] == tc.placed {
				t.Fatalf("gap trigger: listed %v; run %s provisioned-for = %v, want %v", got, run, got[run], !tc.placed)
			}
			// With no persistent worker, the leased worker alone decides the saturation trigger's
			// "a capable worker exists" arm: one that cannot claim the run must not make it read as
			// capable-but-full (that run is the gap trigger's), and one that can has a free slot.
			if got := listedSaturation(fx, 1000, iv); got[run] {
				t.Fatalf("saturation trigger without a persistent worker: listed %v; run %s must not be listed", got, run)
			}
			want := int64(0)
			if tc.placed {
				want = 1
			}
			if n := claimableCount(fx, run, iv); n != want {
				t.Fatalf("claimable = %d, want %d", n, want)
			}
			fx2, run2 := seed(t, 191, tc.protos, true)
			if got := listedSaturation(fx2, 1000, iv); got[run2] == tc.placed {
				t.Fatalf("saturation trigger: listed %v; run %s provisioned-for = %v, want %v", got, run2, got[run2], !tc.placed)
			}
		})
	}

	// The custom-Codex-model gate: a run whose effective Codex model (the owner's lane model, since the
	// run carries no curated one) is not curated needs
	// codex_custom_model_v1 as well. The trigger helpers above pass no curated list (the clause then
	// fails open), so these calls pass it explicitly, as hostedsvc does.
	curated := []string{"gpt-6-sol"}
	listed := func(fx *fleetFixture) (gap, sat map[uuid.UUID]bool) {
		fx.t.Helper()
		gap, sat = map[uuid.UUID]bool{}, map[uuid.UUID]bool{}
		g, err := fx.q.ListUnplaceableQueuedRunsForEphemeral(fx.ctx, store.ListUnplaceableQueuedRunsForEphemeralParams{BackgroundGraceCutoff: pgtype.Timestamptz{Time: time.Now().Add(-15 * time.Minute), Valid: true},
			EphemeralLease: iv, MaxPerUser: 1000, MaxRows: 50, CodexCuratedModels: curated})
		if err != nil {
			fx.t.Fatalf("ListUnplaceableQueuedRunsForEphemeral: %v", err)
		}
		for _, r := range g {
			gap[r.ID] = true
		}
		sr, err := fx.q.ListSaturationQueuedRunsForEphemeral(fx.ctx, store.ListSaturationQueuedRunsForEphemeralParams{BackgroundGraceCutoff: pgtype.Timestamptz{Time: time.Now().Add(-15 * time.Minute), Valid: true},
			SaturationDelay: pgtype.Interval{Valid: true}, EphemeralLease: iv, MaxPerUser: 1000, MaxRows: 50, CodexCuratedModels: curated})
		if err != nil {
			fx.t.Fatalf("ListSaturationQueuedRunsForEphemeral: %v", err)
		}
		for _, r := range sr {
			sat[r.ID] = true
		}
		return gap, sat
	}
	for _, tc := range []struct {
		name   string
		protos []string
		placed bool
	}{
		{"without codex_custom_model_v1", []string{"codex_harness_v1", "codex_runtime_v2"}, false},
		{"with codex_custom_model_v1", []string{"codex_harness_v1", "codex_runtime_v2", "codex_custom_model_v1"}, true},
	} {
		t.Run("a leased worker "+tc.name+" and a Codex run on a custom-model lane", func(t *testing.T) {
			fx, run := seed(t, 192, tc.protos, false)
			mustExec(fx.ctx, fx.t, fx.pool, `UPDATE users SET default_codex_model = 'custom-lane-model' WHERE id = $1`, fx.userID)
			gap, _ := listed(fx)
			fx2, run2 := seed(t, 193, tc.protos, true)
			mustExec(fx2.ctx, fx2.t, fx2.pool, `UPDATE users SET default_codex_model = 'custom-lane-model' WHERE id = $1`, fx2.userID)
			_, sat := listed(fx2)
			if gap[run] == tc.placed {
				t.Fatalf("gap trigger: provisioned-for = %v, want %v", gap[run], !tc.placed)
			}
			if sat[run2] == tc.placed {
				t.Fatalf("saturation trigger: provisioned-for = %v, want %v", sat[run2], !tc.placed)
			}
		})
	}
}
