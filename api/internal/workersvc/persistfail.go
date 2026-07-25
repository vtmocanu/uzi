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
// 🔴 WHAT BOUNDS THESE MAPS — and it is the ownership gate, not the cap.
//
// This block used to say the keys were worker-supplied, so an unknown run id
// would mint an unreachable entry, so the cap was load-bearing. THAT WAS FALSE and
// two validators found it independently. The keys arrive as chi.URLParam, but only
// recordFailure mints, and both of its production callers sit BELOW the ownership
// gate (AppendMessages' default arm requires obs.resolved; NoteOversizeBatch
// returns on any runOwnedByWorker error). An unknown run id gives
// pgx.ErrNoRows → ErrRunNotOwned and records nothing. So these keys ARE
// server-derived from rows it selected — the same property usagepoller's backoff
// map has, not the contrast this once claimed.
//
// What each mechanism actually covers:
//
//   - the OWNERSHIP GATE bounds the key space to runs really claimed by the
//     caller's own workers. It is the memory bound. Weakening it re-opens both
//     the unbounded-growth and the cross-tenant-kill vectors at once.
//   - the TTL covers the one case eviction cannot reach: a run whose worker
//     vanished without the run ever reaching terminal.
//   - the CAP is defense in depth. Its residual threat model is one legitimate
//     user holding more than persistFailMaxEntries concurrently-failing OWNED
//     runs — closer to hygiene than to a security control. Do not cite it as the
//     thing that stops a hostile worker; the gate above does that.
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
// runs, so this is ~two orders of magnitude of headroom over reality. Defense in
// depth behind the ownership gate (see the header) rather than the memory bound
// itself. At the cap a NEW entry is refused while existing entries keep counting:
// refusing to start a streak DELAYS a kill, which is the fail-safe direction,
// whereas evicting to make room would let a worker clear a genuine run's streak.
const persistFailMaxEntries = 4096

// persistFailCapWarnEvery rate-limits the at-capacity warning. At the incident's
// ~2 Hz an unthrottled line is 7,200 log writes an hour.
const persistFailCapWarnEvery = time.Minute

// The two maps, as throttle slots. The at-capacity warning is throttled PER MAP:
// a single shared timestamp meant a lastOK cap event inside the same minute as a
// fail one was suppressed and never appeared at all, so an operator could watch
// `map=fail` indefinitely while lastOK was also saturated. This is the
// observability guard on a guard that disarms silently — it must not itself be lossy.
const (
	persistMapFail = iota
	persistMapLastOK
	persistMapCount
)

func persistMapName(slot int) string {
	if slot == persistMapLastOK {
		return "lastOK"
	}
	return "fail"
}

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
	// persistFailInvalid is ErrInvalidMessage — 400, rejected in the validation loop
	// before any database write. Permanent for the batch as sent, but NOT killable
	// (autoStopKillableKinds): no correct worker of any version can produce it, so a
	// streak of it means the CLIENT is broken rather than the world being hostile.
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
	// stopReqAt is when a stop verdict was enqueued for this run (M5), zero until
	// then. It is the escalation clock, and it is deliberately NOT reset by a class
	// or seq change: those restart the EVIDENCE, while this records that we already
	// acted. A restart loses it, which forgets the escalation and restarts the clock
	// — a delay, never a double kill (and the verdict row itself survives in
	// run_user_inputs, so the stop is not lost, only our bookkeeping about it).
	stopReqAt time.Time
	// declineLoggedAt rate-limits the "auto-stop is holding" line to once per
	// autoStopWindow. A wedged run is evaluated every sweep tick for as long as it
	// lives, and an unthrottled line would be hours of identical warnings.
	declineLoggedAt time.Time
}

// persistFailStats is the read-only view of an entry.
type persistFailStats struct {
	kind      persistFailKind
	streak    int
	firstAt   time.Time
	lastSeq   int32
	stopReqAt time.Time
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
	// capWarnAt rate-limits the at-capacity warning, one slot per map.
	capWarnAt [persistMapCount]time.Time
}

// capWarning is what a locked section hands back so the log line can be emitted
// AFTER the mutex is released.
//
// This mutex is on the hot path of every successful append, so a slog.Warn taken
// under it means a blocked stderr — a full pipe to a log collector — stalls every
// in-flight /messages goroutine. The once-a-minute throttle bounds the exposure to
// one stall per minute rather than one per request, which is what keeps this small;
// it is still the wrong place to do I/O.
type capWarning struct {
	warn    bool
	slot    int
	entries int
	runID   uuid.UUID
}

