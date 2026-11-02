package workersvc

import (
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/capability"
)

func TestPolicyPendingLeaseSurvivesOmissionLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name    string
		capable bool
		mutate  string
	}{
		{name: "legacy"}, {name: "policy-capable", capable: true},
		{name: "queued", capable: true, mutate: "UPDATE runs SET status='queued' WHERE id=$1"},
		{name: "released", capable: true, mutate: "UPDATE runs SET claim_released_at=now() WHERE id=$1"},
		{name: "new-generation", capable: true, mutate: "UPDATE runs SET claim_generation=4 WHERE id=$1"},
		{name: "other-harness", capable: true, mutate: "UPDATE runs SET harness='claude' WHERE id=$1"},
		{name: "interactive", capable: true, mutate: "UPDATE runs SET interactive=true WHERE id=$1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capable := tc.capable
			keep := capable && tc.mutate == ""
			env := setupCodexLiveDB(t)
			user, _, repo := env.seedCodexInfra(t)
			svc := snapshotSvc(env, testParams())
			wk := seedSnapshotWorker(t, env, user, "policy-lease")
			if capable {
				env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", wk, []string{capability.ProviderPolicyHoldV1})
			}
			run := seedOutageRun(t, env, user, repo, wk, "running", "issue", 3, 0)
			env.exec("UPDATE runs SET harness='codex' WHERE id=$1", run)
			apply := func(epoch int64, mode snapshotMode, active []ActiveRunEntry) {
				t.Helper()
				ok, err := applySnapshot(t, svc, env, wk, &ActiveSnapshot{SnapshotEpoch: epoch, RegisterNonce: "policy-lease", Active: active}, mode)
				if err != nil || !ok {
					t.Fatalf("snapshot applied=%v err=%v", ok, err)
				}
			}
			apply(1, snapshotModeHeartbeat, []ActiveRunEntry{entry(run, 3, "running", true)})
			if tc.mutate != "" {
				env.exec(tc.mutate, run)
			}
			var deadline time.Time
			if err := env.pool.QueryRow(env.ctx, "SELECT terminal_pending_until FROM worker_active_runs WHERE worker_id=$1 AND run_id=$2", wk, run).Scan(&deadline); err != nil {
				t.Fatal(err)
			}
			// Restart offers no journal body. Register preserves the old lease; the first
			// ordinary heartbeat must neither erase nor extend that bounded protection.
			apply(2, snapshotModeRegister, nil)
			apply(3, snapshotModeHeartbeat, nil)
			row, exists := readActiveRun(t, env, wk, run)
			if exists != keep {
				t.Fatalf("lease exists=%v want %v", exists, keep)
			}
			pending, err := env.q.RunHasPendingOutcomeLease(env.ctx, run)
			if err != nil || pending != keep {
				t.Fatalf("pending=%v err=%v", pending, err)
			}
			if !keep {
				return
			}
			var unchanged time.Time
			if err := env.pool.QueryRow(env.ctx, "SELECT terminal_pending_until FROM worker_active_runs WHERE worker_id=$1 AND run_id=$2", wk, run).Scan(&unchanged); err != nil {
				t.Fatal(err)
			}
			if !deadline.Equal(unchanged) || row.gen != 3 || !row.terminalPending {
				t.Fatal("omission renewed or changed the existing lease")
			}
			// Expiry remains the declared boundary. No sleep, renewal or new lease exists.
			env.exec("UPDATE worker_active_runs SET terminal_pending_until=now()-interval '1 second' WHERE worker_id=$1 AND run_id=$2", wk, run)
			apply(4, snapshotModeHeartbeat, nil)
			if _, exists := readActiveRun(t, env, wk, run); exists {
				t.Fatal("expired omitted lease survived")
			}
			pending, err = env.q.RunHasPendingOutcomeLease(env.ctx, run)
			if err != nil || pending {
				t.Fatalf("expired pending=%v err=%v", pending, err)
			}
		})
	}
}
