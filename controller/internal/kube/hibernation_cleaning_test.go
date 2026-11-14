package kube

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

// cleaningW is a worker whose api row is preparing or waking but whose hibernation cache
// prune attempt is unsettled, so the api projects "cleaning" (PRD #4206).
func cleaningW(proofUID string, seq int32) protocol.DesiredWorker {
	w := hibW(protocol.HibernationCleaning, "op-1")
	w.Hibernation.Cleanup = &protocol.HibernationCleanup{OpID: "clean-1", StopProofPodUID: proofUID, ReconcileSeq: seq}
	return w
}

// stoppedHibPod is a pod in phase Failed whose only container terminated with reason.
func stoppedHibPod(uid, reason string) *corev1.Pod {
	p := hibPod(uid, "x", false, false)
	p.Spec.Containers = []corev1.Container{{Name: "worker"}}
	p.Status.Phase = corev1.PodFailed
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "worker", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: reason}}}}
	return p
}

func noClusterWrites(t *testing.T, c interface{ Actions() []k8stesting.Action }) {
	t.Helper()
	for _, a := range c.Actions() {
		switch a.GetVerb() {
		case "create", "patch", "update", "delete", "delete-collection":
			if a.GetResource().Resource == "deployments" || a.GetVerb() == "delete" {
				t.Fatalf("cleaning wrote to the cluster: %s %s", a.GetVerb(), a.GetResource().Resource)
			}
		}
	}
}

func TestCleaningWritesNoDeploymentOrReplicas(t *testing.T) {
	// Both a preparing-like worker (replicas 1) and a waking-like one (replicas 0) project cleaning.
	for _, replicas := range []int32{0, 1} {
		m, client, rec := hibEnv(t, hibDeployment(replicas, "op-0"))
		if err := hibTick(t, m, cleaningW("", 1)); err != nil {
			t.Fatal(err)
		}
		if n := len(deploymentPatchBodies(client)); n != 0 {
			t.Fatalf("replicas=%d: %d deployment patches while cleaning", replicas, n)
		}
		if r := hibGet(t, client).Spec.Replicas; *r != replicas {
			t.Fatalf("replicas changed to %d", *r)
		}
		if len(rec.calls) != 0 {
			t.Fatalf("reported %v", rec.transitions())
		}
		noClusterWrites(t, client)
		assertNothingDeleted(t, client)
	}
}

func TestCleaningDoesNotCreateAMissingDeployment(t *testing.T) {
	m, client, rec := hibEnv(t)
	if err := hibTick(t, m, cleaningW("pod-uid-1", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AppsV1().Deployments(testConfig().Namespace).Get(context.Background(), "uzi-hw-w1", metav1.GetOptions{}); err == nil {
		t.Fatal("a Deployment was created while cleaning")
	}
	if len(rec.calls) != 0 {
		t.Fatalf("reported %v", rec.transitions())
	}
}

func TestCleaningStillEnsuresMissingPVCs(t *testing.T) {
	rec := &hibRec{fakeCordoner: &fakeCordoner{}}
	m, client := newMatWithCordoner(t, rec, hibDeployment(1, "op-0"))
	if err := hibTick(t, m, cleaningW("", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().PersistentVolumeClaims(testConfig().Namespace).Get(context.Background(), "uzi-hw-w1-data", metav1.GetOptions{}); err != nil {
		t.Fatalf("data pvc not ensured: %v", err)
	}
}

func TestCleaningStopProof(t *testing.T) {
	const uid = "pod-uid-1"
	cases := map[string]struct {
		pods   []runtime.Object
		proof  string
		report bool
	}{
		"present and stopped":        {pods: []runtime.Object{stoppedHibPod(uid, "Error")}, proof: uid, report: true},
		"absent uid":                 {proof: uid},
		"running pod":                {pods: []runtime.Object{hibPod(uid, "x", true, false)}, proof: uid},
		"container status unknown":   {pods: []runtime.Object{stoppedHibPod(uid, "ContainerStatusUnknown")}, proof: uid},
		"different pod uid stopped":  {pods: []runtime.Object{stoppedHibPod("pod-uid-2", "Error")}, proof: uid},
		"no proof uid asked":         {pods: []runtime.Object{stoppedHibPod(uid, "Error")}, proof: ""},
		"stopped match among others": {pods: []runtime.Object{stoppedHibPod(uid, "Error"), hibPod("pod-uid-3", "x", true, false)}, proof: uid, report: true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			objs := append([]runtime.Object{hibDeployment(0, "op-0")}, c.pods...)
			m, client, rec := hibEnv(t, objs...)
			if err := hibTick(t, m, cleaningW(c.proof, 4)); err != nil {
				t.Fatal(err)
			}
			if !c.report {
				if len(rec.calls) != 0 {
					t.Fatalf("unexpected report %v", rec.transitions())
				}
				return
			}
			if len(rec.calls) != 1 {
				t.Fatalf("calls = %v, want one cleanup_stopped", rec.transitions())
			}
			got, d := rec.calls[0], hibGet(t, client)
			want := protocol.HibernationTransition{OpID: "op-1", Transition: protocol.TransitionCleanupStopped, DeploymentUID: string(d.UID), PodUID: uid, CleanupOpID: "clean-1"}
			if got != want {
				t.Fatalf("report = %+v, want %+v", got, want)
			}
			noClusterWrites(t, client)
		})
	}
}

func TestCleaningNoProofWhenPodListIncomplete(t *testing.T) {
	m, client, rec := hibEnv(t, hibDeployment(0, "op-0"), stoppedHibPod("pod-uid-1", "Error"))
	o, err := m.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The pod list is unreadable on the tick itself (the roll snapshot re-reads it).
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("pods unavailable")
	})
	for i := range o {
		o[i].PodsKnown = false
	}
	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{cleaningW("pod-uid-1", 1)}, o); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("reported with an incomplete pod list: %v", rec.transitions())
	}
}

