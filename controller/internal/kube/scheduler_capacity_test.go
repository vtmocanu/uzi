package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestSchedulerCapacityTickCorpus(t *testing.T) {
	type scenario struct {
		name                                string
		message                             string
		mutate                              func(*appsv1.Deployment, *corev1.Pod) []runtime.Object
		phase, reason                       string
		capacity, failPods                  bool
		noDeployment, noPod, reversePVCList bool
		configure                           func(*Materializer)
	}
	const cpu = "scheduler capacity: insufficient cpu"
	const memory = "scheduler capacity: insufficient memory"
	const both = "scheduler capacity: insufficient cpu and memory"
	oldClaim := func(kind string) *corev1.PersistentVolumeClaim {
		p := pvcFor(capacityPortableID, kind)
		p.CreationTimestamp = metav1.NewTime(m5Now.Add(-time.Hour))
		p.Status.Phase = corev1.ClaimPending
		return p
	}
	mount := func(p *corev1.Pod, kind string) {
		p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: kind, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: oldClaim(kind).Name}}})
	}
	cases := []scenario{
		{name: "cpu", capacity: true, reason: cpu},
		{name: "pod_only", noDeployment: true, phase: "rolling"},
		{name: "podless_gap", noPod: true, phase: "rolling"},
		{name: "podless_replica_failure", noPod: true, reason: "FailedCreate", mutate: func(d *appsv1.Deployment, _ *corev1.Pod) []runtime.Object {
			d.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue, Reason: "FailedCreate"}}
			return nil
		}},
		{name: "uint32_max", message: "0/4294967295 nodes are available: 4294967295 Insufficient cpu.", capacity: true, reason: cpu},
		{name: "memory", message: "0/2 nodes are available: 2 Insufficient memory.", capacity: true, reason: memory},
		{name: "both", message: "0/3 nodes are available: 2 Insufficient cpu, 2 Insufficient memory.", capacity: true, reason: both},
		{name: "preemption", message: "0/2 nodes are available: 2 Insufficient cpu. preemption: 0/2 nodes are available: 1 No preemption victims found for incoming pod, 1 Preemption is not helpful for scheduling.", capacity: true, reason: cpu},
		{name: "young", phase: "rolling", mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object {
			p.CreationTimestamp = metav1.NewTime(m5Now.Add(-time.Minute))
			return nil
		}},
		{name: "young_mounted_old_claim", capacity: true, reason: cpu, mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object {
			p.CreationTimestamp = metav1.NewTime(m5Now.Add(-time.Minute))
			mount(p, "nix")
			return []runtime.Object{oldClaim("nix")}
		}},
		{name: "all_mounted_old_claims", capacity: true, reason: cpu, mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object {
			mount(p, "nix")
			mount(p, "data")
			return []runtime.Object{oldClaim("data"), oldClaim("nix")}
		}},
		{name: "scheduled_storage", mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object {
			p.Spec.NodeName = "node"
			mount(p, "nix")
			return []runtime.Object{oldClaim("nix")}
		}},
		{name: "scheduled", mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object {
			p.Status.Conditions[0].Status = corev1.ConditionTrue
			return nil
		}},
		{name: "unmounted_old_claim", mutate: func(_ *appsv1.Deployment, _ *corev1.Pod) []runtime.Object { return []runtime.Object{oldClaim("nix")} }},
		{name: "young_unmounted_old_claim", mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object {
			p.CreationTimestamp = metav1.NewTime(m5Now.Add(-time.Minute))
			return []runtime.Object{oldClaim("data")}
		}},
		{name: "ephemeral_running", mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object {
			p.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{{Name: "debug", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
			return nil
		}},
		{name: "container_started", mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object {
			started := true
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "worker", Started: &started}}
			return nil
		}},
		{name: "ready_unknown", mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object {
			p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionUnknown})
			return nil
		}},
		{name: "wrong_scheduled_reason", mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object {
			p.Status.Conditions[0].Reason = "Unknown"
			return nil
		}},
		{name: "assigned", mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object { p.Spec.NodeName = "node"; return nil }},
		{name: "wrong_hash", phase: "rolling", mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object {
			p.Annotations[AnnotationSpecHash] = "old"
			return nil
		}},
		{name: "empty_hash", mutate: func(d *appsv1.Deployment, p *corev1.Pod) []runtime.Object {
			d.Spec.Template.Annotations[AnnotationSpecHash] = ""
			p.Annotations[AnnotationSpecHash] = ""
			return nil
		}},
		{name: "terminating_pod", mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object {
			ts := metav1.NewTime(m5Now)
			p.DeletionTimestamp = &ts
			p.Finalizers = []string{"test"}
			return nil
		}},
		{name: "terminating_deployment", mutate: func(d *appsv1.Deployment, _ *corev1.Pod) []runtime.Object {
			ts := metav1.NewTime(m5Now)
			d.DeletionTimestamp = &ts
			d.Finalizers = []string{"test"}
			return nil
		}},
		{name: "multiple_current", mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object {
			other := p.DeepCopy()
			other.Name += "other"
			return []runtime.Object{other}
		}},
		{name: "multiple_contradictory", mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object {
			other := p.DeepCopy()
			other.Name += "other"
			other.Spec.NodeName = "node"
			return []runtime.Object{other}
		}},
		{name: "failed_pod", mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object {
			p.Status.Phase = corev1.PodFailed
			return nil
		}},
		{name: "replica_failure", mutate: func(d *appsv1.Deployment, _ *corev1.Pod) []runtime.Object {
			d.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue, Reason: "FailedCreate"}}
			return nil
		}},
		{name: "replica_failure_empty_reason", mutate: func(d *appsv1.Deployment, _ *corev1.Pod) []runtime.Object {
			d.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue}}
			return nil
		}},
		{name: "duplicate_scheduled", mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object {
			p.Status.Conditions = append(p.Status.Conditions, p.Status.Conditions[0])
			return nil
		}},
		{name: "duplicate_ready_false", mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object {
			p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionFalse}, corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionFalse})
			return nil
		}},
		{name: "duplicate_containers_ready_false", mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object {
			p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{Type: corev1.ContainersReady, Status: corev1.ConditionFalse}, corev1.PodCondition{Type: corev1.ContainersReady, Status: corev1.ConditionFalse})
			return nil
		}},
		{name: "duplicate_deployment_first", mutate: func(d *appsv1.Deployment, _ *corev1.Pod) []runtime.Object {
			other := d.DeepCopy()
			other.Name = "aaa-duplicate"
			other.Spec.Template.Annotations[AnnotationSpecHash] = "old"
			return []runtime.Object{other}
		}},
		{name: "duplicate_deployment_last", mutate: func(d *appsv1.Deployment, _ *corev1.Pod) []runtime.Object {
			other := d.DeepCopy()
			other.Name = "zzz-duplicate"
			return []runtime.Object{other}
		}},
		{name: "missing_scheduled", mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object { p.Status.Conditions = nil; return nil }},
		{name: "ready_conflict", phase: "settled", mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object { ready(p, m5Now); return nil }},
		{name: "containers_ready_conflict", mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object {
			p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{Type: corev1.ContainersReady, Status: corev1.ConditionTrue})
			return nil
		}},
		{name: "container_failure", reason: "CrashLoopBackOff", mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object {
			waiting(p, "worker", "CrashLoopBackOff", 4, false)
			return nil
		}},
		{name: "container_running", mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "worker", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
			return nil
		}},
		{name: "container_exit", reason: "Error", mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object {
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "seed", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}}}}
			return nil
		}},
		{name: "container_restarts", mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "worker", RestartCount: 1}}
			return nil
		}},
		{name: "container_ready", mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "worker", Ready: true}}
			return nil
		}},
		{name: "last_exit", mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "worker", LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}}}
			return nil
		}},
		{name: "observation_failed", phase: "rolling", failPods: true},
		{name: "observation_failed_storage", failPods: true, mutate: func(_ *appsv1.Deployment, _ *corev1.Pod) []runtime.Object { return []runtime.Object{oldClaim("nix")} }},
		{name: "maintenance_readiness", phase: "settled", mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object {
			p.Status.Conditions = nil
			ready(p, m5Now)
			return nil
		}},
	}
	for _, status := range []corev1.ConditionStatus{corev1.ConditionFalse, corev1.ConditionTrue, corev1.ConditionUnknown} {
		for _, duplicate := range []bool{false, true} {
			cases = append(cases, scenario{
				name:     "sandbox_" + string(status) + map[bool]string{false: "_single", true: "_duplicate"}[duplicate],
				capacity: status == corev1.ConditionFalse && !duplicate,
				reason:   map[bool]string{true: cpu}[status == corev1.ConditionFalse && !duplicate],
				mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object {
					condition := corev1.PodCondition{Type: corev1.PodReadyToStartContainers, Status: status}
					p.Status.Conditions = append(p.Status.Conditions, condition)
					if duplicate {
						p.Status.Conditions = append(p.Status.Conditions, condition)
					}
					return nil
				},
			})
		}
	}
	for _, reverse := range []bool{false, true} {
		configure := func(m *Materializer) {
			if reverse {
				m.cfg.Namespace, m.cfg.DockerNamespace = "other-workers", testConfig().Namespace
			} else {
				m.cfg.DockerNamespace = "other-workers"
			}
		}
		order := map[bool]string{false: "_capacity_first", true: "_capacity_last"}[reverse]
		cases = append(cases, scenario{name: "other_namespace_current_pod" + order, configure: configure, phase: map[bool]string{false: "stuck", true: "rolling"}[reverse],
			mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object {
				other := p.DeepCopy()
				other.Namespace = "other-workers"
				other.Status.Phase = corev1.PodRunning
				other.Spec.NodeName = "assigned"
				return []runtime.Object{other}
			}})
		cases = append(cases, scenario{name: "other_namespace_claim_folded" + order, configure: configure,
			mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object {
				p.CreationTimestamp = metav1.NewTime(m5Now.Add(-time.Minute))
				mount(p, "nix")
				foreign := oldClaim("nix")
				foreign.Namespace = "other-workers"
				local := oldClaim("nix")
				local.Status.Phase = corev1.ClaimBound
				return []runtime.Object{foreign, local}
			}})
		cases = append(cases, scenario{name: "other_namespace_claim" + order, configure: configure,
			mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object {
				p.CreationTimestamp = metav1.NewTime(m5Now.Add(-time.Minute))
				mount(p, "nix")
				claim := oldClaim("nix")
				claim.Namespace = "other-workers"
				return []runtime.Object{claim}
			}})
	}
	for _, mounted := range []string{"nix", "data"} {
		for _, reverse := range []bool{false, true} {
			mounted, reverse := mounted, reverse
			order := "forward"
			if reverse {
				order = "reverse"
			}
			cases = append(cases, scenario{name: "mixed_claims_mounted_" + mounted + "_" + order, reversePVCList: reverse, mutate: func(_ *appsv1.Deployment, p *corev1.Pod) []runtime.Object {
				mount(p, mounted)
				claims := []runtime.Object{oldClaim("nix"), oldClaim("data")}
				if reverse {
					claims[0], claims[1] = claims[1], claims[0]
				}
				return claims
			}})
		}
	}
	for name, message := range map[string]string{
		"topology":               "0/2 nodes are available: 2 node(s) had volume node affinity conflict.",
		"affinity":               "0/2 nodes are available: 1 Insufficient cpu, 1 node(s) didn't match Pod's node affinity/selector.",
		"taint":                  "0/2 nodes are available: 1 Insufficient cpu, 1 node(s) had untolerated taint {dedicated: other}.",
		"unbound":                "0/2 nodes are available: 1 Insufficient cpu, 1 pod has unbound immediate PersistentVolumeClaims.",
		"unknown":                "0/2 nodes are available: 2 Unknown.",
		"malformed":              "0/2 nodes are available: two Insufficient cpu.",
		"truncated":              "0/2 nodes are available: 2 Insufficient cpu",
		"oversize":               strings.Repeat("x", 4097),
		"suffix":                 "0/2 nodes are available: 2 Insufficient cpu. extra",
		"unicode":                "0/2 nodes are available: 2 Insufficient cpu.é",
		"control":                "0/2 nodes are available: 2 Insufficient cpu.\n",
		"overflow":               "0/4294967296 nodes are available: 4294967296 Insufficient cpu.",
		"eleven_digits":          "0/00000000002 nodes are available: 2 Insufficient cpu.",
		"zero_nodes":             "0/0 nodes are available: 0 Insufficient cpu.",
		"zero_count":             "0/2 nodes are available: 0 Insufficient cpu.",
		"excess_count":           "0/2 nodes are available: 3 Insufficient cpu.",
		"uncovered":              "0/3 nodes are available: 1 Insufficient cpu, 1 Insufficient memory.",
		"duplicate_term":         "0/2 nodes are available: 1 Insufficient cpu, 1 Insufficient cpu.",
		"three_terms":            "0/3 nodes are available: 1 Insufficient cpu, 1 Insufficient memory, 1 Insufficient cpu.",
		"preemption_wrong_total": "0/2 nodes are available: 2 Insufficient cpu. preemption: 0/3 nodes are available: 3 No preemption victims found for incoming pod.",
		"preemption_uncovered":   "0/2 nodes are available: 2 Insufficient cpu. preemption: 0/2 nodes are available: 1 Preemption is not helpful for scheduling.",
		"preemption_duplicate":   "0/2 nodes are available: 2 Insufficient cpu. preemption: 0/2 nodes are available: 1 Preemption is not helpful for scheduling, 1 Preemption is not helpful for scheduling.",
		"preemption_truncated":   "0/2 nodes are available: 2 Insufficient cpu. preemption: 0/2 nodes are available: 2 Preemption is not helpful for scheduling",
		"preemption_unknown":     "0/2 nodes are available: 2 Insufficient cpu. preemption: 0/2 nodes are available: 2 Unknown.",
	} {
		cases = append(cases, scenario{name: name, message: message})
	}
	// Stable order for byte-for-byte producer fixtures, including the message map.
	sort.Slice(cases, func(i, j int) bool { return cases[i].name < cases[j].name })
	type entry struct {
		Name                 string          `json:"name"`
		ExpectedPhase        string          `json:"expected_phase"`
		ExpectedBlockingKind string          `json:"expected_blocking_kind,omitempty"`
		ExpectedFixedReason  string          `json:"expected_fixed_reason,omitempty"`
		Worker               json.RawMessage `json:"worker"`
	}
	var corpus []entry
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dep, pod := capacityPortableObjects()
			if tc.message != "" {
				pod.Status.Conditions[0].Message = tc.message
			}
			var objects []runtime.Object
			if !tc.noDeployment {
				objects = append(objects, dep)
			}
			if !tc.noPod {
				objects = append(objects, pod)
			}
			if tc.mutate != nil {
				objects = append(objects, tc.mutate(dep, pod)...)
			}
			wire := capacityPortableTickConfigured(t, objects, tc.failPods, tc.reversePVCList, tc.configure)
			var row map[string]any
			if err := json.Unmarshal(wire, &row); err != nil {
				t.Fatal(err)
			}
			phase := tc.phase
			if phase == "" {
				phase = "stuck"
			}
			kind := ""
			if tc.capacity {
				kind = "scheduler_capacity"
			}
			if phase == "absent" {
				if string(wire) != "null" {
					t.Fatalf("expected absent row: %s", wire)
				}
			} else {
				if row["phase"] != phase {
					t.Fatalf("phase want %s: %s", phase, wire)
				}
				if !tc.noDeployment && (!tc.failPods || row["pod_phase"] != "") && row["target_image"] != "example.com/agent:1.2.3" {
					t.Fatalf("TargetImage lost: %s", wire)
				}
				if tc.capacity {
					if row["blocking_kind"] != kind || row["blocking_reason"] != tc.reason {
						t.Fatalf("capacity cause: %s", wire)
					}
				} else if _, exists := row["blocking_kind"]; exists {
					t.Fatalf("unexpected cause: %s", wire)
				}
				if tc.reason != "" && row["blocking_reason"] != tc.reason {
					t.Fatalf("ordinary failure lost: %s", wire)
				}
				if strings.Contains(tc.name, "other_namespace_claim") && (row["pod_phase"] != "Pending" || row["blocking_container"] != nixPVCName(capacityPortableID) || row["blocking_reason"] != "stranded Pending past 10m0s bind-timeout; no auto-rollback") {
					t.Fatalf("foreign claim must retain ordinary storage failure: %s", wire)
				}
				if strings.Contains(tc.name, "mixed_claims") && row["blocking_container"] != nixPVCName(capacityPortableID) {
					t.Fatalf("/nix priority lost: %s", wire)
				}
			}
			corpus = append(corpus, entry{tc.name, phase, kind, tc.reason, wire})
		})
	}
	if t.Failed() {
		return
	}
	got, err := json.MarshalIndent(corpus, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	const path = "testdata/scheduler_capacity_tick_corpus.json"
	if os.Getenv("RECORD_CAPACITY_CORPUS") == "1" {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Tick corpus drift: regenerate with RECORD_CAPACITY_CORPUS=1 go test -count=1 ./internal/kube -run '^TestSchedulerCapacityTickCorpus$'")
	}
}

