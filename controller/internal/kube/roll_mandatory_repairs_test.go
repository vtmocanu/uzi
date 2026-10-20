package kube

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/preset"
	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

type mandatoryReasonCordoner struct {
	fakeCordoner
	reasons []string
}

func (f *mandatoryReasonCordoner) ClearDrainWithReason(ctx context.Context, id string, reasons ...string) error {
	f.reasons = append(f.reasons, reasons...)
	return f.ClearDrain(ctx, id)
}

func TestMandatoryIndependentDinDRecovery(t *testing.T) {
	cfg := dockerTestConfig()
	w := evidence(desiredDocker("w1"), m5Now)
	d := RenderDeployment(cfg, w, testSpec(t, w.Template, w.Size))
	d.UID = "old-deployment"
	objects := []runtime.Object{d}
	for _, p := range RenderPVCs(cfg, w, testSpec(t, w.Template, w.Size)) {
		p.UID = types.UID(p.Name)
		p.Status.Phase = corev1.ClaimBound
		objects = append(objects, p)
	}
	f := &maintenanceFake{op: &protocol.DindMaintenance{Phase: "stopping", Fenced: true, DeploymentUID: string(d.UID), PVCUID: dindDataPVCName(w.ID)}}
	m, c := newMat(t, objects...)
	m.cfg, m.cordoner = cfg, f
	m.now = func() time.Time { return m5Now }
	tick := func() { t.Helper(); w.DindMaintenance = f.op; rollTickTest(t, m, []protocol.DesiredWorker{w}) }
	tick()
	if _, err := c.AppsV1().Deployments(cfg.DockerNamespace).Get(context.Background(), d.Name, metav1.GetOptions{}); err == nil {
		t.Fatal("stopping did not delete deployment")
	}
	tick()
	if f.op.Phase != "recycling" {
		t.Fatalf("stalled after deletion: %+v", f.op)
	}
	tick()
	if _, err := c.CoreV1().PersistentVolumeClaims(cfg.DockerNamespace).Get(context.Background(), dindDataPVCName(w.ID), metav1.GetOptions{}); err == nil {
		t.Fatal("old DinD claim survived")
	}
	tick()
	replacement, err := c.AppsV1().Deployments(cfg.DockerNamespace).Get(context.Background(), d.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("replacement missing: %v", err)
	}
	if replacement.Annotations[annotationRoll] != "" {
		t.Fatal("independent recovery synthesized an image-roll participant")
	}
}

func TestMandatoryIndependentPressureDeadline(t *testing.T) {
	w := evidence(desired("w1"), m5Now)
	w.Busy, w.DiskPressure = true, true
	since := m5Now.Add(-2 * time.Hour)
	w.DrainingSince, w.DrainingReason = &since, token("disk_pressure")
	d := RenderDeployment(testConfig(), w, testSpec(t, w.Template, w.Size))
	objects := []runtime.Object{d}
	for _, p := range RenderPVCs(testConfig(), w, testSpec(t, w.Template, w.Size)) {
		objects = append(objects, p)
	}
	m, c := newMat(t, objects...)
	m.now = func() time.Time { return m5Now }
	m.drain.Deadline = time.Hour
	m.cordoner = &fakeCordoner{}
	rollTickTest(t, m, []protocol.DesiredWorker{w})
	if _, err := c.AppsV1().Deployments(testConfig().Namespace).Get(context.Background(), d.Name, metav1.GetOptions{}); err == nil {
		t.Fatal("expired independent pressure drain did not recycle")
	}
	rollTickTest(t, m, []protocol.DesiredWorker{w})
	if _, err := c.AppsV1().Deployments(testConfig().Namespace).Get(context.Background(), d.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("replacement missing: %v", err)
	}
}

