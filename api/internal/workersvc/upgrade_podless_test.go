package workersvc

import (
	"testing"
	"time"
)

// Issue #148 on THIS side of the wire: what the classifier does with the report a
// pod-less, permanently-blocked hosted worker now produces.
//
// The controller half of the fix is in controller/internal/kube/rollhealth.go — a
// pod-less worker whose Deployment asserts ReplicaFailure=True reports `stuck` instead of
// `rolling`. Nothing in the api changed for it, and that is the claim worth pinning: the
// row it produces must reach `upgrade_failed` and the attention set through the SHIPPED
// decision table, with no container, no pod phase and no phase_since to work from.
//
// If the api needed a change too, these tests are where that would have surfaced.

// podlessBlocked is the row a permanently-blocked hosted worker produces after the fix.
//
// Every display field the pod path fills is EMPTY here, because there is no pod: no
// container to name, no pod phase to report, no Ready condition to date anything from.
// That is the point of the fixture — the api's failed-worker sentence has to survive a
// `stuck` row with nothing in it but a reason.
func podlessBlocked(at time.Time, anchor *time.Time) *RollSignal {
	return &RollSignal{
		Phase:          PhaseStuck,
		ObservedAt:     at,
		UpgradingSince: anchor,
		RolledTag:      "0.11.8",
		BlockingReason: "FailedCreate",
		// PhaseSince, PodPhase, BlockingContainer, LastExitCode: all absent. RestartCount 0.
	}
}

// podlessInput is the worker itself: hosted, and reporting NO version, because it has
// never run and therefore never registered.
//
// The empty Reported is load-bearing rather than realistic-looking. It is what sends the
// version-compare fallback to R5 `unknown` — the second half of #148's silence — so any
// test here that stopped exercising the controller rows would fall into `unknown`, not
// into `outdated`. A fixture reporting an old version would hide that.
func podlessInput(now time.Time, sig *RollSignal) UpgradeInput {
	return UpgradeInput{
		Reported:     "",
		Kind:         "hosted",
		CPVersion:    "0.11.8",
		Signal:       sig,
		Now:          now,
		APIStartedAt: now.Add(-24 * time.Hour), // long ago, so R8's grace explains nothing
	}
}

// The fix, end to end through the shipped decision table — and its control is the
// PRE-FIX report, which is the only thing that makes this test evidence.
//
// A test asserting only that `stuck` ⇒ `upgrade_failed` would pass against the api
// exactly as it shipped, and #148 was never an api bug. What it has to show is that the
// change in the CONTROLLER's answer is what changes the alert: same worker, same clock,
// same everything, one field different on the wire.
func TestPodlessBlockedWorkerReachesTheAttentionSet(t *testing.T) {
	now := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	anchor := now.Add(-30 * time.Second)

	// PRE-FIX: the pod-less branch reported `rolling` with no reason at all. This is the
	// bug, reproduced on this side.
	before := &RollSignal{
		Phase: PhaseRolling, ObservedAt: now, UpgradingSince: &anchor, RolledTag: "0.11.8",
	}
	beforeStatus, beforeDetail := ClassifyUpgrade(podlessInput(now, before), ceilingParams())
	if beforeStatus != UpgradeStatusUpgrading {
		t.Fatalf("precondition: the pre-fix report must classify %q (that is the bug); got %q. If this "+
			"moved, the control below is no longer measuring what #148 was.",
			UpgradeStatusUpgrading, beforeStatus)
	}
	if InUpgradeAttentionSet(beforeStatus) {
		t.Fatalf("precondition: %q must NOT be in the attention set — Decision 1 excludes it, and that "+
			"exclusion is correct and stays. #148 is that a broken worker inherited it, not that the "+
			"exclusion is wrong.", beforeStatus)
	}
	t.Logf("pre-fix (controller says rolling):  status=%q detail=%q attention=%v",
		beforeStatus, beforeDetail, InUpgradeAttentionSet(beforeStatus))

	// POST-FIX: the same worker, the same instant, `stuck` with the condition's reason.
	after := podlessBlocked(now, &anchor)
	status, detail := ClassifyUpgrade(podlessInput(now, after), ceilingParams())
	t.Logf("post-fix (controller says stuck):   status=%q detail=%q attention=%v",
		status, detail, InUpgradeAttentionSet(status))

	if status != UpgradeStatusUpgradeFailed {
		t.Errorf("status = %q, want %q. R1 must fire for a `stuck` row even though it carries no "+
			"container, no pod phase and no phase_since — a worker that never got a pod has none of "+
			"those, and requiring any of them would put the fix back in the blind spot.",
			status, UpgradeStatusUpgradeFailed)
	}
	if !InUpgradeAttentionSet(status) {
		t.Errorf("%q is not in the attention set, so the nav badge still does not count this worker — "+
			"which is the whole of issue #148", status)
	}
	if status == beforeStatus {
		t.Fatalf("the pre-fix and post-fix reports both classify %q. Whatever this classifier keys on, "+
			"it is not the phase, and the controller fix cannot reach the badge through it.", status)
	}
	// The sentence an operator reads. "pod" rather than a container name is stuckDetail's
	// existing fallback and it is the honest subject here: no container was ever created.
	if want := "pod: FailedCreate"; detail != want {
		t.Errorf("detail = %q, want %q. This string is the operator's only statement of WHY there is no "+
			"pod, and it reaches a terminal through api/cmd/uzi/worker.go.", detail, want)
	}
}

