package kube

// Size-only PVC expansion (issue #3890).
//
// A persistent worker's /data and DinD data-root claims are created at the render
// size and, until this file, were never patched: a preset or override that grew left
// every existing claim at its old request forever. expandClaims converges an EXISTING
// claim's storage request UPWARD to the render size with one fenced merge patch per
// claim per tick, and nothing else:
//
//   - never a shrink, a delete, a recreate or a recycle;
//   - never the Deployment, the pod template, the spec-hash or generation annotation,
//     a cordon or drain, a roll reservation or the uzi.dev/image-roll annotation, and
//     never a pod replacement. The pod template does not carry PVC sizes, so a larger
//     claim is invisible to the roll machinery by construction.
//
// Eligibility is one pure gate (claimGate) shared by the mutator and the read-only
// reporter, so the log can never claim a claim is deferred for a reason the mutator
// does not honour, nor the reverse. The decision for /data reads a single snapshot:
// when the worker has a roll entry, the claim rollSnapshot read (kept current by
// mirrorRoll) is the sole source, and Observe's claim only has to agree on identity.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/vtmocanu/uzi/controller/internal/preset"
	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

type claimKind int

const (
	claimData claimKind = iota
	claimDinD
)

const maxClaimErrorLen = 200

// claimObservationOf converts a listed claim into the internal observation.
func claimObservationOf(ns, workerID string, p *corev1.PersistentVolumeClaim) reconcile.ClaimObservation {
	o := reconcile.ClaimObservation{
		Name:             p.Name,
		Namespace:        ns,
		UID:              string(p.UID),
		ResourceVersion:  p.ResourceVersion,
		WorkerID:         workerID,
		Phase:            string(p.Status.Phase),
		Terminating:      p.DeletionTimestamp != nil,
		Requested:        p.Spec.Resources.Requests[corev1.ResourceStorage].DeepCopy(),
		RollAnnotation:   p.Annotations[annotationRoll],
		ResizeConditions: []string{},
	}
	if c, ok := p.Status.Capacity[corev1.ResourceStorage]; ok {
		c = c.DeepCopy()
		o.Capacity = &c
	}
	if p.Spec.StorageClassName != nil {
		sc := *p.Spec.StorageClassName
		o.StorageClassName = &sc
	}
	_, o.RollAnnotationPresent = p.Annotations[annotationRoll]
	for _, c := range p.Status.Conditions {
		if c.Status == corev1.ConditionTrue {
			o.ResizeConditions = append(o.ResizeConditions, string(c.Type))
		}
	}
	if len(p.Status.AllocatedResourceStatuses) > 0 {
		o.AllocatedResourceStatuses = map[string]string{}
		for k, v := range p.Status.AllocatedResourceStatuses {
			o.AllocatedResourceStatuses[string(k)] = string(v)
		}
	}
	return o
}

// claimGate is the pure eligibility gate for converging one claim. It returns ""
// for eligible and otherwise the first blocking reason, in this order:
// maintenance, roll-uncertain, roll-admission and (data only) roll-reservation.
// A Completed canary or cohort marker does not block; any open or unreadable
// reservation does. snap is the claim snapshot the decision reads (may be nil).
func claimGate(w protocol.DesiredWorker, kind claimKind, e *rollEntry, uncertain bool, snap *reconcile.ClaimObservation) string {
	if maintenanceActive(w) {
		return "maintenance"
	}
	if uncertain {
		return "roll-uncertain"
	}
	if e != nil && !e.admitted {
		return "roll-admission"
	}
	if kind != claimData || e == nil {
		return ""
	}
	if e.blocked != "" || (e.marker != nil && !e.marker.Completed) {
		return "roll-reservation"
	}
	annotation, present := "", false
	if snap != nil {
		annotation, present = snap.RollAnnotation, snap.RollAnnotationPresent
	}
	if present && annotation != "" {
		marker, err := decodeRoll(annotation)
		if err != nil || marker == nil || !marker.Completed {
			return "roll-reservation"
		}
		return ""
	}
	if e.rawReservation {
		// The annotation key exists on a claim but this snapshot shows no usable
		// reservation on /data: treat the reservation as unproven.
		return "roll-reservation"
	}
	return ""
}

