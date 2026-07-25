package workersvc

// PRD #108 M5 — the guarded auto-stop.
//
// THE TESTS ARE MOSTLY NEGATIVE AND THAT IS THE MILESTONE'S CENTRE OF GRAVITY. The
// failure modes here are worse than the bug: killing healthy runs during an outage
// turns an outage into data loss. Every guard therefore gets a case that proves it
// DECLINES, not only one that proves it fires.
//
// None of this needs a live database. The guards are pure functions of in-process
// state plus one GetRunByID, Service.now is injectable, and Store is an interface —
// so the "multi-run fixture" the PRD calls its fiddliest piece is two map entries.
// The live-DB half is exactly two things (the migration's CHECK and FailRunAutoStop's
// SQL) and lives in internal/store's *LiveDB suite.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// autoStopFakeStore is the whole store surface the evaluator touches. It embeds
// Store so anything else panics rather than quietly returning a zero value.
type autoStopFakeStore struct {
	Store
	runs      map[uuid.UUID]store.Run
	workers   map[uuid.UUID]store.Worker
	getErr    error
	failRows  int64
	failErr   error
	failCalls []store.FailRunAutoStopParams
	verdicts  []store.CreateStopVerdictInputParams
	verdictEr error
	// runReads counts GetRunByID. The evaluator makes exactly one; a SECOND on the
	// same pass is maybeEnqueueJudgeByID's reload, which is how the terminal funnel
	// is measured rather than assumed.
	runReads int
}

func (f *autoStopFakeStore) GetRunByID(_ context.Context, id uuid.UUID) (store.Run, error) {
	if f.getErr != nil {
		return store.Run{}, f.getErr
	}
	f.runReads++
	r, ok := f.runs[id]
	if !ok {
		return store.Run{}, errors.New("no such run")
	}
	return r, nil
}

func (f *autoStopFakeStore) GetWorkerByID(_ context.Context, id uuid.UUID) (store.Worker, error) {
	w, ok := f.workers[id]
	if !ok {
		// pgx.ErrNoRows, not a generic error: hasLivePoller maps exactly that to
		// "no live poller" and propagates anything else as a failure. A fake that
		// returned a plain error here would send every no-poller test down the
		// error arm and they would pass for the wrong reason.
		return store.Worker{}, pgx.ErrNoRows
	}
	return w, nil
}

func (f *autoStopFakeStore) FailRunAutoStop(_ context.Context, arg store.FailRunAutoStopParams) (int64, error) {
	f.failCalls = append(f.failCalls, arg)
	if f.failErr != nil {
		return 0, f.failErr
	}
	return f.failRows, nil
}

func (f *autoStopFakeStore) CreateStopVerdictInput(_ context.Context, arg store.CreateStopVerdictInputParams) (store.RunUserInput, error) {
	f.verdicts = append(f.verdicts, arg)
	if f.verdictEr != nil {
		return store.RunUserInput{}, f.verdictEr
	}
	return store.RunUserInput{}, nil
}

// autoStopFixture stages one wedged run plus, by default, one healthy neighbour —
// the minimum shape in which a kill is allowed at all.
type autoStopFixture struct {
	svc     *Service
	fs      *autoStopFakeStore
	wedged  uuid.UUID
	peer    uuid.UUID
	nowFunc *time.Time
}

// newAutoStopFixture builds a run with a LIVE worker by default (fresh heartbeat).
func newAutoStopFixture(t *testing.T) *autoStopFixture {
	t.Helper()
	wedged, peer, workerID := uuid.New(), uuid.New(), uuid.New()
	fs := &autoStopFakeStore{
		runs: map[uuid.UUID]store.Run{
			wedged: {ID: wedged, Kind: "issue", Status: "running", WorkerID: pgUUID(workerID)},
		},
		workers:  map[uuid.UUID]store.Worker{workerID: {ID: workerID, LastHeartbeatAt: pgTime(t0)}},
		failRows: 1,
	}
	p := testParams()
	p.AutoStopEnabled = true
	svc := New(fs, nil, p)
	clk := t0
	svc.now = func() time.Time { return clk }
	f := &autoStopFixture{svc: svc, fs: fs, wedged: wedged, peer: peer, nowFunc: &clk}

	// A wedge that clears G1 (streak) and G2 (window) with a little margin.
	start := t0.Add(-(autoStopWindow + 5*time.Second))
	for i := 0; i < autoStopStreak; i++ {
		svc.persistFail.recordFailure(wedged, persistFailUnstorable, 273, start.Add(time.Duration(i)*500*time.Millisecond))
	}
	// The comparison set: one OTHER run that actually persisted messages recently.
	svc.persistFail.recordSuccess(peer, t0.Add(-time.Second))
	return f
}

