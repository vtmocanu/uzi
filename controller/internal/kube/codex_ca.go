package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

// The worker Secret is write-once and the controller cannot read it back, so whether it
// carries codexExtraCACertKey is known only from evidence recorded when it was written:
//
//   - durable: AnnotationCodexExtraCA on the worker's /data claim, stamped at claim create
//     (ensurePVCs) in the same pass that wrote the Secret;
//   - durable, retained recycle only: the same annotation on the retained worker Deployment
//     (held at zero replicas under a reservation). recycleWorkerVolumes syncs it to the
//     own claim's stamp (set when stamped, cleared when not) with a confirmed patch
//     immediately before the claim delete, so it covers the window between that delete and
//     the replacement claim's create across a controller restart. With no own claim the
//     record counts only on a non-terminating Deployment in the worker's own namespace
//     carrying exactly "written"; with an own claim, the claim alone decides. The /data
//     delete in that case carries a UID precondition so a delayed delete cannot hit the
//     replacement;
//   - in-process carry: bridges the window between a confirmed Secret write (or a /data
//     recycle delete of a stamped claim) and the stamped claim's create. It is lost on a
//     controller restart, which only ever costs the env (fail closed), never a crash.
//     Non-retained recycles (the Deployment is deleted, not held) remain carry-only.
//
// Without any of them, the pod template omits UZI_CODEX_CA_FILE.
//
// A carry for a namespace the worker has left (a Docker/Isolated flip) persists until
// teardown or until the worker leaves the desired set. It is bounded and stays truthful:
// it names the namespace whose Secret was written.

// codexCATickKey carries the per-tick Codex CA decision (worker id -> Secret carries the
// key) in the Reconcile context, frozen like placementTickKey.
type codexCATickKey struct{}

// codexCAKey identifies one worker's /data claim namespace.
type codexCAKey struct{ workerID, namespace string }

func (m *Materializer) setCodexCACarry(id, ns string) {
	m.codexCAMu.Lock()
	defer m.codexCAMu.Unlock()
	if m.codexCACarry == nil {
		m.codexCACarry = map[codexCAKey]struct{}{}
	}
	m.codexCACarry[codexCAKey{id, ns}] = struct{}{}
}

func (m *Materializer) clearCodexCACarry(id, ns string) {
	m.codexCAMu.Lock()
	defer m.codexCAMu.Unlock()
	delete(m.codexCACarry, codexCAKey{id, ns})
}

// clearCodexCACarryFor drops every namespace's carry for id (teardown).
func (m *Materializer) clearCodexCACarryFor(id string) {
	m.codexCAMu.Lock()
	defer m.codexCAMu.Unlock()
	for k := range m.codexCACarry {
		if k.workerID == id {
			delete(m.codexCACarry, k)
		}
	}
}

func (m *Materializer) codexCACarried(id, ns string) bool {
	m.codexCAMu.Lock()
	defer m.codexCAMu.Unlock()
	_, ok := m.codexCACarry[codexCAKey{id, ns}]
	return ok
}

// pruneCodexCACarry drops carries whose worker left the desired set.
func (m *Materializer) pruneCodexCACarry(desired map[string]bool) {
	m.codexCAMu.Lock()
	defer m.codexCAMu.Unlock()
	for k := range m.codexCACarry {
		if !desired[k.workerID] {
			delete(m.codexCACarry, k)
		}
	}
}

// codexExtraCARecord reports whether d carries a valid, live Codex CA record: the exact
// "written" value on a Deployment that is not being deleted. Anything else (absent,
// malformed, terminating) is no evidence.
func codexExtraCARecord(d *appsv1.Deployment) bool {
	return d != nil && d.DeletionTimestamp == nil && d.Annotations[AnnotationCodexExtraCA] == codexExtraCAWritten
}

