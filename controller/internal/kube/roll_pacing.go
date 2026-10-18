package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

// Roll metadata is outside the pod template and therefore outside its hash.
const annotationRoll = "uzi.dev/image-roll"
const maxRollMarker = 8192

type rollCoverage struct {
	Owner      string   `json:"owner"`
	Lane       string   `json:"lane"`
	Scheduling []string `json:"scheduling"`
	Protocol   []string `json:"protocol"`
	Ordinary   bool     `json:"ordinary"`
	Checker    bool     `json:"checker"`
}
type rollMarker struct {
	Version         int          `json:"version"`
	Hash            string       `json:"hash"`
	Image           string       `json:"image"`
	TargetVersion   string       `json:"target_version"`
	Coverage        rollCoverage `json:"coverage"`
	Nonce           string       `json:"nonce"`
	PreviousVersion string       `json:"previous_version"`
	Canary          bool         `json:"canary"`
	Completed       bool         `json:"completed"`
}
type rollEntry struct {
	w              protocol.DesiredWorker
	o              reconcile.ObservedWorker
	target         *appsv1.Deployment
	dep            *appsv1.Deployment
	marker         *rollMarker
	pods           []corev1.Pod
	volumeRefs     map[string]bool
	sets           map[string]appsv1.ReplicaSet
	coverage       rollCoverage
	ns             string
	drift          bool
	admitted       bool
	blocked        string
	recovery       bool
	claim          *corev1.PersistentVolumeClaim
	rawReservation bool
}
type rollTick struct {
	entries   map[string]*rollEntry
	outcomes  map[string]*protocol.Pacing
	start     time.Time
	uncertain map[string]bool
	errors    []error
}
type rollTickKey struct{}

// PacingOutcomes is replaced on every synchronous reconcile, never used to admit.
func (m *Materializer) PacingOutcomes() map[string]*protocol.Pacing {
	out := map[string]*protocol.Pacing{}
	for id, p := range m.pacing {
		if p.State != "completed" {
			out[id] = p
		}
	}
	return out
}