// claimSnapshot selects the single snapshot a decision reads. DinD, and /data of a
// worker with no roll entry, use Observe's claim. /data with a roll entry uses the
// roll snapshot's claim exclusively, which must agree with Observe on identity.
func claimSnapshot(w protocol.DesiredWorker, kind claimKind, e *rollEntry, obs reconcile.ObservedWorker) (*reconcile.ClaimObservation, string) {
	if kind == claimDinD {
		return obs.DinDClaim, ""
	}
	if e == nil {
		return obs.DataClaim, ""
	}
	if e.claim == nil && obs.DataClaim == nil {
		return nil, ""
	}
	if e.claim == nil || obs.DataClaim == nil || string(e.claim.UID) != obs.DataClaim.UID {
		return nil, "claim-identity-changed"
	}
	s := claimObservationOf(e.ns, w.ID, e.claim)
	return &s, ""
}

// claimDecision applies the gate and then the identity agreement. The returned
// snapshot may be nil.
func claimDecision(w protocol.DesiredWorker, kind claimKind, tick *rollTick, e *rollEntry, obs reconcile.ObservedWorker) (*reconcile.ClaimObservation, string) {
	snap, identity := claimSnapshot(w, kind, e, obs)
	uncertain := e != nil && tick != nil && (tick.uncertain[coverageKey(e.coverage)] || migrationLaneFenced(tick, e.coverage))
	if reason := claimGate(w, kind, e, uncertain, snap); reason != "" {
		return snap, reason
	}
	return snap, identity
}

// kindOfClaim maps a rendered claim name to its kind; ok is false for /nix.
func kindOfClaim(id, name string) (claimKind, bool) {
	switch name {
	case dataPVCName(id):
		return claimData, true
	case dindDataPVCName(id):
		return claimDinD, true
	}
	return 0, false
}

// expandClaims converges the worker's existing /data and DinD claims upward. It
// logs and continues on every failure: a refused expansion must not abort the
// reconcile of the Deployment below it.
func (m *Materializer) expandClaims(ctx context.Context, w protocol.DesiredWorker, obs reconcile.ObservedWorker, ns string, spec preset.Spec) {
	tick, _ := ctx.Value(rollTickKey{}).(*rollTick)
	e := rollFrom(ctx, w.ID)
	for _, pvc := range RenderPVCs(m.cfg, w, spec) {
		kind, ok := kindOfClaim(w.ID, pvc.Name)
		if !ok {
			continue
		}
		snap, reason := claimDecision(w, kind, tick, e, obs)
		if reason != "" {
			continue // the reporter logs the deferral
		}
		if snap == nil {
			continue
		}
		desired := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		wanted := desired.Cmp(snap.Requested) > 0
		skip := func(why string) {
			if wanted {
				m.claimEvent(w.ID, ns, pvc.Name, "expand", "pvc-expand-skipped", why, "")
			}
		}
		switch {
		case snap.Phase != string(corev1.ClaimBound):
			skip("not-bound")
			continue
		case snap.Terminating:
			skip("terminating")
			continue
		case snap.UID == "" || snap.ResourceVersion == "":
			skip("no-identity")
			continue
		case snap.Name != pvc.Name || snap.WorkerID != w.ID || snap.Namespace != ns:
			skip("foreign")
			continue
		}
		if !wanted {
			continue
		}
		if snap.StorageClassName == nil || *snap.StorageClassName == "" {
			skip("no-storage-class")
			continue
		}
		if why := m.storageClassVerdict(ctx, tick, *snap.StorageClassName); why != "" {
			skip(why)
			continue
		}
		body, err := pvcExpandPatchBody(snap.UID, snap.ResourceVersion, desired.String())
		if err != nil {
			m.claimEvent(w.ID, ns, pvc.Name, "expand", "pvc-expand-failed", "encode", truncateClaimError(err))
			continue
		}
		got, err := m.client.CoreV1().PersistentVolumeClaims(ns).Patch(ctx, pvc.Name, types.MergePatchType, body, metav1.PatchOptions{})
		if err != nil {
			m.claimEvent(w.ID, ns, pvc.Name, "expand", "pvc-expand-failed", patchFailureReason(err), truncateClaimError(err))
			continue
		}
		if kind == claimData && e != nil && got != nil {
			e.claim = got // later mirrorRoll writes in this tick fence on the refreshed identity
		}
		from, to := snap.Requested, desired
		m.claimEvent(w.ID, ns, pvc.Name, "expand", "pvc-expand-requested", "desired-larger", fmt.Sprintf("from=%s to=%s", from.String(), to.String()))
	}
}

