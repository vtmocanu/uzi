package workersvc

// PRD #108 M4 — the persistence-failure tracker and the AppendMessages recorder.
//
// Every guard M5 will evaluate is a pure function of this in-process state plus one
// run row, so the "multi-run fixture" the PRD calls its fiddliest piece is TWO MAP
// ENTRIES here, not two rows in Postgres. That is deliberate and it is why these
// tests need no container and no database.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// -------------------------------------------------------------------------
// The tracker in isolation
// -------------------------------------------------------------------------

const testSeq = int32(273) // the incident's own last persisted seq

func TestPersistFailStreakAccumulatesOnIdenticalFailures(t *testing.T) {
	tr := newPersistFailTracker()
	id := uuid.New()
	for i := 0; i < 20; i++ {
		tr.recordFailure(id, persistFailUnstorable, testSeq, t0.Add(time.Duration(i)*time.Second))
	}
	got := tr.stats(id)
	if got.streak != 20 {
		t.Fatalf("streak = %d, want 20 after 20 identical failures", got.streak)
	}
	if !got.firstAt.Equal(t0) {
		t.Fatalf("firstAt = %v, want the FIRST failure %v — the sustained-duration window must measure the whole episode", got.firstAt, t0)
	}
	if got.kind != persistFailUnstorable {
		t.Fatalf("kind = %v, want unstorable", got.kind)
	}
}

func TestPersistFailClassChangeResetsTheStreak(t *testing.T) {
	// The PRD's "same error class each time" guard, implemented as a reset rather
	// than a predicate: a rotating error is an outage signature, and a streak that
	// cannot accumulate across a rotation can never reach the kill.
	tr := newPersistFailTracker()
	id := uuid.New()
	for i := 0; i < 19; i++ {
		tr.recordFailure(id, persistFailStore, testSeq, t0.Add(time.Duration(i)*time.Second))
	}
	rotated := t0.Add(19 * time.Second)
	tr.recordFailure(id, persistFailUnstorable, testSeq, rotated)

	got := tr.stats(id)
	if got.streak != 1 {
		t.Fatalf("streak = %d, want 1: a CLASS change (store → unstorable) must restart the streak, not extend it to 20", got.streak)
	}
	if !got.firstAt.Equal(rotated) {
		t.Fatalf("firstAt = %v, want the rotation instant %v: the duration window must restart with the streak, or G1 and G2 would describe different episodes", got.firstAt, rotated)
	}
}

func TestPersistFailSeqAdvanceResetsTheStreak(t *testing.T) {
	// The PRD's "max(seq) has not advanced" guard. Reaching the threshold IS the
	// proof of no progress, because any advance restarts the count.
	tr := newPersistFailTracker()
	id := uuid.New()
	for i := 0; i < 19; i++ {
		tr.recordFailure(id, persistFailUnstorable, 100, t0.Add(time.Duration(i)*time.Second))
	}
	tr.recordFailure(id, persistFailUnstorable, 101, t0.Add(19*time.Second))

	got := tr.stats(id)
	if got.streak != 1 {
		t.Fatalf("streak = %d, want 1: last_seq moving 100 → 101 is PROGRESS and must restart the streak", got.streak)
	}
	if got.lastSeq != 101 {
		t.Fatalf("lastSeq = %d, want 101: the reset must adopt the new high-water mark, or the next failure resets again forever", got.lastSeq)
	}
}

func TestPersistFailSuccessClearsStreakAndJoinsComparisonSet(t *testing.T) {
	tr := newPersistFailTracker()
	id := uuid.New()
	for i := 0; i < 10; i++ {
		tr.recordFailure(id, persistFailUnstorable, testSeq, t0)
	}
	tr.recordSuccess(id, t0.Add(time.Minute))

	if got := tr.stats(id); got.streak != 0 {
		t.Fatalf("streak = %d, want 0: one success clears the streak", got.streak)
	}
	tr.mu.Lock()
	at, ok := tr.lastOK[id]
	tr.mu.Unlock()
	if !ok || !at.Equal(t0.Add(time.Minute)) {
		t.Fatalf("lastOK[run] = (%v, %v), want the success instant: this map IS M5's comparison set, and only recordSuccess may write it", at, ok)
	}
}

