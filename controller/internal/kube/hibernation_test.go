package kube

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

// hibRec is a Cordoner that also records hibernation transitions.
type hibRec struct {
	*fakeCordoner
	calls []protocol.HibernationTransition
	err   error
}

func (h *hibRec) ReportHibernation(_ context.Context, _ string, t protocol.HibernationTransition) error {
	h.calls = append(h.calls, t)
	return h.err
}

func (h *hibRec) transitions() []string {
	var out []string
	for _, c := range h.calls {
		out = append(out, c.Transition)
	}
	return out
}

func hibDeployment(replicas int32, op string) *appsv1.Deployment {
	d := deployedWorker("w1", 0, "")
	d.Spec.Replicas = &replicas
	if op != "" {
		d.Annotations = map[string]string{annotationHibernationOp: op}
	}
	return d
}

// hibCurrent is a Deployment whose template matches what the controller renders now.
func hibCurrent(t *testing.T, replicas int32, op string) *appsv1.Deployment {
	d := hibDeployment(replicas, op)
	d.Spec.Template.Annotations[AnnotationSpecHash] = currentHash(t, "w1", 0)
	return d
}

func hibPod(uid string, hash string, ready, terminating bool) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "pod-" + uid, Namespace: testConfig().Namespace, UID: types.UID(uid), Labels: objectLabels("w1"),
		Annotations: map[string]string{AnnotationSpecHash: hash},
	}}
	if ready {
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	}
	if terminating {
		now := metav1.Now()
		p.DeletionTimestamp = &now
	}
	return p
}

func hibW(state, op string) protocol.DesiredWorker {
	return protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", Hibernation: &protocol.Hibernation{State: state, OpID: op}}
}

func hibEnv(t *testing.T, objs ...runtime.Object) (*Materializer, *fake.Clientset, *hibRec) {
	t.Helper()
	rec := &hibRec{fakeCordoner: &fakeCordoner{}}
	all := append([]runtime.Object{pvcNix("w1", "20Gi"), pvcFor("w1", "data")}, objs...)
	m, client := newMatWithCordoner(t, rec, all...)
	return m, client, rec
}

func hibTick(t *testing.T, m *Materializer, ws ...protocol.DesiredWorker) error {
	t.Helper()
	ctx := context.Background()
	o, err := m.Observe(ctx)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	return m.Reconcile(ctx, ws, o)
}

