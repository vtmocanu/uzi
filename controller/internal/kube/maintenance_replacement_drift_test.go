package kube

import (
	"context"
	"encoding/json"
	"slices"
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
	cfg := dockerTestConfig()
	w := evidence(desiredDocker("w1"), m5Now)
	w.DindMaintenance = &protocol.DindMaintenance{Phase: "recycling", Fenced: true, DeploymentUID: driftOldDeployment, PVCUID: driftOldPVC}
	d := markedDeployment(t, cfg, w, true)
	d.UID = types.UID(deploymentUID)
	objects := []runtime.Object{d}
	for _, p := range RenderPVCs(cfg, w, testSpec(t, w.Template, w.Size)) {
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

// addInFlightPeer adds a second worker at the new pin whose canary reservation is
// still in flight, so no other worker can be admitted for an image turn.
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
	}{
		{name: "no admission", setup: func(h *driftHarness) { h.addInFlightPeer() }},
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
			h := newDriftHarness(t, uid, driftNewPVC)
			h.pinChange()
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

func TestDinDRecyclingReplacementTemplateDrift(t *testing.T) {
	t.Run("image change converges and then completes", func(t *testing.T) {
		h := newDriftHarness(t, driftNewDeployment, driftNewPVC)
		h.pinChange()
		want := h.wantHash()
		if want == h.oldRev {
			t.Fatal("pin change did not change the desired hash")
		}
		for i := 0; i < 6 && h.templateHash() != want; i++ {
			h.tick()
		}
		if got := h.templateHash(); got != want {
			t.Fatalf("replacement template stuck at %s, want %s", got, want)
		}
		if h.completed() {
			t.Fatalf("completed before a ready pod at the new hash: %v", h.f.calls)
		}
		h.tick()
		if h.completed() {
			t.Fatalf("completed without a ready pod: %v", h.f.calls)
		}
		// A ready replacement pod at the new hash with settled roll evidence.
		d := h.deployment()
		ready := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "replacement", Namespace: h.cfg.DockerNamespace, UID: "new-pod", Labels: d.Spec.Template.Labels, Annotations: d.Spec.Template.Annotations}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
		stampMaintenancePodOwner(ready, d.Name)
		if _, err := h.client.AppsV1().ReplicaSets(h.cfg.DockerNamespace).Create(context.Background(), maintenanceReplicaSet(ready, d), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := h.client.CoreV1().Pods(h.cfg.DockerNamespace).Create(context.Background(), ready, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		version, nonce, uid, at := driftPinVersion, "new-registration", "new-pod", m5Now
		h.w.RollEvidence.RegisteredVersion, h.w.RollEvidence.RegisterNonce, h.w.RollEvidence.PodUID, h.w.RollEvidence.HeartbeatAt = &version, &nonce, &uid, &at
		h.f.calls = nil
		for i := 0; i < 4 && !h.completed(); i++ {
			h.w.DindMaintenance = h.f.op
			if h.w.DindMaintenance == nil {
				h.w.DindMaintenance = &protocol.DindMaintenance{Phase: "recycling", Fenced: true, DeploymentUID: driftOldDeployment, PVCUID: driftOldPVC}
			}
			h.tick()
		}
		if !slices.Equal(h.f.calls, []string{"report", "complete"}) {
			t.Fatalf("calls = %v, want report then complete", h.f.calls)
		}
	})
}
