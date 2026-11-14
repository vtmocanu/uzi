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
	o.DeploymentMetaGeneration = d.Generation
}

// applyPodEvidence derives pod count, readiness and uids from one worker's pods (every
// pod labelled for it, terminating ones included). It needs o.SpecHash already set: a
// Ready pod counts only when it carries the Deployment template's spec hash and has no
// deletionTimestamp. A pod in a terminal phase (Succeeded or Failed) is counted in
// TerminalPodCount (and in StoppedPodCount when podStopped proves every container
// terminated) and keeps its UID in WorkerPodUIDs, but never supplies LivePodUID or
// ReadyPodUID: kubelet eviction publishes the phase before it kills the containers and
// the pod keeps its stale Ready condition, so a terminal phase is no readiness evidence.
func applyPodEvidence(o *reconcile.ObservedWorker, pods []corev1.Pod) {
	o.PodsKnown = true
	o.WorkerPodCount = len(pods)
	o.TerminalPodCount = 0
	o.StoppedPodCount = 0
	o.ReadyPodUID, o.LivePodUID = "", ""
	o.WorkerPodUIDs = nil
	for _, p := range pods {
		if p.UID != "" {
			o.WorkerPodUIDs = append(o.WorkerPodUIDs, string(p.UID))
		}
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			o.TerminalPodCount++
			if podStopped(p) {
				o.StoppedPodCount++
			}
			continue
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

// podStopped reports positive evidence that no container of the pod is running: the
// phase is Succeeded or Failed AND the status of every regular, init and ephemeral
// container is terminated. A terminal phase alone is not enough, because kubelet
// eviction publishes phase Failed before it kills the containers. It fails closed: a
// missing, short or extra status entry, an unnamed match, a running or waiting
// container, a termination with reason ContainerStatusUnknown, or a pod with no
// regular containers all read as not stopped. ContainerStatusUnknown means the
// container's end was never observed, so process death is unproven (ADR-1470). The
// accepted residual: eviction residue with an unknown status holds the wake until the
// api's wake timeout records wake_error, and keeps holding it until the pod is removed;
// an admin may force-delete it only after confirming the node is down or fenced from its
// storage (docs/hosted-workers.md). A deletionTimestamp is irrelevant.
func podStopped(p corev1.Pod) bool {
	if p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
		return false
	}
	if len(p.Spec.Containers) == 0 {
		return false
	}
	names := func(specs []corev1.Container) []string {
		out := make([]string, 0, len(specs))
		for _, c := range specs {
			out = append(out, c.Name)
		}
		return out
	}
	ephNames := make([]string, 0, len(p.Spec.EphemeralContainers))
	for _, c := range p.Spec.EphemeralContainers {
		ephNames = append(ephNames, c.Name)
	}
	return allTerminated(names(p.Spec.Containers), p.Status.ContainerStatuses) &&
		allTerminated(names(p.Spec.InitContainers), p.Status.InitContainerStatuses) &&
		allTerminated(ephNames, p.Status.EphemeralContainerStatuses)
}

// allTerminated reports that statuses cover exactly the spec names and each is terminated.
func allTerminated(specNames []string, statuses []corev1.ContainerStatus) bool {
	if len(statuses) != len(specNames) {
		return false
	}
	byName := make(map[string]corev1.ContainerStatus, len(statuses))
	for _, s := range statuses {
		byName[s.Name] = s
	}
	for _, n := range specNames {
		s, ok := byName[n]
		// ContainerStatusUnknown is kubelet's or PodGC's placeholder when the container's
		// end was never observed: process death unproven (ADR-1470).
		if !ok || s.State.Terminated == nil || s.State.Running != nil || s.State.Waiting != nil ||
			s.State.Terminated.Reason == "ContainerStatusUnknown" {
			return false
		}
	}
	return len(byName) == len(specNames)
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
	// The operator's labels and annotations ride every update (an admission policy may
	// validate updates); the caller's own annotation keys keep their values.
	patch := map[string]any{"metadata": m.withOperatorMetadata(meta)}
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
		// Inventory gate (PRD #4208): until the api permits, the scale-down is not issued;
		// the identity report is sent every tick instead (the api treats an identical
		// re-send as idempotent). An empty gate is today's behaviour.
		if g := w.Hibernation.InventoryGate; g == protocol.InventoryGateObserve || g == protocol.InventoryGateAwait {
			return m.reportObserved(ctx, w, obs)
		}
		got, stale, err := m.conditionedPatch(ctx, ns, obs, name,
			map[string]any{"annotations": map[string]any{annotationHibernationOp: op}},
			map[string]any{"replicas": 0})
		if err != nil || stale {
			return err
		}
		if got.Spec.Replicas == nil || *got.Spec.Replicas != 0 || got.Annotations[annotationHibernationOp] != op {
			return fmt.Errorf("hibernation scale-down response differs from the request")
		}
		pod := boundPodUID(obs)
		if pod == "" {
			// The api requires a pod uid on scaling_down; with no pod there is no step
			// to report, and the next tick reaches scaled_down once pod absence is read.
			return nil
		}
		t := protocol.HibernationTransition{
			OpID: op, Transition: protocol.TransitionScalingDown,
			DeploymentUID: string(got.UID), ResourceVersion: got.ResourceVersion, PodUID: pod,
		}
		// The write's own generation is the fence. The pod sets ride along only when the
		// pod list was read; omitted, the api marks the pod set unproven (fail closed).
		fence, gen, zero := got.Generation, got.Generation, int32(0)
		t.FenceGeneration, t.DeploymentGeneration, t.DeploymentReplicas = &fence, &gen, &zero
		t.PodUIDs, t.PVCUserPods = podFacts(obs)
		return m.reportHibernation(ctx, w, t)
	}

	// Scaled to zero for this op: the step finishes only when the pods are gone. A
	// terminating pod still counts, it still holds the workdir.
	if !obs.PodsKnown {
		return nil
	}
	if obs.WorkerPodCount > 0 {
		pod := boundPodUID(obs)
		if pod == "" {
			return nil
		}
		// Re-sent each tick until the pods are gone: the report is idempotent api-side,
		// and with no local state a lost one is otherwise never repeated. No fence
		// generation: only the write that scaled carries one.
		t := protocol.HibernationTransition{
			OpID: op, Transition: protocol.TransitionScalingDown,
			DeploymentUID: obs.DeploymentUID, ResourceVersion: obs.DeploymentResourceVersion, PodUID: pod,
		}
		setObservationFacts(&t, obs)
		return m.reportHibernation(ctx, w, t)
	}
	// No pod remains, so PodUIDs is a known-empty set (encoded as []).
	t := protocol.HibernationTransition{
		OpID: op, Transition: protocol.TransitionScaledDown,
		DeploymentUID: obs.DeploymentUID, ResourceVersion: obs.DeploymentResourceVersion,
	}
	setObservationFacts(&t, obs)
	return m.reportHibernation(ctx, w, t)
}

