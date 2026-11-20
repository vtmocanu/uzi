package kube

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

// Duplicate canary reservations (issue #4475). planPool keeps two canary reservations
// held unless both prove a current old pod. One narrow shape is provably safe to
// settle: a recycled participant (scaled to zero, no pod, no storage) that a stale
// canary reservation was copied onto, next to a current canary (currentSurvivor needs
// one replica and not absent; it requires no serving proof). The
// stale reservation protects nothing, so it is cancelled by one conditioned
// metadata-only patch and the pool is re-observed.

const duplicateCancelledReason = "duplicate canary cancelled for absent worker; re-observation required"

// storageFact is one namespace PVC as listed, whatever its labels.
type storageFact struct {
	name     string
	workerID string
}

// replicaSetAbsenceFact retains ownership and desired replicas from the full list,
// including terminating, foreign-owned and oddly named ReplicaSets.
type replicaSetAbsenceFact struct {
	owners        []metav1.OwnerReference
	uid, name     string
	workerID      string
	replicas      int32
	replicasKnown bool
}

// replicaSetOwners indexes absence facts by UID and name. rollEntry.sets keeps the
// narrower serving-proof ancestry unchanged.
type replicaSetOwners map[string]replicaSetAbsenceFact

func (r replicaSetOwners) record(rs *appsv1.ReplicaSet) {
	var owners []metav1.OwnerReference
	for _, ref := range rs.OwnerReferences {
		if ref.Kind == "Deployment" {
			owners = append(owners, ref)
		}
	}
	fact := replicaSetAbsenceFact{owners: owners, uid: string(rs.UID), name: rs.Name,
		workerID: rs.Labels[LabelWorkerID], replicasKnown: rs.Spec.Replicas != nil}
	if rs.Spec.Replicas != nil {
		fact.replicas = *rs.Spec.Replicas
	}
	if rs.UID != "" {
		r["uid:"+string(rs.UID)] = fact
	}
	r["name:"+rs.Name] = fact
}

// lookup resolves a pod's ReplicaSet owner reference and reports whether that exact
// ReplicaSet was listed. A reference carrying a UID resolves by UID only: a listed
// ReplicaSet that merely reuses the name under another UID is a different object, so the
// reference is reported unlisted and the caller's name-prefix safeguard applies. Only a
// UID-less reference falls back to the name.
func (r replicaSetOwners) lookup(ref metav1.OwnerReference) (replicaSetAbsenceFact, bool) {
	if ref.UID != "" {
		o, ok := r["uid:"+string(ref.UID)]
		return o, ok
	}
	o, ok := r["name:"+ref.Name]
	return o, ok
}

// pendingForWorker counts possible pod producers, including terminating sets and
// sets whose ownership labels or references have been removed. A nil replicas field
// is not zero (the apiserver normally defaults it to one).
func (r replicaSetOwners) pendingForWorker(e *rollEntry) int {
	n := 0
	for key, fact := range r {
		if strings.HasPrefix(key, "name:") && fact.uid != "" {
			continue // Count UID-bearing sets once.
		}
		if fact.uid != "" && fact.replicasKnown && fact.replicas == 0 {
			continue
		}
		matches := fact.workerID == e.w.ID || strings.HasPrefix(fact.name, deploymentName(e.w.ID)+"-")
		for _, ref := range fact.owners {
			matches = matches || deploymentRefMatches(e, ref)
		}
		if matches {
			n++
		}
	}
	return n
}

func canonicalClaimNames(id string) []string {
	return []string{dataPVCName(id), nixPVCName(id), dindDataPVCName(id)}
}

func isCanonicalClaim(id, name string) bool {
	for _, c := range canonicalClaimNames(id) {
		if c == name {
			return true
		}
	}
	return false
}

// countStorageObserved counts every claim that belongs to worker id: labelled with its
// id whatever its provenance, or bearing one of its canonical names whatever its labels.
func countStorageObserved(id string, claims []storageFact) int {
	n := 0
	for _, c := range claims {
		if c.workerID == id || isCanonicalClaim(id, c.name) {
			n++
		}
	}
	return n
}

