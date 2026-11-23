package workersvc

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4616 fenced terminal-target cancel (CancelRunForTerminalTarget and, through the same
// core, CancelReworkForMR).

type termCancelEnv struct {
	e      interlockLiveDB
	svc    *Service
	worker uuid.UUID
	wkr    store.Worker
	rec    *readmitRecorder
	nextMR int64
}

func newTermCancelEnv(t *testing.T) *termCancelEnv {
	t.Helper()
	e := setupInterlockLiveDB(t)
	c := &termCancelEnv{e: e, svc: e.permitService(t), rec: newReadmitRecorder(), nextMR: 100}
	c.svc.SetBroadcaster(c.rec)
	c.worker = e.seedWorker(t, nil)
	c.wkr = store.Worker{ID: c.worker, UserID: e.userID}
	c.heartbeat(t, true)
	return c
}

func (c *termCancelEnv) heartbeat(t *testing.T, fresh bool) {
	t.Helper()
	if fresh {
		c.e.exec(t, "UPDATE workers SET last_heartbeat_at=now() WHERE id=$1", c.worker)
	} else {
		c.e.exec(t, "UPDATE workers SET last_heartbeat_at=now()-interval '1 day' WHERE id=$1", c.worker)
	}
}

type termRun struct {
	id     uuid.UUID
	kind   string
	mr     int64
	branch string
	repo   uuid.UUID
}

// seedRun inserts an active ci_fix or mr_rework run bound to a fresh MR. withWorker false leaves it
// worker-less at generation 0 (a never-claimed queued run).
func (c *termCancelEnv) seedRun(t *testing.T, kind, status string, withWorker bool, gen int64) termRun {
	t.Helper()
	c.nextMR++
	r := termRun{id: uuid.New(), kind: kind, mr: c.nextMR, repo: c.e.repoID}
	r.branch = "agent/term-" + r.id.String()[:8]
	var worker any
	if withWorker {
		worker = c.worker
	}
	if kind == "ci_fix" {
		c.e.exec(t, `INSERT INTO runs(id,user_id,repo_id,kind,status,worker_id,claim_generation,pipeline_ref,pipeline_id,ci_fix_mr_iid,issue_title,issue_description,status_since)
 VALUES($1,$2,$3,'ci_fix',$4,$5,$6,$7,123,$8,'t','d',now())`, r.id, c.e.userID, c.e.repoID, status, worker, gen, r.branch, r.mr)
	} else {
		target := uuid.New()
		c.e.exec(t, `INSERT INTO runs(id,user_id,repo_id,kind,status,branch,mr_iid,issue_iid,issue_title,issue_description)
 VALUES($1,$2,$3,'issue','completed',$4,$5,$6,'t','d')`, target, c.e.userID, c.e.repoID, r.branch, r.mr, 9000+r.mr)
		c.e.exec(t, `INSERT INTO runs(id,user_id,repo_id,kind,status,worker_id,claim_generation,pipeline_ref,mr_iid,target_run_id,issue_title,issue_description,status_since)
 VALUES($1,$2,$3,'mr_rework',$4,$5,$6,$7,$8,$9,'t','d',now())`, r.id, c.e.userID, c.e.repoID, status, worker, gen, r.branch, r.mr, target)
	}
	return r
}

func (c *termCancelEnv) obs(t *testing.T, r termRun) TerminalTargetObservation {
	t.Helper()
	run, err := c.e.q.GetRunByID(c.e.ctx, r.id)
	if err != nil {
		t.Fatal(err)
	}
	return TerminalTargetObservation{RunID: r.id, RepoID: c.e.repoID, Kind: r.kind, Status: run.Status, WorkerID: run.WorkerID,
		ClaimGeneration: run.ClaimGeneration, TargetMRIID: r.mr, SourceBranch: r.branch, State: forge.MRStateMerged}
}

func (c *termCancelEnv) cancel(t *testing.T, r termRun) error {
	t.Helper()
	return c.svc.CancelRunForTerminalTarget(c.e.ctx, c.e.repoID, c.obs(t, r))
}

// snapshot is the run row plus its inputs, so a no-op can be proven byte for byte.
func (c *termCancelEnv) snapshot(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := c.e.pool.QueryRow(c.e.ctx, `SELECT jsonb_build_object('run',(SELECT to_jsonb(r) - 'updated_at' FROM runs r WHERE r.id=$1),
 'inputs',(SELECT coalesce(jsonb_agg(to_jsonb(i) ORDER BY i.id),'[]') FROM run_user_inputs i WHERE i.run_id=$1))::text`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (c *termCancelEnv) fields(t *testing.T, id uuid.UUID) (status, stopKind, stopReason, failure, origin string, inputs int) {
	t.Helper()
	if err := c.e.pool.QueryRow(c.e.ctx, `SELECT status, coalesce(stop_kind,''), coalesce(stop_reason,''), coalesce(failure_reason,''), coalesce(fail_origin,''),
 (SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind='cancel') FROM runs WHERE id=$1`, id).
		Scan(&status, &stopKind, &stopReason, &failure, &origin, &inputs); err != nil {
		t.Fatal(err)
	}
	return
}

func (c *termCancelEnv) requireNoOp(t *testing.T, r termRun, call func() error) {
	t.Helper()
	before := c.snapshot(t, r.id)
	if err := call(); err != nil {
		t.Fatalf("no-op returned %v", err)
	}
	if after := c.snapshot(t, r.id); after != before {
		t.Fatalf("expected no write:\n before %s\n after  %s", before, after)
	}
}

func TestTerminalTargetStopReasonUnit(t *testing.T) {
	if got := terminalTargetStopReason(forge.MRStateMerged); got != "target MR merged; further publication stopped." {
		t.Fatalf("merged: %q", got)
	}
	if got := TerminalTargetStopReason(forge.MRStateClosed); got != "target MR closed; further publication stopped." {
		t.Fatalf("closed: %q", got)
	}
	if got := terminalTargetStopReason("opened"); strings.Contains(got, "opened") {
		t.Fatalf("unexpected state echoed: %q", got)
	}
}

func TestTerminalTargetCancelLiveWorkerLiveDB(t *testing.T) {
	for _, kind := range []string{"ci_fix", "mr_rework"} {
		t.Run(kind, func(t *testing.T) {
			c := newTermCancelEnv(t)
			r := c.seedRun(t, kind, "running", true, 1)
			c.e.exec(t, "UPDATE runs SET failure_reason='stale failure', fail_origin='agent_failure' WHERE id=$1", r.id)
			if err := c.cancel(t, r); err != nil {
				t.Fatal(err)
			}
			status, sk, sr, failure, stampOrigin, inputs := c.fields(t, r.id)
			if status != "running" || sk != "cancelled" || sr != termMergedReason || failure != "" || stampOrigin != "" || inputs != 1 {
				t.Fatalf("after cancel: %s %s %q %q %q inputs=%d", status, sk, sr, failure, stampOrigin, inputs)
			}
			// A duplicate call writes no second input.
			c.requireNoOp(t, r, func() error { return c.cancel(t, r) })
			gen := int64(1)
			got, err := c.svc.ConsumeInputs(c.e.ctx, c.wkr, r.id, &gen)
			if err != nil {
				t.Fatal(err)
			}
			drained := got.TerminalCancelled != nil
			for _, in := range got.Inputs {
				drained = drained || in.Kind == "cancel"
			}
			if !drained {
				t.Fatalf("the live worker did not receive the cancel: %+v", got)
			}
			res, err := c.svc.SetStateReportWithReconciliation(c.e.ctx, c.wkr, r.id, StateRequest{State: "failed", ClaimGeneration: &gen,
				FailureReason: strPtr("worker text"), FailOrigin: strPtr("agent_failure")})
			if err != nil || !res.Applied {
				t.Fatalf("failed report: %+v %v", res, err)
			}
			status, sk, sr, failure, origin, _ := c.fields(t, r.id)
			if status != "cancelled" || sk != "cancelled" || sr != termMergedReason || failure != "" || origin != "" {
				t.Fatalf("terminal: %s %s %q %q %q", status, sk, sr, failure, origin)
			}
		})
	}
}

func TestTerminalTargetCancelServerSideLiveDB(t *testing.T) {
	for _, kind := range []string{"ci_fix", "mr_rework"} {
		for _, tc := range []struct {
			status string
			worker bool
		}{{"queued", false}, {"limit_wait", true}, {"pool_wait", true}} {
			t.Run(kind+"/"+tc.status, func(t *testing.T) {
				c := newTermCancelEnv(t)
				r := c.seedRun(t, kind, tc.status, tc.worker, 0)
				c.e.exec(t, "UPDATE runs SET failure_reason='stale failure', fail_origin='agent_failure' WHERE id=$1", r.id)
				if err := c.cancel(t, r); err != nil {
					t.Fatal(err)
				}
				status, sk, sr, failure, origin, inputs := c.fields(t, r.id)
				if status != "cancelled" || sk != "cancelled" || sr != termMergedReason || failure != "" || origin != "" || inputs != 0 {
					t.Fatalf("after cancel: %s %s %q %q %q inputs=%d", status, sk, sr, failure, origin, inputs)
				}
				if _, states := c.rec.published(r.id); len(states) != 1 || states[0] != "cancelled" {
					t.Fatalf("published states = %v, want one cancelled", states)
				}
				// A second call finds a terminal run and writes nothing.
				c.requireNoOp(t, r, func() error { return c.cancel(t, r) })
			})
		}
	}
}

func TestTerminalTargetCancelLeavesHeldAndStaleRunsLiveDB(t *testing.T) {
	for _, kind := range []string{"ci_fix", "mr_rework"} {
		for _, tc := range []struct {
			name   string
			status string
			stale  bool
		}{{"paused", "paused", false}, {"recovery_wait", "recovery_wait", false}, {"stale claimed", "claimed", true}, {"stale running", "running", true}} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				c := newTermCancelEnv(t)
				r := c.seedRun(t, kind, tc.status, true, 1)
				c.heartbeat(t, !tc.stale)
				c.requireNoOp(t, r, func() error { return c.cancel(t, r) })
				if status, sk, _, _, _, inputs := c.fields(t, r.id); status != tc.status || sk != "" || inputs != 0 {
					t.Fatalf("held run changed: %s %q inputs=%d", status, sk, inputs)
				}
			})
		}
	}
}

