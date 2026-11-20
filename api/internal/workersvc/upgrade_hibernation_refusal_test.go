package workersvc

import (
	"strings"
	"testing"
	"time"
)

// A fresh stored controller refusal (size_refused, pvc_rejected) is the one signal that
// survives the pod-less early return: the worker has no pod because the controller refused to
// create one, so it classifies as a failed status carrying the refusal reason (PRD #4213).
func TestClassifyUpgradePodlessFreshRefusal(t *testing.T) {
	now := time.Now()
	classify := func(state string, sig *RollSignal, neverRegistered bool) (string, string) {
		return ClassifyUpgrade(UpgradeInput{
			Reported: "1.0.0", Kind: "hosted", CPVersion: "1.0.0", PinnedWorkerVersion: "1.0.0",
			Signal: sig, Now: now, APIStartedAt: now.Add(-24 * time.Hour), Hibernation: state,
			NeverRegistered: neverRegistered,
		}, UpgradeParams{})
	}
	for _, kind := range []string{BlockingKindSizeRefused, BlockingKindPVCRejected} {
		for _, state := range []string{"preparing", "hibernated", "waking"} {
			sig := &RollSignal{Phase: PhaseStuck, ObservedAt: now.Add(-time.Minute), BlockingKind: kind, BlockingReason: "refused: " + kind}
			status, detail := classify(state, sig, false)
			if status != UpgradeStatusUpgradeFailed || !strings.Contains(detail, "refused: "+kind) {
				t.Errorf("%s/%s: status=%q detail=%q, want upgrade_failed naming the reason", state, kind, status, detail)
			}
			if status, _ = classify(state, sig, true); status != UpgradeStatusProvisioningFailed {
				t.Errorf("%s/%s never registered: status=%q, want provisioning_failed", state, kind, status)
			}

			stale := *sig
			stale.ObservedAt = now.Add(-time.Hour)
			if status, _ = classify(state, &stale, false); status != UpgradeStatusUnknown {
				t.Errorf("%s/%s stale: status=%q, want unknown", state, kind, status)
			}
			settled := *sig
			settled.Phase = PhaseSettled
			if status, _ = classify(state, &settled, false); status != UpgradeStatusUnknown {
				t.Errorf("%s/%s settled: status=%q, want unknown", state, kind, status)
			}
		}
	}
	// A fresh stuck signal of any other kind stays pod-less unknown.
	other := &RollSignal{Phase: PhaseStuck, ObservedAt: now, BlockingKind: "something_else", BlockingReason: "x"}
	if status, _ := classify("hibernated", other, false); status != UpgradeStatusUnknown {
		t.Errorf("other kind: status=%q, want unknown", status)
	}
	// An external worker never takes the refusal path.
	sig := &RollSignal{Phase: PhaseStuck, ObservedAt: now, BlockingKind: BlockingKindSizeRefused}
	external, _ := ClassifyUpgrade(UpgradeInput{Kind: "external", CPVersion: "1.0.0", Signal: sig, Now: now, APIStartedAt: now, Hibernation: "hibernated"}, UpgradeParams{})
	if external != UpgradeStatusUnknown {
		t.Errorf("external: status=%q, want unknown", external)
	}
}
