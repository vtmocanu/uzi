package kube

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

// Paused-restore bind-timeout deferral (issue #4468): a worker deliberately held at zero
// replicas under an open roll reservation has no pod, so its WaitForFirstConsumer claims
// cannot bind and must not be reported as stranded after pvcBindTimeout.

const pwWFFC = "wffc"

var errPWBoom = errors.New("boom")

func pwClass(name string, mode *storagev1.VolumeBindingMode) *storagev1.StorageClass {
	return &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name}, Provisioner: "x", VolumeBindingMode: mode}
}

func pwMode(m storagev1.VolumeBindingMode) *storagev1.VolumeBindingMode { return &m }

// pwPoller serves the fixture's current desired set, so a test can lift a hold between ticks.
type pwPoller struct{ f *pwFix }

func (p pwPoller) Poll(context.Context) (protocol.PollResponse, error) {
	return protocol.PollResponse{Workers: p.f.desired}, nil
}

// pwFix is a real Materializer + reconcile.Loop over an enforcing fake client, with a
// movable clock and a recording reporter.
type pwFix struct {
	t       *testing.T
	cfg     RenderConfig
	ns      string
	now     time.Time
	m       *Materializer
	client  *fake.Clientset
	logs    *strings.Builder
	rep     *strandReporter
	loop    *reconcile.Loop
	desired []protocol.DesiredWorker
}

func newPWFix(t *testing.T, cfg RenderConfig, ns string, desired []protocol.DesiredWorker, objs ...runtime.Object) *pwFix {
	t.Helper()
	f := &pwFix{t: t, cfg: cfg, ns: ns, now: m5Now, logs: &strings.Builder{}, rep: &strandReporter{}, desired: desired}
	f.client = enforcingClient(t, objs...)
	f.m = New(f.client, cfg, testResolver(t), &fakeCordoner{}, DrainPolicy{Deadline: time.Hour}, RecyclePolicy{Enabled: true, Cooldown: time.Hour}, slog.New(slog.NewTextHandler(f.logs, nil)))
	f.m.now = func() time.Time { return f.now }
	f.loop = reconcile.New(pwPoller{f}, f.m, f.rep, 10*time.Second, "1.0.0", slog.New(slog.NewTextHandler(io.Discard, nil)))
	return f
}

func (f *pwFix) tick() {
	f.t.Helper()
	if err := f.loop.Tick(context.Background()); err != nil {
		f.t.Fatalf("Tick: %v", err)
	}
}

// advance moves the clock and refreshes every desired worker's API evidence so a hold
// observed afterwards is the pacing hold, not stale-evidence noise.
func (f *pwFix) advance(d time.Duration) {
	f.now = f.now.Add(d)
	at := f.now
	for i := range f.desired {
		e := f.desired[i].RollEvidence
		until := at.Add(time.Minute)
		e.EvaluatedAt, e.ValidUntil, e.LivenessValidUntil = at, until, &until
		if e.HeartbeatAt != nil {
			e.HeartbeatAt = &at
		}
	}
}

func (f *pwFix) row(id string) *protocol.WorkerStatus {
	if len(f.rep.got) == 0 {
		return nil
	}
	rows := f.rep.got[len(f.rep.got)-1].Workers
	for i := range rows {
		if rows[i].ID == id {
			return &rows[i]
		}
	}
	return nil
}

func (f *pwFix) replicas(id string) int32 {
	f.t.Helper()
	d := npDep(f.t, f.client, f.ns, id)
	if d.Spec.Replicas == nil {
		f.t.Fatalf("%s has nil replicas", id)
	}
	return *d.Spec.Replicas
}

// assertNotStranded fails when the worker reads as stranded in the report, the logs, or
// the retained pacing reason.
func (f *pwFix) assertNotStranded(id string) {
	f.t.Helper()
	if strings.Contains(f.logs.String(), "pvc-bind-timeout") {
		f.t.Errorf("pvc-bind-timeout logged for a deferred claim:\n%s", f.logs)
	}
	row := f.row(id)
	if row == nil {
		return
	}
	if row.Phase == protocol.PhaseStuck {
		f.t.Errorf("row phase = stuck, reason %v", row.BlockingReason)
	}
	if row.BlockingReason != nil && strings.Contains(*row.BlockingReason, "bind-timeout") {
		f.t.Errorf("row reports bind-timeout: %q", *row.BlockingReason)
	}
	if row.Pacing != nil && row.Pacing.BlockingReason != nil && strings.Contains(*row.Pacing.BlockingReason, "bind-timeout") {
		f.t.Errorf("pacing reason reports bind-timeout: %q", *row.Pacing.BlockingReason)
	}
}

