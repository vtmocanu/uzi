package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/preset"
	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

func TestRollExactTarget(t *testing.T) {
	ctx := context.Background()
	t.Run("bare pin holds before cordon or mutation", func(t *testing.T) {
		c := &fakeCordoner{}
		m, client := newMatWithDrain(t, c, DrainPolicy{}, deployedWorker("w1", 0, "old"))
		m.cfg.RollTargetVersion = ""
		m.now = func() time.Time { return m5Now }
		w := evidence(desired("w1"), m5Now)
		w.Busy = true
		o, err := m.Observe(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Reconcile(ctx, []protocol.DesiredWorker{w}, o); err != nil {
			t.Fatal(err)
		}
		p := m.PacingOutcomes()[w.ID]
		if p.TargetVersion != "" || p.BlockingReason == nil || len(c.calls) != 0 || len(templatePatches(client)) != 0 {
			t.Fatalf("bare pin admitted: pacing=%+v cordons=%v patches=%v", p, c.calls, templatePatches(client))
		}
	})
	t.Run("configured exact stamp distinguishes build metadata", func(t *testing.T) {
		m, client := newMat(t, deployedWorker("w1", 0, "old"))
		m.cfg.RollTargetVersion = "0.86.0-rc.9+g37e46a3"
		m.now = func() time.Time { return m5Now }
		w := evidence(desired("w1"), m5Now)
		tick := func() {
			t.Helper()
			o, err := m.Observe(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := m.Reconcile(ctx, []protocol.DesiredWorker{w}, o); err != nil {
				t.Fatal(err)
			}
		}
		tick()
		d, err := client.AppsV1().Deployments(m.cfg.Namespace).Get(ctx, deploymentName(w.ID), metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var marker rollMarker
		if err := json.Unmarshal([]byte(d.Annotations[annotationRoll]), &marker); err != nil {
			t.Fatal(err)
		}
		if marker.TargetVersion != "0.86.0-rc.9+g37e46a3" {
			t.Fatalf("lost stamp: %+v", marker)
		}
		currentProof(t, client, &w, m5Now)
		for _, version := range []string{"0.86.0-rc.9", "0.86.0-rc.9+g0000000"} {
			w.RollEvidence.RegisteredVersion = &version
			tick()
			d, err = client.AppsV1().Deployments(m.cfg.Namespace).Get(ctx, deploymentName(w.ID), metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(d.Annotations[annotationRoll]), &marker); err != nil {
				t.Fatal(err)
			}
			if marker.Completed {
				t.Fatalf("promoted wrong stamp %s", version)
			}
		}
		w.RollEvidence.RegisteredVersion = &m.cfg.RollTargetVersion
		tick()
		d, err = client.AppsV1().Deployments(m.cfg.Namespace).Get(ctx, deploymentName(w.ID), metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(d.Annotations[annotationRoll]), &marker); err != nil {
			t.Fatal(err)
		}
		if !marker.Completed {
			t.Fatal("exact stamp not promoted")
		}
	})
}

// Seed only the approved durable metadata through the Kubernetes interface.
func markedDeployment(t *testing.T, cfg RenderConfig, w protocol.DesiredWorker, completed bool) *appsv1.Deployment {
	t.Helper()
	d := RenderDeployment(cfg, w, testSpec(t, w.Template, w.Size))
	d.UID, d.ResourceVersion = "original-deployment", "1"
	r := rollMarker{Version: 1, Hash: d.Spec.Template.Annotations[AnnotationSpecHash], Image: workerImage(d), TargetVersion: cfg.RollTargetVersion, Coverage: rollCoverage{Owner: "owner-A", Lane: cfg.Namespace, Scheduling: []string{}, Protocol: []string{}, Ordinary: true}, Nonce: "prior-registration", PreviousVersion: "0.1.0", Canary: true, Completed: completed}
	if w.Docker {
		r.Coverage.Lane = cfg.DockerNamespace
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	d.Annotations = map[string]string{annotationRoll: string(raw)}
	return d
}

func TestRollLegacyRetainsDeployment(t *testing.T) {
	for _, arm := range []string{"nix", "pressure", "custody"} {
		t.Run(arm, func(t *testing.T) {
			ctx := context.Background()
			w := evidence(desired("w1"), m5Now)
			w.DiskPressure = arm == "pressure"
			w.CustodyHeld = arm == "custody"
			d := markedDeployment(t, testConfig(), w, true)
			nixSize := "4Gi"
			if arm == "pressure" {
				nixSize = "20Gi"
			}
			terminating := metav1.NewTime(m5Now)
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "old-pod", Namespace: testConfig().Namespace, UID: "old-pod", Labels: objectLabels(w.ID), DeletionTimestamp: &terminating}}
			m, client := newMat(t, d, pvcNix(w.ID, nixSize), pvcFor(w.ID, "data"), pod)
			m.now = func() time.Time { return m5Now }
			tick := func() {
				t.Helper()
				o, err := m.Observe(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if err := m.Reconcile(ctx, []protocol.DesiredWorker{w}, o); err != nil {
					t.Fatal(err)
				}
			}
			tick()
			got, err := client.AppsV1().Deployments(m.cfg.Namespace).Get(ctx, d.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if got.Spec.Replicas == nil || *got.Spec.Replicas != 0 || got.Annotations[annotationRoll] != d.Annotations[annotationRoll] {
				t.Fatal("pause lost marker or failed to stop replicas")
			}
			tick()
			if len(deletedSet(client)) != 0 {
				t.Fatal("deleted PVC while terminating pod remained")
			}
			if err := client.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), m.cfg.Namespace, pod.Name); err != nil {
				t.Fatal(err)
			}
			tick()
			deletes := deletedSet(client)
			if deletes["deployments/"+d.Name] {
				t.Fatal("legacy recycle deleted durable Deployment")
			}
			// Check actual objects, independent of deletedSet's key convention.
			_, dataErr := client.CoreV1().PersistentVolumeClaims(m.cfg.Namespace).Get(ctx, dataPVCName(w.ID), metav1.GetOptions{})
			if arm == "pressure" && dataErr == nil {
				t.Fatal("pressure suppressed /data self-heal")
			}
			if arm != "pressure" && dataErr != nil {
				t.Fatal("nix/custody discarded /data")
			}
			// The next observation recreates claims, then a later observation resumes.
			w.DiskPressure = false
			tick()
			tick()
			got, err = client.AppsV1().Deployments(m.cfg.Namespace).Get(ctx, d.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if got.UID != d.UID || got.Spec.Replicas == nil || *got.Spec.Replicas != 1 || got.Annotations[annotationRoll] != d.Annotations[annotationRoll] {
				t.Fatal("historical no-drift recycle failed to resume durable Deployment")
			}
			for _, a := range client.Actions() {
				if a.GetResource().Resource == "persistentvolumeclaims" && a.GetVerb() == "patch" {
					t.Fatal("legacy recycle mirrored onto /data")
				}
			}
		})
	}
}

func TestRollDinDMirrorHandoff(t *testing.T) {
	ctx := context.Background()
	cfg := dockerTestConfig()
	w := evidence(desiredDocker("w1"), m5Now)
	w.DindMaintenance = &protocol.DindMaintenance{ID: "op", Phase: "stopping", Fenced: true, DeploymentUID: "original-deployment", PVCUID: "old-dind"}
	d := markedDeployment(t, cfg, w, true)
	var objects []runtime.Object
	objects = append(objects, d)
	for _, p := range RenderPVCs(cfg, w, testSpec(t, w.Template, w.Size)) {
		p.UID = types.UID(p.Name)
		if p.Name == dindDataPVCName(w.ID) {
			p.UID = "old-dind"
		}
		p.Status.Phase = corev1.ClaimBound
		objects = append(objects, p)
	}
	m, client := newMat(t, objects...)
	m.cfg, m.cordoner = cfg, &maintenanceFake{}
	m.now = func() time.Time { return m5Now }
	tick := func() {
		t.Helper()
		o, err := m.Observe(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Reconcile(ctx, []protocol.DesiredWorker{w}, o); err != nil {
			t.Fatal(err)
		}
	}
	tick()
	claim, err := client.CoreV1().PersistentVolumeClaims(cfg.DockerNamespace).Get(ctx, dataPVCName(w.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if claim.Annotations[annotationRoll] != d.Annotations[annotationRoll] {
		t.Fatal("DinD delete lost completed marker")
	}
	w.DindMaintenance.Phase = "recycling"
	tick() // delete only old DinD PVC
	tick() // recreate DinD PVC and Deployment with mirrored metadata
	replacement, err := client.AppsV1().Deployments(cfg.DockerNamespace).Get(ctx, d.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if replacement.UID == d.UID || replacement.Annotations[annotationRoll] != d.Annotations[annotationRoll] {
		t.Fatal("replacement lacks original completed marker")
	}
	claim, err = client.CoreV1().PersistentVolumeClaims(cfg.DockerNamespace).Get(ctx, dataPVCName(w.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if claim.Annotations[annotationRoll] == "" {
		t.Fatal("mirror cleared before replacement observed")
	}
	client.ClearActions()
	tick()
	for _, a := range client.Actions() {
		if a.GetResource().Resource == "persistentvolumeclaims" && a.GetVerb() == "get" {
			t.Fatal("controller used PVC GET")
		}
	}
	claim, err = client.CoreV1().PersistentVolumeClaims(cfg.DockerNamespace).Get(ctx, dataPVCName(w.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if claim.Annotations[annotationRoll] != "" {
		t.Fatal("observed replacement did not clean mirror")
	}
}

func TestRollDinDMirrorRecovery(t *testing.T) {
	for _, scenario := range []string{"missing", "terminating", "oversized", "pin-change"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			cfg := dockerTestConfig()
			w := evidence(desiredDocker("w1"), m5Now)
			w.DindMaintenance = &protocol.DindMaintenance{Phase: "recycling", Fenced: true, DeploymentUID: "original-deployment", PVCUID: "old-dind"}
			if scenario == "missing" {
				// A visible roll-owned drain distinguishes an admitted image turn
				// from independent maintenance with no image reservation at all.
				at := m5Now.Add(-time.Hour)
				w.DrainingSince, w.DrainingReason = &at, token("image_roll")
			}
			d := markedDeployment(t, cfg, w, true)
			var objects []runtime.Object
			var data *corev1.PersistentVolumeClaim
			for _, p := range RenderPVCs(cfg, w, testSpec(t, w.Template, w.Size)) {
				p.UID, p.ResourceVersion = types.UID(p.Name), "1"
				if p.Name == dataPVCName(w.ID) {
					data = p
					p.Annotations = map[string]string{annotationRoll: d.Annotations[annotationRoll]}
					switch scenario {
					case "missing":
						p.Annotations = nil
					case "terminating":
						at := metav1.NewTime(m5Now)
						p.DeletionTimestamp = &at
					case "oversized":
						p.Annotations[annotationRoll] = strings.Repeat("x", maxRollMarker+1)
					}
				}
				objects = append(objects, p)
			}
			m, client := newMat(t, objects...)
			m.cfg, m.cordoner = cfg, &maintenanceFake{}
			m.now = func() time.Time { return m5Now }
			if scenario == "pin-change" {
				var err error
				m.resolver, err = preset.NewResolver("harbor.example.com/uzi", "v2.0.0")
				if err != nil {
					t.Fatal(err)
				}
				m.cfg.RollTargetVersion = "2.0.0+gabcdef0"
			}
			o, err := m.Observe(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := m.Reconcile(ctx, []protocol.DesiredWorker{w}, o); err != nil {
				t.Fatal(err)
			}
			if scenario != "pin-change" {
				for _, a := range client.Actions() {
					if a.GetVerb() == "create" || a.GetVerb() == "patch" || a.GetVerb() == "delete" {
						t.Fatalf("unsafe %s with %s mirror", a.GetVerb(), scenario)
					}
				}
				if m.PacingOutcomes()[w.ID].BlockingReason == nil {
					t.Fatal("missing recovery explanation")
				}
				return
			}
			created, err := client.AppsV1().Deployments(cfg.DockerNamespace).Get(ctx, d.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			var marker rollMarker
			if err := json.Unmarshal([]byte(created.Annotations[annotationRoll]), &marker); err != nil {
				t.Fatal(err)
			}
			if marker.TargetVersion != "2.0.0+gabcdef0" || marker.Completed || marker.Hash == d.Spec.Template.Annotations[AnnotationSpecHash] {
				t.Fatal("replacement used obsolete pin mirror")
			}
			// Replay the old immutable metadata write with its original UID/RV.
			body, err := json.Marshal(map[string]any{"metadata": map[string]any{"uid": data.UID, "resourceVersion": "1", "annotations": map[string]any{annotationRoll: d.Annotations[annotationRoll]}}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.CoreV1().PersistentVolumeClaims(cfg.DockerNamespace).Patch(ctx, data.Name, types.MergePatchType, body, metav1.PatchOptions{}); err == nil {
				t.Fatal("late old mirror write was accepted")
			}
		})
	}
}

func TestRollDinDMirrorUnknownHolds(t *testing.T) {
	ctx := context.Background()
	cfg := dockerTestConfig()
	w := evidence(desiredDocker("w1"), m5Now)
	w.DindMaintenance = &protocol.DindMaintenance{Phase: "stopping", Fenced: true, DeploymentUID: "original-deployment", PVCUID: "old-dind"}
	d := markedDeployment(t, cfg, w, true)
	objects := []runtime.Object{d}
	for _, p := range RenderPVCs(cfg, w, testSpec(t, w.Template, w.Size)) {
		if p.Name == dindDataPVCName(w.ID) {
			p.UID = "old-dind"
		}
		objects = append(objects, p)
	}
	m, client := newMat(t, objects...)
	m.cfg, m.cordoner = cfg, &maintenanceFake{}
	m.now = func() time.Time { return m5Now }
	client.PrependReactor("patch", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, fmt.Errorf("unknown outcome") })
	o, err := m.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile(ctx, []protocol.DesiredWorker{w}, o); err == nil {
		t.Fatal("unknown mirror outcome swallowed")
	}
	for _, a := range client.Actions() {
		if a.GetVerb() == "delete" {
			t.Fatal("deleted after unknown mirror result")
		}
		if a.GetResource().Resource == "persistentvolumeclaims" && a.GetVerb() == "get" {
			t.Fatal("controller used PVC GET")
		}
	}
}