func TestSchedulerCapacityLivePodAcrossNamespaces(t *testing.T) {
	for _, tier := range []string{"docker", "isolated"} {
		for _, reverse := range []bool{false, true} {
			t.Run(tier+map[bool]string{false: "_capacity_first", true: "_capacity_last"}[reverse], func(t *testing.T) {
				dep, pod := capacityPortableObjects()
				other := pod.DeepCopy()
				other.Namespace = "other-workers"
				other.Status.Phase = corev1.PodRunning
				other.Spec.NodeName = "assigned"
				m, _ := newMatWithRecycle(t, nil, DrainPolicy{}, RecyclePolicy{}, io.Discard, dep, pod, other)
				first, second := dep.Namespace, other.Namespace
				if reverse {
					first, second = second, first
				}
				m.cfg.Namespace = first
				if tier == "docker" {
					m.cfg.DockerNamespace = second
				} else {
					m.cfg.IsolatedNamespace = second
				}
				observed, err := m.Observe(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if len(observed) != 1 || observed[0].SchedulerCapacityReason != "" || observed[0].Roll.BlockingKind != "" {
					t.Fatalf("second live current pod retained capacity evidence: %+v", observed)
				}
			})
		}
	}
}

func TestSchedulerCapacityDuplicateDeploymentNamespaces(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "capacity_first", true: "capacity_last"}[reverse], func(t *testing.T) {
			dep, pod := capacityPortableObjects()
			other := dep.DeepCopy()
			other.Namespace = "other-workers"
			other.Spec.Template.Annotations[AnnotationSpecHash] = "old"
			m, _ := newMatWithRecycle(t, nil, DrainPolicy{}, RecyclePolicy{}, io.Discard, dep, pod, other)
			if reverse {
				m.cfg.Namespace, m.cfg.DockerNamespace = other.Namespace, dep.Namespace
			} else {
				m.cfg.DockerNamespace = other.Namespace
			}
			observed, err := m.Observe(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range observed {
				if row.ID == capacityPortableID && (row.SchedulerCapacityReason != "" || row.Roll.BlockingKind != "") {
					t.Fatalf("ambiguous deployment retained capacity: %+v", row)
				}
			}
		})
	}
}