// assertNoClaimOrPodLifecycle fails on any create/delete of a PVC or pod.
func (f *pwFix) assertNoClaimOrPodLifecycle() {
	f.t.Helper()
	for _, a := range f.client.Actions() {
		r := a.GetResource().Resource
		if (r == "persistentvolumeclaims" || r == "pods") && (a.GetVerb() == "create" || a.GetVerb() == "delete") {
			f.t.Errorf("unexpected %s %s", a.GetVerb(), r)
		}
	}
}

// pwTarget builds the held worker's objects: a valid incomplete reservation on a
// zero-replica Deployment whose retained template predates the reserved target (so its
// restore is a paced roll step), and Pending WaitForFirstConsumer claims created `age` ago.
func pwTarget(t *testing.T, cfg RenderConfig, w protocol.DesiredWorker, age time.Duration) (*appsv1.Deployment, []*corev1.PersistentVolumeClaim) {
	t.Helper()
	dep := markedDeployment(t, cfg, w, false)
	zero := int32(0)
	dep.Spec.Replicas = &zero
	dep.Spec.Template.Annotations[AnnotationSpecHash] = "retained-old-template"
	var pvcs []*corev1.PersistentVolumeClaim
	for _, o := range livePVCs(t, cfg, w, false) {
		p := o.(*corev1.PersistentVolumeClaim)
		p.Status.Phase = corev1.ClaimPending
		p.CreationTimestamp = metav1.NewTime(m5Now.Add(-age))
		p.Spec.StorageClassName = ptr(pwWFFC)
		pvcs = append(pvcs, p)
	}
	return dep, pvcs
}

type pwVariant struct {
	name   string
	cfg    RenderConfig
	worker protocol.DesiredWorker
}

func pwVariants() []pwVariant {
	dockerPlacement := dockerTestConfig()
	dockerPlacement.NodeSelector = map[string]string{"kubernetes.io/arch": "amd64"}
	dockerPlacement.Tolerations = uziTaint
	dockerPlacement.NewPlacementNodeSelector = map[string]string{"pool": "new"}
	dockerPlacement.NewPlacementTolerations = newPlacementTaint
	return []pwVariant{
		{"newPlacement-off", testConfig(), desired("w1")},
		{"newPlacement-on", placementConfig(), desired("w1")},
		{"docker", dockerTestConfig(), desiredDocker("w1")},
		{"docker-newPlacement-on", dockerPlacement, desiredDocker("w1")},
	}
}

func pwWorkerNS(cfg RenderConfig, w protocol.DesiredWorker) string {
	if w.Docker {
		return cfg.DockerNamespace
	}
	return cfg.Namespace
}

