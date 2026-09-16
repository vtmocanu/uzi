package workersvc

// Per-worker outbox-depth tracking (PRD #1391 M5, Run A).
//
// WHY THIS EXISTS AND WHY IT IS NOT IN THE DATABASE. The worker-owned message
// outbox (M1/M2) buffers a run's frames to disk when the api is unreachable and
// replays them in order once a heartbeat succeeds. While frames sit undrained the
// board looks silent, and PRD #47's health detector — which infers a run's health
// from persisted run_messages — reads that silence as `stalled`, which is exactly
// the misleading signal M5 exists to correct. The only place the api learns the
// depth is the heartbeat's isolated `outbox` field (worker_protocol.go), so this
// tracker keeps the last reported depth per worker, and the detector turns a
// non-zero depth into the truthful "queued on the worker" reason instead.
//
// It is IN-PROCESS and RESTART-LOSING by design, exactly like the persistence-
// failure tracker it is modelled on (persistfail.go): the depth is transient
// telemetry, never a scheduling or authz input, so a restart that forgets it costs
// nothing but one heartbeat of re-reporting. Like persistFailTracker it is the
// reason the api is a hard singleton — a split fleet would scatter the depth across
// replicas — and its worst failure is silent (a depth that never surfaces), which
// looks exactly like a healthy fleet.
//
// CONCURRENCY. This state is written by N parallel HTTP heartbeat goroutines (chi
// serves each request on its own goroutine, and a fleet of workers heartbeats
// concurrently) AND read by the sweeper goroutine (the health detector) and the
// list/register/heartbeat DTO overlay, so EVERY field access goes through mu. The
// shape follows persistFailTracker's — one small method per operation, the lock
// taken and released inside each.

