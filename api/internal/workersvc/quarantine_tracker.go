package workersvc

// Per-worker residue-quarantine tracking (issue #2213).
//
// A single-uid worker that finds an unreadable, unattributed runner-uid process latches
// a process-wide quarantine: it claims nothing and starts no forge-credentialed git or
// provider turn until its container restarts. The only way the api learns of it is the
// heartbeat's top-level `residue_quarantine` member (gated by the
// `worker_residue_quarantine` protocol feature), so this tracker keeps the last report
// per worker and the DTO overlay and the fleet.quarantine health check read it.
//
// It is IN-PROCESS and RESTART-LOSING by design, exactly like outboxTracker: the report
// is transient telemetry, never a scheduling or authz input, and a restarted api relearns
// it from the very next heartbeat of a still-latched worker. A heartbeat WITHOUT the
// member clears the entry, so the visible state follows the worker's own state tick by
// tick. Register and delete clear it too; the TTL prune covers a worker that vanished.
//
// Every field is untrusted worker self-report; the handler sanitizes and bounds it
// before it reaches record.

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// ResidueQuarantine is one worker's reported latch, already validated and sanitized by
// the handler's parseWorkerResidueQuarantine.
type ResidueQuarantine struct {
	// Cause is the sanitized, length-bounded detail text. Untrusted: every surface
	// renders it as plain text.
	Cause string
	// LatchedAt is when the worker latched (the worker's own clock).
	LatchedAt time.Time
	// RunID is the run whose check detected the process, nil when none applies.
	RunID *uuid.UUID
	// Site is the sanitized detection site name.
	Site string
}

// quarantineTrackerMaxWorkers caps the map, the same defense-in-depth posture as
// outboxTrackerMaxWorkers. At the cap a NEW worker's report is not tracked while
// existing entries keep updating.
const quarantineTrackerMaxWorkers = 4096

// quarantineTrackerTTL bounds how long an entry survives without a refreshing
// heartbeat (a worker that vanished without a graceful delete). Above the worker
// heartbeat-stale window so it never expires an entry an online worker still refreshes.
const quarantineTrackerTTL = 10 * time.Minute

type quarantineState struct {
	q         ResidueQuarantine
	updatedAt time.Time
}

type quarantineTracker struct {
	mu      sync.Mutex
	workers map[uuid.UUID]quarantineState
}

func newQuarantineTracker() *quarantineTracker {
	return &quarantineTracker{workers: make(map[uuid.UUID]quarantineState)}
}

// record REPLACES the worker's entry; a nil report clears it (the heartbeat that omits
// the member is the worker saying it is not latched).
func (t *quarantineTracker) record(workerID uuid.UUID, q *ResidueQuarantine, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if q == nil {
		delete(t.workers, workerID)
		return
	}
	if _, ok := t.workers[workerID]; !ok && len(t.workers) >= quarantineTrackerMaxWorkers {
		return
	}
	t.workers[workerID] = quarantineState{q: *q, updatedAt: now}
}

func (t *quarantineTracker) get(workerID uuid.UUID) (ResidueQuarantine, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.workers[workerID]
	if !ok {
		return ResidueQuarantine{}, false
	}
	return st.q, true
}

// evictIfSet drops a worker's entry (worker deleted or re-registered). Nil-safe.
func (t *quarantineTracker) evictIfSet(workerID uuid.UUID) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.workers, workerID)
}

// prune drops entries not refreshed within quarantineTrackerTTL. Called once per sweep.
func (t *quarantineTracker) prune(now time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, st := range t.workers {
		if now.Sub(st.updatedAt) > quarantineTrackerTTL {
			delete(t.workers, id)
		}
	}
}
