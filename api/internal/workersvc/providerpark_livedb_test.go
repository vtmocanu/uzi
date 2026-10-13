package workersvc

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1733: the typed provider-transient park, against a REAL Postgres. These drive the real
// Service (real *store.Queries, the pool as the tx beginner) with an injected clock so the bound
// is crossed without sleeping. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway
// Postgres (run via ./e2e/run-store-it.sh); a package printing `ok` with PASS=0 is INVALID.

const ppBound = 24 * time.Hour

// providerParkService builds a Service over the live store with the pool as the tx beginner, a
// caller-chosen bound, and a clock the test moves through the returned pointer.
func (e interlockLiveDB) providerParkService(t *testing.T, bound time.Duration) (*Service, *time.Time) {
	t.Helper()
	p := testParams()
	p.RunProviderParkMaxDuration = bound
	svc := New(e.q, newBox(t), p)
	svc.SetTxBeginner(e.pool)
	svc.SetBackground(func(func()) {})
	clock := time.Now().UTC().Truncate(time.Microsecond)
	svc.now = func() time.Time { return clock }
	return svc, &clock
}

type ppRow struct {
	status       string
	holdReason   pgtype.Text
	cause        pgtype.Text
	count        int32
	streakStart  pgtype.Timestamptz
	waitCount    int32
	claimRelease pgtype.Timestamptz
	workerID     pgtype.UUID
	generation   int64
	pauseReq     pgtype.Timestamptz
	pauseMode    pgtype.Text
	durability   pgtype.Bool
}

func (e interlockLiveDB) ppRun(t *testing.T, runID uuid.UUID) ppRow {
	t.Helper()
	var r ppRow
	if err := e.pool.QueryRow(e.ctx, `SELECT status, hold_reason, recovery_wait_cause, provider_park_count, provider_park_streak_started_at,
	       recovery_wait_count, claim_released_at, worker_id, claim_generation, pause_requested_at, pause_mode, checkpoint_contains_latest
	  FROM runs WHERE id = $1`, runID).
		Scan(&r.status, &r.holdReason, &r.cause, &r.count, &r.streakStart, &r.waitCount, &r.claimRelease, &r.workerID, &r.generation, &r.pauseReq, &r.pauseMode, &r.durability); err != nil {
		t.Fatalf("read run: %v", err)
	}
	return r
}

func providerParkReport(gen int64, productive *int32) StateRequest {
	return StateRequest{State: "recovery_wait", RecoveryCause: strPtr(recoveryCauseProviderOutage), ClaimGeneration: i64Ptr(gen), ProductiveTurns: productive}
}

func i32Ptr(v int32) *int32 { return &v }

// seedStreak plants an open provider-park streak on a running run.
func (e interlockLiveDB) seedStreak(t *testing.T, runID uuid.UUID, count int32, started time.Time) {
	t.Helper()
	e.exec(t, `UPDATE runs SET provider_park_count = $2, provider_park_streak_started_at = $3 WHERE id = $1`, runID, count, pgconv.Time(started))
}

// TestProviderParkTypedStoresCauseAndCountsLiveDB: a typed park parks the run recovery_wait with
// cause provider_outage, opens a streak (count 1, started now), and bumps the backoff shaper; a
// redelivery is the idempotent no-op; the next park after an AUTOMATIC promotion continues the
// SAME streak (count 2, same start), proving the sweep preserves it.
func TestProviderParkTypedStoresCauseAndCountsLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc, clock := e.providerParkService(t, ppBound)
	t0 := *clock

	w := e.seedWorker(t, nil)
	runID := e.fpSeedRunning(t, w, 1)

	run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID, providerParkReport(1, nil))
	if err != nil || !applied || run.Status != "recovery_wait" {
		t.Fatalf("typed park: applied=%v status=%q err=%v, want applied recovery_wait", applied, run.Status, err)
	}
	r := e.ppRun(t, runID)
	if r.status != "recovery_wait" || !r.cause.Valid || r.cause.String != "provider_outage" {
		t.Fatalf("status=%q cause=%v, want recovery_wait/provider_outage", r.status, r.cause)
	}
	if r.count != 1 || !r.streakStart.Valid || !r.streakStart.Time.Equal(t0) || r.waitCount != 1 {
		t.Fatalf("count=%d start=%v waitCount=%d, want 1/%v/1", r.count, r.streakStart, r.waitCount, t0)
	}

	run, applied, err = svc.SetState(e.ctx, store.Worker{ID: w}, runID, providerParkReport(1, nil))
	if err != nil || applied || run.Status != "recovery_wait" {
		t.Fatalf("redelivered park: applied=%v status=%q err=%v, want a not-applied recovery_wait no-op", applied, run.Status, err)
	}
	if r = e.ppRun(t, runID); r.count != 1 || r.waitCount != 1 {
		t.Fatalf("redelivery changed counters: count=%d waitCount=%d, want 1/1", r.count, r.waitCount)
	}

	gen := e.dpResume(t, runID, w) // the sweep's promotion PRESERVES the streak
	*clock = t0.Add(time.Hour)
	if _, applied, err = svc.SetState(e.ctx, store.Worker{ID: w}, runID, providerParkReport(gen, nil)); err != nil || !applied {
		t.Fatalf("second park: applied=%v err=%v", applied, err)
	}
	if r = e.ppRun(t, runID); r.status != "recovery_wait" || r.count != 2 || !r.streakStart.Time.Equal(t0) {
		t.Fatalf("second park: status=%q count=%d start=%v, want recovery_wait/2/%v (the same streak)", r.status, r.count, r.streakStart.Time, t0)
	}
}

