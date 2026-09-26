package workersvc

import (
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// These cases exercise the Register transaction at the requeue limit. A pending
// outcome is represented by the register snapshot; no replay is run here.
func TestRegisterPendingOutcomeClassificationLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name            string
		snapshot        func(uuid.UUID) *ActiveSnapshot
		wantStatus      string
		wantOrigin      string
		wantLease       bool
		wantOverflow    bool
		preseedLeaseGen int64
	}{
		{
			name:       "no journal and no snapshot fails as worker lost",
			wantStatus: "failed", wantOrigin: "worker_lost",
		},
		{
			name: "valid worker register overflow snapshot preserves run",
			snapshot: func(_ uuid.UUID) *ActiveSnapshot {
				return &ActiveSnapshot{SnapshotEpoch: 0, PendingOverflow: true, Active: []ActiveRunEntry{}}
			},
			wantStatus: "running", wantOverflow: true,
		},
		{
			name:            "prior exact-generation terminal lease protects run without register snapshot",
			preseedLeaseGen: 1,
			wantStatus:      "running", wantLease: true,
		},
		{
			name:            "wrong-generation terminal lease cannot protect run without register snapshot",
			preseedLeaseGen: 2,
			wantStatus:      "failed", wantOrigin: "worker_lost", wantLease: true,
		},
		{
			name: "invalid register snapshot is ignored and orphan pass fails worker lost",
			snapshot: func(run uuid.UUID) *ActiveSnapshot {
				return &ActiveSnapshot{SnapshotEpoch: 0, PendingOverflow: true,
					Active: []ActiveRunEntry{entry(run, 1, "invalid_phase", true)}}
			},
			wantStatus: "failed", wantOrigin: "worker_lost",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			userID, _, repoID := env.seedCodexInfra(t)
			svc := snapshotSvc(env, testParams())
			workerID := seedSnapshotWorker(t, env, userID, "old-nonce")
			runID := seedOutageRun(t, env, userID, repoID, workerID, "running", "issue", 1, 1)
			holdID := claimRecoveryHold(t, env, runID, workerID, 1)
			if tc.preseedLeaseGen != 0 {
				insertActiveLease(t, env, workerID, runID, tc.preseedLeaseGen, true, "1 hour")
			}

			var snap *ActiveSnapshot
			if tc.snapshot != nil {
				snap = tc.snapshot(runID)
			}
			if _, _, err := svc.Register(env.ctx, store.Worker{ID: workerID, UserID: userID},
				"v1", "base", nil, nil, nil, snap); err != nil {
				t.Fatalf("Register: %v", err)
			}

			var status string
			var generation int64
			var requeues int32
			if err := env.pool.QueryRow(env.ctx,
				`SELECT status, claim_generation, requeue_count FROM runs WHERE id = $1`, runID).
				Scan(&status, &generation, &requeues); err != nil {
				t.Fatalf("read run: %v", err)
			}
			if status != tc.wantStatus || generation != 1 || requeues != 1 {
				t.Fatalf("run status=%q generation=%d requeues=%d, want %q/1/1",
					status, generation, requeues, tc.wantStatus)
			}
			if origin := failOriginOf(t, env, runID); origin != tc.wantOrigin {
				t.Fatalf("fail origin = %q, want %q", origin, tc.wantOrigin)
			}
			lease, ok := readActiveRun(t, env, workerID, runID)
			if ok != tc.wantLease {
				t.Fatalf("active lease present = %v, want %v", ok, tc.wantLease)
			}
			if tc.wantLease && (lease.gen != tc.preseedLeaseGen || !lease.terminalPending || !lease.untilFuture) {
				t.Fatalf("active lease = %+v, want future terminal lease at generation %d", lease, tc.preseedLeaseGen)
			}
			if flag, future := workerOverflow(t, env, workerID); flag != tc.wantOverflow || future != tc.wantOverflow {
				t.Fatalf("overflow flag=%v future=%v, want both %v", flag, future, tc.wantOverflow)
			}
			var holdGeneration int64
			var holdState string
			if err := env.pool.QueryRow(env.ctx,
				`SELECT generation, state FROM recovery_custody_holds WHERE id = $1 AND run_id = $2 AND live_worker_id = $3`,
				holdID, runID, workerID).Scan(&holdGeneration, &holdState); err != nil {
				t.Fatalf("read exact custody hold: %v", err)
			}
			if holdGeneration != generation || holdState != "open" {
				t.Fatalf("custody hold generation=%d state=%q, want %d/open", holdGeneration, holdState, generation)
			}
		})
	}
}
