package kube

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

const (
	canaryA = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	canaryB = "9f8e7d6c-5b4a-4a39-8281-706f5e4d3c2b"
)

// canaryCluster builds two idle migrated Docker workers whose Deployments were rendered
// under the BASELINE config (no selector).
func canaryCluster(t *testing.T, o ...lmOpts) (*lmFixture, []protocol.DesiredWorker) {
	t.Helper()
	var objs []runtime.Object
	var ws []protocol.DesiredWorker
	for _, opt := range o {
		oo, w := lmObjects(t, opt)
		objs = append(objs, oo...)
		ws = append(ws, w)
	}
	return lmCluster(t, false, objs...), ws
}

func canaryDeploymentEnv(t *testing.T, f *lmFixture, id string) map[string]string {
	t.Helper()
	d, err := f.client.AppsV1().Deployments(lmNS()).Get(context.Background(), deploymentName(id), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return workerEnvMap(d)
}

func workerEnvMap(d *appsv1.Deployment) map[string]string {
	out := map[string]string{}
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name == workerContainerName {
			for _, e := range c.Env {
				out[e.Name] = e.Value
			}
		}
	}
	return out
}

func canaryPVCDeletes(f *lmFixture) []string {
	var out []string
	for k := range deletedSet(f.client) {
		if strings.HasPrefix(k, "persistentvolumeclaims/") {
			out = append(out, k)
		}
	}
	return out
}

func TestEpochCanaryRollsExactlyTheSelectedIdleWorker(t *testing.T) {
	f, ws := canaryCluster(t, lmOpts{id: canaryA, layout: "new", promoted: true}, lmOpts{id: canaryB, layout: "new", promoted: true})
	f.tickAll(t, ws...) // baseline cfg: steady state
	if wasPatched(f.client) {
		t.Fatal("baseline steady state patched something")
	}
	f.m.cfg.EpochReclaimDeletionCanaryID = canaryA
	f.tickAll(t, ws...)
	if got := templatePatches(f.client); len(got) != 1 || got[0] != deploymentName(canaryA) {
		t.Fatalf("patched = %v, want only %s", got, deploymentName(canaryA))
	}
	if v := canaryDeploymentEnv(t, f, canaryA)[canaryEnvName]; v != "1" {
		t.Fatalf("selected deployment env = %q, want 1", v)
	}
	if _, ok := canaryDeploymentEnv(t, f, canaryB)[canaryEnvName]; ok {
		t.Fatal("unselected deployment carries the env")
	}
	if del := canaryPVCDeletes(f); len(del) != 0 {
		t.Fatalf("PVCs deleted: %v", del)
	}
}

func TestEpochCanaryBusySelectedWorkerIsCordonedNotPatched(t *testing.T) {
	f, ws := canaryCluster(t, lmOpts{id: canaryA, layout: "new", promoted: true})
	ws[0].Busy = true
	f.m.cfg.EpochReclaimDeletionCanaryID = canaryA
	f.tickAll(t, ws...)
	if wasPatched(f.client) {
		t.Fatal("a busy selected worker was patched")
	}
	if len(f.cord.calls) != 1 || f.cord.calls[0] != canaryA {
		t.Fatalf("cordon calls = %v, want [%s]", f.cord.calls, canaryA)
	}
}

func TestEpochCanaryCustodyHeldOldLayoutIsHeld(t *testing.T) {
	f, ws := canaryCluster(t, lmOpts{id: canaryA, layout: "old", custody: true})
	f.m.cfg.EpochReclaimDeletionCanaryID = canaryA
	f.tickAll(t, ws...)
	if wasPatched(f.client) {
		t.Fatal("a custody-held old-layout worker was patched")
	}
	if !strings.Contains(f.logs.String(), "layout-migration-held-custody") {
		t.Fatalf("no migration hold log: %s", f.logs.String())
	}
}

func TestEpochCanaryClearingTheSelectorRepatchesWithoutTheEnv(t *testing.T) {
	// Deployment rendered while selected.
	objs, w := lmObjects(t, lmOpts{id: canaryA, layout: "new", promoted: true})
	selCfg := dockerTestConfig()
	selCfg.EpochReclaimDeletionCanaryID = canaryA
	for _, o := range objs {
		if d, ok := o.(*appsv1.Deployment); ok {
			r := RenderDeployment(selCfg, w, testSpec(t, "base", "m"))
			d.Spec.Template = r.Spec.Template
			d.UID, d.ResourceVersion = types.UID("uid-"+d.Name), "1"
		}
	}
	f := lmCluster(t, false, objs...)
	f.m.cfg.EpochReclaimDeletionCanaryID = canaryA
	f.tickAll(t, w)
	if wasPatched(f.client) {
		t.Fatal("steady selected state patched")
	}
	if canaryDeploymentEnv(t, f, canaryA)[canaryEnvName] != "1" {
		t.Fatal("seed deployment lacks the env")
	}
	f.m.cfg.EpochReclaimDeletionCanaryID = ""
	f.tickAll(t, w)
	if got := templatePatches(f.client); len(got) != 1 || got[0] != deploymentName(canaryA) {
		t.Fatalf("patched = %v after clearing", got)
	}
	if _, ok := canaryDeploymentEnv(t, f, canaryA)[canaryEnvName]; ok {
		t.Fatal("env survived clearing the selector")
	}
}

func TestEpochCanaryDiagnostics(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		f, ws := canaryCluster(t, lmOpts{id: canaryA, layout: "new", promoted: true})
		f.m.cfg.EpochReclaimDeletionCanaryID = canaryB
		f.tickAll(t, ws...)
		if !strings.Contains(f.logs.String(), "epoch-reclaim-deletion-canary-absent worker="+canaryB) {
			t.Fatalf("no absent diagnostic: %s", f.logs.String())
		}
		if wasPatched(f.client) {
			t.Fatal("an absent selector patched a worker")
		}
	})
	t.Run("ineligible", func(t *testing.T) {
		f, ws := canaryCluster(t, lmOpts{id: canaryA, layout: "new", promoted: true})
		ws[0].Ephemeral = true
		f.m.cfg.EpochReclaimDeletionCanaryID = canaryA
		f.tickAll(t, ws...)
		if !strings.Contains(f.logs.String(), "epoch-reclaim-deletion-canary-ineligible worker="+canaryA) {
			t.Fatalf("no ineligible diagnostic: %s", f.logs.String())
		}
		if strings.Contains(f.logs.String(), "canary-absent") {
			t.Fatal("present worker reported absent")
		}
	})
	for _, arm := range []string{"plain", "docker isolated"} {
		t.Run("ineligible "+arm, func(t *testing.T) {
			f, ws := canaryCluster(t, lmOpts{id: canaryA, layout: "new", promoted: true})
			ws[0].Docker = arm == "docker isolated"
			ws[0].Isolated = arm == "docker isolated"
			f.m.cfg.EpochReclaimDeletionCanaryID = canaryA
			_ = f.tickErr(t, ws...) // placement may refuse the mismatched fixture; pass 1 logs first
			if !strings.Contains(f.logs.String(), "epoch-reclaim-deletion-canary-ineligible worker="+canaryA) {
				t.Fatalf("no ineligible diagnostic: %s", f.logs.String())
			}
		})
	}
	t.Run("off", func(t *testing.T) {
		f, ws := canaryCluster(t, lmOpts{id: canaryA, layout: "new", promoted: true})
		f.tickAll(t, ws...)
		if strings.Contains(f.logs.String(), "epoch-reclaim-deletion-canary") {
			t.Fatal("diagnostic logged with the selector off")
		}
	})
}