func hibGet(t *testing.T, c *fake.Clientset) *appsv1.Deployment {
	t.Helper()
	d, err := c.AppsV1().Deployments(testConfig().Namespace).Get(context.Background(), "uzi-hw-w1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	return d
}

func assertNothingDeleted(t *testing.T, c *fake.Clientset) {
	t.Helper()
	for key := range deletedSet(c) {
		if !strings.HasPrefix(key, "pods/") { // a test may delete a pod to model its exit
			t.Fatalf("hibernation path deleted %s; no Deployment, PVC or Secret may be deleted", key)
		}
	}
	for _, name := range []string{"uzi-hw-w1-data", "uzi-hw-w1-nix"} {
		if _, err := c.CoreV1().PersistentVolumeClaims(testConfig().Namespace).Get(context.Background(), name, metav1.GetOptions{}); err != nil {
			t.Fatalf("pvc %s did not survive: %v", name, err)
		}
	}
}

// deploymentPatchBodies returns every Deployment patch body decoded.
func deploymentPatchBodies(c *fake.Clientset) []map[string]any {
	var out []map[string]any
	for _, a := range c.Actions() {
		if p, ok := a.(k8stesting.PatchAction); ok && a.GetResource().Resource == "deployments" {
			var body map[string]any
			_ = json.Unmarshal(p.GetPatch(), &body)
			out = append(out, body)
		}
	}
	return out
}

// replicaPatches counts Deployment patches that touch spec.replicas.
func replicaPatches(c *fake.Clientset) int {
	n := 0
	for _, b := range deploymentPatchBodies(c) {
		if spec, ok := b["spec"].(map[string]any); ok {
			if _, ok := spec["replicas"]; ok {
				n++
			}
		}
	}
	return n
}

func hasTransition(rec *hibRec, name string) bool {
	for _, c := range rec.calls {
		if c.Transition == name {
			return true
		}
	}
	return false
}

func TestHibernatedWorkerIsHeldAtZeroUnderDriftForceRollAndRestart(t *testing.T) {
	for _, forceRoll := range []bool{false, true} {
		// The Deployment is at zero with a stale template hash: ordinary drift.
		m, client, rec := hibEnv(t, hibDeployment(0, "op-1"))
		m.drain.ForceRoll = forceRoll
		if err := hibTick(t, m, hibW(protocol.HibernationHibernated, "op-1")); err != nil {
			t.Fatalf("forceRoll=%v tick: %v", forceRoll, err)
		}
		// A restarted controller: a fresh Materializer over the same objects.
		m2 := New(client, testConfig(), testResolver(t), rec, DrainPolicy{ForceRoll: forceRoll}, RecyclePolicy{Enabled: true}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err := hibTick(t, m2, hibW(protocol.HibernationHibernated, "op-1")); err != nil {
			t.Fatalf("forceRoll=%v restart tick: %v", forceRoll, err)
		}
		if n := len(deploymentPatchBodies(client)); n != 0 {
			t.Fatalf("forceRoll=%v: %d deployment patches on a hibernated worker, want none", forceRoll, n)
		}
		if r := hibGet(t, client).Spec.Replicas; r == nil || *r != 0 {
			t.Fatalf("forceRoll=%v: replicas = %v, want 0", forceRoll, r)
		}
		if len(rec.calls) != 0 {
			t.Fatalf("hold reported %v", rec.transitions())
		}
		assertNothingDeleted(t, client)
	}
}

func TestHibernatedWorkerRepatchesReplicasBackToZero(t *testing.T) {
	m, client, _ := hibEnv(t, hibDeployment(1, "op-1"))
	if err := hibTick(t, m, hibW(protocol.HibernationHibernated, "op-1")); err != nil {
		t.Fatal(err)
	}
	if r := hibGet(t, client).Spec.Replicas; r == nil || *r != 0 {
		t.Fatalf("replicas = %v, want 0", r)
	}
	if replicaPatches(client) != 1 {
		t.Fatalf("replica patches = %d, want exactly 1", replicaPatches(client))
	}
	assertNothingDeleted(t, client)
}

func TestMissingDeploymentIsCreatedAtZeroWhilePreparingAndHibernated(t *testing.T) {
	for _, state := range []string{protocol.HibernationPreparing, protocol.HibernationHibernated} {
		m, client, rec := hibEnv(t)
		if err := hibTick(t, m, hibW(state, "op-9")); err != nil {
			t.Fatalf("%s: %v", state, err)
		}
		d := hibGet(t, client)
		if d.Spec.Replicas == nil || *d.Spec.Replicas != 0 || d.Annotations[annotationHibernationOp] != "op-9" {
			t.Fatalf("%s: created deployment replicas=%v annotations=%v", state, d.Spec.Replicas, d.Annotations)
		}
		if len(rec.calls) != 0 {
			t.Fatalf("%s: a create at zero reported %v (never refuse, nothing to fence)", state, rec.transitions())
		}
		assertNothingDeleted(t, client)
	}
}

func TestHibernatedWorkerWithNixSizeDriftKeepsItsNixVolume(t *testing.T) {
	rec := &hibRec{fakeCordoner: &fakeCordoner{}}
	m, client := newMatWithCordoner(t, rec, hibDeployment(0, "op-1"), pvcNix("w1", "4Gi"), pvcFor("w1", "data"))
	for _, state := range []string{protocol.HibernationHibernated, protocol.HibernationPreparing} {
		if err := hibTick(t, m, hibW(state, "op-1")); err != nil {
			t.Fatalf("%s: %v", state, err)
		}
	}
	p, err := client.CoreV1().PersistentVolumeClaims(testConfig().Namespace).Get(context.Background(), "uzi-hw-w1-nix", metav1.GetOptions{})
	if err != nil || p.DeletionTimestamp != nil {
		t.Fatalf("nix pvc was recycled: %v", err)
	}
	assertNothingDeleted(t, client)
}

func TestScaledDownIsReportedOnlyOnceThePodsAreGone(t *testing.T) {
	m, client, rec := hibEnv(t, hibDeployment(1, ""), hibPod("pod-uid-1", "x", true, false))
	w := hibW(protocol.HibernationPreparing, "op-1")

	if err := hibTick(t, m, w); err != nil {
		t.Fatal(err)
	}
	d := hibGet(t, client)
	if *d.Spec.Replicas != 0 || d.Annotations[annotationHibernationOp] != "op-1" {
		t.Fatalf("tick 1 did not scale down with the op annotation: %v %v", d.Spec.Replicas, d.Annotations)
	}
	if got := rec.transitions(); len(got) != 1 || got[0] != protocol.TransitionScalingDown {
		t.Fatalf("tick 1 transitions = %v, want [scaling_down]", got)
	}
	c := rec.calls[0]
	if c.OpID != "op-1" || c.DeploymentUID != string(d.UID) || c.ResourceVersion != d.ResourceVersion || c.PodUID != "pod-uid-1" {
		t.Fatalf("scaling_down = %+v, deployment uid=%s rv=%s", c, d.UID, d.ResourceVersion)
	}

	// The pod is still there (terminating counts too): still only scaling_down.
	if err := hibTick(t, m, w); err != nil {
		t.Fatal(err)
	}
	if hasTransition(rec, protocol.TransitionScaledDown) {
		t.Fatal("scaled_down reported while a pod still exists")
	}
	if err := client.CoreV1().Pods(testConfig().Namespace).Delete(context.Background(), "pod-pod-uid-1", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := hibTick(t, m, w); err != nil {
		t.Fatal(err)
	}
	last := rec.calls[len(rec.calls)-1]
	d = hibGet(t, client)
	if last.Transition != protocol.TransitionScaledDown || last.DeploymentUID != string(d.UID) || last.ResourceVersion != d.ResourceVersion {
		t.Fatalf("final transition = %+v, want scaled_down at the deployment's uid/rv", last)
	}
	// Only the first tick patched; later ticks only observed.
	if n := len(deploymentPatchBodies(client)); n != 1 {
		t.Fatalf("deployment patches = %d, want 1", n)
	}
	assertNothingDeleted(t, client)
}

func TestTerminatingPodStillBlocksScaledDown(t *testing.T) {
	m, client, rec := hibEnv(t, hibDeployment(0, "op-1"), hibPod("pod-uid-1", "x", false, true))
	if err := hibTick(t, m, hibW(protocol.HibernationPreparing, "op-1")); err != nil {
		t.Fatal(err)
	}
	if hasTransition(rec, protocol.TransitionScaledDown) {
		t.Fatal("scaled_down reported with a terminating pod present")
	}
	assertNothingDeleted(t, client)
}

// A scale-down computed from an old read cannot land once anything else has moved the
// Deployment: first the wake patch, and still after the api has claimed the worker back
// to awake (a later poll with no hibernation state).
func TestStaleScaleDownFailsItsPreconditionAfterWakeAndAfterClaim(t *testing.T) {
	m, client, _ := hibEnv(t, hibDeployment(0, "old-op"))
	stale := hibGet(t, client) // what a delayed scale-down for old-op had read
	if err := hibTick(t, m, hibW(protocol.HibernationWaking, "wake-op")); err != nil {
		t.Fatal(err)
	}
	staleBody := func() []byte {
		b, _ := json.Marshal(map[string]any{
			"metadata": map[string]any{"uid": stale.UID, "resourceVersion": stale.ResourceVersion, "annotations": map[string]any{annotationHibernationOp: "old-op"}},
			"spec":     map[string]any{"replicas": 0},
		})
		return b
	}
	try := func(label string) {
		_, err := client.AppsV1().Deployments(testConfig().Namespace).Patch(context.Background(), "uzi-hw-w1", types.MergePatchType, staleBody(), metav1.PatchOptions{})
		if !apierrors.IsConflict(err) {
			t.Fatalf("%s: stale scale-down err = %v, want a conflict", label, err)
		}
	}
	try("after wake")
	d := hibGet(t, client)
	if *d.Spec.Replicas != 1 || d.Annotations[annotationHibernationOp] != "wake-op" {
		t.Fatalf("wake patch did not land: %v %v", d.Spec.Replicas, d.Annotations)
	}
	// The claim is api-side; the controller sees it as a later poll with no state.
	awake := hibW("", "")
	awake.Hibernation = nil
	if err := hibTick(t, m, awake); err != nil {
		t.Fatal(err)
	}
	try("after claim")
}

// The refusal fence is a conditioned write too: a scale-down computed before the
// refusal cannot land after it, even once the api has set the state back to none.
func TestDelayedScaleDownAfterRefusalFailsItsPrecondition(t *testing.T) {
	d := hibDeployment(1, "")
	m, client, rec := hibEnv(t, d, hibPod("pod-uid-1", "x", true, false))
	stale := hibGet(t, client)
	w := hibW(protocol.HibernationPreparing, "op-1")
	w.Busy = true
	if err := hibTick(t, m, w); err != nil {
		t.Fatal(err)
	}
	if got := rec.transitions(); len(got) != 1 || got[0] != protocol.TransitionRefused {
		t.Fatalf("transitions = %v, want [refused]", got)
	}
	if replicaPatches(client) != 0 {
		t.Fatal("a refusal tick patched replicas")
	}
	body, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"uid": stale.UID, "resourceVersion": stale.ResourceVersion, "annotations": map[string]any{annotationHibernationOp: "op-1"}},
		"spec":     map[string]any{"replicas": 0},
	})
	_, err := client.AppsV1().Deployments(testConfig().Namespace).Patch(context.Background(), "uzi-hw-w1", types.MergePatchType, body, metav1.PatchOptions{})
	if !apierrors.IsConflict(err) {
		t.Fatalf("delayed scale-down err = %v, want a conflict", err)
	}
}

