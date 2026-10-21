package workersvc

// Per-worker memory-authority-loss tracking (issue #3142).
//
// A capable worker reports "held" or "lost" on each run-lane claim body; a gated claim
// (memoryAuthorityLost) records the worker here so surfaces can show since when the api
// has been withholding guard-bound runs from it. Like quarantineTracker it is IN-PROCESS
// and RESTART-LOSING by design: transient telemetry, never a scheduling input (the gate
// itself reads only the request and the persisted capabilities). A "held" claim,
// Register and worker delete clear it; the TTL prune covers a vanished worker.

import (
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
)

// MemoryAuthorityReport is the run-lane claim body's memory_authority member.
// Value is nil when the member was absent.
type MemoryAuthorityReport struct {
	Value *string
}

// memoryAuthorityHeld is the one value that means the worker holds memory authority.
const memoryAuthorityHeld = "held"

func (r MemoryAuthorityReport) held() bool { return r.Value != nil && *r.Value == memoryAuthorityHeld }

// memoryAuthorityLost is the claim-time judgment: a present member that is not exactly
// "held" is lost (trusted alone: only a capable image sends it); an absent member is lost
// only for a worker that registered BOTH the memory-pressure and the claim-report
// capabilities (a capable worker that stopped sending it, e.g. after clearFeatures).
func memoryAuthorityLost(protocolCaps []string, report MemoryAuthorityReport) bool {
	if report.Value != nil {
		return !report.held()
	}
	return slices.Contains(protocolCaps, capability.WorkerMemoryPressureV1) &&
		slices.Contains(protocolCaps, capability.WorkerMemoryAuthorityClaimV1)
}

// memoryAuthorityTrackerMaxWorkers caps the map; at the cap a NEW worker is not tracked.
const memoryAuthorityTrackerMaxWorkers = 4096

// memoryAuthorityTrackerTTL bounds how long an entry survives without a refreshing gated claim.
const memoryAuthorityTrackerTTL = 24 * time.Hour

type memoryAuthorityState struct {
	since     time.Time
	updatedAt time.Time
}

type memoryAuthorityTracker struct {
	mu      sync.Mutex
	workers map[uuid.UUID]memoryAuthorityState
}

func newMemoryAuthorityTracker() *memoryAuthorityTracker {
	return &memoryAuthorityTracker{workers: make(map[uuid.UUID]memoryAuthorityState)}
}

// recordLost keeps the first-observed time and refreshes updatedAt.
func (t *memoryAuthorityTracker) recordLost(workerID uuid.UUID, now time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.workers[workerID]
	if !ok {
		if len(t.workers) >= memoryAuthorityTrackerMaxWorkers {
			return
		}
		st.since = now
	}
	st.updatedAt = now
	t.workers[workerID] = st
}

func (t *memoryAuthorityTracker) get(workerID uuid.UUID) (time.Time, bool) {
	if t == nil {
		return time.Time{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.workers[workerID]
	return st.since, ok
}

// evictIfSet drops a worker's entry (held claim, re-register, delete). Nil-safe.
func (t *memoryAuthorityTracker) evictIfSet(workerID uuid.UUID) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.workers, workerID)
}

// prune drops entries not refreshed within memoryAuthorityTrackerTTL. Called once per sweep.
func (t *memoryAuthorityTracker) prune(now time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, st := range t.workers {
		if now.Sub(st.updatedAt) > memoryAuthorityTrackerTTL {
			delete(t.workers, id)
		}
	}
}
