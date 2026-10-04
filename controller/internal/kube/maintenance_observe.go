package kube

import (
	"context"
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

// maintenanceObservation rechecks authoritative identities before fenced actions.
// Each read is bounded by the reconcile context/client timeout; no read is retried.
func (m *Materializer) maintenanceObservation(ctx context.Context, id, ns string, o reconcile.ObservedWorker) (reconcile.ObservedWorker, error) {
	deployments, err := m.client.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return o, fmt.Errorf("maintenance deployment: %w", err)
	}
	if deployments.Continue != "" {
		return o, fmt.Errorf("maintenance deployment list incomplete")
	}
	var d *appsv1.Deployment
	for i := range deployments.Items {
		if deployments.Items[i].Name == deploymentName(id) {
			d = &deployments.Items[i]
			break
		}
	}
	if d != nil {
		if owner, ours := IsOurs(d.Labels); !ours || owner != id {
			return o, fmt.Errorf("maintenance deployment is foreign")
		}
	}
	o.HasDeployment = d != nil
	o.DeploymentUID = ""
	o.DeploymentTerminating = false
	o.SpecHash, o.Generation = "", 0
	if o.HasDeployment {
		o.DeploymentUID = string(d.UID)
		o.DeploymentTerminating = d.DeletionTimestamp != nil
		o.SpecHash = d.Spec.Template.Annotations[AnnotationSpecHash]
		o.Generation, _ = strconv.ParseInt(d.Spec.Template.Annotations[AnnotationGeneration], 10, 64)
	}
	pvcs, err := m.client.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return o, fmt.Errorf("maintenance pvc: %w", err)
	}
	if pvcs.Continue != "" {
		return o, fmt.Errorf("maintenance pvc list incomplete")
	}
	var p *corev1.PersistentVolumeClaim
	for i := range pvcs.Items {
		if pvcs.Items[i].Name == dindDataPVCName(id) {
			p = &pvcs.Items[i]
			break
		}
	}
	if p != nil {
		if owner, ours := IsOurs(p.Labels); !ours || owner != id {
			return o, fmt.Errorf("maintenance pvc is foreign")
		}
	}
	o.HasDinDDataPVC = p != nil
	o.DinDPVCUID, o.DinDPVCPhase = "", ""
	o.DinDPVCLive = false
	o.DinDPVCCreatedAt = nil
	if o.HasDinDDataPVC {
		o.DinDPVCUID = string(p.UID)
		o.DinDPVCLive = p.DeletionTimestamp == nil
		o.DinDPVCPhase = string(p.Status.Phase)
		if o.DinDPVCLive {
			created := p.CreationTimestamp.Time
			o.DinDPVCCreatedAt = &created
		}
	}
	pods, err := m.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return o, fmt.Errorf("maintenance pods: %w", err)
	}
	if pods.Continue != "" {
		return o, fmt.Errorf("maintenance pod list incomplete")
	}
	o.PodsKnown, o.WorkerPodCount, o.ReadyPodUID = true, 0, ""
	var current []corev1.Pod
	for _, pod := range pods.Items {
		// Count worker-labelled pods and any pod mounting the data root, even if
		// labels changed. Terminating pods still hold the stop-before-delete gate.
		referencesData := false
		for _, volume := range pod.Spec.Volumes {
			if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == dindDataPVCName(id) {
				referencesData = true
			}
		}
		if pod.Labels[LabelWorkerID] != id && !referencesData {
			continue
		}
		o.WorkerPodCount++
		owner, ours := IsOurs(pod.Labels)
		if !ours || owner != id || !maintenancePodOwner(pod, deploymentName(id)) || !o.HasDeployment || o.DeploymentTerminating || pod.DeletionTimestamp != nil || pod.UID == "" || o.SpecHash == "" || pod.Annotations[AnnotationSpecHash] != o.SpecHash {
			continue
		}
		current = append(current, pod)
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				o.ReadyPodUID = string(pod.UID)
			}
		}
	}
	o.Roll = deriveRollHealth(current, o.SpecHash, "", m.now())
	if o.HasDeployment {
		o.Roll = deriveRollHealth(current, o.SpecHash, replicaFailureReason(d), m.now())
		o.Roll.TargetImage = workerImage(d)
	}
	return o, nil
}

// maintenancePodOwner rejects unmanaged and unrelated ReplicaSet pods. The
// template hash is the Deployment controller stamp shared with its ReplicaSet.
func maintenancePodOwner(p corev1.Pod, deployment string) bool {
	hash := p.Labels["pod-template-hash"]
	if hash == "" {
		return false
	}
	for _, owner := range p.OwnerReferences {
		if owner.APIVersion == "apps/v1" && owner.Kind == "ReplicaSet" && owner.UID != "" && owner.Controller != nil && *owner.Controller && owner.Name == deployment+"-"+hash {
			return true
		}
	}
	return false
}