func TestRefusedCarriesTheFencePatchsResourceVersion(t *testing.T) {
	m, client, rec := hibEnv(t, hibDeployment(1, ""), hibPod("pod-uid-1", "x", true, false))
	before := hibGet(t, client)
	w := hibW(protocol.HibernationPreparing, "op-1")
	w.Busy = true
	if err := hibTick(t, m, w); err != nil {
		t.Fatal(err)
	}
	after := hibGet(t, client)
	if len(rec.calls) != 1 {
		t.Fatalf("calls = %v", rec.transitions())
	}
	c := rec.calls[0]
	if c.Transition != protocol.TransitionRefused || c.OpID != "op-1" || c.DeploymentUID != string(before.UID) || c.Reason != refuseBusy {
		t.Fatalf("refused = %+v", c)
	}
	if c.ResourceVersion != after.ResourceVersion || c.ResourceVersion == before.ResourceVersion {
		t.Fatalf("refused rv = %q, want the fence patch's %q (not the observed %q)", c.ResourceVersion, after.ResourceVersion, before.ResourceVersion)
	}
	if !strings.HasPrefix(after.Annotations[annotationHibernationRefused], "op-1/") {
		t.Fatalf("fence annotation missing: %v", after.Annotations)
	}
	if *after.Spec.Replicas != 1 {
		t.Fatal("refusal scaled the worker")
	}
}