func (f *autoStopFixture) sweep(t *testing.T) int64 {
	t.Helper()
	return f.svc.autoStopWedgedRuns(context.Background(), *f.nowFunc)
}

// advanceWithHealthyPeer moves the clock and refreshes the neighbour's last success,
// which is what a genuinely healthy neighbour does — it keeps appending. Without the
// refresh the comparison set ages out of the 60s window and every multi-tick test
// would silently start measuring G4 instead of what it meant to measure.
func (f *autoStopFixture) advanceWithHealthyPeer(d time.Duration) {
	*f.nowFunc = t0.Add(d)
	f.svc.persistFail.recordSuccess(f.peer, f.nowFunc.Add(-time.Second))
}

// -------------------------------------------------------------------------
// The positive case, first — so every negative below has something to negate
// -------------------------------------------------------------------------

func TestAutoStopSingleFailingRunBesideHealthyNeighboursIsStopped(t *testing.T) {
	f := newAutoStopFixture(t)
	if n := f.sweep(t); n != 1 {
		t.Fatalf("stopped = %d, want 1", n)
	}
	if len(f.fs.verdicts) != 1 {
		t.Fatalf("stop verdicts enqueued = %d, want 1 (the run has a live poller)", len(f.fs.verdicts))
	}
	v := f.fs.verdicts[0]
	if v.Kind != "cancel" {
		t.Fatalf("verdict kind = %q, want \"cancel\": every worker version routes cancel, while an UNKNOWN kind is logged and dropped by SteeringChannel.route's default arm — and /inputs is consume-on-read, so that drop is permanent and unacknowledgeable", v.Kind)
	}
	if !v.StopKind.Valid || v.StopKind.String != stopKindAutoStopped {
		t.Fatalf("verdict stop_kind = %v, want %q: it is the ONLY field that survives both halves of this stop", v.StopKind, stopKindAutoStopped)
	}
	if v.RunID != f.wedged {
		t.Fatalf("verdict ran against %s, want the wedged run %s", v.RunID, f.wedged)
	}
	if len(f.fs.failCalls) != 0 {
		t.Fatalf("FailRunAutoStop called %d times, want 0: a live worker must be asked to stop itself, not terminated under it", len(f.fs.failCalls))
	}
	if got := f.svc.persistFail.stats(f.wedged).stopReqAt; got.IsZero() {
		t.Fatal("stopReqAt was not stamped, so the escalation clock never starts and a worker that ignores the cancel rides to RUN_TIMEOUT")
	}
}

// -------------------------------------------------------------------------
// G4 — the comparison set. The safety argument, and the two PRD negatives
// -------------------------------------------------------------------------

func TestAutoStopApiWideOutageStopsNothing(t *testing.T) {
	// Every run's appends are failing, so nothing is in the comparison set. Killing
	// here turns an outage into data loss.
	f := newAutoStopFixture(t)
	f.svc.persistFail = newPersistFailTracker() // clear the staged peer success
	start := t0.Add(-(autoStopWindow + 5*time.Second))
	for _, id := range []uuid.UUID{f.wedged, f.peer} {
		f.fs.runs[id] = store.Run{ID: id, Kind: "issue", Status: "running"}
		for i := 0; i < autoStopStreak*2; i++ {
			f.svc.persistFail.recordFailure(id, persistFailStore, 10, start.Add(time.Duration(i)*500*time.Millisecond))
		}
	}
	if n := f.sweep(t); n != 0 {
		t.Fatalf("stopped = %d, want 0: when EVERY run is failing the fault is the api, and killing runs turns an outage into data loss", n)
	}
	if len(f.fs.failCalls)+len(f.fs.verdicts) != 0 {
		t.Fatalf("an api-wide outage produced %d fail + %d verdict writes, want 0", len(f.fs.failCalls), len(f.fs.verdicts))
	}
}