// pwProof is currentProof for a worker in an arbitrary namespace: a registered, current
// pod, which makes the worker a usable coverage provider.
func pwProof(t *testing.T, c *fake.Clientset, w *protocol.DesiredWorker, ns string, at time.Time) {
	t.Helper()
	d, err := c.AppsV1().Deployments(ns).Get(context.Background(), deploymentName(w.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "rs-" + w.ID, Namespace: ns, UID: types.UID("rs-uid-" + w.ID), OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: d.Name, UID: d.UID, Controller: &yes}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-" + w.ID, Namespace: ns, UID: types.UID("pod-uid-" + w.ID), Labels: objectLabels(w.ID), Annotations: map[string]string{AnnotationSpecHash: d.Spec.Template.Annotations[AnnotationSpecHash]}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: &yes}}}, Spec: d.Spec.Template.Spec, Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: workerContainerName, Image: workerImage(d), State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
	if err := c.Tracker().Create(appsv1.SchemeGroupVersion.WithResource("replicasets"), rs, ns); err != nil {
		t.Fatal(err)
	}
	if err := c.Tracker().Create(corev1.SchemeGroupVersion.WithResource("pods"), pod, ns); err != nil {
		t.Fatal(err)
	}
	w.RollEvidence = onlineEvidence(at)
	version, nonce, uid := "1.0.0", "new-"+w.ID, string(pod.UID)
	w.RollEvidence.RegisteredVersion, w.RollEvidence.RegisterNonce, w.RollEvidence.PodUID, w.RollEvidence.HeartbeatAt = &version, &nonce, &uid, &at
}

// pwHeldFleet builds a held restore: the only other coverage provider (w2) is current but
// manually cordoned, which is a separate, legitimate pacing hold on the target's restore.
func pwHeldFleet(t *testing.T, v pwVariant, age time.Duration) (*pwFix, protocol.DesiredWorker, protocol.DesiredWorker) {
	t.Helper()
	target := evidence(v.worker, m5Now)
	manualWorker(&target)
	dep, pvcs := pwTarget(t, v.cfg, target, age)
	objs := []runtime.Object{dep, pwClass(pwWFFC, pwMode(storagev1.VolumeBindingWaitForFirstConsumer))}
	for _, p := range pvcs {
		objs = append(objs, p)
	}
	peerBase := desired("w2")
	if target.Docker {
		peerBase = desiredDocker("w2")
	}
	peer := evidence(peerBase, m5Now)
	manualWorker(&peer)
	objs = append(objs, liveDeployment(t, v.cfg, peer, false))
	objs = append(objs, livePVCs(t, v.cfg, peer, false)...)
	ns := pwWorkerNS(v.cfg, target)
	f := newPWFix(t, v.cfg, ns, []protocol.DesiredWorker{target, peer}, objs...)
	pwProof(t, f.client, &peer, ns, m5Now)
	f.desired[1] = peer
	return f, target, peer
}

func TestPausedPacedRestoreWFFCDoesNotStrand(t *testing.T) {
	for _, v := range pwVariants() {
		t.Run(v.name, func(t *testing.T) {
			f, _, _ := pwHeldFleet(t, v, 15*time.Minute)
			var uids map[string]string
			for i := 0; i < 4; i++ {
				f.tick()
				if i == 0 {
					uids = pwClaimUIDs(t, f)
				}
			}
			if f.replicas("w1") != 0 {
				t.Fatalf("held worker replicas = %d, want 0", f.replicas("w1"))
			}
			f.assertNotStranded("w1")
			row := f.row("w1")
			if row == nil || row.Pacing == nil || row.Pacing.BlockingReason == nil || *row.Pacing.BlockingReason == "" {
				t.Fatalf("legitimate pacing hold reason not retained: %+v", row)
			}
			if got := pwClaimUIDs(t, f); len(got) != len(uids) || len(got) < 2 {
				t.Fatalf("claims changed: %v -> %v", uids, got)
			}
			if v.worker.Docker {
				if _, ok := uids[dindDataPVCName("w1")]; !ok {
					t.Fatalf("docker variant lacks the dind-data claim: %v", uids)
				}
			}
			f.assertNoClaimOrPodLifecycle()
		})
	}
}

func pwClaimUIDs(t *testing.T, f *pwFix) map[string]string {
	t.Helper()
	list, err := f.client.CoreV1().PersistentVolumeClaims(f.ns).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, p := range list.Items {
		if _, ours := IsOurs(p.Labels); ours && strings.HasPrefix(p.Name, "uzi-hw-w1-") {
			out[p.Name] = string(p.UID) + "@" + p.CreationTimestamp.String()
		}
	}
	return out
}

func TestPausedPacedRestoreResumesAfterAdmission(t *testing.T) {
	for _, v := range pwVariants() {
		t.Run(v.name, func(t *testing.T) {
			f, _, peer := pwHeldFleet(t, v, time.Minute)
			created := m5Now.Add(-time.Minute)
			f.tick()
			f.tick()
			before := pwClaimUIDs(t, f)
			f.assertNotStranded("w1")

			// Held well past the bind-timeout: still no strand.
			f.advance(15 * time.Minute)
			f.tick()
			f.tick()
			f.assertNotStranded("w1")
			if f.replicas("w1") != 0 {
				t.Fatal("restored while the coverage hold was in place")
			}

			// Lift the hold; ordinary admission restores the worker.
			peer.ManualCordonedAt, peer.EffectiveCordoned = nil, nil
			f.desired[1] = peer
			restored := false
			for i := 0; i < 8 && !restored; i++ {
				f.tick()
				f.assertNotStranded("w1")
				restored = f.replicas("w1") == 1
			}
			if !restored {
				t.Fatalf("hold lifted but worker not restored: %+v", f.m.PacingOutcomes()["w1"])
			}
			if after := pwClaimUIDs(t, f); len(after) != len(before) {
				t.Fatalf("claims changed: %v -> %v", before, after)
			} else {
				for k, id := range before {
					if after[k] != id {
						t.Errorf("claim %s identity changed: %s -> %s", k, id, after[k])
					}
				}
			}
			f.assertNoClaimOrPodLifecycle()

			// A real consumer that cannot place: the genuine strand returns, timed from the
			// claim's own creation.
			d := npDep(t, f.client, f.ns, "w1")
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-w1", Namespace: f.ns, UID: "pod-uid-w1", Labels: objectLabels("w1"),
				Annotations:       map[string]string{AnnotationSpecHash: d.Spec.Template.Annotations[AnnotationSpecHash]},
				CreationTimestamp: metav1.NewTime(f.now)}, Spec: d.Spec.Template.Spec, Status: corev1.PodStatus{Phase: corev1.PodPending,
				Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable"}}}}
			pod.Spec.Volumes = []corev1.Volume{
				{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: dataPVCName("w1")}}},
				{Name: "nix", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: nixPVCName("w1")}}},
			}
			if err := f.client.Tracker().Create(corev1.SchemeGroupVersion.WithResource("pods"), pod, f.ns); err != nil {
				t.Fatal(err)
			}
			f.advance(0)
			f.tick()
			row := f.row("w1")
			if row == nil || row.Phase != protocol.PhaseStuck {
				t.Fatalf("a placed-but-unbound consumer must read as stranded: %+v", row)
			}
			if row.PhaseSince == nil || !row.PhaseSince.Equal(created) {
				t.Errorf("PhaseSince = %v, want the claim's creation %v", row.PhaseSince, created)
			}
			if !strings.Contains(f.logs.String(), "pvc-bind-timeout worker=w1") {
				t.Errorf("missing pvc-bind-timeout token:\n%s", f.logs)
			}

			// Binding clears it.
			bind := []string{dataPVCName("w1"), nixPVCName("w1")}
			if v.worker.Docker {
				bind = append(bind, dindDataPVCName("w1"))
			}
			for _, name := range bind {
				p := npPVC(t, f.client, f.ns, name)
				p.Status.Phase = corev1.ClaimBound
				if _, err := f.client.CoreV1().PersistentVolumeClaims(f.ns).UpdateStatus(context.Background(), p, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			f.rep.got = nil
			f.logs.Reset()
			f.tick()
			if row := f.row("w1"); row != nil && row.Phase == protocol.PhaseStuck {
				t.Errorf("bound claims still read as stuck: %+v", row)
			}
			if strings.Contains(f.logs.String(), "pvc-bind-timeout") {
				t.Errorf("bind-timeout logged after binding:\n%s", f.logs)
			}
		})
	}
}