func TestFenceConflictSendsNoRefused(t *testing.T) {
	m, client, rec := hibEnv(t, hibDeployment(1, ""), hibPod("pod-uid-1", "x", true, false))
	client.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, "uzi-hw-w1", errors.New("moved"))
	})
	w := hibW(protocol.HibernationPreparing, "op-1")
	w.Busy = true
	if err := hibTick(t, m, w); err != nil {
		t.Fatalf("a fence conflict is not a tick error: %v", err)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("fence conflict reported %v", rec.transitions())
	}
}

func TestFailClosedRefusalsIssueNoScaleDown(t *testing.T) {
	cfg := testConfig()
	base := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m"}
	malformed := hibCurrent(t, 1, "")
	malformed.Annotations = map[string]string{annotationRoll: "{not json"}
	garbageMirror := pvcFor("w1", "data")
	garbageMirror.Annotations = map[string]string{annotationRoll: "{not json"}
	type tc struct {
		objs   []runtime.Object
		noData bool
		mod    func(*protocol.DesiredWorker)
		setup  func(*fake.Clientset)
		reason string
	}
	for name, c := range map[string]tc{
		"existing roll marker":     {objs: []runtime.Object{markedDeployment(t, cfg, base, false)}, reason: refuseRollReservation},
		"malformed marker":         {objs: []runtime.Object{malformed}, reason: refuseRollReservation},
		"retained raw reservation": {objs: []runtime.Object{hibCurrent(t, 1, ""), garbageMirror}, noData: true, reason: refuseRollReservation},
		"disk pressure":            {objs: []runtime.Object{hibCurrent(t, 1, "")}, mod: func(w *protocol.DesiredWorker) { w.DiskPressure = true }, reason: refuseDiskPressure},
		"dind maintenance": {objs: []runtime.Object{hibCurrent(t, 1, "")}, mod: func(w *protocol.DesiredWorker) { w.DindMaintenance = &protocol.DindMaintenance{Phase: "requested"} },
			reason: refuseDinDMaintenance},
		"worker busy": {objs: []runtime.Object{hibCurrent(t, 1, "")}, mod: func(w *protocol.DesiredWorker) { w.Busy = true }, reason: refuseBusy},
		"incomplete snapshot": {objs: []runtime.Object{hibCurrent(t, 1, "")}, reason: refuseRollBlocked, setup: func(c *fake.Clientset) {
			c.PrependReactor("list", "replicasets", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("replicasets unavailable")
			})
		}},
		"no roll evidence (ephemeral)": {objs: []runtime.Object{hibCurrent(t, 1, "")}, mod: func(w *protocol.DesiredWorker) { w.Ephemeral = true }, reason: refuseNoRollEvidence},
	} {
		t.Run(name, func(t *testing.T) {
			rec := &hibRec{fakeCordoner: &fakeCordoner{}}
			objs := c.objs
			if c.noData {
				objs = append([]runtime.Object{pvcNix("w1", "20Gi")}, objs...)
			} else {
				objs = append([]runtime.Object{pvcNix("w1", "20Gi"), pvcFor("w1", "data")}, objs...)
			}
			m, client := newMatWithCordoner(t, rec, objs...)
			if c.setup != nil {
				c.setup(client)
			}
			w := hibW(protocol.HibernationPreparing, "op-1")
			if c.mod != nil {
				c.mod(&w)
			}
			if err := hibTick(t, m, w); err != nil {
				t.Fatalf("refusal tick: %v", err)
			}
			if replicaPatches(client) != 0 {
				t.Fatalf("a refusal case issued a replicas patch: %v", deploymentPatchBodies(client))
			}
			if got := rec.transitions(); len(got) != 1 || got[0] != protocol.TransitionRefused {
				t.Fatalf("transitions = %v, want exactly [refused]", got)
			}
			if rec.calls[0].Reason != c.reason {
				t.Fatalf("reason = %q, want %q", rec.calls[0].Reason, c.reason)
			}
			assertNothingDeleted(t, client)
		})
	}

	t.Run("layout migration held", func(t *testing.T) {
		f := lmSetup(t, "old", false, "20Gi")
		rec := &hibRec{fakeCordoner: &fakeCordoner{}}
		f.m.cordoner = rec
		w := lmWorker(true)
		w.Hibernation = &protocol.Hibernation{State: protocol.HibernationPreparing, OpID: "op-1"}
		obs, err := f.m.Observe(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := f.m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, obs); err != nil {
			t.Fatalf("refusal tick: %v", err)
		}
		if replicaPatches(f.client) != 0 {
			t.Fatalf("a refusal case issued a replicas patch: %v", deploymentPatchBodies(f.client))
		}
		if got := rec.transitions(); len(got) != 1 || got[0] != protocol.TransitionRefused || rec.calls[0].Reason != refuseLayoutHeld {
			t.Fatalf("calls = %+v, want one refused with %s", rec.calls, refuseLayoutHeld)
		}
	})
}

