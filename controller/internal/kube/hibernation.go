package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/vtmocanu/uzi/controller/internal/apiclient"
	"github.com/vtmocanu/uzi/controller/internal/preset"
	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

// Worker hibernation (PRD #4205).
//
// Everything here is derived from the desired state of this poll plus what the
// cluster shows right now (the Deployment's replicas and annotations, the pods). No
// state is kept in this process: a restarted controller reaches the same decisions.
//
// Stale-mutation safety: every Deployment write is a JSON merge patch whose metadata
// carries the observed uid and resourceVersion, which the apiserver treats as a
// precondition. A scale-down computed for an operation the api has since abandoned
// therefore fails once anything else (the wake patch, the refusal fence) has moved
// the object. The wire reports carry the Deployment uid, the resourceVersion the
// write returned and the pod uid so the api can fence them against its own state.
//
// The PVCs and the Secret are never deleted on any hibernation path.
const (
	// annotationHibernationOp is the op id of the hibernation or wake operation that
	// last scaled this Deployment. It lives on the Deployment object (not the pod
	// template, which would roll the pod).
	annotationHibernationOp = "uzi.dev/hibernation-op"
	// annotationHibernationRefused is the refusal fence, valued "<op>/<UnixNano>-<seq>"
	// (refusalFenceValue), unique per send. Writing it bumps the resourceVersion, so a
	// scale-down patch computed before the refusal can no longer satisfy its precondition.
	annotationHibernationRefused = "uzi.dev/hibernation-refused"
)

// Compile-time pin: the real api client is the hibernation channel the Materializer's
// Cordoner type assertion looks for. Without it a signature drift would silently turn
// every hibernation report into a no-op.
var _ hibernationReporter = (*apiclient.Client)(nil)

var refuseFenceSeq atomic.Uint64

// refusalFenceValue is the annotation value of one refusal fence: "<op>/<unique>". It is
// unique per send, never derived from the injectable clock alone, because a merge patch
// that changes nothing does not advance a real apiserver's resourceVersion, and the
// fence exists to advance it. The op id is the prefix up to the last "/".
func refusalFenceValue(op string) string {
	return op + "/" + strconv.FormatInt(time.Now().UnixNano(), 10) + "-" + strconv.FormatUint(refuseFenceSeq.Add(1), 10)
}

// Refusal reasons reported on a refused transition.
const (
	refuseRollReservation = "roll_reservation"
	refuseRollBlocked     = "roll_blocked"
	refuseDiskPressure    = "disk_pressure"
	refuseDinDMaintenance = "dind_maintenance"
	refuseBusy            = "busy"
	refuseLayoutHeld      = "layout_migration_held"
	refuseNoRollEvidence  = "roll_evidence_unavailable"
)

// hibernationReporter is the api channel for transitions; *apiclient.Client satisfies
// it through the Cordoner the Materializer already holds. A build or test without it
// reports nothing, which leaves every transition unreported and so uncommitted api-side.
type hibernationReporter interface {
	ReportHibernation(ctx context.Context, workerID string, t protocol.HibernationTransition) error
}

// applyDeploymentHibernationEvidence copies the hibernation-relevant Deployment facts
// onto the observation.
func applyDeploymentHibernationEvidence(o *reconcile.ObservedWorker, d *appsv1.Deployment) {
	o.DeploymentReplicas = nil
	if d.Spec.Replicas != nil {
		r := *d.Spec.Replicas
		o.DeploymentReplicas = &r
	}
	o.HibernationOp = d.Annotations[annotationHibernationOp]
}

// applyPodEvidence derives pod count, readiness and uids from one worker's pods (every
// pod labelled for it, terminating ones included). It needs o.SpecHash already set: a
// Ready pod counts only when it carries the Deployment template's spec hash and has no
// deletionTimestamp.
func applyPodEvidence(o *reconcile.ObservedWorker, pods []corev1.Pod) {
	o.PodsKnown = true
	o.WorkerPodCount = len(pods)
	o.ReadyPodUID, o.LivePodUID = "", ""
	o.WorkerPodUIDs = nil
	for _, p := range pods {
		if p.UID != "" {
			o.WorkerPodUIDs = append(o.WorkerPodUIDs, string(p.UID))
		}
		if p.DeletionTimestamp != nil || p.UID == "" {
			continue
		}
		if o.LivePodUID == "" || string(p.UID) < o.LivePodUID {
			o.LivePodUID = string(p.UID)
		}
		if p.Annotations[AnnotationSpecHash] == o.SpecHash {
			for _, c := range p.Status.Conditions {
				if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
					o.ReadyPodUID = string(p.UID)
				}
			}
		}
	}
	sort.Strings(o.WorkerPodUIDs)
}

