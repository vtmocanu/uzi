package kube

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/vtmocanu/uzi/controller/internal/preset"
	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

// Optional structural interfaces preserve New and legacy Cordoner compatibility.
type maintenanceClient interface {
	TransitionDinD(context.Context, string, protocol.DindMaintenance) (protocol.DindMaintenance, error)
}

// reconcileDinD makes at most one server transition per worker per tick.
// A persisted initial intent may precede legacy recycling in the same tick.
// Failures defer this worker; Reconcile continues siblings.
func (m *Materializer) reconcileDinD(ctx context.Context, w protocol.DesiredWorker, o reconcile.ObservedWorker, ns string, spec preset.Spec) (bool, error) {
	c, supported := m.cordoner.(maintenanceClient)
	op := w.DindMaintenance
	active := op != nil && (op.Phase == "requested" || op.Phase == "ready" || op.Phase == "stopping" || op.Phase == "recycling")
	if !w.Docker || w.Ephemeral || !supported {
		return active, nil
	}
	transition := func(next protocol.DindMaintenance) {
		if _, err := c.TransitionDinD(ctx, w.ID, next); err != nil {
			m.log.Info("DinD maintenance waiting for server preconditions", "worker_id", w.ID, "phase", next.Phase, "error", err)
		}
	}
	if !active {
		pressure := false
		for _, v := range w.DiskPressureVolumes {
			if v == "dind" {
				pressure = true
			}
		}
		if !pressure || !m.recycle.Enabled {
			return false, nil
		}
		var err error
		o, err = m.maintenanceObservation(ctx, w.ID, ns, o)
		if err != nil {
			return true, err
		}
		if o.DinDPVCCreatedAt != nil && m.now().Sub(*o.DinDPVCCreatedAt) < m.recycle.Cooldown {
			m.log.Info(fmt.Sprintf("disk-recycle-skipped-cooldown worker=%s", w.ID))
			return false, nil
		}
		if o.HasDeployment && !o.DeploymentTerminating && o.DeploymentUID != "" && o.DinDPVCLive && o.DinDPVCUID != "" {
			_, err = c.TransitionDinD(ctx, w.ID, protocol.DindMaintenance{Phase: "requested", DeploymentUID: o.DeploymentUID, PVCUID: o.DinDPVCUID})
			if err == nil {
				return !m.legacyRecycleDue(w, o, spec), nil
			}
			// Explicit refusal proves no request was accepted. Ambiguous transport
			// failures defer legacy until the next authoritative poll.
			if refusal, ok := err.(interface{ HTTPStatus() int }); ok && (refusal.HTTPStatus() == 404 || refusal.HTTPStatus() == 409) {
				return false, nil
			}
			m.log.Info("DinD intent request deferred", "worker_id", w.ID, "error", err)
			return true, nil
		}
		return false, nil
	}
	next := *op
	// Absent pressure can mean a fresh low sample OR unknown telemetry. Let the
	// API distinguish them under its worker lock; only confirmed cancellation
	// releases the drain. A 409 is a conservative refusal, not clearance.
	if (op.Phase == "requested" || op.Phase == "ready") && m.recycle.Enabled {
		pressured := false
		for _, volume := range w.DiskPressureVolumes {
			pressured = pressured || volume == "dind"
		}
		if !pressured {
			cancel := next
			cancel.Phase, cancel.Reason = "cancelled", "below_threshold"
			if _, err := c.TransitionDinD(ctx, w.ID, cancel); err == nil {
				return true, nil
			} else if refusal, ok := err.(interface{ HTTPStatus() int }); !ok || refusal.HTTPStatus() != 409 {
				return true, nil
			}
		}
	}
	wantHash := RenderDeployment(m.cfg, w, spec).Spec.Template.Annotations[AnnotationSpecHash]
	if !m.recycle.Enabled && (op.Phase == "requested" || op.Phase == "ready") {
		next.Phase = "cancelled"
		next.Reason = "recycle_disabled"
		transition(next)
		return true, nil
	}
	{
		var err error
		o, err = m.maintenanceObservation(ctx, w.ID, ns, o, op)
		if err != nil {
			return true, err
		}
	}
	switch op.Phase {
	case "requested":
		// Retain the request through a simultaneous legacy recycle. Once replacement
		// readiness is authoritative, refresh its binding; the server qualifies the
		// new registration and pressure observations before accepting this refresh.
		if o.DeploymentUID != op.DeploymentUID || o.DinDPVCUID != op.PVCUID {
			if !w.Busy && !w.CustodyHeld && o.HasDeployment && !o.DeploymentTerminating && o.ReadyPodUID != "" && o.SpecHash == wantHash && o.Generation == w.Generation && o.Roll.Phase == protocol.PhaseSettled && o.DinDPVCLive {
				next.DeploymentUID, next.PVCUID = o.DeploymentUID, o.DinDPVCUID
				next.Phase = "ready"
				transition(next)
			}
			// Provision after legacy deletion, but suppress another legacy recycle.
			w.DindMaintenance = nil
			w.DiskPressure = false
			return true, m.reconcileReplacement(ctx, w, o)
		}
		if m.legacyRecycleDue(w, o, spec) {
			return false, nil
		}
		if w.Busy || w.CustodyHeld || !o.DinDPVCLive || o.DeploymentTerminating {
			return true, nil
		}
		next.Phase = "ready"
		transition(next)
	case "ready":
		if !op.Fenced || !op.ReadyACK || w.Busy || w.CustodyHeld || o.DeploymentUID != op.DeploymentUID || o.DinDPVCUID != op.PVCUID || o.DeploymentTerminating || !o.DinDPVCLive {
			return true, nil
		}
		next.Phase = "stopping"
		transition(next)
	case "stopping":
		if !op.Fenced || w.Busy || w.CustodyHeld {
			return true, nil
		}
		if o.HasDeployment {
			if o.DeploymentUID != op.DeploymentUID || o.DeploymentTerminating {
				return true, nil
			}
			uid := types.UID(op.DeploymentUID)
			foreground := metav1.DeletePropagationForeground
			err := m.client.AppsV1().Deployments(ns).Delete(ctx, deploymentName(w.ID), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}, PropagationPolicy: &foreground})
			if err != nil && !apierrors.IsNotFound(err) {
				return true, err
			}
			return true, nil
		}
		if !o.PodsKnown || o.WorkerPodCount != 0 {
			return true, nil
		}
		next.Phase = "recycling"
		transition(next)
	case "recycling":
		if !op.Fenced || w.Busy || w.CustodyHeld {
			return true, nil
		}
		if o.HasDinDDataPVC && o.DinDPVCUID == op.PVCUID {
			if o.HasDeployment || !o.PodsKnown || o.WorkerPodCount != 0 || !o.DinDPVCLive {
				return true, nil
			}
			uid := types.UID(op.PVCUID)
			err := m.client.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, dindDataPVCName(w.ID), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
			if err != nil && !apierrors.IsNotFound(err) {
				return true, err
			}
			return true, nil
		}
		// Do not rebuild over the old daemon, even after an external PVC delete.
		if o.HasDeployment && o.DeploymentUID == op.DeploymentUID ||
			!o.HasDeployment && o.WorkerPodCount != 0 {
			return true, nil
		}
		// A same-name PVC with a different UID proves the old identity no longer
		// occupies that name, even across a restart. Never delete the replacement.
		if !o.HasDinDDataPVC || !o.HasDeployment {
			if o.HasDinDDataPVC && !o.DinDPVCLive {
				return true, nil
			}
			return true, m.reconcileReplacement(ctx, w, o)
		}
		if !o.DinDPVCLive || o.DinDPVCPhase != string(corev1.ClaimBound) || o.DinDPVCUID == "" || o.DeploymentUID == "" || o.DeploymentUID == op.DeploymentUID || o.DeploymentTerminating || !o.PodsKnown || o.ReadyPodUID == "" || o.ReadyPodUID == op.DeploymentUID || o.SpecHash != wantHash || o.Generation != w.Generation || o.Roll.Phase != protocol.PhaseSettled {
			return true, nil
		}
		reporter, ok := m.cordoner.(interface {
			ReportReadiness(context.Context, protocol.StatusReport) error
		})
		if !ok {
			return true, nil
		}
		// Publish readiness before complete; completion proves registration server-side.
		err := reporter.ReportReadiness(ctx, protocol.StatusReport{ReportedAt: m.now(), PollIntervalSeconds: 10, Workers: []protocol.WorkerStatus{{ID: w.ID, Phase: protocol.PhaseSettled, PhaseSince: &o.Roll.PhaseSince, TargetImage: o.Roll.TargetImage, PodPhase: o.Roll.PodPhase}}})
		if err != nil {
			return true, err
		}
		next.Phase = "complete"
		transition(next)
	}
	return true, nil
}

func (m *Materializer) legacyRecycleDue(w protocol.DesiredWorker, o reconcile.ObservedWorker, spec preset.Spec) bool {
	return m.recycle.Enabled && !w.Ephemeral && ((o.HasNixPVC && o.NixPVCSize != nil && o.NixPVCSize.Cmp(spec.NixSize) < 0) || (w.DiskPressure && !w.CustodyHeld && !m.withinRecycleCooldown(o)))
}

func (m *Materializer) reconcileReplacement(ctx context.Context, w protocol.DesiredWorker, o reconcile.ObservedWorker) error {
	// Disable only the legacy triggers in this local copy during fenced rebuilding.
	w.DindMaintenance = nil
	w.DiskPressure = false
	return m.reconcileWorkerWithLegacy(ctx, w, o, false)
}