func TestAwakeDriftedSiblingStillRollsWhenItsOnlyCoveragePeerIsHibernated(t *testing.T) {
	sibling := deployedWorker("w2", 0, "the-old-releases-hash")
	rec := &hibRec{fakeCordoner: &fakeCordoner{}}
	m, client := newMatWithCordoner(t, rec, hibDeployment(0, "op-1"), sibling,
		pvcNix("w1", "20Gi"), pvcFor("w1", "data"), pvcNix("w2", "20Gi"), pvcFor("w2", "data"))
	w2 := evidence(protocol.DesiredWorker{ID: "w2", Template: "base", Size: "m"}, m5Now)
	w1 := evidence(hibW(protocol.HibernationHibernated, "op-1"), m5Now)
	if err := hibTick(t, m, w1, w2); err != nil {
		t.Fatalf("tick: %v", err)
	}
	patched := false
	for _, a := range client.Actions() {
		if p, ok := a.(k8stesting.PatchAction); ok && a.GetResource().Resource == "deployments" && p.GetName() == "uzi-hw-w2" && strings.Contains(string(p.GetPatch()), `"template"`) {
			patched = true
		}
		if p, ok := a.(k8stesting.PatchAction); ok && p.GetName() == "uzi-hw-w1" {
			t.Fatalf("hibernated sibling was patched: %s", p.GetPatch())
		}
	}
	if !patched {
		t.Fatal("the awake drifted worker was not rolled because its coverage peer is hibernated")
	}
}

func TestAmbiguousPatchErrorReportsNothingAndTheNextTickRereads(t *testing.T) {
	m, client, rec := hibEnv(t, hibDeployment(1, ""), hibPod("pod-uid-1", "x", true, false))
	fail := true
	client.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if fail {
			return true, nil, errors.New("timeout: outcome unknown")
		}
		return false, nil, nil
	})
	w := hibW(protocol.HibernationPreparing, "op-1")
	if err := hibTick(t, m, w); err == nil {
		t.Fatal("an ambiguous patch error was swallowed")
	}
	if len(rec.calls) != 0 {
		t.Fatalf("reported %v after an ambiguous error", rec.transitions())
	}
	fail = false
	if err := hibTick(t, m, w); err != nil {
		t.Fatal(err)
	}
	if got := rec.transitions(); len(got) != 1 || got[0] != protocol.TransitionScalingDown {
		t.Fatalf("next tick transitions = %v, want [scaling_down]", got)
	}
}

