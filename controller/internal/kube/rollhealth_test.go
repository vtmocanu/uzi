package kube

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"gitlab.example.com/vtmocanu/uzi/controller/internal/protocol"
	"gitlab.example.com/vtmocanu/uzi/controller/internal/reconcile"
)

// Roll-health derivation (PRD #113 M3). The headline test is the two-tick one at the
// bottom; the unit table below covers the arms.

var testNow = time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)

// workerPod builds a pod as the Deployment would produce it: stamped with our
// labels, carrying the pod template's spec-hash annotation.
func workerPod(id, hash string, age time.Duration) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "uzi-hw-" + id + "-abc123-xyz",
			Namespace:         testConfig().Namespace,
			Labels:            objectLabels(id),
			Annotations:       map[string]string{AnnotationSpecHash: hash},
			CreationTimestamp: metav1.NewTime(testNow.Add(-age)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
}

func ready(p *corev1.Pod, at time.Time) *corev1.Pod {
	p.Status.Phase = corev1.PodRunning
	p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{
		Type:               corev1.PodReady,
		Status:             corev1.ConditionTrue,
		LastTransitionTime: metav1.NewTime(at),
	})
	return p
}

func waiting(p *corev1.Pod, container, reason string, restarts int32, init bool) *corev1.Pod {
	st := corev1.ContainerStatus{
		Name:         container,
		RestartCount: restarts,
		State:        corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}},
	}
	if init {
		p.Status.InitContainerStatuses = append(p.Status.InitContainerStatuses, st)
	} else {
		p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, st)
	}
	return p
}

func withLastExit(p *corev1.Pod, code int32) *corev1.Pod {
	sets := [][]corev1.ContainerStatus{p.Status.InitContainerStatuses, p.Status.ContainerStatuses}
	for _, set := range sets {
		if len(set) > 0 {
			set[len(set)-1].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{ExitCode: code}
			return p
		}
	}
	return p
}

