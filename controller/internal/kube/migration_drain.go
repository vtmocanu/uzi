package kube

import (
	"context"
	"fmt"
	"sort"

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

func (m *Materializer) prepareMigrationDrains(t *rollTick, desired []protocol.DesiredWorker, observed map[string]reconcile.ObservedWorker) {
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
		if w.Ephemeral || !migrationHeld(w, observed[w.ID].RunnerLayout) {
			continue
		}
		if w.DrainingSince != nil || w.DrainingReason != nil {
			t.unavailable[w.ID] = true
		}
	}
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
		if w.Ephemeral || !migrationHeld(w, observed[w.ID].RunnerLayout) {
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
		if w.DrainingSince != nil && ok {
			drained[migrationLane(c)] = append(drained[migrationLane(c)], w.ID)
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
	for lane, ids := range drained {
		if len(ids) > 1 {
			t.migrationMultiple = true
			sort.Strings(ids)
			for _, e := range t.entries {
				if migrationLane(e.coverage) == lane && t.outcomes[e.w.ID].State == "held" {
					t.outcomes[e.w.ID].BlockingReason = pacingText(layoutMigrationReason + ": " + migrationMultiplicity(ids) + "; operator custody decision")
				}
			}
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
			reason = migrationMultiplicity(drained[migrationLane(e.coverage)]) + "; " + reason
		}
		p.BlockingReason = pacingText(layoutMigrationReason + ": " + reason)
	}
}

// The caller sorts identities; include a count and a bounded sample, not an
// unbounded list that would crowd the cause out of the pacing text.
func migrationMultiplicity(ids []string) string {
	return fmt.Sprintf("multiple held drains=%d; sample=%v", len(ids), ids[:min(2, len(ids))])
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
		p.BlockingReason = pacingText("fleet: multiple migration-held drains; operator custody decision; " + layoutMigrationReason + "; " + cause)
	}
}