func TestTerminalTargetCancelPendingOutcomeLiveDB(t *testing.T) {
	for _, kind := range []string{"ci_fix", "mr_rework"} {
		t.Run(kind, func(t *testing.T) {
			c := newTermCancelEnv(t)
			r := c.seedRun(t, kind, "running", true, 3)
			c.e.exec(t, `INSERT INTO worker_active_runs(worker_id, run_id, claim_generation, phase, terminal_pending, terminal_pending_until, snapshot_epoch, reported_at)
 VALUES ($1,$2,3,'running',true,$3,1,now())`, c.worker, r.id, time.Now().Add(time.Hour))
			c.requireNoOp(t, r, func() error { return c.cancel(t, r) })
		})
	}
}

func TestTerminalTargetCancelObservationMismatchLiveDB(t *testing.T) {
	for _, kind := range []string{"ci_fix", "mr_rework"} {
		mutations := map[string]func(t *testing.T, c *termCancelEnv, r termRun, o *TerminalTargetObservation){
			"generation": func(_ *testing.T, _ *termCancelEnv, _ termRun, o *TerminalTargetObservation) { o.ClaimGeneration++ },
			"worker": func(_ *testing.T, _ *termCancelEnv, _ termRun, o *TerminalTargetObservation) {
				o.WorkerID = pgconv.UUID(uuid.New())
			},
			"worker unset": func(_ *testing.T, _ *termCancelEnv, _ termRun, o *TerminalTargetObservation) {
				o.WorkerID = pgtype.UUID{}
			},
			"iid":    func(_ *testing.T, _ *termCancelEnv, _ termRun, o *TerminalTargetObservation) { o.TargetMRIID++ },
			"branch": func(_ *testing.T, _ *termCancelEnv, _ termRun, o *TerminalTargetObservation) { o.SourceBranch += "-x" },
			"kind": func(_ *testing.T, _ *termCancelEnv, _ termRun, o *TerminalTargetObservation) {
				if o.Kind == "ci_fix" {
					o.Kind = "mr_rework"
				} else {
					o.Kind = "ci_fix"
				}
			},
			"repo": func(_ *testing.T, _ *termCancelEnv, _ termRun, o *TerminalTargetObservation) { o.RepoID = uuid.New() },
			"opened": func(_ *testing.T, _ *termCancelEnv, _ termRun, o *TerminalTargetObservation) {
				o.State = forge.MRStateOpened
			},
			"no state": func(_ *testing.T, _ *termCancelEnv, _ termRun, o *TerminalTargetObservation) { o.State = "" },
			"locked": func(_ *testing.T, _ *termCancelEnv, _ termRun, o *TerminalTargetObservation) {
				o.State = forge.MRStateLocked
			},
			"bad kind": func(_ *testing.T, _ *termCancelEnv, _ termRun, o *TerminalTargetObservation) { o.Kind = "issue" },
			"stop_kind": func(t *testing.T, c *termCancelEnv, r termRun, _ *TerminalTargetObservation) {
				c.e.exec(t, "UPDATE runs SET stop_kind='stopped', stop_reason='op' WHERE id=$1", r.id)
			},
			"terminal": func(t *testing.T, c *termCancelEnv, r termRun, _ *TerminalTargetObservation) {
				c.e.exec(t, "UPDATE runs SET status='failed' WHERE id=$1", r.id)
			},
		}
		for name, mutate := range mutations {
			t.Run(kind+"/"+name, func(t *testing.T) {
				c := newTermCancelEnv(t)
				r := c.seedRun(t, kind, "queued", true, 1) // a server-side cancel would apply on a match
				o := c.obs(t, r)
				mutate(t, c, r, &o)
				repoID := c.e.repoID
				if name == "repo" {
					repoID = uuid.New() // the repo is the call's argument
				}
				c.requireNoOp(t, r, func() error { return c.svc.CancelRunForTerminalTarget(c.e.ctx, repoID, o) })
			})
		}
	}
}

