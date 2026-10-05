package kube

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

type maintenanceFake struct {
	op        *protocol.DindMaintenance
	calls     []string
	refuse    string
	reportErr error
	below     bool
}

type maintenanceRefusal struct{}

func (maintenanceRefusal) Error() string   { return "HTTP 409" }
func (maintenanceRefusal) HTTPStatus() int { return 409 }

func (f *maintenanceFake) RequestDrain(context.Context, string) error {
	f.calls = append(f.calls, "drain")
	return nil
}
func (f *maintenanceFake) ClearDrain(context.Context, string) error {
	f.calls = append(f.calls, "clear")
	return nil
}
func (f *maintenanceFake) ReportReadiness(ctx context.Context, r protocol.StatusReport) error {
	return f.Report(ctx, r)
}
func (f *maintenanceFake) Report(_ context.Context, r protocol.StatusReport) error {
	f.calls = append(f.calls, "report")
	if len(r.Workers) != 1 || r.Workers[0].Phase != protocol.PhaseSettled {
		return errors.New("not settled")
	}
	return f.reportErr
}
func (f *maintenanceFake) TransitionDinD(_ context.Context, _ string, op protocol.DindMaintenance) (protocol.DindMaintenance, error) {
	f.calls = append(f.calls, op.Phase)
	if f.refuse == op.Phase || op.Reason == "below_threshold" && !f.below {
		return protocol.DindMaintenance{}, maintenanceRefusal{}
	}
	if op.ID == "" {
		op.ID = "operation"
		op.Nonce = "nonce"
		op.RegisterNonce = "old-register"
	}
	op.Fenced = op.Phase != "requested" && op.Phase != "cancelled" && op.Phase != "complete"
	f.op = &op
	return op, nil
}

