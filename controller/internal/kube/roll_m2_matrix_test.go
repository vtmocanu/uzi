package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/preset"
	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

// These fixtures exercise simulated optimistic mutations, not a real apiserver.
func matrixFixture(t *testing.T, n int) (*Materializer, *fake.Clientset, []protocol.DesiredWorker, *fakeCordoner) {
	t.Helper()
	var objects []runtime.Object
	var workers []protocol.DesiredWorker
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("w%d", i)
		workers = append(workers, evidence(desired(id), m5Now))
		objects = append(objects, physicalOld(t, deployedWorker(id, 0, "old"))...)
	}
	cordon := &fakeCordoner{}
	m, c := newMatWithDrain(t, cordon, DrainPolicy{Deadline: time.Hour}, objects...)
	return m, c, workers, cordon
}

func matrixRestart(m *Materializer) *Materializer {
	next := New(m.client, m.cfg, m.resolver, m.cordoner, m.drain, m.recycle, m.log)
	next.now = m.now
	return next
}

func matrixUnknownTick(t *testing.T, m *Materializer, workers []protocol.DesiredWorker) {
	t.Helper()
	obs, err := m.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile(context.Background(), workers, obs); err == nil {
		t.Fatal("lost mutation response was reported as confirmed")
	}
}

func matrixNoSibling(t *testing.T, c *fake.Clientset) {
	t.Helper()
	for _, name := range templatePatches(c) {
		if name != deploymentName("w1") {
			t.Fatalf("unproven canary admitted sibling %s", name)
		}
	}
}

func TestM2MatrixRestartDelayedTemplateAndLostCommit(t *testing.T) {
	m, c, workers, _ := matrixFixture(t, 3)
	base := c.ReactionChain[1] // enforcingClient's deployment patch reactor
	var delayed k8stesting.Action
	armed := true
	c.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		p := a.(k8stesting.PatchAction)
		if armed && string(p.GetPatch()) != "" {
			// Only template mutations are delayed; metadata reservations really commit.
			if matrixTemplate(p) {
				if delayed == nil {
					delayed = a.DeepCopy()
				}
				return true, nil, fmt.Errorf("template response lost before commit")
			}
		}
		return false, nil, nil
	})
	matrixUnknownTick(t, m, workers)
	if delayed == nil {
		t.Fatal("template mutation never reached fake API")
	}
	m = matrixRestart(m)
	matrixUnknownTick(t, m, workers)
	matrixNoSibling(t, c)
	armed = false
	// Commit the original immutable request, then discard its response.
	handled, _, err := base.React(delayed)
	if !handled || err != nil {
		t.Fatalf("delayed template commit: handled=%t err=%v", handled, err)
	}
	rollTickTest(t, m, workers)
	matrixNoSibling(t, c)
	removeOldPod(t, c, "w1")
	currentProof(t, c, &workers[0], m5Now)
	workers[0].RollEvidence.PodUID = token("delayed-old-uid")
	rollTickTest(t, matrixRestart(m), workers)
	matrixNoSibling(t, c)
	workers[0].RollEvidence.PodUID = token("pod-uid-w1")
	rollTickTest(t, matrixRestart(m), workers)
	found := false
	for _, name := range templatePatches(c) {
		found = found || name == deploymentName("w2")
	}
	if !found {
		t.Fatal("authoritative committed template and current UID proof did not admit next worker")
	}
}

func TestM2MatrixCommittedTemplateLostResponse(t *testing.T) {
	m, c, workers, _ := matrixFixture(t, 3)
	base := c.ReactionChain[1]
	lost := false
	c.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if lost || !matrixTemplate(a.(k8stesting.PatchAction)) {
			return false, nil, nil
		}
		lost = true
		handled, _, err := base.React(a)
		if !handled || err != nil {
			t.Fatalf("committed template mutation: %v", err)
		}
		return true, nil, fmt.Errorf("committed template response lost")
	})
	matrixUnknownTick(t, m, workers)
	if !lost {
		t.Fatal("no committed template response was lost")
	}
	m = matrixRestart(m)
	rollTickTest(t, m, workers)
	matrixNoSibling(t, c)
	if len(templatePatches(c)) != 1 {
		t.Fatal("authoritative template was patched again after restart")
	}
	removeOldPod(t, c, "w1")
	currentProof(t, c, &workers[0], m5Now)
	rollTickTest(t, m, workers)
	if len(templatePatches(c)) != 2 {
		t.Fatal("current UID proof did not recover unknown committed outcome")
	}
}

func matrixTemplate(p k8stesting.PatchAction) bool {
	var body map[string]any
	if err := json.Unmarshal(p.GetPatch(), &body); err != nil {
		return false
	}
	_, ok := body["spec"]
	return ok
}

