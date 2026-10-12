package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
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
		// An ACTUAL retained checkpoint, a persisted run tip and an open custody hold: the refused
		// cancel must leave every one of them byte-for-byte as seeded (a row that is merely absent
		// before and after would not prove preservation).
		const tip = "3333333333333333333333333333333333333333"
		branch := agentIssueBranch(19697)
		ref := checkpointRefPrefix + branch
		env.exec(`INSERT INTO checkpoint_retentions(run_id,user_id,repo_id,branch,ref,tip,state)
		          VALUES($1,$2,$3,$4,$5,$6,'retained')`, id, o.userID, o.repoID, branch, ref, tip)
		env.exec(`UPDATE runs SET checkpoint_tip = $2, checkpoint_tip_at = '2026-01-02 03:04:05+00' WHERE id = $1`, id, tip)
		holdID := uuid.New()
		env.exec(`INSERT INTO recovery_custody_holds
		            (id, user_id, repo_id, run_id, generation, state,
		             original_worker_id, original_worker_identity, live_worker_id, live_run_id)
		          VALUES ($1, $2, $3, $4, 4, 'open', $5, 'ident', $5, $4)`,
			holdID, o.userID, o.repoID, id, o.workerID)
		snapshot := func() string {
			var s string
			if err := env.pool.QueryRow(env.ctx, `SELECT status || '|' || coalesce(hold_reason,'') || '|' || claim_generation
			    || '|' || (SELECT count(*) FROM run_user_inputs WHERE run_id = $1)
			    || '|' || (SELECT count(*) FROM worker_active_runs WHERE run_id = $1 AND terminal_pending)
			    || '|tip=' || coalesce(checkpoint_tip,'') || '@' || coalesce(extract(epoch FROM checkpoint_tip_at)::bigint::text,'')
			    || '|ret=' || (SELECT count(*) FROM checkpoint_retentions WHERE run_id = $1)
			    || ':' || coalesce((SELECT string_agg(state || ',' || tip || ',' || ref, ';') FROM checkpoint_retentions WHERE run_id = $1),'')
			    || '|hold=' || coalesce((SELECT string_agg(id::text || ',' || run_id::text || ',' || generation || ',' || state
			         || ',' || coalesce(live_worker_id::text,'') || ',' || coalesce(live_run_id::text,'')
			         || ',' || coalesce(extract(epoch FROM released_at)::bigint::text,'null'), ';') FROM recovery_custody_holds WHERE run_id = $1),'')
			    FROM runs WHERE id = $1`, id).Scan(&s); err != nil {
				t.Fatalf("snapshot: %v", err)
			}
			return s
		}
		want := "paused|completion_blocked|4|0|1|tip=" + tip + "@1767323045" +
			"|ret=1:retained," + tip + "," + ref +
			"|hold=" + holdID.String() + "," + id.String() + ",4,open," + o.workerID.String() + "," + id.String() + ",null"
		before := snapshot()
		if before != want {
			t.Fatalf("unexpected seeded snapshot %q, want %q", before, want)
		}
		_, err := svc.SubmitInputWithOptions(env.ctx, o.userID, id, "cancel", "", nil, SubmitInputOptions{})
		if !errors.Is(err, ErrOutcomePendingConfirmationRequired) {
			t.Fatalf("err = %v, want ErrOutcomePendingConfirmationRequired", err)
		}
		if after := snapshot(); after != want {
			t.Fatalf("refused cancel changed state: want %q after %q", want, after)
		}
	})
}