import (
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

// OutboxEntry is one run's outbox depth as a heartbeat reported it, the value the
// handler's parseWorkerOutbox produces and the tracker stores. Every field is
// model-independent worker telemetry — no secret, no repo content — validated and
// bounded by parseWorkerOutbox before it ever reaches here.
type OutboxEntry struct {
	RunID uuid.UUID
	// PendingMessages/PendingTerminal/StaleRetired are non-negative counts (the
	// handler drops any entry that is negative or absurd). In Run A PendingTerminal
	// is always 0 (terminal journaling is Run B); StaleRetired counts frames a
	// re-claim forced this worker to retire locally (D11).
	PendingMessages int
	PendingTerminal int
	StaleRetired    int
	// BlockedReason is the oldest permanent-refusal reason on this run's outbox, or
	// "" when nothing is blocked (never set in Run A; forward-compat for Run B's
	// blocked terminal journals).
	BlockedReason string
	// Since is when this run first started queuing (epoch-ms on the wire, decoded to
	// a time here). It orders the per-worker "oldest blocked reason" aggregate.
	Since time.Time
}

// outboxTrackerMaxWorkers caps the per-worker map. A single-replica api runs a
// fleet of at most tens of workers, so this is ~two orders of magnitude of
// headroom, the same defense-in-depth posture as persistFailMaxEntries. At the cap
// a NEW worker's report is refused (its depth simply is not tracked) while existing
// workers keep updating: dropping a depth report is the fail-safe direction (the
// worst case is a missing "queued" reason), whereas evicting to make room would
// discard a live worker's real depth.
const outboxTrackerMaxWorkers = 4096

// outboxTrackerTTL bounds how long a worker's set survives without a fresh
// heartbeat. It is the memory bound for the one case no other path reaches — a
// worker that vanished without a graceful delete — and the mechanism by which an
// offline worker's stale depth clears: an offline worker stops heartbeating, so its
// set ages out here (the same role persistFailTTL plays for a vanished worker's
// streak). Above the worker-heartbeat-stale window so it can never expire a set an
// online worker is still refreshing.
const outboxTrackerTTL = 10 * time.Minute

// outboxCapWarnEvery rate-limits the at-capacity warning so a saturated tracker
// cannot itself become a log flood.
const outboxCapWarnEvery = time.Minute

// outboxWorkerState is one worker's last reported outbox set. runs is REPLACED
// wholesale on every report (record), so a run the worker no longer lists drops out
// — that is how "clears on the next empty report" works at the per-run grain.
type outboxWorkerState struct {
	updatedAt time.Time
	runs      map[uuid.UUID]OutboxEntry
}

// outboxTracker is the whole of M5's server-side visibility state. Modelled exactly
// on persistFailTracker (mu-guarded maps, a cap, a TTL, prune-on-sweep).
type outboxTracker struct {
	mu sync.Mutex
	// workers holds each worker's last reported set. Bounded by
	// outboxTrackerMaxWorkers, pruned by TTL.
	workers map[uuid.UUID]*outboxWorkerState
	// runIndex maps a run id to the worker that most recently reported it, so
	// runDepth (called by the health detector for every running run each sweep tick)
	// is an O(1) lookup rather than a scan of every worker's set. Kept in lockstep
	// with `workers` by record/evict/prune — every entry in workers[w].runs has
	// runIndex[run] == w unless a newer report for the same run moved it.
	runIndex map[uuid.UUID]uuid.UUID
	// capWarnAt rate-limits the at-capacity warning.
	capWarnAt time.Time
}

func newOutboxTracker() *outboxTracker {
	return &outboxTracker{
		workers:  make(map[uuid.UUID]*outboxWorkerState),
		runIndex: make(map[uuid.UUID]uuid.UUID),
	}
}

// outboxCapWarning is what a locked section hands back so the log line is emitted
// AFTER the mutex is released (the persistFail capWarning idiom): a slog.Warn taken
// under the lock would let a blocked stderr stall every in-flight heartbeat.
type outboxCapWarning struct {
	warn     bool
	entries  int
	workerID uuid.UUID
}

func (c outboxCapWarning) emit() {
	if !c.warn {
		return
	}
	slog.Warn("workersvc: outbox tracker is at capacity; this worker's outbox depth is NOT being tracked",
		"workers", c.entries, "cap", outboxTrackerMaxWorkers, "worker_id", c.workerID.String())
}

// record REPLACES the worker's whole reported set. An empty/nil entries CLEARS the
// worker's set (the "clears on the next empty report" contract): the drainer empties
// the outbox, the worker's next heartbeat carries no entries, and the depth vanishes.
func (t *outboxTracker) record(workerID uuid.UUID, entries []OutboxEntry, now time.Time) {
	t.applyRecord(workerID, entries, now).emit()
}

func (t *outboxTracker) applyRecord(workerID uuid.UUID, entries []OutboxEntry, now time.Time) outboxCapWarning {
	t.mu.Lock()
	defer t.mu.Unlock()

	// A report REPLACES the whole set, so first drop this worker's previous runs from
	// the reverse index; the loop below re-adds only the runs it still lists.
	t.forgetWorkerRunsLocked(workerID)

	if len(entries) == 0 {
		delete(t.workers, workerID)
		return outboxCapWarning{}
	}

	st, ok := t.workers[workerID]
	if !ok {
		if len(t.workers) >= outboxTrackerMaxWorkers {
			return t.noteAtCapLocked(len(t.workers), workerID, now)
		}
		st = &outboxWorkerState{}
		t.workers[workerID] = st
	}
	runs := make(map[uuid.UUID]OutboxEntry, len(entries))
	for _, e := range entries {
		runs[e.RunID] = e
		t.runIndex[e.RunID] = workerID
	}
	st.runs = runs
	st.updatedAt = now
	return outboxCapWarning{}
}

// forgetWorkerRunsLocked removes a worker's runs from the reverse index. The owner
// guard matters: another worker may have re-reported the same run since (runIndex
// then points at the newer owner), and that newer mapping must survive.
func (t *outboxTracker) forgetWorkerRunsLocked(workerID uuid.UUID) {
	st, ok := t.workers[workerID]
	if !ok {
		return
	}
	for runID := range st.runs {
		if t.runIndex[runID] == workerID {
			delete(t.runIndex, runID)
		}
	}
}

// runDepth returns the outbox entry for a run, across whichever worker reported it.
// O(1) via the reverse index — the health detector calls it for every running run
// each sweep tick.
func (t *outboxTracker) runDepth(runID uuid.UUID) (OutboxEntry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	workerID, ok := t.runIndex[runID]
	if !ok {
		return OutboxEntry{}, false
	}
	st, ok := t.workers[workerID]
	if !ok {
		return OutboxEntry{}, false
	}
	e, ok := st.runs[runID]
	return e, ok
}

// workerAggregate sums a worker's outbox depth across its runs, for the DTO overlay.
// blocked is the oldest non-empty BlockedReason (or nil). Returns all zero / nil
// when the worker has no tracked set, so a worker with no outbox shows nulls.
func (t *outboxTracker) workerAggregate(workerID uuid.UUID) (pendingMessages, pendingTerminal, staleRetired int, blocked *string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.workers[workerID]
	if !ok {
		return 0, 0, 0, nil
	}
	var oldest *OutboxEntry
	for runID := range st.runs {
		e := st.runs[runID]
		pendingMessages += e.PendingMessages
		pendingTerminal += e.PendingTerminal
		staleRetired += e.StaleRetired
		if e.BlockedReason != "" && (oldest == nil || e.Since.Before(oldest.Since)) {
			cp := e
			oldest = &cp
		}
	}
	if oldest != nil {
		reason := oldest.BlockedReason
		blocked = &reason
	}
	return pendingMessages, pendingTerminal, staleRetired, blocked
}

// evict drops a worker's whole set — called when the worker is deleted, so a removed
// worker's last-known depth does not linger on the fleet view.
func (t *outboxTracker) evict(workerID uuid.UUID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.forgetWorkerRunsLocked(workerID)
	delete(t.workers, workerID)
}

// prune drops workers whose set has not been refreshed within outboxTrackerTTL.
// Called once per sweep tick. This is the memory bound and the offline-clear path:
// an offline worker stops heartbeating, so its set ages out here.
func (t *outboxTracker) prune(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, st := range t.workers {
		if now.Sub(st.updatedAt) > outboxTrackerTTL {
			t.forgetWorkerRunsLocked(id)
			delete(t.workers, id)
		}
	}
}

// noteAtCapLocked decides whether the at-capacity warning is due, stamping the
// throttle when it is. Called with mu held; the caller emits after unlocking.
func (t *outboxTracker) noteAtCapLocked(entries int, workerID uuid.UUID, now time.Time) outboxCapWarning {
	if !t.capWarnAt.IsZero() && now.Sub(t.capWarnAt) < outboxCapWarnEvery {
		return outboxCapWarning{}
	}
	t.capWarnAt = now
	return outboxCapWarning{warn: true, entries: entries, workerID: workerID}
}