func TestSchedulerCapacityForeignContainerFault(t *testing.T) {
	dep, pod := capacityPortableObjects()
	dep.Namespace, pod.Namespace = "other-workers", "other-workers"
	fault := waiting(workerPod(capacityPortableID, "", time.Hour), "worker", "CrashLoopBackOff", 4, false)
	m, _ := newMatWithRecycle(t, nil, DrainPolicy{}, RecyclePolicy{}, io.Discard, fault, dep, pod)
	m.cfg.DockerNamespace = dep.Namespace
	observed, err := m.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(observed) != 1 || observed[0].Roll.BlockingKind != "" || observed[0].SchedulerCapacityReason != "" ||
		observed[0].Roll.BlockingReason != "CrashLoopBackOff" || observed[0].Roll.RestartCount != 4 {
		t.Fatalf("foreign container fault overwritten by capacity: %+v", observed)
	}
}

func TestSchedulerCapacityPodOnlyAndGap(t *testing.T) {
	dep, pod := capacityPortableObjects()
	for name, objects := range map[string][]runtime.Object{"pod_only": {pod}, "no_pod": {dep}} {
		t.Run(name, func(t *testing.T) {
			wire := capacityPortableTick(t, objects, false)
			var row map[string]any
			if err := json.Unmarshal(wire, &row); err != nil {
				t.Fatal(err)
			}
			if _, exists := row["blocking_kind"]; exists {
				t.Fatalf("unexpected cause: %s", wire)
			}
		})
	}
}