func TestMandatoryCompletedLateDrainRelease(t *testing.T) {
	for _, negative := range []string{"none", "unpromoted", "wrong-uid", "maintenance", "pressure", "custody", "independent-drain"} {
		t.Run(negative, func(t *testing.T) {
			w := evidence(desired("w1"), m5Now)
			d := markedDeployment(t, testConfig(), w, true)
			m, c := newMat(t, d)
			m.now = func() time.Time { return m5Now }
			f := &mandatoryReasonCordoner{}
			m.cordoner = f
			currentProof(t, c, &w, m5Now)
			rollTickTest(t, m, []protocol.DesiredWorker{w})
			since := m5Now.Add(-time.Minute)
			w.DrainingSince, w.DrainingReason = &since, token("image_roll")
			switch negative {
			case "unpromoted":
				w.RollEvidence.RegisterNonce = token("prior-registration")
			case "wrong-uid":
				w.RollEvidence.PodUID = token("wrong")
			case "maintenance":
				w.DindMaintenance = &protocol.DindMaintenance{Phase: "requested"}
			case "pressure":
				w.DiskPressure = true
			case "custody":
				w.CustodyHeld = true
			case "independent-drain":
				w.DrainingReason = token("disk_pressure")
			}
			rollTickTest(t, m, []protocol.DesiredWorker{w})
			want := 0
			if negative == "none" {
				want = 1
			}
			if len(f.clearCalls) != want {
				t.Fatalf("clears = %v; want %d", f.clearCalls, want)
			}
			if want == 1 && (len(f.reasons) != 1 || f.reasons[0] != "image_roll") {
				t.Fatalf("release reasons: %v", f.reasons)
			}
			if want == 1 {
				w.DrainingSince, w.DrainingReason = nil, nil
			}
			rollTickTest(t, m, []protocol.DesiredWorker{w})
			if len(f.clearCalls) != want {
				t.Fatalf("repeated clears: %v", f.clearCalls)
			}
		})
	}
}

func TestMandatoryRecycleMountReferences(t *testing.T) {
	for _, claim := range []string{"data", "nix", "unrelated"} {
		for _, labels := range []bool{false, true} {
			for _, terminating := range []bool{false, true} {
				t.Run(claim+"/"+map[bool]string{false: "unlabelled", true: "labelled"}[labels]+"/"+map[bool]string{false: "live", true: "terminating"}[terminating], func(t *testing.T) {
					w := evidence(desired("w1"), m5Now)
					w.DiskPressure = true
					d := markedDeployment(t, testConfig(), w, true)
					objects := []runtime.Object{d}
					for _, p := range RenderPVCs(testConfig(), w, testSpec(t, w.Template, w.Size)) {
						objects = append(objects, p)
					}
					name := "unrelated"
					if claim == "data" {
						name = dataPVCName(w.ID)
					}
					if claim == "nix" {
						name = nixPVCName(w.ID)
					}
					pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "unmanaged", Namespace: testConfig().Namespace}, Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "mount", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: name}}}}}}
					if labels {
						pod.Labels = objectLabels(w.ID)
					}
					if terminating {
						at := metav1.NewTime(m5Now)
						pod.DeletionTimestamp = &at
					}
					objects = append(objects, pod)
					m, c := newMat(t, objects...)
					m.now = func() time.Time { return m5Now }
					rollTickTest(t, m, []protocol.DesiredWorker{w})
					c.ClearActions()
					rollTickTest(t, m, []protocol.DesiredWorker{w})
					blocked := claim != "unrelated" || labels
					deleted := false
					for _, a := range c.Actions() {
						if a.GetVerb() == "delete" && a.GetResource().Resource == "pods" {
							t.Fatal("unmanaged pod deleted")
						}
						if a.GetVerb() == "delete" && a.GetResource().Resource == "persistentvolumeclaims" {
							deleted = true
						}
					}
					if deleted == blocked {
						t.Fatalf("PVC deletion=%v blocked=%v", deleted, blocked)
					}
				})
			}
		}
	}
}

func TestMandatoryLostImageReservationHolds(t *testing.T) {
	for _, phase := range []string{"stopping", "recycling"} {
		t.Run(phase, func(t *testing.T) {
			cfg := dockerTestConfig()
			w := evidence(desiredDocker("w1"), m5Now)
			since := m5Now.Add(-time.Hour)
			w.DrainingSince, w.DrainingReason = &since, token("image_roll")
			w.DindMaintenance = &protocol.DindMaintenance{Phase: phase, Fenced: true, DeploymentUID: "lost", PVCUID: "old-dind"}
			objects := []runtime.Object{}
			for _, p := range RenderPVCs(cfg, w, testSpec(t, w.Template, w.Size)) {
				p.UID = "old-dind"
				objects = append(objects, p)
			}
			m, c := newMat(t, objects...)
			m.cfg, m.cordoner = cfg, &maintenanceFake{}
			m.now = func() time.Time { return m5Now }
			for i := 0; i < 2; i++ {
				rollTickTest(t, m, []protocol.DesiredWorker{w})
			}
			for _, a := range c.Actions() {
				if a.GetVerb() == "delete" || a.GetVerb() == "create" {
					t.Fatalf("lost active image reservation allowed %s %s", a.GetVerb(), a.GetResource().Resource)
				}
			}
		})
	}
}

