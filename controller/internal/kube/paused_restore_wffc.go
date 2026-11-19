package kube

// Bind-timeout deferral for a paced restore (issue #4468).
//
// A worker deliberately held at zero replicas under an open roll reservation has no pod,
// so its WaitForFirstConsumer claims cannot bind. flagStrandedPVCs would read that as a
// stranded volume after pvcBindTimeout. timeoutDeferredClaims names the claims for which
// that reading is provably wrong; every doubt (incomplete list, odd shape, unreadable
// StorageClass) leaves the claim subject to the timeout.

import (
	"context"

	corev1 "k8s.io/api/core/v1"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

// timeoutDeferredClaims returns the "namespace/name" keys of o's aged Pending claims whose
// bind-timeout is deferred, or nil. classCache memoises StorageClass verdicts for the pass.
func (m *Materializer) timeoutDeferredClaims(ctx context.Context, o *reconcile.ObservedWorker, ev *schedulerCapacityObservation, deploymentCount int, classCache map[string]bool) map[string]bool {
	if len(o.SchedulerPendingClaims) == 0 || deploymentCount != 1 || ev.podListFailed || len(ev.incomplete) > 0 {
		return nil
	}
	desired, ok := reconcile.DesiredWorkers(ctx)
	if !ok {
		return nil
	}
	var w *protocol.DesiredWorker
	for i := range desired {
		if desired[i].ID == o.ID {
			w = &desired[i]
			break
		}
	}
	d := ev.deployments[o.ID]
	if w == nil || d == nil || d.DeletionTimestamp != nil || d.Spec.Replicas == nil || *d.Spec.Replicas != 0 || d.Annotations[annotationHibernationOp] != "" {
		return nil
	}
	ns := ev.deploymentNamespace[o.ID]
	marker, err := decodeRoll(d.Annotations[annotationRoll])
	if err != nil || marker == nil || marker.Completed || !rollOwnerMatches(marker, *w, ns) {
		return nil
	}
	if c := ev.dataClaims[ns+"/"+dataPVCName(o.ID)]; c != nil && c.Annotations[annotationRoll] != "" {
		cm, err := decodeRoll(c.Annotations[annotationRoll])
		if err != nil || cm == nil || !rollOwnerMatches(cm, *w, ns) || c.DeletionTimestamp != nil {
			return nil
		}
	}
	// A pod labelled for this worker in any namespace (terminating included) may be the
	// consumer, so the hold proves nothing about the claim.
	if len(ev.pods[o.ID]) > 0 {
		return nil
	}
	var deferred map[string]bool
	for _, claim := range o.SchedulerPendingClaims {
		key := claim.Namespace + "/" + claim.Name
		if m.now().Sub(claim.CreatedAt) < pvcBindTimeout || claim.Namespace != ns || ev.claimMounts[key] || claim.StorageClassName == "" {
			continue
		}
		// Last, so a class is read only for a claim that passed every other test.
		if !m.bindingModeWFFC(ctx, classCache, claim.StorageClassName) {
			continue
		}
		if deferred == nil {
			deferred = map[string]bool{}
		}
		deferred[key] = true
	}
	return deferred
}

// recordClaimMounts folds every pod in the namespace into ev.claimMounts, regardless of
// label or uid.
func recordClaimMounts(ev *schedulerCapacityObservation, ns string, pods []corev1.Pod) {
	if ev.claimMounts == nil {
		ev.claimMounts = map[string]bool{}
	}
	for i := range pods {
		for _, v := range pods[i].Spec.Volumes {
			if v.PersistentVolumeClaim != nil {
				ev.claimMounts[ns+"/"+v.PersistentVolumeClaim.ClaimName] = true
			}
		}
	}
}