func TestAutoStopWithNoComparisonSetFlagsAndDoesNotKill(t *testing.T) {
	// The lonely-instance case — and THE INCIDENT'S OWN SHAPE. One active run means
	// no comparison set, so this degrades to flag-and-notify PERMANENTLY: there is no
	// fallback, no timeout into killing, no "if it has been zero long enough".
	//
	// Worth stating for whoever prunes tests later: this and the outage test above
	// exercise ONE predicate, not two. They differ in SETUP (which is where a fixture
	// bug would live), so neither proves the other, but a mutation to peersSucceeding
	// reddens both. Do not delete either as redundant.
	f := newAutoStopFixture(t)
	f.svc.persistFail.recordSuccess(f.peer, t0.Add(-time.Hour)) // long outside the window
	f.svc.persistFail.prune(t0)                                 // ...and now gone entirely

	for i := 0; i < 40; i++ { // many sweeps: it must never "eventually" kill
		*f.nowFunc = t0.Add(time.Duration(i) * 15 * time.Second)
		if n := f.sweep(t); n != 0 {
			t.Fatalf("sweep %d stopped %d runs, want 0 forever: a rule that cannot tell 'this run is poisoned' from 'the database is down' must not kill runs", i, n)
		}
	}
	if len(f.fs.failCalls)+len(f.fs.verdicts) != 0 {
		t.Fatal("a run with no comparison set was written to; it must be flagged and left alone")
	}
}

func TestAutoStopDoesNotCountTheWedgedRunsOwnPastSuccesses(t *testing.T) {
	// recordSuccess writes lastOK[R], so without the explicit `id == runID` skip a
	// lonely run would kill ITSELF on the strength of its own earlier successes —
	// the comparison set would be non-empty and contain only the accused.
	f := newAutoStopFixture(t)
	f.svc.persistFail = newPersistFailTracker()
	f.svc.persistFail.recordSuccess(f.wedged, t0.Add(-time.Second)) // its own success, recent
	start := t0.Add(-(autoStopWindow + 5*time.Second))
	for i := 0; i < autoStopStreak; i++ {
		f.svc.persistFail.recordFailure(f.wedged, persistFailUnstorable, 273, start.Add(time.Duration(i)*500*time.Millisecond))
	}
	if n := f.sweep(t); n != 0 {
		t.Fatalf("stopped = %d, want 0: the run under evaluation must never be its own comparison set", n)
	}
}

func TestAutoStopComparisonSetIsStrictlyRecent(t *testing.T) {
	// "Succeeding IN THE WINDOW", not "succeeded at some point". A neighbour that
	// last persisted 61 seconds ago proves nothing about the write path right now.
	f := newAutoStopFixture(t)
	f.svc.persistFail = newPersistFailTracker()
	f.svc.persistFail.recordSuccess(f.peer, t0.Add(-(autoStopWindow + time.Second)))
	start := t0.Add(-(autoStopWindow + 5*time.Second))
	for i := 0; i < autoStopStreak; i++ {
		f.svc.persistFail.recordFailure(f.wedged, persistFailUnstorable, 273, start.Add(time.Duration(i)*500*time.Millisecond))
	}
	if n := f.sweep(t); n != 0 {
		t.Fatalf("stopped = %d, want 0: a neighbour whose last success is outside the window is not evidence the write path works NOW", n)
	}
	// Positive control: pull that same success inside the window and it fires.
	f.svc.persistFail.recordSuccess(f.peer, t0.Add(-time.Second))
	if n := f.sweep(t); n != 1 {
		t.Fatalf("stopped = %d, want 1 once the neighbour's success is inside the window — without this control the assertion above could pass for any reason at all", n)
	}
}

