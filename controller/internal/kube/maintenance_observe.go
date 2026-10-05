package kube

import (
	"context"
	"fmt"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

// maintenanceObservation rechecks authoritative identities before fenced actions.
// The entire scan has a 30-second deadline; failed pages are never restarted.
func (m *Materializer) maintenanceObservation(ctx context.Context, id, ns string, o reconcile.ObservedWorker, operation ...*protocol.DindMaintenance) (reconcile.ObservedWorker, error) {
	ctx, cancel := context.WithTimeout(markMaintenanceRead(ctx), 30*time.Second)
	defer cancel()
	var d *appsv1.Deployment
	err := maintenancePages(ctx, "deployments", func(options metav1.ListOptions) (int, string, error) {
		page, err := m.client.AppsV1().Deployments(ns).List(ctx, options)
		if err != nil {
			return 0, "", err
		}
		if len(page.Items) <= maintenancePageSize {
			for i := range page.Items {
				if page.Items[i].Name == deploymentName(id) {
					d = page.Items[i].DeepCopy()
				}
			}
		}
		return len(page.Items), page.Continue, nil
	})
	if err != nil {
		return o, err
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
	var p *corev1.PersistentVolumeClaim
	err = maintenancePages(ctx, "pvcs", func(options metav1.ListOptions) (int, string, error) {
		page, err := m.client.CoreV1().PersistentVolumeClaims(ns).List(ctx, options)
		if err != nil {
			return 0, "", err
		}
		if len(page.Items) <= maintenancePageSize {
			for i := range page.Items {
				if page.Items[i].Name == dindDataPVCName(id) {
					p = page.Items[i].DeepCopy()
				}
			}
		}
		return len(page.Items), page.Continue, nil
	})
	if err != nil {
		return o, err
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
	var pods []corev1.Pod
	err = maintenancePages(ctx, "pods", func(options metav1.ListOptions) (int, string, error) {
		page, err := m.client.CoreV1().Pods(ns).List(ctx, options)
		if err != nil {
			return 0, "", err
		}
		if len(page.Items) <= maintenancePageSize {
			for i := range page.Items {
				pod := &page.Items[i]
				relevant := pod.Labels[LabelWorkerID] == id
				for _, volume := range pod.Spec.Volumes {
					if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == dindDataPVCName(id) {
						relevant = true
					}
				}
				if relevant {
					pods = append(pods, *pod.DeepCopy())
				}
			}
		}
		return len(page.Items), page.Continue, nil
	})
	if err != nil {
		return o, err
	}
	// Only replacement readiness needs ReplicaSet ancestry. Denial withholds
	// readiness proof while preserving the base observations used for rebuilding.
	sets := map[string]appsv1.ReplicaSet{}
	replacement := false
	if len(operation) != 0 && operation[0] != nil {
		op := operation[0]
		replacement = o.HasDeployment && !o.DeploymentTerminating && o.DeploymentUID != "" && o.DeploymentUID != op.DeploymentUID &&
			(op.Phase == "requested" || (op.Phase == "recycling" && o.DinDPVCLive && o.DinDPVCUID != "" && o.DinDPVCUID != op.PVCUID && o.DinDPVCPhase == string(corev1.ClaimBound)))
	}
	if replacement {
		err = maintenancePages(ctx, "replicasets", func(options metav1.ListOptions) (int, string, error) {
			page, err := m.client.AppsV1().ReplicaSets(ns).List(ctx, options)
			if err != nil {
				return 0, "", err
			}
			if len(page.Items) <= maintenancePageSize {
				for i := range page.Items {
					rs := &page.Items[i]
					owner := metav1.GetControllerOf(rs)
					if rs.DeletionTimestamp == nil && owner != nil && owner.APIVersion == "apps/v1" && owner.Kind == "Deployment" && owner.Name == d.Name && owner.UID == d.UID {
						// Keep identity only; historical pod templates need not stay in memory.
						sets[rs.Name] = appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: rs.Name, UID: rs.UID}}
					}
				}
			}
			return len(page.Items), page.Continue, nil
		})
		if err != nil {
			clear(sets)
			m.log.Info("DinD replacement readiness ancestry unavailable", "worker_id", id, "error", err)
		}
	}
	o.PodsKnown, o.WorkerPodCount, o.ReadyPodUID = true, 0, ""
	var current []corev1.Pod
	for _, pod := range pods {
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
		if !ours || owner != id || !maintenancePodOwner(pod, sets) || !o.HasDeployment || o.DeploymentTerminating || pod.DeletionTimestamp != nil || pod.UID == "" || o.SpecHash == "" || pod.Annotations[AnnotationSpecHash] != o.SpecHash {
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
	if err := ctx.Err(); err != nil {
		return o, err
	}
	return o, nil
}

// maintenancePodOwner resolves the Pod controller by both ReplicaSet name and UID.
// sets contains only live ReplicaSets controlled by the current Deployment UID.
func maintenancePodOwner(p corev1.Pod, sets map[string]appsv1.ReplicaSet) bool {
	owner := metav1.GetControllerOf(&p)
	if owner == nil || owner.APIVersion != "apps/v1" || owner.Kind != "ReplicaSet" || owner.UID == "" {
		return false
	}
	rs, ok := sets[owner.Name]
	return ok && rs.UID == owner.UID
}