// podObservedForWorker reports whether pod could belong to e's worker by any identity
// signal, ignoring deletion state, labels it may have lost, and naming conventions.
func podObservedForWorker(e *rollEntry, pod corev1.Pod) bool {
	id := e.w.ID
	prefix := deploymentName(id) + "-"
	if pod.Labels[LabelWorkerID] == id || strings.HasPrefix(pod.Name, prefix) {
		return true
	}
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && isCanonicalClaim(id, v.PersistentVolumeClaim.ClaimName) {
			return true
		}
	}
	for _, ref := range pod.OwnerReferences {
		switch ref.Kind {
		case "Deployment":
			if deploymentRefMatches(e, ref) {
				return true
			}
		case "ReplicaSet":
			fact, listed := e.absenceOwners.lookup(ref)
			if listed && fact.workerID == id {
				return true
			}
			for _, o := range fact.owners {
				if deploymentRefMatches(e, o) {
					return true
				}
			}
			if strings.HasPrefix(ref.Name, prefix) {
				return true
			}
		}
	}
	return false
}

func deploymentRefMatches(e *rollEntry, ref metav1.OwnerReference) bool {
	if ref.Kind != "Deployment" {
		return false
	}
	return ref.Name == deploymentName(e.w.ID) || (ref.UID != "" && e.dep != nil && ref.UID == e.dep.UID)
}

func liveRollDeployment(e *rollEntry) bool {
	return e.dep != nil && e.dep.UID != "" && e.dep.ResourceVersion != "" && e.dep.DeletionTimestamp == nil
}

// physicallyAbsent is physical evidence only: a scaled-to-zero Deployment with no pod,
// no storage, no pending ReplicaSet producer and no PVC user in the namespace listings.
func physicallyAbsent(e *rollEntry) bool {
	return e.dep != nil && e.dep.Spec.Replicas != nil && *e.dep.Spec.Replicas == 0 &&
		e.replicaSetsPending == 0 &&
		len(e.pods) == 0 && e.podsObserved == 0 &&
		e.claim == nil && !e.rawReservation && e.storageObserved == 0 &&
		len(e.o.PVCUserPodUIDs) == 0
}

// staleDuplicateEligible reports a canary reservation that no longer matches its own
// Deployment's template and is not shielded by custody, maintenance, work or hibernation.
// The Deployment's pin may be older than the survivor's, so equality is not required.
func staleDuplicateEligible(e *rollEntry) bool {
	r := e.marker
	return r != nil && r.Canary && !r.Completed && e.dep != nil &&
		e.dep.Spec.Template.Annotations[AnnotationSpecHash] != r.Hash && e.drift &&
		!e.w.CustodyHeld && !maintenanceActive(e.w) && !e.w.Busy && e.w.Hibernation == nil
}

// currentSurvivor reports an incomplete canary reservation that matches the rendered
// and deployed target exactly, on a live one-replica Deployment that is not absent.
// It is not the negation of the stale predicate.
func currentSurvivor(t *rollTick, e *rollEntry) bool {
	r := e.marker
	if r == nil || !r.Canary || r.Completed || e.w.CustodyHeld || e.rawReservation || !liveRollDeployment(e) || physicallyAbsent(e) ||
		e.dep.Spec.Replicas == nil || *e.dep.Spec.Replicas != 1 || e.o.Roll.Phase == protocol.PhaseStuck ||
		(e.claim != nil && e.claim.Annotations[annotationRoll] != "") {
		return false
	}
	version := t.outcomes[e.w.ID].TargetVersion
	if version == "" || r.TargetVersion != version || r.Hash != e.target.Spec.Template.Annotations[AnnotationSpecHash] ||
		r.Image != workerImage(e.target) || e.dep.Spec.Template.Annotations[AnnotationSpecHash] != r.Hash || workerImage(e.dep) != r.Image {
		return false
	}
	c, ok := coverageOf(e.w, e.ns)
	// Like the pool coverage-key check, an owner/lane mismatch is blocked in planRoll
	// before pooling, so this is defensive.
	return ok && r.Coverage.Owner == c.Owner && r.Coverage.Lane == e.ns
}