func TestDeriveRollHealth(t *testing.T) {
	const want = "hash-new"

	cases := []struct {
		name string
		pods []corev1.Pod
		// replicaFailure is the reason on the Deployment's ReplicaFailure condition when
		// True, as observeNamespace reads it. Empty on every pre-#148 case, which is what
		// keeps them asserting what they always did.
		replicaFailure string
		wantPhase      string
		wantReason     string
		check          func(t *testing.T, h reconcile.RollHealth)
	}{
		{
			name:      "no pod at all is the Recreate gap, and dates from nothing",
			pods:      nil,
			wantPhase: protocol.PhaseRolling,
			check: func(t *testing.T, h reconcile.RollHealth) {
				if !h.PhaseSince.IsZero() {
					t.Errorf("PhaseSince = %v, want zero: there is no pod to date the gap from, and "+
						"inventing now() would restart the clock every tick", h.PhaseSince)
				}
			},
		},
		{
			// #148. The case above is this one's NEGATIVE CONTROL and they must be read as a
			// pair: same empty pod list, opposite answers, and the only difference is the
			// condition. TestPodlessIsStuckOnlyWithReplicaFailure asserts the pair inside one
			// function, because two independent subtests can both be satisfied by code that
			// ignores the pods and keys on nothing at all.
			name:           "a pod-less worker whose Deployment reports ReplicaFailure=True is STUCK, not rolling",
			pods:           nil,
			replicaFailure: "FailedCreate",
			wantPhase:      protocol.PhaseStuck,
			wantReason:     "FailedCreate",
			check: func(t *testing.T, h reconcile.RollHealth) {
				if !h.PhaseSince.IsZero() {
					t.Errorf("PhaseSince = %v, want zero: there is still no pod to date this from, and the "+
						"condition's own LastTransitionTime must not be smuggled into a field documented "+
						"as a pod timestamp everywhere else", h.PhaseSince)
				}
				if h.BlockingContainer != "" {
					t.Errorf("BlockingContainer = %q, want empty: no container exists — no pod was ever "+
						"created. Naming one would invent a subject", h.BlockingContainer)
				}
			},
		},
		{
			// The OTHER way the case above could pass vacuously: an implementation that keys
			// on replicaFailure alone and never looks at the pods. A Deployment can carry
			// ReplicaFailure=True from a create that failed while a healthy pod from the
			// previous generation is still Ready, and that worker is NOT failed.
			name:           "ReplicaFailure alongside a Ready pod does not override the pod",
			pods:           []corev1.Pod{*ready(workerPod("w1", want, time.Hour), testNow.Add(-5*time.Minute))},
			replicaFailure: "FailedCreate",
			wantPhase:      protocol.PhaseSettled,
		},
		{
			name:      "only the OLD pod is left: the new one is not up yet, so rolling",
			pods:      []corev1.Pod{*workerPod("w1", "hash-old", time.Minute)},
			wantPhase: protocol.PhaseRolling,
		},
		{
			// The stale-pod branch is deliberately NOT gated on the condition — see its
			// comment in rollhealth.go. Under Recreate a stale pod still being present means
			// the delete is in flight, which is transient by construction, so gating it would
			// add a cry-wolf window for no coverage the pod-less branch does not already give.
			name:           "ReplicaFailure with only a STALE pod stays rolling: that branch is transient by construction",
			pods:           []corev1.Pod{*workerPod("w1", "hash-old", time.Minute)},
			replicaFailure: "FailedCreate",
			wantPhase:      protocol.PhaseRolling,
		},
		{
			name:      "the current pod is Ready: settled, dated from the Ready transition",
			pods:      []corev1.Pod{*ready(workerPod("w1", want, time.Hour), testNow.Add(-5*time.Minute))},
			wantPhase: protocol.PhaseSettled,
			check: func(t *testing.T, h reconcile.RollHealth) {
				if got := h.PhaseSince; !got.Equal(testNow.Add(-5 * time.Minute)) {
					t.Errorf("PhaseSince = %v, want the Ready condition's transition time", got)
				}
			},
		},
		{
			name:       "the motivating incident: an INIT container in CrashLoopBackOff is stuck at once",
			pods:       []corev1.Pod{*withLastExit(waiting(workerPod("w1", want, 2*time.Minute), "seed-nix", "CrashLoopBackOff", 6, true), 2)},
			wantPhase:  protocol.PhaseStuck,
			wantReason: "CrashLoopBackOff",
			check: func(t *testing.T, h reconcile.RollHealth) {
				if h.BlockingContainer != "seed-nix" {
					t.Errorf("BlockingContainer = %q, want seed-nix", h.BlockingContainer)
				}
				if h.RestartCount != 6 {
					t.Errorf("RestartCount = %d, want 6", h.RestartCount)
				}
				// CrashLoopBackOff means the container is Waiting NOW and terminated
				// BEFORE, so the exit code lives in LastTerminationState. Reading only
				// State.Terminated would silently report no exit code for the single
				// case this feature was built for.
				if h.LastExitCode == nil || *h.LastExitCode != 2 {
					t.Errorf("LastExitCode = %v, want 2 (from LastTerminationState — a CrashLoopBackOff "+
						"container is Waiting now and Terminated before)", h.LastExitCode)
				}
			},
		},
		{
			name:       "ImagePullBackOff is stuck immediately: waiting longer cannot fix a missing image",
			pods:       []corev1.Pod{*waiting(workerPod("w1", want, 10*time.Second), "worker", "ImagePullBackOff", 0, false)},
			wantPhase:  protocol.PhaseStuck,
			wantReason: "ImagePullBackOff",
		},
		{
			name:      "a young pod pulling its image is ROLLING, not stuck — the healthy case",
			pods:      []corev1.Pod{*waiting(workerPod("w1", want, 30*time.Second), "worker", "ContainerCreating", 0, false)},
			wantPhase: protocol.PhaseRolling,
		},
		{
			name: "restarts past the threshold are stuck even with no blocking reason visible",
			pods: []corev1.Pod{*waiting(workerPod("w1", want, time.Minute), "worker", "", stuckRestartThreshold, false)},
			// The reason arm cannot see this one: the container flickers through
			// Running, so the waiting reason is only intermittently present.
			wantPhase: protocol.PhaseStuck,
		},
		{
			name:      "a reasonless pod past stuckAge is stuck (Pending/FailedScheduling has no container to blame)",
			pods:      []corev1.Pod{*workerPod("w1", want, stuckAge+time.Minute)},
			wantPhase: protocol.PhaseStuck,
			check: func(t *testing.T, h reconcile.RollHealth) {
				if h.BlockingContainer != "" {
					t.Errorf("BlockingContainer = %q, want empty: no container exists to blame", h.BlockingContainer)
				}
			},
		},
		{
			name:      "the same reasonless pod INSIDE stuckAge is still rolling",
			pods:      []corev1.Pod{*workerPod("w1", want, stuckAge-time.Minute)},
			wantPhase: protocol.PhaseRolling,
		},
		{
			name: "an init container is named before an app container, since it blocks everything behind it",
			pods: []corev1.Pod{*waiting(
				waiting(workerPod("w1", want, time.Minute), "worker", "PodInitializing", 0, false),
				"seed-nix", "CrashLoopBackOff", 1, true)},
			wantPhase:  protocol.PhaseStuck,
			wantReason: "CrashLoopBackOff",
			check: func(t *testing.T, h reconcile.RollHealth) {
				if h.BlockingContainer != "seed-nix" {
					t.Errorf("BlockingContainer = %q, want the INIT container seed-nix: the app container's "+
						"PodInitializing is a CONSEQUENCE of the init failure, so naming it sends the "+
						"operator to the wrong container", h.BlockingContainer)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := deriveRollHealth(tc.pods, want, tc.replicaFailure, testNow)
			if h.Phase != tc.wantPhase {
				t.Errorf("Phase = %q, want %q", h.Phase, tc.wantPhase)
			}
			if tc.wantReason != "" && h.BlockingReason != tc.wantReason {
				t.Errorf("BlockingReason = %q, want %q", h.BlockingReason, tc.wantReason)
			}
			if tc.check != nil {
				tc.check(t, h)
			}
		})
	}
}

// ===========================================================================
// THE TEST THAT SEPARATES THE CORRECT IMPLEMENTATION FROM THE PLAUSIBLE ONE.
// ===========================================================================
//
// A drift-driven implementation (`phase = rolling if obs.SpecHash != wantHash`) is
// CORRECT ON TICK ONE and confidently wrong afterwards, because Reconcile's patch
// body carries the spec-hash annotation — so the deployment stops looking drifted
// the instant the patch returns, while the pod is still Pending. A single-tick test
// passes against it, exactly as an all-equal version fixture passes against a
// comparison that never normalizes.
//
// So: roll a drifted worker, then observe TWICE with the pod still not Ready, and
// require `rolling` both times. The second observation is the one with teeth.
func TestRollHealthStaysRollingAcrossTicksWhileThePodIsNotReady(t *testing.T) {
	ctx := context.Background()
	id := "w1"
	ns := testConfig().Namespace
	desired := protocol.DesiredWorker{ID: id, Template: "base", Size: "m", Generation: 1}

	// A deployment already in the cluster at a STALE spec hash — i.e. drifted, which
	// is what a release does to every worker.
	stale := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "uzi-hw-" + id, Namespace: ns, Labels: objectLabels(id)},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: objectLabels(id),
					Annotations: map[string]string{
						AnnotationSpecHash:   "stale-hash",
						AnnotationGeneration: "1",
					},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: workerContainerName, Image: "harbor.example/uzi/agent-base:0.11.7"}}},
			},
		},
	}
	m, client := newMat(t, stale)
	m.now = func() time.Time { return testNow }

	// --- Tick 1: observe the drift, reconcile it (this patches the deployment).
	observed, err := m.Observe(ctx)
	if err != nil {
		t.Fatalf("Observe (tick 1): %v", err)
	}
	if err := m.Reconcile(ctx, []protocol.DesiredWorker{desired}, observed); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// The patch landed, so the deployment no longer looks drifted. This is the state
	// that makes the drift-driven implementation start lying.
	dep, err := client.AppsV1().Deployments(ns).Get(ctx, "uzi-hw-"+id, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	live := dep.Spec.Template.Annotations[AnnotationSpecHash]
	if live == "stale-hash" {
		t.Fatalf("the deployment was not patched, so this test never reaches the state it exists to cover "+
			"(spec hash still %q)", live)
	}

	// The new pod is up but NOT Ready — the real mid-roll state.
	pod := workerPod(id, live, 30*time.Second)
	if _, err := client.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	// --- Ticks 2 and 3: the deployment is settled, the pod is not.
	for tick := 2; tick <= 3; tick++ {
		observed, err = m.Observe(ctx)
		if err != nil {
			t.Fatalf("Observe (tick %d): %v", tick, err)
		}
		if len(observed) != 1 {
			t.Fatalf("tick %d: observed %d workers, want 1", tick, len(observed))
		}
		got := observed[0].Roll.Phase
		if got != protocol.PhaseRolling {
			t.Fatalf("tick %d: Roll.Phase = %q, want %q. The deployment's spec hash EQUALS what is wanted "+
				"here (the patch landed on tick 1), so a phase derived from the drift predicate reads "+
				"%q — while the pod is still Pending. Roll health must come from POD READINESS.",
				tick, got, protocol.PhaseRolling, protocol.PhaseSettled)
		}
	}

	// And once the pod goes Ready, it settles — otherwise "always rolling" would pass
	// the assertion above without deriving anything.
	readyPod := ready(workerPod(id, live, time.Minute), testNow.Add(-10*time.Second))
	if _, err := client.CoreV1().Pods(ns).Update(ctx, readyPod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update pod: %v", err)
	}
	observed, err = m.Observe(ctx)
	if err != nil {
		t.Fatalf("Observe (settled): %v", err)
	}
	if got := observed[0].Roll.Phase; got != protocol.PhaseSettled {
		t.Fatalf("after the pod went Ready, Roll.Phase = %q, want %q — a derivation that answers "+
			"%q unconditionally would pass the two-tick assertion above while measuring nothing",
			got, protocol.PhaseSettled, protocol.PhaseRolling)
	}
	// The expected image comes from the RESOLVER, not from what this test seeded:
	// Reconcile's patch re-renders the whole pod template on tick 1, so the live
	// deployment now carries the image the resolver produces. Hardcoding the seeded
	// value here asserted that the patch had NOT happened, which is the opposite of
	// what tick 1 is for.
	wantImage := testSpec(t, "base", "m").Image
	if got := observed[0].Roll.TargetImage; got != wantImage {
		t.Errorf("TargetImage = %q, want %q — the image the live deployment asks for", got, wantImage)
	}
}

