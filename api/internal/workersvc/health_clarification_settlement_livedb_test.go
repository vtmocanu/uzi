package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// Issue #3985: SettleRunClarificationToGate returns a run to awaiting_approval with
// status_since = now() while the human revise that preceded the clarification stays pending. The
// approval-idle arm of healthTargetFor must keep reading that run as waiting on its WORKER (the
// lead's next presentation applies the revise), not on its owner, however old the gate gets.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// ./e2e/run-store-it.sh. A package that prints `ok` with PASS=0 is INVALID, not green.

func (f *gateFix) crsHealthTarget(id uuid.UUID) (string, string) {
	f.t.Helper()
	rows, err := f.q.ListActiveRunsForHealth(f.ctx, codexCuratedModelsSlice())
	if err != nil {
		f.t.Fatalf("ListActiveRunsForHealth: %v", err)
	}
	for _, r := range rows {
		if uuid.UUID(r.ID) == id {
			return f.svc.healthTargetFor(f.ctx, time.Now(), r, healthThresholds{approval: time.Hour})
		}
	}
	f.t.Fatalf("run %v not in ListActiveRunsForHealth", id)
	return "", ""
}

// crsAgeGate makes the gate episode 2h old and the given human inputs older than it, the way a
// revise submitted before the settlement is older than the settlement's status_since.
func (f *gateFix) crsAgeGate(id uuid.UUID) {
	f.t.Helper()
	f.exec(`UPDATE runs SET status_since = now() - interval '2 hours' WHERE id = $1`, id)
	f.exec(`UPDATE run_user_inputs SET created_at = now() - interval '3 hours' WHERE run_id = $1`, id)
}

func TestClarificationSettlementHealthWaitingWorkerLiveDB(t *testing.T) {
	f := newGateFix(t, 3)

	t.Run("a settled run with its revise still pending reads waiting_worker past the approval threshold", func(t *testing.T) {
		r := f.crsParked("gated")
		f.crsBackdate(r.id)
		if _, applied, err := f.crsSettle(r); err != nil || !applied {
			t.Fatalf("settle = (applied %v, err %v)", applied, err)
		}
		f.crsAgeGate(r.id)
		flag, reason := f.crsHealthTarget(r.id)
		if flag != healthWaitingWorker || reason != reasonVerdictUndelivered {
			t.Fatalf("health = (%q, %q), want (%q, %q)", flag, reason, healthWaitingWorker, reasonVerdictUndelivered)
		}

		// Once the lead's next presentation applies the revise, nothing is pending: an idle owner.
		f.exec(`UPDATE run_user_inputs SET applied_at = now() WHERE id = $1`, r.revise)
		flag, reason = f.crsHealthTarget(r.id)
		if flag != healthApprovalIdle || reason != reasonApprovalIdle {
			t.Fatalf("after the revise was applied: health = (%q, %q), want (%q, %q)", flag, reason, healthApprovalIdle, reasonApprovalIdle)
		}
	})

	t.Run("control: an old gate with no pending revise still reads approval_idle", func(t *testing.T) {
		id := f.crsNewGatedRun("gated")
		f.crsAgeGate(id)
		flag, reason := f.crsHealthTarget(id)
		if flag != healthApprovalIdle || reason != reasonApprovalIdle {
			t.Fatalf("health = (%q, %q), want (%q, %q)", flag, reason, healthApprovalIdle, reasonApprovalIdle)
		}
	})

	t.Run("control: a pending revise bound to an older gate revision does not count", func(t *testing.T) {
		r := f.crsParked("gated")
		f.crsBackdate(r.id)
		if _, applied, err := f.crsSettle(r); err != nil || !applied {
			t.Fatalf("settle = (applied %v, err %v)", applied, err)
		}
		f.exec(`UPDATE runs SET gate_revision = gate_revision + 1 WHERE id = $1`, r.id)
		f.crsAgeGate(r.id)
		flag, reason := f.crsHealthTarget(r.id)
		if flag != healthApprovalIdle || reason != reasonApprovalIdle {
			t.Fatalf("health = (%q, %q), want (%q, %q)", flag, reason, healthApprovalIdle, reasonApprovalIdle)
		}
	})
}