// deploymentPlacementBlocked reports that a Deployment must not be created yet: a nix
// PVC EXISTS but is not LIVE at the desired size (Terminating, or mid-recycle), or a
// /data or DinD claim is Terminating. Never place a Deployment over a doomed or
// undersized volume. False when a claim is absent (first provision and re-mint still
// create in one tick) or live (steady state).
func deploymentPlacementBlocked(w protocol.DesiredWorker, obs reconcile.ObservedWorker, spec preset.Spec) bool {
	nixLiveAtSize := obs.NixPVCSize != nil && obs.NixPVCSize.Cmp(spec.NixSize) >= 0
	dataBlocking := obs.HasDataPVC && !obs.DataPVCLive // present but Terminating
	dindBlocking := w.Docker && obs.HasDinDDataPVC && !obs.DinDPVCLive
	return (obs.HasNixPVC && !nixLiveAtSize) || dataBlocking || dindBlocking
}

func (m *Materializer) reconcileHibernation(ctx context.Context, w protocol.DesiredWorker, obs reconcile.ObservedWorker, ns string, spec preset.Spec) error {
	h := w.Hibernation
	if h == nil || h.OpID == "" {
		m.log.Warn("hibernation state without an op id; doing nothing (fail closed)", "worker_id", w.ID)
		return nil
	}
	switch h.State {
	case protocol.HibernationPreparing:
		return m.hibernatePreparing(ctx, w, obs, ns, spec)
	case protocol.HibernationHibernated:
		return m.hibernateHold(ctx, w, obs, ns, spec)
	case protocol.HibernationWaking:
		return m.hibernateWake(ctx, w, obs, ns, spec)
	default:
		m.log.Warn("unknown hibernation state; doing nothing (fail closed)", "worker_id", w.ID, "state", h.State)
		return nil
	}
}

// createHibernationDeployment creates the worker's Deployment with the op annotation, at the
// given replicas. A fresh create is safe without a precondition: AlreadyExists means
// something else won, and the next tick re-reads.
func (m *Materializer) createHibernationDeployment(ctx context.Context, w protocol.DesiredWorker, obs reconcile.ObservedWorker, ns string, spec preset.Spec, replicas int32) error {
	if deploymentPlacementBlocked(w, obs, spec) {
		return nil
	}
	dep := RenderDeployment(m.cfg, w, spec)
	dep.Spec.Replicas = &replicas
	if dep.Annotations == nil {
		dep.Annotations = map[string]string{}
	}
	dep.Annotations[annotationHibernationOp] = w.Hibernation.OpID
	// Same as the ordinary create path: a retained roll marker rides onto the new object.
	if e := rollFrom(ctx, w.ID); e != nil && e.marker != nil {
		raw, err := json.Marshal(e.marker)
		if err != nil {
			return err
		}
		dep.Annotations[annotationRoll] = string(raw)
	}
	if _, err := m.client.AppsV1().Deployments(ns).Create(ctx, dep, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create deployment: %w", err)
	}
	return nil
}

// conditionedPatch applies a merge patch carrying the observed uid and resourceVersion
// as preconditions. The returned object is the apiserver's response to this exact
// write: its uid must match and its resourceVersion must have advanced, otherwise the
// outcome is not proven and an error is returned. A Conflict returns (nil, true, nil):
// the observation was stale, the write did not happen, and this tick stops.
func (m *Materializer) conditionedPatch(ctx context.Context, ns string, obs reconcile.ObservedWorker, name string, meta map[string]any, spec map[string]any) (*appsv1.Deployment, bool, error) {
	if obs.DeploymentUID == "" || obs.DeploymentResourceVersion == "" {
		return nil, false, fmt.Errorf("hibernation mutation requires observed UID/resourceVersion")
	}
	meta["uid"], meta["resourceVersion"] = obs.DeploymentUID, obs.DeploymentResourceVersion
	patch := map[string]any{"metadata": meta}
	if spec != nil {
		patch["spec"] = spec
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return nil, false, err
	}
	got, err := m.client.AppsV1().Deployments(ns).Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{})
	if err != nil {
		if apierrors.IsConflict(err) {
			return nil, true, nil
		}
		// Ambiguous (timeout, 5xx): the write may have landed. Nothing is reported from
		// it; the next tick re-reads and decides from what the cluster now shows.
		return nil, false, fmt.Errorf("patch deployment: %w", err)
	}
	if got == nil || string(got.UID) != obs.DeploymentUID || got.ResourceVersion == "" || got.ResourceVersion == obs.DeploymentResourceVersion {
		return nil, false, fmt.Errorf("patch deployment: response lacks the advanced resourceVersion")
	}
	return got, false, nil
}