// The reference hash must be the OBSERVED deployment's annotation, never a freshly
// rendered one — a second instance of the drift trap inside the same function, and
// invisible to the two-tick test above.
//
// The substitution reads naturally and is one identifier long: pass the rendered
// wantHash instead of o.SpecHash. It would report `rolling` ONE TICK EARLY — before
// the patch has landed, while the worker is still happily running its old spec and
// nothing is rolling at all.
//
// The fixture makes the two differ in a way no renderer can accidentally satisfy:
// the deployment and its Ready pod both carry a hash string the render path could
// never produce. Under the correct reference the pod matches and the worker is
// SETTLED; under the rendered-hash reference no pod matches and it reads rolling.
func TestRollHealthReferenceIsTheObservedDeploymentHashNotARenderedOne(t *testing.T) {
	ctx := context.Background()
	id := "w1"
	ns := testConfig().Namespace
	const observedHash = "a-hash-no-render-would-ever-produce"

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "uzi-hw-" + id, Namespace: ns, Labels: objectLabels(id)},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      objectLabels(id),
					Annotations: map[string]string{AnnotationSpecHash: observedHash, AnnotationGeneration: "1"},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: workerContainerName, Image: "harbor.example.com/uzi/agent-base:v1.0.0"}}},
			},
		},
	}
	// The pod the cluster is actually running: matches the deployment as OBSERVED.
	pod := ready(workerPod(id, observedHash, 10*time.Minute), testNow.Add(-9*time.Minute))

	m, _ := newMat(t, dep, pod)
	m.now = func() time.Time { return testNow }

	observed, err := m.Observe(ctx)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(observed) != 1 {
		t.Fatalf("observed %d workers, want 1", len(observed))
	}
	if got := observed[0].SpecHash; got != observedHash {
		t.Fatalf("precondition: observed SpecHash = %q, want %q — the fixture is not set up as intended", got, observedHash)
	}
	if got := observed[0].Roll.Phase; got != protocol.PhaseSettled {
		t.Fatalf("Roll.Phase = %q, want %q. The pod matches the hash the deployment is OBSERVED to carry, "+
			"so this worker is settled and nothing is rolling. Reading %q here means the derivation matched "+
			"pods against a freshly RENDERED hash instead of the observed one, which reports a roll one tick "+
			"before the patch that starts it.", got, protocol.PhaseSettled, protocol.PhaseRolling)
	}

	// The other direction, so "always settled" cannot pass the assertion above: a pod
	// carrying a hash the deployment no longer asks for is the OLD pod, and says
	// nothing about the new one.
	stale := workerPod(id, "some-other-hash", time.Minute)
	stale.Name = "uzi-hw-" + id + "-old-pod"
	if _, err := m.client.CoreV1().Pods(ns).Create(ctx, stale, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create stale pod: %v", err)
	}
	if err := m.client.CoreV1().Pods(ns).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete current pod: %v", err)
	}
	observed, err = m.Observe(ctx)
	if err != nil {
		t.Fatalf("Observe (stale only): %v", err)
	}
	if got := observed[0].Roll.Phase; got != protocol.PhaseRolling {
		t.Errorf("with only a stale-hash pod present, Roll.Phase = %q, want %q", got, protocol.PhaseRolling)
	}
}

// podVerbs mirrors secretVerbs: walk the whole fake-client action log and return the
// verbs issued against pods. Both RBAC halves are asserted from it.
func podVerbs(client *fake.Clientset) []string {
	var out []string
	for _, a := range client.Actions() {
		if a.GetResource().Resource == "pods" {
			out = append(out, a.GetVerb())
		}
	}
	return out
}

// The pod grant is `list` and only `list`. Unlike assertNoSecretReads — which is
// Secret-scoped and therefore cannot break under a pod grant, so it proves nothing
// about this call — this gate covers the new verb directly.
//
// BOTH halves are required and neither is sufficient. The negative alone passes
// trivially on a build that never touches pods at all; the positive alone would not
// notice a `watch` creeping in beside the list.
func TestObserveIssuesListOnlyOnPodsAndActuallyListsThem(t *testing.T) {
	ctx := context.Background()
	id := "w1"
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "uzi-hw-" + id, Namespace: testConfig().Namespace, Labels: objectLabels(id)},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: objectLabels(id), Annotations: map[string]string{AnnotationSpecHash: "h"}},
		}},
	}
	m, client := newMat(t, dep, workerPod(id, "h", time.Minute))
	if _, err := m.Observe(ctx); err != nil {
		t.Fatalf("Observe: %v", err)
	}

	verbs := podVerbs(client)

	// POSITIVE: pods are actually listed. Without this the negative below is vacuous.
	if len(verbs) == 0 {
		t.Fatalf("no pod call reached the apiserver at all, so the list-only assertion below cannot fail. " +
			"Roll health is unreadable without listing pods — a green here would mean the feature is absent, " +
			"not that it is safe.")
	}

	// NEGATIVE: nothing but list, ever. `get` has no call site (pod names are
	// Deployment-generated, so the controller cannot construct one) and `watch` would
	// be a standing subscription this stateless loop has no use for. Either appearing
	// here means the Role must widen, which is what the grant's comment forbids.
	for _, v := range verbs {
		if v != "list" {
			t.Errorf("a %q on pods reached the apiserver; the Role grants `list` ONLY (PRD #113 M3). "+
				"Verbs seen: %v", v, verbs)
		}
	}

	// And Secrets stay untouched: the new grant must not have loosened the old line.
	assertNoSecretReads(t, client)
}

// MH-13b: a pod parked in a worker namespace that this controller did not stamp is
// WARNED about, where flagOrphan's uzi-hw- name prefix check would have skipped it
// silently. Nothing prevents such a pod being admitted — the NetworkPolicy, PSA and
// quota controls all bound what it can DO, not whether it can exist.
func TestObserveWarnsAboutAnUnmanagedPodInAWorkerNamespace(t *testing.T) {
	ctx := context.Background()
	var logged strings.Builder
	client := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			// Deliberately NOT uzi-hw-*: that is the case flagOrphan cannot see, and
			// the whole reason this check is wider than that one.
			Name:      "someones-debug-shell",
			Namespace: testConfig().Namespace,
		},
	})
	m := New(client, testConfig(), testResolver(t), slog.New(slog.NewTextHandler(&logged, nil)))

	observed, err := m.Observe(ctx)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	// Flagged only. Never adopted, never deleted, never returned to Reconcile — which
	// cannot act on what it cannot see.
	if len(observed) != 0 {
		t.Errorf("observed %d workers, want 0: an unstamped pod must never enter the observed set", len(observed))
	}
	out := logged.String()
	if !strings.Contains(out, "unmanaged pod in a worker namespace") {
		t.Errorf("no warning logged for an unstamped pod parked in a worker namespace; got:\n%s", out)
	}
	if !strings.Contains(out, "someones-debug-shell") {
		t.Errorf("the warning does not name the pod, so an operator cannot act on it; got:\n%s", out)
	}
	for _, v := range podVerbs(client) {
		if v != "list" {
			t.Errorf("detecting an unmanaged pod used a %q; it must be visible from the list alone", v)
		}
	}
}

