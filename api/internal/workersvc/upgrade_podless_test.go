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

// THE RESIDUAL HALF OF #148, PINNED RATHER THAN FIXED — read this before treating the
// issue as closed.
//
// MEASURED, not predicted: past MaxUpgradingWindow the INV-5 ceiling stops gating R1 as
// well as R2, so the api stops believing the controller about a worker it is asserting is
// BROKEN. The fallback is version compare, the worker never registered, and R5 answers
// `unknown` — which is not in the attention set. So the fix above alerts for
// MaxUpgradingWindow and then goes silent again, permanently.
//
// Why this is not fixed here. The ceiling is owner-accepted (Decision 7 / INV-5) and it is
// correct for R2, which SUPPRESSES `outdated`: bounding how long a controller may suppress
// an alert is the whole reason it exists. R1 is the opposite kind of row — it RAISES an
// alert — so applying the same bound makes the system quieter the longer a worker stays
// broken. Splitting the two is a design call on an invariant this branch was told not to
// weaken, and it changes what a compromised controller can do (it could then badge the
// fleet `upgrade_failed` indefinitely, where today every claim it makes expires).
//
// Pinned so the gap is a fact in the suite rather than a surprise, and so a later fix
// REDDENS this test deliberately instead of finding it by accident. If you are here
// because this test failed: that is probably correct — delete it and say so.
func TestPodlessBlockedWorkerGoesSilentAgainPastTheCeiling(t *testing.T) {
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

	// Past it: the controller is still reporting `stuck` every poll, freshly.
	past := t0.Add(p.MaxUpgradingWindow + time.Minute)
	status, detail = ClassifyUpgrade(podlessInput(past, podlessBlocked(past, &anchor)), p)
	t.Logf("at anchor+%v: status=%q detail=%q attention=%v",
		p.MaxUpgradingWindow+time.Minute, status, detail, InUpgradeAttentionSet(status))

	if status != UpgradeStatusUnknown {
		t.Errorf("past the ceiling: status = %q, want %q. This test PINS a known gap rather than a "+
			"desired behaviour — if the ceiling was split so R1 survives it, this is the test that "+
			"should have been deleted in the same commit.", status, UpgradeStatusUnknown)
	}
	if InUpgradeAttentionSet(status) {
		t.Errorf("past the ceiling: %q is in the attention set, which would mean the residual gap this "+
			"test documents is closed. Good — remove the test.", status)
	}
}
