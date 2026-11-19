package kube

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

// PRD #4213: render-time refusal and PVC-create rejection reporting.

func refusalMat(t *testing.T, cfg RenderConfig, objs ...runtime.Object) (*Materializer, *fake.Clientset) {
	t.Helper()
	client := enforcingClient(t, objs...)
	m := New(client, cfg, testResolver(t), nil, DrainPolicy{}, RecyclePolicy{Enabled: true}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return m, client
}

func deploymentWrites(c *fake.Clientset) []string {
	var out []string
	for _, a := range c.Actions() {
		if a.GetResource().Resource != "deployments" {
			continue
		}
		switch a.GetVerb() {
		case "create", "update", "patch", "delete":
			out = append(out, a.GetVerb())
		}
	}
	return out
}

func wantRefusal(t *testing.T, m *Materializer, id, kind, reason string) {
	t.Helper()
	got, ok := m.SizeRefusals()[id]
	if !ok {
		t.Fatalf("no refusal recorded for %s: %+v", id, m.SizeRefusals())
	}
	if got.Kind != kind || got.Reason != reason {
		t.Fatalf("refusal for %s = %+v, want {%s %q}", id, got, kind, reason)
	}
}

func TestOverCeilingSizeIsNotRenderedAndIsRecorded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    func(*RenderConfig)
		worker func(*protocol.DesiredWorker)
		reason string
	}{
		{"memory", func(c *RenderConfig) { c.MaxMemory = "1Gi" }, nil, "size refused: memory limit 2Gi exceeds tier max 1Gi"},
		{"cpu", func(c *RenderConfig) { c.MaxCPU = "500m" }, nil, "size refused: cpu limit 1 exceeds tier max 500m"},
		{"data", func(c *RenderConfig) { c.MaxPVCStorage = "4Gi" }, nil, "size refused: data size 5Gi exceeds tier max 4Gi"},
		{"isolated memory", func(c *RenderConfig) { c.IsolatedNamespace = "uzi-lane"; c.IsolatedMaxMemory = "1Gi" },
			func(w *protocol.DesiredWorker) { w.Isolated = true }, "size refused: memory limit 2Gi exceeds tier max 1Gi"},
		{"docker cpu", func(c *RenderConfig) { c.DockerNamespace = "uzi-docker"; c.DockerMaxCPU = "500m" },
			func(w *protocol.DesiredWorker) { w.Docker = true }, "size refused: cpu limit 1 exceeds tier max 500m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			tc.cfg(&cfg)
			m, client := refusalMat(t, cfg)
			w := newWorkerFixture(xsWorker("x1"))
			if tc.worker != nil {
				tc.worker(&w)
			}
			if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, nil); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if got := deploymentWrites(client); len(got) != 0 {
				t.Fatalf("refused worker wrote deployments: %v", got)
			}
			if len(createdPVCs(client)) != 0 {
				t.Fatalf("refused worker created claims: %v", createdPVCs(client))
			}
			wantRefusal(t, m, "x1", protocol.BlockingKindSizeRefused, tc.reason)
		})
	}
}

func TestUnsetCeilingsCheckNothing(t *testing.T) {
	m, _ := refusalMat(t, testConfig())
	w := newWorkerFixture(xsWorker("x1"))
	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, nil); err != nil {
		t.Fatal(err)
	}
	if len(m.SizeRefusals()) != 0 {
		t.Fatalf("unexpected refusals %+v", m.SizeRefusals())
	}
}