// running puts a container in the RUNNING state with a restart count — the fixture the
// restart-threshold arm actually needs and which this file did not have, which is why
// the arm's gap survived: the case was not constructible.
//
// k8s resets the restart backoff after ~10 minutes of running, so a container that runs
// a minute, dies and repeats is observed Running most of the time. That is precisely the
// shape the arm exists for.
func running(p *corev1.Pod, container string, restarts int32, init bool) *corev1.Pod {
	st := corev1.ContainerStatus{
		Name:         container,
		RestartCount: restarts,
		State:        corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(testNow.Add(-time.Minute))}},
		LastTerminationState: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 137},
		},
	}
	if init {
		p.Status.InitContainerStatuses = append(p.Status.InitContainerStatuses, st)
	} else {
		p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, st)
	}
	return p
}

// The restart-threshold arm must fire for a container that is RUNNING right now.
//
// This was a real gap, not a comment problem: RestartCount was only populated inside the
// blocking-container branch, and blockingContainer returns nil unless a container is
// Waiting or Terminated-nonzero. So a flapping-but-currently-Running container left
// RestartCount at 0 and the arm evaluated `0 >= 3` forever.
//
// The second consequence was worse than the first, and it is asserted here too: the
// REPORTED restart count was 0 and no container was named, so a flapping worker looked
// pristine to whoever was trying to fix it.
func TestRestartThresholdArmFiresForARunningContainer(t *testing.T) {
	const want = "hash-new"

	flapping := running(workerPod("w1", want, 5*time.Minute), "worker", stuckRestartThreshold, false)
	h := deriveRollHealth([]corev1.Pod{*flapping}, want, "", testNow)

	if h.Phase != protocol.PhaseStuck {
		t.Errorf("Phase = %q, want %q. The container is RUNNING with %d restarts, which is the case "+
			"this arm exists for — a container that runs a minute, dies and repeats is observed Running "+
			"most of the time, because k8s resets the restart backoff after ~10m.",
			h.Phase, protocol.PhaseStuck, stuckRestartThreshold)
	}
	if h.RestartCount != stuckRestartThreshold {
		t.Errorf("RestartCount = %d, want %d. This is the worse half of the bug: a blanked count makes "+
			"a flapping worker look pristine in the failed-worker strip.", h.RestartCount, stuckRestartThreshold)
	}
	if h.BlockingContainer != "worker" {
		t.Errorf("BlockingContainer = %q, want the flapping container's name; without it an operator has "+
			"nothing to act on", h.BlockingContainer)
	}
	// The exit code comes from LastTerminationState, since the container is Running NOW
	// and terminated before.
	if h.LastExitCode == nil || *h.LastExitCode != 137 {
		t.Errorf("LastExitCode = %v, want 137 from LastTerminationState", h.LastExitCode)
	}

	// CONTROL, and it is what makes the above evidence rather than a coincidence: a
	// Running container BELOW the threshold must stay `rolling`, so the test is not
	// simply passing because everything Running reads as stuck.
	below := running(workerPod("w2", want, 5*time.Minute), "worker", stuckRestartThreshold-1, false)
	if h := deriveRollHealth([]corev1.Pod{*below}, want, "", testNow); h.Phase != protocol.PhaseRolling {
		t.Errorf("a Running container with %d restarts (below the threshold of %d) reads %q, want %q",
			stuckRestartThreshold-1, stuckRestartThreshold, h.Phase, protocol.PhaseRolling)
	}

	// And a healthy not-yet-Ready pod with ZERO restarts must have NO container blamed —
	// naming one with 0 restarts would point an operator at innocent code.
	fresh := running(workerPod("w3", want, 10*time.Second), "worker", 0, false)
	if h := deriveRollHealth([]corev1.Pod{*fresh}, want, "", testNow); h.BlockingContainer != "" {
		t.Errorf("BlockingContainer = %q for a pod with no restarts, want empty", h.BlockingContainer)
	}
}

// runningFor is running() with the TWO fields issue #145's rule actually reads chosen
// per case instead of baked in: how long the CURRENT instance has been up, and whether
// there is a last termination at all.
//
// running() cannot serve here and the difference is the whole point. It hardcodes
// `StartedAt = testNow-1m`, so a "Ready + restarts" case built on it would read `stuck`
// under both the shipped and the fixed derivation — by accident of that one minute
// being inside flapWindow, not because the rule works. Every case below therefore
// states its uptime and says why that number.
//
// lastTerm nil builds the shape MEASURED on a real crash-looping pod: a container
// instance with restartCount 6 and `"lastState": {}`. It is not a defensive nil; it is
// what killed a finishedAt-anchored design.
func runningFor(p *corev1.Pod, container string, restarts int32, up time.Duration, lastTerm *corev1.ContainerStateTerminated, init bool) *corev1.Pod {
	st := corev1.ContainerStatus{
		Name:         container,
		RestartCount: restarts,
		State:        corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(testNow.Add(-up))}},
	}
	if lastTerm != nil {
		st.LastTerminationState.Terminated = lastTerm
	}
	if init {
		p.Status.InitContainerStatuses = append(p.Status.InitContainerStatuses, st)
	} else {
		p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, st)
	}
	return p
}

