package kube

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

// #3852: telemetry names are exact; neither missing nor unknown names authorize
// destruction of Docker's durable /data, including after an old drain deadline.
func TestDockerDataPressureDefersWithoutDrainMutation(t *testing.T) {
	volumes := [][]string{{"data"}, {"data", "dind"}, {"dind"}, nil, {}, {"unknown"}, {"NIX"}}
	for i, volumes := range volumes {
		for _, mode := range []string{"idle", "busy", "busy-past-deadline", "cooldown", "force", "unknown-target"} {
			t.Run(fmt.Sprintf("%d/%s", i, mode), func(t *testing.T) {
				ctx := context.Background()
				cfg := dockerTestConfig()
				if mode == "unknown-target" {
					cfg.RollTargetVersion = ""
				}
				w := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", Docker: true, DiskPressure: true, DiskPressureVolumes: volumes}
				if strings.HasPrefix(mode, "busy") {
					w.Busy = true
				}
				if mode == "busy-past-deadline" {
					since := m5Now.Add(-48 * time.Hour)
					w.DrainingSince, w.DrainingReason = &since, token("pressure_recycle")
				}
				spec := testSpec(t, w.Template, w.Size)
				dep := RenderDeployment(cfg, w, spec)
				dep.UID = "old-deployment"
				objects := []runtime.Object{dep}
				for _, p := range RenderPVCs(cfg, w, spec) {
					p.UID = types.UID(p.Name)
					p.Status.Phase = corev1.ClaimBound
					if mode == "cooldown" {
						p.CreationTimestamp = metav1.NewTime(m5Now.Add(-time.Minute))
					}
					objects = append(objects, p)
				}
				client := enforcingClient(t, objects...)
				cordon := &pressureReasonCordoner{}
				var logs strings.Builder
				m := New(client, cfg, testResolver(t), cordon,
					DrainPolicy{Deadline: time.Hour, ForceRoll: mode == "force"},
					RecyclePolicy{Enabled: true, Cooldown: time.Hour}, slog.New(slog.NewTextHandler(&logs, nil)))
				m.now = func() time.Time { return m5Now }
				obs, err := m.Observe(ctx)
				if err != nil {
					t.Fatal(err)
				}
				client.ClearActions()
				if err = m.Reconcile(ctx, []protocol.DesiredWorker{w}, obs); err != nil {
					t.Fatal(err)
				}
				for _, a := range client.Actions() {
					if a.GetVerb() == "delete" || a.GetVerb() == "patch" || a.GetVerb() == "update" || a.GetVerb() == "create" {
						t.Fatalf("deferred pressure mutated %s/%s", a.GetVerb(), a.GetResource().Resource)
					}
				}
				if len(cordon.calls) != 0 || len(cordon.clearCalls) != 0 || len(cordon.requested) != 0 || len(cordon.released) != 0 {
					t.Fatalf("deferral changed drain: %+v", cordon)
				}
				if !strings.Contains(logs.String(), "disk-recycle-skipped-docker-data worker=w1") ||
					strings.Contains(logs.String(), "disk-recycle-skipped-cooldown") {
					t.Fatalf("wrong deferral diagnostic: %s", logs.String())
				}
				for _, name := range []string{nixPVCName(w.ID), dataPVCName(w.ID), dindDataPVCName(w.ID)} {
					p, err := client.CoreV1().PersistentVolumeClaims(cfg.DockerNamespace).Get(ctx, name, metav1.GetOptions{})
					if err != nil || p.UID != types.UID(name) {
						t.Fatalf("changed identity %s: %v", name, err)
					}
				}
			})
		}
	}
}

func TestDockerRecycleHelperDataFloor(t *testing.T) {
	for _, mode := range []string{"idle", "busy", "deadline", "force", "custody"} {
		for _, selector := range []string{"empty", "data", "combined"} {
			t.Run(mode+"/"+selector, func(t *testing.T) {
				ctx := context.Background()
				cfg := dockerTestConfig()
				w := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", Docker: true, Busy: mode != "idle", CustodyHeld: mode == "custody"}
				if mode == "deadline" {
					since := m5Now.Add(-48 * time.Hour)
					w.DrainingSince = &since
				}
				spec := testSpec(t, w.Template, w.Size)
				dep := RenderDeployment(cfg, w, spec)
				objects := []runtime.Object{dep}
				for _, p := range RenderPVCs(cfg, w, spec) {
					p.UID = types.UID(p.Name)
					objects = append(objects, p)
				}
				client := enforcingClient(t, objects...)
				cordon := &fakeCordoner{}
				m := New(client, cfg, testResolver(t), cordon, DrainPolicy{Deadline: time.Hour, ForceRoll: mode == "force"}, RecyclePolicy{Enabled: true}, slog.Default())
				m.now = func() time.Time { return m5Now }
				obs, err := m.Observe(ctx)
				if err != nil {
					t.Fatal(err)
				}
				vols := recycleVolumes{Data: selector != "empty", Nix: selector == "combined"}
				client.ClearActions()
				if err = m.recycleWorkerVolumes(ctx, w, obs[0], cfg.DockerNamespace, vols); err != nil {
					t.Fatal(err)
				}
				if selector != "combined" && (len(client.Actions()) != 0 || len(cordon.calls) != 0 || len(cordon.clearCalls) != 0) {
					t.Fatalf("empty filtered selection mutated worker: %v %+v", client.Actions(), cordon)
				}
				del := deletedSet(client)
				if del["persistentvolumeclaims/"+dataPVCName(w.ID)] || del["persistentvolumeclaims/"+dindDataPVCName(w.ID)] {
					t.Fatal("helper deleted durable data")
				}
				if selector == "combined" && (mode == "idle" || mode == "force" || mode == "deadline") && !del["persistentvolumeclaims/"+nixPVCName(w.ID)] {
					t.Fatal("permitted nix recycle did not progress")
				}
				p, err := client.CoreV1().PersistentVolumeClaims(cfg.DockerNamespace).Get(ctx, dataPVCName(w.ID), metav1.GetOptions{})
				if err != nil || p.UID != types.UID(dataPVCName(w.ID)) {
					t.Fatalf("data identity changed: %v", err)
				}
			})
		}
	}
}