// The run is reclaimed between the observation and the call: the new generation is untouched.
func TestTerminalTargetCancelReclaimedBetweenObservationAndCallLiveDB(t *testing.T) {
	for _, kind := range []string{"ci_fix", "mr_rework"} {
		t.Run(kind, func(t *testing.T) {
			c := newTermCancelEnv(t)
			r := c.seedRun(t, kind, "queued", true, 1)
			o := c.obs(t, r)
			c.e.exec(t, "UPDATE runs SET status='running', claim_generation=2 WHERE id=$1", r.id)
			c.requireNoOp(t, r, func() error { return c.svc.CancelRunForTerminalTarget(c.e.ctx, c.e.repoID, o) })
			// The same through the legacy policy: the shared core fences the generation too.
			c.requireNoOp(t, r, func() error {
				return c.svc.cancelTerminalTarget(c.e.ctx, o, terminalTargetPolicyLegacyRework, "mr closed")
			})
		})
	}
}

func TestTerminalTargetCancelRequiresTxBeginnerLiveDB(t *testing.T) {
	c := newTermCancelEnv(t)
	r := c.seedRun(t, "mr_rework", "queued", false, 0)
	bare := New(c.e.q, newBox(t), testParams())
	if err := bare.CancelRunForTerminalTarget(c.e.ctx, c.e.repoID, c.obs(t, r)); err == nil {
		t.Fatal("CancelRunForTerminalTarget without a transaction beginner must fail closed")
	}
	if err := bare.CancelReworkForMR(c.e.ctx, c.e.repoID, r.mr, "mr closed"); err == nil {
		t.Fatal("CancelReworkForMR without a transaction beginner must fail closed")
	}
	if status, sk, _, _, _, _ := c.fields(t, r.id); status != "queued" || sk != "" {
		t.Fatalf("a fail-closed cancel wrote: %s %q", status, sk)
	}
}