// Issue #145 — A READY POD IS NOT SETTLED WHILE A CONTAINER IS FLAPPING.
//
// The worker container has no readiness probe, so `Ready` means the kubelet started the
// process, not that the agent works. A container that starts, dies ~40s later and
// repeats is Ready on 31% of ticks — MEASURED, 41 of 133 samples, and decaying from 100%
// at 0 restarts to 10% at 6 as kubelet's backoff grows. The unguarded early return
// reported `settled` on every one of those, so the badge alternated; and because
// `settled` returns BEFORE the blocking-container lookup, the report carried zeroed
// diagnostics over the real ones.
//
// Each case below is here because a WRONG implementation passes the others:
//
//  1. the defect itself.
//  2. a count-only rule fails it — and so does a rule whose recency check sits inside
//     the switch instead of at the entry. This is the most important case in the file.
//  3. a recency-only rule fails it.
//  4. the healthy roll; any rule that over-fires fails it.
//  5. a finishedAt-anchored rule fails it — the shape measured on a real pod.
func TestReadyPodIsNotSettledWhileAContainerIsFlapping(t *testing.T) {
	const want = "hash-new"
	exit1 := &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}

	// --- 1. THE DEFECT. Ready, 5 restarts, this instance up 30s. ---
	//
	// 30s because that is the live shape: the sampled flapper ran `sleep 40; exit 1`, so
	// a fresh instance sits ~30s into its life on the Ready ticks. Any value well inside
	// flapWindow does; the point is that the container came up recently and has died
	// repeatedly. (Not "Ready for most of each cycle" — measured, it is Ready on 31% of
	// ticks overall and less as the backoff grows. The fixture is unaffected; only the
	// justification was overstated.)
	pod := ready(runningFor(workerPod("w1", want, 20*time.Minute), "worker", 5, 30*time.Second, exit1, false),
		testNow.Add(-30*time.Second))
	h := deriveRollHealth([]corev1.Pod{*pod}, want, "", testNow)
	if h.Phase != protocol.PhaseStuck {
		t.Errorf("Phase = %q, want %q. The pod is Ready, but its container has died 5 times and the "+
			"current instance is 30s old — Ready means the kubelet started the process, not that the "+
			"agent works, because the worker container has NO readiness probe.", h.Phase, protocol.PhaseStuck)
	}
	if h.BlockingContainer != "worker" || h.RestartCount != 5 {
		t.Errorf("BlockingContainer = %q RestartCount = %d, want worker/5. The whole point of withholding "+
			"`settled` is that the report then carries the diagnostics a settled report never collects.",
			h.BlockingContainer, h.RestartCount)
	}
	if h.LastExitCode == nil || *h.LastExitCode != 1 {
		t.Errorf("LastExitCode = %v, want 1 from LastTerminationState — the container is Running NOW and "+
			"terminated before, which is exactly the crash-loop shape", h.LastExitCode)
	}
	// The reason comes from the last termination, because a Running container has no
	// current-state reason. Without it stuckDetail renders "not ready" about a pod that
	// IS ready.
	if h.BlockingReason != "Error" {
		t.Errorf("BlockingReason = %q, want %q from the last termination. Left empty, the api's "+
			"stuckDetail substitutes \"not ready\", which is false of a Ready pod.", h.BlockingReason, "Error")
	}

	// --- 2. 🔴 THE ANTI-PERMANENT-RED CASE. Ready, 5 restarts, up 10m1s. ---
	//
	// A worker that restarted 5 times last week and has been up ever since is HEALTHY.
	// RestartCount is the kubelet's lifetime counter and never decreases, so a rule
	// built on the count alone pins this red forever — and since #151 removed the INV-5
	// ceiling from R1, that `upgrade_failed` has no expiry at all.
	//
	// It is also the case that fails if the recency conjunct is moved INTO the switch
	// rather than gating entry to the fall-through: this pod would then reach the arms,
	// where its 4-hour age trips the reasonless `stuckAge` arm even if the restart arm
	// is correctly gated. The guard must keep it out of the switch entirely.
	//
	// 10m1s, one second past flapWindow, because a boundary is where an off-by-one
	// lives; the pod is 4h old to make it unmistakably a long-lived worker.
	recovered := ready(runningFor(workerPod("w2", want, 4*time.Hour), "worker", 5, flapWindow+time.Second, exit1, false),
		testNow.Add(-(flapWindow + time.Second)))
	if h := deriveRollHealth([]corev1.Pod{*recovered}, want, "", testNow); h.Phase != protocol.PhaseSettled {
		t.Errorf("Phase = %q, want %q. This worker restarted 5 times and has been up %v since — it is "+
			"HEALTHY. The verdict must expire on continuous uptime, or a lifetime counter that never "+
			"decreases pins a working worker red forever with no ceiling to end it.",
			h.Phase, protocol.PhaseSettled, flapWindow+time.Second)
	}

	// --- 2b. THE OTHER SIDE OF THE SAME BOUNDARY. Ready, 5 restarts, up 9m59s. ---
	//
	// Case 2 ALONE IS VACUOUS and that is not a hypothetical: "up 10m1s -> settled"
	// passes against an implementation whose guard never fires at all. Only the
	// DISAGREEMENT between the pair is signal. One second on the other side of
	// flapWindow, with every other input identical to case 2 — same restart count, same
	// pod age — so the only thing that can explain a different verdict is the conjunct
	// under test.
	stillFlapping := ready(runningFor(workerPod("w2b", want, 4*time.Hour), "worker", 5, flapWindow-time.Second, exit1, false),
		testNow.Add(-(flapWindow - time.Second)))
	if h := deriveRollHealth([]corev1.Pod{*stillFlapping}, want, "", testNow); h.Phase == protocol.PhaseSettled {
		t.Errorf("Phase = %q for a container up %v — one second INSIDE flapWindow, with the same 5 "+
			"restarts and the same 4h pod age as the settled case above. If both sides of this boundary "+
			"read the same, the recency conjunct is not being evaluated at all and case 2 proves nothing.",
			h.Phase, flapWindow-time.Second)
	}

	// --- 2c. THE CONSTANT'S VALUE, pinned with LITERAL durations. ---
	//
	// Cases 2 and 2b express their uptime as `flapWindow ± time.Second`, so they pin the
	// COMPARISON — that it is `<`, and where the off-by-one sits — but they cannot pin
	// the NUMBER: retune flapWindow and both inputs move with it and both stay green.
	// MEASURED: halving flapWindow from 10m to 5m left the entire kube suite passing.
	//
	// That matters here specifically because flapWindow is 10 minutes and stuckAge
	// (a DIFFERENT quantity — pod age, not container uptime) is also 10 minutes, so a
	// silent retune of one while reaching for the other is the live hazard.
	//
	// These two are literals and bracket the constant from both sides: 9m must be inside
	// the window and 11m outside. Shrinking flapWindow reddens the first; widening it
	// reddens the second. A deliberate retune therefore has to come here and say so,
	// which is the point.
	inside := ready(runningFor(workerPod("w2c", want, 4*time.Hour), "worker", 5, 9*time.Minute, exit1, false),
		testNow.Add(-9*time.Minute))
	if h := deriveRollHealth([]corev1.Pod{*inside}, want, "", testNow); h.Phase == protocol.PhaseSettled {
		t.Errorf("a container up 9m with 5 restarts reads %q; 9 minutes must be INSIDE the flap window. "+
			"If this fails alone, flapWindow has been shrunk below 9m — check whether that was deliberate "+
			"and whether stuckAge was the constant actually meant.", h.Phase)
	}
	outside := ready(runningFor(workerPod("w2d", want, 4*time.Hour), "worker", 5, 11*time.Minute, exit1, false),
		testNow.Add(-11*time.Minute))
	if h := deriveRollHealth([]corev1.Pod{*outside}, want, "", testNow); h.Phase != protocol.PhaseSettled {
		t.Errorf("a container up 11m with 5 restarts reads %q, want %q; 11 minutes must be OUTSIDE the "+
			"flap window, or the verdict stops expiring on continuous uptime.", h.Phase, protocol.PhaseSettled)
	}

	// --- 2e. NEGATIVE uptime (clock skew) is not evidence of a restart loop. ---
	//
	// StartedAt is stamped by the kubelet and compared against this controller's clock.
	// Skew can put it in the FUTURE, and a negative duration is `< flapWindow`, so an
	// unguarded comparison reads skew as flapping. Bounded in practice — the window
	// shifts by the skew rather than pinning red, and 3 lifetime restarts are needed
	// first — but a duration we cannot trust is not evidence, which is the same
	// direction the nil-Running skip takes.
	skewed := ready(runningFor(workerPod("w2e", want, 4*time.Hour), "worker", 5, -4*time.Hour, exit1, false),
		testNow.Add(-time.Minute))
	if h := deriveRollHealth([]corev1.Pod{*skewed}, want, "", testNow); h.Phase != protocol.PhaseSettled {
		t.Errorf("a container whose StartedAt is 4h in the FUTURE reads %q, want %q. A negative uptime is "+
			"clock skew between the kubelet and this controller, and negative < flapWindow is true, so "+
			"without the `up >= 0` guard a clock disagreement reads as a crash loop.",
			h.Phase, protocol.PhaseSettled)
	}

	// --- 3. Ready, 2 restarts, up 5s. Recency without the count. ---
	//
	// One OOM or one node hiccup restarts a container recently. A recency-only rule
	// calls that failure; the threshold of 3 is what makes this a non-event, and it is
	// deliberately the same threshold the not-Ready path uses so the two paths cannot
	// disagree about one pod.
	blip := ready(runningFor(workerPod("w3", want, 20*time.Minute), "worker", stuckRestartThreshold-1, 5*time.Second, exit1, false),
		testNow.Add(-5*time.Second))
	if h := deriveRollHealth([]corev1.Pod{*blip}, want, "", testNow); h.Phase != protocol.PhaseSettled {
		t.Errorf("Phase = %q, want %q. %d restarts is below the threshold of %d — a single benign restart "+
			"must not read as a crash loop.", h.Phase, protocol.PhaseSettled, stuckRestartThreshold-1, stuckRestartThreshold)
	}

	// --- 4. THE HEALTHY ROLL. Ready, 0 restarts. Must not regress. ---
	//
	// This is every worker on every release. If it moves, the fix has turned the whole
	// fleet's badge red — the cry-wolf failure the roll-health feature exists to prevent.
	healthy := ready(runningFor(workerPod("w4", want, 3*time.Minute), "worker", 0, 90*time.Second, nil, false),
		testNow.Add(-90*time.Second))
	hh := deriveRollHealth([]corev1.Pod{*healthy}, want, "", testNow)
	if hh.Phase != protocol.PhaseSettled {
		t.Errorf("Phase = %q, want %q for a Ready pod that has never restarted", hh.Phase, protocol.PhaseSettled)
	}
	if hh.BlockingContainer != "" || hh.RestartCount != 0 || hh.LastExitCode != nil || hh.BlockingReason != "" {
		t.Errorf("a healthy settled pod carries diagnostics %q/%q/%d/%v, want all empty — blaming a "+
			"container on a healthy roll points an operator at innocent code",
			hh.BlockingContainer, hh.BlockingReason, hh.RestartCount, hh.LastExitCode)
	}
	if !hh.PhaseSince.Equal(testNow.Add(-90 * time.Second)) {
		t.Errorf("PhaseSince = %v, want the Ready transition — settled must still date from readiness",
			hh.PhaseSince)
	}

	// --- 5. Ready, 5 restarts, up 30s, NO lastState at all. ---
	//
	// MEASURED, and it is why the anchor is state.running.startedAt: a real container
	// instance carried restartCount 6 with `"lastState": {}` across its whole Ready
	// window. An implementation anchoring recency on lastState.terminated.finishedAt is
	// BLIND here and reports settled — the defect, unfixed, on the exact shape that
	// motivated the design.
	noLast := ready(runningFor(workerPod("w5", want, 20*time.Minute), "worker", 5, 30*time.Second, nil, false),
		testNow.Add(-30*time.Second))
	nh := deriveRollHealth([]corev1.Pod{*noLast}, want, "", testNow)
	if nh.Phase != protocol.PhaseStuck {
		t.Errorf("Phase = %q, want %q. This container has no lastState — measured on a real pod, not "+
			"hypothesised — so a rule needing lastState.terminated.finishedAt cannot see it at all.",
			nh.Phase, protocol.PhaseStuck)
	}
	if nh.BlockingContainer != "worker" || nh.RestartCount != 5 {
		t.Errorf("BlockingContainer = %q RestartCount = %d, want worker/5 even with no lastState",
			nh.BlockingContainer, nh.RestartCount)
	}
	if nh.LastExitCode != nil {
		t.Errorf("LastExitCode = %v, want nil — k8s genuinely had no exit code here, and inventing one "+
			"would be the same class of lie as blanking a measured one", nh.LastExitCode)
	}
	if nh.BlockingReason != "Restarting" {
		t.Errorf("BlockingReason = %q, want %q — with no termination to quote, the fallback token must "+
			"still be true of the pod, and \"not ready\" is not", nh.BlockingReason, "Restarting")
	}
}