func TestWakingPatchesReplicasAndAnnotationOnceThenReportsReady(t *testing.T) {
	hash := currentHash(t, "w1", 0)
	m, client, rec := hibEnv(t, hibCurrent(t, 0, "sleep-op"))
	w := hibW(protocol.HibernationWaking, "wake-op")

	if err := hibTick(t, m, w); err != nil {
		t.Fatal(err)
	}
	d := hibGet(t, client)
	if *d.Spec.Replicas != 1 || d.Annotations[annotationHibernationOp] != "wake-op" {
		t.Fatalf("wake patch: replicas=%v annotations=%v", d.Spec.Replicas, d.Annotations)
	}
	if n := len(deploymentPatchBodies(client)); n != 1 {
		t.Fatalf("wake issued %d patches, want exactly one", n)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("ready reported before any pod: %v", rec.transitions())
	}

	// A Ready pod for the current template appears.
	if _, err := client.CoreV1().Pods(testConfig().Namespace).Create(context.Background(), hibPod("pod-uid-7", hash, true, false), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := hibTick(t, m, w); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("calls = %v", rec.transitions())
	}
	c := rec.calls[0]
	d = hibGet(t, client)
	if c.Transition != protocol.TransitionReady || c.OpID != "wake-op" || c.PodUID != "pod-uid-7" || c.DeploymentUID != string(d.UID) || c.ResourceVersion != d.ResourceVersion {
		t.Fatalf("ready = %+v (deployment uid=%s rv=%s)", c, d.UID, d.ResourceVersion)
	}
	if n := len(deploymentPatchBodies(client)); n != 1 {
		t.Fatalf("ready tick patched again: %d patches", n)
	}
	assertNothingDeleted(t, client)
}

func TestWakingCreatesAMissingDeploymentAwakeWithTheOpAnnotation(t *testing.T) {
	m, client, _ := hibEnv(t)
	if err := hibTick(t, m, hibW(protocol.HibernationWaking, "wake-op")); err != nil {
		t.Fatal(err)
	}
	d := hibGet(t, client)
	if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 || d.Annotations[annotationHibernationOp] != "wake-op" {
		t.Fatalf("created: replicas=%v annotations=%v", d.Spec.Replicas, d.Annotations)
	}
}

// A wake that has to create the Deployment carries a retained roll marker onto it, the
// same as the ordinary create path.
func TestWakingCreateCarriesTheRetainedRollMarker(t *testing.T) {
	cfg := testConfig()
	base := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m"}
	marker := markedDeployment(t, cfg, base, false).Annotations[annotationRoll]
	mirror := pvcFor("w1", "data")
	mirror.Annotations = map[string]string{annotationRoll: marker}
	rec := &hibRec{fakeCordoner: &fakeCordoner{}}
	m, client := newMatWithCordoner(t, rec, pvcNix("w1", "20Gi"), mirror)
	if err := hibTick(t, m, hibW(protocol.HibernationWaking, "wake-op")); err != nil {
		t.Fatal(err)
	}
	d := hibGet(t, client)
	if d.Annotations[annotationRoll] != marker {
		t.Fatalf("created Deployment roll annotation = %q, want the retained marker %q", d.Annotations[annotationRoll], marker)
	}
	if d.Annotations[annotationHibernationOp] != "wake-op" {
		t.Fatalf("op annotation = %q", d.Annotations[annotationHibernationOp])
	}
}

func TestWakingBypassesBusyAndConvergesAStaleTemplate(t *testing.T) {
	// Already at replicas 1 under the wake op, but the template is stale: converge it
	// rather than wait forever for a Ready pod of the wrong template.
	m, client, rec := hibEnv(t, hibDeployment(1, "wake-op"), hibPod("pod-uid-1", "stale", true, false))
	w := hibW(protocol.HibernationWaking, "wake-op")
	w.Busy = true
	if err := hibTick(t, m, w); err != nil {
		t.Fatal(err)
	}
	if n := len(deploymentPatchBodies(client)); n != 1 {
		t.Fatalf("patches = %d, want one template patch despite Busy", n)
	}
	if hibGet(t, client).Spec.Template.Annotations[AnnotationSpecHash] != currentHash(t, "w1", 0) {
		t.Fatal("template was not converged")
	}
	if hasTransition(rec, protocol.TransitionReady) {
		t.Fatal("ready reported for a pod of the stale template")
	}
}

func TestTerminatingReadyPodIsNeverReportedReady(t *testing.T) {
	hash := currentHash(t, "w1", 0)
	m, client, rec := hibEnv(t, hibCurrent(t, 1, "wake-op"), hibPod("dying", hash, true, true))
	w := hibW(protocol.HibernationWaking, "wake-op")
	if err := hibTick(t, m, w); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("terminating ready pod reported %v", rec.transitions())
	}
	if _, err := client.CoreV1().Pods(testConfig().Namespace).Create(context.Background(), hibPod("live", hash, true, false), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := hibTick(t, m, w); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 1 || rec.calls[0].PodUID != "live" {
		t.Fatalf("calls = %+v, want ready for the live pod", rec.calls)
	}
}

