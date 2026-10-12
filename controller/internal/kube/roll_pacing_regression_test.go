package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

// M1 recorded these three failures on the base revision. Additive API liveness
// and ordinary coverage make the fixture usable by M2 without changing its assertions.
func TestRollPacingRegression(t *testing.T) {
	for _, scenario := range []string{"idleN_must_roll_only1", "nonadmitted_busy_should_not_cordon", "stalled_canary_past_deadline_others_not_rolled"} {
		t.Run(scenario, func(t *testing.T) {
			var desired []protocol.DesiredWorker
			var observed []reconcile.ObservedWorker
			var objects []runtime.Object
			for i := range 3 {
				id := fmt.Sprintf("00000000-0000-0000-0000-%012x", i+1)
				podID := fmt.Sprintf("11111111-1111-1111-1111-%012x", i+1)
				wire, err := json.Marshal(map[string]any{
					"id": id, "template": "base", "size": "m", "generation": 0,
					"roll_evidence": map[string]any{
						"version": 1, "owner_id": "22222222-2222-2222-2222-222222222222",
						"registered": true, "registered_version": "0.1.0", "register_nonce": "prior-registration",
						"pod_uid": podID, "heartbeat_at": m5Now, "evaluated_at": m5Now,
						"valid_until":             m5Now.Add(time.Minute),
						"scheduling_capabilities": []string{}, "protocol_capabilities": []string{},
						"ordinary_coverage": true, "checker_coverage": false,
						"liveness_valid_until": m5Now.Add(time.Minute),
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				var w protocol.DesiredWorker
				if err := json.Unmarshal(wire, &w); err != nil {
					t.Fatal(err)
				}
				if scenario == "nonadmitted_busy_should_not_cordon" && i > 0 {
					w.Busy = true
				}
				hash := "old-image-hash"
				roll := reconcile.RollHealth{Phase: protocol.PhaseSettled, PodPhase: "Running"}
				if scenario == "stalled_canary_past_deadline_others_not_rolled" {
					at := m5Now.Add(-48 * time.Hour)
					w.Busy, w.DrainingSince = true, &at
					if i == 0 {
						spec, err := testResolver(t).Resolve(w.Template, w.Size)
						if err != nil {
							t.Fatal(err)
						}
						hash = SpecHashOf(testConfig(), w, spec)
						roll = reconcile.RollHealth{Phase: protocol.PhaseStuck, BlockingContainer: "worker", BlockingReason: "ImagePullBackOff"}
					}
				}
				desired = append(desired, w)
				observed = append(observed, reconcile.ObservedWorker{
					ID: id, HasDeployment: true, SpecHash: hash, Roll: roll,
					PodsKnown: true, WorkerPodCount: 1, ReadyPodUID: podID,
				})
				objects = append(objects, physicalOld(t, deployedWorker(id, 0, hash))...)
			}
			c := &fakeCordoner{}
			m, client := newMatWithDrain(t, c, DrainPolicy{Deadline: time.Hour}, objects...)
			if err := m.Reconcile(context.Background(), desired, observed); err != nil {
				t.Fatal(err)
			}
			patches := 0
			for _, action := range client.Actions() {
				if action.GetVerb() != "patch" || action.GetResource().Resource != "deployments" {
					continue
				}
				var body map[string]json.RawMessage
				if err := json.Unmarshal(action.(k8stesting.PatchAction).GetPatch(), &body); err != nil {
					t.Fatal(err)
				}
				if _, templateMutation := body["spec"]; templateMutation {
					patches++
				}
			}
			switch scenario {
			case "idleN_must_roll_only1":
				if patches != 1 {
					t.Fatalf("idle fleet rolled %d workers; want exactly one admitted canary", patches)
				}
			case "nonadmitted_busy_should_not_cordon":
				if len(c.calls) != 0 {
					t.Fatalf("nonadmitted busy workers cordoned: %v", c.calls)
				}
			case "stalled_canary_past_deadline_others_not_rolled":
				if patches != 0 {
					t.Fatalf("stalled canary deadline rolled %d siblings; all must remain held", patches)
				}
			}
		})
	}
}