func TestM2MatrixUnknownReservation(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("committed=%t", committed), func(t *testing.T) {
			m, c, workers, cordon := matrixFixture(t, 3)
			for i := range workers {
				workers[i].Busy = true
			}
			base := c.ReactionChain[1]
			var delayed k8stesting.Action
			c.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
				if delayed != nil || matrixTemplate(a.(k8stesting.PatchAction)) {
					return false, nil, nil
				}
				delayed = a.DeepCopy()
				if committed {
					handled, _, err := base.React(a)
					if !handled || err != nil {
						t.Fatalf("reservation commit: %v", err)
					}
				}
				return true, nil, fmt.Errorf("reservation response lost")
			})
			matrixUnknownTick(t, m, workers)
			if len(cordon.calls) != 0 || len(templatePatches(c)) != 0 {
				t.Fatalf("unknown reservation caused destructive work: cordons=%v patches=%v", cordon.calls, templatePatches(c))
			}
			if !committed {
				handled, _, err := base.React(delayed)
				if !handled || err != nil {
					t.Fatalf("late reservation commit: %v", err)
				}
			}
			rollTickTest(t, matrixRestart(m), workers)
			if len(templatePatches(c)) != 0 {
				t.Fatal("late metadata reservation did more than consume busy canary slot")
			}
			if len(cordon.calls) != 1 || cordon.calls[0] != "w1" {
				t.Fatalf("authoritative reservation not re-cordoned: %v", cordon.calls)
			}
		})
	}
}

func TestM2MatrixProofFailures(t *testing.T) {
	for _, mode := range []string{"stale-heartbeat", "wrong-version", "wrong-owner-chain", "denied-rs"} {
		t.Run(mode, func(t *testing.T) {
			m, c, workers, _ := matrixFixture(t, 3)
			rollTickTest(t, m, workers)
			removeOldPod(t, c, "w1")
			currentProof(t, c, &workers[0], m5Now)
			switch mode {
			case "stale-heartbeat":
				at := m5Now.Add(-24 * time.Hour)
				workers[0].RollEvidence.HeartbeatAt = &at
				workers[0].RollEvidence.ValidUntil = m5Now.Add(-time.Second)
			case "wrong-version":
				workers[0].RollEvidence.RegisteredVersion = token("1.0.0+wrong")
			case "wrong-owner-chain":
				rs, err := c.AppsV1().ReplicaSets(m.cfg.Namespace).Get(context.Background(), "rs-w1", metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				rs.OwnerReferences[0].UID = "retired-deployment"
				if err := c.Tracker().Update(appsv1.SchemeGroupVersion.WithResource("replicasets"), rs, rs.Namespace); err != nil {
					t.Fatal(err)
				}
			case "denied-rs":
				c.PrependReactor("list", "replicasets", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, fmt.Errorf("replicaset list denied")
				})
			}
			obs, err := m.Observe(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			err = matrixRestart(m).Reconcile(context.Background(), workers, obs)
			if mode != "denied-rs" && err != nil {
				t.Fatal(err)
			}
			matrixNoSibling(t, c)
		})
	}
}

func TestM2MatrixForceHeldPoolAndEphemeral(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("force=%t", force), func(t *testing.T) {
			m, c, workers, cordon := matrixFixture(t, 4)
			workers[3].Ephemeral = true
			rollTickTest(t, m, workers)
			removeOldPod(t, c, "w1")
			currentProof(t, c, &workers[0], m5Now)
			targetWaiting(t, c, "w1", "ImagePullBackOff")
			since := m5Now.Add(-48 * time.Hour)
			for i := 1; i < 3; i++ {
				workers[i].Busy = true
				workers[i].DrainingSince = &since
				workers[i].DrainingReason = token("image_roll")
			}
			before := len(templatePatches(c))
			m = matrixRestart(m)
			m.drain.ForceRoll = force
			rollTickTest(t, m, workers)
			if force {
				if len(templatePatches(c)) != before+2 {
					t.Fatalf("force did not push held peers: %v", templatePatches(c))
				}
			} else {
				if len(templatePatches(c)) != before {
					t.Fatalf("deadline broke stuck hold: %v", templatePatches(c))
				}
				for _, id := range cordon.calls {
					if id == "w2" || id == "w3" {
						t.Fatalf("held sibling cordoned: %s", id)
					}
				}
			}
			count := 0
			for _, name := range templatePatches(c) {
				if name == deploymentName("w4") {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("ephemeral worker was paced or rolled twice: %d", count)
			}
		})
	}
}

