package kube

import (
	"strconv"
	"strings"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
	corev1 "k8s.io/api/core/v1"
)

// schedulerCapacityMessage accepts the complete standard scheduler grammar, never
// a substring. Work is bounded by 4096 bytes and two terms in each clause.
func schedulerCapacityMessage(message string) string {
	if len(message) == 0 || len(message) > 4096 {
		return ""
	}
	for i := range len(message) {
		if message[i] < 32 || message[i] > 126 {
			return ""
		}
	}
	parts := strings.Split(message, ". preemption: ")
	if len(parts) > 2 {
		return ""
	}
	first := parts[0]
	if len(parts) == 1 {
		if !strings.HasSuffix(first, ".") {
			return ""
		}
		first = strings.TrimSuffix(first, ".")
	}
	n, terms, ok := schedulerClause(first, 0, []string{"Insufficient cpu", "Insufficient memory"})
	if !ok {
		return ""
	}
	if len(parts) == 2 {
		if !strings.HasSuffix(parts[1], ".") {
			return ""
		}
		if _, _, ok := schedulerClause(strings.TrimSuffix(parts[1], "."), n, []string{"No preemption victims found for incoming pod", "Preemption is not helpful for scheduling"}); !ok {
			return ""
		}
	}
	if terms[0] && terms[1] {
		return "scheduler capacity: insufficient cpu and memory"
	}
	if terms[0] {
		return "scheduler capacity: insufficient cpu"
	}
	return "scheduler capacity: insufficient memory"
}

func schedulerNumber(s string) (uint32, bool) {
	if len(s) == 0 || len(s) > 10 {
		return 0, false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(s, 10, 32)
	return uint32(n), err == nil && n > 0
}

func schedulerClause(s string, expected uint32, names []string) (uint32, [2]bool, bool) {
	var seen [2]bool
	if !strings.HasPrefix(s, "0/") {
		return 0, seen, false
	}
	head, tail, ok := strings.Cut(s[2:], " nodes are available: ")
	if !ok {
		return 0, seen, false
	}
	n, ok := schedulerNumber(head)
	if !ok || (expected != 0 && n != expected) {
		return 0, seen, false
	}
	terms := strings.Split(tail, ", ")
	if len(terms) < 1 || len(terms) > 2 {
		return 0, seen, false
	}
	var sum uint64
	for _, term := range terms {
		count, name, ok := strings.Cut(term, " ")
		c, valid := schedulerNumber(count)
		if !ok || !valid || c > n {
			return 0, seen, false
		}
		index := -1
		for i, allowed := range names {
			if name == allowed {
				index = i
			}
		}
		if index < 0 || seen[index] {
			return 0, seen, false
		}
		seen[index] = true
		sum += uint64(c)
	}
	return n, seen, sum >= uint64(n)
}

// schedulerCapacityObservation collects evidence across all configured namespace
// folds for one Observe call. Ordinary roll derivation remains namespace-local.
type schedulerCapacityObservation struct {
	pods                map[string][]corev1.Pod
	blocked             map[string]bool
	deploymentNamespace map[string]string
	podListFailed       bool
}

// recordSchedulerCapacity adds evidence without changing ordinary pod selection or
// failure classification. Only a unique live current pod can justify a typed cause.
func recordSchedulerCapacity(o *reconcile.ObservedWorker, pods []corev1.Pod, deploymentNamespace string, replicaFailure bool) {
	if !o.HasDeployment || o.DeploymentTerminating || o.SpecHash == "" || replicaFailure || o.Namespace != deploymentNamespace {
		return
	}
	var current *corev1.Pod
	for i := range pods {
		p := &pods[i]
		if p.Annotations[AnnotationSpecHash] != o.SpecHash || p.DeletionTimestamp != nil || p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodSucceeded {
			continue
		}
		if current != nil {
			return
		}
		current = p
	}
	if current == nil || current.Namespace != deploymentNamespace || current.Spec.NodeName != "" || current.Status.Phase != corev1.PodPending {
		return
	}
	for _, statuses := range [][]corev1.ContainerStatus{current.Status.InitContainerStatuses, current.Status.ContainerStatuses, current.Status.EphemeralContainerStatuses} {
		for _, s := range statuses {
			if s.RestartCount != 0 || s.Ready || s.Started != nil && *s.Started || s.State.Running != nil || s.State.Terminated != nil || s.LastTerminationState.Terminated != nil || s.LastTerminationState.Running != nil {
				return
			}
			if s.State.Waiting != nil && s.State.Waiting.Reason != "" && s.State.Waiting.Reason != "ContainerCreating" && s.State.Waiting.Reason != "PodInitializing" {
				return
			}
		}
	}
	scheduled := 0
	seenReady := map[corev1.PodConditionType]bool{}
	reason := ""
	for _, c := range current.Status.Conditions {
		switch c.Type {
		case corev1.PodScheduled:
			scheduled++
			if c.Status != corev1.ConditionFalse || c.Reason != "Unschedulable" {
				return
			}
			reason = schedulerCapacityMessage(c.Message)
		case corev1.PodReady, corev1.ContainersReady, corev1.PodReadyToStartContainers:
			if seenReady[c.Type] || c.Status != corev1.ConditionFalse {
				return
			}
			seenReady[c.Type] = true
		}
	}
	if scheduled != 1 || reason == "" {
		return
	}
	o.SchedulerCapacityReason = reason
	o.SchedulerMountedClaims = map[string]bool{}
	for _, v := range current.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			o.SchedulerMountedClaims[current.Namespace+"/"+v.PersistentVolumeClaim.ClaimName] = true
		}
	}
	if o.Roll.Phase == protocol.PhaseStuck {
		o.Roll.BlockingKind = "scheduler_capacity"
		o.Roll.BlockingReason = reason
		o.Roll.BlockingContainer = ""
	}
}
