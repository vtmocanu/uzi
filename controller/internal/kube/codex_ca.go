package kube

import (
	"context"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

// The worker Secret is write-once and the controller cannot read it back, so whether it
// carries codexExtraCACertKey is known only from evidence recorded when it was written:
//
//   - durable: AnnotationCodexExtraCA on the worker's /data claim, stamped at claim create
//     (ensurePVCs) in the same pass that wrote the Secret;
//   - in-process carry: bridges the window between a confirmed Secret write (or a /data
//     recycle delete of a stamped claim) and the stamped claim's create. It is lost on a
//     controller restart, which only ever costs the env (fail closed), never a crash.
//
// Without either, the pod template omits UZI_CODEX_CA_FILE.

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

// ownDataClaim returns obs's /data claim when it is w's own claim in ns.
func ownDataClaim(w protocol.DesiredWorker, obs reconcile.ObservedWorker, ns string) *reconcile.ClaimObservation {
	c := obs.DataClaim
	if obs.HasDataPVC && c != nil && c.Namespace == ns && c.Name == dataPVCName(w.ID) {
		return c
	}
	return nil
}

// deriveCodexCA applies the evidence rules with no side effects: the own-namespace /data
// claim's stamp decides when one is observed; otherwise the carry does. A present but
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
	return m.codexCACarried(w.ID, ns), false, ns, true
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

// codexCAWriteDeferred reports that this pass wrote the Codex CA evidence (a carry is
// set, or the stamped claim was just created) while the tick's frozen decision still
// says no. Rendering a Deployment now would omit the env that the next observation will
// require, so the pass stops after ensurePVCs and the next tick creates it.
func (m *Materializer) codexCAWriteDeferred(ctx context.Context, w protocol.DesiredWorker, obs reconcile.ObservedWorker, ns string, codexStamped bool) bool {
	if m.codexCAInSecretFor(ctx, w, obs) {
		return false
	}
	return codexStamped || m.codexCACarried(w.ID, ns)
}