// TestProviderParkPastBoundHoldsLiveDB: the first provider park at or past the bound becomes a
// paused 'provider_unavailable' hold that KEEPS the claim (claim_released_at NULL, worker_id and
// the worker_active_runs row intact), records the park's durability report against 'paused', and
// keeps the streak's count and start for the surfaces.
func TestProviderParkPastBoundHoldsLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc, clock := e.providerParkService(t, ppBound)
	t0 := *clock

	w := e.seedWorker(t, nil)
	runID := e.fpSeedRunning(t, w, 1)
	if _, _, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID, providerParkReport(1, nil)); err != nil {
		t.Fatalf("first park: %v", err)
	}
	gen := e.dpResume(t, runID, w)
	e.exec(t, `INSERT INTO worker_active_runs (worker_id, run_id, claim_generation, phase, snapshot_epoch, reported_at)
	           VALUES ($1, $2, $3, 'running', 1, now())`, w, runID, gen)

	*clock = t0.Add(ppBound) // exactly the bound: >= holds
	req := providerParkReport(gen, nil)
	req.CheckpointContainsLatest = func() *bool { b := true; return &b }()
	run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID, req)
	if err != nil || !applied || run.Status != "paused" {
		t.Fatalf("past-bound park: applied=%v status=%q err=%v, want applied paused", applied, run.Status, err)
	}
	r := e.ppRun(t, runID)
	if r.status != "paused" || !r.holdReason.Valid || r.holdReason.String != "provider_unavailable" {
		t.Fatalf("status=%q hold_reason=%v, want paused/provider_unavailable", r.status, r.holdReason)
	}
	if r.claimRelease.Valid || !r.workerID.Valid || uuid.UUID(r.workerID.Bytes) != w {
		t.Fatalf("hold released the claim or dropped affinity: claim_released_at=%v worker_id=%v", r.claimRelease, r.workerID)
	}
	var active int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM worker_active_runs WHERE worker_id = $1 AND run_id = $2`, w, runID).Scan(&active); err != nil || active != 1 {
		t.Fatalf("worker_active_runs rows = %d (err %v), want the row intact", active, err)
	}
	if r.count != 2 || !r.streakStart.Time.Equal(t0) {
		t.Fatalf("hold streak: count=%d start=%v, want 2/%v", r.count, r.streakStart.Time, t0)
	}
	if !r.durability.Valid || !r.durability.Bool {
		t.Fatalf("checkpoint_contains_latest = %v, want true recorded against the paused hold", r.durability)
	}

	// A duplicate of the applied hold is a no-op and never counts twice.
	if run, applied, err = svc.SetState(e.ctx, store.Worker{ID: w}, runID, providerParkReport(gen, nil)); err != nil || applied || run.Status != "paused" {
		t.Fatalf("duplicate hold report: applied=%v status=%q err=%v, want a not-applied paused no-op", applied, run.Status, err)
	}
	if r = e.ppRun(t, runID); r.count != 2 {
		t.Fatalf("duplicate hold report changed count to %d, want 2", r.count)
	}
}

// TestProviderParkBoundZeroNeverHoldsLiveDB: RUN_PROVIDER_PARK_MAX_DURATION=0 keeps typing and
// counting on but never converts a park to the hold, however old the streak.
func TestProviderParkBoundZeroNeverHoldsLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc, clock := e.providerParkService(t, 0)
	w := e.seedWorker(t, nil)
	runID := e.fpSeedRunning(t, w, 1)
	e.seedStreak(t, runID, 3, clock.Add(-1000*time.Hour))

	if _, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID, providerParkReport(1, nil)); err != nil || !applied {
		t.Fatalf("park: applied=%v err=%v", applied, err)
	}
	if r := e.ppRun(t, runID); r.status != "recovery_wait" || r.count != 4 {
		t.Fatalf("bound 0: status=%q count=%d, want recovery_wait/4", r.status, r.count)
	}
}

// TestProviderParkProductiveTurnsStartNewStreakLiveDB: a park reporting productive turns starts a
// NEW streak at this park (count 1, started now) even when the old streak is long past the bound;
// absent or zero productive turns continue it and hold.
func TestProviderParkProductiveTurnsStartNewStreakLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name       string
		productive *int32
		wantStatus string
		wantCount  int32
		fresh      bool
	}{
		{"progress", i32Ptr(2), "recovery_wait", 1, true},
		{"zero is not progress", i32Ptr(0), "paused", 4, false},
		{"absent is not progress", nil, "paused", 4, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			svc, clock := e.providerParkService(t, ppBound)
			w := e.seedWorker(t, nil)
			runID := e.fpSeedRunning(t, w, 1)
			old := clock.Add(-48 * time.Hour)
			e.seedStreak(t, runID, 3, old)

			if _, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID, providerParkReport(1, tc.productive)); err != nil || !applied {
				t.Fatalf("park: applied=%v err=%v", applied, err)
			}
			r := e.ppRun(t, runID)
			wantStart := old
			if tc.fresh {
				wantStart = *clock
			}
			if r.status != tc.wantStatus || r.count != tc.wantCount || !r.streakStart.Time.Equal(wantStart) {
				t.Fatalf("status=%q count=%d start=%v, want %s/%d/%v", r.status, r.count, r.streakStart.Time, tc.wantStatus, tc.wantCount, wantStart)
			}
		})
	}
}

// TestProviderParkCountSaturatesLiveDB: the count saturates at the int32 column maximum instead of
// overflowing the write.
func TestProviderParkCountSaturatesLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc, clock := e.providerParkService(t, ppBound)
	w := e.seedWorker(t, nil)
	runID := e.fpSeedRunning(t, w, 1)
	e.seedStreak(t, runID, math.MaxInt32, *clock)

	if _, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID, providerParkReport(1, nil)); err != nil || !applied {
		t.Fatalf("park: applied=%v err=%v", applied, err)
	}
	if r := e.ppRun(t, runID); r.status != "recovery_wait" || r.count != math.MaxInt32 {
		t.Fatalf("status=%q count=%d, want recovery_wait/%d", r.status, r.count, math.MaxInt32)
	}
}

// TestProviderParkUntypedWithoutProgressPreservesStreakLiveDB: without progress evidence,
// untyped and empty_turn parks keep cause NULL and preserve the open provider streak.
func TestProviderParkUntypedWithoutProgressPreservesStreakLiveDB(t *testing.T) {
	for name, cause := range map[string]*string{"untyped": nil, "empty_turn": strPtr("empty_turn")} {
		t.Run(name, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			svc, clock := e.providerParkService(t, ppBound)
			w := e.seedWorker(t, nil)
			runID := e.fpSeedRunning(t, w, 1)
			old := clock.Add(-48 * time.Hour)
			e.seedStreak(t, runID, 3, old)

			req := StateRequest{State: "recovery_wait", RecoveryCause: cause, ClaimGeneration: i64Ptr(1)}
			if _, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID, req); err != nil || !applied {
				t.Fatalf("park: applied=%v err=%v", applied, err)
			}
			r := e.ppRun(t, runID)
			if r.status != "recovery_wait" || r.cause.Valid || r.count != 3 || !r.streakStart.Time.Equal(old) {
				t.Fatalf("status=%q cause=%v count=%d start=%v, want an ordinary NULL-cause park with the streak 3/%v untouched", r.status, r.cause, r.count, r.streakStart.Time, old)
			}
		})
	}
}

// TestProviderParkFencedReportChangesNothingLiveDB: a report from a superseded generation, and one
// onto a released claim, are refused ErrStaleClaim with nothing mutated, counters included.
func TestProviderParkFencedReportChangesNothingLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc, clock := e.providerParkService(t, ppBound)
	w := e.seedWorker(t, nil)
	started := clock.Add(-time.Hour)

	stale := e.fpSeedRunning(t, w, 2)
	e.seedStreak(t, stale, 3, started)
	released := e.fpSeedRunning(t, w, 1)
	e.seedStreak(t, released, 3, started)
	e.exec(t, `UPDATE runs SET claim_released_at = now() WHERE id = $1`, released)

	for name, tc := range map[string]struct {
		id  uuid.UUID
		gen int64
	}{"stale generation": {stale, 1}, "released claim": {released, 1}} {
		_, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, tc.id, providerParkReport(tc.gen, i32Ptr(5)))
		if !errors.Is(err, ErrStaleClaim) || applied {
			t.Fatalf("%s: applied=%v err=%v, want ErrStaleClaim", name, applied, err)
		}
		r := e.ppRun(t, tc.id)
		if r.status != "running" || r.cause.Valid || r.count != 3 || !r.streakStart.Time.Equal(started) || r.waitCount != 0 {
			t.Fatalf("%s mutated the run: status=%q cause=%v count=%d start=%v waitCount=%d", name, r.status, r.cause, r.count, r.streakStart.Time, r.waitCount)
		}
	}
}

// TestProviderParkRefusesInvalidReportsLiveDB: every refusal is ErrInvalidState (the 400 "state
// must be one of" body, never a decode error) and changes nothing: a typed park needs a positive
// generation, and productive_turns is valid only on the typed park and never negative.
func TestProviderParkRefusesInvalidReportsLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc, _ := e.providerParkService(t, ppBound)
	w := e.seedWorker(t, nil)
	runID := e.fpSeedRunning(t, w, 1)

	typed := func(gen *int64, productive *int32) StateRequest {
		return StateRequest{State: "recovery_wait", RecoveryCause: strPtr(recoveryCauseProviderOutage), ClaimGeneration: gen, ProductiveTurns: productive}
	}
	for name, req := range map[string]StateRequest{
		"typed without a generation":    typed(nil, nil),
		"typed with generation zero":    typed(i64Ptr(0), nil),
		"negative productive_turns":     typed(i64Ptr(1), i32Ptr(-1)),
		"productive_turns, no cause":    {State: "recovery_wait", ClaimGeneration: i64Ptr(1), ProductiveTurns: i32Ptr(1)},
		"productive_turns, other cause": {State: "recovery_wait", RecoveryCause: strPtr("empty_turn"), ClaimGeneration: i64Ptr(1), ProductiveTurns: i32Ptr(1)},
		"productive_turns, wrong state": {State: "running", RecoveryCause: strPtr(recoveryCauseProviderOutage), ClaimGeneration: i64Ptr(1), ProductiveTurns: i32Ptr(1)},
	} {
		_, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID, req)
		if !errors.Is(err, ErrInvalidState) || applied {
			t.Fatalf("%s: applied=%v err=%v, want ErrInvalidState", name, applied, err)
		}
	}
	if r := e.ppRun(t, runID); r.status != "running" || r.count != 0 || r.streakStart.Valid {
		t.Fatalf("a refused report mutated the run: status=%q count=%d start=%v", r.status, r.count, r.streakStart)
	}
}

// TestProviderParkStampedStopCancelsLiveDB: a stop verdict stamped on the run wins over the park:
// the run is cancelled through the worker path, never parked or held, and the streak is untouched.
func TestProviderParkStampedStopCancelsLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc, clock := e.providerParkService(t, ppBound)
	w := e.seedWorker(t, nil)
	runID := e.fpSeedRunning(t, w, 1)
	e.seedStreak(t, runID, 5, clock.Add(-48*time.Hour)) // past the bound: would hold if not stopped
	e.exec(t, `UPDATE runs SET stop_kind = 'cancelled' WHERE id = $1`, runID)

	if _, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID, providerParkReport(1, nil)); err != nil || !applied {
		t.Fatalf("park onto a stamped stop: applied=%v err=%v", applied, err)
	}
	if r := e.ppRun(t, runID); r.status != "cancelled" || r.count != 5 {
		t.Fatalf("status=%q count=%d, want cancelled with the streak untouched", r.status, r.count)
	}
}

// ppHold parks a fresh running run into the provider hold and returns the run and its generation.
func (e interlockLiveDB) ppHold(t *testing.T, svc *Service, clock *time.Time, w uuid.UUID) (uuid.UUID, int64) {
	t.Helper()
	runID := e.fpSeedRunning(t, w, 1)
	e.seedStreak(t, runID, 3, clock.Add(-48*time.Hour))
	if _, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID, providerParkReport(1, nil)); err != nil || !applied {
		t.Fatalf("hold: applied=%v err=%v", applied, err)
	}
	if r := e.ppRun(t, runID); r.status != "paused" || r.holdReason.String != "provider_unavailable" {
		t.Fatalf("setup did not hold: status=%q hold=%v", r.status, r.holdReason)
	}
	return runID, 1
}

// TestProviderHoldSettlesEveryPendingPauseInputLiveDB: the hold consumes EVERY unapplied owner
// pause input (wall, now, milestone, and the pause_cancel flag-clear) and clears pause_*, so a
// resumed flight is never handed a stale abort; an unrelated steering input survives.
func TestProviderHoldSettlesEveryPendingPauseInputLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc, clock := e.providerParkService(t, ppBound)
	w := e.seedWorker(t, nil)
	runID := e.fpSeedRunning(t, w, 1)
	e.seedStreak(t, runID, 3, clock.Add(-48*time.Hour))
	e.exec(t, `UPDATE runs SET pause_requested_at = now(), pause_mode = 'now', pause_after_count = 0 WHERE id = $1`, runID)
	for _, in := range [][2]any{{"pause", "wall"}, {"pause", "now"}, {"pause", "milestone"}, {"pause_cancel", nil}, {"follow_up", "keep going"}} {
		e.exec(t, `INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1, $2, $3)`, runID, in[0], in[1])
	}

	if _, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID, providerParkReport(1, nil)); err != nil || !applied {
		t.Fatalf("hold: applied=%v err=%v", applied, err)
	}
	r := e.ppRun(t, runID)
	if r.status != "paused" || r.pauseReq.Valid || r.pauseMode.Valid {
		t.Fatalf("status=%q pause_requested_at=%v pause_mode=%v, want paused with pause_* cleared", r.status, r.pauseReq, r.pauseMode)
	}
	var unapplied []string
	rows, err := e.pool.Query(e.ctx, `SELECT kind FROM run_user_inputs WHERE run_id = $1 AND applied_at IS NULL ORDER BY id`, runID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		unapplied = append(unapplied, k)
	}
	rows.Close()
	if len(unapplied) != 1 || unapplied[0] != "follow_up" {
		t.Fatalf("unapplied inputs after the hold = %v, want only [follow_up]", unapplied)
	}

	// After the owner resumes and the run is re-claimed, the next consume returns no pause.
	if _, err := e.q.ResumePausedRun(e.ctx, store.ResumePausedRunParams{ID: runID, UserID: e.userID, GlobalTimeoutSeconds: 3600}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	e.exec(t, `UPDATE runs SET status = 'running', claim_generation = claim_generation + 1 WHERE id = $1`, runID)
	got, err := e.q.ConsumeRunInputs(e.ctx, runID)
	if err != nil {
		t.Fatalf("ConsumeRunInputs: %v", err)
	}
	for _, in := range got {
		if in.Kind == "pause" || in.Kind == "pause_cancel" {
			t.Fatalf("the resumed flight was handed a stale %q input", in.Kind)
		}
	}
	if len(got) != 1 || got[0].Kind != "follow_up" {
		t.Fatalf("ConsumeRunInputs = %+v, want only the follow_up", got)
	}
}

// TestProviderHoldNeverFailedByLateReportLiveDB: a late same-generation failed report, and the
// three sibling failing writers, leave the hold untouched. The control proves a budget_exhausted
// hold is guarded identically, so the guard list was extended rather than replaced.
func TestProviderHoldNeverFailedByLateReportLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc, clock := e.providerParkService(t, ppBound)
	w := e.seedWorker(t, nil)
	runID, gen := e.ppHold(t, svc, clock, w)
	wid := pgconv.UUID(w)

	run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID, StateRequest{State: "failed", ClaimGeneration: i64Ptr(gen), FailureReason: strPtr("late")})
	if err != nil || applied || run.Status != "paused" {
		t.Fatalf("late failed report: applied=%v status=%q err=%v, want a not-applied paused no-op", applied, run.Status, err)
	}
	if n, err := e.q.SetRunFailed(e.ctx, store.SetRunFailedParams{ID: runID, WorkerID: wid}); err != nil || n != 0 {
		t.Fatalf("SetRunFailed rows=%d err=%v, want 0", n, err)
	}
	if n, err := e.q.SetRunFailedPlanRejected(e.ctx, store.SetRunFailedPlanRejectedParams{ID: runID, WorkerID: wid}); err != nil || n != 0 {
		t.Fatalf("SetRunFailedPlanRejected transitioned=%d err=%v, want 0", n, err)
	}
	if n, err := e.q.SupersedeRunByWorker(e.ctx, store.SupersedeRunByWorkerParams{ID: runID, WorkerID: wid, StopReason: "superseded"}); err != nil || n != 0 {
		t.Fatalf("SupersedeRunByWorker rows=%d err=%v, want 0", n, err)
	}
	if r := e.ppRun(t, runID); r.status != "paused" || r.holdReason.String != "provider_unavailable" {
		t.Fatalf("hold changed: status=%q hold=%v", r.status, r.holdReason)
	}
}

// TestProviderHoldAwaitingGuardLiveDB (R3): a late same-generation awaiting_input / awaiting_followup
// write cannot turn the provider hold into a question park. Only the provider hold is guarded: a
// budget_exhausted wall park still takes both, exactly as before.
func TestProviderHoldAwaitingGuardLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc, clock := e.providerParkService(t, ppBound)
	w := e.seedWorker(t, nil)
	wid := pgconv.UUID(w)

	held, _ := e.ppHold(t, svc, clock, w)
	n, err := e.q.SetRunAwaitingInput(e.ctx, store.SetRunAwaitingInputParams{ID: held, WorkerID: wid, OpenQuestionID: pgtype.Text{String: "q", Valid: true}})
	if err != nil || n != 0 {
		t.Fatalf("SetRunAwaitingInput on the provider hold: rows=%d err=%v, want 0", n, err)
	}
	if n, err = e.q.SetRunAwaitingFollowup(e.ctx, store.SetRunAwaitingFollowupParams{ID: held, WorkerID: wid}); err != nil || n != 0 {
		t.Fatalf("SetRunAwaitingFollowup on the provider hold: rows=%d err=%v, want 0", n, err)
	}
	if r := e.ppRun(t, held); r.status != "paused" || r.holdReason.String != "provider_unavailable" {
		t.Fatalf("provider hold changed: status=%q hold=%v", r.status, r.holdReason)
	}

	for name, write := range map[string]func(id uuid.UUID) (int64, error){
		"awaiting_input": func(id uuid.UUID) (int64, error) {
			return e.q.SetRunAwaitingInput(e.ctx, store.SetRunAwaitingInputParams{ID: id, WorkerID: wid, OpenQuestionID: pgtype.Text{String: "q", Valid: true}})
		},
		"awaiting_followup": func(id uuid.UUID) (int64, error) {
			return e.q.SetRunAwaitingFollowup(e.ctx, store.SetRunAwaitingFollowupParams{ID: id, WorkerID: wid})
		},
	} {
		wall := e.fpSeedRunning(t, w, 1)
		e.exec(t, `UPDATE runs SET status = 'paused', hold_reason = 'budget_exhausted' WHERE id = $1`, wall)
		if n, err := write(wall); err != nil || n != 1 {
			t.Fatalf("control: %s onto a budget_exhausted park rows=%d err=%v, want 1 (unchanged behaviour)", name, n, err)
		}
	}
}

// TestProviderHoldResumeRejectsLateWritersLiveDB covers the retained-marker interval:
// owner Resume queues the same unreleased worker/generation, so generation fencing alone
// cannot reject its old flight yet. The real ClaimRun query then opens a fresh generation.
func TestProviderHoldResumeRejectsLateWritersLiveDB(t *testing.T) {
	for _, name := range []string{
		"service failed", "service failed legacy",
		"failed", "failed legacy", "plan rejected", "plan rejected legacy",
		"superseded", "awaiting input", "awaiting followup",
	} {
		t.Run(name, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			svc, clock := e.providerParkService(t, ppBound)
			w := e.seedWorker(t, nil)
			worker := store.Worker{ID: w, UserID: e.userID}
			wid := pgconv.UUID(w)
			runID, gen := e.ppHold(t, svc, clock, w)

			// The count from SetRunFailedPlanRejected is transitioned runs, not settled inputs.
			// SupersedeRunByWorker exercises both its candidate and final UPDATE guards.
			write := func(id uuid.UUID, generation int64) (int64, error) {
				g := pgtype.Int8{Int64: generation, Valid: true}
				switch name {
				case "service failed", "service failed legacy":
					req := StateRequest{State: "failed", FailureReason: strPtr("late"), ClaimGeneration: i64Ptr(generation)}
					if name == "service failed legacy" {
						req.ClaimGeneration = nil
					}
					_, applied, err := svc.SetState(e.ctx, worker, id, req)
					if applied {
						return 1, err
					}
					return 0, err
				case "failed", "failed legacy":
					if name == "failed legacy" {
						g = pgtype.Int8{}
					}
					return e.q.SetRunFailed(e.ctx, store.SetRunFailedParams{ID: id, WorkerID: wid, ClaimGeneration: g})
				case "plan rejected", "plan rejected legacy":
					if name == "plan rejected legacy" {
						g = pgtype.Int8{}
					}
					return e.q.SetRunFailedPlanRejected(e.ctx, store.SetRunFailedPlanRejectedParams{ID: id, WorkerID: wid, ClaimGeneration: g})
				case "superseded":
					return e.q.SupersedeRunByWorker(e.ctx, store.SupersedeRunByWorkerParams{ID: id, WorkerID: wid, StopReason: "superseded"})
				case "awaiting input":
					return e.q.SetRunAwaitingInput(e.ctx, store.SetRunAwaitingInputParams{ID: id, WorkerID: wid, OpenQuestionID: pgtype.Text{String: "q", Valid: true}})
				default:
					return e.q.SetRunAwaitingFollowup(e.ctx, store.SetRunAwaitingFollowupParams{ID: id, WorkerID: wid})
				}
			}

			held := e.ppRun(t, runID)
			if n, err := write(runID, gen); err != nil || n != 0 {
				t.Fatalf("late writer on hold: rows=%d err=%v, want 0", n, err)
			}
			if after := e.ppRun(t, runID); after != held {
				t.Fatalf("late writer changed hold: %+v -> %+v", held, after)
			}
			if _, err := e.q.ResumePausedRun(e.ctx, store.ResumePausedRunParams{ID: runID, UserID: e.userID, GlobalTimeoutSeconds: 3600}); err != nil {
				t.Fatalf("owner resume: %v", err)
			}
			queued := e.ppRun(t, runID)
			if queued.status != "queued" || queued.count != 0 || queued.streakStart.Valid ||
				queued.holdReason != held.holdReason || queued.workerID != held.workerID ||
				queued.generation != gen || queued.claimRelease.Valid {
				t.Fatalf("resume must clear accounting and retain marker/claim: %+v -> %+v", held, queued)
			}
			if n, err := write(runID, gen); err != nil || n != 0 {
				t.Fatalf("late writer after owner resume: rows=%d err=%v, want 0", n, err)
			}
			if after := e.ppRun(t, runID); after != queued {
				t.Fatalf("late writer changed queued resume: %+v -> %+v", queued, after)
			}

			// Exercise ClaimRun itself, not a manually fired promotion helper. No active
			// snapshot was seeded, so the old flight's snapshot exclusion does not apply.
			fresh, err := e.q.ClaimRun(e.ctx, claimRunParams(worker))
			if err != nil {
				t.Fatalf("ClaimRun: %v", err)
			}
			if fresh.ID != runID || fresh.Status != "claimed" || fresh.ClaimGeneration != gen+1 ||
				fresh.WorkerID != wid || fresh.ClaimReleasedAt.Valid || fresh.HoldReason != held.holdReason {
				t.Fatalf("fresh claim must advance generation and retain marker: %+v", fresh)
			}
			if _, applied, err := svc.SetState(e.ctx, worker, runID, StateRequest{State: "failed", ClaimGeneration: i64Ptr(gen)}); !errors.Is(err, ErrStaleClaim) || applied {
				t.Fatalf("old generation after reclaim: applied=%v err=%v, want ErrStaleClaim", applied, err)
			}
			if run, applied, err := svc.SetState(e.ctx, worker, runID, StateRequest{State: "running", ClaimGeneration: i64Ptr(fresh.ClaimGeneration)}); err != nil || !applied || run.Status != "running" {
				t.Fatalf("fresh running report: applied=%v status=%q err=%v", applied, run.Status, err)
			}
			if r := e.ppRun(t, runID); r.holdReason.Valid || r.generation != gen+1 {
				t.Fatalf("fresh running report did not clear marker: %+v", r)
			}
			if n, err := write(runID, fresh.ClaimGeneration); err != nil || n != 1 {
				t.Fatalf("ordinary writer on fresh running flight: rows=%d err=%v, want 1", n, err)
			}
		})
	}
}

// TestProviderHoldResumeClearsStreakLiveDB: the owner's resume re-queues the hold with the streak
// cleared in the same statement and keeps the claim fields; a refused resume (foreign owner)
// changes nothing.
func TestProviderHoldResumeClearsStreakLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc, clock := e.providerParkService(t, ppBound)
	w := e.seedWorker(t, nil)
	runID, _ := e.ppHold(t, svc, clock, w)
	before := e.ppRun(t, runID)

	if _, err := e.q.ResumePausedRun(e.ctx, store.ResumePausedRunParams{ID: runID, UserID: uuid.New(), GlobalTimeoutSeconds: 3600}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("foreign resume err=%v, want no row", err)
	}
	if after := e.ppRun(t, runID); after != before {
		t.Fatalf("a refused resume changed the run: %+v -> %+v", before, after)
	}

	if _, err := e.q.ResumePausedRun(e.ctx, store.ResumePausedRunParams{ID: runID, UserID: e.userID, GlobalTimeoutSeconds: 3600}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	r := e.ppRun(t, runID)
	if r.status != "queued" || r.count != 0 || r.streakStart.Valid {
		t.Fatalf("after resume: status=%q count=%d start=%v, want queued with the streak cleared", r.status, r.count, r.streakStart)
	}
	if r.claimRelease.Valid || !r.workerID.Valid {
		t.Fatalf("resume dropped worker affinity: claim_released_at=%v worker_id=%v", r.claimRelease, r.workerID)
	}
}

// TestProviderParkOwnerPromoteClearsAutomaticPreservesLiveDB: the owner's early promote
// (PromoteRecoveryWaitRunNow, the set-token path) clears the streak, while the vault-unlock
// promotion keeps it (the sweep's preservation is pinned by the typed-park test above).
func TestProviderParkOwnerPromoteClearsAutomaticPreservesLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc, clock := e.providerParkService(t, ppBound)
	w := e.seedWorker(t, nil)

	early := e.fpSeedRunning(t, w, 1)
	if _, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, early, providerParkReport(1, nil)); err != nil || !applied {
		t.Fatalf("park: applied=%v err=%v", applied, err)
	}
	if n, err := e.q.PromoteRecoveryWaitRunNow(e.ctx, store.PromoteRecoveryWaitRunNowParams{ID: early, UserID: e.userID}); err != nil || n != 1 {
		t.Fatalf("early promote rows=%d err=%v, want 1", n, err)
	}
	if r := e.ppRun(t, early); r.status != "queued" || r.count != 0 || r.streakStart.Valid {
		t.Fatalf("early promote: status=%q count=%d start=%v, want queued with the streak cleared", r.status, r.count, r.streakStart)
	}

	unlocked := e.fpSeedRunning(t, w, 1)
	e.seedStreak(t, unlocked, 3, *clock)
	e.exec(t, `UPDATE runs SET status = 'recovery_wait', recovery_wait_cause = 'vault_locked' WHERE id = $1`, unlocked)
	if _, err := e.q.PromoteVaultLockedRecoveryWaitRuns(e.ctx, e.userID); err != nil {
		t.Fatalf("vault unlock promote: %v", err)
	}
	if r := e.ppRun(t, unlocked); r.status != "queued" || r.count != 3 || !r.streakStart.Valid {
		t.Fatalf("vault unlock: status=%q count=%d start=%v, want queued with the streak preserved", r.status, r.count, r.streakStart)
	}
}