// 🔴 A NON-RUNNING CONTAINER WITH A HIGH RESTART COUNT MUST NOT FLAG. This is the
// counterfactual for flappingContainer's `State.Running == nil` skip, and it is the
// normal case on every worker pod rather than an edge.
//
// `seed-nix` is a PLAIN init container — no RestartPolicy, so not a native sidecar. It
// runs once, exits, and stays Terminated for the pod's whole remaining life. Since the
// rule scans all containers, this loop reads that status on every tick of every pod in
// the fleet.
//
// THE MUTATION THIS TEST EXISTS TO CATCH IS DELETING THE NIL CHECK, not changing the
// count. With the check gone, an absent Running state reads as a zero StartedAt; the
// obvious "hardening" of that (`if t.IsZero() { t = now }`) makes it "up 0 seconds",
// which SATISFIES the recency conjunct — so the container flags forever, with no uptime
// that could ever clear it because it will never run again.
//
// RestartCount is 5 ON PURPOSE. With 0 the container fails the restart conjunct anyway
// and the case passes identically whether or not the nil check exists — a fixture where
// broken and correct agree, which proves nothing. 5 is what makes the guard falsifiable.
//
// ⚠ THIS PINS HALF OF WHAT THE TIDY WOULD BREAK. There is a sibling case with the same
// symptom and a DIFFERENT cause: a container that IS Running but carries a ZERO
// StartedAt also comes out settled today, because now.Sub(zero) is decades and the
// comparison is `<`. So the nil case is excluded by the skip, and the zero case by the
// arithmetic. `if t.IsZero() { t = now }` would flip BOTH into "up 0 seconds", and only
// this fixture would redden. If you are here because that edit broke this test, check
// the zero-StartedAt path too — nothing covers it.
//
// What this test does NOT establish: whether Kubernetes really persists an init
// container's RestartCount after a retry finally succeeds. That needs a live pod and it
// is not what is being pinned here — this is a hand-built ContainerStatus whose values
// are chosen to make the mutation redden. The guard is correct regardless: a container
// that is not running cannot be flapping.
func TestNonRunningContainerWithRestartsDoesNotFlag(t *testing.T) {
	const want = "hash-new"

	// A healthy Ready pod whose init container finished long ago, after retries.
	pod := ready(workerPod("w1", want, 3*time.Hour), testNow.Add(-3*time.Hour))
	pod.Status.InitContainerStatuses = append(pod.Status.InitContainerStatuses, corev1.ContainerStatus{
		Name:         "seed-nix",
		RestartCount: 5,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 0, Reason: "Completed", FinishedAt: metav1.NewTime(testNow.Add(-3 * time.Hour)),
		}},
	})
	// And the worker itself running happily since then, never restarted.
	pod = runningFor(pod, "worker", 0, 3*time.Hour, nil, false)

	h := deriveRollHealth([]corev1.Pod{*pod}, want, "", testNow)
	if h.Phase != protocol.PhaseSettled {
		t.Errorf("Phase = %q, want %q. seed-nix has 5 restarts and NO Running state — it exited hours "+
			"ago and will never run again. \"Is it flapping right now\" is unanswerable for a container "+
			"that is not running, so it must be skipped; scoring its absent StartedAt as \"up 0 seconds\" "+
			"pins a healthy worker red permanently, with no uptime that could ever clear it.",
			h.Phase, protocol.PhaseSettled)
	}
	if h.BlockingContainer != "" {
		t.Errorf("BlockingContainer = %q, want empty — a settled pod must blame nobody", h.BlockingContainer)
	}
}