func (m *Materializer) planRoll(ctx context.Context, desired []protocol.DesiredWorker, observed map[string]reconcile.ObservedWorker) (*rollTick, []protocol.DesiredWorker) {
	t := &rollTick{entries: map[string]*rollEntry{}, outcomes: map[string]*protocol.Pacing{}, start: m.now(), uncertain: map[string]bool{}}
	if start, ok := reconcile.PollStart(ctx); ok {
		t.start = start
	}
	namespaces := map[string][]*rollEntry{}
	for _, w := range desired {
		if w.Ephemeral {
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
		target := RenderDeployment(m.cfg, w, spec)
		e := &rollEntry{w: w, o: observed[w.ID], target: target, ns: ns}
		t.entries[w.ID] = e
		namespaces[ns] = append(namespaces[ns], e)
		t.outcomes[w.ID] = &protocol.Pacing{State: "waiting_evidence", TargetImage: workerImage(target), TargetVersion: targetVersion(m.cfg, workerImage(target))}
	}
	for ns, entries := range namespaces {
		if err := m.rollSnapshot(ctx, ns, entries); err != nil {
			for _, e := range entries {
				e.blocked = "incomplete deployment/pod/ancestry snapshot"
			}
		}
	}
	// Retire completed history only after every participant has been cleaned up.
	// Do this before grouping so a new pin uses the current API capability set.
	for _, e := range t.entries {
		if e.dep != nil {
			e.drift = e.dep.Spec.Template.Annotations[AnnotationSpecHash] != e.target.Spec.Template.Annotations[AnnotationSpecHash] ||
				e.dep.Spec.Template.Annotations[AnnotationGeneration] != e.target.Spec.Template.Annotations[AnnotationGeneration]
		}
	}
	for _, e := range t.entries {
		if e.blocked != "" || e.marker == nil || !e.marker.Completed || e.dep == nil {
			continue
		}
		pending, drift := false, e.drift
		for _, peer := range t.entries {
			c, ok := coverageOf(peer.w, peer.ns)
			drift = drift || (ok && c.Owner == e.marker.Coverage.Owner && c.Lane == e.marker.Coverage.Lane && peer.drift)
			if peer.marker != nil && !peer.marker.Completed && coverageKey(peer.marker.Coverage) == coverageKey(e.marker.Coverage) {
				pending = true
			}
		}
		if pending || !drift {
			continue
		}
		if e.claim != nil && e.claim.Annotations[annotationRoll] != "" {
			if err := m.mirrorRoll(ctx, e, nil); err != nil {
				e.blocked = "historic mirror cancellation unconfirmed"
				continue
			}
		}
		if err := m.reserveRoll(ctx, e, nil); err != nil {
			e.blocked = "historic marker cancellation unconfirmed"
		}
	}
	pools := map[string][]*rollEntry{}
	for _, e := range t.entries {
		m.releasePressureHold(ctx, t, e)
		if firstProvision(e) {
			e.admitted = true
			t.outcomes[e.w.ID].State = "completed"
			continue
		}
		// An unchanged busy worker can acquire its independent pressure cordon
		// without an exact image-roll version. This admits no replacement.
		if t.outcomes[e.w.ID].TargetVersion == "" && e.blocked == "" && e.dep != nil &&
			!e.drift && e.marker == nil && !e.rawReservation && e.w.Busy && e.w.DiskPressure &&
			e.w.DrainingSince == nil && e.w.DrainingReason == nil && !e.w.CustodyHeld && !maintenanceActive(e.w) {
			e.admitted = true
			t.outcomes[e.w.ID].State = "completed"
			continue
		}
		coverage, ok := coverageOf(e.w, e.ns)
		if e.marker != nil {
			if !ok || e.marker.Coverage.Owner != coverage.Owner || e.marker.Coverage.Lane != e.ns {
				e.blocked = "reservation owner or placement does not match API evidence"
			} else {
				coverage = e.marker.Coverage
			}
		}
		e.coverage = coverage
		if !ok && e.blocked == "" {
			e.blocked = "API coverage evidence unavailable; explicit recovery required"
		}
		if e.dep == nil && e.marker == nil && !independentRebuild(e) && e.blocked == "" {
			e.blocked = "missing deployment lacks first-provision evidence; explicit recovery required"
		}
		if t.outcomes[e.w.ID].TargetVersion == "" {
			e.blocked = "target version unknown; configure exact override"
		}
		if m.drain.ForceRoll {
			e.admitted = true
			t.outcomes[e.w.ID].State = "override"
			m.log.Warn("operator force-roll bypasses capacity and incarnation proof", "worker_id", e.w.ID)
			continue
		}
		if !ok {
			// Legacy independent DinD has its own fresh identity and pod-absence
			// observations. Missing image evidence must not suppress those checks.
			if e.w.RollEvidence == nil && e.marker == nil && !e.drift && maintenanceActive(e.w) &&
				(e.blocked == "API coverage evidence unavailable; explicit recovery required" ||
					e.blocked == "deployment missing from snapshot" || e.blocked == "incomplete deployment/pod/ancestry snapshot") {
				e.admitted = true
				e.blocked = ""
			}
			// API skew must not prevent first provisioning or independent maintenance
			// of an unchanged template. No drift or reservation is admitted here.
			if e.blocked == "API coverage evidence unavailable; explicit recovery required" && e.marker == nil && !e.drift && t.outcomes[e.w.ID].TargetVersion != "" && e.dep != nil {
				e.admitted = true
				e.blocked = ""
			}
			t.outcomes[e.w.ID].BlockingReason = pacingText(e.blocked)
			continue
		}
		pools[coverageKey(coverage)] = append(pools[coverageKey(coverage)], e)
	}
	// Pools are independent. Sort both pool and participant iteration for replay.
	keys := []string{}
	for key := range pools {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		m.planPool(ctx, t, pools[key])
	}
	order := append([]protocol.DesiredWorker{}, desired...)
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].Busy != order[j].Busy {
			return !order[i].Busy
		}
		return order[i].ID < order[j].ID
	})
	for _, e := range t.entries {
		if strings.Contains(e.blocked, "unconfirmed") {
			t.uncertain[coverageKey(e.coverage)] = true
			t.errors = append(t.errors, fmt.Errorf("worker %s: %s; re-observe and re-poll required", e.w.ID, e.blocked))
		}
	}
	m.pacing = t.outcomes
	return t, order
}
func pacingText(s string) *string {
	if s == "" {
		return nil
	}
	s = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return ' '
		}
		return r
	}, s)
	if len(s) > 192 {
		s = s[:192]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return &s
}
func (m *Materializer) planPool(ctx context.Context, t *rollTick, pool []*rollEntry) {
	for _, e := range pool {
		if e.blocked == "" && m.promoted(t, e) {
			m.releaseRollHold(ctx, t, e)
		}
	}
	sort.Slice(pool, func(i, j int) bool {
		if pool[i].w.Busy != pool[j].w.Busy {
			return !pool[i].w.Busy
		}
		return pool[i].w.ID < pool[j].w.ID
	})
	// A completed canary closes the original cohort even across partial cleanup
	// and pin changes. Unresolved replacements retain their original evidence.
	for _, e := range pool {
		if e.blocked != "" || e.marker == nil || !e.marker.Completed ||
			(e.dep == nil && e.recovery && (e.marker.Hash != e.target.Spec.Template.Annotations[AnnotationSpecHash] || e.marker.Image != workerImage(e.target) || e.marker.TargetVersion != t.outcomes[e.w.ID].TargetVersion)) {
			continue
		}
		for _, peer := range pool {
			// Current-incarnation proof retires completed pacing metadata only;
			// this does not release source custody or mutate protected storage.
			if peer.blocked == "" && peer.marker != nil && !peer.marker.Canary && m.promoted(t, peer) {
				if err := m.reserveRoll(ctx, peer, nil); err != nil {
					peer.blocked = "completion cleanup unconfirmed"
				}
			}
			if peer.blocked != "" || (peer.marker != nil && !peer.marker.Completed && !m.promoted(t, peer)) || peer.drift {
				t.outcomes[peer.w.ID].State = "held"
				t.outcomes[peer.w.ID].BlockingReason = pacingText("completed cohort cleanup pending; explicit recovery required if proof is unavailable")
			} else {
				t.outcomes[peer.w.ID].State = "completed"
				peer.admitted = true
			}
		}
		return
	}
	// Duplicate reservations are cancelled only with current physical old-pod
	// proof and no evidence of drain, recycle, or template replacement. At most
	// len(pool) conditioned writes are attempted; uncertainty blocks the cohort.
	var canaries []*rollEntry
	for _, e := range pool {
		if e.marker != nil && e.marker.Canary {
			canaries = append(canaries, e)
		}
	}
	if len(canaries) > 1 {
		safe := true
		for _, e := range canaries {
			proof := e.w.RollEvidence
			if e.w.CustodyHeld || e.blocked != "" || !e.drift || e.dep == nil || e.dep.Spec.Replicas == nil || *e.dep.Spec.Replicas != 1 ||
				e.dep.Spec.Template.Annotations[AnnotationSpecHash] == e.marker.Hash || e.w.DrainingSince != nil ||
				(e.claim != nil && e.claim.Annotations[annotationRoll] != "") || len(e.pods) != 1 || !m.oldUsable(t, e) ||
				proof == nil || proof.RegisterNonce == nil || *proof.RegisterNonce == "" || *proof.RegisterNonce != e.marker.Nonce ||
				proof.RegisteredVersion == nil || *proof.RegisteredVersion != e.marker.PreviousVersion ||
				proof.PodUID == nil || *proof.PodUID == "" || *proof.PodUID != string(e.pods[0].UID) ||
				proof.HeartbeatAt == nil || proof.HeartbeatAt.IsZero() || proof.HeartbeatAt.After(proof.EvaluatedAt) ||
				!freshUntil(m.now(), t.start, proof.EvaluatedAt, proof.ValidUntil) {
				safe = false
			}
		}
		if safe {
			for _, e := range canaries[1:] {
				if err := m.reserveRoll(ctx, e, nil); err != nil {
					safe = false
					e.blocked = "duplicate cancellation unconfirmed; explicit recovery required"
					break
				}
			}
		}
		if !safe {
			for _, e := range pool {
				e.blocked = "duplicate canary reservations unresolved; explicit recovery required"
				t.outcomes[e.w.ID].BlockingReason = pacingText(e.blocked)
				m.releaseRollHold(ctx, t, e)
			}
			return
		}
	}
	// Obsolete markers are replaced/cancelled before admission. A successful
	// cancellation advances RV, fencing delayed patches from the retired pin.
	changed := false
	for _, e := range pool {
		if e.marker == nil {
			continue
		}
		if e.blocked != "" {
			continue
		}
		if e.marker.Hash == e.target.Spec.Template.Annotations[AnnotationSpecHash] && e.marker.Image == workerImage(e.target) && e.marker.TargetVersion != t.outcomes[e.w.ID].TargetVersion && t.outcomes[e.w.ID].TargetVersion != "" {
			r := *e.marker
			r.TargetVersion = t.outcomes[e.w.ID].TargetVersion
			r.Completed = false
			if e.claim != nil && e.claim.Annotations[annotationRoll] != "" {
				if err := m.mirrorRoll(ctx, e, &r); err != nil {
					e.blocked = "mapping mirror correction unconfirmed"
					continue
				}
			}
			if e.dep != nil {
				if err := m.reserveRoll(ctx, e, &r); err != nil {
					e.blocked = "mapping correction unconfirmed"
					continue
				}
			} else {
				e.marker = &r
			}
			continue
		}
		if e.marker.Hash != e.target.Spec.Template.Annotations[AnnotationSpecHash] || e.marker.Image != workerImage(e.target) || e.marker.TargetVersion != t.outcomes[e.w.ID].TargetVersion {
			if e.claim != nil && e.claim.Annotations[annotationRoll] != "" {
				if e.dep == nil {
					if t.outcomes[e.w.ID].TargetVersion == "" {
						e.blocked = "target version unknown; retained pin recovery required"
						continue
					}
					r := *e.marker
					r.Hash, r.Image, r.TargetVersion = e.target.Spec.Template.Annotations[AnnotationSpecHash], workerImage(e.target), t.outcomes[e.w.ID].TargetVersion
					r.Completed = false
					if err := m.mirrorRoll(ctx, e, &r); err != nil {
						e.blocked = "retained pin replacement unconfirmed"
					} else {
						e.marker = &r
						changed = true
					}
					continue
				}
				if err := m.mirrorRoll(ctx, e, nil); err != nil {
					e.blocked = "retained pin cancellation unconfirmed"
					continue
				}
			}
			if e.dep == nil {
				e.blocked = "pin reservation lost; explicit recovery required"
			} else if err := m.reserveRoll(ctx, e, nil); err != nil {
				e.blocked = "pin cancellation unconfirmed"
			} else {
				changed = true
			}
		}
	}
	canary := ""
	inFlight := 0
	active := false
	blocker := ""
	for _, e := range pool {
		if e.blocked != "" {
			blocker = e.w.ID
			continue
		}
		if e.marker != nil {
			if e.marker.Canary {
				if canary != "" {
					e.blocked = "duplicate canary reservations"
					blocker = e.w.ID
				}
				canary = e.w.ID
			}
			if !e.marker.Completed {
				active = true
				if !m.promoted(t, e) {
					inFlight++
				}
			}
		}
		active = active || e.drift
	}
	if changed {
		blocker = "pin-change"
	}
	if inFlight > 0 && canary == "" {
		blocker = "missing-canary"
		for _, e := range pool {
			if e.marker != nil {
				e.blocked = "participant reservation without canary; explicit recovery required"
			}
		}
	}
	allPromoted := canary != ""
	for _, e := range pool {
		if e.drift || (e.marker != nil && !m.promoted(t, e)) {
			allPromoted = false
		}
	}
	if allPromoted && blocker == "" {
		for _, e := range pool {
			if e.w.ID == canary {
				r := *e.marker
				r.Completed = true
				if err := m.reserveRoll(ctx, e, &r); err != nil {
					e.blocked = "completion mutation unconfirmed"
					t.outcomes[e.w.ID].State = "held"
					t.outcomes[e.w.ID].BlockingReason = pacingText(e.blocked)
				} else {
					for _, participant := range pool {
						t.outcomes[participant.w.ID].State = "completed"
					}
				}
			}
		}
		// Cleanup on the next tick, after the completed flag is observed.
		return
	}
	promotedCanary := false
	for _, e := range pool {
		if e.w.ID == canary {
			promotedCanary = m.promoted(t, e)
		}
		if active && e.marker != nil && e.o.Roll.Phase == protocol.PhaseStuck {
			blocker = e.w.ID
			e.blocked = e.o.Roll.BlockingReason
			if e.blocked == "" {
				e.blocked = "participant roll health stuck"
			}
		}
		if active && !e.drift && e.marker == nil && e.dep != nil && !maintenanceActive(e.w) && !m.currentUsable(t, e) {
			blocker = e.w.ID
			e.blocked = "target template has no reservation proof"
		}
	}
	concurrency := m.cfg.RollConcurrency
	if concurrency < 1 {
		concurrency = 1
	}
	budget := concurrency - inFlight
	if !promotedCanary {
		budget = 0
		if canary == "" && inFlight == 0 {
			budget = 1
		}
	}
	// Releases are attempted before sibling admission; uncertainty lasts this tick.
	for _, e := range pool {
		if blocker != "" || e.blocked != "" || (e.marker == nil && budget <= 0) {
			m.releaseRollHold(ctx, t, e)
		}
	}
	if t.uncertain[coverageKey(pool[0].coverage)] {
		for _, e := range pool {
			if strings.Contains(e.blocked, "release unconfirmed") {
				blocker = e.w.ID
				break
			}
		}
	}
	reason := "pool participant unresolved"
	for _, e := range pool {
		if e.w.ID == blocker && e.blocked != "" {
			reason = e.blocked
		}
	}
	for _, e := range pool {
		p := t.outcomes[e.w.ID]
		if canary != "" {
			id := canary
			p.CanaryID = &id
		}
		for _, old := range pool {
			if old.marker == nil && old.drift && m.oldUsable(t, old) {
				p.RetainedImage = pacingText(workerImage(old.dep))
				p.RetainedVersion = old.w.RollEvidence.RegisteredVersion
				break
			}
		}
		if blocker != "" {
			p.BlockingWorkerID = pacingText(blocker)
			p.BlockingReason = pacingText(reason)
		}
		if e.blocked != "" {
			p.State = "waiting_evidence"
			if e.o.Roll.Phase == protocol.PhaseStuck {
				p.State = "held"
			}
			p.BlockingReason = pacingText(e.blocked)
			continue
		}
		if !active {
			p.State = "completed"
			m.releaseRollHold(ctx, t, e)
			e.admitted = true
			if e.dep == nil && canary == "" && !independentRebuild(e) {
				canary = e.w.ID
				id := canary
				p.CanaryID = &id
				p.State = "active"
			}
			continue
		}
		if e.blocked != "" {
			p.BlockingReason = pacingText(e.blocked)
		}
		if blocker != "" {
			if e.recovery && e.blocked == "" {
				e.admitted = true
			}
			p.State = "held"
			p.BlockingWorkerID = pacingText(blocker)
			if p.BlockingReason == nil {
				p.BlockingReason = pacingText(reason)
			}
			m.releaseRollHold(ctx, t, e)
			continue
		}
		if e.dep == nil {
			// First creation supplies no capacity until current-pod proof is available.
			if e.o.HasDeployment {
				p.State = "waiting_evidence"
			} else {
				e.admitted = true
				p.State = "waiting_evidence"
			}
			continue
		}
		if e.marker != nil {
			if !e.drift && !m.promoted(t, e) {
				p.State = "waiting_evidence"
				p.BlockingReason = pacingText("waiting for current-pod registration and nonce heartbeat proof")
				e.admitted = true
				continue
			}
			if m.promoted(t, e) {
				p.State = "active"
			} else {
				if !m.remainingCoverage(t, pool, e) || (!promotedCanary && e.w.ID != canary) {
					p.State = "held"
					p.BlockingReason = pacingText("confirmed admission suspended for usable coverage")
					m.releaseRollHold(ctx, t, e)
				} else {
					e.admitted = true
					p.State = "active"
					if promotedCanary {
						p.State = "active"
					}
				}
			}
			continue
		}
		if !e.drift {
			p.State = "waiting_evidence"
			continue
		}
		if budget <= 0 {
			p.State = "waiting_evidence"
			id, predicate := canary, "promotion_unconfirmed"
			if promotedCanary {
				for _, peer := range pool {
					if peer.admitted && peer.drift && peer.marker == nil {
						id, predicate = peer.w.ID, "replacement_admitted"
						break
					}
					if peer.marker != nil && !peer.marker.Completed && !m.promoted(t, peer) {
						id, predicate = peer.w.ID, "replacement_in_flight"
						break
					}
				}
			}
			p.BlockingWorkerID = pacingText(id)
			p.BlockingReason = pacingText("no roll slot: " + id + " " + predicate)
			m.releaseRollHold(ctx, t, e)
			continue
		}
		if !m.remainingCoverage(t, pool, e) {
			p.State = "waiting_evidence"
			id, reason := m.coverageBlocker(t, e)
			p.BlockingWorkerID, p.BlockingReason = pacingText(id), pacingText(reason)
			m.releaseRollHold(ctx, t, e)
			continue
		}
		e.admitted = true
		budget--
		if canary == "" {
			canary = e.w.ID
			id := canary
			p.CanaryID = &id
			p.State = "active"
		} else {
			p.State = "active"
		}
	}
}
func (m *Materializer) remainingCoverage(t *rollTick, pool []*rollEntry, candidate *rollEntry) bool {
	// Capability pools share physical owner/lane capacity. A promoted incarnation
	// can supply it; an admitted old pod or unresolved replacement cannot.
	population, usable := 0, false
	for _, e := range t.entries {
		if e.coverage.Owner != candidate.coverage.Owner || e.ns != candidate.ns {
			continue
		}
		population++
		if e == candidate || e.blocked != "" {
			continue
		}
		if e.marker != nil {
			usable = usable || m.promoted(t, e) || m.currentUsable(t, e)
		} else if !e.admitted || (!e.drift && m.currentUsable(t, e)) {
			// Planning precedes reservation writes. Current proof for a drifted,
			// already admitted old template is not its future replacement.
			usable = usable || m.currentUsable(t, e) || m.oldUsable(t, e)
		}
	}
	if population > 1 && !usable {
		return false
	}
	if len(pool) == 1 {
		return true
	}
	for _, e := range t.entries {
		if e.ns != candidate.ns || !covers(e.w, candidate.coverage) || e.blocked != "" {
			continue
		}
		if e == candidate || (e.admitted && (e.drift || (!m.currentUsable(t, e) && !m.promoted(t, e)))) {
			continue
		}
		if e.marker != nil {
			if m.promoted(t, e) || m.currentUsable(t, e) {
				return true
			}
			continue
		}
		if m.currentUsable(t, e) || m.oldUsable(t, e) {
			return true
		}
	}
	return false
}