// hibernateHold keeps a hibernated worker at zero replicas. It runs none of the roll
// machinery: no drift patch, no ForceRoll, no marker or pacing slot.
func (m *Materializer) hibernateHold(ctx context.Context, w protocol.DesiredWorker, obs reconcile.ObservedWorker, ns string, spec preset.Spec) error {
	op := w.Hibernation.OpID
	if !obs.HasDeployment {
		return m.createHibernationDeployment(ctx, w, obs, ns, spec, 0)
	}
	if !obs.DeploymentTerminating && (obs.DeploymentReplicas == nil || *obs.DeploymentReplicas != 0) {
		// A re-patch changes what the held report would describe; the next tick reports it
		// from a fresh observation.
		_, _, err := m.conditionedPatch(ctx, ns, obs, deploymentName(w.ID),
			map[string]any{"annotations": map[string]any{annotationHibernationOp: op}},
			map[string]any{"replicas": 0})
		return err
	}
	// Every tick: the writer exclusion is still held. With the pod list unread nothing is
	// sent, and the api's gap check fails closed.
	if !obs.PodsKnown {
		return nil
	}
	t := protocol.HibernationTransition{
		OpID: op, Transition: protocol.TransitionHeld,
		DeploymentUID: obs.DeploymentUID, ResourceVersion: obs.DeploymentResourceVersion,
	}
	setObservationFacts(&t, obs)
	return m.reportHibernation(ctx, w, t)
}

// setObservationFacts fills a transition's pod and PVC-user sets, the Deployment's
// metadata.generation and its replicas from obs, which must have PodsKnown. The sets are
// non-nil copies, so a known-empty set encodes as [].
func setObservationFacts(t *protocol.HibernationTransition, obs reconcile.ObservedWorker) {
	t.PodUIDs, t.PVCUserPods = podFacts(obs)
	gen := obs.DeploymentMetaGeneration
	t.DeploymentGeneration = &gen
	if obs.DeploymentReplicas != nil {
		r := *obs.DeploymentReplicas
		t.DeploymentReplicas = &r
	}
}

// podFacts returns copies of the worker's pod uids and PVC-user pod uids, both nil when
// the pod list was not read (an unobserved set is omitted, never sent as empty).
func podFacts(obs reconcile.ObservedWorker) (pods, users *[]string) {
	if !obs.PodsKnown {
		return nil, nil
	}
	p := append(make([]string, 0, len(obs.WorkerPodUIDs)), obs.WorkerPodUIDs...)
	u := append(make([]string, 0, len(obs.PVCUserPodUIDs)), obs.PVCUserPodUIDs...)
	return &p, &u
}