func TestAmbiguousWakePatchErrorReportsNoReady(t *testing.T) {
	hash := currentHash(t, "w1", 0)
	m, client, rec := hibEnv(t, hibCurrent(t, 0, "sleep-op"), hibPod("pod-uid-7", hash, true, false))
	fail := true
	client.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if fail {
			return true, nil, errors.New("timeout: outcome unknown")
		}
		return false, nil, nil
	})
	w := hibW(protocol.HibernationWaking, "wake-op")
	if err := hibTick(t, m, w); err == nil {
		t.Fatal("ambiguous wake patch error was swallowed")
	}
	if len(rec.calls) != 0 {
		t.Fatalf("reported %v", rec.transitions())
	}
	fail = false
	if err := hibTick(t, m, w); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 0 {
		t.Fatal("ready reported on the tick that patched; it must re-read first")
	}
	if err := hibTick(t, m, w); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 1 || rec.calls[0].Transition != protocol.TransitionReady {
		t.Fatalf("calls = %v", rec.transitions())
	}
}

func TestSecretAndVolumesSurviveEveryHibernationPath(t *testing.T) {
	for _, state := range []string{protocol.HibernationPreparing, protocol.HibernationHibernated, protocol.HibernationWaking} {
		m, client, _ := hibEnv(t, hibDeployment(1, ""), hibPod("p", "x", true, false))
		w := hibW(state, "op")
		w.JoinToken = token("uzw_secret")
		for i := 0; i < 3; i++ {
			if err := hibTick(t, m, w); err != nil {
				t.Fatalf("%s: %v", state, err)
			}
		}
		for _, a := range client.Actions() {
			if a.GetResource().Resource == "secrets" && a.GetVerb() == "delete" {
				t.Fatalf("%s: the Secret was deleted", state)
			}
		}
		assertNothingDeleted(t, client)
		assertNoSecretReads(t, client)
	}
}

// noopKeepsRV makes the fake apiserver behave like a real one for a merge patch that
// changes nothing: the object is returned as stored, with no resourceVersion bump.
// The shared enforcing client bumps on every patch, which hides that a repeated
// identical fence would not advance the resourceVersion.
func noopKeepsRV(client *fake.Clientset) {
	client.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		action := a.(k8stesting.PatchAction)
		old, err := client.Tracker().Get(a.GetResource(), a.GetNamespace(), action.GetName())
		if err != nil {
			return true, nil, err
		}
		var patch map[string]any
		if err := json.Unmarshal(action.GetPatch(), &patch); err != nil {
			return true, nil, err
		}
		oldMeta := old.(*appsv1.Deployment)
		md, _ := patch["metadata"].(map[string]any)
		if md["uid"] != string(oldMeta.UID) || md["resourceVersion"] != oldMeta.ResourceVersion {
			return false, nil, nil // let the enforcing reactor produce the conflict
		}
		delete(md, "uid")
		delete(md, "resourceVersion")
		stripped, _ := json.Marshal(patch)
		raw, _ := json.Marshal(old)
		merged, err := jsonpatch.MergePatch(raw, stripped)
		if err != nil {
			return true, nil, err
		}
		if jsonpatch.Equal(raw, merged) {
			return true, old.DeepCopyObject(), nil
		}
		return false, nil, nil
	})
}

// The fence must change the object every time it is sent, or a real apiserver leaves
// the resourceVersion alone and the write is unprovable. A refused report that failed
// is re-fenced and re-sent next tick.
func TestRefusalFenceReadvancesAndIsResentAfterAFailedReport(t *testing.T) {
	m, client, rec := hibEnv(t, hibDeployment(1, ""), hibPod("pod-uid-1", "x", true, false))
	noopKeepsRV(client)
	rec.err = errors.New("api 503")
	w := hibW(protocol.HibernationPreparing, "op-1")
	w.Busy = true

	if err := hibTick(t, m, w); err == nil {
		t.Fatal("tick 1 swallowed the failed refused report")
	}
	rv1 := hibGet(t, client).ResourceVersion
	if err := hibTick(t, m, w); err == nil {
		t.Fatal("tick 2 swallowed the failed refused report")
	}
	rv2 := hibGet(t, client).ResourceVersion
	if rv1 == rv2 {
		t.Fatalf("second fence did not advance the resourceVersion (%s)", rv1)
	}
	if got := rec.transitions(); len(got) != 2 || got[0] != protocol.TransitionRefused || got[1] != protocol.TransitionRefused {
		t.Fatalf("transitions = %v, want refused re-sent", got)
	}
	if rec.calls[1].ResourceVersion != rv2 || rec.calls[0].ResourceVersion != rv1 {
		t.Fatalf("refused rvs = %s, %s; fence rvs = %s, %s", rec.calls[0].ResourceVersion, rec.calls[1].ResourceVersion, rv1, rv2)
	}
	if replicaPatches(client) != 0 {
		t.Fatal("a refusal tick patched replicas")
	}
}

