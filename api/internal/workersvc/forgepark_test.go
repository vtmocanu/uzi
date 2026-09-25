package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestSetStateUnknownRecoveryCauseRejected: an unknown non-nil recovery_cause is validated
// BEFORE any state SQL and rejected as ErrInvalidState (400), so a garbled cause can never
// reach the CHECK-constrained column. Uses the fake fixture (no DB): the validation is the
// first thing SetState does.
func TestSetStateUnknownRecoveryCauseRejected(t *testing.T) {
	run := runningRun(false)
	_, svc, wkr := limitParkFixture(t, run)

	_, applied, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{
		State: "recovery_wait", RecoveryCause: strPtr("not-a-real-cause"),
	})
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("SetState err = %v, want ErrInvalidState for an unknown recovery_cause", err)
	}
	if applied {
		t.Fatal("applied = true for a rejected unknown cause")
	}
}

// TestSetStateForgeParkNoTxBeginnerFailsSafe: when no transaction beginner is wired the atomic
// release+park cannot run, so a forge_unreachable report MUST NOT blind-park (which would leak
// the generation's hold) — it takes today's safe FAILED path instead (D3). Uses the fake
// fixture, which wires no tx beginner.
func TestSetStateForgeParkNoTxBeginnerFailsSafe(t *testing.T) {
	run := runningRun(false)
	fs, svc, wkr := limitParkFixture(t, run)
	if svc.txBeginner != nil {
		t.Fatal("fixture unexpectedly wired a tx beginner; this test needs the nil path")
	}

	if _, _, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{
		State: "recovery_wait", RecoveryCause: strPtr("forge_unreachable"), ClaimGeneration: i64Ptr(1),
	}); err != nil {
		t.Fatalf("SetState(forge park, no tx beginner): %v", err)
	}
	if fs.setFailed == nil {
		t.Fatal("SetRunFailed was never called — a forge park with no tx beginner must fail safe, not park")
	}
	if fs.setRecoveryWait != nil {
		t.Fatalf("SetRunRecoveryWait was called with no tx beginner — a forge park must never blind-park (leaks the hold)")
	}
}

// TestForgeUnreachableSkipsJudgeRegardlessOfIteration pins the judge exclusion mechanism:
// forge_unreachable is a member of neverJudgeFailOrigins (the skip that does NOT depend on
// iteration_count), so a run failed past the forge cap is not judged even when it resumed and
// did work (iteration_count > 0). It must ALSO be absent from preStartInfraFailOrigins — the
// iteration_count==0-gated set — else the regardless-of-iteration guarantee would be masked by
// the conjunct. Cheap, gate-visible, and the direct mutation guard for the "enqueues no judge"
// behaviour (SC3).
func TestForgeUnreachableSkipsJudgeRegardlessOfIteration(t *testing.T) {
	if !neverJudgeFailOrigins["forge_unreachable"] {
		t.Fatal("forge_unreachable is not in neverJudgeFailOrigins; a forge-cap failure would be judged")
	}
	if preStartInfraFailOrigins["forge_unreachable"] {
		t.Fatal("forge_unreachable is in preStartInfraFailOrigins; that would gate the skip on " +
			"iteration_count==0, judging a resumed run's forge cap-fail (iteration_count>0)")
	}
	// It must also be a real stored member (else the exclusion references a phantom origin).
	if !failOriginSet["forge_unreachable"] {
		t.Fatal("forge_unreachable is not in the fail_origin vocabulary")
	}
	// And it must NOT be worker-reportable (it is server-derived).
	if workerReportableFailOrigins["forge_unreachable"] {
		t.Fatal("forge_unreachable is worker-reportable; it must be server-derived only")
	}
}

// TestSetStateServerOnlyRecoveryCauseRejected (PRD #1590 C2): codex_account_unavailable is a
// SERVER-written cause (the exact-claim park). A worker reporting it through SetState is refused
// as ErrInvalidState before any state SQL, exactly like an unknown cause, so a worker can never
// park a run on the account hold the promoters treat specially.
func TestSetStateServerOnlyRecoveryCauseRejected(t *testing.T) {
	run := runningRun(false)
	fs, svc, wkr := limitParkFixture(t, run)

	_, applied, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{
		State: "recovery_wait", RecoveryCause: strPtr(recoveryCauseCodexAccountUnavailable), ClaimGeneration: i64Ptr(1),
	})
	// The server-only refusal, not the generic unknown-cause one: without the
	// serverRecoveryWaitCauses check the cause is still refused, but only as "unknown".
	if !errors.Is(err, ErrInvalidState) || !strings.Contains(err.Error(), "server-only") {
		t.Fatalf("SetState err = %v, want ErrInvalidState naming the cause server-only", err)
	}
	if applied || fs.setRecoveryWait != nil || fs.setFailed != nil {
		t.Fatalf("a refused server-only cause mutated the run: applied=%v recovery_wait=%v failed=%v",
			applied, fs.setRecoveryWait, fs.setFailed)
	}
	if recoveryWaitCauses[recoveryCauseCodexAccountUnavailable] {
		t.Fatal("codex_account_unavailable is in the worker-reportable set")
	}
}