func TestMandatoryIncompleteMountSnapshotHolds(t *testing.T) {
	w := evidence(desired("w1"), m5Now)
	w.DiskPressure = true
	d := markedDeployment(t, testConfig(), w, true)
	objects := []runtime.Object{d}
	for _, p := range RenderPVCs(testConfig(), w, testSpec(t, w.Template, w.Size)) {
		objects = append(objects, p)
	}
	m, c := newMat(t, objects...)
	m.now = func() time.Time { return m5Now }
	rollTickTest(t, m, []protocol.DesiredWorker{w})
	c.ClearActions()
	c.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("incomplete pod list")
	})
	for i := 0; i < 2; i++ {
		rollTickTest(t, m, []protocol.DesiredWorker{w})
	}
	for _, a := range c.Actions() {
		if a.GetVerb() == "delete" {
			t.Fatal("incomplete pod evidence allowed deletion")
		}
	}
}

func TestMandatoryHeldImageDeadlineCannotRecycle(t *testing.T) {
	w := evidence(desired("w1"), m5Now)
	w.Busy, w.DiskPressure = true, true
	since := m5Now.Add(-2 * time.Hour)
	w.DrainingSince, w.DrainingReason = &since, token("image_roll")
	d := RenderDeployment(testConfig(), w, testSpec(t, w.Template, w.Size))
	d.Spec.Template.Annotations[AnnotationSpecHash] = "old"
	peer := evidence(desired("w2"), m5Now)
	pd := RenderDeployment(testConfig(), peer, testSpec(t, peer.Template, peer.Size))
	pd.Spec.Template.Annotations[AnnotationSpecHash] = "old"
	objects := []runtime.Object{d, pd}
	for _, p := range RenderPVCs(testConfig(), w, testSpec(t, w.Template, w.Size)) {
		objects = append(objects, p)
	}
	m, c := newMat(t, objects...)
	m.now = func() time.Time { return m5Now }
	m.drain.Deadline = time.Hour
	m.cordoner = &fakeCordoner{}
	for i := 0; i < 2; i++ {
		rollTickTest(t, m, []protocol.DesiredWorker{w, peer})
	}
	for _, a := range c.Actions() {
		if a.GetVerb() == "delete" {
			t.Fatal("held image deadline allowed deletion")
		}
	}
}

func TestMandatoryPromotedDrainRelease(t *testing.T) {
	w := evidence(desired("w1"), m5Now)
	d := markedDeployment(t, testConfig(), w, false)
	m, c := newMat(t, d)
	m.now = func() time.Time { return m5Now }
	f := &mandatoryReasonCordoner{}
	m.cordoner = f
	currentProof(t, c, &w, m5Now)
	since := m5Now.Add(-time.Minute)
	w.DrainingSince, w.DrainingReason = &since, token("image_roll")
	rollTickTest(t, m, []protocol.DesiredWorker{w})
	if len(f.clearCalls) != 1 || len(f.reasons) != 1 || f.reasons[0] != "image_roll" {
		t.Fatalf("promoted release: %+v", f)
	}
	w.DrainingSince, w.DrainingReason = nil, nil
	rollTickTest(t, m, []protocol.DesiredWorker{w})
	if len(f.clearCalls) != 1 {
		t.Fatal("repeated promoted release")
	}
}

