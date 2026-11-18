package kube

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

// #4457: waking a hibernated worker must not scale its Deployment 0 -> 1 while one of its
// claims is Terminating; the pod would sit Pending on the doomed volume.

type wakeClaimEnv struct {
	cfg    RenderConfig
	w      protocol.DesiredWorker
	ns     string
	client *fake.Clientset
	m      *Materializer
}

// wakeClaimSetup seeds a waking worker whose Deployment sits at explicit replicas 0 with
// every claim live at its desired size, except that mutate may alter the claim set.
// newPlacement toggles the placement feature; docker selects a Docker worker.
func wakeClaimSetup(t *testing.T, newPlacement, docker bool, mutate func(claims []*corev1.PersistentVolumeClaim) []runtime.Object) *wakeClaimEnv {
	t.Helper()
	cfg := placementConfig()
	if !newPlacement {
		cfg = globalOnly(cfg)
	}
	w := desired("w1")
	if docker {
		cfg.DockerNamespace = "uzi-workers-docker"
		cfg.DinDImage = "docker:28-dind-rootless@sha256:deadbeef"
		w = desiredDocker("w1")
	}
	w = evidence(w, m5Now)
	w.Hibernation = &protocol.Hibernation{State: protocol.HibernationWaking, OpID: "wake-op"}
	ns := cfg.Namespace
	if docker {
		ns = cfg.DockerNamespace
	}

	var claims []*corev1.PersistentVolumeClaim
	for _, o := range livePVCs(t, cfg, w, newPlacement) {
		claims = append(claims, o.(*corev1.PersistentVolumeClaim))
	}
	objs := mutate(claims)
	dep := liveDeployment(t, cfg, w, newPlacement)
	zero := int32(0)
	dep.Spec.Replicas = &zero
	dep.Annotations = map[string]string{annotationHibernationOp: "wake-op"}
	objs = append(objs, dep)

	m, client := npMat(t, cfg, objs...)
	return &wakeClaimEnv{cfg: cfg, w: w, ns: ns, client: client, m: m}
}

func (e *wakeClaimEnv) replicas(t *testing.T) int32 {
	t.Helper()
	d, err := e.client.AppsV1().Deployments(e.ns).Get(context.Background(), deploymentName(e.w.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if d.Spec.Replicas == nil {
		t.Fatal("replicas unset")
	}
	return *d.Spec.Replicas
}

func (e *wakeClaimEnv) tick(t *testing.T) {
	t.Helper()
	if err := npTick(t, e.m, e.w); err != nil {
		t.Fatalf("tick: %v", err)
	}
}

func (e *wakeClaimEnv) claim(t *testing.T, name string) (*corev1.PersistentVolumeClaim, bool) {
	t.Helper()
	p, err := e.client.CoreV1().PersistentVolumeClaims(e.ns).Get(context.Background(), name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("get pvc %s: %v", name, err)
	}
	return p, true
}

func TestWakeHeldWhileAClaimIsTerminating(t *testing.T) {
	for _, mode := range []string{"newPlacement-off", "newPlacement-on"} {
		for _, tc := range []struct {
			name   string
			kind   string
			docker bool
		}{
			{"data", "data", false},
			{"nix", "nix", false},
			{"dind-data", "dind-data", true},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				on := mode == "newPlacement-on"
				claimName := map[string]string{"data": dataPVCName("w1"), "nix": nixPVCName("w1"), "dind-data": dindDataPVCName("w1")}[tc.kind]
				e := wakeClaimSetup(t, on, tc.docker, func(claims []*corev1.PersistentVolumeClaim) []runtime.Object {
					var out []runtime.Object
					found := false
					for _, p := range claims {
						if p.Name == claimName {
							now := metav1.Now()
							p.DeletionTimestamp = &now
							found = true
						}
						out = append(out, p)
					}
					if !found {
						t.Fatalf("claim %s not rendered for this worker", claimName)
					}
					return out
				})

				e.client.ClearActions()
				e.tick(t)
				if n := replicaPatches(e.client); n != 0 {
					t.Fatalf("%d replica patches while %s is Terminating, want none", n, claimName)
				}
				if got := e.replicas(t); got != 0 {
					t.Fatalf("replicas = %d while %s is Terminating, want 0", got, claimName)
				}

				// The claim finishes terminating (gone); the controller recreates it and wakes.
				if err := e.client.CoreV1().PersistentVolumeClaims(e.ns).Delete(context.Background(), claimName, metav1.DeleteOptions{}); err != nil {
					t.Fatalf("delete claim: %v", err)
				}
				e.client.ClearActions()
				e.tick(t)
				if e.replicas(t) != 1 {
					e.tick(t)
				}
				if got := e.replicas(t); got != 1 {
					t.Fatalf("replicas = %d after the claim was recreated, want 1", got)
				}
				p, ok := e.claim(t, claimName)
				if !ok {
					t.Fatalf("claim %s was not recreated", claimName)
				}
				if p.DeletionTimestamp != nil {
					t.Fatalf("recreated claim %s is still terminating", claimName)
				}
			})
		}
	}
}

func TestWakeProceedsWhenAllClaimsLive(t *testing.T) {
	for _, on := range []bool{false, true} {
		name := "newPlacement-off"
		if on {
			name = "newPlacement-on"
		}
		t.Run(name, func(t *testing.T) {
			e := wakeClaimSetup(t, on, false, func(claims []*corev1.PersistentVolumeClaim) []runtime.Object {
				var out []runtime.Object
				for _, p := range claims {
					out = append(out, p)
				}
				return out
			})
			e.client.ClearActions()
			e.tick(t)
			if got := e.replicas(t); got != 1 {
				t.Fatalf("replicas = %d on the first tick with every claim live, want 1", got)
			}
		})
	}
}

func TestWakeProceedsWhenAClaimIsCreatedThisTick(t *testing.T) {
	e := wakeClaimSetup(t, false, false, func(claims []*corev1.PersistentVolumeClaim) []runtime.Object {
		var out []runtime.Object
		for _, p := range claims {
			if p.Name == dataPVCName("w1") {
				continue // absent: ensurePVCs creates it this tick
			}
			out = append(out, p)
		}
		return out
	})
	e.client.ClearActions()
	e.tick(t)
	if _, ok := e.claim(t, dataPVCName("w1")); !ok {
		t.Fatal("absent /data claim was not created")
	}
	if got := e.replicas(t); got != 1 {
		t.Fatalf("replicas = %d on the tick that created the claim, want 1", got)
	}
}

// Control: an undersized but live /nix claim is a size-drift matter, not a doomed volume,
// so it must not hold the wake.
func TestWakeNotHeldByUndersizedLiveNixClaim(t *testing.T) {
	e := wakeClaimSetup(t, false, false, func(claims []*corev1.PersistentVolumeClaim) []runtime.Object {
		var out []runtime.Object
		for _, p := range claims {
			if p.Name == nixPVCName("w1") {
				p.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}
			}
			out = append(out, p)
		}
		return out
	})
	e.client.ClearActions()
	e.tick(t)
	if got := e.replicas(t); got != 1 {
		t.Fatalf("replicas = %d with an undersized live nix claim, want 1", got)
	}
}
