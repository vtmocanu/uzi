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
		name       string
		pods       []corev1.Pod
		wantPhase  string
		wantReason string
		check      func(t *testing.T, h reconcile.RollHealth)
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
			name:      "only the OLD pod is left: the new one is not up yet, so rolling",
			pods:      []corev1.Pod{*workerPod("w1", "hash-old", time.Minute)},
			wantPhase: protocol.PhaseRolling,
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
			h := deriveRollHealth(tc.pods, want, testNow)
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
	h := deriveRollHealth([]corev1.Pod{*flapping}, want, testNow)

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
	if h := deriveRollHealth([]corev1.Pod{*below}, want, testNow); h.Phase != protocol.PhaseRolling {
		t.Errorf("a Running container with %d restarts (below the threshold of %d) reads %q, want %q",
			stuckRestartThreshold-1, stuckRestartThreshold, h.Phase, protocol.PhaseRolling)
	}

	// And a healthy not-yet-Ready pod with ZERO restarts must have NO container blamed —
	// naming one with 0 restarts would point an operator at innocent code.
	fresh := running(workerPod("w3", want, 10*time.Second), "worker", 0, false)
	if h := deriveRollHealth([]corev1.Pod{*fresh}, want, testNow); h.BlockingContainer != "" {
		t.Errorf("BlockingContainer = %q for a pod with no restarts, want empty", h.BlockingContainer)
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