func TestPersistFailEvictDropsBothMaps(t *testing.T) {
	// evict runs when a run is observed TERMINAL, and it must leave the comparison
	// set as well as the streak.
	//
	// An earlier version kept lastOK, reasoning that a successful append proves the
	// write path works whatever the run's status. True but insufficient: G4 is a
	// GLOBAL "other runs are succeeding", so a worker holding one terminal run and
	// re-POSTing a deduplicated append every few minutes — near-zero cost, no tokens,
	// no run slot — could keep the kill armed for every OTHER user's run on the
	// instance. It kills nothing on its own (the victim still has to satisfy four
	// other guards), but it hollows out the one guard that is supposed to be an
	// INDEPENDENT observation that the world is healthy.
	tr := newPersistFailTracker()
	id := uuid.New()
	tr.recordSuccess(id, t0)
	tr.recordFailure(id, persistFailUnstorable, testSeq, t0.Add(time.Second))
	tr.evict(id)

	if got := tr.stats(id); got.streak != 0 {
		t.Fatalf("streak = %d, want 0 after evict", got.streak)
	}
	tr.mu.Lock()
	_, ok := tr.lastOK[id]
	tr.mu.Unlock()
	if ok {
		t.Fatal("evict left the run in the comparison set: a terminal run must not be able to vouch for the write path, or warming G4 costs one deduplicated append instead of a live run doing real work")
	}
}

func TestAppendMessagesDropsASubThresholdStreakWhenTheRunLeavesRunning(t *testing.T) {
	// The gap the two Sweep hooks and the evaluator all miss. The evaluator only ever
	// sees CANDIDATES (streak >= autoStopStreak and the window elapsed), so a streak
	// below the threshold crossed a requeue untouched — and kept GROWING, because
	// this method went on recording against a queued run.
	//
	// Measured before the fix: 12 carried across a Register-path requeue, then the
	// fresh attempt killed after 8 new failures, with the whole window leg satisfied
	// by the DEAD attempt's firstAt. A streak has to pass through 12 to reach 20, so
	// which side of the threshold a worker OOM lands on is close to a coin flip;
	// half that population landed here.
	//
	// Register's RequeueWorkerRuns returns no ids, so it can never have a Sweep-style
	// hook. Closing it at the RECORDER retires the class instead of adding a fourth
	// path-specific patch — and costs nothing, since appendMessages already read the
	// run row.
	w := worker()
	runID := uuid.New()
	fs := &persistFakeStore{
		run:       store.Run{ID: runID, WorkerID: pgUUID(w.ID), Status: "running"},
		poisonSeq: 1,
		insertErr: unstorableErr(),
	}
	clk := t0
	svc := persistSvc(fs, &clk)

	const subThreshold = 12
	if subThreshold >= autoStopStreak {
		t.Fatalf("fixture bug: %d is not below autoStopStreak (%d), so this test is not exercising the sub-threshold path at all", subThreshold, autoStopStreak)
	}
	for i := 0; i < subThreshold; i++ {
		clk = t0.Add(time.Duration(i) * 500 * time.Millisecond)
		_ = svc.AppendMessages(context.Background(), w, runID, []IncomingMessage{msg(274)})
	}
	if got := svc.persistFail.stats(runID).streak; got != subThreshold {
		t.Fatalf("precondition: streak = %d, want %d", got, subThreshold)
	}

	// The requeue. No hook fires for this path; the next POST is what must clear it.
	fs.run.Status = "queued"
	_ = svc.AppendMessages(context.Background(), w, runID, []IncomingMessage{msg(274)})

	if got := svc.persistFail.stats(runID).streak; got != 0 {
		t.Fatalf("streak = %d, want 0: a sub-threshold streak survived the run leaving `running`, so the fresh attempt starts %d failures into a 20-failure budget it never spent — and the window leg is already satisfied by the dead attempt", got, got)
	}
}

func TestAppendMessagesComparisonSetIsRunningRunsOnly(t *testing.T) {
	// The recorder's non-running arm has a SECOND effect beyond dropping streaks:
	// sitting above the success arm, it means recordSuccess fires only for running
	// runs, so it also narrows G4's comparison set. A run parked at the approval gate
	// that IS successfully persisting messages no longer vouches for the write path.
	//
	// Intended on both counts — fail-safe (fewer peers ⇒ fewer kills), and it keeps
	// warming the comparison set costing a live run doing real work. Pinned here
	// because the effect is a CONSEQUENCE of arm ordering rather than of anything
	// that names G4, so it is invisible at the site someone would edit: restoring
	// recordSuccess for a parked run looks like a fix for an oversight.
	for _, tc := range []struct {
		status string
		peer   bool
	}{
		{"running", true},
		{"awaiting_approval", false},
		{"claimed", false},
		{"queued", false},
		{"completed", false},
	} {
		t.Run(tc.status, func(t *testing.T) {
			w := worker()
			runID, accused := uuid.New(), uuid.New()
			fs := &persistFakeStore{run: store.Run{ID: runID, WorkerID: pgUUID(w.ID), Status: tc.status}}
			clk := t0
			svc := persistSvc(fs, &clk)

			if err := svc.AppendMessages(context.Background(), w, runID, []IncomingMessage{msg(1)}); err != nil {
				t.Fatalf("AppendMessages: %v", err)
			}
			peers := svc.persistFail.peersSucceeding(accused, t0, autoStopWindow)
			if got := peers > 0; got != tc.peer {
				t.Fatalf("a SUCCESSFUL append on a %s run counts as a peer = %v, want %v — G4 asks whether the write path works, and only a RUNNING run answers it for this design's purposes",
					tc.status, got, tc.peer)
			}
		})
	}
}

