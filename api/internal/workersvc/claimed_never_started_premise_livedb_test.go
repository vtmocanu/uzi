package workersvc

import (
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
)

// TestClaimedNeverStartedRefusedRunningPremiseLiveDB pins the premise of issue #3859 through the
// real Service.SetStateReportWithReconciliation path: a retained continuation (an open custody
// hold of an EARLIER generation than runs.claim_generation) whose first `running` report is
// refused by a SetRunRunning guard stays `claimed` with Applied=false, for each refusal reason,
// while an ordinary continuation with no refusing guard reaches `running`.
func TestClaimedNeverStartedRefusedRunningPremiseLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)

	// secondGeneration claims the run (generation 1, which opens the custody hold), requeues it
	// with the hold left in place, and re-claims it as generation 2.
	secondGeneration := func(t *testing.T, f *cncFixture, run uuid.UUID) {
		t.Helper()
		f.claimRun(t, run, 1)
		f.requeue(run)
		f.claimRun(t, run, 2)
		var holdGen int64
		if err := f.env.pool.QueryRow(f.env.ctx,
			`SELECT min(generation) FROM recovery_custody_holds WHERE run_id = $1 AND state = 'open'`, run).Scan(&holdGen); err != nil {
			t.Fatalf("read hold generation: %v", err)
		}
		if st := f.run(t, run); st.Status != "claimed" || st.ClaimGeneration != 2 || holdGen >= st.ClaimGeneration {
			t.Fatalf("setup: status=%s claim_generation=%d open hold generation=%d, want claimed at generation 2 with an earlier open hold", st.Status, st.ClaimGeneration, holdGen)
		}
	}

	refused := func(t *testing.T, f *cncFixture, run uuid.UUID) {
		t.Helper()
		res := f.evidenceReport(t, run, 2, f.nonce, f.next())
		if res.Applied {
			t.Fatalf("running report applied, want refused by the SetRunRunning guard")
		}
		if st := f.run(t, run); st.Status != "claimed" || st.ClaimGeneration != 2 {
			t.Fatalf("status=%s claim_generation=%d, want still claimed at generation 2", st.Status, st.ClaimGeneration)
		}
	}

	t.Run("semantic_planning_mismatch", func(t *testing.T) {
		f := newCNCFixture(t, env, 3)
		run := f.seedGuardedRun(t)
		secondGeneration(t, f, run)
		refused(t, f, run)
	})

	for _, col := range []string{"plan_cross_check_required", "plan_cross_check_gated"} {
		t.Run("unapproved_plan_"+col, func(t *testing.T) {
			f := newCNCFixture(t, env, 3)
			// A plan_cross_check_required run is claimable only by a cross_check_v1 worker.
			f.wkr.ProtocolCapabilities = append(f.wkr.ProtocolCapabilities, capability.CrossCheckV1)
			f.env.exec(`UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, f.workerID, f.wkr.ProtocolCapabilities)
			run := f.seedRun("queued", 0)
			f.env.exec(`UPDATE runs SET plan_md = 'planned', auto_approve = false, `+col+` = true WHERE id = $1`, run)
			secondGeneration(t, f, run)
			var approved int
			if err := f.env.pool.QueryRow(f.env.ctx,
				`SELECT count(*) FROM run_user_inputs WHERE run_id = $1 AND kind = 'approve_plan' AND applied_at IS NOT NULL`, run).Scan(&approved); err != nil || approved != 0 {
				t.Fatalf("setup: applied approve_plan rows = %d (%v), want none", approved, err)
			}
			refused(t, f, run)
		})
	}

	t.Run("control_ordinary_continuation_reaches_running", func(t *testing.T) {
		f := newCNCFixture(t, env, 3)
		run := f.seedRun("queued", 0)
		secondGeneration(t, f, run)
		res := f.evidenceReport(t, run, 2, f.nonce, f.next())
		if !res.Applied {
			t.Fatalf("control running report not applied")
		}
		if st := f.run(t, run); st.Status != "running" {
			t.Fatalf("control status=%s, want running", st.Status)
		}
	})
}