func TestM2MatrixEligibilityFrozenSlot(t *testing.T) {
	for _, slots := range []string{"NULL", "zero", "positive"} {
		for _, protocolCap := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/protocol=%t", slots, protocolCap), func(t *testing.T) {
				m, c, workers, _ := matrixFixture(t, 3)
				// The controller consumes producer booleans, never recomputes nullable slots.
				for i := range workers {
					workers[i].RollEvidence.OrdinaryCoverage = slots != "positive"
					workers[i].RollEvidence.CheckerCoverage = slots != "zero"
					if protocolCap {
						workers[i].RollEvidence.ProtocolCapabilities = []string{"resume-v1"}
					}
				}
				rollTickTest(t, m, workers)
				removeOldPod(t, c, "w1")
				currentProof(t, c, &workers[0], m5Now)
				workers[0].RollEvidence.OrdinaryCoverage = slots != "positive"
				workers[0].RollEvidence.CheckerCoverage = slots != "zero"
				if protocolCap {
					workers[0].RollEvidence.ProtocolCapabilities = []string{"resume-v1"}
				}
				workers[0].RollEvidence.SchedulingCapabilities = []string{"new-capability"}
				rollTickTest(t, matrixRestart(m), workers)
				if len(templatePatches(c)) != 2 {
					t.Fatalf("added capability lost original slot: %v", templatePatches(c))
				}
			})
		}
	}
}