func TestAppendMessagesTerminalRunNeverJoinsTheComparisonSet(t *testing.T) {
	// The same property at the recorder, which is where the attack would actually be
	// mounted: a SUCCESSFUL append on a terminal run must not write lastOK. The
	// terminal arm therefore has to be evaluated BEFORE the success arm.
	w := worker()
	runID := uuid.New()
	fs := &persistFakeStore{run: store.Run{ID: runID, WorkerID: pgUUID(w.ID), Status: "completed"}}
	clk := t0
	svc := persistSvc(fs, &clk)

	if err := svc.AppendMessages(context.Background(), w, runID, []IncomingMessage{msg(1)}); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	svc.persistFail.mu.Lock()
	_, ok := svc.persistFail.lastOK[runID]
	svc.persistFail.mu.Unlock()
	if ok {
		t.Fatal("a successful append to a TERMINAL run joined M5's comparison set; a worker can then keep the kill armed for every other user's run for the cost of one deduplicated POST every few minutes")
	}
}

func TestPersistFailPruneDropsOnlyStaleEntries(t *testing.T) {
	// The TTL is the memory bound for the one case no other eviction path reaches:
	// a run whose worker vanished without the run ever reaching terminal.
	// Four distinct runs, because recordSuccess DELETES the fail entry — staging both
	// maps on one id would silently test only whichever call came last.
	tr := newPersistFailTracker()
	staleFail, freshFail := uuid.New(), uuid.New()
	staleOKRun, freshOKRun := uuid.New(), uuid.New()
	now := t0.Add(persistFailTTL + time.Second)

	tr.recordFailure(staleFail, persistFailUnstorable, testSeq, t0)
	tr.recordSuccess(staleOKRun, t0)
	tr.recordFailure(freshFail, persistFailUnstorable, testSeq, now)
	tr.recordSuccess(freshOKRun, now)

	tr.prune(now)

	if got := tr.stats(staleFail); got.streak != 0 {
		t.Fatalf("stale streak = %d, want 0: an entry untouched for longer than persistFailTTL (%v) must be pruned", got.streak, persistFailTTL)
	}
	if got := tr.stats(freshFail); got.streak != 1 {
		t.Fatalf("fresh streak = %d, want 1: prune must not touch an entry inside the TTL — it would silently disarm a live decision", got.streak)
	}
	tr.mu.Lock()
	_, staleOK := tr.lastOK[staleOKRun]
	_, freshOK := tr.lastOK[freshOKRun]
	tr.mu.Unlock()
	if staleOK {
		t.Fatal("prune must expire lastOK too, or the comparison set grows without bound")
	}
	if !freshOK {
		t.Fatal("prune expired a lastOK entry inside the TTL")
	}
}

func TestPersistFailCapRefusesNewEntriesAndKeepsCountingExistingOnes(t *testing.T) {
	// The cap is DEFENSE IN DEPTH, not the memory bound. This comment used to say the
	// keys were worker-supplied so an unknown run id would mint an unreachable entry;
	// that was false, and two validators found it independently. Only recordFailure
	// mints, and both production callers sit below the ownership gate, so an unknown
	// run id records nothing. The cap's residual threat model is one legitimate user
	// holding more than persistFailMaxEntries concurrently-failing OWNED runs.
	//
	// What it must still get right: at the cap a NEW entry is refused and existing
	// ones keep counting. Refusing to START a streak delays a kill (fail-safe), while
	// evicting to make room would hand a worker a way to CLEAR a genuine run's streak.
	tr := newPersistFailTracker()
	genuine := uuid.New()
	tr.recordFailure(genuine, persistFailUnstorable, testSeq, t0)
	for i := 0; i < persistFailMaxEntries+64; i++ {
		tr.recordFailure(uuid.New(), persistFailUnstorable, testSeq, t0)
	}

	tr.mu.Lock()
	n := len(tr.fail)
	tr.mu.Unlock()
	if n > persistFailMaxEntries {
		t.Fatalf("len(fail) = %d, want <= %d: a worker minting run ids must not grow this map without bound", n, persistFailMaxEntries)
	}
	tr.recordFailure(genuine, persistFailUnstorable, testSeq, t0.Add(time.Second))
	if got := tr.stats(genuine); got.streak != 2 {
		t.Fatalf("streak of the pre-existing run = %d, want 2: the cap must refuse NEW entries only, never stop counting a run already tracked", got.streak)
	}
}

