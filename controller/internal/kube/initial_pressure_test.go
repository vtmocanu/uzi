package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/controller/internal/preset"
	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

func TestInitialProvisionSnapshotMatrix(t *testing.T) {
	for _, mode := range []string{"fresh", "partial", "replacement", "valid-reservation", "malformed-reservation", "legacy-nononce", "registered", "nil-evidence", "observed-deployment", "orphan-pod", "snapshot-failure"} {
		t.Run(mode, func(t *testing.T) {
			w := evidence(desired("w1"), m5Now)
			yes := true
			w.RollEvidence.Registered = false
			w.RollEvidence.NeverRegistered = &yes
			w.RollEvidence.RegisterNonce = nil
			w.RollEvidence.RegisteredVersion = nil
			w.JoinToken = token("fixture-join")
			var objects []runtime.Object
			cfg := testConfig()
			cfg.RollTargetVersion = ""
			spec := testSpec(t, w.Template, w.Size)
			switch mode {
			case "partial", "valid-reservation", "malformed-reservation":
				pvc := RenderPVCs(cfg, w, spec)[0]
				if mode != "partial" {
					pvc.Annotations = map[string]string{annotationRoll: "{malformed"}
				}
				if mode == "valid-reservation" {
					pvc.Annotations[annotationRoll] = `{"version":1,"hash":"old","image":"worker:old","target_version":"0.1.0","coverage":{"owner":"owner-A","lane":"uzi-workers","scheduling":[],"protocol":[],"ordinary":true,"checker":false},"nonce":"old","previous_version":"0.1.0","canary":true}`
				}
				objects = append(objects, pvc)
			case "replacement":
				objects = append(objects, RenderDeployment(testConfig(), w, spec))
			case "legacy-nononce":
				w.RollEvidence.NeverRegistered = nil
				cfg.RollTargetVersion = "1.0.0"
			case "registered":
				w.RollEvidence.Registered = true
				cfg.RollTargetVersion = "1.0.0"
			case "nil-evidence":
				w.RollEvidence = nil
				cfg.RollTargetVersion = "1.0.0"
			case "orphan-pod":
				objects = append(objects, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "orphan", Namespace: cfg.Namespace, Labels: objectLabels(w.ID)}})
			}
			m, c := newMat(t, objects...)
			m.cfg = cfg
			m.now = func() time.Time { return m5Now }
			obs, err := m.Observe(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if mode == "observed-deployment" {
				obs = append(obs, reconcile.ObservedWorker{ID: w.ID, HasDeployment: true})
			}
			if mode == "snapshot-failure" {
				c.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, fmt.Errorf("snapshot unavailable")
				})
			}
			c.ClearActions()
			m = matrixRestart(m)
			_ = m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, obs)
			positive := mode == "fresh" || mode == "partial"
			created := map[string]bool{}
			for _, a := range c.Actions() {
				if a.GetVerb() == "create" {
					created[a.GetResource().Resource] = true
				}
				if !positive && (a.GetVerb() == "create" || a.GetVerb() == "patch" || a.GetVerb() == "delete") {
					t.Fatalf("blocked %s mutated %s", mode, a.GetResource().Resource)
				}
			}
			if positive {
				for _, resource := range []string{"secrets", "persistentvolumeclaims", "deployments"} {
					if !created[resource] {
						t.Fatalf("first provision did not create %s", resource)
					}
				}
				dep, err := c.AppsV1().Deployments(cfg.Namespace).Get(context.Background(), deploymentName(w.ID), metav1.GetOptions{})
				if err != nil || dep.Annotations[annotationRoll] != "" {
					t.Fatalf("initial create reserved replacement: %v", err)
				}
			}
		})
	}
}

type pressureReasonCordoner struct {
	fakeCordoner
	requested []string
	released  []string
}

func (c *pressureReasonCordoner) RequestDrainWithReason(ctx context.Context, id string, reasons ...string) error {
	c.requested = append(c.requested, reasons...)
	return c.RequestDrain(ctx, id)
}
func (c *pressureReasonCordoner) ClearDrainWithReason(ctx context.Context, id string, reasons ...string) error {
	c.released = append(c.released, reasons...)
	return c.ClearDrain(ctx, id)
}