// coverageBlocker explains a failed remainingCoverage decision; it never grants
// admission. Pick a stable owner/lane peer so repeated polls name the same fence.
func (m *Materializer) coverageBlocker(t *rollTick, candidate *rollEntry) (string, string) {
	peers := []*rollEntry{}
	for _, peer := range t.entries {
		if peer != candidate && peer.ns == candidate.ns && peer.coverage.Owner == candidate.coverage.Owner {
			peers = append(peers, peer)
		}
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].w.ID < peers[j].w.ID })
	if len(peers) == 0 {
		return "", "no usable coverage: provider_evidence_unavailable"
	}
	peer := peers[0]
	proof := peer.w.RollEvidence
	predicate := "incarnation_proof_unconfirmed"
	switch {
	case peer.blocked != "":
		predicate = peer.blocked
	case peer.admitted && peer.drift:
		predicate = "roll_admitted"
	case peer.w.DiskPressure:
		predicate = "disk_pressure"
	case maintenanceActive(peer.w):
		predicate = "maintenance_active"
	case !covers(peer.w, candidate.coverage):
		predicate = "capability_mismatch"
	case !liveWorkerPod(peer):
		predicate = "live_pod_unconfirmed"
	case proof == nil || proof.LivenessValidUntil == nil:
		predicate = "liveness_unavailable"
	case !freshUntil(m.now(), t.start, proof.EvaluatedAt, *proof.LivenessValidUntil):
		predicate = "liveness_stale"
	case peer.w.DrainingSince != nil && (!imageRollDrain(peer.w) || !m.currentUsable(t, peer)):
		predicate = "draining"
	}
	return peer.w.ID, "no usable coverage: " + peer.w.ID + " " + predicate
}

