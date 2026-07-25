package workersvc

// Per-run persistence-failure tracking (PRD #108 M4/M5).
//
// WHY THIS EXISTS AND WHY IT IS NOT IN THE DATABASE. PRD #47's health detector
// infers a run's health from persisted run_messages (toolWindow →
// ListRunToolWindow). A run whose messages cannot be persisted is therefore
// invisible to it BY CONSTRUCTION, not by threshold — the wedge is a failure to
// write the detector's only evidence source. The signal that CAN see it is the
// api's own count of AppendMessages failures for that run, produced on the
// failing path itself, needing no cooperation from the worker. That count lives
// here: AppendMessages and detectRunHealth are methods on the same *Service in
// the same process, so the failing writer writes this struct directly and the
// sweeper reads it directly.
//
// 🔴 THE OWNERSHIP TRIPWIRE. This counter drives a KILL (M5). Every recorded
// failure must therefore be reached only AFTER runOwnedByWorker has succeeded for
// the calling worker. If it is not, a worker POSTing to a run it does not own
// drives THAT run's streak toward being stopped — and the check that failed is
// precisely the ownership check. AppendMessages satisfies this because every one
// of its failure returns is below runOwnedByWorker; NoteOversizeBatch satisfies
// it by re-checking ownership itself, because the 413 is answered before any
// ownership check runs. IF YOU ADD A THIRD RECORDING HOOK WITHOUT THAT CHECK,
// YOU HAVE ADDED A CROSS-TENANT KILL PRIMITIVE.
//
// 🔴 THE KEYS ARE WORKER-SUPPLIED. usagepoller's backoff map (the shape this
// mirrors, engine.go) is keyed by ids the server derived from rows it selected,
// so it is bounded by real data. These keys arrive as chi.URLParam on a route a
// worker calls, so an unknown run id would mint an entry that "evict on terminal
// state" can never reach. Hence the cap and the TTL below: they are the memory
// bound, and they are load-bearing rather than hygiene.
//
// 🔴 THE SAFETY DOES NOT REST ON EVICTION. Every write M5 performs is
// status-scoped in SQL (AND status NOT IN ('completed','failed','cancelled')), so
// a stale map entry can at worst cause one no-op UPDATE. A reader who believes
// eviction is the safety mechanism will be tempted to make it clever; it is a
// memory bound, and that is all it is.

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

// persistFailTTL bounds how long an entry survives without being touched. It is
// the only mechanism covering a run whose worker vanished without the run ever
// reaching terminal, and it is deliberately an order of magnitude above M5's 60s
// decision window so it can never expire an entry a live decision is using.
const persistFailTTL = 10 * time.Minute

// persistFailMaxEntries caps each map. A single-replica api runs tens of active
// runs, so this is ~two orders of magnitude of headroom over reality and is here
// only to bound a worker minting run ids. At the cap a NEW entry is refused while
// existing entries keep counting: refusing to start a streak DELAYS a kill, which
// is the fail-safe direction, whereas evicting to make room would let a hostile
// worker clear a genuine run's streak.
const persistFailMaxEntries = 4096

// persistFailCapWarnEvery rate-limits the at-capacity warning. At the incident's
// ~2 Hz an unthrottled line is 7,200 log writes an hour.
const persistFailCapWarnEvery = time.Minute

// persistFailKind classifies WHY an AppendMessages attempt failed. The PRD's
// "same error class each time" guard is implemented as a streak RESET on a change
// of this value rather than as a separate predicate: a rotating error is an
// outage signature, and a streak that cannot accumulate across a rotation can
// never reach the kill.
//
// Deliberately coarse — it classifies by SENTINEL, not by SQLSTATE.
// ErrUnstorableMessage already covers all four enumerated codes (22P05/22P02/
// 22021/22003, sanitize.go), and a payload tripping two of them alternately is
// still the same permanent-poison story. Splitting by SQLSTATE would make the
// guard more fragile, not more precise.
type persistFailKind uint8