// A refusal reason that appears after the scale-down landed fences the object but
// carries on with the op's progress, on every later tick, without erroring.
func TestRefusalAfterScaleDownLandedKeepsReportingProgress(t *testing.T) {
	m, client, rec := hibEnv(t, hibDeployment(1, ""), hibPod("pod-uid-1", "x", true, false))
	noopKeepsRV(client)
	w := hibW(protocol.HibernationPreparing, "op-1")

	if err := hibTick(t, m, w); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	w.Busy = true // the reason appears only now
	if err := hibTick(t, m, w); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if err := client.CoreV1().Pods(testConfig().Namespace).Delete(context.Background(), "pod-pod-uid-1", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := hibTick(t, m, w); err != nil {
		t.Fatalf("tick 3: %v", err)
	}
	want := []string{protocol.TransitionScalingDown, protocol.TransitionScalingDown, protocol.TransitionScaledDown}
	got := rec.transitions()
	if len(got) != len(want) {
		t.Fatalf("transitions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("transitions = %v, want %v", got, want)
		}
	}
	d := hibGet(t, client)
	if last := rec.calls[2]; last.ResourceVersion != d.ResourceVersion || last.DeploymentUID != string(d.UID) {
		t.Fatalf("scaled_down = %+v, deployment rv=%s", last, d.ResourceVersion)
	}
	if replicaPatches(client) != 1 {
		t.Fatalf("replica patches = %d, want exactly the first scale-down", replicaPatches(client))
	}
}

// A Deployment missing while a roll recycle is evidenced must not be recreated at zero
// and nothing is reported: the api's preparing deadline moves the worker on.
func TestMissingDeploymentUnderRollEvidenceIsNotCreatedWhilePreparing(t *testing.T) {
	cfg := testConfig()
	base := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m"}
	marker := markedDeployment(t, cfg, base, false).Annotations[annotationRoll]
	mirror := pvcFor("w1", "data")
	mirror.Annotations = map[string]string{annotationRoll: marker}
	garbage := pvcFor("w1", "data")
	garbage.Annotations = map[string]string{annotationRoll: "{not json"}
	type tc struct {
		claim *corev1.PersistentVolumeClaim
		setup func(*fake.Clientset)
	}
	for name, c := range map[string]tc{
		"retained marker":         {claim: mirror},
		"raw garbage reservation": {claim: garbage},
		"blocked snapshot": {claim: pvcFor("w1", "data"), setup: func(c *fake.Clientset) {
			c.PrependReactor("list", "replicasets", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("replicasets unavailable")
			})
		}},
	} {
		t.Run(name, func(t *testing.T) {
			rec := &hibRec{fakeCordoner: &fakeCordoner{}}
			m, client := newMatWithCordoner(t, rec, pvcNix("w1", "20Gi"), c.claim)
			if c.setup != nil {
				c.setup(client)
			}
			if err := hibTick(t, m, hibW(protocol.HibernationPreparing, "op-1")); err != nil {
				t.Fatal(err)
			}
			if _, err := client.AppsV1().Deployments(testConfig().Namespace).Get(context.Background(), "uzi-hw-w1", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
				t.Fatalf("a Deployment was created mid-roll: %v", err)
			}
			if len(rec.calls) != 0 {
				t.Fatalf("reported %v", rec.transitions())
			}
		})
	}
}

// A controller restarted mid-preparing finds the op annotation and replicas 0 with a pod
// still present: it reports progress from what it reads, then scaled_down, and never
// scales down a second time.
func TestRestartMidPreparingResumesWithoutASecondScaleDown(t *testing.T) {
	_, client, rec := hibEnv(t, hibDeployment(0, "op-1"), hibPod("pod-uid-1", "x", false, true))
	m2 := New(client, testConfig(), testResolver(t), rec, DrainPolicy{}, RecyclePolicy{Enabled: true}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w := hibW(protocol.HibernationPreparing, "op-1")
	if err := hibTick(t, m2, w); err != nil {
		t.Fatal(err)
	}
	if got := rec.transitions(); len(got) != 1 || got[0] != protocol.TransitionScalingDown || rec.calls[0].PodUID != "pod-uid-1" {
		t.Fatalf("restart tick = %+v, want scaling_down for the surviving pod", rec.calls)
	}
	if err := client.CoreV1().Pods(testConfig().Namespace).Delete(context.Background(), "pod-pod-uid-1", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := hibTick(t, m2, w); err != nil {
		t.Fatal(err)
	}
	if last := rec.calls[len(rec.calls)-1]; last.Transition != protocol.TransitionScaledDown {
		t.Fatalf("final = %+v, want scaled_down", last)
	}
	if n := len(deploymentPatchBodies(client)); n != 0 {
		t.Fatalf("restart issued %d deployment patches, want none", n)
	}
}