func (m *Materializer) reportHibernation(ctx context.Context, w protocol.DesiredWorker, t protocol.HibernationTransition) error {
	r, ok := m.cordoner.(hibernationReporter)
	if !ok {
		return nil
	}
	if err := r.ReportHibernation(ctx, w.ID, t); err != nil {
		if errors.Is(err, apiclient.ErrHibernationConflict) {
			// The api's definite "stale or illegal": final for this tick, and the next
			// poll shows its actual state.
			m.log.Info("hibernation transition rejected as stale; re-polling", "worker_id", w.ID, "transition", t.Transition)
			return nil
		}
		return fmt.Errorf("report hibernation %s: %w", t.Transition, err)
	}
	return nil
}

// hibernationRefusal returns why a worker preparing to hibernate must be refused, ""
// when it may proceed. It fails closed: anything the roll machinery is doing, or could
// not be shown not to be doing, refuses.
func (m *Materializer) hibernationRefusal(ctx context.Context, w protocol.DesiredWorker, obs reconcile.ObservedWorker) string {
	e := rollFrom(ctx, w.ID)
	switch {
	case e == nil:
		return refuseNoRollEvidence
	case e.marker != nil || e.rawReservation || hasOpenReservation(e):
		return refuseRollReservation
	case e.blocked != "":
		return refuseRollBlocked
	case w.DiskPressure:
		return refuseDiskPressure
	case maintenanceActive(w):
		return refuseDinDMaintenance
	case migrationHeld(w, obs.RunnerLayout):
		return refuseLayoutHeld
	case w.Busy:
		return refuseBusy
	}
	return ""
}

func (m *Materializer) hibernatePreparing(ctx context.Context, w protocol.DesiredWorker, obs reconcile.ObservedWorker, ns string, spec preset.Spec) error {
	op := w.Hibernation.OpID
	if !obs.HasDeployment {
		// Roll evidence that a recycle is in flight (a marker, a reservation, or a
		// blocked snapshot) means the Deployment is missing mid-roll, not never-made:
		// creating it here would fight the roll. Do nothing and let the api's preparing
		// deadline move the worker on.
		if e := rollFrom(ctx, w.ID); e != nil && (e.marker != nil || e.rawReservation || hasOpenReservation(e) || e.blocked != "") {
			return nil
		}
		// Nothing runs, so there is nothing to protect and nothing to fence (a refusal
		// carries the Deployment uid): the worker is created already asleep.
		return m.createHibernationDeployment(ctx, w, obs, ns, spec, 0)
	}
	if obs.DeploymentTerminating {
		return nil
	}
	name := deploymentName(w.ID)

	if reason := m.hibernationRefusal(ctx, w, obs); reason != "" {
		got, stale, err := m.conditionedPatch(ctx, ns, obs, name, map[string]any{"annotations": map[string]any{annotationHibernationRefused: refusalFenceValue(op)}}, nil)
		if err != nil || stale {
			// No fence, no refusal this tick: a refusal the api acted on while a
			// scale-down for this op could still land would be unsafe.
			return err
		}
		if got.Annotations[annotationHibernationOp] != op {
			return m.reportHibernation(ctx, w, protocol.HibernationTransition{
				OpID: op, Transition: protocol.TransitionRefused,
				DeploymentUID: string(got.UID), ResourceVersion: got.ResourceVersion, Reason: reason,
			})
		}
		// The scale-down for this op already landed, so it can no longer be refused.
		// Carry on with its progress from the fenced object.
		obs.DeploymentResourceVersion = got.ResourceVersion
		applyDeploymentHibernationEvidence(&obs, got)
	}

	scaled := obs.DeploymentReplicas != nil && *obs.DeploymentReplicas == 0 && obs.HibernationOp == op
	if !scaled {
		got, stale, err := m.conditionedPatch(ctx, ns, obs, name,
			map[string]any{"annotations": map[string]any{annotationHibernationOp: op}},
			map[string]any{"replicas": 0})
		if err != nil || stale {
			return err
		}
		if got.Spec.Replicas == nil || *got.Spec.Replicas != 0 || got.Annotations[annotationHibernationOp] != op {
			return fmt.Errorf("hibernation scale-down response differs from the request")
		}
		pod := obs.LivePodUID
		if pod == "" && len(obs.WorkerPodUIDs) > 0 {
			pod = obs.WorkerPodUIDs[0]
		}
		if pod == "" {
			// The api requires a pod uid on scaling_down; with no pod there is no step
			// to report, and the next tick reaches scaled_down once pod absence is read.
			return nil
		}
		return m.reportHibernation(ctx, w, protocol.HibernationTransition{
			OpID: op, Transition: protocol.TransitionScalingDown,
			DeploymentUID: string(got.UID), ResourceVersion: got.ResourceVersion, PodUID: pod,
		})
	}

	// Scaled to zero for this op: the step finishes only when the pods are gone. A
	// terminating pod still counts, it still holds the workdir.
	if !obs.PodsKnown {
		return nil
	}
	if obs.WorkerPodCount > 0 {
		pod := obs.LivePodUID
		if pod == "" && len(obs.WorkerPodUIDs) > 0 {
			pod = obs.WorkerPodUIDs[0]
		}
		if pod == "" {
			return nil
		}
		// Re-sent each tick until the pods are gone: the report is idempotent api-side,
		// and with no local state a lost one is otherwise never repeated.
		return m.reportHibernation(ctx, w, protocol.HibernationTransition{
			OpID: op, Transition: protocol.TransitionScalingDown,
			DeploymentUID: obs.DeploymentUID, ResourceVersion: obs.DeploymentResourceVersion, PodUID: pod,
		})
	}
	return m.reportHibernation(ctx, w, protocol.HibernationTransition{
		OpID: op, Transition: protocol.TransitionScaledDown,
		DeploymentUID: obs.DeploymentUID, ResourceVersion: obs.DeploymentResourceVersion,
	})
}