// pwEnv is a single Observe-only scenario: the deferral's inputs, each mutable by a row.
type pwEnv struct {
	cfg       RenderConfig
	w         protocol.DesiredWorker
	ns        string
	dep       *appsv1.Deployment
	pvcs      []*corev1.PersistentVolumeClaim
	classes   map[string]*storagev1.StorageClass
	objs      []runtime.Object // extra seeded objects
	late      []*corev1.Pod    // pods created after seeding, keeping an empty UID
	desired   []protocol.DesiredWorker
	noDesired bool
	reactors  []func(*fake.Clientset)
}

func (e *pwEnv) claim(name string) *corev1.PersistentVolumeClaim {
	for _, p := range e.pvcs {
		if p.Name == name {
			return p
		}
	}
	return nil
}

func pwPod(name, ns string, labels map[string]string, claims ...string) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels}}
	for _, c := range claims {
		p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: c, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: c}}})
	}
	return p
}

// pwReact prepends a reactor for a verb/resource.
func pwReact(verb, resource string, fn func(a k8stesting.Action) (bool, runtime.Object, error)) func(*fake.Clientset) {
	return func(c *fake.Clientset) { c.PrependReactor(verb, resource, fn) }
}

func pwFail(verb, resource string, err error) func(*fake.Clientset) {
	return pwReact(verb, resource, func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, err })
}

// pwContinue makes every list of the resource report a continuation token.
func pwContinue(gvr schema.GroupVersionResource, gvk schema.GroupVersionKind) func(*fake.Clientset) {
	return func(c *fake.Clientset) {
		c.PrependReactor("list", gvr.Resource, func(a k8stesting.Action) (bool, runtime.Object, error) {
			obj, err := c.Tracker().List(gvr, gvk, a.GetNamespace())
			if err != nil {
				return true, nil, err
			}
			acc, err := meta.ListAccessor(obj)
			if err != nil {
				return true, nil, err
			}
			acc.SetContinue("more")
			return true, obj, nil
		})
	}
}

func pwSetMarker(t *testing.T, e *pwEnv, edit func(*rollMarker)) {
	t.Helper()
	r, err := decodeRoll(e.dep.Annotations[annotationRoll])
	if err != nil || r == nil {
		t.Fatalf("decode fixture marker: %v", err)
	}
	edit(r)
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	e.dep.Annotations[annotationRoll] = string(raw)
}

type pwNames struct{ nix, data, dind string }

type pwRow struct {
	name       string
	dockerOnly bool
	mut        func(t *testing.T, e *pwEnv)
	// stuck names the claims that must still time out; nil means all of the worker's claims,
	// and none selects the deferral (positive control).
	stuck func(n pwNames) []string
	none  bool
	// scGets is the exact number of StorageClass reads expected, when set.
	scGets *int
}