// TestCancelReworkForMRPausedRunLiveDB: CancelReworkForMR on a paused mr_rework run (worker kept, fresh
// heartbeat for its other runs) must cancel server-side rather than enqueue a cancel input for a
// poller that does not exist (#1969).
func TestCancelReworkForMRPausedRunLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	o := seedReevalOwner(t, env, BindModeAuto, false)
	setWorkerHeartbeatFresh(t, env, o.workerID)

	seedRework := func(t *testing.T, issueIID, mrIID int64, status string) uuid.UUID {
		t.Helper()
		target := seedRunningRun(t, env, o, issueIID, 0)
		env.exec(`UPDATE runs SET status = 'completed' WHERE id = $1`, target)
		id := uuid.New()
		env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_title, issue_description, status, status_since,
		             worker_id, pipeline_ref, mr_iid, target_run_id)
		          VALUES ($1, $2, $3, 'mr_rework', 't', 'd', $4, now(), $5, $6, $7, $8)`,
			id, o.userID, o.repoID, status, o.workerID, agentIssueBranch(issueIID), mrIID, target)
		return id
	}
	cancelInputs := func(t *testing.T, id uuid.UUID) int {
		t.Helper()
		var n int
		if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM run_user_inputs WHERE run_id = $1 AND kind = 'cancel'`, id).Scan(&n); err != nil {
			t.Fatalf("count cancel inputs: %v", err)
		}
		return n
	}

	t.Run("paused rework cancels server-side", func(t *testing.T) {
		id := seedRework(t, 27511, 27511, "paused")
		if err := svc.CancelReworkForMR(env.ctx, o.repoID, 27511, "mr closed"); err != nil {
			t.Fatalf("CancelReworkForMR err = %v, want nil", err)
		}
		if s := statusOf(t, env, id); s != "cancelled" {
			t.Fatalf("status = %q, want cancelled", s)
		}
		if n := cancelInputs(t, id); n != 0 {
			t.Fatalf("%d cancel input(s) enqueued, want 0 (server-side cancel)", n)
		}
		var reason *string
		if err := env.pool.QueryRow(env.ctx, `SELECT stop_reason FROM runs WHERE id = $1`, id).Scan(&reason); err != nil {
			t.Fatalf("read stop_reason: %v", err)
		}
		if reason == nil || *reason != "mr closed" {
			t.Fatalf("stop_reason = %v, want %q", reason, "mr closed")
		}
	})

	// Control: a running rework on the live worker is enqueued, so "always not live" fails here.
	t.Run("running rework still enqueues", func(t *testing.T) {
		id := seedRework(t, 27512, 27512, "running")
		if err := svc.CancelReworkForMR(env.ctx, o.repoID, 27512, "mr closed"); err != nil {
			t.Fatalf("CancelReworkForMR err = %v, want nil", err)
		}
		if s := statusOf(t, env, id); s != "running" {
			t.Fatalf("status = %q, want running", s)
		}
		if n := cancelInputs(t, id); n != 1 {
			t.Fatalf("%d cancel input(s) enqueued, want 1", n)
		}
	})
}

