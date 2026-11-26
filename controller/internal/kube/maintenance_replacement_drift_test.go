package kube

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vtmocanu/uzi/controller/internal/preset"
	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

const (
	driftPinVersion    = "2.0.0+gabcdef0"
	driftOldDeployment = "original-deployment"
	driftOldPVC        = "old-dind"
	driftNewDeployment = "replacement-deployment"
	driftNewPVC        = "replacement-dind"
)

type driftHarness struct {
	t      *testing.T
	cfg    RenderConfig
	w      protocol.DesiredWorker
	m      *Materializer
	client *fake.Clientset
	f      *maintenanceFake
	oldRev string
	peers  []protocol.DesiredWorker
}

// newDriftHarness seeds a recycling operation whose replacement Deployment (at
// the old image pin) and bound replacement DinD claim already exist.
// deploymentUID is the observed Deployment UID and pvcUID the DinD claim UID.
func newDriftHarness(t *testing.T, deploymentUID, pvcUID string) *driftHarness {
	t.Helper()
	return newDriftHarnessWith(t, deploymentUID, pvcUID, false)
}

// newStampedDriftHarness seeds the same recycling operation for a worker moved to
// the opt-in new placement: newPlacement is configured, every claim carries the
// create-time stamp and the replacement Deployment renders the new placement.
func newStampedDriftHarness(t *testing.T, deploymentUID, pvcUID string) *driftHarness {
	t.Helper()
	h := newDriftHarnessWith(t, deploymentUID, pvcUID, true)
	o, err := h.m.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(o) != 1 || !newPlacementStamped(h.m.cfg, h.w, o[0], h.m.cfg.Namespace) {
		t.Fatalf("seeded worker is not observed as stamped: %+v", o)
	}
	spec, err := h.m.resolver.Resolve(h.w.Template, h.w.Size)
	if err != nil {
		t.Fatal(err)
	}
	if got := RenderDeployment(h.m.cfg.withPlacement(true), h.w, spec).Spec.Template.Annotations[AnnotationSpecHash]; got != h.oldRev {
		t.Fatalf("stamped render %s differs from the seeded template %s", got, h.oldRev)
	}
	if podPlacement(h.deployment()) != "new" {
		t.Fatalf("seeded replacement placement = %s, want new", podPlacement(h.deployment()))
	}
	return h
}

func newDriftHarnessWith(t *testing.T, deploymentUID, pvcUID string, stamped bool) *driftHarness {
	t.Helper()
	cfg := dockerTestConfig()
	if stamped {
		p := placementConfig()
		cfg.NodeSelector, cfg.Tolerations = p.NodeSelector, p.Tolerations
		cfg.NewPlacementNodeSelector, cfg.NewPlacementTolerations = p.NewPlacementNodeSelector, p.NewPlacementTolerations
	}
	w := evidence(desiredDocker("w1"), m5Now)
	w.DindMaintenance = &protocol.DindMaintenance{Phase: "recycling", Fenced: true, DeploymentUID: driftOldDeployment, PVCUID: driftOldPVC}
	d := markedDeployment(t, cfg.withPlacement(stamped), w, true)
	d.UID = types.UID(deploymentUID)
	objects := []runtime.Object{d}
	for _, obj := range livePVCs(t, cfg, w, stamped) {
		p := obj.(*corev1.PersistentVolumeClaim)
		p.UID, p.ResourceVersion = types.UID(p.Name), "1"
		p.Status.Phase = corev1.ClaimBound
		if p.Name == dindDataPVCName(w.ID) {
			p.UID = types.UID(pvcUID)
		}
		objects = append(objects, p)
	}
	m, client := newMat(t, objects...)
	f := &maintenanceFake{}
	m.cfg, m.cordoner = cfg, f
	m.now = func() time.Time { return m5Now }
	return &driftHarness{t: t, cfg: cfg, w: w, m: m, client: client, f: f, oldRev: d.Spec.Template.Annotations[AnnotationSpecHash]}
}

// pinChange moves the resolver and authoritative roll target to a newer image.
func (h *driftHarness) pinChange() {
	h.t.Helper()
	var err error
	h.m.resolver, err = preset.NewResolver("harbor.example.com/uzi", "v2.0.0")
	if err != nil {
		h.t.Fatal(err)
	}
	h.m.cfg.RollTargetVersion = driftPinVersion
}