func TestPersistFailCapWarningIsThrottledPerMapNotGlobally(t *testing.T) {
	// A single shared timestamp meant a lastOK cap event inside the same minute as a
	// fail one was suppressed and never appeared AT ALL — an operator could watch
	// `map=fail` indefinitely while lastOK was equally saturated. This is the
	// observability guard on a guard that disarms silently; it must not itself drop
	// events for a different map.
	tr := newPersistFailTracker()
	id := uuid.New()

	first := tr.noteAtCapLocked(persistMapFail, persistFailMaxEntries, id, t0)
	if !first.warn || first.slot != persistMapFail {
		t.Fatalf("first fail-map cap event = %+v, want a warning", first)
	}
	// Same map, same minute: throttled.
	if again := tr.noteAtCapLocked(persistMapFail, persistFailMaxEntries, id, t0.Add(time.Second)); again.warn {
		t.Fatal("a second fail-map cap event one second later warned again; the throttle is not applied")
	}
	// DIFFERENT map, same instant: must still warn.
	other := tr.noteAtCapLocked(persistMapLastOK, persistFailMaxEntries, id, t0.Add(time.Second))
	if !other.warn {
		t.Fatal("a lastOK cap event inside the fail map's throttle window was suppressed — that is the exact event an operator would never see")
	}
	if other.slot != persistMapLastOK || persistMapName(other.slot) != "lastOK" {
		t.Fatalf("cap event names map %q, want lastOK", persistMapName(other.slot))
	}
	// And the throttle still expires.
	if later := tr.noteAtCapLocked(persistMapFail, persistFailMaxEntries, id, t0.Add(persistFailCapWarnEvery+time.Second)); !later.warn {
		t.Fatal("the fail-map throttle never expires, so a persistent cap condition goes silent forever")
	}
	if first.entries != persistFailMaxEntries || first.runID != id {
		t.Fatalf("cap event = %+v, want the live entry count and the refused run id: a line that says only 'at capacity' is alarming rather than actionable", first)
	}
}

func TestPersistFailTrackerSerializesConcurrentWriters(t *testing.T) {
	// The lock is LOAD-BEARING, not decoration. AppendMessages runs on N parallel
	// chi handler goroutines while the sweeper reads the same maps, so this drives
	// exactly that shape.
	//
	// The assertion is an EXACT count, not "no panic". MEASURED by deleting
	// recordFailure's `t.mu.Lock()`: under `go test -race` it reddens every time
	// (WARNING: DATA RACE). Without -race it reddened 2 of 3 runs — once as a short
	// count (`streak = 64, want 1024`, a lost update) and once as the runtime's
	// "fatal error: concurrent map read and map write" — and passed the third.
	//
	// So: -race is what makes this deterministic, and the count assertion is what
	// makes the failure legible when it fires. Stated rather than claimed, because a
	// test that only "passes under -race" would be decoration and this one has to be
	// the thing standing between N handler goroutines and a corrupted kill counter.
	//
	// -race IS ENFORCED: `test:api` in .gitlab-ci.yml runs `go test -race ./...`.
	// It did not until PRD #108's review pointed out that the only -race job was
	// `-run 'LiveDB$'` over store/handler — wrong packages and filtered — so this
	// paragraph described a guarantee CI was not providing. If you drop that flag,
	// drop these three sentences with it.
	const writers, perWriter = 16, 64

	tr := newPersistFailTracker()
	id := uuid.New()
	stop := make(chan struct{})
	var readers sync.WaitGroup

	// Two concurrent READERS in the sweeper's two shapes, so the race detector sees
	// read/write pairs and not only write/write.
	readers.Add(2)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = tr.stats(id)
			}
		}
	}()
	go func() {
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
				tr.prune(t0) // nothing is stale at t0; this is here for the map walk
			}
		}
	}()

	var writersWG sync.WaitGroup
	writersWG.Add(writers)
	for w := 0; w < writers; w++ {
		go func() {
			defer writersWG.Done()
			for i := 0; i < perWriter; i++ {
				// Identical kind and seq on purpose: every call must therefore be an
				// increment, so the total is arithmetic rather than a race-dependent range.
				tr.recordFailure(id, persistFailUnstorable, testSeq, t0)
			}
		}()
	}
	writersWG.Wait()
	close(stop)
	readers.Wait()

	if got := tr.stats(id).streak; got != writers*perWriter {
		t.Fatalf("streak = %d, want exactly %d (%d goroutines × %d increments): a short count is a LOST UPDATE, which is what an unguarded map does under concurrency",
			got, writers*perWriter, writers, perWriter)
	}
}

