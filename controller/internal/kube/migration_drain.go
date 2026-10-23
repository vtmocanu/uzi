package kube

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

// Pending writes are process-local uncertainty, not a settlement fence. Even a
// later explicit undrained poll cannot prevent an earlier timed-out POST from
// committing afterwards. Custody remains the replacement authority throughout.
func migrationLane(c rollCoverage) string { return c.Owner + "\x00" + c.Lane }
func migrationLaneFenced(t *rollTick, c rollCoverage) bool {
	return t.migrationFences[migrationLane(c)] || t.migrationFences[migrationLane(rollCoverage{Lane: c.Lane})]
}

// Entries retain the original observation if the snapshot fails, and otherwise
// carry the newer pod LIST layout. Missing entries still use full inventory.
func migrationLayout(t *rollTick, w protocol.DesiredWorker, observed map[string]reconcile.ObservedWorker) reconcile.RunnerLayout {
	if e := t.entries[w.ID]; e != nil {
		return e.o.RunnerLayout
	}
	return observed[w.ID].RunnerLayout
}

func (m *Materializer) prepareMigrationDrains(ctx context.Context, t *rollTick, desired []protocol.DesiredWorker, observed map[string]reconcile.ObservedWorker) {
	t.unavailable = map[string]bool{}
	t.migrationFences = map[string]bool{}
	t.pollDrained = map[string]bool{}
	if m.migrationPending == nil {
		m.migrationPending = map[string]rollCoverage{}
	}
	byID := map[string]protocol.DesiredWorker{}
	for _, w := range desired {
		byID[w.ID] = w
		t.pollDrained[w.ID] = w.DrainingSince != nil || w.DrainingReason != nil
	}
	for id, frozen := range m.migrationPending {
		w, present := byID[id]
		if _, seen := observed[id]; !present && !seen && m.migrationPodsAbsent(ctx, id, frozen.Lane) {
			delete(m.migrationPending, id)
			continue
		}
		c, valid := coverageOf(w, frozen.Lane)
		// Missing owner/evidence is not a resolution. A successful explicit poll
		// can retire uncertainty, including when custody itself has cleared.
		if present && valid && c.Owner == frozen.Owner &&
			(!w.CustodyHeld || w.DrainingSince != nil || (w.DrainingSince == nil && w.DrainingReason == nil)) {
			delete(m.migrationPending, id)
			continue
		}
		t.unavailable[id] = true
		fenceMigrationCoverage(t, frozen)
	}
	// Full desired inventory, independent of template resolution or drift proof.
	for _, w := range desired {
		if w.Ephemeral || !migrationHeld(w, migrationLayout(t, w, observed)) {
			continue
		}
		if w.DrainingSince != nil || w.DrainingReason != nil {
			t.unavailable[w.ID] = true
		}
	}
}

// migrationPodsAbsent requires a fresh, complete, unselected scan: Observe can
// omit a worker after swallowing a pod-list error. Any matching pod, including
// terminal or terminating pods, retains uncertainty. maintenancePages bounds
// each attempt to 16 pages of 256 objects; failure fences this entry and does
// not prevent independent pending entries from being checked.
func (m *Materializer) migrationPodsAbsent(ctx context.Context, id, lane string) bool {
	if lane == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(markMaintenanceRead(ctx), 30*time.Second)
	defer cancel()
	found := false
	err := maintenancePages(ctx, "migration pending pods", func(options metav1.ListOptions) (int, string, error) {
		page, err := m.client.CoreV1().Pods(lane).List(ctx, options)
		if err != nil {
			return 0, "", err
		}
		for _, pod := range page.Items {
			if workerID, ours := IsOurs(pod.Labels); ours && workerID == id {
				found = true
			}
		}
		return len(page.Items), page.Continue, nil
	})
	return err == nil && !found
}