func (c capWarning) emit() {
	if !c.warn {
		return
	}
	slog.Warn("workersvc: persistence-failure tracker is at capacity; this run is NOT being tracked",
		"map", persistMapName(c.slot), "entries", c.entries, "cap", persistFailMaxEntries, "run_id", c.runID.String())
}

func newPersistFailTracker() *persistFailTracker {
	return &persistFailTracker{
		fail:   make(map[uuid.UUID]*persistFailEntry),
		lastOK: make(map[uuid.UUID]time.Time),
	}
}

// recordSuccess clears any streak for the run and records it in the comparison
// set. Both halves matter: the delete is the ordinary recovery path, and the
// lastOK write is the only evidence M5's comparison-set guard will accept that the
// write path works.
func (t *persistFailTracker) recordSuccess(runID uuid.UUID, now time.Time) {
	t.applySuccess(runID, now).emit()
}

func (t *persistFailTracker) applySuccess(runID uuid.UUID, now time.Time) capWarning {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.fail, runID)
	if _, ok := t.lastOK[runID]; !ok && len(t.lastOK) >= persistFailMaxEntries {
		return t.noteAtCapLocked(persistMapLastOK, len(t.lastOK), runID, now)
	}
	t.lastOK[runID] = now
	return capWarning{}
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
	t.applyFailure(runID, kind, lastSeq, now).emit()
}

func (t *persistFailTracker) applyFailure(runID uuid.UUID, kind persistFailKind, lastSeq int32, now time.Time) capWarning {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.fail[runID]
	if !ok {
		if len(t.fail) >= persistFailMaxEntries {
			return t.noteAtCapLocked(persistMapFail, len(t.fail), runID, now)
		}
		t.fail[runID] = &persistFailEntry{kind: kind, streak: 1, firstAt: now, lastAt: now, lastSeq: lastSeq}
		return capWarning{}
	}
	if e.kind != kind || e.lastSeq != lastSeq {
		e.kind = kind
		e.lastSeq = lastSeq
		e.streak = 1
		e.firstAt = now
		e.lastAt = now
		return capWarning{}
	}
	e.streak++
	e.lastAt = now
	return capWarning{}
}

// evict drops a run from BOTH maps. Called when the run is observed terminal — a
// worker_id survives the terminal transition and neither GetRunOwnedByWorker nor
// AppendMessages filters on status, so a dead run can still be POSTed to and must
// not accumulate a kill streak.
//
// It drops the lastOK entry too, and that half is a security property rather than
// tidiness. An earlier version kept it, reasoning that a successful append proves
// the write path works whatever the run's status. True but insufficient: the
// comparison-set guard is a
// GLOBAL "other runs are succeeding", so a worker holding one terminal run and
// re-POSTing a deduplicated append every few minutes — near-zero cost, no tokens,
// no slot — could keep the kill armed for every OTHER user's run on the instance.
// Requiring the comparison set to be non-terminal means warming it costs a live
// run that is really doing work.
func (t *persistFailTracker) evict(runID uuid.UUID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.fail, runID)
	delete(t.lastOK, runID)
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
	return statsOf(e)
}

func statsOf(e *persistFailEntry) persistFailStats {
	return persistFailStats{kind: e.kind, streak: e.streak, firstAt: e.firstAt, lastSeq: e.lastSeq, stopReqAt: e.stopReqAt}
}

// persistFailCandidate is one run the auto-stop evaluator should look at, with its
// state snapshotted under the lock.
type persistFailCandidate struct {
	runID uuid.UUID
	persistFailStats
}

// candidates returns every run whose streak has reached the kill threshold on both
// legs, as VALUE snapshots.
//
// The candidate set is the in-process map and NOT ListActiveRunsForHealth, which is
// what makes a wedged CHAT run covered: that query ends `AND kind <> 'chat'`, while
// agent/src/chat-runner.ts builds the same MessageBatcher against the same
// /messages route, so a chat run wedges identically. It is also why the evaluator
// costs zero queries in the common case — this map is normally empty.
func (t *persistFailTracker) candidates(now time.Time, minStreak int, minWindow time.Duration) []persistFailCandidate {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []persistFailCandidate
	for id, e := range t.fail {
		if e.streak >= minStreak && !e.firstAt.IsZero() && now.Sub(e.firstAt) >= minWindow {
			out = append(out, persistFailCandidate{runID: id, persistFailStats: statsOf(e)})
		}
	}
	return out
}