const (
	// persistFailNone is the zero value: no failure recorded.
	persistFailNone persistFailKind = iota
	// persistFailUnstorable is ErrUnstorableMessage — 400, permanent by SQLSTATE.
	persistFailUnstorable
	// persistFailInvalid is ErrInvalidMessage — 400, permanent by validation.
	persistFailInvalid
	// persistFailOversize is the 413 answered by the handler before AppendMessages
	// is ever called. Permanent in steady state for a pre-0.10.1 worker, whose
	// retry batch GROWS (PRD #108 M0 defect 4).
	persistFailOversize
	// persistFailStore is everything else — 500, transient BY CONTRACT.
	persistFailStore
)

func (k persistFailKind) String() string {
	switch k {
	case persistFailUnstorable:
		return "unstorable"
	case persistFailInvalid:
		return "invalid"
	case persistFailOversize:
		return "oversize"
	case persistFailStore:
		return "store"
	default:
		return "none"
	}
}

// classifyPersistFail maps an appendMessages error onto its failure class. A nil
// error is persistFailNone; callers never record that.
func classifyPersistFail(err error) persistFailKind {
	switch {
	case err == nil:
		return persistFailNone
	case errors.Is(err, ErrUnstorableMessage):
		return persistFailUnstorable
	case errors.Is(err, ErrInvalidMessage):
		return persistFailInvalid
	default:
		return persistFailStore
	}
}

// persistFailEntry is one run's current failure streak. Never handed out by
// pointer: callers get a persistFailStats value snapshot, so no field can be read
// outside the mutex.
type persistFailEntry struct {
	kind    persistFailKind
	streak  int
	firstAt time.Time // start of the CURRENT streak (a reset restarts it)
	lastAt  time.Time // last failure — TTL eviction only
	lastSeq int32     // runs.last_seq as the last failure observed it
}

// persistFailStats is the read-only view of an entry.
type persistFailStats struct {
	kind    persistFailKind
	streak  int
	firstAt time.Time
	lastSeq int32
}

// persistFailTracker is the whole of PRD #108 Phase 2's state. It is IN-PROCESS
// by design (Decision 7): it is the FOURTH reason the api is a hard singleton,
// and the one whose HA failure is silent — split traffic means neither replica's
// streak reaches the threshold, so auto-stop stops firing rather than misfiring.
// See deploy/chart/values.yaml's api.replicaCount comment.
//
// CONCURRENCY. Service's only other mutable field, lastSlowClampWarn, is
// documented as lock-free because it is sweeper-only. THAT REASONING DOES NOT
// TRANSFER AND MUST NOT BE COPIED HERE: this state is written by N parallel HTTP
// handler goroutines (chi serves each request on its own goroutine, and a fleet
// of workers appends concurrently) and read by the sweeper goroutine. Every field
// access goes through mu. The shape follows usagepoller.Engine's
// inBackoff/setBackoff/clearBackoff — one small method per operation, the lock
// taken and released inside each.
type persistFailTracker struct {
	mu sync.Mutex
	// fail holds the runs currently failing. Bounded by persistFailMaxEntries and
	// pruned by TTL.
	fail map[uuid.UUID]*persistFailEntry
	// lastOK is the last SUCCESSFUL append per run — M5's comparison set, the
	// outage-vs-poison discriminator. Written here in M4 (one map write per
	// successful append) and read only in M5, deliberately: it means the
	// AppendMessages hot path is opened exactly ONCE, so holding M5 reverts nothing
	// and adds no second hook later.
	//
	// A member is a run that ACTUALLY PERSISTED messages, never merely a run that
	// exists — "active" is not "succeeding", and a neighbour mid-long-build appends
	// nothing. Only recordSuccess writes this map, which is what enforces that.
	lastOK map[uuid.UUID]time.Time
	// capWarnAt rate-limits the at-capacity warning.
	capWarnAt time.Time
}