// The flicker itself, which a single-tick assertion cannot see — the same discipline
// TestRollHealthStaysRollingAcrossTicksWhileThePodIsNotReady applies to `rolling`.
//
// A crash-looping pod alternates Ready and not-Ready every few tens of seconds, and the
// badge followed it: settled → stuck → settled → stuck. Asserting one Ready tick reads
// `stuck` does not prove the flicker is gone; asserting it across the ALTERNATION does.
// The verdict must hold `stuck` continuously, including on the Ready ticks in between.
func TestFlappingPodDoesNotFlickerBackToSettledAcrossTicks(t *testing.T) {
	const want = "hash-new"
	exit1 := &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}

	// One cycle of the measured trajectory: Ready running, dead, backing off, Ready
	// again on a fresh instance. Restart counts climb exactly as kubelet's do.
	ticks := []struct {
		at   time.Duration // before testNow
		pod  func(at time.Duration) *corev1.Pod
		what string
	}{
		{90 * time.Second, func(at time.Duration) *corev1.Pod {
			return ready(runningFor(workerPod("w1", want, time.Hour), "worker", 3, 20*time.Second, exit1, false),
				testNow.Add(-at))
		}, "Ready, 3 restarts, instance 20s old"},
		{60 * time.Second, func(time.Duration) *corev1.Pod {
			p := workerPod("w1", want, time.Hour)
			p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, corev1.ContainerStatus{
				Name: "worker", RestartCount: 3,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}},
			})
			return p
		}, "not Ready, terminated Error"},
		{40 * time.Second, func(time.Duration) *corev1.Pod {
			return waiting(workerPod("w1", want, time.Hour), "worker", "CrashLoopBackOff", 4, false)
		}, "not Ready, CrashLoopBackOff"},
		{10 * time.Second, func(at time.Duration) *corev1.Pod {
			return ready(runningFor(workerPod("w1", want, time.Hour), "worker", 4, 5*time.Second, exit1, false),
				testNow.Add(-at))
		}, "Ready again, 4 restarts, instance 5s old"},
	}

	for _, tk := range ticks {
		h := deriveRollHealth([]corev1.Pod{*tk.pod(tk.at)}, want, "", testNow)
		if h.Phase != protocol.PhaseStuck {
			t.Errorf("tick %q: Phase = %q, want %q. The badge alternating settled/stuck across this "+
				"cycle IS the flicker in issue #145 — one tick reading correctly proves nothing about it.",
				tk.what, h.Phase, protocol.PhaseStuck)
		}
	}
}

// A-1: a pod-list failure must NOT abort the reconcile cycle.
//
// MEASURED on dev-cluster: the deployed controller ServiceAccount answers `no` to
// `list pods` in both worker namespaces, with `list deployments` = yes as the positive
// control. Nothing in the chart orders worker-rbac.yaml ahead of
// controller-deployment.yaml. So this 403 is what the FIRST TICK after deployment does,
// not a hypothetical — and if it aborted the cycle, provisioning, teardown, patching and
// token delivery would all stop for the whole hosted fleet, on account of a read whose
// only consumer is a badge.
func TestPodListFailureDoesNotAbortObservation(t *testing.T) {
	ctx := context.Background()
	id := "w1"
	ns := testConfig().Namespace
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "uzi-hw-" + id, Namespace: ns, Labels: objectLabels(id)},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: objectLabels(id), Annotations: map[string]string{AnnotationSpecHash: "h"}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: workerContainerName, Image: "img:1"}}},
		}},
	}
	m, client := newMat(t, dep)

	// Exactly what RBAC produces before the new Role lands.
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Resource: "pods"}, "",
			errors.New(`pods is forbidden: User "system:serviceaccount:uzi:uzi-controller" cannot list resource "pods"`))
	})

	observed, err := m.Observe(ctx)
	if err != nil {
		t.Fatalf("Observe returned %v. A pod-list failure must be logged and swallowed: `.Roll` feeds "+
			"ONLY the display-only status report, and aborting here stops provisioning, teardown, "+
			"patching and token delivery for the entire fleet.", err)
	}
	if len(observed) != 1 {
		t.Fatalf("observed %d workers, want 1 — the deployment was read successfully, so the worker must "+
			"still be observable", len(observed))
	}
	if observed[0].Roll.Phase != "" {
		t.Errorf("Roll.Phase = %q, want empty: no pods could be read, so there is no roll health to "+
			"report and the api must see absence rather than a guess", observed[0].Roll.Phase)
	}

	// The whole point: Reconcile still runs, so the fleet still converges.
	if err := m.Reconcile(ctx, []protocol.DesiredWorker{{ID: id, Template: "base", Size: "m"}}, observed); err != nil {
		t.Fatalf("Reconcile after a pod-list failure: %v", err)
	}
}

// The Recreate gap: a worker with a Deployment and NO pods reports `rolling` explicitly.
//
// Emitting no row would make the api read "no signal", and the only thing then keeping a
// healthy mid-roll worker from flashing `outdated` would be the freshness TTL holding the
// PREVIOUS report alive — an invisible dependency on a constant in another module.
func TestRecreateGapReportsRollingRatherThanNoRow(t *testing.T) {
	ctx := context.Background()
	id := "w1"
	ns := testConfig().Namespace
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "uzi-hw-" + id, Namespace: ns, Labels: objectLabels(id)},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: objectLabels(id), Annotations: map[string]string{AnnotationSpecHash: "h"}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: workerContainerName, Image: "img:1"}}},
		}},
	}
	m, _ := newMat(t, dep) // deployment present, no pods at all

	observed, err := m.Observe(ctx)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(observed) != 1 {
		t.Fatalf("observed %d workers, want 1", len(observed))
	}
	if got := observed[0].Roll.Phase; got != protocol.PhaseRolling {
		t.Errorf("Roll.Phase = %q for a worker whose Deployment exists and whose pods do not, want %q "+
			"(the Recreate gap)", got, protocol.PhaseRolling)
	}
	if !observed[0].Roll.PhaseSince.IsZero() {
		t.Errorf("PhaseSince = %v, want zero: there is no pod to date the gap from", observed[0].Roll.PhaseSince)
	}
}