func TestAutoStopComparisonSetAcceptsAChatNeighbour(t *testing.T) {
	// Deliberate: chat appends ride the same route and the same recorder, so a
	// healthy chat proves the write path works — which is the only question G4 asks.
	// The tracker is kind-blind by construction, so this pins the property rather
	// than a filter that would have to be maintained.
	f := newAutoStopFixture(t)
	f.fs.runs[f.peer] = store.Run{ID: f.peer, Kind: RunKindChat, Status: "running"}
	if n := f.sweep(t); n != 1 {
		t.Fatalf("stopped = %d, want 1: a succeeding CHAT neighbour is a valid comparison set", n)
	}
}

// -------------------------------------------------------------------------
// G1/G2 — the streak and window legs
// -------------------------------------------------------------------------

func TestAutoStopNeedsTheFullStreakAndWindow(t *testing.T) {
	cases := []struct {
		name   string
		streak int
		since  time.Duration
	}{
		{"one failure short of the threshold", autoStopStreak - 1, autoStopWindow + 5*time.Second},
		{"threshold reached but compressed into 5 seconds", autoStopStreak * 2, 5 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newAutoStopFixture(t)
			f.svc.persistFail.evict(f.wedged)
			start := t0.Add(-c.since)
			for i := 0; i < c.streak; i++ {
				f.svc.persistFail.recordFailure(f.wedged, persistFailUnstorable, 273, start.Add(time.Duration(i)*time.Millisecond))
			}
			if n := f.sweep(t); n != 0 {
				t.Fatalf("stopped = %d, want 0: %d failures over %v must not reach the kill (needs >= %d AND >= %v)",
					n, c.streak, c.since, autoStopStreak, autoStopWindow)
			}
		})
	}
}

func TestAutoStopOneSuccessResetsTheStreak(t *testing.T) {
	f := newAutoStopFixture(t)
	f.svc.persistFail.recordSuccess(f.wedged, t0) // the run recovered
	if n := f.sweep(t); n != 0 {
		t.Fatalf("stopped = %d, want 0: one success clears the streak, so the run is no longer a candidate at all", n)
	}
}

func TestAutoStopARotatingErrorNeverAccumulates(t *testing.T) {
	// G5. A rotating error class is an outage signature, so the streak resets on
	// every rotation and can never reach the threshold. Driven at 4x the threshold to
	// show the guard is structural, not a race with the count.
	f := newAutoStopFixture(t)
	f.svc.persistFail.evict(f.wedged)
	start := t0.Add(-(autoStopWindow + time.Minute))
	kinds := []persistFailKind{persistFailStore, persistFailUnstorable}
	for i := 0; i < autoStopStreak*4; i++ {
		f.svc.persistFail.recordFailure(f.wedged, kinds[i%2], 273, start.Add(time.Duration(i)*100*time.Millisecond))
	}
	if n := f.sweep(t); n != 0 {
		t.Fatalf("stopped = %d, want 0: an error that keeps CHANGING CLASS is an outage signature, and a streak that cannot accumulate across a rotation can never reach the kill", n)
	}
}

func TestAutoStopAProgressingRunNeverAccumulates(t *testing.T) {
	// G3. Every advance of runs.last_seq restarts the streak, so a run that is still
	// landing messages — a 0.10.1+ worker bisecting the poison out, for instance —
	// can never be killed. The server declines to stop a run whose client is
	// already handling it.
	f := newAutoStopFixture(t)
	f.svc.persistFail.evict(f.wedged)
	start := t0.Add(-(autoStopWindow + time.Minute))
	for i := 0; i < autoStopStreak*4; i++ {
		f.svc.persistFail.recordFailure(f.wedged, persistFailUnstorable, int32(100+i), start.Add(time.Duration(i)*100*time.Millisecond))
	}
	if n := f.sweep(t); n != 0 {
		t.Fatalf("stopped = %d, want 0: last_seq advancing is PROGRESS, and progress must restart the streak however many failures accompany it", n)
	}
}