func TestMandatoryHistoricalPressureDeadline(t *testing.T) {
	for _, mode := range []string{"unchanged", "admission-held", "pod-present", "restore-pod-present", "nix-unready", "data-unready", "drift-generation", "drift-hash", "drift-image"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			w := evidence(desired("w1"), m5Now)
			w.Busy, w.DiskPressure = true, true
			since := m5Now.Add(-2 * time.Hour)
			w.DrainingSince, w.DrainingReason = &since, token("disk_pressure")
			d := markedDeployment(t, testConfig(), w, true)
			peer := evidence(desired("w2"), m5Now)
			peer.Busy = true
			peerDep := RenderDeployment(testConfig(), peer, testSpec(t, peer.Template, peer.Size))
			peerDep.UID, peerDep.ResourceVersion = "peer-deployment", "1"
			objects := []runtime.Object{d, peerDep}
			for _, p := range RenderPVCs(testConfig(), w, testSpec(t, w.Template, w.Size)) {
				objects = append(objects, p)
			}
			m, c := newMat(t, objects...)
			m.now = func() time.Time { return m5Now }
			m.drain.Deadline = time.Hour
			f := &fakeCordoner{}
			m.cordoner = f
			currentProof(t, c, &peer, m5Now)
			tick := func() {
				t.Helper()
				m = matrixRestart(m)
				rollTickTest(t, m, []protocol.DesiredWorker{w, peer})
			}
			get := func() *appsv1.Deployment {
				t.Helper()
				got, err := c.AppsV1().Deployments(m.cfg.Namespace).Get(ctx, d.Name, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				return got
			}
			tick()
			if got := get(); got.Spec.Replicas == nil || *got.Spec.Replicas != 0 {
				t.Fatal("independent deadline did not scale down")
			}
			if mode == "pod-present" {
				at := metav1.NewTime(m5Now)
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "terminating-old", Namespace: m.cfg.Namespace, UID: "old-pod", Labels: objectLabels(w.ID), DeletionTimestamp: &at}}
				if err := c.Tracker().Create(corev1.SchemeGroupVersion.WithResource("pods"), pod, pod.Namespace); err != nil {
					t.Fatal(err)
				}
				c.ClearActions()
				tick()
				assertBusyDriftUnchanged(t, c, get())
				if len(deletedSet(c)) != 0 {
					t.Fatal("pressure recycle deleted claims while old pod remained")
				}
				if err := c.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), pod.Namespace, pod.Name); err != nil {
					t.Fatal(err)
				}
			}
			tick()
			if _, err := c.CoreV1().PersistentVolumeClaims(m.cfg.Namespace).Get(ctx, dataPVCName(w.ID), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
				t.Fatalf("historical reservation blocked pressure deadline: %v", err)
			}
			// Pressure has cleared, but Busy remains true through re-mint and restore.
			// New drift must not borrow the independent drain's elapsed permission.
			w.DiskPressure = false
			switch mode {
			case "drift-generation":
				w.Generation++
			case "drift-hash":
				m.cfg.APIURL = "https://replacement.example.com"
			case "drift-image":
				var err error
				m.resolver, err = preset.NewResolver("harbor.example.com/uzi", "v2.0.0")
				if err != nil {
					t.Fatal(err)
				}
				m.cfg.RollTargetVersion = "2.0.0"
			}
			before := get()
			c.ClearActions()
			tick() // Re-mint missing claims; restoration needs a later live observation.
			if mode == "admission-held" {
				w.RollEvidence.OwnerID = "different-owner"
				tick()
				assertBusyDriftUnchanged(t, c, before)
				w.RollEvidence.OwnerID = "owner-A"
			}
			if mode == "restore-pod-present" {
				at := metav1.NewTime(m5Now)
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "late-old-pod", Namespace: m.cfg.Namespace, UID: "late-old-pod", Labels: objectLabels(w.ID), DeletionTimestamp: &at}}
				if err := c.Tracker().Create(corev1.SchemeGroupVersion.WithResource("pods"), pod, pod.Namespace); err != nil {
					t.Fatal(err)
				}
				tick()
				assertBusyDriftUnchanged(t, c, before)
				if err := c.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), pod.Namespace, pod.Name); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "nix-unready" || mode == "data-unready" {
				name := nixPVCName(w.ID)
				if mode == "data-unready" {
					name = dataPVCName(w.ID)
				}
				p, err := c.CoreV1().PersistentVolumeClaims(m.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				at := metav1.NewTime(m5Now)
				p.DeletionTimestamp = &at
				if err := c.Tracker().Update(corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims"), p, p.Namespace); err != nil {
					t.Fatal(err)
				}
				tick()
				assertBusyDriftUnchanged(t, c, before)
				p.DeletionTimestamp = nil
				if err := c.Tracker().Update(corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims"), p, p.Namespace); err != nil {
					t.Fatal(err)
				}
			}
			tick()
			if mode == "drift-generation" || mode == "drift-hash" || mode == "drift-image" {
				tick() // A fresh observation now sees the re-minted live claims.
				obs, err := m.Observe(ctx)
				if err != nil {
					t.Fatal(err)
				}
				live := false
				for _, o := range obs {
					if o.ID == w.ID {
						live = o.HasNixPVC && o.NixPVCSize != nil && o.HasDataPVC && o.DataPVCLive
					}
				}
				if !live {
					t.Fatal("drift negative never reached live replacement volumes")
				}
				if outcome := m.PacingOutcomes()[w.ID]; outcome == nil || outcome.State != "active" {
					t.Fatalf("drift negative was held before busy template guard: %+v", outcome)
				}
				assertBusyDriftUnchanged(t, c, before)
				return
			}
			got := get()
			if !w.Busy || got.UID != d.UID || got.Spec.Replicas == nil || *got.Spec.Replicas != 1 || !reflect.DeepEqual(got.Spec.Template, d.Spec.Template) || got.Annotations[annotationRoll] != d.Annotations[annotationRoll] {
				t.Fatal("unchanged retained deployment failed to restore while busy")
			}
			if len(f.clearCalls) != 0 || len(f.calls) != 0 {
				t.Fatalf("independent drain was changed: %+v", f)
			}
		})
	}
}