// TestSubmitInputRejectPlanPausedRunLiveDB: reject_plan on a paused run applies server-side (#1969),
// while a gate-revision-bound reject is refused up front and an awaiting_approval run still binds.
func TestSubmitInputRejectPlanPausedRunLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	o := seedReevalOwner(t, env, BindModeAuto, false)
	setWorkerHeartbeatFresh(t, env, o.workerID)

	inputs := func(t *testing.T, id uuid.UUID) int {
		t.Helper()
		var n int
		if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM run_user_inputs WHERE run_id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("count inputs: %v", err)
		}
		return n
	}

	t.Run("paused run rejects server-side", func(t *testing.T) {
		id := seedRunningRun(t, env, o, 27513, 0)
		env.exec(`UPDATE runs SET status = 'paused', status_since = now() WHERE id = $1`, id)
		res, err := svc.SubmitInputWithOptions(env.ctx, o.userID, id, "reject_plan", "nope", nil, SubmitInputOptions{})
		if err != nil {
			t.Fatalf("reject_plan err = %v, want nil", err)
		}
		if !res.ServerSide {
			t.Fatal("reject_plan of a paused run was enqueued for a poller that does not exist; it must apply server-side")
		}
		var status, stopKind, reason string
		if err := env.pool.QueryRow(env.ctx, `SELECT status, coalesce(stop_kind,''), coalesce(failure_reason,'') FROM runs WHERE id = $1`, id).Scan(&status, &stopKind, &reason); err != nil {
			t.Fatalf("read run: %v", err)
		}
		if status != "failed" || stopKind != "plan_rejected" || reason != "nope" {
			t.Fatalf("status/stop_kind/failure_reason = %q/%q/%q, want failed/plan_rejected/nope", status, stopKind, reason)
		}
		if n := inputs(t, id); n != 0 {
			t.Fatalf("%d input(s) enqueued, want 0", n)
		}
	})

	// The expected-revision refusal happens up front in checkExpectedGateRevision (a paused run is not
	// awaiting_approval), before hasLivePoller runs, so the #1969 guard does not pin this case.
	t.Run("paused run with a bound reject is refused and unchanged", func(t *testing.T) {
		id := seedRunningRun(t, env, o, 27514, 0)
		env.exec(`UPDATE runs SET status = 'paused', status_since = now(), hold_reason = 'completion_blocked', gate_revision = 2 WHERE id = $1`, id)
		snapshot := func() string {
			var s string
			if err := env.pool.QueryRow(env.ctx, `SELECT status || '|' || coalesce(hold_reason,'') || '|' || coalesce(stop_kind,'')
			    || '|' || coalesce(failure_reason,'') || '|' || gate_revision
			    || '|' || (SELECT count(*) FROM run_user_inputs WHERE run_id = $1)
			    FROM runs WHERE id = $1`, id).Scan(&s); err != nil {
				t.Fatalf("snapshot: %v", err)
			}
			return s
		}
		before := snapshot()
		if before != "paused|completion_blocked|||2|0" {
			t.Fatalf("unexpected seeded snapshot %q", before)
		}
		for _, expected := range []int64{2, 3} {
			_, err := svc.SubmitInputWithOptions(env.ctx, o.userID, id, "reject_plan", "nope", nil, SubmitInputOptions{ExpectedGateRevision: &expected})
			var mm *GateRevisionMismatchError
			if !errors.As(err, &mm) {
				t.Fatalf("expected %d: err = %v, want *GateRevisionMismatchError", expected, err)
			}
			if mm.Expected != expected || mm.Current != 2 {
				t.Fatalf("expected %d: mismatch = %+v, want Expected=%d Current=2", expected, *mm, expected)
			}
			if after := snapshot(); after != before {
				t.Fatalf("expected %d: refused reject changed state: before %q after %q", expected, before, after)
			}
		}
	})

	// Control: an awaiting_approval run on the live worker still enqueues a revision-bound reject.
	t.Run("awaiting_approval run enqueues a bound reject", func(t *testing.T) {
		id := seedRunningRun(t, env, o, 27515, 0)
		env.exec(`UPDATE runs SET status = 'awaiting_approval', status_since = now(), gate_revision = 2 WHERE id = $1`, id)
		expected := int64(2)
		res, err := svc.SubmitInputWithOptions(env.ctx, o.userID, id, "reject_plan", "nope", nil, SubmitInputOptions{ExpectedGateRevision: &expected})
		if err != nil {
			t.Fatalf("reject_plan err = %v, want nil", err)
		}
		if res.ServerSide {
			t.Fatal("reject_plan of an awaiting_approval run with a live poller must be enqueued, not applied server-side")
		}
		var n int
		var binding *string
		var rev *int64
		if err := env.pool.QueryRow(env.ctx, `SELECT count(*), max(gate_binding), max(gate_revision) FROM run_user_inputs WHERE run_id = $1 AND kind = 'reject_plan'`, id).Scan(&n, &binding, &rev); err != nil {
			t.Fatalf("read inputs: %v", err)
		}
		if n != 1 || binding == nil || *binding != "bound" || rev == nil || *rev != 2 {
			t.Fatalf("reject_plan rows = %d binding = %v revision = %v, want 1 bound at 2", n, binding, rev)
		}
	})
}