// codexCANoClaimEvidence is the evidence used when w has no own-namespace /data claim:
// the in-process carry, or the durable record on the worker's Deployment in ns.
func (m *Materializer) codexCANoClaimEvidence(w protocol.DesiredWorker, obs reconcile.ObservedWorker, ns string) bool {
	return m.codexCACarried(w.ID, ns) || obs.DeploymentCodexExtraCA[ns]
}

// syncRetainedCodexCARecord makes the retained Deployment's AnnotationCodexExtraCA record
// match the claim's stamp (written: present with the exact value; otherwise absent). It
// is a no-op when it already matches. Otherwise it patches under UID and resourceVersion
// preconditions and requires a confirmed response before returning nil: the caller
// deletes the claim only on nil.
func (m *Materializer) syncRetainedCodexCARecord(ctx context.Context, ns string, e *rollEntry, written bool) error {
	if e == nil || e.dep == nil {
		return fmt.Errorf("codex CA evidence write unavailable; re-poll required")
	}
	dep := e.dep
	cur, has := dep.Annotations[AnnotationCodexExtraCA]
	if written && has && cur == codexExtraCAWritten || !written && !has {
		return nil
	}
	if dep.UID == "" || dep.ResourceVersion == "" || dep.DeletionTimestamp != nil {
		return fmt.Errorf("codex CA evidence write unavailable; re-poll required")
	}
	var value any // nil clears the key under a merge patch
	if written {
		value = codexExtraCAWritten
	}
	body, err := json.Marshal(map[string]any{"metadata": m.withOperatorMetadata(map[string]any{
		"uid":             dep.UID,
		"resourceVersion": dep.ResourceVersion,
		"annotations":     map[string]any{AnnotationCodexExtraCA: value},
	})})
	if err != nil {
		return err
	}
	got, err := m.client.AppsV1().Deployments(ns).Patch(ctx, dep.Name, types.MergePatchType, body, metav1.PatchOptions{})
	if err != nil || got == nil || got.UID != dep.UID || got.ResourceVersion == "" || got.ResourceVersion == dep.ResourceVersion || got.DeletionTimestamp != nil ||
		got.Annotations[annotationRoll] != dep.Annotations[annotationRoll] || got.Spec.Replicas == nil || *got.Spec.Replicas != 0 {
		return fmt.Errorf("codex CA evidence write unconfirmed; re-poll required")
	}
	gotVal, gotHas := got.Annotations[AnnotationCodexExtraCA]
	if written && (!gotHas || gotVal != codexExtraCAWritten) || !written && gotHas {
		return fmt.Errorf("codex CA evidence write unconfirmed; re-poll required")
	}
	// Confirmed: refresh the tick state so no stale resourceVersion remains. annotationRoll
	// is confirmed unchanged, so the marker stays.
	e.dep = got
	e.o.DeploymentUID, e.o.DeploymentResourceVersion = string(got.UID), got.ResourceVersion
	return nil
}

// ownDataClaim returns obs's /data claim when it is w's own claim in ns. When the
// namespace-keyed observation is present it alone decides, so a same-ID claim scanned
// later in another namespace cannot shadow or masquerade as the own claim.
func ownDataClaim(w protocol.DesiredWorker, obs reconcile.ObservedWorker, ns string) *reconcile.ClaimObservation {
	if obs.DataClaimsByNamespace != nil {
		if c, ok := obs.DataClaimsByNamespace[ns]; ok && c.Name == dataPVCName(w.ID) {
			cp := c
			return &cp
		}
		return nil
	}
	c := obs.DataClaim
	if obs.HasDataPVC && c != nil && c.Namespace == ns && c.Name == dataPVCName(w.ID) {
		return c
	}
	return nil
}