func TestClassifyPersistFailMapsSentinelsNotSQLSTATEs(t *testing.T) {
	// Deliberately coarse: ErrUnstorableMessage already covers all four enumerated
	// SQLSTATEs, and a payload tripping two of them alternately is still the same
	// permanent-poison story. Splitting by SQLSTATE would make the same-class guard
	// MORE fragile (each rotation resets the streak), not more precise.
	cases := []struct {
		name string
		err  error
		want persistFailKind
	}{
		{"nil", nil, persistFailNone},
		{"unstorable 22P05", classifyStoreError(&pgconn.PgError{Code: "22P05"}), persistFailUnstorable},
		{"unstorable 22003", classifyStoreError(&pgconn.PgError{Code: "22003"}), persistFailUnstorable},
		{"invalid", ErrInvalidMessage, persistFailInvalid},
		{"wrapped invalid", fmt.Errorf("append: %w", ErrInvalidMessage), persistFailInvalid},
		{"transient pg", classifyStoreError(&pgconn.PgError{Code: "53300"}), persistFailStore},
		{"plain error", errors.New("connection reset"), persistFailStore},
	}
	for _, c := range cases {
		if got := classifyPersistFail(c.err); got != c.want {
			t.Errorf("%s: classifyPersistFail = %v, want %v", c.name, got, c.want)
		}
	}
	// Two DIFFERENT unstorable SQLSTATEs must land on the SAME class, or an
	// alternating poison would reset the streak forever and never be stopped.
	a := classifyPersistFail(classifyStoreError(&pgconn.PgError{Code: "22P05"}))
	b := classifyPersistFail(classifyStoreError(&pgconn.PgError{Code: "22021"}))
	if a != b {
		t.Fatalf("22P05 → %v and 22021 → %v must be the same class; splitting by SQLSTATE re-arms the rotation the guard exists to catch", a, b)
	}
}

// TestReasonPersistFailingIsMirroredBySlack pins the reason string that
// slacksvc/health.go mirrors as reasonPersistFailing.
//
// slacksvc deliberately holds NO workersvc import (stated twice in slacksvc/gate.go
// and gatekeeper.go), which is why the string is mirrored rather than shared — the
// same treatment the health ENUM values already get there. This assertion is the
// pin: reword the constant without updating the mirror and this test reddens,
// naming the file to update. A missed mirror degrades to the generic looping head
// rather than breaking, so the failure mode is a stale sentence, not an outage.
func TestReasonPersistFailingIsMirroredBySlack(t *testing.T) {
	const mirrored = "the agent's updates can't be saved, so it keeps resending them"
	if reasonPersistFailing != mirrored {
		t.Fatalf("reasonPersistFailing = %q, want %q.\nIf you meant to reword it, update the mirrored constant in internal/slacksvc/health.go in the SAME commit — otherwise the Slack nudge silently reverts to the tool-repetition wording, which is false for a persistence wedge.",
			reasonPersistFailing, mirrored)
	}
}

// -------------------------------------------------------------------------
// The AppendMessages recorder
// -------------------------------------------------------------------------

// persistFakeStore is the narrow store the recorder path touches. It embeds Store
// so any other query panics, and it models the two properties the partial-apply
// case depends on: InsertRunMessage is idempotent on seq, and UpdateRunLastSeq is
// GREATEST(last_seq, seq) — which is what makes runs.last_seq a faithful
// high-water mark for the no-progress guard.
type persistFakeStore struct {
	Store
	run    store.Run
	runErr error
	// poisonSeq, when non-zero, makes InsertRunMessage fail from that seq onward
	// with insertErr. Rows before it commit, exactly as the real non-transactional
	// loop leaves them.
	poisonSeq int32
	insertErr error
	stored    map[int32]bool
	inserts   int
}

func (f *persistFakeStore) GetRunOwnedByWorker(context.Context, store.GetRunOwnedByWorkerParams) (store.Run, error) {
	return f.run, f.runErr
}

func (f *persistFakeStore) InsertRunMessage(_ context.Context, arg store.InsertRunMessageParams) (int64, error) {
	if f.poisonSeq != 0 && arg.Seq >= f.poisonSeq {
		return 0, f.insertErr
	}
	f.inserts++
	if f.stored == nil {
		f.stored = map[int32]bool{}
	}
	if f.stored[arg.Seq] {
		return 0, nil // ON CONFLICT DO NOTHING — stored is stored
	}
	f.stored[arg.Seq] = true
	return 1, nil
}