// Fence both admission keys and the physical lane. Before the snapshot and pool
// classification, entries have only their current API coverage available.
func fenceMigrationCoverage(t *rollTick, frozen rollCoverage) {
	t.migrationFences[migrationLane(frozen)] = true
	t.uncertain[coverageKey(frozen)] = true
	for _, e := range t.entries {
		c := e.coverage
		if c.Lane == "" {
			c, _ = coverageOf(e.w, e.ns)
		}
		if c.Lane == frozen.Lane && (frozen.Owner == "" || c.Owner == frozen.Owner) {
			t.uncertain[coverageKey(c)] = true
		}
	}
}

func (m *Materializer) migrationCover(t *rollTick, candidate *rollEntry) bool {
	for _, peer := range t.entries {
		if peer == candidate || peer.ns != candidate.ns || t.pollDrained[peer.w.ID] || peer.w.DrainingSince != nil || peer.w.DrainingReason != nil ||
			peer.blocked != "" || migrationLaneFenced(t, peer.coverage) || t.uncertain[coverageKey(peer.coverage)] ||
			!covers(peer.w, candidate.coverage) || (peer.admitted && peer.drift) {
			continue
		}
		// Independent /nix maintenance runs after planning and may recycle or
		// cordon this otherwise usable provider in the same reconciliation.
		spec, err := m.resolver.Resolve(peer.w.Template, peer.w.Size)
		if err != nil || m.legacyRecycleDue(peer.w, peer.o, spec) {
			continue
		}
		if m.oldUsable(t, peer) || m.currentUsable(t, peer) || m.promoted(t, peer) {
			return true
		}
	}
	return false
}

func (m *Materializer) selectMigrationDrains(ctx context.Context, t *rollTick, desired []protocol.DesiredWorker, observed map[string]reconcile.ObservedWorker, pools map[string][]*rollEntry) {
	budget := map[string]bool{}
	drained := map[string][]string{}
	for id, c := range m.migrationPending {
		budget[migrationLane(c)] = true
		t.unavailable[id] = true
	}
	for _, w := range desired {
		if w.Ephemeral || !migrationHeld(w, migrationLayout(t, w, observed)) {
			continue
		}
		ns, err := m.cfg.Placement(w)
		if err != nil {
			continue
		}
		c, ok := coverageOf(w, ns)
		if !ok {
			c = rollCoverage{Lane: ns}
		}
		if !ok || w.DrainingSince != nil || w.DrainingReason != nil {
			budget[migrationLane(c)] = true
		}
		// Diagnostic identity needs no capability proof. Unknown owners still
		// consume the conservative lane budget above, but cannot share samples.
		if w.DrainingSince != nil && w.RollEvidence != nil && w.RollEvidence.Version == 1 && w.RollEvidence.OwnerID != "" {
			identity := rollCoverage{Owner: w.RollEvidence.OwnerID, Lane: ns}
			drained[migrationLane(identity)] = append(drained[migrationLane(identity)], w.ID)
		}
	}
	// Ordinary planning has finished, including its unconfirmed metadata writes.
	// Fence the entire physical owner/lane, even across overlapping capability keys.
	ordinaryUncertain := map[string]bool{}
	for _, e := range t.entries {
		if t.uncertain[coverageKey(e.coverage)] {
			ordinaryUncertain[migrationLane(e.coverage)] = true
		}
	}
	for _, ids := range drained {
		if len(ids) > 1 {
			t.migrationMultiple = true
			sort.Strings(ids)
		}
	}
	ids := []string{}
	for id := range t.entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		e := t.entries[id]
		p := t.outcomes[id]
		if !e.migrationHeldDrift {
			continue
		}
		reason := ""
		switch {
		case m.migrationPending[id].Lane != "":
			reason = "drain pending; re-poll required"
		case e.w.DrainingSince != nil || e.w.DrainingReason != nil:
			reason = "retained drain; operator custody decision"
			if !m.migrationCover(t, e) {
				reason = "retained drain without coverage; operator custody decision"
			}
		case e.blocked != "" || migrationLaneFenced(t, e.coverage) || ordinaryUncertain[migrationLane(e.coverage)]:
			if len(t.entries) == 1 {
				continue
			}
			reason = "drain blocked by uncertain evidence"
		case budget[migrationLane(e.coverage)] || budget[migrationLane(rollCoverage{Lane: e.ns})]:
			reason = "drain budget occupied"
		case e.coverage.Owner == "" || !m.migrationCover(t, e):
			// Preserve the established singleton hold reason.
			if len(t.entries) == 1 {
				continue
			}
			reason = "no distinct live covering sibling; operator custody decision"
		default:
			t.unavailable[id] = true
			safe := true
			for _, ordinary := range t.entries {
				if ordinary != e && ordinary.admitted && ordinary.drift &&
					ordinary.coverage.Owner == e.coverage.Owner && ordinary.ns == e.ns &&
					!m.remainingCoverage(t, pools[coverageKey(ordinary.coverage)], ordinary) {
					safe = false
					break
				}
			}
			if !safe {
				delete(t.unavailable, id)
				reason = "drain waits for ordinary roll coverage"
				break
			}
			budget[migrationLane(e.coverage)] = true
			reason = "drain selected; operator custody decision"
			if err := m.rollDrain(ctx, id); err != nil {
				m.migrationPending[id] = e.coverage
				fenceMigrationCoverage(t, e.coverage)
				reason = "drain pending; re-poll required"
				t.errors = append(t.errors, fmt.Errorf("worker %s: migration drain unconfirmed: %w", id, err))
			}
		}
		if len(drained[migrationLane(e.coverage)]) > 1 {
			// The final fleet notice supplies the operator action. Keep the
			// local cause before one complete sampled identity within 192 bytes.
			reason = strings.TrimSuffix(reason, "; operator custody decision") + "; " + migrationMultiplicity(drained[migrationLane(e.coverage)])
		}
		p.BlockingReason = pacingText(layoutMigrationReason + ": " + reason)
	}
	for _, ids := range drained {
		if len(ids) < 2 {
			continue
		}
		for _, id := range ids {
			p := t.outcomes[id]
			if p == nil {
				// Display only: unresolved desired image/version stay unknown.
				// No roll entry, pool, reservation or admission is created.
				p = &protocol.Pacing{State: "held", BlockingReason: pacingText(layoutMigrationReason + ": retained drain; operator custody decision")}
				t.outcomes[id] = p
			}
			if p.State == "held" && p.BlockingReason != nil && !strings.Contains(*p.BlockingReason, "; held drains=") {
				cause := strings.TrimSuffix(*p.BlockingReason, "; operator custody decision")
				p.BlockingReason = pacingText(cause + "; " + migrationMultiplicity(ids))
			}
		}
	}
}