// -------------------------------------------------------------------------
// G0 / G6 — the kill switch and the terminal guard
// -------------------------------------------------------------------------

func TestAutoStopKillSwitchDisarmsTheKillButNotTheFlag(t *testing.T) {
	// UZI_AUTOSTOP_ENABLED=false. The PRD's "ship M4 (flag only), hold M5" fallback
	// expressed as configuration rather than a revert, so the assertion is TWO-sided:
	// nothing is killed, AND the run is still flagged.
	f := newAutoStopFixture(t)
	f.svc.p.AutoStopEnabled = false
	if n := f.sweep(t); n != 0 {
		t.Fatalf("stopped = %d, want 0 while UZI_AUTOSTOP_ENABLED is false", n)
	}
	if len(f.fs.failCalls)+len(f.fs.verdicts) != 0 {
		t.Fatal("the kill switch is off but the evaluator still wrote")
	}
	got := f.svc.persistFail.stats(f.wedged)
	if got.streak < persistFlagStreak || t0.Sub(got.firstAt) < persistFlagWindow {
		t.Fatalf("streak=%d firstAt=%v: the kill switch must not stop the COUNTER, or the M4 flag goes dark with it — the whole point of a separate switch is that the flag survives",
			got.streak, got.firstAt)
	}
}

func TestAutoStopKillSwitchDefaultsOffInABareParamsLiteral(t *testing.T) {
	// The zero value of a bool is false, so a Params literal that forgets the field
	// has auto-stop OFF. That is the fail-safe direction and it is why the `true`
	// default lives in config.go where the env is read. Pinned so nobody "helpfully"
	// flips the sense of the field.
	f := newAutoStopFixture(t)
	f.svc.p = testParams() // no AutoStopEnabled
	if n := f.sweep(t); n != 0 {
		t.Fatalf("stopped = %d, want 0: an unset Params.AutoStopEnabled must mean OFF", n)
	}
}

func TestAutoStopSkipsAndEvictsATerminalRun(t *testing.T) {
	f := newAutoStopFixture(t)
	r := f.fs.runs[f.wedged]
	r.Status = "completed"
	f.fs.runs[f.wedged] = r

	if n := f.sweep(t); n != 0 {
		t.Fatalf("stopped = %d, want 0 for an already-terminal run", n)
	}
	if got := f.svc.persistFail.stats(f.wedged); got.streak != 0 {
		t.Fatalf("streak = %d, want 0: the evaluator must evict a run it observed terminal", got.streak)
	}
}

func TestAutoStopStatusScopedWriteThatMatchesNothingIsNotCounted(t *testing.T) {
	// The race between the evaluator's read and its write. FailRunAutoStop returns 0
	// rows when the run reached terminal in between; that is not a stop, must not be
	// counted, must not broadcast, and must evict.
	f := newAutoStopFixture(t)
	f.fs.workers = map[uuid.UUID]store.Worker{} // no live poller → the server-side half
	f.fs.failRows = 0

	if n := f.sweep(t); n != 0 {
		t.Fatalf("stopped = %d, want 0 when the status-scoped UPDATE matched no rows", n)
	}
	if len(f.fs.failCalls) != 1 {
		t.Fatalf("FailRunAutoStop calls = %d, want 1 (it must be ATTEMPTED — the SQL is the guard)", len(f.fs.failCalls))
	}
	if got := f.svc.persistFail.stats(f.wedged); got.streak != 0 {
		t.Fatalf("streak = %d, want 0: a no-op write still means the run is gone", got.streak)
	}
}

// -------------------------------------------------------------------------
// The two halves, and the escalation
// -------------------------------------------------------------------------