func (f *persistFakeStore) UpdateRunLastSeq(_ context.Context, arg store.UpdateRunLastSeqParams) (int64, error) {
	if arg.Seq > f.run.LastSeq {
		f.run.LastSeq = arg.Seq
	}
	return 1, nil
}

func unstorableErr() error { return classifyStoreError(&pgconn.PgError{Code: "22P05"}) }

func msg(seq int32) IncomingMessage {
	return IncomingMessage{Seq: seq, Kind: "text", Payload: json.RawMessage(`{"t":"x"}`)}
}

// persistSvc builds a Service on a fixed clock. The clock is a pointer so a test
// can advance it between appends.
func persistSvc(fs Store, clk *time.Time) *Service {
	svc := New(fs, nil, testParams())
	svc.now = func() time.Time { return *clk }
	return svc
}

func TestAppendMessagesRecordsAStreakOnRepeatedUnstorableBatches(t *testing.T) {
	w := worker()
	runID := uuid.New()
	fs := &persistFakeStore{
		run:       store.Run{ID: runID, WorkerID: pgUUID(w.ID), Status: "running"},
		poisonSeq: 1,
		insertErr: unstorableErr(),
	}
	clk := t0
	svc := persistSvc(fs, &clk)

	for i := 0; i < 20; i++ {
		clk = t0.Add(time.Duration(i) * 500 * time.Millisecond) // ~2 Hz, the incident's rate
		if err := svc.AppendMessages(context.Background(), w, runID, []IncomingMessage{msg(274)}); !errors.Is(err, ErrUnstorableMessage) {
			t.Fatalf("attempt %d: err = %v, want ErrUnstorableMessage", i, err)
		}
	}
	got := svc.persistFail.stats(runID)
	if got.streak != 20 {
		t.Fatalf("streak = %d, want 20: the recorder must count EVERY failing return of AppendMessages, which is the signal the wedge cannot suppress", got.streak)
	}
	if got.kind != persistFailUnstorable {
		t.Fatalf("kind = %v, want unstorable", got.kind)
	}
}

func TestAppendMessagesRecordsNothingForARunTheWorkerDoesNotOwn(t *testing.T) {
	// The ownership tripwire. This counter drives a kill, so a worker POSTing to a
	// run it does not own must not drive THAT run's streak — the check that failed
	// IS the ownership check.
	w := worker()
	runID := uuid.New()
	fs := &persistFakeStore{runErr: pgx.ErrNoRows}
	clk := t0
	svc := persistSvc(fs, &clk)

	for i := 0; i < 50; i++ {
		if err := svc.AppendMessages(context.Background(), w, runID, []IncomingMessage{msg(1)}); !errors.Is(err, ErrRunNotOwned) {
			t.Fatalf("err = %v, want ErrRunNotOwned", err)
		}
	}
	if got := svc.persistFail.stats(runID); got.streak != 0 {
		t.Fatalf("streak = %d, want 0 after 50 unowned POSTs: recording here is a cross-tenant kill primitive", got.streak)
	}
}

func TestAppendMessagesEvictsInsteadOfCountingOnATerminalRun(t *testing.T) {
	// worker_id survives the terminal transition and neither GetRunOwnedByWorker nor
	// AppendMessages filters on status, so without this positive check a POST to a
	// dead run resurrects its entry one tick after the evaluator evicted it.
	w := worker()
	runID := uuid.New()
	fs := &persistFakeStore{
		run:       store.Run{ID: runID, WorkerID: pgUUID(w.ID), Status: "running"},
		poisonSeq: 1,
		insertErr: unstorableErr(),
	}
	clk := t0
	svc := persistSvc(fs, &clk)

	for i := 0; i < 10; i++ {
		_ = svc.AppendMessages(context.Background(), w, runID, []IncomingMessage{msg(274)})
	}
	if got := svc.persistFail.stats(runID); got.streak != 10 {
		t.Fatalf("precondition: streak = %d, want 10 while the run is running", got.streak)
	}

	fs.run.Status = "failed"
	_ = svc.AppendMessages(context.Background(), w, runID, []IncomingMessage{msg(274)})

	if got := svc.persistFail.stats(runID); got.streak != 0 {
		t.Fatalf("streak = %d, want 0: a failure recorded against a TERMINAL run keeps a dead run's kill streak alive", got.streak)
	}
}