// clearPlacement clears workers.newPlacement: the stamped worker's desired
// template returns to the global placement while its stamps stay.
func (h *driftHarness) clearPlacement() {
	h.t.Helper()
	h.m.cfg = globalOnly(h.m.cfg)
}

// addInFlightPeer adds a second worker at the current desired render whose canary
// reservation is still in flight, so no other worker can be admitted for a roll turn.
func (h *driftHarness) addInFlightPeer() {
	h.t.Helper()
	peer := evidence(desiredDocker("w0"), m5Now)
	spec, err := h.m.resolver.Resolve(peer.Template, peer.Size)
	if err != nil {
		h.t.Fatal(err)
	}
	d := RenderDeployment(h.m.cfg, peer, spec)
	d.UID, d.ResourceVersion = "peer-deployment", "1"
	r := rollMarker{Version: 1, Hash: d.Spec.Template.Annotations[AnnotationSpecHash], Image: workerImage(d), TargetVersion: h.m.cfg.RollTargetVersion, Coverage: rollCoverage{Owner: "owner-A", Lane: h.cfg.DockerNamespace, Scheduling: []string{}, Protocol: []string{}, Ordinary: true}, Nonce: "prior-registration", PreviousVersion: "0.1.0", Canary: true}
	raw, err := json.Marshal(r)
	if err != nil {
		h.t.Fatal(err)
	}
	d.Annotations = map[string]string{annotationRoll: string(raw)}
	ctx := context.Background()
	if _, err := h.client.AppsV1().Deployments(h.cfg.DockerNamespace).Create(ctx, d, metav1.CreateOptions{}); err != nil {
		h.t.Fatal(err)
	}
	for _, p := range RenderPVCs(h.m.cfg, peer, spec) {
		p.UID, p.ResourceVersion, p.Status.Phase = types.UID(p.Name), "1", corev1.ClaimBound
		if _, err := h.client.CoreV1().PersistentVolumeClaims(h.cfg.DockerNamespace).Create(ctx, p, metav1.CreateOptions{}); err != nil {
			h.t.Fatal(err)
		}
	}
	h.peers = append(h.peers, peer)
}

func (h *driftHarness) wantHash() string {
	h.t.Helper()
	spec, err := h.m.resolver.Resolve(h.w.Template, h.w.Size)
	if err != nil {
		h.t.Fatal(err)
	}
	return RenderDeployment(h.m.cfg, h.w, spec).Spec.Template.Annotations[AnnotationSpecHash]
}

func (h *driftHarness) tick() {
	h.t.Helper()
	o, err := h.m.Observe(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.m.Reconcile(context.Background(), append([]protocol.DesiredWorker{h.w}, h.peers...), o); err != nil {
		h.t.Fatal(err)
	}
}

func (h *driftHarness) deployment() *appsv1.Deployment {
	h.t.Helper()
	d, err := h.client.AppsV1().Deployments(h.cfg.DockerNamespace).Get(context.Background(), deploymentName(h.w.ID), metav1.GetOptions{})
	if err != nil {
		h.t.Fatal(err)
	}
	return d
}

func (h *driftHarness) templateHash() string {
	return h.deployment().Spec.Template.Annotations[AnnotationSpecHash]
}

func (h *driftHarness) completed() bool { return slices.Contains(h.f.calls, "complete") }

// A drifted replacement template must stay untouched, and the operation must not
// complete, whenever the fenced path has not authorized replacement work.
func TestDinDRecyclingReplacementTemplateDriftHeld(t *testing.T) {
	cases := []struct {
		name  string
		setup func(h *driftHarness)
		// deploymentUID is the observed Deployment UID; empty means the replacement.
		deploymentUID string
		// placement seeds a stamped worker and clears newPlacement instead of
		// changing the image pin.
		placement bool
	}{
		{name: "no admission", setup: func(h *driftHarness) { h.addInFlightPeer() }},
		{name: "cleared newPlacement, no admission", placement: true, setup: func(h *driftHarness) { h.addInFlightPeer() }},
		{name: "busy", setup: func(h *driftHarness) { h.w.Busy = true }},
		{name: "busy with ForceRoll", setup: func(h *driftHarness) { h.w.Busy, h.m.drain.ForceRoll = true, true }},
		{name: "custody held", setup: func(h *driftHarness) { h.w.CustodyHeld = true }},
		{name: "custody held with ForceRoll", setup: func(h *driftHarness) { h.w.CustodyHeld, h.m.drain.ForceRoll = true, true }},
		{name: "original daemon still present", deploymentUID: driftOldDeployment, setup: func(h *driftHarness) { h.m.drain.ForceRoll = true }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			uid := c.deploymentUID
			if uid == "" {
				uid = driftNewDeployment
			}
			var h *driftHarness
			if c.placement {
				h = newStampedDriftHarness(t, uid, driftNewPVC)
				h.clearPlacement()
			} else {
				h = newDriftHarness(t, uid, driftNewPVC)
				h.pinChange()
			}
			if h.wantHash() == h.oldRev {
				t.Fatal("trigger did not change the desired hash")
			}
			c.setup(h)
			for i := 0; i < 6; i++ {
				h.tick()
			}
			if got := h.templateHash(); got != h.oldRev {
				t.Fatalf("drifted template patched to %s while held", got)
			}
			if h.completed() {
				t.Fatalf("completed while held: %v", h.f.calls)
			}
		})
	}
}