func (m *Materializer) releaseRollHold(ctx context.Context, t *rollTick, e *rollEntry) {
	// Serving proof is independent of custody. Releasing a drain can refill a
	// source-protected worker, so retain this mutation's custody fence explicitly.
	if e.w.CustodyHeld || !imageRollDrain(e.w) || e.blocked != "" {
		return
	}
	copy := *e
	copy.w.DrainingSince = nil
	copy.w.DrainingReason = nil
	if !m.promoted(t, e) && ((e.marker != nil && !e.drift) || e.admitted || !m.oldUsable(t, &copy)) {
		return
	}
	if c, ok := m.cordoner.(rollCordoner); ok {
		if err := c.ClearDrainWithReason(ctx, e.w.ID, "image_roll"); err != nil {
			e.blocked = "roll-owned release unconfirmed; re-poll required"
			t.uncertain[coverageKey(e.coverage)] = true
			t.outcomes[e.w.ID].BlockingReason = pacingText(e.blocked)
		} else {
			e.w.DrainingSince, e.w.DrainingReason = nil, nil
		}
	}
}
func (m *Materializer) confirmAdmission(ctx context.Context, t *rollTick, e *rollEntry) error {
	if firstProvision(e) || e.coverage.Owner == "" || e.marker != nil || m.drain.ForceRoll || (e.dep != nil && !e.drift) {
		return nil
	}
	p := t.outcomes[e.w.ID]
	if independentRebuild(e) && p.State == "completed" {
		return nil // An unchanged independent recovery is not an image cohort.
	}
	r := &rollMarker{Version: 1, Hash: e.target.Spec.Template.Annotations[AnnotationSpecHash], Image: workerImage(e.target), TargetVersion: p.TargetVersion, Coverage: e.coverage, Canary: p.CanaryID != nil && *p.CanaryID == e.w.ID}
	if proof := e.w.RollEvidence; proof != nil {
		if proof.RegisterNonce != nil {
			r.Nonce = *proof.RegisterNonce
		}
		if proof.RegisteredVersion != nil {
			r.PreviousVersion = *proof.RegisteredVersion
		}
	}
	if e.dep == nil {
		// Initial create atomically carries evidence; it establishes no usable
		// capacity until this incarnation registers and proves its nonce.
		e.marker = r
		return nil
	}
	return m.reserveRoll(ctx, e, r)
}

