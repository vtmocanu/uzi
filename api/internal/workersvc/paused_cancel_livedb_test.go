package workersvc

import (
	"testing"
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
}
