package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestQuarantineTrackerSetClearEvictPrune(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	tr := newQuarantineTracker()
	a, b := uuid.New(), uuid.New()
	runID := uuid.New()
	q := &ResidueQuarantine{Cause: "c", LatchedAt: now, RunID: &runID, Site: "pre_clone"}

	tr.record(a, q, now)
	tr.record(b, q, now)
	got, ok := tr.get(a)
	if !ok || got.Cause != "c" || got.RunID == nil || *got.RunID != runID || got.Site != "pre_clone" {
		t.Fatalf("get(a) = %+v ok=%v", got, ok)
	}

	// A nil report (a heartbeat without the member) clears only that worker.
	tr.record(a, nil, now)
	if _, ok := tr.get(a); ok {
		t.Fatal("nil report must clear")
	}
	if _, ok := tr.get(b); !ok {
		t.Fatal("clearing a must not touch b")
	}

	// evict (register / delete) clears.
	tr.evictIfSet(b)
	if _, ok := tr.get(b); ok {
		t.Fatal("evict must clear")
	}

	// prune drops only entries past the TTL.
	tr.record(a, q, now)
	tr.record(b, q, now.Add(-quarantineTrackerTTL-time.Second))
	tr.prune(now)
	if _, ok := tr.get(b); ok {
		t.Fatal("a stale entry must be pruned")
	}
	if _, ok := tr.get(a); !ok {
		t.Fatal("a fresh entry must survive the prune")
	}
}

func TestQuarantineTrackerCapRefusesNewWorkersButUpdatesExisting(t *testing.T) {
	now := time.Now()
	tr := newQuarantineTracker()
	first := uuid.New()
	tr.record(first, &ResidueQuarantine{Cause: "one", LatchedAt: now}, now)
	for i := 1; i < quarantineTrackerMaxWorkers; i++ {
		tr.record(uuid.New(), &ResidueQuarantine{Cause: "x", LatchedAt: now}, now)
	}
	extra := uuid.New()
	tr.record(extra, &ResidueQuarantine{Cause: "new", LatchedAt: now}, now)
	if _, ok := tr.get(extra); ok {
		t.Fatal("a new worker past the cap must not be tracked")
	}
	tr.record(first, &ResidueQuarantine{Cause: "two", LatchedAt: now}, now)
	if got, _ := tr.get(first); got.Cause != "two" {
		t.Fatalf("an existing worker must keep updating at the cap, got %q", got.Cause)
	}
}

// A struct-literal Service (no New) has a nil tracker; every accessor reads "not latched".
func TestServiceQuarantineAccessorsAreNilSafe(t *testing.T) {
	s := &Service{}
	id := uuid.New()
	s.RecordResidueQuarantine(id, &ResidueQuarantine{Cause: "c"})
	if _, ok := s.ResidueQuarantineFor(id); ok {
		t.Fatal("a service without a tracker must read as not latched")
	}
	s.quarantine.evictIfSet(id)
	s.quarantine.prune(time.Now())
}