// inventoryPVCs returns one entry per existing worker claim (data, nix, and DinD data only
// when that claim exists), each with its storage class's reclaim policy.
func (m *Materializer) inventoryPVCs(ctx context.Context, obs reconcile.ObservedWorker) []protocol.HibernationPVC {
	var out []protocol.HibernationPVC
	for _, c := range []struct {
		role  string
		claim *reconcile.ClaimObservation
	}{
		{protocol.PVCRoleData, obs.DataClaim},
		{protocol.PVCRoleNix, obs.NixClaim},
		{protocol.PVCRoleDindData, obs.DinDClaim},
	} {
		if c.claim == nil {
			continue
		}
		class := ""
		if c.claim.StorageClassName != nil {
			class = *c.claim.StorageClassName
		}
		out = append(out, protocol.HibernationPVC{
			Role: c.role, Name: c.claim.Name, UID: c.claim.UID, StorageClass: class,
			ClassReclaimPolicy: m.classReclaimPolicy(ctx, c.claim.StorageClassName),
		})
	}
	return out
}

// reportObserved sends the pre-mutation identity report for an inventory-gated scale-down.
// It sends nothing while the pod list is unread or no pod uid is known, and never patches.
func (m *Materializer) reportObserved(ctx context.Context, w protocol.DesiredWorker, obs reconcile.ObservedWorker) error {
	pod := boundPodUID(obs)
	if !obs.PodsKnown || pod == "" {
		return nil
	}
	pvcs := m.inventoryPVCs(ctx, obs)
	if len(pvcs) == 0 {
		return nil // no claim observed: hold this tick rather than report an empty inventory
	}
	t := protocol.HibernationTransition{
		OpID: w.Hibernation.OpID, Transition: protocol.TransitionObserved,
		DeploymentUID: obs.DeploymentUID, ResourceVersion: obs.DeploymentResourceVersion, PodUID: pod,
		PVCs: pvcs,
	}
	setObservationFacts(&t, obs)
	return m.reportHibernation(ctx, w, t)
}

func boundPodUID(obs reconcile.ObservedWorker) string {
	if obs.LivePodUID != "" {
		return obs.LivePodUID
	}
	if len(obs.WorkerPodUIDs) > 0 {
		return obs.WorkerPodUIDs[0]
	}
	return ""
}

// wakeHeldForPods reports that a wake must not bring a pod up yet: the pod list was not
// read completely this tick, or some pod of the worker is not positively stopped
// (terminating ones included). Only a pod in phase Succeeded or Failed whose every
// container status is terminated (podStopped) stops holding the wake: an evicted pod,
// which a ReplicaSet at zero leaves in place, then cannot be the admission pod surviving
// the wake. A terminal phase alone does not release the hold, since kubelet eviction
// publishes Failed before it kills the containers. It logs the hold so a slow wake is
// visible before the api's wake timeout.
func (m *Materializer) wakeHeldForPods(w protocol.DesiredWorker, obs reconcile.ObservedWorker) bool {
	switch {
	case !obs.PodsKnown:
		m.log.Info("wake held: worker pods not observed", "worker_id", w.ID)
		return true
	case obs.WorkerPodCount-obs.StoppedPodCount > 0:
		m.log.Info("wake held: worker pods remain", "worker_id", w.ID,
			"live_pods", obs.WorkerPodCount-obs.StoppedPodCount,
			"terminal_pods", obs.TerminalPodCount, "stopped_pods", obs.StoppedPodCount)
		return true
	}
	return false
}

// hibernateWake brings a hibernated worker back at its current template. It
// deliberately bypasses the busy/migration-held deferral that guards a drift roll: the
// worker has no pod and no run, and an old-layout custody hold protects a pod's
// in-pod workdir that a woken-from-zero worker does not have.
//
// A wake that finds the Deployment at zero (or absent) holds there until one complete
// pod observation shows every worker pod positively stopped (terminating pods included;
// see wakeHeldForPods and podStopped). Restoring
// replicas earlier can let the original admission pod survive the scale-down, and once
// the api has recorded scaling_down it never settles a wake on that pod (trySettle rule
// (a)), so the worker would stay waking for good. Waiting until the pods are gone forces
// a fresh pod, which settles through ready plus its Register. The decision is read from
// the cluster every tick, so a restart or a lost acknowledgement re-derives it.
func (m *Materializer) hibernateWake(ctx context.Context, w protocol.DesiredWorker, obs reconcile.ObservedWorker, ns string, spec preset.Spec) error {
	op := w.Hibernation.OpID
	if !obs.HasDeployment {
		if m.wakeHeldForPods(w, obs) {
			return nil
		}
		return m.createHibernationDeployment(ctx, w, obs, ns, spec, 1)
	}
	if obs.DeploymentTerminating {
		return nil
	}
	// Only an explicit zero holds. A nil replicas field is what the fake client stores
	// for an unset one; a real apiserver defaults it to 1, and scaling_down can only be
	// recorded after a write of 0, so nil restores at once.
	if obs.DeploymentReplicas != nil && *obs.DeploymentReplicas == 0 && m.wakeHeldForPods(w, obs) {
		return nil
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