func TestDockerMixedPressureBusyDrainsOnlyForNix(t *testing.T) {
	for _, causes := range [][]string{{"nix"}, {"nix", "data"}} {
		t.Run(strings.Join(causes, "+"), func(t *testing.T) {
			ctx := context.Background()
			cfg := dockerTestConfig()
			w := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", Docker: true, Busy: true, DiskPressure: true, DiskPressureVolumes: causes}
			spec := testSpec(t, w.Template, w.Size)
			dep := RenderDeployment(cfg, w, spec)
			objects := []runtime.Object{dep}
			for _, p := range RenderPVCs(cfg, w, spec) {
				objects = append(objects, p)
			}
			client := enforcingClient(t, objects...)
			cordon := &pressureReasonCordoner{}
			m := New(client, cfg, testResolver(t), cordon, DrainPolicy{Deadline: time.Hour}, RecyclePolicy{Enabled: true}, slog.Default())
			m.now = func() time.Time { return m5Now }
			obs, err := m.Observe(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err = m.Reconcile(ctx, []protocol.DesiredWorker{w}, obs); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(cordon.requested, []string{"pressure_recycle"}) || len(cordon.clearCalls) != 0 {
				t.Fatalf("wrong drain: %+v", cordon)
			}
			if del := deletedSet(client); len(del) != 0 {
				t.Fatalf("busy deleted %v", del)
			}
			// The accepted independent-maintenance deadline may interrupt the busy
			// worker for nix, but the same pressure drain must never reclaim /data.
			since := m5Now.Add(-2 * time.Hour)
			w.DrainingSince, w.DrainingReason = &since, token("pressure_recycle")
			client.ClearActions()
			obs, err = m.Observe(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err = m.Reconcile(ctx, []protocol.DesiredWorker{w}, obs); err != nil {
				t.Fatal(err)
			}
			del := deletedSet(client)
			if !del["persistentvolumeclaims/"+nixPVCName(w.ID)] ||
				del["persistentvolumeclaims/"+dataPVCName(w.ID)] ||
				del["persistentvolumeclaims/"+dindDataPVCName(w.ID)] {
				t.Fatalf("mixed pressure must recycle only nix: %v", del)
			}
			if !slices.Equal(cordon.requested, []string{"pressure_recycle"}) || len(cordon.clearCalls) != 0 || len(cordon.released) != 0 {
				t.Fatalf("nix recycle changed drain ownership: %+v", cordon)
			}
		})
	}
}

func TestDockerDataPressureRequestedDinDAdvancesReady(t *testing.T) {
	ctx := context.Background()
	cfg := dockerTestConfig()
	w := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", Docker: true,
		DiskPressure: true, DiskPressureVolumes: []string{"data", "dind"}}
	spec := testSpec(t, w.Template, w.Size)
	dep := RenderDeployment(cfg, w, spec)
	dep.UID = "old-deployment"
	objects := []runtime.Object{dep}
	for _, p := range RenderPVCs(cfg, w, spec) {
		p.UID = types.UID(p.Name)
		p.Status.Phase = corev1.ClaimBound
		objects = append(objects, p)
	}
	client := enforcingClient(t, objects...)
	f := &maintenanceFake{op: &protocol.DindMaintenance{ID: "operation", Nonce: "nonce",
		RegisterNonce: "register", Phase: "requested", DeploymentUID: string(dep.UID),
		PVCUID: dindDataPVCName(w.ID)}}
	w.DindMaintenance = f.op
	var logs strings.Builder
	m := New(client, cfg, testResolver(t), f, DrainPolicy{Deadline: time.Hour},
		RecyclePolicy{Enabled: true}, slog.New(slog.NewTextHandler(&logs, nil)))
	obs, err := m.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	client.ClearActions()
	if err = m.Reconcile(ctx, []protocol.DesiredWorker{w}, obs); err != nil {
		t.Fatal(err)
	}
	if f.op.Phase != "ready" || !slices.Equal(f.calls, []string{"ready"}) {
		t.Fatalf("data pressure starved requested DinD: %+v %v", f.op, f.calls)
	}
	if !strings.Contains(logs.String(), "disk-recycle-skipped-docker-data worker=w1") {
		t.Fatalf("DinD early return lost data deferral: %s", logs.String())
	}
	for _, a := range client.Actions() {
		if a.GetVerb() != "list" && a.GetVerb() != "get" {
			t.Fatalf("ready transition mutated Kubernetes: %v", a)
		}
	}
	p, err := client.CoreV1().PersistentVolumeClaims(cfg.DockerNamespace).Get(ctx, dataPVCName(w.ID), metav1.GetOptions{})
	if err != nil || p.UID != types.UID(dataPVCName(w.ID)) {
		t.Fatalf("data identity changed: %v", err)
	}
}