// hibernateHold keeps a hibernated worker at zero replicas. It runs none of the roll
// machinery: no drift patch, no ForceRoll, no marker or pacing slot.
func (m *Materializer) hibernateHold(ctx context.Context, w protocol.DesiredWorker, obs reconcile.ObservedWorker, ns string, spec preset.Spec) error {
	op := w.Hibernation.OpID
	if !obs.HasDeployment {
		return m.createHibernationDeployment(ctx, w, obs, ns, spec, 0)
	}
	if obs.DeploymentTerminating || (obs.DeploymentReplicas != nil && *obs.DeploymentReplicas == 0) {
		return nil
	}
	_, _, err := m.conditionedPatch(ctx, ns, obs, deploymentName(w.ID),
		map[string]any{"annotations": map[string]any{annotationHibernationOp: op}},
		map[string]any{"replicas": 0})
	return err
}

// hibernateWake brings a hibernated worker back at its current template. It
// deliberately bypasses the busy/migration-held deferral that guards a drift roll: the
// worker has no pod and no run, and an old-layout custody hold protects a pod's
// in-pod workdir that a woken-from-zero worker does not have.
//
// A Deployment at an explicit zero is held there, with no write and no report, until a
// complete pod observation shows no pod of the worker left (terminating ones included).
// Only then does the single restoring patch run. This covers a quick wake, where the api
// moved preparing to waking while the original pod was still stopping.
func (m *Materializer) hibernateWake(ctx context.Context, w protocol.DesiredWorker, obs reconcile.ObservedWorker, ns string, spec preset.Spec) error {
	op := w.Hibernation.OpID
	if !obs.HasDeployment {
		return m.createHibernationDeployment(ctx, w, obs, ns, spec, 1)
	}
	if obs.DeploymentTerminating {
		return nil
	}
	// A Deployment scaled to zero (by this or an earlier op) is restored only once a complete
	// observation shows that no pod of the worker remains, terminating ones included. Restoring
	// earlier can keep the original pod alive; the api never settles that pod after scaling_down
	// (trySettle), so the worker would stay in waking for good. nil replicas is excluded: a real
	// apiserver defaults it to 1.
	if obs.DeploymentReplicas != nil && *obs.DeploymentReplicas == 0 {
		if !obs.PodsKnown || obs.WorkerPodCount > 0 {
			return nil
		}
	}
	dep := RenderDeployment(m.cfg, w, spec)
	wantHash := dep.Spec.Template.Annotations[AnnotationSpecHash]
	asleep := obs.DeploymentReplicas == nil || *obs.DeploymentReplicas == 0
	drifted := obs.SpecHash != wantHash || obs.Generation != w.Generation
	if asleep || obs.HibernationOp != op || drifted {
		// ONE conditioned patch: replicas 1, the current template and the op
		// annotation together, so no intermediate state is observable.
		dep.UID, dep.ResourceVersion = types.UID(obs.DeploymentUID), obs.DeploymentResourceVersion
		if obs.DeploymentUID == "" || obs.DeploymentResourceVersion == "" {
			return fmt.Errorf("template mutation requires observed UID/resourceVersion")
		}
		patch, err := patchForWithAnnotations(dep, obs.DeploymentNodeSelector, map[string]string{annotationHibernationOp: op})
		if err != nil {
			return err
		}
		if _, err := m.client.AppsV1().Deployments(ns).Patch(ctx, dep.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
			if apierrors.IsConflict(err) {
				return nil
			}
			return fmt.Errorf("patch deployment: %w", err)
		}
		return nil
	}
	if obs.ReadyPodUID == "" {
		return nil
	}
	return m.reportHibernation(ctx, w, protocol.HibernationTransition{
		OpID: op, Transition: protocol.TransitionReady,
		DeploymentUID: obs.DeploymentUID, ResourceVersion: obs.DeploymentResourceVersion, PodUID: obs.ReadyPodUID,
	})
}