// firstProvision requires positive API history evidence and a complete cluster snapshot.
func firstProvision(e *rollEntry) bool {
	p := e.w.RollEvidence
	return e.blocked == "" && p != nil && p.NeverRegistered != nil && *p.NeverRegistered && !p.Registered &&
		e.dep == nil && !e.o.HasDeployment && len(e.pods) == 0 && !e.rawReservation && e.marker == nil &&
		e.w.DrainingSince == nil && e.w.DrainingReason == nil && !e.w.CustodyHeld && !maintenanceActive(e.w)
}

// Each tick attempts at most one reason-owned release per worker. Failure leaves
// ownership intact for the next poll and does not prevent sibling releases.
func (m *Materializer) releasePressureHold(ctx context.Context, t *rollTick, e *rollEntry) {
	w := e.w
	if e.blocked != "" || e.dep == nil || e.drift || e.marker != nil || e.rawReservation ||
		w.DrainingSince == nil || w.DrainingReason == nil || *w.DrainingReason != "pressure_recycle" ||
		w.DiskPressure || w.CustodyHeld || maintenanceActive(w) || e.dep.DeletionTimestamp != nil ||
		e.dep.Spec.Replicas == nil || *e.dep.Spec.Replicas != 1 {
		return
	}
	spec, err := m.resolver.Resolve(w.Template, w.Size)
	if err != nil || !e.o.HasNixPVC || e.o.NixPVCSize == nil || e.o.NixPVCSize.Cmp(spec.NixSize) < 0 ||
		!e.o.HasDataPVC || !e.o.DataPVCLive || (w.Docker && (!e.o.HasDinDDataPVC || !e.o.DinDPVCLive)) {
		return
	}
	c, ok := m.cordoner.(rollCordoner)
	if !ok {
		return
	}
	if err := c.ClearDrainWithReason(ctx, w.ID, "pressure_recycle"); err != nil {
		t.errors = append(t.errors, fmt.Errorf("pressure release unconfirmed for %s: %w", w.ID, err))
	}
}

func independentRebuild(e *rollEntry) bool {
	return e.dep == nil && e.marker == nil && e.w.RollEvidence != nil && e.w.RollEvidence.Registered &&
		(maintenanceActive(e.w) || (e.w.DiskPressure && !imageRollDrain(e.w)))
}

// Independent maintenance creates no image-roll participant. Existing image-roll
// reservations are retained by the caller through the retained data claim.
func (m *Materializer) reserveMaintenanceRoll(e *rollEntry) error {
	if e.marker == nil && e.drift && !m.drain.ForceRoll {
		return fmt.Errorf("maintenance image admission unavailable; explicit recovery required")
	}
	return nil
}

func rollFrom(ctx context.Context, id string) *rollEntry {
	tick, _ := ctx.Value(rollTickKey{}).(*rollTick)
	if tick == nil {
		return nil
	}
	return tick.entries[id]
}

type rollCordoner interface {
	RequestDrainWithReason(context.Context, string, ...string) error
	ClearDrainWithReason(context.Context, string, ...string) error
}

func (m *Materializer) rollDrain(ctx context.Context, id string) error {
	c, ok := m.cordoner.(rollCordoner)
	if !ok {
		return fmt.Errorf("image-roll drain channel unavailable")
	}
	return c.RequestDrainWithReason(ctx, id, "image_roll")
}
func (m *Materializer) requestRecycleDrain(ctx context.Context, w protocol.DesiredWorker, vols recycleVolumes) error {
	e := rollFrom(ctx, w.ID)
	if vols.Pressure && e != nil && !e.drift && e.marker == nil && !e.rawReservation {
		c, ok := m.cordoner.(rollCordoner)
		if !ok {
			return fmt.Errorf("pressure drain channel unavailable")
		}
		return c.RequestDrainWithReason(ctx, w.ID, "pressure_recycle")
	}
	return m.requestWorkerDrain(ctx, w)
}
func (m *Materializer) requestWorkerDrain(ctx context.Context, w protocol.DesiredWorker) error {
	if e := rollFrom(ctx, w.ID); e != nil && !w.Ephemeral && (e.drift || (e.marker != nil && !e.marker.Completed)) {
		return m.rollDrain(ctx, w.ID)
	}
	return m.cordoner.RequestDrain(ctx, w.ID)
}

// maintenanceDespiteBusy preserves force-roll and otherwise permits only an
// independent maintenance deadline. Render the desired hash here as direct
// callers may have no pacing entry.
func (m *Materializer) maintenanceDespiteBusy(ctx context.Context, w protocol.DesiredWorker, obs reconcile.ObservedWorker) bool {
	if m.drain.ForceRoll {
		return true
	}
	if w.DrainingReason != nil && *w.DrainingReason == "image_roll" {
		return false
	}
	if e := rollFrom(ctx, w.ID); e != nil && (e.drift || (e.marker != nil && !e.marker.Completed) || (e.rawReservation && e.marker == nil)) {
		return false
	}
	spec, err := m.resolver.Resolve(w.Template, w.Size)
	if err != nil {
		return false
	}
	dep := RenderDeployment(m.cfg, w, spec)
	if obs.Generation != w.Generation || obs.SpecHash != dep.Spec.Template.Annotations[AnnotationSpecHash] {
		return false
	}
	return w.DrainingSince != nil && m.drain.Deadline > 0 && m.now().Sub(*w.DrainingSince) >= m.drain.Deadline
}

func (m *Materializer) retainRollReservation(ctx context.Context, e *rollEntry) error {
	if e.marker == nil {
		return nil
	}
	return m.mirrorRoll(ctx, e, e.marker)
}