// ===========================================================================
// ISSUE #148 — THE DISCRIMINATOR, ASSERTED AS A PAIR IN ONE FUNCTION.
// ===========================================================================
//
// The bug: a hosted worker whose Deployment can NEVER produce a pod was never alerted
// on. Not at MaxUpgradingWindow, not ever. It took the pod-less branch, reported
// `rolling`, became api R2 `upgrading` — which Decision 1 deliberately excludes from the
// attention set — and past the INV-5 ceiling fell through to `unknown`, also silent.
//
// The fix is one condition lookup. The DANGER in the fix is the opposite failure: a
// healthy Recreate roll passes through pod-less for ~1.4s on every release, so reporting
// `stuck` for pod-lessness as such would turn the whole fleet red every time uzi ships.
//
// So the only thing worth testing is that the two are SEPARATED, and that is a claim
// about a pair, not about either case alone. Two independent subtests each asserting one
// half are both satisfied by an implementation that ignores its inputs in some way; this
// function is red unless the condition — and nothing else — is what moves the answer.
func TestPodlessIsStuckOnlyWithReplicaFailure(t *testing.T) {
	const want = "hash-new"

	// The healthy Recreate gap. MEASURED at ~1.4s on a real roll, on every release.
	gap := deriveRollHealth(nil, want, "", testNow)
	// The permanently-blocked worker. Same empty pod list, same hash, same clock.
	blocked := deriveRollHealth(nil, want, "FailedCreate", testNow)

	if gap.Phase != protocol.PhaseRolling {
		t.Errorf("the healthy Recreate gap reports %q, want %q. Reporting stuck for pod-lessness AS SUCH "+
			"cries wolf on every release — which is the failure PRD #113 exists to prevent, arriving "+
			"through the branch that fixes #148.", gap.Phase, protocol.PhaseRolling)
	}
	if blocked.Phase != protocol.PhaseStuck {
		t.Errorf("a pod-less worker whose Deployment asserts ReplicaFailure=True reports %q, want %q. "+
			"This is issue #148: %q becomes api R2 `upgrading`, which Decision 1 excludes from the "+
			"attention set, so the worker is never alerted on at all.",
			blocked.Phase, protocol.PhaseStuck, blocked.Phase)
	}
	if gap.Phase == blocked.Phase {
		t.Fatalf("both pod-less cases report %q. The condition is not the discriminator — whatever this "+
			"implementation keys on, it is not ReplicaFailure, and one of the two failure modes "+
			"(silent broken worker, or a red fleet on every release) is live.", gap.Phase)
	}
	if blocked.BlockingReason != "FailedCreate" {
		t.Errorf("BlockingReason = %q, want the condition's reason forwarded. Without it the api's "+
			"stuckDetail renders \"pod: not ready\" and the operator learns nothing about WHY no pod "+
			"exists", blocked.BlockingReason)
	}
	if gap.BlockingReason != "" {
		t.Errorf("the healthy gap carries BlockingReason = %q, want empty: nothing is blocking a roll "+
			"that is simply between pods", gap.BlockingReason)
	}
}

// The WIRING, which the table above cannot reach: that the reason actually comes off the
// Deployment's own `.status.conditions` through Observe, with no new RBAC.
//
// This is the half most likely to be silently absent — deriveRollHealth can be perfectly
// correct while observeNamespace never reads the condition, and every unit test above
// still passes because they pass the string in by hand.
//
// Three Deployments, all pod-less, differing ONLY in the condition, so the negative
// controls are aimed at the two ways this could pass without reading anything:
//
//   - ReplicaFailure=True   -> stuck. The bug's own shape.
//   - ReplicaFailure=False  -> rolling. Aimed at code that matches on the condition TYPE
//     and forgets the status, which would fire on any Deployment that ever recorded one.
//   - no conditions at all  -> rolling. The healthy Recreate gap, and what a healthy
//     Deployment really shows: the Deployment controller REMOVES the condition on a
//     successful sync rather than setting it False.
func TestObserveReadsReplicaFailureOffTheDeployment(t *testing.T) {
	ctx := context.Background()
	ns := testConfig().Namespace

	dep := func(id string, conds ...appsv1.DeploymentCondition) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "uzi-hw-" + id, Namespace: ns, Labels: objectLabels(id)},
			Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: objectLabels(id), Annotations: map[string]string{AnnotationSpecHash: "h"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: workerContainerName, Image: "img:1"}}},
			}},
			Status: appsv1.DeploymentStatus{Conditions: conds},
		}
	}
	// MEASURED VERBATIM, 2026-07-26, dev-cluster / uzi-workers, on issue #148's own worker
	// (d26fb0f9) while it was live and pod-less:
	//
	//	type=ReplicaFailure status=True reason=FailedCreate  message=<the 185 bytes below>
	//
	// A healthy hosted worker measured in the same sweep (uzi-hw-8e1fef71, ready 1/1) carried
	// NO ReplicaFailure condition at all — only Available=True and Progressing=True. So
	// ABSENCE is the healthy state, which is what replicaFailureReason's comment says and
	// what the "gap" fixture below stands for. ConditionFalse was NOT observed on a real
	// cluster; that case is kept anyway because it is the mutation target for the
	// Status-check, and a defensive control that costs one fixture is worth having.
	//
	// The message is in the fixture deliberately and expected NOWHERE in the output. At 185
	// bytes it is nearly 3x the api's 64-byte cap on controller-supplied display strings, and
	// truncation lands mid-token before the cause is ever reached — measured, the cap would
	// deliver `pods "uzi-hw-d26fb0f9-7158-42a5-9d9f-bed63526c217-65965d65fc-" i` and drop
	// "serviceaccount ... not found" entirely. Forwarding it would be strictly worse than the
	// reason.
	failing := appsv1.DeploymentCondition{
		Type:    appsv1.DeploymentReplicaFailure,
		Status:  corev1.ConditionTrue,
		Reason:  "FailedCreate",
		Message: `pods "uzi-hw-d26fb0f9-7158-42a5-9d9f-bed63526c217-65965d65fc-" is forbidden: error looking up service account uzi-workers/uzi-hosted-worker: serviceaccount "uzi-hosted-worker" not found`,
	}
	cleared := failing
	cleared.Status = corev1.ConditionFalse

	// No pods for any of them: every one of these is the pod-less branch.
	m, _ := newMat(t, dep("blocked", failing), dep("cleared", cleared), dep("gap"))

	observed, err := m.Observe(ctx)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	byID := map[string]reconcile.RollHealth{}
	for _, o := range observed {
		byID[o.ID] = o.Roll
	}
	if len(byID) != 3 {
		t.Fatalf("observed %d workers, want 3: %v", len(byID), byID)
	}

	if got := byID["blocked"].Phase; got != protocol.PhaseStuck {
		t.Errorf("the worker whose Deployment carries ReplicaFailure=True reports %q, want %q. The "+
			"condition is on an object observeNamespace ALREADY reads for SpecHash and TargetImage, so "+
			"a %q here means the lookup is not wired in — issue #148 is live even though "+
			"deriveRollHealth handles it", got, protocol.PhaseStuck, got)
	}
	if got := byID["blocked"].BlockingReason; got != "FailedCreate" {
		t.Errorf("BlockingReason = %q, want %q read off the condition", got, "FailedCreate")
	}
	// The MESSAGE must not travel. Same line the pod path holds: a k8s message is free text
	// carrying namespaces, object names and paths, and the api caps every controller-supplied
	// display string at 64 bytes — so forwarding this one would deliver its least
	// informative prefix and drop the actual cause. Asserted over every string on the row,
	// not only BlockingReason, so a later "helpful" assignment to any display field trips it.
	for field, v := range map[string]string{
		"BlockingReason":    byID["blocked"].BlockingReason,
		"BlockingContainer": byID["blocked"].BlockingContainer,
		"PodPhase":          byID["blocked"].PodPhase,
	} {
		if v != "" && strings.Contains(failing.Message, v) && len(v) > len("FailedCreate") {
			t.Errorf("%s = %q, which is a slice of the condition's MESSAGE. Only the reason may be "+
				"forwarded", field, v)
		}
	}

	if got := byID["cleared"].Phase; got != protocol.PhaseRolling {
		t.Errorf("a pod-less worker whose ReplicaFailure condition is FALSE reports %q, want %q. "+
			"Matching the condition TYPE without checking its STATUS fires on any Deployment that "+
			"ever recorded a create failure, long after it was fixed", got, protocol.PhaseRolling)
	}
	if got := byID["gap"].Phase; got != protocol.PhaseRolling {
		t.Errorf("a pod-less worker with NO conditions reports %q, want %q. This is the healthy Recreate "+
			"gap — measured at ~1.4s and traversed on every release — so a %q here turns the whole "+
			"fleet's badge red every time uzi ships", got, protocol.PhaseRolling, got)
	}
}