// pvcExpandPatchBody encodes the fenced merge patch whole, so no value is spliced into
// hand-written JSON. Field order is fixed by the struct: metadata.uid, then
// metadata.resourceVersion (the apiserver's preconditions), then the storage request.
func pvcExpandPatchBody(uid, resourceVersion, storage string) ([]byte, error) {
	type requests struct {
		Storage string `json:"storage"`
	}
	type resources struct {
		Requests requests `json:"requests"`
	}
	type spec struct {
		Resources resources `json:"resources"`
	}
	type metadata struct {
		UID             string `json:"uid"`
		ResourceVersion string `json:"resourceVersion"`
	}
	return json.Marshal(struct {
		Metadata metadata `json:"metadata"`
		Spec     spec     `json:"spec"`
	}{
		Metadata: metadata{UID: uid, ResourceVersion: resourceVersion},
		Spec:     spec{Resources: resources{Requests: requests{Storage: storage}}},
	})
}

func patchFailureReason(err error) string {
	switch {
	case apierrors.IsConflict(err):
		return "conflict"
	case apierrors.IsInvalid(err):
		return "invalid"
	case apierrors.IsForbidden(err):
		return "forbidden"
	}
	return "error"
}

func truncateClaimError(err error) string {
	s := strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return ' '
		}
		return r
	}, err.Error())
	if len(s) > maxClaimErrorLen {
		s = s[:maxClaimErrorLen]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return "error=" + fmt.Sprintf("%q", s)
}

// storageClassVerdict returns "" when the class permits volume expansion, else the
// skip reason. It is memoised for the tick; a lookup failure of any kind (including
// NotFound) fails closed and leaves the claim untouched.
func (m *Materializer) storageClassVerdict(ctx context.Context, tick *rollTick, name string) string {
	if tick != nil {
		if v, ok := tick.storageClasses[name]; ok {
			return v
		}
	}
	verdict := ""
	sc, err := m.client.StorageV1().StorageClasses().Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsForbidden(err):
		verdict = "class-lookup-forbidden"
	case err != nil || sc == nil:
		verdict = "class-lookup-failed"
	case sc.AllowVolumeExpansion == nil || !*sc.AllowVolumeExpansion:
		verdict = "class-not-expandable"
	}
	if tick != nil {
		if tick.storageClasses == nil {
			tick.storageClasses = map[string]string{}
		}
		tick.storageClasses[name] = verdict
	}
	return verdict
}

// resizeState classifies a claim's resize progress from its observed status only.
func resizeState(c *reconcile.ClaimObservation) (token, reason string) {
	statuses := make([]string, 0, len(c.AllocatedResourceStatuses))
	for _, v := range c.AllocatedResourceStatuses {
		statuses = append(statuses, v)
	}
	sort.Strings(statuses)
	for _, v := range statuses {
		if strings.HasSuffix(v, "Failed") || strings.HasSuffix(v, "Infeasible") {
			return "pvc-resize-failed", "status-" + v
		}
	}
	for _, t := range c.ResizeConditions {
		if t == "ControllerResizeError" || t == "NodeResizeError" {
			return "pvc-resize-failed", "condition-" + t
		}
	}
	if c.Capacity != nil && c.Capacity.Cmp(c.Requested) < 0 {
		return "pvc-resize-pending", "capacity-below-request"
	}
	for _, t := range c.ResizeConditions {
		if t == "FileSystemResizePending" || t == "Resizing" {
			return "pvc-resize-pending", "condition-" + t
		}
	}
	for _, v := range statuses {
		if v == "ControllerResizeInProgress" || v == "NodeResizePending" || v == "NodeResizeInProgress" {
			return "pvc-resize-pending", "status-" + v
		}
	}
	return "", ""
}