func TestPausedPacedRestoreTimeoutExemptionIsNarrow(t *testing.T) {
	onlyData := func(n pwNames) []string { return []string{n.data} }
	rows := []pwRow{
		{name: "positive control: eligible claims are deferred", none: true, scGets: ptr(1)},
		{name: "positive control: valid data-claim mirror still defers", none: true, mut: func(t *testing.T, e *pwEnv) {
			e.claim(dataPVCName("w1")).Annotations = map[string]string{annotationRoll: e.dep.Annotations[annotationRoll]}
		}},
		{name: "storage class Immediate", mut: func(t *testing.T, e *pwEnv) {
			e.classes[pwWFFC] = pwClass(pwWFFC, pwMode(storagev1.VolumeBindingImmediate))
		}},
		{name: "storage class nil binding mode", mut: func(t *testing.T, e *pwEnv) { e.classes[pwWFFC] = pwClass(pwWFFC, nil) }},
		{name: "storage class missing", mut: func(t *testing.T, e *pwEnv) { delete(e.classes, pwWFFC) }},
		{name: "storage class forbidden", mut: func(t *testing.T, e *pwEnv) {
			e.reactors = append(e.reactors, pwFail("get", "storageclasses", apierrors.NewForbidden(schema.GroupResource{Group: "storage.k8s.io", Resource: "storageclasses"}, pwWFFC, errPWBoom)))
		}},
		{name: "storage class get error", mut: func(t *testing.T, e *pwEnv) {
			e.reactors = append(e.reactors, pwFail("get", "storageclasses", errPWBoom))
		}},
		{name: "empty storageClassName", mut: func(t *testing.T, e *pwEnv) {
			for _, p := range e.pvcs {
				p.Spec.StorageClassName = nil
			}
		}, scGets: ptr(0)},
		{name: "mixed WFFC and Immediate times out only the Immediate claim", stuck: onlyData, mut: func(t *testing.T, e *pwEnv) {
			e.claim(dataPVCName("w1")).Spec.StorageClassName = ptr("immediate")
			e.classes["immediate"] = pwClass("immediate", pwMode(storagev1.VolumeBindingImmediate))
		}},
		{name: "worker pod mounting the claims", scGets: ptr(0), mut: func(t *testing.T, e *pwEnv) {
			e.objs = append(e.objs, pwPod("p1", e.ns, objectLabels("w1"), nixPVCName("w1"), dataPVCName("w1")))
		}},
		{name: "worker pod elsewhere without volumes", scGets: ptr(0), mut: func(t *testing.T, e *pwEnv) {
			e.objs = append(e.objs, pwPod("p1", e.ns, objectLabels("w1")))
		}},
		{name: "terminating worker pod", scGets: ptr(0), mut: func(t *testing.T, e *pwEnv) {
			p := pwPod("p1", e.ns, objectLabels("w1"))
			now := metav1.Now()
			p.DeletionTimestamp = &now
			e.objs = append(e.objs, p)
		}},
		{name: "unmanaged pod mounting each claim", mut: func(t *testing.T, e *pwEnv) {
			var names []string
			for _, p := range e.pvcs {
				names = append(names, p.Name)
			}
			e.objs = append(e.objs, pwPod("stray", e.ns, map[string]string{"app": "x"}, names...))
		}, scGets: ptr(0)},
		{name: "unmanaged pod with empty UID mounting the data claim", stuck: onlyData, mut: func(t *testing.T, e *pwEnv) {
			e.late = append(e.late, pwPod("stray", e.ns, nil, dataPVCName("w1")))
		}},
		{name: "pod list error", mut: func(t *testing.T, e *pwEnv) { e.reactors = append(e.reactors, pwFail("list", "pods", errPWBoom)) }, scGets: ptr(0)},
		{name: "pod list continuation", mut: func(t *testing.T, e *pwEnv) {
			e.reactors = append(e.reactors, pwContinue(corev1.SchemeGroupVersion.WithResource("pods"), corev1.SchemeGroupVersion.WithKind("Pod")))
		}, scGets: ptr(0)},
		{name: "deployment list continuation", mut: func(t *testing.T, e *pwEnv) {
			e.reactors = append(e.reactors, pwContinue(appsv1.SchemeGroupVersion.WithResource("deployments"), appsv1.SchemeGroupVersion.WithKind("Deployment")))
		}, scGets: ptr(0)},
		{name: "pvc list continuation", mut: func(t *testing.T, e *pwEnv) {
			e.reactors = append(e.reactors, pwContinue(corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims"), corev1.SchemeGroupVersion.WithKind("PersistentVolumeClaim")))
		}, scGets: ptr(0)},
		{name: "two deployments across namespaces", dockerOnly: true, scGets: ptr(0), mut: func(t *testing.T, e *pwEnv) {
			other := e.dep.DeepCopy()
			other.Namespace, other.UID = e.cfg.Namespace, "other-uid"
			e.objs = append(e.objs, other)
		}},
		{name: "claims in another namespace than the deployment", dockerOnly: true, scGets: ptr(0), mut: func(t *testing.T, e *pwEnv) {
			for _, p := range e.pvcs {
				p.Namespace = e.cfg.Namespace
			}
		}},
		{name: "absent deployment", scGets: ptr(0), mut: func(t *testing.T, e *pwEnv) { e.dep = nil }},
		{name: "terminating deployment", scGets: ptr(0), mut: func(t *testing.T, e *pwEnv) {
			now := metav1.Now()
			e.dep.DeletionTimestamp = &now
		}},
		{name: "nil replicas", scGets: ptr(0), mut: func(t *testing.T, e *pwEnv) { e.dep.Spec.Replicas = nil }},
		{name: "replicas 1", scGets: ptr(0), mut: func(t *testing.T, e *pwEnv) { e.dep.Spec.Replicas = ptr(int32(1)) }},
		{name: "hibernation operation annotation", scGets: ptr(0), mut: func(t *testing.T, e *pwEnv) {
			e.dep.Annotations[annotationHibernationOp] = "op-1"
		}},
		{name: "missing marker", scGets: ptr(0), mut: func(t *testing.T, e *pwEnv) { delete(e.dep.Annotations, annotationRoll) }},
		{name: "malformed marker", scGets: ptr(0), mut: func(t *testing.T, e *pwEnv) { e.dep.Annotations[annotationRoll] = "{not json" }},
		{name: "completed marker", scGets: ptr(0), mut: func(t *testing.T, e *pwEnv) {
			pwSetMarker(t, e, func(r *rollMarker) { r.Completed = true })
		}},
		{name: "marker owner mismatch", scGets: ptr(0), mut: func(t *testing.T, e *pwEnv) {
			pwSetMarker(t, e, func(r *rollMarker) { r.Coverage.Owner = "someone-else" })
		}},
		{name: "marker lane mismatch", scGets: ptr(0), mut: func(t *testing.T, e *pwEnv) {
			pwSetMarker(t, e, func(r *rollMarker) { r.Coverage.Lane = "another-lane" })
		}},
		{name: "worker absent from desired", scGets: ptr(0), mut: func(t *testing.T, e *pwEnv) { e.desired = []protocol.DesiredWorker{} }},
		{name: "no desired set on the context", scGets: ptr(0), mut: func(t *testing.T, e *pwEnv) { e.noDesired = true }},
		{name: "invalid data-claim mirror", scGets: ptr(0), mut: func(t *testing.T, e *pwEnv) {
			e.claim(dataPVCName("w1")).Annotations = map[string]string{annotationRoll: "{not json"}
		}},
		{name: "data-claim mirror owned by someone else", scGets: ptr(0), mut: func(t *testing.T, e *pwEnv) {
			pwSetMarker(t, e, func(r *rollMarker) { r.Coverage.Owner = "someone-else" })
			mirror := e.dep.Annotations[annotationRoll]
			pwSetMarker(t, e, func(r *rollMarker) { r.Coverage.Owner = "owner-A" })
			e.claim(dataPVCName("w1")).Annotations = map[string]string{annotationRoll: mirror}
		}},
		{name: "terminating data claim with a mirror", scGets: ptr(0), stuck: func(n pwNames) []string {
			// The terminating data claim is not a Pending timeout candidate itself; the others still time out.
			out := []string{n.nix}
			if n.dind != "" {
				out = append(out, n.dind)
			}
			return out
		}, mut: func(t *testing.T, e *pwEnv) {
			p := e.claim(dataPVCName("w1"))
			p.Annotations = map[string]string{annotationRoll: e.dep.Annotations[annotationRoll]}
			now := metav1.Now()
			p.DeletionTimestamp = &now
		}},
	}
	for _, v := range pwVariants() {
		for _, row := range rows {
			if row.dockerOnly && !v.worker.Docker {
				continue
			}
			t.Run(v.name+"/"+row.name, func(t *testing.T) { runPWRow(t, v, row) })
		}
	}
}

func runPWRow(t *testing.T, v pwVariant, row pwRow) {
	e := &pwEnv{cfg: v.cfg, w: evidence(v.worker, m5Now), classes: map[string]*storagev1.StorageClass{
		pwWFFC: pwClass(pwWFFC, pwMode(storagev1.VolumeBindingWaitForFirstConsumer)),
	}}
	e.ns = pwWorkerNS(v.cfg, e.w)
	e.dep, e.pvcs = pwTarget(t, v.cfg, e.w, 15*time.Minute)
	e.desired = []protocol.DesiredWorker{e.w}
	if row.mut != nil {
		row.mut(t, e)
	}
	names := pwNames{nix: nixPVCName("w1"), data: dataPVCName("w1")}
	order := []string{names.nix, names.data}
	if v.worker.Docker {
		names.dind = dindDataPVCName("w1")
		order = append(order, names.dind)
	}
	var objs []runtime.Object
	if e.dep != nil {
		objs = append(objs, e.dep)
	}
	for _, p := range e.pvcs {
		objs = append(objs, p)
	}
	for _, c := range e.classes {
		objs = append(objs, c)
	}
	objs = append(objs, e.objs...)
	client := enforcingClient(t, objs...)
	for _, p := range e.late {
		if err := client.Tracker().Create(corev1.SchemeGroupVersion.WithResource("pods"), p, p.Namespace); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range e.reactors {
		r(client)
	}
	var logs strings.Builder
	m := New(client, v.cfg, testResolver(t), nil, DrainPolicy{}, RecyclePolicy{Enabled: true}, slog.New(slog.NewTextHandler(&logs, nil)))
	m.now = func() time.Time { return m5Now }
	ctx := context.Background()
	if !e.noDesired {
		ctx = reconcile.WithDesiredWorkers(ctx, e.desired)
	}
	observed, err := m.Observe(ctx)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	var o *reconcile.ObservedWorker
	for i := range observed {
		if observed[i].ID == "w1" {
			o = &observed[i]
		}
	}
	if o == nil {
		t.Fatalf("worker not observed: %+v", observed)
	}

	want := map[string]bool{}
	switch {
	case row.none:
	case row.stuck != nil:
		for _, n := range row.stuck(names) {
			want[n] = true
		}
	default:
		for _, p := range e.pvcs {
			want[p.Name] = true
		}
	}
	var first string
	for _, n := range order {
		if want[n] && first == "" {
			first = n
		}
		if got := strings.Contains(logs.String(), "pvc-bind-timeout worker=w1 pvc="+n+" "); got != want[n] {
			t.Errorf("pvc-bind-timeout for %s = %v, want %v\n%s", n, got, want[n], logs.String())
		}
	}
	if first == "" {
		if o.Roll.Phase == protocol.PhaseStuck {
			t.Errorf("worker reads stuck (%q) with every claim deferred", o.Roll.BlockingReason)
		}
		if len(o.TimeoutDeferredClaims) == 0 {
			t.Errorf("no claim deferred in the positive control")
		}
	} else if o.Roll.Phase != protocol.PhaseStuck || o.Roll.BlockingContainer != first || !strings.Contains(o.Roll.BlockingReason, "bind-timeout") {
		t.Errorf("roll = %+v, want stuck naming %s with the bind-timeout reason", o.Roll, first)
	}

	gets := map[string]int{}
	total := 0
	for _, a := range client.Actions() {
		if a.GetResource().Resource != "storageclasses" {
			continue
		}
		if a.GetVerb() != "get" {
			t.Errorf("storageclasses verb %q, only get is allowed", a.GetVerb())
			continue
		}
		gets[a.(k8stesting.GetAction).GetName()]++
		total++
	}
	for class, n := range gets {
		if n > 1 {
			t.Errorf("StorageClass %s read %d times in one Observe", class, n)
		}
	}
	if row.scGets != nil && total != *row.scGets {
		t.Errorf("StorageClass reads = %d (%v), want %d", total, gets, *row.scGets)
	}
}

// pwObserveFault observes a worker holding aged Pending WFFC claims, a labelled worker pod
// carrying a real container fault that mounts every claim, and the given replica count.
func pwObserveFault(t *testing.T, v pwVariant, replicas int32, status corev1.ContainerStatus) (reconcile.ObservedWorker, string) {
	t.Helper()
	w := evidence(v.worker, m5Now)
	ns := pwWorkerNS(v.cfg, w)
	dep, pvcs := pwTarget(t, v.cfg, w, 15*time.Minute)
	dep.Spec.Replicas = &replicas
	var claims []string
	objs := []runtime.Object{dep, pwClass(pwWFFC, pwMode(storagev1.VolumeBindingWaitForFirstConsumer))}
	for _, p := range pvcs {
		claims = append(claims, p.Name)
		objs = append(objs, p)
	}
	pod := pwPod("p1", ns, objectLabels("w1"), claims...)
	pod.Annotations = map[string]string{AnnotationSpecHash: "retained-old-template"}
	pod.Status = corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{status}}
	objs = append(objs, pod)
	var logs strings.Builder
	m := New(enforcingClient(t, objs...), v.cfg, testResolver(t), nil, DrainPolicy{}, RecyclePolicy{Enabled: true}, slog.New(slog.NewTextHandler(&logs, nil)))
	m.now = func() time.Time { return m5Now }
	observed, err := m.Observe(reconcile.WithDesiredWorkers(context.Background(), []protocol.DesiredWorker{w}))
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	for i := range observed {
		if observed[i].ID == "w1" {
			return observed[i], logs.String()
		}
	}
	t.Fatalf("worker not observed: %+v", observed)
	return reconcile.ObservedWorker{}, ""
}

// A worker pod with a real container fault keeps the exemption out of the way: the claims
// still time out, and the fault evidence deriveRollHealth recorded (restart count, last
// exit code, target image) survives flagStrandedPVCs exactly as it does without the paused
// evidence.
func TestPausedPacedRestoreTimeoutExemptionPreservesFaultEvidence(t *testing.T) {
	exit1 := int32(1)
	faults := []struct {
		name         string
		status       corev1.ContainerStatus
		wantRestarts int32
		wantExit     *int32
	}{
		{"CrashLoopBackOff", corev1.ContainerStatus{Name: workerContainerName, RestartCount: 4,
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}}}, 4, &exit1},
		{"ImagePullBackOff", corev1.ContainerStatus{Name: workerContainerName,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}, 0, nil},
	}
	for _, v := range pwVariants() {
		for _, f := range faults {
			t.Run(v.name+"/"+f.name, func(t *testing.T) {
				held, logs := pwObserveFault(t, v, 0, f.status)
				base, _ := pwObserveFault(t, v, 1, f.status)
				if len(held.TimeoutDeferredClaims) != 0 {
					t.Errorf("claims deferred despite a labelled worker pod: %v", held.TimeoutDeferredClaims)
				}
				if held.Roll.Phase != protocol.PhaseStuck || !strings.Contains(held.Roll.BlockingReason, "bind-timeout") {
					t.Errorf("roll = %+v, want stuck with the bind-timeout reason", held.Roll)
				}
				if !strings.Contains(logs, "pvc-bind-timeout worker=w1 pvc="+nixPVCName("w1")+" ") {
					t.Errorf("missing pvc-bind-timeout line:\n%s", logs)
				}
				if held.Roll.RestartCount != f.wantRestarts {
					t.Errorf("RestartCount = %d, want %d", held.Roll.RestartCount, f.wantRestarts)
				}
				if (held.Roll.LastExitCode == nil) != (f.wantExit == nil) || (f.wantExit != nil && *held.Roll.LastExitCode != *f.wantExit) {
					t.Errorf("LastExitCode = %v, want %v", held.Roll.LastExitCode, f.wantExit)
				}
				if held.Roll.TargetImage == "" {
					t.Errorf("TargetImage lost: %+v", held.Roll)
				}
				if !reflect.DeepEqual(held.Roll, base.Roll) {
					t.Errorf("roll differs with paused evidence:\n held %+v\n base %+v", held.Roll, base.Roll)
				}
			})
		}
	}
}

// Deployment and PVC list errors abort Observe as before; a pod list error does not.
func TestPausedRestoreObserveErrorBehaviourUnchanged(t *testing.T) {
	for _, tc := range []struct {
		resource string
		wantErr  bool
	}{{"deployments", true}, {"persistentvolumeclaims", true}, {"pods", false}} {
		t.Run(tc.resource, func(t *testing.T) {
			w := evidence(desired("w1"), m5Now)
			dep, pvcs := pwTarget(t, testConfig(), w, 15*time.Minute)
			objs := []runtime.Object{dep}
			for _, p := range pvcs {
				objs = append(objs, p)
			}
			client := enforcingClient(t, objs...)
			pwFail("list", tc.resource, errPWBoom)(client)
			m := New(client, testConfig(), testResolver(t), nil, DrainPolicy{}, RecyclePolicy{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			m.now = func() time.Time { return m5Now }
			_, err := m.Observe(reconcile.WithDesiredWorkers(context.Background(), []protocol.DesiredWorker{w}))
			if (err != nil) != tc.wantErr {
				t.Fatalf("Observe error = %v, want error=%v", err, tc.wantErr)
			}
		})
	}
}
