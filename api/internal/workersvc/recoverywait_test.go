package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// 🔴 TestRecoveryParkFallbackIsCappedExponential pins the transient-recovery backoff
// (issue #1197), and the two properties that make it a RECOVERY schedule rather than a
// LIMIT one are the point: it is exponential-then-capped, and it is ALWAYS a finite
// positive duration — there is NO terminal/zero sentinel, because a recovery park always
// becomes promotable again (no lifetime cap).
func TestRecoveryParkFallbackIsCappedExponential(t *testing.T) {
	// base=1m, cap=30m via testParams(); recoveryParkFallbackFor reads them off Params.
	svc := New(&fakeStore{}, newBox(t), testParams())

	// The argument is the PRIOR park count (0 on the first), doubling from the 1m base.
	for priorParks, want := range map[int32]time.Duration{
		0: 1 * time.Minute,
		1: 2 * time.Minute,
		2: 4 * time.Minute,
		3: 8 * time.Minute,
		4: 16 * time.Minute,
	} {
		if got := svc.recoveryParkFallbackFor(priorParks); got != want {
			t.Fatalf("priorParks=%d: %v, want %v (doubling from the 1m base)", priorParks, got, want)
		}
	}

	// The 5th park doubles past the 30m cap and every later one is clamped — and, unlike
	// the usage-limit park, a HIGH count never fails the run: it just keeps parking at the
	// cap. This is the "no lifetime cap" property. Any count at or above
	// recoveryParkFallbackMaxShift short-circuits STRAIGHT to maxPark without shifting, so
	// none of these large values (20, 21, 64, 1<<20) reach the doubling at all — the d < base
	// overflow guard stays defensive, exercised only by a pathologically-large configured
	// base, never by an unbounded count.
	for _, priorParks := range []int32{5, 6, 19, 20, 21, 64, 1 << 20} {
		got := svc.recoveryParkFallbackFor(priorParks)
		if got != 30*time.Minute {
			t.Fatalf("priorParks=%d: %v, want the 30m cap", priorParks, got)
		}
		if got <= 0 {
			t.Fatalf("priorParks=%d produced a non-positive duration %v — every count must map "+
				"to a finite positive stamp in the FUTURE; a non-positive one would be a stamp in "+
				"the past and the promote->re-park loop this schedule exists to avoid", priorParks, got)
		}
	}

	// Negative is not reachable through the column (int NOT NULL DEFAULT 0), but the
	// function is total and a shift by a negative count panics in Go.
	if got := svc.recoveryParkFallbackFor(-1); got != time.Minute {
		t.Fatalf("negative priorParks: %v, want the base %v", got, time.Minute)
	}
}

// recoveryRun is a running run with a given prior recovery-park count, the source shape
// setRecoveryWait reads (RecoveryWaitCount feeds the backoff curve).
func recoveryRun(priorParks int32) store.Run {
	r := runningRun(true)
	r.RecoveryWaitCount = priorParks
	return r
}

// TestSetStateRecoveryWaitParks walks the whole arm through the REAL SetState: the report
// routes to setRecoveryWait, the stamp is computed from the run's prior park count, and
// SetRunRecoveryWait is called with recovery_retry_not_before ~= now + backoff + jitter.
func TestSetStateRecoveryWaitParks(t *testing.T) {
	for name, tc := range map[string]struct {
		priorParks int32
		backoff    time.Duration
	}{
		"first park":  {0, 1 * time.Minute},
		"third park":  {2, 4 * time.Minute},
		"capped park": {9, 30 * time.Minute},
	} {
		run := recoveryRun(tc.priorParks)
		fs, svc, wkr := limitParkFixture(t, run)
		fs.setRecoveryWaitRows = 1

		_, applied, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{State: "recovery_wait"})
		if err != nil {
			t.Fatalf("%s: SetState: %v", name, err)
		}
		if !applied {
			t.Fatalf("%s: applied = false for a park the store accepted", name)
		}
		if fs.setRecoveryWait == nil {
			t.Fatalf("%s: SetRunRecoveryWait was never called — the recovery_wait report did not route to setRecoveryWait", name)
		}
		got := *fs.setRecoveryWait
		if !got.RetryNotBefore.Valid {
			t.Fatalf("%s: recovery_retry_not_before was not stamped", name)
		}
		lo := parkNow.Add(tc.backoff + recoveryParkJitterMin)
		hi := parkNow.Add(tc.backoff + recoveryParkJitterMax)
		if got.RetryNotBefore.Time.Before(lo) || got.RetryNotBefore.Time.After(hi) {
			t.Fatalf("%s: recovery_retry_not_before = %v, want within [%v, %v] — now + the "+
				"count-shaped backoff + jitter", name, got.RetryNotBefore.Time, lo, hi)
		}
		if got.ID != run.ID {
			t.Fatalf("%s: parked the wrong run: got %s want %s", name, got.ID, run.ID)
		}
		if !got.WorkerID.Valid || uuid.UUID(got.WorkerID.Bytes) != wkr.ID {
			t.Fatalf("%s: worker_id not carried for affinity", name)
		}
		// A recovery park is never a failure — no failed transition rides alongside it.
		if fs.setFailed != nil {
			t.Fatalf("%s: a recovery park also failed the run: %+v", name, fs.setFailed)
		}
	}
}