// addFlappingReadyPod creates a ready replacement pod carrying the Deployment's
// current template, with a container restarted past the stuck threshold and
// started startedAgo before the harness clock. Within the flap window the roll
// health is not settled even though the pod is Ready.
func (h *driftHarness) addFlappingReadyPod(startedAgo time.Duration) {
	h.t.Helper()
	d := h.deployment()
	started := metav1.NewTime(m5Now.Add(-startedAgo))
	ready := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "replacement", Namespace: h.cfg.DockerNamespace, UID: "new-pod", Labels: d.Spec.Template.Labels, Annotations: d.Spec.Template.Annotations}, Status: corev1.PodStatus{
		Phase:             corev1.PodRunning,
		Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		ContainerStatuses: []corev1.ContainerStatus{{Name: "worker", RestartCount: stuckRestartThreshold, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: started}}}},
	}}
	stampMaintenancePodOwner(ready, d.Name)
	if _, err := h.client.AppsV1().ReplicaSets(h.cfg.DockerNamespace).Create(context.Background(), maintenanceReplicaSet(ready, d), metav1.CreateOptions{}); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.client.CoreV1().Pods(h.cfg.DockerNamespace).Create(context.Background(), ready, metav1.CreateOptions{}); err != nil {
		h.t.Fatal(err)
	}
	// Register at the roll's actual target: an image turn targets the new pin, a
	// placement-only turn keeps the current one.
	var marker rollMarker
	if err := json.Unmarshal([]byte(d.Annotations[annotationRoll]), &marker); err != nil {
		h.t.Fatalf("replacement roll marker: %v", err)
	}
	version := h.m.cfg.RollTargetVersion
	if version == "" || marker.TargetVersion != version || marker.Completed {
		h.t.Fatalf("replacement reservation target %q (completed %v), want an open reservation for %q", marker.TargetVersion, marker.Completed, version)
	}
	nonce, uid, at := "new-registration", "new-pod", m5Now
	h.w.RollEvidence.RegisteredVersion, h.w.RollEvidence.RegisterNonce, h.w.RollEvidence.PodUID, h.w.RollEvidence.HeartbeatAt = &version, &nonce, &uid, &at
}

// settlePod ages the container past the flap window so roll health reads settled.
func (h *driftHarness) settlePod() {
	h.t.Helper()
	p, err := h.client.CoreV1().Pods(h.cfg.DockerNamespace).Get(context.Background(), "replacement", metav1.GetOptions{})
	if err != nil {
		h.t.Fatal(err)
	}
	p.Status.ContainerStatuses[0].State.Running.StartedAt = metav1.NewTime(m5Now.Add(-2 * flapWindow))
	if _, err := h.client.CoreV1().Pods(h.cfg.DockerNamespace).Update(context.Background(), p, metav1.UpdateOptions{}); err != nil {
		h.t.Fatal(err)
	}
}

// tickOp ticks with the operation the fake last recorded, as the poller would resend it.
func (h *driftHarness) tickOp() {
	h.t.Helper()
	h.w.DindMaintenance = h.f.op
	if h.w.DindMaintenance == nil {
		h.w.DindMaintenance = &protocol.DindMaintenance{Phase: "recycling", Fenced: true, DeploymentUID: driftOldDeployment, PVCUID: driftOldPVC}
	}
	h.tick()
}