func newPersistFailTracker() *persistFailTracker {
	return &persistFailTracker{
		fail:   make(map[uuid.UUID]*persistFailEntry),
		lastOK: make(map[uuid.UUID]time.Time),
	}
}

// recordSuccess clears any streak for the run and records it in the comparison
// set. Both halves matter: the delete is the ordinary recovery path, and the
// lastOK write is the only evidence M5's G4 will accept that the write path works.
func (t *persistFailTracker) recordSuccess(runID uuid.UUID, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.fail, runID)
	if _, ok := t.lastOK[runID]; !ok && len(t.lastOK) >= persistFailMaxEntries {
		t.warnAtCapLocked(now, "lastOK")
		return
	}
	t.lastOK[runID] = now
}

// recordFailure advances (or restarts) the run's streak.
//
// The reset rules ARE the PRD's "max(seq) has not advanced" and "same error class
// each time" guards, implemented structurally rather than as extra predicates at
// decision time — reaching the threshold IS the proof that neither changed. A
// reset is harder to get wrong than a predicate, and it restarts firstAt too, so
// the streak count and the sustained-duration window always describe the same
// episode.
func (t *persistFailTracker) recordFailure(runID uuid.UUID, kind persistFailKind, lastSeq int32, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.fail[runID]
	if !ok {
		if len(t.fail) >= persistFailMaxEntries {
			t.warnAtCapLocked(now, "fail")
			return
		}
		t.fail[runID] = &persistFailEntry{kind: kind, streak: 1, firstAt: now, lastAt: now, lastSeq: lastSeq}
		return
	}
	if e.kind != kind || e.lastSeq != lastSeq {
		e.kind = kind
		e.lastSeq = lastSeq
		e.streak = 1
		e.firstAt = now
		e.lastAt = now
		return
	}
	e.streak++
	e.lastAt = now
}

// evict drops a run's streak. Called when the run is observed terminal — a
// worker_id survives the terminal transition and neither GetRunOwnedByWorker nor
// AppendMessages filters on status, so a dead run can still be POSTed to and must
// not accumulate a kill streak. The lastOK entry is deliberately KEPT: a
// successful append proves the write path works whatever the run's status.
func (t *persistFailTracker) evict(runID uuid.UUID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.fail, runID)
}

// stats returns a value snapshot of a run's streak, or the zero value if it has
// none. Returning a value rather than *persistFailEntry is what keeps every field
// read inside the lock.
func (t *persistFailTracker) stats(runID uuid.UUID) persistFailStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.fail[runID]
	if !ok {
		return persistFailStats{}
	}
	return persistFailStats{kind: e.kind, streak: e.streak, firstAt: e.firstAt, lastSeq: e.lastSeq}
}

// prune drops entries untouched for persistFailTTL. Called once per sweep tick.
// This is the memory bound for a run whose worker vanished without the run ever
// reaching terminal — the one case no other eviction path covers.
func (t *persistFailTracker) prune(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, e := range t.fail {
		if now.Sub(e.lastAt) > persistFailTTL {
			delete(t.fail, id)
		}
	}
	for id, at := range t.lastOK {
		if now.Sub(at) > persistFailTTL {
			delete(t.lastOK, id)
		}
	}
}

// warnAtCapLocked emits the at-capacity warning at most once per
// persistFailCapWarnEvery. Called with mu held. Worth a line at all because a
// capped tracker is a SILENTLY DISARMED guard, which looks exactly like a healthy
// fleet — the same failure direction the values.yaml singleton comment warns about.
func (t *persistFailTracker) warnAtCapLocked(now time.Time, which string) {
	if !t.capWarnAt.IsZero() && now.Sub(t.capWarnAt) < persistFailCapWarnEvery {
		return
	}
	t.capWarnAt = now
	slog.Warn("workersvc: persistence-failure tracker is at capacity; new runs are not being tracked",
		"map", which, "cap", persistFailMaxEntries)
}