func TestAutoStopWithNoLivePollerTakesTheServerSideTransition(t *testing.T) {
	// Nobody will ever consume a verdict, so enqueuing one would strand the run until
	// RUN_TIMEOUT. hasLivePoller is the same discriminator a HUMAN cancel uses.
	f := newAutoStopFixture(t)
	f.fs.workers = map[uuid.UUID]store.Worker{} // the worker row is gone

	if n := f.sweep(t); n != 1 {
		t.Fatalf("stopped = %d, want 1", n)
	}
	if len(f.fs.verdicts) != 0 {
		t.Fatalf("stop verdicts = %d, want 0 with no live poller", len(f.fs.verdicts))
	}
	if len(f.fs.failCalls) != 1 {
		t.Fatalf("FailRunAutoStop calls = %d, want 1", len(f.fs.failCalls))
	}
	if got := f.fs.failCalls[0]; got.ID != f.wedged || !got.FailureReason.Valid {
		t.Fatalf("FailRunAutoStop params = %+v, want the wedged run with a failure reason", got)
	}
	// The side effects every other server-side failed path performs. Omit them and
	// the run rots in the wrong board column with no judge — a silent regression the
	// status write itself would never reveal.
	if f.fs.runReads != 2 {
		t.Fatalf("GetRunByID calls = %d, want 2: one is the evaluator's guard read, the second is maybeEnqueueJudgeByID's reload (PRD #46 Decision 2 — a committed-terminal run is judged)", f.fs.runReads)
	}
}

func TestAutoStopServerSideTransitionPublishesAndNotifies(t *testing.T) {
	// publishSwept feeds BOTH the live WS hub and the board's column automation. The
	// second is the one that bites silently: `failed` restores the origin column, and
	// without the notify the run's card is stranded in In Progress forever.
	f := newAutoStopFixture(t)
	f.fs.workers = map[uuid.UUID]store.Worker{}
	b := &autoStopBroadcaster{}
	lc := &autoStopLifecycle{}
	f.svc.SetBroadcaster(b)
	f.svc.SetLifecycle(lc)

	if n := f.sweep(t); n != 1 {
		t.Fatalf("stopped = %d, want 1", n)
	}
	if len(b.states) != 1 || b.states[0].status != "failed" || b.states[0].id != f.wedged {
		t.Fatalf("PublishState calls = %+v, want one 'failed' for the wedged run", b.states)
	}
	if len(lc.notes) != 1 || lc.notes[0].status != "failed" {
		t.Fatalf("lifecycle Notify calls = %+v, want one 'failed' — without it the board card never leaves In Progress", lc.notes)
	}
}

func TestAutoStopVerdictHalfDoesNotPublishTerminalState(t *testing.T) {
	// The mirror of the test above. On the live half the run is still RUNNING — the
	// worker will report its own terminal state — so announcing 'failed' here would
	// move the board card and redden the UI for a run that has not stopped yet.
	f := newAutoStopFixture(t)
	b := &autoStopBroadcaster{}
	lc := &autoStopLifecycle{}
	f.svc.SetBroadcaster(b)
	f.svc.SetLifecycle(lc)

	if n := f.sweep(t); n != 1 {
		t.Fatalf("stopped = %d, want 1", n)
	}
	if len(b.states)+len(lc.notes) != 0 {
		t.Fatalf("the verdict half published %d states and %d notifies, want 0/0: the run is still running until the worker says otherwise", len(b.states), len(lc.notes))
	}
}

type autoStopEvent struct {
	id     uuid.UUID
	status string
}

type autoStopBroadcaster struct{ states []autoStopEvent }

func (b *autoStopBroadcaster) PublishMessage(uuid.UUID, int32, string, string, string, string, []byte, time.Time) {
}
func (b *autoStopBroadcaster) PublishState(id uuid.UUID, status string) {
	b.states = append(b.states, autoStopEvent{id, status})
}
func (b *autoStopBroadcaster) PublishHealth(uuid.UUID, string, string, bool) {}
func (b *autoStopBroadcaster) PublishInput(uuid.UUID)                        {}

type autoStopLifecycle struct{ notes []autoStopEvent }

func (l *autoStopLifecycle) Notify(id uuid.UUID, status string) {
	l.notes = append(l.notes, autoStopEvent{id, status})
}