// cancelAbsentDuplicateCanary settles a two-canary pool when exactly one canary D is
// provably absent and stale and the other S is a current canary (see currentSurvivor;
// no serving proof is required or granted). It returns
// true when it wrote and fully settled the tick's outcomes for the pool (the caller
// then returns); on any refusal it writes nothing and the caller's hold applies. A
// failed write marks the pool uncertain and records an error, then returns false.
func (m *Materializer) cancelAbsentDuplicateCanary(ctx context.Context, t *rollTick, pool, canaries []*rollEntry) bool {
	if len(canaries) != 2 || len(pool) == 0 {
		return false
	}
	ns, key := pool[0].ns, coverageKey(pool[0].coverage)
	owner, lane := pool[0].coverage.Owner, pool[0].coverage.Lane
	inPool := map[*rollEntry]bool{}
	// Defence in depth: several guards below overlap. identityAmbiguous/strayReservation
	// are namespace-wide (also refused in the peer loop); an undecodable Deployment or
	// claim annotation also blocks the entry in rollSnapshot, but the bare rawReservation
	// arm (annotation key present with an empty value, or on a non-data claim) has no
	// upstream block and is pinned by the "empty-valued annotation on its data claim"
	// row of TestDuplicateCanaryAbsenceProofFailClosed; a lane migration fence also marks
	// the pool key uncertain. Each is kept so that no single upstream change can open the write.
	for _, e := range pool {
		inPool[e] = true
		if e.hibernation || e.w.Hibernation != nil || e.mutated || t.unavailable[e.w.ID] || e.snapshotIncomplete || e.blocked != "" ||
			e.identityAmbiguous || e.strayReservation {
			return false
		}
		if e.marker == nil && (e.rawReservation || (e.claim != nil && e.claim.Annotations[annotationRoll] != "") || (e.dep != nil && e.dep.Annotations[annotationRoll] != "")) {
			return false
		}
	}
	if t.uncertain[key] || migrationLaneFenced(t, pool[0].coverage) || t.layoutUncertain[key] != "" {
		return false
	}
	for _, u := range t.unplanned {
		if !u.nsKnown || u.ns == ns {
			return false
		}
	}
	// planRoll groups a pool by the coverage key of the entry's own valid marker, and an
	// entry whose marker owner or lane does not match is blocked (refused above), so
	// every canary here already carries the pool's key; this check is defensive and has
	// no reachable failing input from planPool.
	for _, c := range canaries {
		if coverageKey(c.marker.Coverage) != key {
			return false
		}
	}
	for _, p := range t.entries {
		if p.ns != ns {
			continue
		}
		if p.identityAmbiguous || p.strayReservation {
			return false
		}
		if inPool[p] {
			continue
		}
		if p.marker != nil && p.marker.Canary && p.marker.Coverage.Owner == owner && p.marker.Coverage.Lane == lane {
			return false
		}
		if (p.marker != nil || p.rawReservation || (p.claim != nil && p.claim.Annotations[annotationRoll] != "") || hasOpenReservation(p)) &&
			!rollPeerOutOfScope(p, owner) {
			return false
		}
	}

	var d, s *rollEntry
	for i, c := range canaries {
		if liveRollDeployment(c) && physicallyAbsent(c) && staleDuplicateEligible(c) {
			if d != nil {
				return false
			}
			d, s = c, canaries[1-i]
		}
	}
	if d == nil || !currentSurvivor(t, s) {
		return false
	}

	// rollSnapshot reads pods before ReplicaSets. Recheck after the sets have
	// settled to zero so a pod created between those observations cannot be missed.
	// A failed or incomplete list proves no absence and leaves the duplicate hold.
	pods, err := m.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil || pods.Continue != "" {
		return false
	}
	for _, pod := range pods.Items {
		if podObservedForWorker(d, pod) {
			return false
		}
	}

	err = m.reserveRoll(ctx, d, nil)
	t.uncertain[key] = true
	if err != nil {
		t.errors = append(t.errors, fmt.Errorf("worker %s: duplicate canary cancellation unconfirmed: %w; re-observe and re-poll required", d.w.ID, err))
		return false
	}
	for _, e := range pool {
		p := t.outcomes[e.w.ID]
		e.admitted = false
		e.blocked = duplicateCancelledReason
		p.State = "held"
		p.BlockingWorkerID = pacingText(d.w.ID)
		p.BlockingReason = pacingText(e.blocked)
	}
	return true
}