// reconcileDinDDirect runs the maintenance state machine on a fresh observation
// of the harness's current pods, bypassing the roll-admission gate in Reconcile.
func (h *driftHarness) reconcileDinDDirect() {
	h.t.Helper()
	o, err := h.m.Observe(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	spec, err := h.m.resolver.Resolve(h.w.Template, h.w.Size)
	if err != nil {
		h.t.Fatal(err)
	}
	h.w.DindMaintenance = h.f.op
	if h.w.DindMaintenance == nil {
		h.w.DindMaintenance = &protocol.DindMaintenance{Phase: "recycling", Fenced: true, DeploymentUID: driftOldDeployment, PVCUID: driftOldPVC}
	}
	if _, err := h.m.reconcileDinD(context.Background(), h.w, o[0], h.cfg.DockerNamespace, spec); err != nil {
		h.t.Fatal(err)
	}
}

// convergeThenSettle drives the drifted replacement to want, then proves that a
// Ready pod at the new template with unsettled roll health does not complete the
// operation, and that the same pod does once settled.
func (h *driftHarness) convergeThenSettle(want func() bool) {
	h.t.Helper()
	for i := 0; i < 6 && !want(); i++ {
		h.tick()
	}
	if !want() {
		h.t.Fatal("replacement template did not converge")
	}
	if h.completed() {
		h.t.Fatalf("completed before a ready pod at the new template: %v", h.f.calls)
	}
	h.tick()
	if h.completed() {
		h.t.Fatalf("completed without a ready pod: %v", h.f.calls)
	}
	h.addFlappingReadyPod(time.Minute)
	h.f.calls = nil
	for i := 0; i < 4; i++ {
		h.tickOp()
	}
	// Reconcile holds a stuck worker before the maintenance state machine runs, so
	// the settled-pod completion guard is exercised through reconcileDinD itself.
	h.reconcileDinDDirect()
	if len(h.f.calls) != 0 {
		h.t.Fatalf("reported or completed on a ready but unsettled pod: %v", h.f.calls)
	}
	h.settlePod()
	for i := 0; i < 4 && !h.completed(); i++ {
		h.tickOp()
	}
	if !slices.Equal(h.f.calls, []string{"report", "complete"}) {
		h.t.Fatalf("calls = %v, want report then complete", h.f.calls)
	}
}

func TestDinDRecyclingReplacementTemplateDrift(t *testing.T) {
	t.Run("image change converges and then completes", func(t *testing.T) {
		h := newDriftHarness(t, driftNewDeployment, driftNewPVC)
		h.pinChange()
		want := h.wantHash()
		if want == h.oldRev {
			t.Fatal("pin change did not change the desired hash")
		}
		h.convergeThenSettle(func() bool { return h.templateHash() == want })
	})

	t.Run("cleared newPlacement converges to the global placement and then completes", func(t *testing.T) {
		h := newStampedDriftHarness(t, driftNewDeployment, driftNewPVC)
		h.clearPlacement()
		want := h.wantHash()
		if want == h.oldRev {
			t.Fatal("clearing newPlacement did not change the desired hash")
		}
		h.convergeThenSettle(func() bool { return h.templateHash() == want })
		if got := podPlacement(h.deployment()); got != "global" {
			t.Fatalf("converged placement = %s, want global", got)
		}
		for _, n := range []string{dataPVCName(h.w.ID), nixPVCName(h.w.ID), dindDataPVCName(h.w.ID)} {
			if !isStamped(npPVC(t, h.client, h.cfg.DockerNamespace, n)) {
				t.Fatalf("stamp on %s was removed", n)
			}
		}
	})

	t.Run("generation-only drift converges and then completes", func(t *testing.T) {
		h := newDriftHarness(t, driftNewDeployment, driftNewPVC)
		h.w.Generation++
		wantGen := strconv.FormatInt(h.w.Generation, 10)
		if h.wantHash() != h.oldRev {
			t.Fatal("generation bump unexpectedly changed the spec hash")
		}
		if h.deployment().Spec.Template.Annotations[AnnotationGeneration] == wantGen {
			t.Fatal("seed deployment already at the new generation")
		}
		h.convergeThenSettle(func() bool {
			return h.deployment().Spec.Template.Annotations[AnnotationGeneration] == wantGen
		})
	})
}