// ownDataPresence reports whether w's own /data claim exists in ns (has) and is live (not
// Terminating). Same-ID claims in other namespaces never count.
func ownDataPresence(w protocol.DesiredWorker, obs reconcile.ObservedWorker, ns string) (has, live bool) {
	if obs.DataClaimsByNamespace != nil {
		if c, ok := obs.DataClaimsByNamespace[ns]; ok && c.Name == dataPVCName(w.ID) {
			return true, !c.Terminating
		}
		return false, false
	}
	// The fallback is looser than ownDataClaim's (which checks the namespace), and only
	// literal-observation tests reach it, because Observe always fills the map.
	return obs.HasDataPVC, obs.DataPVCLive
}

// ownDataCreatedAt returns the creation time of w's own LIVE /data claim in ns, else nil.
func ownDataCreatedAt(w protocol.DesiredWorker, obs reconcile.ObservedWorker, ns string) *time.Time {
	if obs.DataClaimsByNamespace != nil {
		if c, ok := obs.DataClaimsByNamespace[ns]; ok && c.Name == dataPVCName(w.ID) && !c.Terminating {
			t := c.CreatedAt
			return &t
		}
		return nil
	}
	return obs.DataPVCCreatedAt
}

// deriveCodexCA applies the evidence rules with no side effects: the own-namespace /data
// claim's stamp decides when one is observed; otherwise the in-process carry or the
// retained Deployment's durable record does. A present but
// unreadable claim, a claim in another namespace and a brand-new worker with no carry
// all read false.
func (m *Materializer) deriveCodexCA(w protocol.DesiredWorker, obs reconcile.ObservedWorker) (decision bool, liveOwnClaim bool, ns string, ok bool) {
	ns, err := m.cfg.Placement(w)
	if err != nil {
		return false, false, "", false
	}
	if c := ownDataClaim(w, obs, ns); c != nil {
		return c.CodexExtraCA, !c.Terminating, ns, true
	}
	return m.codexCANoClaimEvidence(w, obs, ns), false, ns, true
}

// codexCADecisions computes the Codex CA decision for every DESIRED worker once per tick,
// from that tick's observation, before any claim or Secret is written. It also retires a
// carry once a live own claim is observed (the stamp, or its absence, is then the
// durable truth). A Terminating claim never clears the carry: a recycle replacement is
// still to come.
func (m *Materializer) codexCADecisions(desired []protocol.DesiredWorker, observed map[string]reconcile.ObservedWorker) map[string]bool {
	d := make(map[string]bool, len(desired))
	for _, w := range desired {
		decision, live, ns, ok := m.deriveCodexCA(w, observed[w.ID])
		if ok && live {
			m.clearCodexCACarry(w.ID, ns)
		}
		d[w.ID] = decision
	}
	return d
}

// codexCAInSecretFor returns the tick's frozen decision for w, or derives it from obs
// plus the carry when called outside Reconcile.
func (m *Materializer) codexCAInSecretFor(ctx context.Context, w protocol.DesiredWorker, obs reconcile.ObservedWorker) bool {
	if p, ok := ctx.Value(codexCATickKey{}).(map[string]bool); ok {
		if v, ok := p[w.ID]; ok {
			return v
		}
	}
	decision, _, _, _ := m.deriveCodexCA(w, obs)
	return decision
}

// codexCAWriteDeferred reports that this pass wrote Codex CA evidence (secretWritten:
// this tick's own Secret create response carried the key; codexStamped: this pass's
// stamped claim create was confirmed) while the tick's frozen decision still says no.
// Rendering a Deployment now would omit the env that the next observation will require,
// so the pass stops after ensurePVCs and the next tick creates it.
//
// Only evidence from THIS pass counts. A carry left by an earlier tick must not fire the
// guard: a Terminating own unstamped claim never clears it, and a guard keyed on it would
// return on every tick (skipping, in the hibernation branch, the Preparing stop of a live
// Deployment).
func (m *Materializer) codexCAWriteDeferred(ctx context.Context, w protocol.DesiredWorker, obs reconcile.ObservedWorker, secretWritten, codexStamped bool) bool {
	if m.codexCAInSecretFor(ctx, w, obs) {
		return false
	}
	return secretWritten || codexStamped
}