func TestPressureClearedTwoTicks(t *testing.T) {
	for _, mode := range []string{"clear", "unknown-target", "sibling-roll", "maintenance", "maintenance-owned", "maintenance-fenced", "raw-pvc-reservation", "custody", "image", "legacy", "pressure-present", "volume-pending", "release-error"} {
		t.Run(mode, func(t *testing.T) {
			w := evidence(desired("w1"), m5Now)
			w.Busy, w.DiskPressure = true, true
			cfg := testConfig()
			if mode == "unknown-target" {
				cfg.RollTargetVersion = ""
			}
			resolver := testResolver(t)
			if mode == "unknown-target" {
				var err error
				resolver, err = preset.NewResolver("harbor.example.com/uzi", "1.0.0")
				if err != nil {
					t.Fatal(err)
				}
			}
			spec, err := resolver.Resolve(w.Template, w.Size)
			if err != nil {
				t.Fatal(err)
			}
			dep := RenderDeployment(cfg, w, spec)
			objects := []runtime.Object{dep}
			for _, p := range RenderPVCs(cfg, w, spec) {
				objects = append(objects, p)
			}
			cordon := &pressureReasonCordoner{}
			m, c := newMat(t, objects...)
			m.cfg = cfg
			m.cordoner = cordon
			m.resolver = resolver
			m.now = func() time.Time { return m5Now }
			rollTickTest(t, m, []protocol.DesiredWorker{w})
			if len(cordon.requested) != 1 || cordon.requested[0] != "pressure_recycle" {
				t.Fatalf("pressure ownership missing: %v", cordon.requested)
			}
			if mode == "unknown-target" {
				current, err := c.AppsV1().Deployments(cfg.Namespace).Get(context.Background(), dep.Name, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if current.Annotations[annotationRoll] != "" || current.Spec.Template.Annotations[AnnotationSpecHash] != dep.Spec.Template.Annotations[AnnotationSpecHash] {
					t.Fatal("busy pressure request reserved or rolled unknown target")
				}
			}
			since := m5Now
			w.DrainingSince, w.DrainingReason = &since, token("pressure_recycle")
			w.DiskPressure = false
			workers := []protocol.DesiredWorker{w}
			var siblingMarker string
			switch mode {
			case "sibling-roll":
				sibling := evidence(desired("w2"), m5Now)
				siblingDep := markedDeployment(t, cfg, sibling, false)
				siblingMarker = siblingDep.Annotations[annotationRoll]
				if _, err := c.AppsV1().Deployments(cfg.Namespace).Create(context.Background(), siblingDep, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
				for _, p := range RenderPVCs(cfg, sibling, spec) {
					if _, err := c.CoreV1().PersistentVolumeClaims(cfg.Namespace).Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
						t.Fatal(err)
					}
				}
				workers = append(workers, sibling)
			case "maintenance":
				w.DindMaintenance = &protocol.DindMaintenance{Phase: "requested"}
			case "maintenance-owned", "maintenance-fenced":
				w.DindMaintenance = &protocol.DindMaintenance{Phase: "stopping", DeploymentUID: string(dep.UID), PVCUID: dindDataPVCName(w.ID), Fenced: mode == "maintenance-fenced"}
			case "raw-pvc-reservation":
				p, err := c.CoreV1().PersistentVolumeClaims(cfg.Namespace).Get(context.Background(), nixPVCName(w.ID), metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				p.Annotations = map[string]string{annotationRoll: ""}
				if _, err := c.CoreV1().PersistentVolumeClaims(cfg.Namespace).Update(context.Background(), p, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			case "custody":
				w.CustodyHeld = true
			case "image":
				m.resolver, _ = preset.NewResolver("harbor.example.com/uzi", "v2.0.0")
			case "legacy":
				w.DrainingReason = nil
			case "pressure-present":
				w.DiskPressure = true
			case "volume-pending":
				p, err := c.CoreV1().PersistentVolumeClaims(cfg.Namespace).Get(context.Background(), nixPVCName(w.ID), metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				p.DeletionTimestamp = &metav1.Time{Time: m5Now}
				if _, err = c.CoreV1().PersistentVolumeClaims(cfg.Namespace).Update(context.Background(), p, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			case "release-error":
				cordon.clearErr = fmt.Errorf("release unavailable")
			}
			workers[0] = w
			c.ClearActions()
			obs, err := m.Observe(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			err = m.Reconcile(context.Background(), workers, obs)
			positive := mode == "clear" || mode == "unknown-target" || mode == "sibling-roll" || mode == "release-error"
			if positive && (len(cordon.released) != 1 || cordon.released[0] != "pressure_recycle") {
				t.Fatalf("pressure not released: %v", cordon.released)
			}
			if !positive && len(cordon.released) != 0 {
				t.Fatalf("independent fence released: %v", cordon.released)
			}
			if mode != "release-error" && err != nil {
				t.Fatal(err)
			}
			if mode == "sibling-roll" {
				if len(cordon.clearCalls) != 1 || cordon.clearCalls[0] != w.ID {
					t.Fatalf("pressure clear targeted sibling: %v", cordon.clearCalls)
				}
				siblingDep, err := c.AppsV1().Deployments(cfg.Namespace).Get(context.Background(), deploymentName("w2"), metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if siblingDep.Annotations[annotationRoll] != siblingMarker {
					t.Fatal("pressure release changed sibling reservation")
				}
				var marker rollMarker
				if err := json.Unmarshal([]byte(siblingDep.Annotations[annotationRoll]), &marker); err != nil {
					t.Fatal(err)
				}
				if !marker.Canary || marker.Completed {
					t.Fatalf("sibling roll no longer active: %+v", marker)
				}
				for _, id := range []string{w.ID, "w2"} {
					p := m.PacingOutcomes()[id]
					if p == nil || p.CanaryID == nil || *p.CanaryID != "w2" || p.State == "completed" {
						t.Fatalf("worker %s lost active sibling pool: %+v", id, p)
					}
				}
			}
			if mode == "release-error" && err == nil {
				t.Fatal("failed release unreported")
			}
			for _, a := range c.Actions() {
				if a.GetVerb() == "delete" {
					t.Fatalf("release tick destructive: %v", a)
				}
			}
		})
	}
}