// The caller sorts at least two UUIDs. One complete identity leaves room for
// the local cause and final fleet notice within the pacing byte budget.
func migrationMultiplicity(ids []string) string {
	return fmt.Sprintf("held drains=%d; sample=%s", len(ids), ids[0])
}

// Display-only fleet notice must run after materialization can create late held
// outcomes. No identity leaves the affected owner/lane through this prefix.
func (m *Materializer) finalizeMigrationNotice(t *rollTick) {
	if !t.migrationMultiple {
		return
	}
	for _, p := range t.outcomes {
		if p.State != "held" {
			continue
		}
		cause := ""
		if p.BlockingReason != nil {
			cause = *p.BlockingReason
		}
		const prefix = "fleet: multiple migration-held drains; intake risk; operator decision; "
		if detailAt := strings.LastIndex(cause, "; held drains="); detailAt >= 0 {
			local, detail := cause[:detailAt], cause[detailAt:]
			// Count width varies. Reserve the complete metadata suffix before
			// shortening the local cause at a word boundary, never inside a UUID.
			// A UUID and any int-sized count leave room for the custody token.
			budget := 192 - len(prefix) - len(detail)
			if len(local) > budget && budget > len(layoutMigrationReason) {
				cut := strings.LastIndexByte(local[:budget+1], ' ')
				if cut > len(layoutMigrationReason) {
					local = local[:cut]
				}
			}
			cause = local + detail
		}
		p.BlockingReason = pacingText(prefix + cause)
	}
}