func TestAutoStopTreatsAStaleHeartbeatAsNoLivePoller(t *testing.T) {
	// hasLivePoller is heartbeat-based, so a worker whose process died mid-wedge
	// takes the server-side half rather than being asked to stop itself.
	f := newAutoStopFixture(t)
	for id, w := range f.fs.workers {
		w.LastHeartbeatAt = pgTime(t0.Add(-2 * testParams().WorkerHeartbeatStale))
		f.fs.workers[id] = w
	}
	if n := f.sweep(t); n != 1 {
		t.Fatalf("stopped = %d, want 1", n)
	}
	if len(f.fs.failCalls) != 1 || len(f.fs.verdicts) != 0 {
		t.Fatalf("fail=%d verdict=%d, want 1/0: a stale heartbeat means nobody is polling", len(f.fs.failCalls), len(f.fs.verdicts))
	}
}

func TestAutoStopEscalatesWhenTheWorkerIgnoresTheCancel(t *testing.T) {
	// There is NO acknowledgement channel for a steering input — route's cancel arm
	// reports nothing back and /inputs is consume-on-read — so a worker that
	// heartbeats while its steering loop is wedged would otherwise ride to
	// RUN_TIMEOUT (2h) against this PRD's ~2 minute goal.
	f := newAutoStopFixture(t)
	if n := f.sweep(t); n != 1 {
		t.Fatalf("first sweep: stopped = %d, want 1", n)
	}

	// A tick later, still inside the escalation window: no second action.
	f.advanceWithHealthyPeer(autoStopEscalateAfter - time.Second)
	if n := f.sweep(t); n != 0 {
		t.Fatalf("stopped = %d one second BEFORE the escalation is due, want 0 — the worker is still being given time to honour the cancel", n)
	}
	if len(f.fs.failCalls) != 0 {
		t.Fatalf("escalated early: %d FailRunAutoStop calls", len(f.fs.failCalls))
	}

	// Past it: escalate to the server-side transition.
	f.advanceWithHealthyPeer(autoStopEscalateAfter + time.Second)
	if n := f.sweep(t); n != 1 {
		t.Fatalf("stopped = %d after the escalation window, want 1 — without this the live half has NO completion guarantee at all", n)
	}
	if len(f.fs.failCalls) != 1 {
		t.Fatalf("FailRunAutoStop calls = %d, want 1", len(f.fs.failCalls))
	}
	if len(f.fs.verdicts) != 1 {
		t.Fatalf("stop verdicts = %d, want 1: the escalation must not enqueue a SECOND cancel", len(f.fs.verdicts))
	}
}

func TestAutoStopDoesNotReEnqueueAVerdictEveryTick(t *testing.T) {
	// Without stopReqAt the evaluator would enqueue one cancel per 15s sweep for as
	// long as the run lived — an unbounded write amplification on run_user_inputs.
	f := newAutoStopFixture(t)
	for i := 0; i < 4; i++ {
		f.advanceWithHealthyPeer(time.Duration(i) * 15 * time.Second)
		f.sweep(t)
	}
	if len(f.fs.verdicts) != 1 {
		t.Fatalf("stop verdicts = %d over four sweeps, want exactly 1", len(f.fs.verdicts))
	}
}

func TestAutoStopStoreFailuresNeverFailTheSweep(t *testing.T) {
	// Best-effort, in the sweep's own style: a health hiccup must not fail the sweep,
	// and neither may this. Each arm degrades to "did nothing this tick".
	t.Run("re-read fails", func(t *testing.T) {
		f := newAutoStopFixture(t)
		f.fs.getErr = errors.New("boom")
		if n := f.sweep(t); n != 0 {
			t.Fatalf("stopped = %d, want 0", n)
		}
	})
	t.Run("verdict insert fails", func(t *testing.T) {
		f := newAutoStopFixture(t)
		f.fs.verdictEr = errors.New("boom")
		if n := f.sweep(t); n != 0 {
			t.Fatalf("stopped = %d, want 0", n)
		}
		if got := f.svc.persistFail.stats(f.wedged).stopReqAt; !got.IsZero() {
			t.Fatal("stopReqAt was stamped despite the insert failing, so the retry is lost and the escalation clock started for a verdict that never existed")
		}
	})
	t.Run("terminal write fails", func(t *testing.T) {
		f := newAutoStopFixture(t)
		f.fs.workers = map[uuid.UUID]store.Worker{}
		f.fs.failErr = errors.New("boom")
		if n := f.sweep(t); n != 0 {
			t.Fatalf("stopped = %d, want 0", n)
		}
		if got := f.svc.persistFail.stats(f.wedged); got.streak == 0 {
			t.Fatal("the entry was evicted despite the write failing, so the next tick would not retry")
		}
	})
}