// mirrorRoll uses only the bounded PVC LIST identity. Every write, including
// cancellation, advances RV so a delayed write from an old pin is rejected.
func (m *Materializer) mirrorRoll(ctx context.Context, e *rollEntry, marker *rollMarker) error {
	p := e.claim
	if p == nil || p.UID == "" || p.ResourceVersion == "" || p.DeletionTimestamp != nil {
		return fmt.Errorf("no retained claim identity; explicit roll recovery required")
	}
	var value any
	if marker != nil {
		raw, err := json.Marshal(marker)
		if err != nil || len(raw) > maxRollMarker {
			return fmt.Errorf("retained reservation exceeds bound")
		}
		value = string(raw)
		// A complete fresh LIST already confirms this exact durable marker.
		// Do not demand a new revision from an idempotent apiserver no-op.
		if p.Annotations[annotationRoll] == value {
			return nil
		}
	}
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{"uid": p.UID, "resourceVersion": p.ResourceVersion, "annotations": map[string]any{annotationRoll: value}}})
	if err != nil {
		return err
	}
	got, err := m.client.CoreV1().PersistentVolumeClaims(e.ns).Patch(ctx, p.Name, types.MergePatchType, body, metav1.PatchOptions{})
	if err != nil || got == nil || got.UID != p.UID || got.ResourceVersion == "" || got.ResourceVersion == p.ResourceVersion ||
		(marker == nil && got.Annotations[annotationRoll] != "") || (marker != nil && got.Annotations[annotationRoll] != value) {
		return fmt.Errorf("retained reservation unconfirmed; explicit recovery required")
	}
	e.claim = got
	return nil
}
func imageRollDrain(w protocol.DesiredWorker) bool {
	return w.DrainingSince != nil && w.DrainingReason != nil && *w.DrainingReason == "image_roll"
}
func canonicalCaps(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return compactCaps(out)
}
func compactCaps(in []string) []string {
	out := []string{}
	for _, s := range in {
		if len(out) == 0 || out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	return out
}
func coverageOf(w protocol.DesiredWorker, ns string) (rollCoverage, bool) {
	e := w.RollEvidence
	if e == nil || e.Version != 1 || e.OwnerID == "" || e.SchedulingCapabilities == nil || e.ProtocolCapabilities == nil {
		return rollCoverage{}, false
	}
	scheduling := append([]string{}, e.SchedulingCapabilities...)
	// Match fn_effective_worker_caps: API-authorized Docker placement grants
	// Docker claim coverage even when registration omitted the self-report.
	if w.Docker {
		scheduling = append(scheduling, "docker")
	}
	return rollCoverage{Owner: e.OwnerID, Lane: ns, Scheduling: canonicalCaps(scheduling), Protocol: canonicalCaps(e.ProtocolCapabilities), Ordinary: e.OrdinaryCoverage, Checker: e.CheckerCoverage}, true
}
func coverageKey(c rollCoverage) string { b, _ := json.Marshal(c); return string(b) }
func covers(w protocol.DesiredWorker, c rollCoverage) bool {
	got, ok := coverageOf(w, c.Lane)
	if !ok || got.Owner != c.Owner || (c.Ordinary && !got.Ordinary) || (c.Checker && !got.Checker) {
		return false
	}
	contains := func(have, want []string) bool {
		for _, s := range want {
			i := sort.SearchStrings(have, s)
			if i == len(have) || have[i] != s {
				return false
			}
		}
		return true
	}
	return contains(got.Scheduling, c.Scheduling) && contains(got.Protocol, c.Protocol)
}
func validRollOwner(r *rollMarker, e *rollEntry) bool {
	coverage, ok := coverageOf(e.w, e.ns)
	return r != nil && ok && r.Coverage.Owner == coverage.Owner && r.Coverage.Lane == e.ns
}
func decodeRoll(raw string) (*rollMarker, error) {
	if raw == "" {
		return nil, nil
	}
	if len(raw) > maxRollMarker {
		return nil, fmt.Errorf("oversized reservation")
	}
	fields, err := rollObject([]byte(raw), []string{"version", "hash", "image", "target_version", "coverage", "nonce", "previous_version", "canary", "completed"})
	if err != nil {
		return nil, fmt.Errorf("malformed reservation")
	}
	coverage, err := rollObject(fields["coverage"], []string{"owner", "lane", "scheduling", "protocol", "ordinary", "checker"})
	if err != nil {
		return nil, fmt.Errorf("malformed coverage")
	}
	for _, name := range []string{"scheduling", "protocol"} {
		var caps []json.RawMessage
		if err := json.Unmarshal(coverage[name], &caps); err != nil {
			return nil, fmt.Errorf("malformed capabilities")
		}
		for _, cap := range caps {
			if len(cap) == 0 || cap[0] != '"' {
				return nil, fmt.Errorf("malformed capability")
			}
		}
	}
	var r rollMarker
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return nil, fmt.Errorf("malformed reservation")
	}
	if r.Version != 1 || len(r.Hash) != 64 || r.Image == "" || r.TargetVersion == "" || r.Coverage.Owner == "" || r.Coverage.Lane == "" || r.Coverage.Scheduling == nil || r.Coverage.Protocol == nil || (r.Completed && !r.Canary) {
		return nil, fmt.Errorf("incomplete reservation")
	}
	return &r, nil
}

// rollObject accepts the complete version-1 schema only. The caller bounds raw
// bytes; each object has a fixed field count, rejecting duplicates and nulls.
func rollObject(raw []byte, names []string) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("expected object")
	}
	fields := make(map[string]json.RawMessage, len(names))
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return nil, err
		}
		name, ok := token.(string)
		known := false
		for _, want := range names {
			known = known || name == want
		}
		if !ok || !known || fields[name] != nil {
			return nil, fmt.Errorf("unknown or duplicate field")
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, fmt.Errorf("invalid field")
		}
		fields[name] = value
	}
	if _, err := d.Token(); err != nil || len(fields) != len(names) {
		return nil, fmt.Errorf("missing field")
	}
	if d.More() {
		return nil, fmt.Errorf("trailing data")
	}
	return fields, nil
}
func targetVersion(cfg RenderConfig, image string) string {
	if cfg.RollTargetVersion != "" {
		return cfg.RollTargetVersion
	}
	i := strings.LastIndex(image, ":")
	if i < 0 || strings.Contains(image, "@") {
		return ""
	}
	tag := image[i+1:]
	// ADR-1720 stamps release binaries with build metadata absent from bare OCI
	// tags. There is no authoritative controller mapping from tag to commit.
	// Never infer the expected stamp from the replacement worker itself.
	if _, ok := parseSemver(tag); !ok || !strings.Contains(tag, "+") {
		return ""
	}
	// Worker binaries strip only the optional v prefix; build metadata is identity.
	return strings.TrimPrefix(tag, "v")
}
func freshUntil(now, start time.Time, evaluated, expiry time.Time) bool {
	return !evaluated.IsZero() && expiry.After(evaluated) && now.Sub(start) < expiry.Sub(evaluated)
}

// oldUsable proves serving coverage, not source disposition. An open custody
// hold protects storage but cannot negate independent positive serving proof.
// Mutation callers keep their own custody fences.
func (m *Materializer) oldUsable(t *rollTick, e *rollEntry) bool {
	proof := e.w.RollEvidence
	return e.blocked == "" && liveWorkerPod(e) && proof != nil && (workerImage(e.dep) != workerImage(e.target) || e.dep.Spec.Template.Annotations[AnnotationSpecHash] != e.target.Spec.Template.Annotations[AnnotationSpecHash] || m.currentUsable(t, e)) && proof.LivenessValidUntil != nil &&
		freshUntil(m.now(), t.start, proof.EvaluatedAt, *proof.LivenessValidUntil) &&
		!e.w.DiskPressure && !maintenanceActive(e.w) &&
		e.w.DrainingSince == nil && covers(e.w, e.coverage)
}

