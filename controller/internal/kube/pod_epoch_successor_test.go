package kube

import (
	"context"
	"errors"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	ktesting "k8s.io/client-go/testing"
)

// epochTerminalPrefixed is epochTerminal with a chosen runtime prefix.
func epochTerminalPrefixed(p *corev1.Pod, prefix string) *corev1.Pod {
	p = epochTerminal(p)
	p.Status.ContainerStatuses[0].ContainerID = prefix + "worker"
	p.Status.InitContainerStatuses[0].ContainerID = prefix + "seed"
	return p
}

// epochReadyHarness has A terminal and gone, ready for successor B's Added.
func epochReadyHarness(t *testing.T, x *corev1.Pod) *epochHarness {
	t.Helper()
	h := newEpochHarness(t, epochPod(epochA))
	h.event(watch.Modified, x)
	return h
}

// epochDeclareRuntime points the worker Deployment, its ReplicaSet and pod p at
// the runtime so the live identity resolver accepts p.
func epochDeclareRuntime(t *testing.T, h *epochHarness, p *corev1.Pod, runtime string) {
	t.Helper()
	set := func(s *corev1.PodSpec) {
		for i := range s.Containers {
			for j := range s.Containers[i].Env {
				if s.Containers[i].Env[j].Name == PodEpochRuntimeEnv {
					s.Containers[i].Env[j].Value = runtime
				}
			}
		}
	}
	ctx := context.Background()
	dep, err := h.client.AppsV1().Deployments("docker").Get(ctx, deploymentName("w"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	set(&dep.Spec.Template.Spec)
	if _, err := h.client.AppsV1().Deployments("docker").Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	rs, err := h.client.AppsV1().ReplicaSets("docker").Get(ctx, "rs", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	set(&rs.Spec.Template.Spec)
	if _, err := h.client.AppsV1().ReplicaSets("docker").Update(ctx, rs, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	set(&p.Spec)
}

func epochReason(t *testing.T, d *PodEpochDecision) (*PodEpochEvidence, *PodEpochNegative) {
	t.Helper()
	if d == nil {
		t.Fatal("missing decision")
	}
	e, n, err := DecodePodEpochDocument([]byte(d.Document()))
	if err != nil {
		t.Fatal(err)
	}
	return e, n
}

func TestPodEpochSuccessorPositiveWaitsForContainerIDs(t *testing.T) {
	h := epochReadyHarness(t, epochTerminal(epochPod(epochA)))
	if d := h.event(watch.Added, epochPod(epochB)); d != nil {
		t.Fatalf("positive emitted at Added: %s", d.Document())
	}
	if d := h.event(watch.Modified, withRV(epochPod(epochB), "/no-ids")); d != nil {
		t.Fatal("emitted without container IDs")
	}
	if len(h.patches) != 0 {
		t.Fatal("request before IDs")
	}
	d := h.event(watch.Modified, epochWithIDs(epochPod(epochB), "containerd://"))
	e := h.assertDecision(d, true)
	if e.Version != 2 || e.Attestation == nil || *e.Attestation != (PodEpochAttestation{Result: "consistent", Reason: "observations_consistent"}) {
		t.Fatalf("%+v", e)
	}
	want := []PodEpochSuccessorContainer{{Name: "seed", ContainerID: "containerd://seed2"}, {Name: "worker", ContainerID: "containerd://worker2"}}
	if !reflect.DeepEqual(e.SuccessorContainers, want) {
		t.Fatalf("%+v", e.SuccessorContainers)
	}
	if d.rv != epochWithIDs(epochPod(epochB), "containerd://").ResourceVersion {
		t.Fatalf("rv=%s", d.rv)
	}
	more := epochWithIDs(epochPod(epochB), "containerd://")
	more.ResourceVersion += "/more"
	if h.event(watch.Modified, more) != nil || len(h.patches) != 1 {
		t.Fatal("second decision for one epoch")
	}
}

func TestPodEpochSuccessorAttestation(t *testing.T) {
	tests := []struct {
		name     string
		runtime  string
		xPrefix  string
		sPrefix  string
		result   string
		reason   string
		mainOnly bool
	}{
		{"consistent", "containerd-runc", "containerd://", "containerd://", "consistent", "observations_consistent", false},
		{"crio-consistent", "crio-crun", "cri-o://", "cri-o://", "consistent", "observations_consistent", false},
		{"crio-with-containerd-successor", "crio-crun", "cri-o://", "containerd://", "contradicted", "successor_runtime_prefix", false},
		{"predecessor-prefix", "containerd-runc", "cri-o://", "containerd://", "contradicted", "predecessor_runtime_prefix", false},
		{"successor-prefix", "containerd-runc", "containerd://", "cri-o://", "contradicted", "successor_runtime_prefix", false},
		{"both-wrong-reports-predecessor", "containerd-runc", "cri-o://", "cri-o://", "contradicted", "predecessor_runtime_prefix", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := epochReadyHarness(t, epochTerminalPrefixed(epochPod(epochA), tc.xPrefix))
			h.session.workers[0].Runtime = tc.runtime
			b := epochPod(epochB)
			if tc.runtime == "crio-crun" {
				epochDeclareRuntime(t, h, b, tc.runtime)
			}
			if h.event(watch.Added, b) != nil {
				t.Fatal("emitted at Added")
			}
			e := h.assertDecision(h.event(watch.Modified, epochWithIDs(b, tc.sPrefix)), true)
			if e.Runtime != tc.runtime || e.Attestation.Result != tc.result || e.Attestation.Reason != tc.reason {
				t.Fatalf("%+v %+v", e, e.Attestation)
			}
		})
	}
}

func TestPodEpochSuccessorPredecessorInitPrefixContradicts(t *testing.T) {
	x := epochTerminal(epochPod(epochA))
	x.Status.InitContainerStatuses[0].ContainerID = "cri-o://seed"
	h := epochReadyHarness(t, x)
	h.event(watch.Added, epochPod(epochB))
	e := h.assertDecision(h.event(watch.Modified, epochWithIDs(epochPod(epochB), "containerd://")), true)
	if e.Attestation.Result != "contradicted" || e.Attestation.Reason != "predecessor_runtime_prefix" {
		t.Fatalf("%+v", e.Attestation)
	}
}

func TestPodEpochSuccessorProofIsFrozenAtAdded(t *testing.T) {
	h := epochReadyHarness(t, epochTerminal(epochPod(epochA)))
	h.event(watch.Added, epochPod(epochB))
	// A later predecessor change that would make a recomputed proof negative.
	a := withRV(epochTerminal(epochPod(epochA)), "/tainted")
	a.Spec.Volumes = a.Spec.Volumes[:1]
	h.event(watch.Modified, a)
	if !h.session.records[epochA].tainted {
		t.Fatal("predecessor not tainted; test does not discriminate")
	}
	e := h.assertDecision(h.event(watch.Modified, epochWithIDs(epochPod(epochB), "containerd://")), true)
	if e.Attestation == nil || e.Attestation.Result != "consistent" {
		t.Fatalf("%+v", e.Attestation)
	}
}

func TestPodEpochSuccessorMainContainerOnlyIDs(t *testing.T) {
	h := epochReadyHarness(t, epochTerminal(epochPod(epochA)))
	h.event(watch.Added, epochPod(epochB))
	p := epochPod(epochB)
	p.ResourceVersion += "/main"
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "worker", ContainerID: "containerd://worker2"}}
	e := h.assertDecision(h.event(watch.Modified, p), true)
	if !reflect.DeepEqual(e.SuccessorContainers, []PodEpochSuccessorContainer{{Name: "worker", ContainerID: "containerd://worker2"}}) {
		t.Fatalf("%+v", e.SuccessorContainers)
	}
}

func withRV(p *corev1.Pod, suffix string) *corev1.Pod {
	p = p.DeepCopy()
	p.ResourceVersion += suffix
	return p
}

func TestPodEpochSuccessorNeverEmits(t *testing.T) {
	ids := func() *corev1.Pod { return epochWithIDs(epochPod(epochB), "containerd://") }
	tests := []struct {
		name string
		prep func(h *epochHarness)
		last func(h *epochHarness) *PodEpochDecision
	}{
		{"deleted-event-with-ids", nil, func(h *epochHarness) *PodEpochDecision { return h.event(watch.Deleted, ids()) }},
		{"deleted-before-ids", func(h *epochHarness) { h.event(watch.Deleted, epochPod(epochB)) }, func(h *epochHarness) *PodEpochDecision { return h.event(watch.Modified, ids()) }},
		{"terminal-before-ids", func(h *epochHarness) {
			// The terminal status already carries container IDs, so this event alone
			// would complete the pending proof if the terminal guard were missing.
			if d := h.event(watch.Modified, epochTerminal(epochPod(epochB))); d != nil {
				h.t.Fatalf("emitted on terminal successor: %s", d.Document())
			}
		}, func(h *epochHarness) *PodEpochDecision { return h.event(watch.Modified, ids()) }},
		{"tainted-before-ids", func(h *epochHarness) {
			p := withRV(epochPod(epochB), "/mounts")
			p.Spec.Volumes = p.Spec.Volumes[:1]
			h.event(watch.Modified, p)
		}, func(h *epochHarness) *PodEpochDecision { return h.event(watch.Modified, ids()) }},
		{"not-current", func(h *epochHarness) { h.event(watch.Added, epochPod(epochC)) }, func(h *epochHarness) *PodEpochDecision { return h.event(watch.Modified, ids()) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := epochReadyHarness(t, epochTerminal(epochPod(epochA)))
			if h.event(watch.Added, epochPod(epochB)) != nil {
				t.Fatal("emitted at Added")
			}
			if tc.prep != nil {
				tc.prep(h)
			}
			if d := tc.last(h); d != nil {
				t.Fatalf("emitted: %s", d.Document())
			}
			if len(h.patches) != 0 {
				t.Fatal("request")
			}
			// Dropped pending entries release their budget.
			if tc.name != "deleted-event-with-ids" && h.session.records[epochB].pending != nil {
				t.Fatal("pending retained")
			}
			if r := h.session.records[epochB]; r != nil && r.pending == nil && r.pendingCost != 0 {
				t.Fatal("cost retained")
			}
		})
	}
}

// A successor in a terminal phase never gets positive evidence, even when no
// audited termination signal is recorded for it.
func TestPodEpochSuccessorTerminalPhaseWithoutSignalNeverEmits(t *testing.T) {
	tests := []struct {
		name string
		mod  func(p *corev1.Pod)
	}{
		{"failed-evicted-no-statuses", func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodFailed
			p.Status.Reason = "Evicted"
		}},
		{"failed-exit-137", func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodFailed
			p.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137}}
		}},
		{"succeeded-incomplete-statuses", func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodSucceeded
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := epochReadyHarness(t, epochTerminal(epochPod(epochA)))
			if h.event(watch.Added, epochPod(epochB)) != nil {
				t.Fatal("emitted at Added")
			}
			p := withRV(epochWithIDs(epochPod(epochB), "containerd://"), "/term")
			tc.mod(p)
			if d := h.event(watch.Modified, p); d != nil {
				t.Fatalf("emitted for terminal successor: %s", d.Document())
			}
			if h.session.records[epochB].pending != nil {
				t.Fatal("pending retained")
			}
			if d := h.event(watch.Modified, withRV(epochWithIDs(epochPod(epochB), "containerd://"), "/later")); d != nil {
				t.Fatalf("emitted after terminal phase: %s", d.Document())
			}
			if len(h.patches) != 0 {
				t.Fatal("request")
			}
		})
	}
}