func TestSweepReportsAutoStopsAndRunsAfterTheDetector(t *testing.T) {
	// Wiring, not logic: the evaluator is reached from Sweep and its count lands in
	// SweepResult. Also pins the ORDER — health first, kill second — by asserting the
	// detector ran on the same pass.
	f := newAutoStopFixture(t)
	fs := &autoStopSweepStore{autoStopFakeStore: f.fs}
	f.svc.q = fs
	f.svc.healthSettings = defaultHealthSettings()
	fs.active = []store.ListActiveRunsForHealthRow{
		{ID: f.wedged, UserID: uuid.New(), Status: "running", Health: healthOK, StartedAt: pgTime(t0.Add(-time.Hour))},
	}

	res, err := f.svc.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.AutoStopped != 1 {
		t.Fatalf("SweepResult.AutoStopped = %d, want 1", res.AutoStopped)
	}
	if len(fs.healthWrites) == 0 {
		t.Fatal("the health detector did not run in the same pass; 'health first, kill second' cannot be observed")
	}
	if w := fs.healthWrites[0]; w.Health != healthLooping || w.HealthReason.String != reasonPersistFailing {
		t.Fatalf("health write = %q/%q, want looping/%q: the flag must land on the same pass that kills, not after it",
			w.Health, w.HealthReason.String, reasonPersistFailing)
	}
}

// autoStopSweepStore adds the whole-Sweep surface on top of the evaluator's.
type autoStopSweepStore struct {
	*autoStopFakeStore
	active       []store.ListActiveRunsForHealthRow
	healthWrites []store.SetRunHealthParams
}

func (f *autoStopSweepStore) MarkStaleWorkersOffline(context.Context, pgtype.Timestamptz) (int64, error) {
	return 0, nil
}
func (f *autoStopSweepStore) SweepClaimedNeverStarted(context.Context, pgtype.Timestamptz) ([]store.SweepClaimedNeverStartedRow, error) {
	return nil, nil
}
func (f *autoStopSweepStore) SweepRunningTimeout(context.Context, store.SweepRunningTimeoutParams) ([]store.SweepRunningTimeoutRow, error) {
	return nil, nil
}
func (f *autoStopSweepStore) FailRunsOfStaleWorkersOverCap(context.Context, store.FailRunsOfStaleWorkersOverCapParams) ([]store.FailRunsOfStaleWorkersOverCapRow, error) {
	return nil, nil
}
func (f *autoStopSweepStore) RequeueRunsOfStaleWorkers(context.Context, store.RequeueRunsOfStaleWorkersParams) ([]store.RequeueRunsOfStaleWorkersRow, error) {
	return nil, nil
}
func (f *autoStopSweepStore) SweepIdleChatRuns(context.Context, pgtype.Timestamptz) ([]store.SweepIdleChatRunsRow, error) {
	return nil, nil
}
func (f *autoStopSweepStore) SweepStuckConfirmingProposals(context.Context, pgtype.Timestamptz) ([]uuid.UUID, error) {
	return nil, nil
}
func (f *autoStopSweepStore) ListActiveRunsForHealth(context.Context) ([]store.ListActiveRunsForHealthRow, error) {
	return f.active, nil
}
func (f *autoStopSweepStore) ListRunToolWindow(context.Context, store.ListRunToolWindowParams) ([]store.ListRunToolWindowRow, error) {
	return nil, nil
}
func (f *autoStopSweepStore) SetRunHealth(_ context.Context, arg store.SetRunHealthParams) (int64, error) {
	f.healthWrites = append(f.healthWrites, arg)
	return 1, nil
}