// currentUsable requires current incarnation proof even without roll history.
// It borrows the promotion seam with an empty prior nonce; the current nonce must
// still be nonempty and proven by the heartbeat for the single physical pod.
func (m *Materializer) currentUsable(t *rollTick, e *rollEntry) bool {
	if e.dep == nil || workerImage(e.dep) != workerImage(e.target) {
		return false
	}
	c, ok := coverageOf(e.w, e.ns)
	if !ok {
		return false
	}
	copy := *e
	copy.marker = &rollMarker{Hash: e.dep.Spec.Template.Annotations[AnnotationSpecHash], Image: workerImage(e.target), TargetVersion: targetVersion(m.cfg, workerImage(e.target)), Coverage: c}
	if e.marker != nil {
		if e.marker.Hash != copy.marker.Hash || e.marker.Image != copy.marker.Image || e.marker.TargetVersion != copy.marker.TargetVersion {
			return false
		}
		copy.marker.Nonce = e.marker.Nonce
	}
	return m.promoted(t, &copy)
}
func liveWorkerPod(e *rollEntry) bool {
	if e.dep == nil || e.dep.DeletionTimestamp != nil || e.dep.UID == "" {
		return false
	}
	for _, pod := range e.pods {
		if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning || !maintenancePodOwner(pod, e.sets) || pod.Annotations[AnnotationSpecHash] != e.dep.Spec.Template.Annotations[AnnotationSpecHash] {
			continue
		}
		for _, c := range pod.Spec.Containers {
			if c.Name != workerContainerName || c.Image != workerImage(e.dep) {
				continue
			}
			for _, status := range pod.Status.ContainerStatuses {
				if status.Name == workerContainerName && status.Image == c.Image && status.State.Running != nil {
					return true
				}
			}
		}
	}
	return false
}
func maintenanceActive(w protocol.DesiredWorker) bool {
	op := w.DindMaintenance
	return op != nil && op.Phase != "complete" && op.Phase != "completed" && op.Phase != "cancelled"
}

// promoted proves the current serving incarnation. It grants no authority to
// discard protected sources or clear a custody-held drain. A protected provider
// still under a cordon cannot supply claimable coverage.
func (m *Materializer) promoted(t *rollTick, e *rollEntry) bool {
	r, p := e.marker, e.w.RollEvidence
	if e.o.Roll.Phase == protocol.PhaseStuck || r == nil || p == nil || !p.Registered || p.Version != 1 || e.dep == nil || e.dep.UID == "" || e.dep.DeletionTimestamp != nil ||
		e.dep.Spec.Template.Annotations[AnnotationSpecHash] != r.Hash || workerImage(e.dep) != r.Image ||
		p.RegisteredVersion == nil || *p.RegisteredVersion != r.TargetVersion || p.RegisterNonce == nil || *p.RegisterNonce == "" || *p.RegisterNonce == r.Nonce ||
		p.PodUID == nil || *p.PodUID == "" || p.HeartbeatAt == nil || p.HeartbeatAt.IsZero() || p.HeartbeatAt.After(p.EvaluatedAt) ||
		!freshUntil(m.now(), t.start, p.EvaluatedAt, p.ValidUntil) || !covers(e.w, r.Coverage) ||
		e.w.DiskPressure || maintenanceActive(e.w) || (e.w.CustodyHeld && e.w.DrainingSince != nil) ||
		(e.w.DrainingSince != nil && !imageRollDrain(e.w)) {
		return false
	}
	current := 0
	good := false
	for _, pod := range e.pods {
		if pod.DeletionTimestamp != nil || pod.UID == "" || !maintenancePodOwner(pod, e.sets) {
			continue
		}
		current++
		if pod.Status.Phase != corev1.PodRunning || string(pod.UID) != *p.PodUID || pod.Annotations[AnnotationSpecHash] != r.Hash {
			continue
		}
		for _, c := range pod.Spec.Containers {
			if c.Name != workerContainerName || c.Image != r.Image {
				continue
			}
			for _, s := range pod.Status.ContainerStatuses {
				if s.Name == workerContainerName && s.Image == r.Image && s.State.Running != nil {
					good = true
				}
			}
		}
	}
	return current == 1 && good
}