// TestSetStateRecoveryWaitGuardRefusalStaysARefusal: when SetRunRecoveryWait's POSITIVE
// source guard rejects (not `running`, wrong worker, a judge run, a re-delivery), 0 rows
// must reach the handler as applied=false → 409, and the run must NOT be failed. The two
// outcomes stay distinguishable, exactly as the limit-wait park's do.
func TestSetStateRecoveryWaitGuardRefusalStaysARefusal(t *testing.T) {
	run := recoveryRun(0)
	fs, svc, wkr := limitParkFixture(t, run)
	fs.setRecoveryWaitRows = 0 // the guard refused

	_, applied, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{State: "recovery_wait"})
	if err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if applied {
		t.Fatal("a 0-row recovery park reported applied = true")
	}
	if fs.setFailed != nil {
		t.Fatalf("a guard refusal also failed the run: %+v — the two outcomes must stay distinguishable", fs.setFailed)
	}
}

// TestSweepPromotesRecoveryWaitRuns pins the sweeper's recovery promotion pass: it runs
// exactly once per Sweep off the pass's own clock, its count lands in
// SweepResult.RecoveryPromoted, and each promotion is broadcast so a resumed run does not
// sit invisible in the board's In Progress column.
func TestSweepPromotesRecoveryWaitRuns(t *testing.T) {
	promoted := []store.PromoteRecoveryWaitRunsRow{
		{ID: uuid.New(), UserID: uuid.New(), Status: "queued"},
		{ID: uuid.New(), UserID: uuid.New(), Status: "queued"},
	}
	fs := &fakeStore{promotedRecoveryWait: promoted}
	svc := New(fs, newBox(t), testParams())
	svc.now = func() time.Time { return parkNow }
	bc := &parkBroadcaster{}
	svc.SetBroadcaster(bc)

	res, err := svc.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.RecoveryPromoted != 2 {
		t.Fatalf("RecoveryPromoted = %d, want 2", res.RecoveryPromoted)
	}
	if len(fs.promoteRecoveryWaitAt) != 1 {
		t.Fatalf("the recovery promotion pass ran %d times, want exactly 1", len(fs.promoteRecoveryWaitAt))
	}
	if !fs.promoteRecoveryWaitAt[0].Valid || !fs.promoteRecoveryWaitAt[0].Time.Equal(parkNow) {
		t.Fatalf("promotion clock = %v, want the sweep's own now %v", fs.promoteRecoveryWaitAt[0], parkNow)
	}
	for _, r := range promoted {
		if !bc.sawState(r.ID, "queued") {
			t.Fatalf("promotion of %s was not broadcast; a resumed run would sit invisible in the board", r.ID)
		}
	}
}

// TestHasLivePollerIsFalseForARecoveryParkedRun: a recovery-parked run KEEPS its worker_id
// for affinity, and that worker keeps heartbeating for its other runs — so both existing
// conditions are false and, without the recovery_wait clause, a cancel would be enqueued
// for a poller that will never read it. It must route server-side, like limit_wait.
func TestHasLivePollerIsFalseForARecoveryParkedRun(t *testing.T) {
	wkrID := uuid.New()
	fs := &fakeStore{workerByID: store.Worker{
		ID: wkrID, LastHeartbeatAt: pgconv.Time(parkNow),
	}}
	svc := New(fs, newBox(t), testParams())
	svc.now = func() time.Time { return parkNow }

	parked := store.Run{ID: uuid.New(), Status: "recovery_wait", WorkerID: pgconv.UUID(wkrID)}
	live, err := svc.hasLivePoller(context.Background(), parked)
	if err != nil {
		t.Fatalf("hasLivePoller: %v", err)
	}
	if live {
		t.Fatal("a recovery-parked run reported a live poller; a cancel enqueued here would sit unconsumed until the promotion pass")
	}
}