// peersSucceeding counts runs OTHER than runID that persisted messages inside the
// window. This is the outage-vs-poison discriminator, and it is the whole safety
// argument for killing anything at all.
//
// Membership, stated because every clause is load-bearing:
//   - a member is any OTHER run id — including a chat run, since chat appends ride
//     the same route and the same recorder. Deliberate: a healthy chat proves the
//     write path works, which is the only question this asks.
//   - runID itself NEVER counts, not even for its own pre-streak successes.
//     recordSuccess writes lastOK[runID], so the exclusion has to be explicit here.
//   - "active" is not "succeeding": a neighbour mid-long-build appends nothing and
//     is not a member. Only recordSuccess writes lastOK, which enforces that.
//   - a run that appended one second past the window is not a member. Strict recency.
//
// When the answer is zero the rule is FLAG AND DO NOT KILL — permanently, with no
// fallback and no timeout into killing (Decision 5).
//
// This paragraph used to end "the outage case and the lonely-instance case are the
// SAME PREDICATE, not two mechanisms." THAT IS NOW FALSE and it is worth saying why,
// because it was true when written: a realistic api-wide outage is the `store` class,
// and since autoStopKillableKinds landed, the KILLABLE-CLASS guard refuses `store`
// BEFORE this function is
// ever called. So a transient outage is stopped by the CLASS gate, not by an empty
// comparison set. MEASURED — folding this function to `return 99` leaves the transient
// outage test green and reddens the others. It was true only while nothing upstream
// refused `store`.
//
// (An earlier draft of this block ended "the guard that made the old sentence true is
// the guard that falsified it" — memorable and WRONG, which is the dangerous pair.
// This guard made it true; the KILLABLE-CLASS guard falsified it, by taking the
// outage away from this one before it
// arrives. Two different guards. It was quoted approvingly in a review before anyone
// checked it, which is exactly how a quotable formulation travels into a spec.)
//
// What this function alone still proves is the case where the class guard PASSES
// and only this one
// stands: every active run failing on a KILLING class, i.e. a fleet-wide worker bug.
// That is TestAutoStopFleetWideKillingClassStopsNothing, and it is the comparison
// set's only real proof.
//
// 🔴 DO NOT ALSO EXCLUDE PEERS HELD BY THE FAILING RUN'S OWN WORKER. It was
// proposed and declined deliberately. This guard asks "IS THE API'S WRITE PATH
// WORKING?", and a peer on the same worker persisting successfully answers that
// correctly — the api did persist it. "Is the worker at fault?" is a different
// question, it is the KILLABLE-CLASS guard's, and that guard answers it with the
// could-a-correct-worker test. A same-worker exclusion would make this guard attempt
// that one's job with a worse
// instrument. (It would also bite only at WORKER_MAX_CONCURRENT_RUNS > 1, since the
// default is 1 — agent/src/config.ts — where a single-worker instance has one run
// and this count is already zero.)
func (t *persistFailTracker) peersSucceeding(runID uuid.UUID, now time.Time, window time.Duration) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for id, at := range t.lastOK {
		if id == runID {
			continue
		}
		if now.Sub(at) <= window {
			n++
		}
	}
	return n
}

// markStopRequested stamps the escalation clock. No-op if the entry is gone (the
// run recovered or was evicted between the decision and this call).
func (t *persistFailTracker) markStopRequested(runID uuid.UUID, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.fail[runID]; ok {
		e.stopReqAt = now
	}
}

// shouldLogDecline reports whether the "auto-stop is holding" line is due for this
// run, stamping it when it is. Rate-limited to once per `every`.
func (t *persistFailTracker) shouldLogDecline(runID uuid.UUID, now time.Time, every time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.fail[runID]
	if !ok {
		return false
	}
	if !e.declineLoggedAt.IsZero() && now.Sub(e.declineLoggedAt) < every {
		return false
	}
	e.declineLoggedAt = now
	return true
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

// noteAtCapLocked decides whether the at-capacity warning is due for this map,
// stamping the per-map throttle when it is. Called with mu held; the caller emits
// the line after unlocking (see capWarning).
//
// Worth a line at all because a capped tracker is a SILENTLY DISARMED guard, which
// looks exactly like a healthy fleet — the same failure direction the values.yaml
// singleton comment warns about. It carries the map, the live entry count and the
// run it refused, so the line is actionable rather than merely alarming.
func (t *persistFailTracker) noteAtCapLocked(slot, entries int, runID uuid.UUID, now time.Time) capWarning {
	at := t.capWarnAt[slot]
	if !at.IsZero() && now.Sub(at) < persistFailCapWarnEvery {
		return capWarning{}
	}
	t.capWarnAt[slot] = now
	return capWarning{warn: true, slot: slot, entries: entries, runID: runID}
}
