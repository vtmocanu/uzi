package workersvc

import (
	"errors"
	"testing"
	"time"
)

// TestSubmitInputCancelPausedRunLiveDB is the live-DB regression for issue #1969: a paused run
// keeps its worker_id, and that worker keeps heartbeating for its other runs, so hasLivePoller used
// to read it as polled. The owner's cancel then took the enqueue arm (CreateStopVerdictInput) for an
// executor that does not exist, and the run stayed paused with the cancel never consumed. Every
// hold_reason must cancel SERVER-SIDE instead. Skipped unless UZI_TEST_DATABASE_URL is set
// (setupCodexLiveDB skips).
func TestSubmitInputCancelPausedRunLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	o := seedReevalOwner(t, env, BindModeAuto, false)
	setWorkerHeartbeatFresh(t, env, o.workerID) // fresh: the worker is alive, just not polling these runs

	cancelInputs := func(t *testing.T, id any) int {
		t.Helper()
		var n int
		if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM run_user_inputs WHERE run_id = $1 AND kind = 'cancel'`, id).Scan(&n); err != nil {
			t.Fatalf("count cancel inputs: %v", err)
		}
		return n
	}

	cases := []struct {
		name          string
		issueIID      int64
		holdReason    any // nil = an owner pause
		claimReleased bool
	}{
		{"credential_disabled park (claim released)", 19691, "credential_disabled", true},
		{"server wall park (budget_exhausted, claim released)", 19692, "budget_exhausted", true},
		{"worker wall park (budget_exhausted, claim kept)", 19693, "budget_exhausted", false},
		{"completion_blocked hold", 19694, "completion_blocked", false},
		{"owner pause (no hold_reason)", 19695, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := seedRunningRun(t, env, o, tc.issueIID, 0)
			env.exec(`UPDATE runs SET status = 'paused', status_since = now(), hold_reason = $2,
			              claim_released_at = CASE WHEN $3::boolean THEN now() END
			          WHERE id = $1`, id, tc.holdReason, tc.claimReleased)

			res, err := svc.SubmitInputWithOptions(env.ctx, o.userID, id, "cancel", "owner cancel", nil, SubmitInputOptions{})
			if err != nil {
				t.Fatalf("cancel err = %v, want nil", err)
			}
			if !res.ServerSide {
				t.Fatal("a cancel of a paused run was enqueued for a poller that does not exist; it must apply server-side")
			}
			if s := statusOf(t, env, id); s != "cancelled" {
				t.Fatalf("status = %q, want cancelled", s)
			}
			if n := cancelInputs(t, id); n != 0 {
				t.Fatalf("%d cancel input(s) enqueued, want 0 (server-side cancel)", n)
			}
		})
	}

	// Control: the same fresh-heartbeat worker on a RUNNING run is a live poller, so the cancel is
	// still enqueued for it. Without this half a mutation returning false unconditionally would pass.
	t.Run("running run on the same worker still enqueues", func(t *testing.T) {
		id := seedRunningRun(t, env, o, 19696, 0)
		res, err := svc.SubmitInputWithOptions(env.ctx, o.userID, id, "cancel", "owner cancel", nil, SubmitInputOptions{})
		if err != nil {
			t.Fatalf("cancel err = %v, want nil", err)
		}
		if res.ServerSide {
			t.Fatal("a cancel of a running run with a live poller must be enqueued, not applied server-side")
		}
		if s := statusOf(t, env, id); s != "running" {
			t.Fatalf("status = %q, want running", s)
		}
		if n := cancelInputs(t, id); n != 1 {
			t.Fatalf("%d cancel input(s) enqueued, want 1", n)
		}
	})

	// The paused clause returns from hasLivePoller BEFORE its RunHasPendingOutcomeLease check, so a
	// paused run holding a pending-outcome lease on its fresh-heartbeat worker relies on SubmitInput's
	// own confirmation gate: an ordinary cancel is refused and changes nothing.
	t.Run("paused run with a pending outcome refuses an ordinary cancel", func(t *testing.T) {
		id := seedRunningRun(t, env, o, 19697, 0)
		setRunGeneration(t, env, id, 4)
		seedTerminalPendingLease(t, env, o.workerID, id, 4, time.Now().Add(time.Hour))
		env.exec(`UPDATE runs SET status = 'paused', status_since = now(), hold_reason = 'completion_blocked' WHERE id = $1`, id)
		snapshot := func() string {
			var s string
			if err := env.pool.QueryRow(env.ctx, `SELECT status || '|' || coalesce(hold_reason,'') || '|' || claim_generation
			    || '|' || (SELECT count(*) FROM run_user_inputs WHERE run_id = $1)
			    || '|' || (SELECT count(*) FROM worker_active_runs WHERE run_id = $1 AND terminal_pending)
			    FROM runs WHERE id = $1`, id).Scan(&s); err != nil {
				t.Fatalf("snapshot: %v", err)
			}
			return s
		}
		before := snapshot()
		_, err := svc.SubmitInputWithOptions(env.ctx, o.userID, id, "cancel", "", nil, SubmitInputOptions{})
		if !errors.Is(err, ErrOutcomePendingConfirmationRequired) {
			t.Fatalf("err = %v, want ErrOutcomePendingConfirmationRequired", err)
		}
		if after := snapshot(); after != before {
			t.Fatalf("refused cancel changed state: before %q after %q", before, after)
		}
		if before != "paused|completion_blocked|4|0|1" {
			t.Fatalf("unexpected seeded snapshot %q", before)
		}
	})
}