func TestM2MatrixOwnerAndLaneCapacity(t *testing.T) {
	for _, split := range []string{"owner", "lane"} {
		t.Run(split, func(t *testing.T) {
			m, c, workers, _ := matrixFixture(t, 4)
			m.cfg = dockerTestConfig()
			for i := 2; i < 4; i++ {
				if split == "owner" {
					workers[i].RollEvidence.OwnerID = "owner-B"
				} else {
					workers[i].Docker = true
					// Move the old physical fixtures to the Docker namespace.
					for _, resource := range []string{"deployments", "replicasets", "pods"} {
						gvr := appsv1.SchemeGroupVersion.WithResource(resource)
						if resource == "pods" {
							gvr = corev1.SchemeGroupVersion.WithResource(resource)
						}
						name := deploymentName(workers[i].ID)
						if resource == "replicasets" {
							name = "old-rs-" + name
						}
						if resource == "pods" {
							name = "old-pod-" + name
						}
						obj, err := c.Tracker().Get(gvr, m.cfg.Namespace, name)
						if err != nil {
							t.Fatal(err)
						}
						if err := c.Tracker().Delete(gvr, m.cfg.Namespace, name); err != nil {
							t.Fatal(err)
						}
						metadata, err := meta.Accessor(obj)
						if err != nil {
							t.Fatal(err)
						}
						metadata.SetNamespace(m.cfg.DockerNamespace)
						if err := c.Tracker().Create(gvr, obj, m.cfg.DockerNamespace); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			// A's last provider is unavailable; healthy B/Docker capacity cannot be borrowed.
			workers[1].RollEvidence.LivenessValidUntil = nil
			workers[1].Busy = true
			rollTickTest(t, m, workers)
			patches := templatePatches(c)
			if len(patches) != 1 || patches[0] != deploymentName("w3") {
				t.Fatalf("capacity crossed owner/lane or independent pool blocked: %v", patches)
			}
			rollTickTest(t, matrixRestart(m), workers)
			if len(templatePatches(c)) != 1 {
				t.Fatal("unproven independent canary admitted another worker")
			}
		})
	}
}

func TestM2MatrixABAOldUIDCannotPromote(t *testing.T) {
	m, c, workers, _ := matrixFixture(t, 3)
	rollTickTest(t, m, workers)
	removeOldPod(t, c, "w1")
	currentProof(t, c, &workers[0], m5Now)
	oldUID := *workers[0].RollEvidence.PodUID
	// Pin B then A without accepting delayed registration from the previous A pod.
	var err error
	m.resolver, err = preset.NewResolver("harbor.example.com/uzi", "v2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	m.cfg.RollTargetVersion = "2.0.0"
	rollTickTest(t, m, workers)
	m.resolver = testResolver(t)
	m.cfg.RollTargetVersion = "1.0.0"
	workers[0].RollEvidence.PodUID = &oldUID
	// The old A pod is gone: delayed API evidence cannot stand in for current ancestry.
	if err := c.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), m.cfg.Namespace, "pod-w1"); err != nil {
		t.Fatal(err)
	}
	before := len(templatePatches(c))
	rollTickTest(t, matrixRestart(m), workers)
	if len(templatePatches(c)) != before {
		t.Fatalf("A->B->A stale UID admitted a sibling: %v", templatePatches(c))
	}
}

func TestM2MatrixDelayedUncordonPinChange(t *testing.T) {
	m, c, workers, cordon := matrixFixture(t, 3)
	rollTickTest(t, m, workers)
	// A late image-roll cordon reaches a held old worker. Its release is sent,
	// but the API does not commit it until after the controller has restarted.
	since := m5Now.Add(-48 * time.Hour)
	workers[1].Busy = true
	workers[1].DrainingSince = &since
	workers[1].DrainingReason = token("image_roll")
	cordon.clearErr = fmt.Errorf("uncordon response lost; event pending")
	matrixUnknownTick(t, m, workers)
	if len(cordon.clearCalls) != 1 || cordon.clearCalls[0] != "w2" {
		t.Fatalf("held worker release events: %v", cordon.clearCalls)
	}
	matrixNoSibling(t, c)
	var oldTemplate []byte
	for _, action := range c.Actions() {
		if patch, ok := action.(k8stesting.PatchAction); ok && action.GetResource().Resource == "deployments" && matrixTemplate(patch) {
			oldTemplate = append([]byte{}, patch.GetPatch()...)
		}
	}
	if len(oldTemplate) == 0 {
		t.Fatal("missing old template mutation")
	}
	// A pin change may conditionally cancel the obsolete reservation. The new
	// resourceVersion must fence its delayed template write, and the delayed API
	// release is never replacement proof or permission to roll a sibling.
	var err error
	m.resolver, err = preset.NewResolver("harbor.example.com/uzi", "v2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	m.cfg.RollTargetVersion = "2.0.0"
	workers[1].DrainingSince, workers[1].DrainingReason = nil, nil
	cordon.clearErr = nil
	rollTickTest(t, matrixRestart(m), workers)
	matrixNoSibling(t, c)
	d, err := c.AppsV1().Deployments(m.cfg.Namespace).Get(context.Background(), deploymentName("w1"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AppsV1().Deployments(m.cfg.Namespace).Patch(context.Background(), d.Name, types.MergePatchType, oldTemplate, metav1.PatchOptions{}); !apierrors.IsConflict(err) {
		t.Fatalf("pin cancellation failed to fence obsolete template write: %v", err)
	}
	rollTickTest(t, matrixRestart(m), workers)
	matrixNoSibling(t, c)
	d, err = c.AppsV1().Deployments(m.cfg.Namespace).Get(context.Background(), d.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var marker rollMarker
	if err := json.Unmarshal([]byte(d.Annotations[annotationRoll]), &marker); err != nil {
		t.Fatal(err)
	}
	if marker.TargetVersion != "2.0.0" || marker.Completed {
		t.Fatalf("new target lacks an unresolved reservation: %+v", marker)
	}
	for _, id := range cordon.clearCalls {
		if id == "w1" {
			t.Fatal("unresolved admitted canary drain prematurely cleared")
		}
	}
}

func TestM2MatrixUnknownCordonStopsPoolAdmissions(t *testing.T) {
	m, c, workers, cordon := matrixFixture(t, 3)
	m.cfg.RollConcurrency = 2
	rollTickTest(t, m, workers)
	removeOldPod(t, c, "w1")
	currentProof(t, c, &workers[0], m5Now)
	workers[1].Busy, workers[2].Busy = true, true
	cordon.err = fmt.Errorf("cordon outcome unknown")
	matrixUnknownTick(t, m, workers)
	if len(cordon.calls) != 1 || cordon.calls[0] != "w2" {
		t.Fatalf("unknown cordon admitted another worker: %v", cordon.calls)
	}
	matrixNoSibling(t, c)
}

func TestM2MatrixDelayedDrainEventsRestart(t *testing.T) {
	m, c, workers, cordon := matrixFixture(t, 3)
	for i := range workers {
		workers[i].Busy = true
	}
	cordon.err = fmt.Errorf("cordon response lost; event pending")
	matrixUnknownTick(t, m, workers)
	if len(cordon.clearCalls) != 0 || len(templatePatches(c)) != 0 {
		t.Fatal("pending cordon was cleared or rolled")
	}
	// A delayed API cordon remains a busy defer across restart, even when old.
	since := m5Now.Add(-48 * time.Hour)
	workers[0].DrainingSince = &since
	workers[0].DrainingReason = token("image_roll")

	cordon.err = nil
	rollTickTest(t, matrixRestart(m), workers)
	if len(templatePatches(c)) != 0 {
		t.Fatal("delayed expired cordon authorized busy roll after restart")
	}
	// Once idle, the durable reservation permits only this canary, and restart
	// still cannot admit a sibling without replacement proof.
	workers[0].Busy = false
	rollTickTest(t, matrixRestart(m), workers)
	if len(templatePatches(c)) != 1 {
		t.Fatalf("idle reserved canary did not roll: %v", templatePatches(c))
	}
	rollTickTest(t, matrixRestart(m), workers)
	matrixNoSibling(t, c)
	if len(cordon.clearCalls) != 0 {
		t.Fatal("admitted image roll prematurely cleared drain")
	}
}