func TestCleaningStopProofReportedOncePerSeq(t *testing.T) {
	m, _, rec := hibEnv(t, hibDeployment(0, "op-0"), stoppedHibPod("pod-uid-1", "Error"))
	for i := 0; i < 3; i++ {
		if err := hibTick(t, m, cleaningW("pod-uid-1", 1)); err != nil {
			t.Fatal(err)
		}
	}
	if len(rec.calls) != 1 {
		t.Fatalf("seq 1 reports = %d, want 1", len(rec.calls))
	}
	if err := hibTick(t, m, cleaningW("pod-uid-1", 2)); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 2 {
		t.Fatalf("a new seq did not re-report: %d", len(rec.calls))
	}
}

func TestCleaningFailedReportIsRetried(t *testing.T) {
	m, _, rec := hibEnv(t, hibDeployment(0, "op-0"), stoppedHibPod("pod-uid-1", "Error"))
	rec.err = context.DeadlineExceeded
	if err := hibTick(t, m, cleaningW("pod-uid-1", 1)); err == nil {
		t.Fatal("expected the report error")
	}
	rec.err = nil
	if err := hibTick(t, m, cleaningW("pod-uid-1", 1)); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 2 {
		t.Fatalf("calls = %d, want the failed one then a retry", len(rec.calls))
	}
}

func TestUnknownHibernationStateStillFailsClosed(t *testing.T) {
	m, client, rec := hibEnv(t, hibDeployment(1, "op-0"))
	if err := hibTick(t, m, hibW("cleaning-v2", "op-1")); err != nil {
		t.Fatal(err)
	}
	if n := len(deploymentPatchBodies(client)); n != 0 || len(rec.calls) != 0 {
		t.Fatalf("unknown state acted: %d patches, %v", n, rec.transitions())
	}
}

func TestCleaningWorkerIsExcludedFromRollPlanning(t *testing.T) {
	// The cleaning worker's coverage peer is drifted: the cleaning one must not count as
	// serving coverage nor be patched, exactly like hibernated.
	sibling := deployedWorker("w2", 0, "the-old-releases-hash")
	rec := &hibRec{fakeCordoner: &fakeCordoner{}}
	m, client := newMatWithCordoner(t, rec, hibDeployment(1, "op-0"), sibling,
		pvcNix("w1", "20Gi"), pvcFor("w1", "data"), pvcNix("w2", "20Gi"), pvcFor("w2", "data"))
	w2 := evidence(protocol.DesiredWorker{ID: "w2", Template: "base", Size: "m"}, m5Now)
	w1 := evidence(cleaningW("", 1), m5Now)
	if err := hibTick(t, m, w1, w2); err != nil {
		t.Fatal(err)
	}
	for _, a := range client.Actions() {
		if p, ok := a.(k8stesting.PatchAction); ok && p.GetName() == "uzi-hw-w1" {
			t.Fatalf("cleaning worker was patched: %s", p.GetPatch())
		}
	}
}