// Legacy CancelReworkForMR on the fenced core.
func TestCancelReworkForMRFencedLiveDB(t *testing.T) {
	t.Run("pending outcome is not discarded", func(t *testing.T) {
		c := newTermCancelEnv(t)
		r := c.seedRun(t, "mr_rework", "running", true, 3)
		c.e.exec(t, `INSERT INTO worker_active_runs(worker_id, run_id, claim_generation, phase, terminal_pending, terminal_pending_until, snapshot_epoch, reported_at)
 VALUES ($1,$2,3,'running',true,$3,1,now())`, c.worker, r.id, time.Now().Add(time.Hour))
		c.requireNoOp(t, r, func() error { return c.svc.CancelReworkForMR(c.e.ctx, c.e.repoID, r.mr, "mr closed") })
		var leases int
		if err := c.e.pool.QueryRow(c.e.ctx, "SELECT count(*) FROM worker_active_runs WHERE run_id=$1 AND terminal_pending", r.id).Scan(&leases); err != nil || leases != 1 {
			t.Fatalf("pending outcome lease = %d err=%v", leases, err)
		}
	})
	t.Run("existing stop_kind is not overwritten", func(t *testing.T) {
		for _, fresh := range []bool{true, false} {
			c := newTermCancelEnv(t)
			r := c.seedRun(t, "mr_rework", "running", true, 1)
			c.heartbeat(t, fresh)
			c.e.exec(t, "UPDATE runs SET stop_kind='cancelled', stop_reason='operator reason' WHERE id=$1", r.id)
			c.requireNoOp(t, r, func() error { return c.svc.CancelReworkForMR(c.e.ctx, c.e.repoID, r.mr, "mr closed") })
			if _, sk, sr, _, _, _ := c.fields(t, r.id); sk != "cancelled" || sr != "operator reason" {
				t.Fatalf("operator verdict overwritten: %q %q", sk, sr)
			}
		}
	})
	t.Run("terminal run is not overwritten", func(t *testing.T) {
		c := newTermCancelEnv(t)
		r := c.seedRun(t, "mr_rework", "queued", false, 0)
		o := c.obs(t, r)
		c.e.exec(t, "UPDATE runs SET status='completed' WHERE id=$1", r.id)
		c.requireNoOp(t, r, func() error {
			return c.svc.cancelTerminalTarget(c.e.ctx, o, terminalTargetPolicyLegacyRework, "mr closed")
		})
	})
	t.Run("parked rework is cancelled server-side with the passed reason", func(t *testing.T) {
		for _, status := range []string{"paused", "recovery_wait", "limit_wait"} {
			c := newTermCancelEnv(t)
			r := c.seedRun(t, "mr_rework", status, true, 1)
			if err := c.svc.CancelReworkForMR(c.e.ctx, c.e.repoID, r.mr, TerminalTargetStopReason(forge.MRStateClosed)); err != nil {
				t.Fatal(err)
			}
			if st, sk, sr, _, _, inputs := c.fields(t, r.id); st != "cancelled" || sk != "cancelled" || sr != termClosedReason || inputs != 0 {
				t.Fatalf("%s: %s %s %q inputs=%d", status, st, sk, sr, inputs)
			}
		}
	})
	t.Run("stale claimed rework is cancelled server-side", func(t *testing.T) {
		c := newTermCancelEnv(t)
		r := c.seedRun(t, "mr_rework", "claimed", true, 1)
		c.heartbeat(t, false)
		if err := c.svc.CancelReworkForMR(c.e.ctx, c.e.repoID, r.mr, "mr closed"); err != nil {
			t.Fatal(err)
		}
		if st, _, sr, _, _, _ := c.fields(t, r.id); st != "cancelled" || sr != "mr closed" {
			t.Fatalf("%s %q", st, sr)
		}
	})
	t.Run("live rework enqueues one cancel input", func(t *testing.T) {
		c := newTermCancelEnv(t)
		r := c.seedRun(t, "mr_rework", "running", true, 1)
		for i := 0; i < 2; i++ {
			if err := c.svc.CancelReworkForMR(c.e.ctx, c.e.repoID, r.mr, "mr closed"); err != nil {
				t.Fatal(err)
			}
		}
		if st, sk, sr, _, _, inputs := c.fields(t, r.id); st != "running" || sk != "cancelled" || sr != "mr closed" || inputs != 1 {
			t.Fatalf("%s %s %q inputs=%d", st, sk, sr, inputs)
		}
	})
}