func TestAppendMessagesSuccessResetsTheStreak(t *testing.T) {
	w := worker()
	runID := uuid.New()
	fs := &persistFakeStore{
		run:       store.Run{ID: runID, WorkerID: pgUUID(w.ID), Status: "running"},
		poisonSeq: 9,
		insertErr: unstorableErr(),
	}
	clk := t0
	svc := persistSvc(fs, &clk)

	for i := 0; i < 10; i++ {
		_ = svc.AppendMessages(context.Background(), w, runID, []IncomingMessage{msg(9)})
	}
	if got := svc.persistFail.stats(runID); got.streak == 0 {
		t.Fatal("precondition: expected a streak before the success")
	}

	fs.poisonSeq = 0 // the poison is gone; the next batch lands
	clk = t0.Add(time.Minute)
	if err := svc.AppendMessages(context.Background(), w, runID, []IncomingMessage{msg(9)}); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	if got := svc.persistFail.stats(runID); got.streak != 0 {
		t.Fatalf("streak = %d, want 0: one success clears the streak", got.streak)
	}
	svc.persistFail.mu.Lock()
	_, ok := svc.persistFail.lastOK[runID]
	svc.persistFail.mu.Unlock()
	if !ok {
		t.Fatal("a successful append must join the comparison set — that map is written HERE in M4 so M5 adds no second hot-path hook")
	}
}

func TestAppendMessagesPartialApplyAdvancesLastSeqExactlyOnce(t *testing.T) {
	// The subtlety the AppendMessages comment flags: the insert loop is not
	// transactional, so a batch whose Nth message the database refuses leaves the
	// first N-1 committed and ADVANCES last_seq. That happens on the FIRST failure
	// of a streak and never again (the loop breaks at the same message, so maxStored
	// can never exceed the mark it already set). Cost: one streak reset. It DELAYS a
	// kill by one failure and cannot cause a false one.
	w := worker()
	runID := uuid.New()
	fs := &persistFakeStore{
		run:       store.Run{ID: runID, WorkerID: pgUUID(w.ID), Status: "running", LastSeq: 0},
		poisonSeq: 5,
		insertErr: unstorableErr(),
	}
	clk := t0
	svc := persistSvc(fs, &clk)

	batch := []IncomingMessage{msg(1), msg(2), msg(3), msg(4), msg(5), msg(6)}
	for i := 0; i < 21; i++ {
		clk = t0.Add(time.Duration(i) * time.Second)
		if err := svc.AppendMessages(context.Background(), w, runID, batch); !errors.Is(err, ErrUnstorableMessage) {
			t.Fatalf("attempt %d: err = %v, want ErrUnstorableMessage", i, err)
		}
	}
	if fs.run.LastSeq != 4 {
		t.Fatalf("runs.last_seq = %d, want 4: rows 1-4 commit before the poison at 5, and nothing after it is ever attempted", fs.run.LastSeq)
	}
	got := svc.persistFail.stats(runID)
	// Attempt 1 creates the entry at lastSeq 4 (the partial apply already landed
	// inside that same call, so even the first observation is 4). Attempts 2..21
	// increment. A streak of 20 at attempt 21 is what "the kill fires on schedule
	// despite the partial apply" looks like.
	if got.streak != 21 {
		t.Fatalf("streak = %d, want 21: after the first failure last_seq is FROZEN, so every later attempt must extend the streak rather than reset it", got.streak)
	}
	if got.lastSeq != 4 {
		t.Fatalf("observed lastSeq = %d, want 4", got.lastSeq)
	}
}

func TestAppendMessagesPartialApplyCostsAtMostOneStreakReset(t *testing.T) {
	// The other half of the partial-apply story, and the one where a reset DOES
	// happen: a run already carrying a streak at last_seq 0 (a transient 500 that
	// stored nothing) then hits a batch that partially applies. The mark moves once,
	// the streak restarts once, and it accumulates cleanly from there.
	//
	// The claim being measured is "at most ONE reset" — a second one would mean the
	// mark is still moving, and a guard that resets forever can never fire.
	w := worker()
	runID := uuid.New()
	fs := &persistFakeStore{
		run:       store.Run{ID: runID, WorkerID: pgUUID(w.ID), Status: "running", LastSeq: 0},
		poisonSeq: 1, // nothing stores: last_seq stays 0
		insertErr: unstorableErr(),
	}
	clk := t0
	svc := persistSvc(fs, &clk)

	batch := []IncomingMessage{msg(1), msg(2), msg(3), msg(4), msg(5)}
	for i := 0; i < 3; i++ {
		_ = svc.AppendMessages(context.Background(), w, runID, batch)
	}
	if got := svc.persistFail.stats(runID); got.streak != 3 || got.lastSeq != 0 {
		t.Fatalf("precondition: streak=%d lastSeq=%d, want 3 and 0 (nothing stored yet)", got.streak, got.lastSeq)
	}

	// Now rows 1-3 start landing and only seq 4 is poisoned: the partial apply moves
	// last_seq 0 → 3 on this attempt.
	fs.poisonSeq = 4
	_ = svc.AppendMessages(context.Background(), w, runID, batch)
	if got := svc.persistFail.stats(runID); got.streak != 1 || got.lastSeq != 3 {
		t.Fatalf("streak=%d lastSeq=%d, want 1 and 3: last_seq ADVANCED, which is progress and must restart the streak exactly here", got.streak, got.lastSeq)
	}

	for i := 0; i < 19; i++ {
		_ = svc.AppendMessages(context.Background(), w, runID, batch)
	}
	if got := svc.persistFail.stats(runID); got.streak != 20 {
		t.Fatalf("streak = %d, want 20: after the single advance the mark is FROZEN, so the streak must accumulate — a guard that reset forever could never fire", got.streak)
	}
}

