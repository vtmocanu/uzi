package workersvc

// PRD #1391 M5 — the in-process outbox-depth tracker (outbox_tracker.go). These
// drive the tracker directly (record/aggregate/runDepth/evict/prune) because that is
// how the signal travels: it never touches the database, so a DB fake would test
// nothing.

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestOutboxTrackerRecordAndAggregate(t *testing.T) {
	tr := newOutboxTracker()
	w := uuid.New()
	r1, r2 := uuid.New(), uuid.New()
	now := time.Unix(1000, 0)
	tr.record(w, []OutboxEntry{
		{RunID: r1, PendingMessages: 3, PendingTerminal: 0, StaleRetired: 1, Since: now.Add(-2 * time.Minute)},
		{RunID: r2, PendingMessages: 5, PendingTerminal: 2, StaleRetired: 0, BlockedReason: "reserve_exhausted", Since: now.Add(-time.Minute)},
	}, now)

	pm, pt, sr, blocked := tr.workerAggregate(w)
	if pm != 8 || pt != 2 || sr != 1 {
		t.Fatalf("aggregate = (%d,%d,%d), want (8,2,1)", pm, pt, sr)
	}
	if blocked == nil || *blocked != "reserve_exhausted" {
		t.Fatalf("blocked = %v, want reserve_exhausted", blocked)
	}
}

func TestOutboxTrackerAggregateReportsOldestBlocked(t *testing.T) {
	tr := newOutboxTracker()
	w := uuid.New()
	now := time.Unix(2000, 0)
	tr.record(w, []OutboxEntry{
		{RunID: uuid.New(), PendingMessages: 1, BlockedReason: "newer_block", Since: now.Add(-time.Minute)},
		{RunID: uuid.New(), PendingMessages: 1, BlockedReason: "older_block", Since: now.Add(-10 * time.Minute)},
	}, now)
	_, _, _, blocked := tr.workerAggregate(w)
	if blocked == nil || *blocked != "older_block" {
		t.Fatalf("blocked = %v, want older_block (oldest Since wins)", blocked)
	}
}

func TestOutboxTrackerAggregateUntrackedWorkerIsAllNull(t *testing.T) {
	tr := newOutboxTracker()
	pm, pt, sr, blocked := tr.workerAggregate(uuid.New())
	if pm != 0 || pt != 0 || sr != 0 || blocked != nil {
		t.Fatalf("untracked worker aggregate = (%d,%d,%d,%v), want all zero/nil", pm, pt, sr, blocked)
	}
}

func TestOutboxTrackerRunDepth(t *testing.T) {
	tr := newOutboxTracker()
	w := uuid.New()
	r := uuid.New()
	now := time.Unix(1000, 0)
	if _, ok := tr.runDepth(r); ok {
		t.Fatal("runDepth present before any record")
	}
	tr.record(w, []OutboxEntry{{RunID: r, PendingMessages: 7, Since: now}}, now)
	e, ok := tr.runDepth(r)
	if !ok || e.PendingMessages != 7 || e.RunID != r {
		t.Fatalf("runDepth = (%+v,%v), want pending 7 for %s", e, ok, r)
	}
}

func TestOutboxTrackerClearsOnEmptyReport(t *testing.T) {
	tr := newOutboxTracker()
	w := uuid.New()
	r := uuid.New()
	now := time.Unix(1000, 0)
	tr.record(w, []OutboxEntry{{RunID: r, PendingMessages: 4, Since: now}}, now)
	// The drainer emptied the outbox; the next heartbeat carries no entries and clears.
	tr.record(w, nil, now.Add(time.Second))
	if _, ok := tr.runDepth(r); ok {
		t.Fatal("runDepth present after an empty report; the set must clear on the next empty report")
	}
	if pm, _, _, blocked := tr.workerAggregate(w); pm != 0 || blocked != nil {
		t.Fatalf("aggregate = (%d,%v) after clear, want (0,nil)", pm, blocked)
	}
}

func TestOutboxTrackerRecordReplacesWholeSet(t *testing.T) {
	tr := newOutboxTracker()
	w := uuid.New()
	r1, r2 := uuid.New(), uuid.New()
	now := time.Unix(1000, 0)
	tr.record(w, []OutboxEntry{
		{RunID: r1, PendingMessages: 2, Since: now},
		{RunID: r2, PendingMessages: 3, Since: now},
	}, now)
	// A later report lists only r2: r1 must drop out entirely (index included).
	tr.record(w, []OutboxEntry{{RunID: r2, PendingMessages: 9, Since: now}}, now.Add(time.Second))
	if _, ok := tr.runDepth(r1); ok {
		t.Fatal("r1 present after a report that omitted it; a report REPLACES the whole set")
	}
	if e, ok := tr.runDepth(r2); !ok || e.PendingMessages != 9 {
		t.Fatalf("r2 = (%+v,%v), want pending 9", e, ok)
	}
	if pm, _, _, _ := tr.workerAggregate(w); pm != 9 {
		t.Fatalf("aggregate pm = %d, want 9 (only r2 remains)", pm)
	}
}

func TestOutboxTrackerEvict(t *testing.T) {
	tr := newOutboxTracker()
	w := uuid.New()
	r := uuid.New()
	now := time.Unix(1000, 0)
	tr.record(w, []OutboxEntry{{RunID: r, PendingMessages: 1, Since: now}}, now)
	tr.evict(w)
	if _, ok := tr.runDepth(r); ok {
		t.Fatal("runDepth present after evict")
	}
	if pm, _, _, _ := tr.workerAggregate(w); pm != 0 {
		t.Fatalf("aggregate after evict = %d, want 0", pm)
	}
}

func TestOutboxTrackerPruneEvictsStale(t *testing.T) {
	tr := newOutboxTracker()
	w := uuid.New()
	r := uuid.New()
	base := time.Unix(10000, 0)
	tr.record(w, []OutboxEntry{{RunID: r, PendingMessages: 1, Since: base}}, base)
	// Within the TTL: survives.
	tr.prune(base.Add(outboxTrackerTTL - time.Second))
	if _, ok := tr.runDepth(r); !ok {
		t.Fatal("pruned within TTL; a fresh set must survive")
	}
	// Past the TTL: gone (from the reverse index too — runDepth proves it).
	tr.prune(base.Add(outboxTrackerTTL + time.Second))
	if _, ok := tr.runDepth(r); ok {
		t.Fatal("not pruned past TTL; a vanished/offline worker's set must age out")
	}
}

func TestOutboxTrackerCapRefusesNewWorker(t *testing.T) {
	tr := newOutboxTracker()
	now := time.Unix(1000, 0)
	for i := 0; i < outboxTrackerMaxWorkers; i++ {
		tr.record(uuid.New(), []OutboxEntry{{RunID: uuid.New(), PendingMessages: 1, Since: now}}, now)
	}
	// A NEW worker at cap is refused (its depth is simply not tracked) — the fail-safe
	// direction, whereas evicting to make room would drop a live worker's real depth.
	newW, newR := uuid.New(), uuid.New()
	tr.record(newW, []OutboxEntry{{RunID: newR, PendingMessages: 1, Since: now}}, now)
	if _, ok := tr.runDepth(newR); ok {
		t.Fatal("a new worker's report was tracked past the cap; the cap must refuse to start a new entry")
	}
}