// rollSnapshot uses the existing bounded pagination/response transport. A failed
// list withholds all proof in that namespace; independent namespaces still run.
func (m *Materializer) rollSnapshot(ctx context.Context, ns string, entries []*rollEntry) error {
	ctx, cancel := context.WithTimeout(markMaintenanceRead(ctx), 30*time.Second)
	defer cancel()
	deps := map[string]*appsv1.Deployment{}
	pods := []corev1.Pod{}
	sets := map[types.UID]map[string]appsv1.ReplicaSet{}
	err := maintenancePages(ctx, "roll deployments", func(opt metav1.ListOptions) (int, string, error) {
		page, err := m.client.AppsV1().Deployments(ns).List(ctx, opt)
		if err != nil {
			return 0, "", err
		}
		for i := range page.Items {
			d := &page.Items[i]
			if id, ours := IsOurs(d.Labels); ours {
				deps[id] = d.DeepCopy()
			}
		}
		return len(page.Items), page.Continue, nil
	})
	if err != nil {
		return err
	}

	err = maintenancePages(ctx, "roll pods", func(opt metav1.ListOptions) (int, string, error) {
		page, err := m.client.CoreV1().Pods(ns).List(ctx, opt)
		if err != nil {
			return 0, "", err
		}
		pods = append(pods, page.Items...)
		return len(page.Items), page.Continue, nil
	})
	if err != nil {
		return err
	}
	owners := map[string]types.UID{}
	for _, dep := range deps {
		if dep.UID != "" && dep.DeletionTimestamp == nil {
			owners[dep.Name] = dep.UID
		}
	}
	err = maintenancePages(ctx, "roll replicasets", func(opt metav1.ListOptions) (int, string, error) {
		page, err := m.client.AppsV1().ReplicaSets(ns).List(ctx, opt)
		if err != nil {
			return 0, "", err
		}
		for i := range page.Items {
			rs := &page.Items[i]
			owner := metav1.GetControllerOf(rs)
			if rs.UID == "" || rs.DeletionTimestamp != nil || owner == nil || owner.APIVersion != "apps/v1" || owner.Kind != "Deployment" ||
				owner.UID == "" || owners[owner.Name] != owner.UID {
				continue
			}
			if sets[owner.UID] == nil {
				sets[owner.UID] = map[string]appsv1.ReplicaSet{}
			}
			// Retain current ancestry identity only, never historical templates.
			sets[owner.UID][rs.Name] = appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: rs.Name, UID: rs.UID}}
		}
		return len(page.Items), page.Continue, nil
	})
	if err != nil {
		return err
	}

	// The retained /data claim carries the same bounded immutable reservation
	// during a Deployment recycle. No separate journal or new kube object is used.
	claims := map[string]*corev1.PersistentVolumeClaim{}
	rawReservations := map[string]bool{}
	err = maintenancePages(ctx, "roll retained claims", func(opt metav1.ListOptions) (int, string, error) {
		page, err := m.client.CoreV1().PersistentVolumeClaims(ns).List(ctx, opt)
		if err != nil {
			return 0, "", err
		}
		for _, p := range page.Items {
			if id, ours := IsOurs(p.Labels); ours {
				if _, present := p.Annotations[annotationRoll]; present {
					rawReservations[id] = true
				}
			}
			if id, ours := IsOurs(p.Labels); ours && p.Name == dataPVCName(id) {
				claims[id] = p.DeepCopy()
			}
		}
		return len(page.Items), page.Continue, nil
	})
	if err != nil {
		return err
	}
	for _, e := range entries {
		e.rawReservation = rawReservations[e.w.ID]
		e.dep = deps[e.w.ID]
		e.claim = claims[e.w.ID]
		if e.dep == nil && e.claim != nil && e.claim.Annotations[annotationRoll] != "" {
			e.marker, err = decodeRoll(e.claim.Annotations[annotationRoll])
			if err != nil {
				e.blocked = "retained reservation malformed; explicit recovery required"
			} else if e.claim.DeletionTimestamp != nil {
				e.blocked = "retained reservation terminating; explicit recovery required"
			} else {
				e.recovery = true
			}
		}
		if e.dep != nil {
			e.o.HasDeployment = true
			e.o.DeploymentUID = string(e.dep.UID)
			e.o.DeploymentResourceVersion = e.dep.ResourceVersion
			e.o.SpecHash = e.dep.Spec.Template.Annotations[AnnotationSpecHash]
			e.marker, err = decodeRoll(e.dep.Annotations[annotationRoll])
			if err != nil {
				e.blocked = err.Error()
			} else if e.marker != nil && !validRollOwner(e.marker, e) {
				e.blocked = "reservation owner or placement does not match API evidence"
			}
			if e.blocked == "" && e.claim != nil && e.claim.Annotations[annotationRoll] != "" {
				mirror, mirrorErr := decodeRoll(e.claim.Annotations[annotationRoll])
				if mirrorErr != nil || !validRollOwner(mirror, e) || e.claim.DeletionTimestamp != nil {
					e.blocked = "retained reservation invalid; explicit recovery required"
				} else if e.marker == nil && mirror != nil {
					// Restore metadata before any replacement template mutation. Pin
					// reconciliation below fences an obsolete mirror before proceeding.
					if err := m.reserveRoll(ctx, e, mirror); err != nil {
						e.blocked = "replacement reservation restoration unconfirmed"
					}
				} else if e.marker != nil && e.dep.Annotations[annotationRoll] == e.claim.Annotations[annotationRoll] && (e.w.DindMaintenance == nil || string(e.dep.UID) != e.w.DindMaintenance.DeploymentUID) {
					// This LIST observed the replacement's matching metadata.
					if err := m.mirrorRoll(ctx, e, nil); err != nil {
						e.blocked = "retained reservation cleanup unconfirmed"
					}
				}
			}
		} else if e.o.HasDeployment {
			e.blocked = "deployment missing from snapshot"
		}
		if e.dep != nil && e.marker != nil && e.dep.Spec.Replicas != nil && *e.dep.Spec.Replicas == 0 {
			e.recovery = true
		}
		if e.w.RollEvidence != nil && maintenanceActive(e.w) && e.w.DindMaintenance.Phase != "requested" && e.w.DindMaintenance.Phase != "ready" && e.dep == nil && e.marker == nil && imageRollDrain(e.w) {
			e.blocked = "maintenance reservation missing; explicit recovery required"
		}
		e.sets = map[string]appsv1.ReplicaSet{}
		if e.dep != nil && sets[e.dep.UID] != nil {
			e.sets = sets[e.dep.UID]
		}
		e.volumeRefs = map[string]bool{}
		for _, pod := range pods {
			for _, volume := range pod.Spec.Volumes {
				if volume.PersistentVolumeClaim != nil {
					e.volumeRefs[volume.PersistentVolumeClaim.ClaimName] = true
				}
			}
			if id, ours := IsOurs(pod.Labels); ours && id == e.w.ID {
				e.pods = append(e.pods, pod)
			}
		}
		current := []corev1.Pod{}
		for _, pod := range e.pods {
			if maintenancePodOwner(pod, e.sets) {
				current = append(current, pod)
			}
		}
		failure := ""
		if e.dep != nil {
			failure = replicaFailureReason(e.dep)
		}
		health := deriveRollHealth(current, e.target.Spec.Template.Annotations[AnnotationSpecHash], failure, m.now())
		if e.o.Roll.Phase != protocol.PhaseStuck || e.o.Roll.PodPhase != string(corev1.ClaimPending) {
			e.o.Roll = health
		}

	}
	return nil
}

// reserveRoll confirms a metadata mutation before any cordon or destructive
// action. Every later patch uses its returned resourceVersion, including cancel.
func (m *Materializer) reserveRoll(ctx context.Context, e *rollEntry, r *rollMarker) error {
	if e.dep == nil || e.dep.UID == "" || e.dep.ResourceVersion == "" || e.dep.DeletionTimestamp != nil {
		return fmt.Errorf("reservation needs live deployment UID/resourceVersion")
	}
	var value any
	if r != nil {
		raw, err := json.Marshal(r)
		if err != nil || len(raw) > maxRollMarker {
			return fmt.Errorf("reservation exceeds bound")
		}
		value = string(raw)
	}
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{"uid": e.dep.UID, "resourceVersion": e.dep.ResourceVersion, "annotations": map[string]any{annotationRoll: value}}})
	if err != nil {
		return err
	}
	d, err := m.client.AppsV1().Deployments(e.ns).Patch(ctx, e.dep.Name, types.MergePatchType, body, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("reservation mutation unconfirmed: %w", err)
	}
	if d == nil || d.UID != e.dep.UID || d.ResourceVersion == "" || d.ResourceVersion == e.dep.ResourceVersion {
		return fmt.Errorf("reservation response lacks advanced resourceVersion")
	}
	got, err := decodeRoll(d.Annotations[annotationRoll])
	if err != nil || (r == nil && got != nil) || (r != nil && (got == nil || d.Annotations[annotationRoll] != value)) {
		return fmt.Errorf("reservation response differs")
	}
	e.dep, e.marker = d, got
	e.o.DeploymentUID, e.o.DeploymentResourceVersion = string(d.UID), d.ResourceVersion
	return nil
}