func TestDinDMaintenanceMultiTick(t *testing.T) {
	ctx := context.Background()
	cfg := dockerTestConfig()
	w := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", Docker: true, DiskPressure: false, DiskPressureVolumes: []string{"dind"}}
	spec, err := testResolver(t).Resolve(w.Template, w.Size)
	if err != nil {
		t.Fatal(err)
	}
	d := RenderDeployment(cfg, w, spec)
	d.UID = "old-deployment"
	pvcs := RenderPVCs(cfg, w, spec)
	objs := []runtime.Object{d}
	for _, p := range pvcs {
		p.UID = types.UID(p.Name)
		p.Status.Phase = corev1.ClaimBound
		objs = append(objs, p)
	}
	m, client := newMat(t, objs...)
	m.cfg = cfg
	m.drain.ForceRoll = true
	f := &maintenanceFake{}
	m.cordoner = f
	tick := func() {
		t.Helper()
		w.DindMaintenance = f.op
		observed, err := m.Observe(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = m.Reconcile(ctx, []protocol.DesiredWorker{w}, observed); err != nil {
			t.Fatal(err)
		}
	}
	tick()
	if f.op == nil || f.op.Phase != "requested" {
		t.Fatalf("initial binding: %+v", f.op)
	}
	for _, a := range client.Actions() {
		if a.GetVerb() == "delete" {
			t.Fatal("initial request destroyed an object")
		}
	}
	w.Busy = true
	tick()
	if f.op.Phase != "requested" {
		t.Fatal("busy worker fenced")
	}
	w.Busy = false
	w.CustodyHeld = true
	tick()
	if f.op.Phase != "requested" {
		t.Fatal("custody worker fenced")
	}
	w.CustodyHeld = false
	tick()
	if f.op.Phase != "ready" || !f.op.Fenced {
		t.Fatal("missing ready fence")
	}
	tick()
	if f.op.Phase != "ready" {
		t.Fatal("stopped without worker ACK")
	}
	f.op.ReadyACK = true
	tick()
	if f.op.Phase != "stopping" {
		t.Fatal("missing stopping")
	}
	// Turning off recycling after stopping must finish; no force override is used.
	m.recycle.Enabled = false
	f.op.ReadyACK = false // re-registration after stopping needs no new ACK.
	tick()
	for _, a := range client.Actions() {
		if a.GetVerb() == "delete" && a.GetResource().Resource == "deployments" {
			options := a.(k8stesting.DeleteAction).GetDeleteOptions()
			if options.Preconditions == nil || *options.Preconditions.UID != "old-deployment" || options.PropagationPolicy == nil || *options.PropagationPolicy != metav1.DeletePropagationForeground {
				t.Fatalf("unsafe delete: %+v", options)
			}
		}
	}
	// An unprovenance-stamped, terminating worker pod still prevents volume deletion.
	now := metav1.Now()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "old-pod", Namespace: cfg.DockerNamespace, Labels: map[string]string{LabelWorkerID: w.ID}, DeletionTimestamp: &now}}
	if _, err := client.CoreV1().Pods(cfg.DockerNamespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	tick()
	if f.op.Phase != "stopping" {
		t.Fatal("advanced with terminating worker pod")
	}
	if err := client.CoreV1().Pods(cfg.DockerNamespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	// Unknown pod listing must fail closed.
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("unknown pods") })
	observed, err := m.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Reconcile(ctx, []protocol.DesiredWorker{w}, observed); err == nil {
		t.Fatal("unknown pods accepted")
	}
	client.ReactionChain = client.ReactionChain[1:]
	tick()
	if f.op.Phase != "recycling" {
		t.Fatal("did not enter recycling")
	}
	// A terminating old PVC is awaited, not deleted again or recreated.
	p, err := client.CoreV1().PersistentVolumeClaims(cfg.DockerNamespace).Get(ctx, dindDataPVCName(w.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p.DeletionTimestamp = &now
	if _, err = client.CoreV1().PersistentVolumeClaims(cfg.DockerNamespace).Update(ctx, p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	client.ClearActions()
	tick()
	for _, a := range client.Actions() {
		if a.GetVerb() == "delete" || a.GetVerb() == "create" {
			t.Fatal("acted over terminating old PVC")
		}
	}
	p.DeletionTimestamp = nil
	if _, err = client.CoreV1().PersistentVolumeClaims(cfg.DockerNamespace).Update(ctx, p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	tick()
	for _, a := range client.Actions() {
		if a.GetVerb() == "delete" && a.GetResource().Resource == "persistentvolumeclaims" {
			action := a.(k8stesting.DeleteAction)
			if action.GetName() != dindDataPVCName(w.ID) || action.GetDeleteOptions().Preconditions == nil || *action.GetDeleteOptions().Preconditions.UID != p.UID {
				t.Fatal("unsafe PVC deletion")
			}
		}
	}
	tick() // recreate only after disappearance
	replacement, err := client.CoreV1().PersistentVolumeClaims(cfg.DockerNamespace).Get(ctx, dindDataPVCName(w.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	replacement.UID = "new-pvc"
	replacement.Status.Phase = corev1.ClaimPending
	if _, err = client.CoreV1().PersistentVolumeClaims(cfg.DockerNamespace).Update(ctx, replacement, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	newDep, err := client.AppsV1().Deployments(cfg.DockerNamespace).Get(ctx, d.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	newDep.UID = "new-deployment"
	if _, err = client.AppsV1().Deployments(cfg.DockerNamespace).Update(ctx, newDep, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	ready := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "replacement", Namespace: cfg.DockerNamespace, UID: "new-pod", Labels: newDep.Spec.Template.Labels, Annotations: newDep.Spec.Template.Annotations}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	stampMaintenancePodOwner(ready, newDep.Name)
	if _, err = client.AppsV1().ReplicaSets(cfg.DockerNamespace).Create(ctx, maintenanceReplicaSet(ready, newDep), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = client.CoreV1().Pods(cfg.DockerNamespace).Create(ctx, ready, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	tick()
	if slices.Contains(f.calls, "complete") {
		t.Fatal("complete over Pending replacement")
	}
	replacement.Status.Phase = corev1.ClaimBound
	if _, err = client.CoreV1().PersistentVolumeClaims(cfg.DockerNamespace).Update(ctx, replacement, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.reportErr = errors.New("publication failed")
	w.DindMaintenance = f.op
	observed, err = m.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Reconcile(ctx, []protocol.DesiredWorker{w}, observed); err == nil {
		t.Fatal("publication error swallowed")
	}
	if slices.Contains(f.calls, "complete") {
		t.Fatal("completed despite publication failure")
	}
	f.reportErr = nil
	f.refuse = "complete"
	f.calls = nil
	tick()
	if !slices.Equal(f.calls, []string{"report", "complete"}) || f.op.Phase != "recycling" || !f.op.Fenced {
		t.Fatalf("registration refusal released fence: %+v %v", f.op, f.calls)
	}
	f.refuse = ""
	f.calls = nil
	tick()
	if f.op.Phase != "complete" || f.op.Fenced || !slices.Equal(f.calls, []string{"report", "complete"}) {
		t.Fatal("successful completion did not release fence")
	}
	assertNoSecretReads(t, client)
}

func TestDinDReportOnlyAndCooldown(t *testing.T) {
	for _, mode := range []string{"plain", "ephemeral", "unsupported", "refused", "cooldown", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			cfg := dockerTestConfig()
			w := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", Docker: true, DiskPressure: false, DiskPressureVolumes: []string{"dind"}}
			if mode == "plain" {
				w.Docker = false
			}
			if mode == "ephemeral" {
				w.Ephemeral = true
			}
			spec, err := testResolver(t).Resolve(w.Template, w.Size)
			if err != nil {
				t.Fatal(err)
			}
			dep := RenderDeployment(cfg, w, spec)
			dep.UID = "old-deployment"
			objs := []runtime.Object{dep}
			for _, p := range RenderPVCs(cfg, w, spec) {
				p.UID = types.UID(p.Name)
				p.Status.Phase = corev1.ClaimBound
				p.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
				if mode == "cooldown" && p.Name == dindDataPVCName(w.ID) {
					p.CreationTimestamp = metav1.Now()
				}
				objs = append(objs, p)
			}
			m, client := newMat(t, objs...)
			m.cfg = cfg
			m.recycle.Cooldown = time.Minute
			f := &maintenanceFake{refuse: "requested"}
			if mode != "unsupported" {
				m.cordoner = f
			}
			if mode == "disabled" {
				m.recycle.Enabled = false
			}
			obs, err := m.Observe(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err = m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, obs); err != nil {
				t.Fatal(err)
			}
			for _, a := range client.Actions() {
				if a.GetVerb() == "delete" || a.GetVerb() == "patch" {
					t.Fatalf("report-only actuated: %v", a)
				}
			}
			if f.op != nil {
				t.Fatal("unexpected operation")
			}
		})
	}
}