func TestRefusedEditOfExistingDeploymentLeavesItUntouched(t *testing.T) {
	base := *sizeSeedsFromAPI(t)["l"]
	w := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "l", Resources: &base}
	hash := hashVia(t, testConfig(), w)
	cfg := testConfig()
	cfg.MaxMemory = "20Gi" // l's 20Gi limit fits exactly
	m, client := refusalMat(t, cfg, deployedWorker("w1", 0, hash))
	observed := []reconcile.ObservedWorker{{ID: "w1", HasDeployment: true, SpecHash: hash}}
	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{evidence(w, m.now())}, observed); err != nil {
		t.Fatal(err)
	}
	if len(m.SizeRefusals()) != 0 {
		t.Fatalf("size at the ceiling was refused: %+v", m.SizeRefusals())
	}

	edited := base
	edited.MemoryLimitMiB += 1024
	w.Resources = &edited
	client.ClearActions()
	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{evidence(w, m.now())}, observed); err != nil {
		t.Fatal(err)
	}
	if got := deploymentWrites(client); len(got) != 0 {
		t.Fatalf("refused edit touched the deployment: %v", got)
	}
	wantRefusal(t, m, "w1", protocol.BlockingKindSizeRefused, "size refused: memory limit 21Gi exceeds tier max 20Gi")

	// Lowering it back renders again and the refusal clears with the next tick.
	w.Resources = &base
	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{evidence(w, m.now())}, observed); err != nil {
		t.Fatal(err)
	}
	if len(m.SizeRefusals()) != 0 {
		t.Fatalf("refusal did not clear: %+v", m.SizeRefusals())
	}
}

func TestHibernatedWorkerWithRefusedSizeIsRecordedAndUntouched(t *testing.T) {
	cfg := testConfig()
	cfg.MaxMemory = "1Gi"
	m, client := refusalMat(t, cfg)
	w := newWorkerFixture(xsWorker("h1"))
	w.Hibernation = &protocol.Hibernation{State: protocol.HibernationHibernated, OpID: "op-1"}
	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, nil); err != nil {
		t.Fatal(err)
	}
	if got := deploymentWrites(client); len(got) != 0 {
		t.Fatalf("deployment writes %v", got)
	}
	wantRefusal(t, m, "h1", protocol.BlockingKindSizeRefused, "size refused: memory limit 2Gi exceeds tier max 1Gi")
}

func TestInvalidResourcesAreRefused(t *testing.T) {
	m, client := refusalMat(t, testConfig())
	w := newWorkerFixture(xsWorker("z1"))
	bad := *customXS()
	bad.MemoryLimitMiB = 0
	w.Resources = &bad
	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, nil); err != nil {
		t.Fatalf("an invalid size must be a skip, not a tick error: %v", err)
	}
	if got := deploymentWrites(client); len(got) != 0 {
		t.Fatalf("deployment writes %v", got)
	}
	wantRefusal(t, m, "z1", protocol.BlockingKindSizeRefused, "size refused: invalid memory_limit_mib 0")
}

// planRoll resolves every non-ephemeral worker, so it must record on its own, not only
// via reconcileWorker.
func TestPlanRollRecordsARefusalWithoutAnyRender(t *testing.T) {
	cfg := testConfig()
	cfg.MaxMemory = "1Gi"
	m, _ := refusalMat(t, cfg)
	w := newWorkerFixture(xsWorker("p1"))
	m.planRoll(context.Background(), []protocol.DesiredWorker{w}, map[string]reconcile.ObservedWorker{})
	wantRefusal(t, m, "p1", protocol.BlockingKindSizeRefused, "size refused: memory limit 2Gi exceeds tier max 1Gi")
}

func TestDinDOverrideBypassesTheDinDDataCheck(t *testing.T) {
	cfg := testConfig()
	cfg.DockerNamespace = "uzi-docker"
	cfg.DockerMaxPVCStorage = "8Gi"
	res := *customXS()
	res.DataGiB, res.DinDDataGiB = 5, 20
	w := newWorkerFixture(protocol.DesiredWorker{ID: "d1", Template: "base", Size: "xs", Docker: true, Resources: &res})

	m, _ := refusalMat(t, cfg)
	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, nil); err != nil {
		t.Fatal(err)
	}
	wantRefusal(t, m, "d1", protocol.BlockingKindSizeRefused, "size refused: dind data size 20Gi exceeds tier max 8Gi")

	cfg.DinDDataSize = "6Gi" // the cluster override wins, so the size's own DinD value is moot
	m, _ = refusalMat(t, cfg)
	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, nil); err != nil {
		t.Fatal(err)
	}
	if len(m.SizeRefusals()) != 0 {
		t.Fatalf("override should bypass the dind check: %+v", m.SizeRefusals())
	}
}

