package kube

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

// currentProof replaces RollEvidence, so restore the shared, nonempty requirements
// after every call. These are API capability names, not inferred pod features.
func terminalCaps(w *protocol.DesiredWorker) {
	w.RollEvidence.SchedulingCapabilities = []string{"docker"}
	w.RollEvidence.ProtocolCapabilities = []string{"resume-v1"}
}

func terminalPodUpdate(t *testing.T, c *fake.Clientset, name string, phase corev1.PodPhase) {
	t.Helper()
	p, err := c.CoreV1().Pods(testConfig().Namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p.Status.Phase = phase
	if phase == corev1.PodFailed || phase == corev1.PodSucceeded {
		reason := "Completed"
		if phase == corev1.PodFailed {
			reason = "ContainerStatusUnknown"
		}
		p.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: reason}}
	}
	if phase == corev1.PodRunning || phase == corev1.PodUnknown {
		p.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	}
	if phase == corev1.PodPending {
		p.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}
	}
	if p.DeletionTimestamp != nil {
		t.Fatal("fixture must retain residue without a deletion timestamp")
	}
	if err := c.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), p, p.Namespace); err != nil {
		t.Fatal(err)
	}
}

func terminalResidue(t *testing.T, c *fake.Clientset, phase corev1.PodPhase) {
	t.Helper()
	name := "old-rs-" + deploymentName("w1")
	rs, err := c.AppsV1().ReplicaSets(testConfig().Namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	zero := int32(0)
	rs.Spec.Replicas = &zero
	rs.Status = appsv1.ReplicaSetStatus{}
	if err := c.Tracker().Update(appsv1.SchemeGroupVersion.WithResource("replicasets"), rs, rs.Namespace); err != nil {
		t.Fatal(err)
	}
	terminalPodUpdate(t, c, "old-pod-"+deploymentName("w1"), phase)
}

// Tick N reserves and mutates A only. Between ticks the apiserver retains the
// scaled-zero old ReplicaSet/pod while a distinct target incarnation registers.
func terminalAdmittedFixture(t *testing.T, phase corev1.PodPhase) (*Materializer, *fake.Clientset, []protocol.DesiredWorker, *fakeCordoner) {
	t.Helper()
	m, c, workers, cordon := matrixFixture(t, 2)
	m.cfg.RollConcurrency = 1
	for i := range workers {
		terminalCaps(&workers[i])
	}
	rollTickTest(t, m, workers)
	if got := templatePatches(c); len(got) != 1 || got[0] != deploymentName("w1") {
		t.Fatalf("tick N must admit only A: %v", got)
	}
	terminalNoReservation(t, c, "w2")
	terminalResidue(t, c, phase)
	currentProof(t, c, &workers[0], m5Now)
	terminalCaps(&workers[0])
	return m, c, workers, cordon
}

func terminalNoReservation(t *testing.T, c *fake.Clientset, id string) {
	t.Helper()
	d, err := c.AppsV1().Deployments(testConfig().Namespace).Get(context.Background(), deploymentName(id), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Annotations[annotationRoll] != "" {
		t.Fatalf("%s unexpectedly reserved: %s", id, d.Annotations[annotationRoll])
	}
}

func terminalProgress(t *testing.T, phase corev1.PodPhase) {
	t.Helper()
	m, c, workers, _ := terminalAdmittedFixture(t, phase)
	// A is the sole other provider for B's owner/lane and required capabilities.
	for tick := 1; tick <= 2; tick++ {
		rollTickTest(t, m, workers)
		if p := m.PacingOutcomes()["w1"]; p == nil || p.State != "active" {
			t.Fatalf("tick N+%d A not promoted/active: %+v", tick, p)
		}
		want := "active"
		if tick == 2 {
			// B's committed replacement still awaits its own registration.
			want = "waiting_evidence"
		}
		if p := m.PacingOutcomes()["w2"]; p == nil || p.State != want {
			t.Fatalf("tick N+%d B progress lost: %+v", tick, p)
		}
		d, err := c.AppsV1().Deployments(m.cfg.Namespace).Get(context.Background(), deploymentName("w2"), metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var marker rollMarker
		if err := json.Unmarshal([]byte(d.Annotations[annotationRoll]), &marker); err != nil {
			t.Fatal(err)
		}
		if marker.Completed || marker.Hash != d.Spec.Template.Annotations[AnnotationSpecHash] || marker.Image != workerImage(d) {
			t.Fatalf("tick N+%d B reservation/template progress lost: %+v", tick, marker)
		}
		if got := templatePatches(c); len(got) != 2 || got[0] != deploymentName("w1") || got[1] != deploymentName("w2") {
			t.Fatalf("tick N+%d did not preserve progress: %v", tick, got)
		}
	}
	old, err := c.CoreV1().Pods(m.cfg.Namespace).Get(context.Background(), "old-pod-"+deploymentName("w1"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if old.Status.Phase != phase || old.DeletionTimestamp != nil {
		t.Fatalf("progress discarded terminal residue: phase=%s deleting=%v", old.Status.Phase, old.DeletionTimestamp)
	}
	rs, err := c.AppsV1().ReplicaSets(m.cfg.Namespace).Get(context.Background(), "old-rs-"+deploymentName("w1"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rs.Spec.Replicas == nil || *rs.Spec.Replicas != 0 {
		t.Fatal("old ReplicaSet no longer explicitly scaled zero")
	}
}

func TestRollTerminalResidueProgress(t *testing.T) {
	for _, phase := range []corev1.PodPhase{corev1.PodFailed, corev1.PodSucceeded} {
		t.Run(string(phase), func(t *testing.T) { terminalProgress(t, phase) })
	}
}

func TestRollTerminalResidueRefusals(t *testing.T) {
	for _, mode := range []string{
		"all-terminal", "extra-Pending", "extra-Running", "extra-Unknown",
		"terminal-proof-uid", "missing-heartbeat", "expired-equal", "expired-before",
		"unchanged-nonce", "missing-nonce", "custody-drain",
	} {
		t.Run(mode, func(t *testing.T) {
			// An independent positive tick sequence precedes each refusal so a
			// broken admission fixture cannot make these negative checks vacuous.
			terminalProgress(t, corev1.PodFailed)
			m, c, workers, cordon := terminalAdmittedFixture(t, corev1.PodFailed)
			claim, err := c.CoreV1().PersistentVolumeClaims(m.cfg.Namespace).Get(context.Background(), dataPVCName("w1"), metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "all-terminal":
				terminalPodUpdate(t, c, "pod-w1", corev1.PodSucceeded)
			case "extra-Pending":
				terminalPodUpdate(t, c, "old-pod-"+deploymentName("w1"), corev1.PodPending)
			case "extra-Running":
				terminalPodUpdate(t, c, "old-pod-"+deploymentName("w1"), corev1.PodRunning)
			case "extra-Unknown":
				terminalPodUpdate(t, c, "old-pod-"+deploymentName("w1"), corev1.PodUnknown)
			case "terminal-proof-uid":
				workers[0].RollEvidence.PodUID = token("pod-" + deploymentName("w1"))
			case "missing-heartbeat":
				workers[0].RollEvidence.HeartbeatAt = nil
			case "expired-equal", "expired-before":
				workers[0].RollEvidence.ValidUntil = workers[0].RollEvidence.EvaluatedAt
				if mode == "expired-before" {
					workers[0].RollEvidence.ValidUntil = m5Now.Add(-time.Second)
				}
				// Liveness remains independently valid; only promotion evidence expires.
				if workers[0].RollEvidence.LivenessValidUntil == nil || !workers[0].RollEvidence.LivenessValidUntil.After(m5Now) {
					t.Fatal("invalid independent liveness fixture")
				}
			case "unchanged-nonce":
				workers[0].RollEvidence.RegisterNonce = token("prior-registration")
			case "missing-nonce":
				workers[0].RollEvidence.RegisterNonce = nil
			case "custody-drain":
				workers[0].CustodyHeld = true
				workers[0].DrainingSince = recentDrain()
				workers[0].DrainingReason = token("image_roll")
			}
			for tick := 1; tick <= 2; tick++ {
				// An extra old-hash pod must exercise multiplicity rather than
				// an unrelated stuck-target health veto.
				obs, err := m.Observe(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, o := range obs {
					if o.ID == "w1" {
						found = true
						if o.Roll.Phase == protocol.PhaseStuck {
							t.Fatalf("control accidentally stuck: %+v", o.Roll)
						}
					}
				}
				if !found {
					t.Fatal("A missing from observation")
				}
				rollTickTest(t, m, workers)
				if got := templatePatches(c); len(got) != 1 || got[0] != deploymentName("w1") {
					t.Fatalf("refusal released slot/admitted B: %v", got)
				}
				terminalNoReservation(t, c, "w2")
				if p := m.PacingOutcomes()["w1"]; p == nil || p.State == "active" {
					t.Fatalf("invalid proof promoted A: %+v", p)
				}
				d, err := c.AppsV1().Deployments(m.cfg.Namespace).Get(context.Background(), deploymentName("w1"), metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				var marker rollMarker
				if err := json.Unmarshal([]byte(d.Annotations[annotationRoll]), &marker); err != nil {
					t.Fatal(err)
				}
				if marker.Completed {
					t.Fatal("invalid proof completed reservation")
				}
				if len(cordon.calls) != 0 || len(cordon.clearCalls) != 0 {
					t.Fatalf("invalid proof changed drains: requests=%v clears=%v", cordon.calls, cordon.clearCalls)
				}
				retained, err := c.CoreV1().PersistentVolumeClaims(m.cfg.Namespace).Get(context.Background(), claim.Name, metav1.GetOptions{})
				if err != nil || retained.UID != claim.UID {
					t.Fatalf("protected PVC UID changed: %v", err)
				}
			}
		})
	}
}

// A has already reached the current template with no reservation history. This
// exercises currentUsable through real sibling admission, independently of canary
// promotion. The negative case asserts the actual unmarked-template veto.
func TestRollTerminalResidueUnmarkedCurrentProvider(t *testing.T) {
	for _, phase := range []corev1.PodPhase{corev1.PodFailed, corev1.PodSucceeded} {
		t.Run(string(phase), func(t *testing.T) {
			for _, terminalOnly := range []bool{false, true} {
				name := "running-target"
				if terminalOnly {
					name = "terminal-only"
				}
				t.Run(name, func(t *testing.T) {
					workers := []protocol.DesiredWorker{evidence(desired("w1"), m5Now), evidence(desired("w2"), m5Now)}
					var objects []runtime.Object
					for i := range workers {
						terminalCaps(&workers[i])
						d := deployedWorker(workers[i].ID, 0, "old")
						old := physicalOld(t, d)
						if i == 0 {
							target := RenderDeployment(testConfig(), workers[i], testSpec(t, workers[i].Template, workers[i].Size))
							d.Spec.Template = target.Spec.Template
						}
						objects = append(objects, old...)
					}
					cordon := &fakeCordoner{}
					m, c := newMatWithDrain(t, cordon, DrainPolicy{Deadline: time.Hour}, objects...)
					m.cfg.RollConcurrency = 1
					terminalResidue(t, c, phase)
					currentProof(t, c, &workers[0], m5Now)
					terminalCaps(&workers[0])
					if terminalOnly {
						terminalPodUpdate(t, c, "pod-w1", phase)
					}
					for tick := 0; tick < 2; tick++ {
						rollTickTest(t, m, workers)
						terminalNoReservation(t, c, "w1")
						if terminalOnly {
							if len(templatePatches(c)) != 0 || len(cordon.calls) != 0 {
								t.Fatal("terminal-only unmarked provider admitted peer")
							}
							terminalNoReservation(t, c, "w2")
							p := m.PacingOutcomes()["w1"]
							if p == nil || p.BlockingReason == nil || *p.BlockingReason != "target template has no reservation proof" {
								t.Fatalf("missing actual unmarked-template veto: %+v", p)
							}
						} else if got := templatePatches(c); len(got) != 1 || got[0] != deploymentName("w2") {
							t.Fatalf("current provider did not supply peer capacity/coverage: %v", got)
						}
					}
					// The target pod is physically distinct from retained residue.
					p, err := c.CoreV1().Pods(m.cfg.Namespace).Get(context.Background(), "pod-w1", metav1.GetOptions{})
					if err != nil || p.UID == types.UID("pod-"+deploymentName("w1")) {
						t.Fatalf("target identity fixture invalid: %v", err)
					}
				})
			}
		})
	}
}