// reportClaimStates is the read-only reporter: no API calls. It logs resize
// progress and deferred expansion through the same gate the mutator uses.
func (m *Materializer) reportClaimStates(desired []protocol.DesiredWorker, observed map[string]reconcile.ObservedWorker, tick *rollTick) {
	live := map[string]bool{}
	for _, o := range observed {
		for _, c := range []*reconcile.ClaimObservation{o.DataClaim, o.DinDClaim} {
			if c != nil {
				live[c.Namespace+"/"+c.Name] = true
			}
		}
	}
	m.pruneClaimLog(live)
	for _, w := range desired {
		obs, ok := observed[w.ID]
		if !ok {
			continue
		}
		ns, err := m.cfg.Placement(w)
		if err != nil {
			continue
		}
		spec, err := m.resolver.Resolve(w.Template, w.Size)
		if err != nil {
			continue
		}
		e := tick.entries[w.ID]
		if e != nil {
			obs = e.o
		}
		for _, pvc := range RenderPVCs(m.cfg, w, spec) {
			kind, ok := kindOfClaim(w.ID, pvc.Name)
			if !ok {
				continue
			}
			observedClaim := obs.DataClaim
			if kind == claimDinD {
				observedClaim = obs.DinDClaim
			}
			if observedClaim != nil && !observedClaim.Terminating {
				if token, reason := resizeState(observedClaim); token != "" {
					m.claimEvent(w.ID, ns, pvc.Name, "resize", token, reason, "")
				} else {
					m.clearClaimEvent(ns, pvc.Name, "resize")
				}
			}
			snap, reason := claimDecision(w, kind, tick, e, obs)
			if snap == nil {
				snap = observedClaim
			}
			if snap == nil {
				continue
			}
			want := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
			if want.Cmp(snap.Requested) <= 0 {
				m.clearClaimEvent(ns, pvc.Name, "expand")
				continue
			}
			switch reason {
			case "maintenance", "roll-uncertain", "roll-admission", "roll-reservation", "claim-identity-changed":
				m.claimEvent(w.ID, ns, pvc.Name, "expand", "pvc-expand-deferred", reason, "")
			}
		}
	}
}

// claimEvent logs one pvc-* token, only when it differs from the last one logged for
// the same claim and category. State is process-local: a restart re-logs once.
func (m *Materializer) claimEvent(workerID, ns, name, category, token, reason, extra string) {
	key := ns + "/" + name + "#" + category
	state := token + "|" + reason
	m.claimLogMu.Lock()
	if m.claimLog == nil {
		m.claimLog = map[string]string{}
	}
	if m.claimLog[key] == state {
		m.claimLogMu.Unlock()
		return
	}
	m.claimLog[key] = state
	m.claimLogMu.Unlock()
	line := fmt.Sprintf("%s worker=%s pvc=%s reason=%s", token, workerID, name, reason)
	if extra != "" {
		line += " " + extra
	}
	m.log.Info(line)
}

func (m *Materializer) clearClaimEvent(ns, name, category string) {
	m.claimLogMu.Lock()
	delete(m.claimLog, ns+"/"+name+"#"+category)
	m.claimLogMu.Unlock()
}

// pruneClaimLog drops dedup state for claims that were not observed this tick.
func (m *Materializer) pruneClaimLog(live map[string]bool) {
	m.claimLogMu.Lock()
	defer m.claimLogMu.Unlock()
	for key := range m.claimLog {
		claim, _, _ := strings.Cut(key, "#")
		if !live[claim] {
			delete(m.claimLog, key)
		}
	}
}