func TestNoteOversizeBatchCountsOnlyOwnedNonTerminalRuns(t *testing.T) {
	// The 413 is answered before AppendMessages runs, so without this hook the
	// incident's own long tail — a pre-0.10.1 worker whose retry batch grows past
	// the 1 MiB cap and then stays there — is invisible to both the flag and the kill.
	w := worker()
	runID := uuid.New()
	clk := t0

	t.Run("owned and running", func(t *testing.T) {
		fs := &persistFakeStore{run: store.Run{ID: runID, WorkerID: pgUUID(w.ID), Status: "running", LastSeq: 42}}
		svc := persistSvc(fs, &clk)
		for i := 0; i < 5; i++ {
			svc.NoteOversizeBatch(context.Background(), w, runID)
		}
		got := svc.persistFail.stats(runID)
		if got.streak != 5 {
			t.Fatalf("streak = %d, want 5", got.streak)
		}
		if got.kind != persistFailOversize {
			t.Fatalf("kind = %v, want oversize: its own class, so the 500 → 413 rotation resets the streak ONCE and then rebuilds", got.kind)
		}
		if got.lastSeq != 42 {
			t.Fatalf("lastSeq = %d, want the run row's 42", got.lastSeq)
		}
	})

	t.Run("not owned", func(t *testing.T) {
		fs := &persistFakeStore{runErr: pgx.ErrNoRows}
		svc := persistSvc(fs, &clk)
		for i := 0; i < 5; i++ {
			svc.NoteOversizeBatch(context.Background(), w, runID)
		}
		if got := svc.persistFail.stats(runID); got.streak != 0 {
			t.Fatalf("streak = %d, want 0: the 413 arm answers BEFORE any ownership check, so this hook must re-check ownership itself or it is a cross-tenant kill primitive", got.streak)
		}
	})

	// BOTH recording hooks must be on the SAME rule. This one was left on a
	// terminal-only check when AppendMessages moved to `status != "running"`, so the
	// two sites disagreed with each other — and the divergence was reachable through
	// the exact case that forced the status narrowing: a run parks at
	// `awaiting_approval` while a pre-0.10.1 batcher keeps taking 413s on its grown
	// batch. Measured then: streak 20 built entirely at the gate, `window_seconds=95`,
	// and `oversize` is a killable class, so the run died on the first sweep after
	// the human approved it.
	for _, status := range []string{"cancelled", "completed", "failed", "awaiting_approval", "queued", "claimed"} {
		t.Run("not running: "+status, func(t *testing.T) {
			fs := &persistFakeStore{run: store.Run{ID: runID, WorkerID: pgUUID(w.ID), Status: status}}
			svc := persistSvc(fs, &clk)
			for i := 0; i < 12; i++ {
				svc.NoteOversizeBatch(context.Background(), w, runID)
			}
			if got := svc.persistFail.stats(runID); got.streak != 0 {
				t.Fatalf("streak = %d after 12 oversize batches on a %s run, want 0: the 413 hook must hold the SAME rule as the recorder, or a streak accumulates where the recorder would have refused it",
					got.streak, status)
			}
		})
	}
}

func TestSweepPrunesTheTracker(t *testing.T) {
	// The prune is wired into the sweep, not only implemented. Without the call the
	// map has no bound at all for a run whose worker vanished mid-wedge.
	fs := &fakeStore{}
	svc := New(fs, nil, testParams())
	clk := t0
	svc.now = func() time.Time { return clk }

	id := uuid.New()
	svc.persistFail.recordFailure(id, persistFailUnstorable, testSeq, t0)

	clk = t0.Add(persistFailTTL + time.Minute)
	if _, err := svc.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if got := svc.persistFail.stats(id); got.streak != 0 {
		t.Fatalf("streak = %d, want 0: Sweep must prune the tracker, or an abandoned run's entry lives forever", got.streak)
	}
}