// THE RESIDUAL HALF OF #148, NOW CLOSED BY #151. This function replaces
// TestPodlessBlockedWorkerGoesSilentAgainPastTheCeiling, which pinned the gap and whose
// own comment instructed whoever closed #151 to delete it in the same commit. Same
// scenario, same fixtures, inverted expectation — kept rather than dropped, because the
// silence it documented is the exact regression a future re-gating of R1 would reintroduce.
//
// The gap it pinned, for the record: the ceiling used to gate R1 as well as R2, so past
// MaxUpgradingWindow the api stopped believing the controller about a worker it was
// asserting is BROKEN. The fallback is version compare; a worker that never registered
// reports "" and R5 answers `unknown`, which is not in the attention set. So #148's fix
// alerted for the remainder of the window and then went silent again, permanently.
//
// MEASURED ON A REAL CLUSTER (#151, dev-cluster, 0.11.8), which is what made it urgent
// rather than theoretical: the anchor armed at 20:10:28 while the worker was pod-less and
// nothing was yet wrong, the pod finally appeared at 20:44:18 having spent 33m50s of the
// budget, the age arm could not fire before 20:54:18, and the ceiling expired at 20:55:28 —
// a 70-SECOND window out of 45 minutes in which `upgrade_failed` was reachable at all. The
// flip to `unknown` was observed 73 seconds after the correct `upgrade_failed`, while the DB
// row still said `phase=stuck`.
//
// Note what the fix does NOT do, because it bounds what this test proves: the anchor still
// arms on the first non-terminal report and still clears only on a version move. R2 remains
// gated on it, so a worker that blipped once can still badge `outdated` on a later healthy
// roll. That is a separate defect in the same anchor, tracked on its own.
func TestPodlessBlockedWorkerStaysLoudPastTheCeiling(t *testing.T) {
	t0 := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	anchor := t0
	p := ceilingParams()

	// Inside the window: loud, as the test above establishes.
	inside := t0.Add(p.MaxUpgradingWindow - time.Minute)
	status, detail := ClassifyUpgrade(podlessInput(inside, podlessBlocked(inside, &anchor)), p)
	if status != UpgradeStatusUpgradeFailed {
		t.Fatalf("inside the window: status = %q, want %q — without this the assertion below is "+
			"vacuous", status, UpgradeStatusUpgradeFailed)
	}
	t.Logf("at anchor+%v: status=%q detail=%q attention=%v",
		p.MaxUpgradingWindow-time.Minute, status, detail, InUpgradeAttentionSet(status))

	// Past it: the controller is still reporting `stuck` every poll, freshly — and is
	// still believed. Two multiples so this cannot pass by landing just inside some other
	// bound; the second is 24x the window.
	for _, elapsed := range []time.Duration{p.MaxUpgradingWindow + time.Minute, 24 * p.MaxUpgradingWindow} {
		past := t0.Add(elapsed)
		status, detail = ClassifyUpgrade(podlessInput(past, podlessBlocked(past, &anchor)), p)
		t.Logf("at anchor+%v: status=%q detail=%q attention=%v",
			elapsed, status, detail, InUpgradeAttentionSet(status))

		if status != UpgradeStatusUpgradeFailed {
			t.Errorf("at anchor+%v: status = %q, want %q. The alert expired — this is #148's residual "+
				"reopening, which means ceilingOK is gating R1 again.",
				elapsed, status, UpgradeStatusUpgradeFailed)
		}
		if !InUpgradeAttentionSet(status) {
			t.Errorf("at anchor+%v: %q is not in the attention set, so nobody is told about a worker "+
				"that cannot get a pod.", elapsed, status)
		}
		if detail != "pod: FailedCreate" {
			t.Errorf("at anchor+%v: the diagnosis must survive the ceiling with the alert, got %q",
				elapsed, detail)
		}
	}
}