func TestPVCCreateRejectionIsRecordedAndClearsOnSuccess(t *testing.T) {
	m, client := refusalMat(t, testConfig())
	reject := true
	client.PrependReactor("create", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if !reject {
			return false, nil, nil
		}
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumeclaims"}, "x", errors.New("exceeded quota: secret-detail"))
	})
	w := newWorkerFixture(xsWorker("q1"))
	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, nil); err == nil {
		t.Fatal("a rejected claim create must keep returning its error")
	}
	wantRefusal(t, m, "q1", protocol.BlockingKindPVCRejected, "Forbidden creating data claim")
	if strings.Contains(m.SizeRefusals()["q1"].Reason, "secret-detail") {
		t.Fatal("raw apiserver text leaked into the reason")
	}

	reject = false
	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, nil); err != nil {
		t.Fatalf("later tick: %v", err)
	}
	if len(m.SizeRefusals()) != 0 {
		t.Fatalf("rejection did not clear after success: %+v", m.SizeRefusals())
	}
}

func TestPVCRejectionNamesTheClaim(t *testing.T) {
	cfg := testConfig()
	cfg.DockerNamespace = "uzi-docker"
	m, client := refusalMat(t, cfg)
	client.PrependReactor("create", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		p := a.(k8stesting.CreateAction).GetObject().(*corev1.PersistentVolumeClaim)
		if p.Name == dindDataPVCName("q2") {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumeclaims"}, p.Name, errors.New("no"))
		}
		return false, nil, nil
	})
	w := newWorkerFixture(protocol.DesiredWorker{ID: "q2", Template: "base", Size: "m", Docker: true})
	_ = m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, nil)
	wantRefusal(t, m, "q2", protocol.BlockingKindPVCRejected, "Forbidden creating dind claim")
}

// Invalid and RequestEntityTooLarge are admission rejections of the claim itself, so they
// record pvc_rejected like Forbidden does.
func TestPVCRejectionRecordsInvalidAndTooLarge(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		reason string
	}{
		"invalid":  {apierrors.NewInvalid(schema.GroupKind{Kind: "PersistentVolumeClaim"}, "x", nil), "Invalid creating data claim"},
		"toolarge": {apierrors.NewRequestEntityTooLargeError("too big"), "RequestEntityTooLarge creating data claim"},
	} {
		t.Run(name, func(t *testing.T) {
			m, client := refusalMat(t, testConfig())
			client.PrependReactor("create", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, tc.err
			})
			w := newWorkerFixture(xsWorker("r1"))
			_ = m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, nil)
			wantRefusal(t, m, "r1", protocol.BlockingKindPVCRejected, tc.reason)
		})
	}
}

// A transient create failure is not the cluster rejecting the claim: nothing is recorded,
// and the error still surfaces.
func TestTransientPVCCreateErrorsRecordNothing(t *testing.T) {
	gr := schema.GroupResource{Resource: "persistentvolumeclaims"}
	for name, createErr := range map[string]error{
		"timeout":  apierrors.NewTimeoutError("slow", 1),
		"internal": apierrors.NewInternalError(errors.New("boom")),
		"429":      apierrors.NewTooManyRequests("later", 1),
		"conflict": apierrors.NewConflict(gr, "x", errors.New("c")),
	} {
		t.Run(name, func(t *testing.T) {
			m, client := refusalMat(t, testConfig())
			client.PrependReactor("create", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, createErr
			})
			w := newWorkerFixture(xsWorker("t1"))
			if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, nil); err == nil {
				t.Fatal("a failed claim create must keep returning its error")
			}
			if got := m.SizeRefusals(); len(got) != 0 {
				t.Fatalf("transient error recorded a refusal: %+v", got)
			}
		})
	}
}

// Startup stays builtins-only: a chart ceiling below a shipped builtin (l's 50Gi) refuses
// to boot regardless of what an admin has since defined, and the message says why.
func TestValidatePVCCeilingsChecksOnlyTheCompiledInBuiltins(t *testing.T) {
	err := ValidatePVCCeilings(RenderConfig{MaxPVCStorage: "40Gi"}, testResolver(t))
	if err == nil {
		t.Fatal("a 40Gi ceiling is below l's shipped 50Gi data claim and must refuse startup")
	}
	for _, want := range []string{"restricted tier", "50Gi", "chart ceilings must stay at or above the shipped defaults"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%v", want, err)
		}
	}
}