func TestPodEpochSuccessorResetDiscardsPending(t *testing.T) {
	h := epochReadyHarness(t, epochTerminal(epochPod(epochA)))
	h.event(watch.Added, epochPod(epochB))
	g, err := h.session.Reset(context.Background(), []corev1.Pod{*epochPod(epochB)})
	if err != nil {
		t.Fatal(err)
	}
	h.generation = g
	if h.session.records[epochB].pending != nil {
		t.Fatal("pending survived Reset")
	}
	if h.event(watch.Modified, epochWithIDs(epochPod(epochB), "containerd://")) != nil {
		t.Fatal("emitted after Reset")
	}
}

func TestPodEpochNegativesStayAtAdded(t *testing.T) {
	t.Run("predecessor-unproven", func(t *testing.T) {
		h := newEpochHarness(t, epochPod(epochA))
		_, n := epochReason(t, h.event(watch.Added, epochPod(epochB)))
		if n == nil || n.Reason != "predecessor_unproven" {
			t.Fatalf("%+v", n)
		}
	})
	t.Run("undeclared-runtime", func(t *testing.T) {
		h := epochReadyHarness(t, epochTerminal(epochPod(epochA)))
		h.session.workers[0].Runtime = ""
		_, n := epochReason(t, h.event(watch.Added, epochPod(epochB)))
		if n == nil || n.Reason != "identity_unverified" {
			t.Fatalf("%+v", n)
		}
	})
	t.Run("remint-after-added-does-not-upgrade", func(t *testing.T) {
		h := epochReadyHarness(t, epochTerminal(epochPod(epochA)))
		minted, _ := h.client.CoreV1().PersistentVolumeClaims("docker").Get(context.Background(), dindDataPVCName("w"), metav1.GetOptions{})
		minted.UID = epochNewDinD
		minted.Spec.VolumeName = "new-dind-pv"
		if _, err := h.client.CoreV1().PersistentVolumeClaims("docker").Update(context.Background(), minted, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		d := h.event(watch.Added, epochPod(epochB))
		if _, n := epochReason(t, d); n == nil || n.Reason != "dind_remint_unrecorded" {
			t.Fatalf("%s", d.Document())
		}
		if err := h.session.RecordMaintenanceRemint(h.generation, "w", PodEpochRemint{OperationID: "op", OldClaimUID: epochDinD, NewClaimUID: epochNewDinD}); err != nil {
			t.Fatal(err)
		}
		if d := h.event(watch.Modified, epochWithIDs(epochPod(epochB), "containerd://")); d != nil {
			t.Fatalf("late receipt produced a second decision: %s", d.Document())
		}
	})
}

func TestPodEpochSuccessorPendingBudget(t *testing.T) {
	h := epochReadyHarness(t, epochTerminal(epochPod(epochA)))
	before := h.session.bytes
	h.event(watch.Added, epochPod(epochB))
	r := h.session.records[epochB]
	if r.pending == nil || r.pendingCost < 500 {
		t.Fatalf("pending not charged or too small to discriminate: %d", r.pendingCost)
	}
	// Control: the same successor Added against an unproven predecessor yields a
	// negative with no pending proof. Everything else charged is identical, so the
	// difference between the two session totals is exactly the pending charge.
	neg := newEpochHarness(t, epochPod(epochA))
	negBefore := neg.session.bytes
	neg.event(watch.Added, epochPod(epochB))
	if neg.session.records[epochB].pending != nil {
		t.Fatal("control kept a pending proof")
	}
	if got, want := (h.session.bytes-before)-(neg.session.bytes-negBefore), r.pendingCost; got != want {
		t.Fatalf("session total grew %d more than the control, want pending cost %d", got, want)
	}
	charged := h.session.bytes
	h.event(watch.Modified, epochWithIDs(epochPod(epochB), "containerd://"))
	// The Modified status adds a few hundred bytes of raw JSON, far less than the
	// pending proof, so the session total must fall once the entry is released.
	if h.session.records[epochB].pending != nil || h.session.records[epochB].pendingCost != 0 || h.session.bytes >= charged {
		t.Fatalf("pending bytes not released: before=%d charged=%d now=%d cost=%d", before, charged, h.session.bytes, r.pendingCost)
	}

	// Overflow on the pending charge invalidates the session like other retained data.
	h = epochReadyHarness(t, epochTerminal(epochPod(epochA)))
	h.session.bytes = PodEpochMaxMetadata - 2048
	if h.event(watch.Added, epochPod(epochB)) != nil || !h.session.overflow {
		t.Fatal("pending overflow not latched")
	}
	if h.event(watch.Modified, epochWithIDs(epochPod(epochB), "containerd://")) != nil {
		t.Fatal("emitted after overflow")
	}
}

func TestPodEpochSuccessorConflictConsumesSingleAttempt(t *testing.T) {
	h := epochReadyHarness(t, epochTerminal(epochPod(epochA)))
	h.event(watch.Added, epochPod(epochB))
	d := h.event(watch.Modified, epochWithIDs(epochPod(epochB), "containerd://"))
	if d == nil {
		t.Fatal("missing decision")
	}
	requests := 0
	h.client.PrependReactor("patch", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		requests++
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, d.name, errors.New("conflict"))
	})
	if err := h.session.Deliver(context.Background(), d); !apierrors.IsConflict(err) {
		t.Fatalf("err=%v", err)
	}
	if err := h.session.Deliver(context.Background(), d); err == nil {
		t.Fatal("retry allowed")
	}
	more := epochWithIDs(epochPod(epochB), "containerd://")
	more.ResourceVersion += "/more"
	if h.event(watch.Modified, more) != nil || requests != 1 {
		t.Fatalf("requests=%d", requests)
	}
}
