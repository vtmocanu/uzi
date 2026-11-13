package workersvc

import (
	"testing"

	"github.com/google/uuid"
)

// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (run via
// ./e2e/run-store-it.sh).
//
// Issue #3630. A Codex worker's "retained stop" leaves the run row `running` at the SAME
// claim_generation but drops it from the worker's heartbeat active-run snapshot while the worker
// keeps heartbeating. These tests pin, through the production Heartbeat / SubmitInput / Sweep
// paths only, that the existing server-side reconciliation acts on such a row: every case that
// expects a transition has a control in which the snapshot LISTS the run at its current claim
// generation and the row must stay `running`, so a change that makes the heartbeat ignore (or
// always act on) omission reddens one side.
//
// Raw statements only shape the seeded row (status_since, started_at, wall budget, interactive,
// completion_attempts, requeue counters) and backdate pause_requested_at past the wall-park grace.

const retainedStopNonce = "nonce-A"

type retainedStopRig struct {
	env    codexTestEnv
	svc    *Service
	userID uuid.UUID
	repoID uuid.UUID
	worker uuid.UUID
	epoch  int64
}

func newRetainedStopRig(t *testing.T, mutate func(*Params)) *retainedStopRig {
	t.Helper()
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	p := testParams()
	if mutate != nil {
		mutate(&p)
	}
	return &retainedStopRig{
		env: env, svc: snapshotSvc(env, p), userID: userID, repoID: repoID,
		worker: seedSnapshotWorker(t, env, userID, retainedStopNonce),
	}
}

// seed inserts a running run owned by the rig's worker: wall budget 24h, started and status_since
// 3h ago (past the missing-run fence, under the wall), requeue_count as given.
func (r *retainedStopRig) seed(t *testing.T, gen int64, requeueCount int32) uuid.UUID {
	t.Helper()
	return seedOutageRun(t, r.env, r.userID, r.repoID, r.worker, "running", "issue", gen, requeueCount)
}

// beat sends one real heartbeat whose snapshot lists exactly `active` (none = the run is
// omitted) with the next epoch.
func (r *retainedStopRig) beat(t *testing.T, active ...ActiveRunEntry) {
	t.Helper()
	r.epoch++
	if active == nil {
		active = []ActiveRunEntry{}
	}
	snap := &ActiveSnapshot{SnapshotEpoch: r.epoch, RegisterNonce: retainedStopNonce, Active: active}
	if _, err := r.svc.Heartbeat(r.env.ctx, hbWorker(t, r.env, r.worker), nil, nil, snap); err != nil {
		t.Fatalf("Heartbeat epoch %d: %v", r.epoch, err)
	}
}

// requireStayedRunning asserts the row is untouched by reconciliation: running, same worker,
// same generation, no requeue charged, claim not released.
func (r *retainedStopRig) requireStayedRunning(t *testing.T, id uuid.UUID, gen int64, requeues int32) {
	t.Helper()
	got := mustRun(t, r.env, id)
	if got.Status != "running" || got.RequeueCount != requeues || got.ClaimGeneration != gen ||
		!got.WorkerID.Valid || uuid.UUID(got.WorkerID.Bytes) != r.worker || got.ClaimReleasedAt.Valid {
		t.Fatalf("run = status %q requeue_count %d gen %d worker %v released %v, want untouched running (requeue_count %d gen %d)",
			got.Status, got.RequeueCount, got.ClaimGeneration, got.WorkerID, got.ClaimReleasedAt.Valid, requeues, gen)
	}
}

// requireRequeued asserts the production requeue: queued, SAME worker kept (affinity), same
// generation, requeue_count + 1, no refund stamp.
func (r *retainedStopRig) requireRequeued(t *testing.T, id uuid.UUID, gen int64, wantRequeues int32) {
	t.Helper()
	got := mustRun(t, r.env, id)
	if got.Status != "queued" || got.RequeueCount != wantRequeues || got.ClaimGeneration != gen ||
		!got.WorkerID.Valid || uuid.UUID(got.WorkerID.Bytes) != r.worker {
		t.Fatalf("run = status %q requeue_count %d gen %d worker %v, want queued requeue_count %d gen %d worker %s",
			got.Status, got.RequeueCount, got.ClaimGeneration, got.WorkerID, wantRequeues, gen, r.worker)
	}
	if _, valid := staleReqGenOf(t, r.env, id); valid {
		t.Fatal("stale_requeue_generation set after a missing requeue; a genuine loss is never refunded")
	}
}

// TestRetainedStopOmittedRunRequeuedLiveDB covers the under-cap requeue across run shapes: an
// omitted heartbeat requeues the row, a heartbeat that lists it at its current generation leaves
// it running, and (for the stale-generation case) a listing at an OLDER generation does not
// protect it.
func TestRetainedStopOmittedRunRequeuedLiveDB(t *testing.T) {
	cases := []struct {
		name     string
		gen      int64
		prep     func(r *retainedStopRig, id uuid.UUID)
		requeues int32 // seeded requeue_count; one requeue adds 1
		// staleListing adds a third run: the snapshot lists gen-1 while the row is at gen.
		staleListing bool
	}{
		{name: "timed_noninteractive_pre_attempt", gen: 1},
		{
			name: "completion_attempts_1", gen: 1,
			prep: func(r *retainedStopRig, id uuid.UUID) {
				r.env.exec(`UPDATE runs SET completion_attempts = 1, completion_contract_version = 1 WHERE id = $1`, id)
			},
		},
		{
			name: "interactive", gen: 1,
			prep: func(r *retainedStopRig, id uuid.UUID) {
				r.env.exec(`UPDATE runs SET interactive = true WHERE id = $1`, id)
			},
		},
		{
			// requeue_count 1 would be at the cap (RUN_MAX_REQUEUES=1) but the episode baseline
			// already absorbed it, so the row is still under cap and requeues.
			name: "requeue_episode_baseline_absorbs_prior_requeue", gen: 1, requeues: 1,
			prep: func(r *retainedStopRig, id uuid.UUID) {
				r.env.exec(`UPDATE runs SET requeue_episode_baseline = 1 WHERE id = $1`, id)
			},
		},
		{name: "snapshot_lists_stale_generation", gen: 2, staleListing: true},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/omitted_requeues", func(t *testing.T) {
			r := newRetainedStopRig(t, nil)
			id := r.seed(t, tc.gen, tc.requeues)
			if tc.prep != nil {
				tc.prep(r, id)
			}
			r.beat(t)
			r.requireRequeued(t, id, tc.gen, tc.requeues+1)
		})
		t.Run(tc.name+"/control_listed_stays_running", func(t *testing.T) {
			r := newRetainedStopRig(t, nil)
			id := r.seed(t, tc.gen, tc.requeues)
			if tc.prep != nil {
				tc.prep(r, id)
			}
			r.beat(t, entry(id, tc.gen, "running", false))
			r.requireStayedRunning(t, id, tc.gen, tc.requeues)
		})
		if tc.staleListing {
			t.Run(tc.name+"/listed_at_older_generation_does_not_protect", func(t *testing.T) {
				r := newRetainedStopRig(t, nil)
				id := r.seed(t, tc.gen, tc.requeues)
				r.beat(t, entry(id, tc.gen-1, "running", false))
				r.requireRequeued(t, id, tc.gen, tc.requeues+1)
			})
		}
	}
}

// TestRetainedStopAtCapLiveDB covers the over-cap disposition of an omitted running row:
//   - custody recorded (a real Service.Claim opened the hold): recovery_wait /
//     worker_requeue_exhausted with the claim released and custody-uncertain evidence;
//   - no custody recorded: failed / worker_lost.
//
// Each has the listed control.
func TestRetainedStopAtCapLiveDB(t *testing.T) {
	t.Run("custody_recorded", func(t *testing.T) {
		t.Run("omitted_parks_worker_requeue_exhausted", func(t *testing.T) {
			env := setupCodexLiveDB(t)
			// max 0: the first lost generation is already at the cap.
			c := newClaimantExhaustion(t, env, true, 0)
			run := c.seedKind(t, "issue")
			c.claimAndRun(t, c.wkr, run, 1, true)
			c.omitHeartbeat(t, c.workerID)
			f := c.facts(t, run)
			if f.status != "recovery_wait" || f.cause != "worker_requeue_exhausted" || !f.released {
				t.Fatalf("omitted at cap: %+v, want recovery_wait/worker_requeue_exhausted with the claim released", f)
			}
			requireCustodyUncertainEvidence(t, f)
			c.requireAllHoldsOpen(t, run, 1)
		})
		t.Run("control_listed_stays_running", func(t *testing.T) {
			env := setupCodexLiveDB(t)
			c := newClaimantExhaustion(t, env, true, 0)
			run := c.seedKind(t, "issue")
			c.claimAndRun(t, c.wkr, run, 1, true)
			c.epoch++
			if _, err := c.svc.Heartbeat(env.ctx, hbWorker(t, env, c.workerID), nil, nil, &ActiveSnapshot{
				SnapshotEpoch: c.epoch, RegisterNonce: exhaustionNonce,
				Active: []ActiveRunEntry{entry(run, 1, "running", false)},
			}); err != nil {
				t.Fatalf("Heartbeat: %v", err)
			}
			if f := c.facts(t, run); f.status != "running" || f.released || f.requeues != 0 {
				t.Fatalf("listed control: %+v, want untouched running", f)
			}
		})
	})
	t.Run("no_custody", func(t *testing.T) {
		t.Run("omitted_fails_worker_lost", func(t *testing.T) {
			r := newRetainedStopRig(t, nil)
			id := r.seed(t, 1, 1) // requeue_count == RUN_MAX_REQUEUES
			r.beat(t)
			status, origin := statusOf(t, r.env, id), failOriginOf(t, r.env, id)
			if status != "failed" || origin != "worker_lost" {
				t.Fatalf("omitted at cap without custody: status %q fail_origin %q, want failed/worker_lost", status, origin)
			}
		})
		t.Run("control_listed_stays_running", func(t *testing.T) {
			r := newRetainedStopRig(t, nil)
			id := r.seed(t, 1, 1)
			r.beat(t, entry(id, 1, "running", false))
			r.requireStayedRunning(t, id, 1, 1)
		})
	})
}

func cancelInputApplied(t *testing.T, r *retainedStopRig, id uuid.UUID) bool {
	t.Helper()
	var applied bool
	if err := r.env.pool.QueryRow(r.env.ctx,
		`SELECT applied_at IS NOT NULL FROM run_user_inputs WHERE run_id = $1 AND kind = 'cancel'`, id).Scan(&applied); err != nil {
		t.Fatalf("read cancel input: %v", err)
	}
	return applied
}

// TestRetainedStopOwnerCancelLiveDB: an owner Cancel on a running row whose worker heartbeats is
// enqueued for its poller (stop_kind='cancelled', the run stays running); when the worker's
// snapshot then omits the run, the heartbeat completes the cancel. The control lists the run: the
// cancel stays pending and the row stays running.
func TestRetainedStopOwnerCancelLiveDB(t *testing.T) {
	for _, interactive := range []bool{true, false} {
		for _, listed := range []bool{false, true} {
			name := "noninteractive"
			if interactive {
				name = "interactive"
			}
			if listed {
				name += "/control_listed_cancel_stays_pending"
			} else {
				name += "/omitted_cancels"
			}
			t.Run(name, func(t *testing.T) {
				r := newRetainedStopRig(t, nil)
				id := r.seed(t, 1, 0)
				if interactive {
					r.env.exec(`UPDATE runs SET interactive = true WHERE id = $1`, id)
				}
				res, err := r.svc.SubmitInput(r.env.ctx, r.userID, id, "cancel", "operator cancel", nil)
				if err != nil {
					t.Fatalf("SubmitInput cancel: %v", err)
				}
				if res.ServerSide {
					t.Fatal("cancel went server-side; this test needs the live-poller path (stop_kind stamped, run left running)")
				}
				if got := mustRun(t, r.env, id); got.Status != "running" || got.StopKind.String != "cancelled" {
					t.Fatalf("after SubmitInput: status %q stop_kind %q, want running/cancelled", got.Status, got.StopKind.String)
				}
				if listed {
					r.beat(t, entry(id, 1, "running", false))
					r.requireStayedRunning(t, id, 1, 0)
					if cancelInputApplied(t, r, id) {
						t.Fatal("listed run's cancel input was applied by the heartbeat; only an omitted run may be cancelled there")
					}
					return
				}
				r.beat(t)
				if got := statusOf(t, r.env, id); got != "cancelled" {
					t.Fatalf("omitted run with a pending owner cancel: status %q, want cancelled", got)
				}
				if !cancelInputApplied(t, r, id) {
					t.Fatal("cancel input not marked applied after the heartbeat completed the cancel")
				}
			})
		}
	}
}

// TestRetainedStopFenceBoundaryLiveDB: the omission acts only once status_since is older than the
// missing fence (WorkerHeartbeatStale + WorkerHeartbeatInterval = 60s in testParams).
func TestRetainedStopFenceBoundaryLiveDB(t *testing.T) {
	cases := []struct {
		name  string
		age   string // status_since = now() - age
		fresh bool
	}{
		{"newer_than_fence_stays_running", "10 seconds", true},
		{"older_than_fence_requeues", "5 minutes", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRetainedStopRig(t, nil)
			id := r.seed(t, 1, 0)
			r.env.exec(`UPDATE runs SET status_since = now() - $2::interval WHERE id = $1`, id, tc.age)
			r.beat(t)
			if tc.fresh {
				r.requireStayedRunning(t, id, 1, 0)
				return
			}
			r.requireRequeued(t, id, 1, 1)
		})
	}
}

// TestRetainedStopNullWallBudgetLiveDB: a timed non-interactive pre-attempt row with NULL
// budget_wall_seconds is measured against the global RUN_TIMEOUT (2h in testParams): under it the
// omitted row requeues; past it the heartbeat leaves it running (the wall sweep owns it).
func TestRetainedStopNullWallBudgetLiveDB(t *testing.T) {
	cases := []struct {
		name    string
		started string
		listed  bool
		requeue bool
	}{
		{"under_global_timeout_omitted_requeues", "1 hour", false, true},
		{"under_global_timeout_control_listed", "1 hour", true, false},
		{"past_global_timeout_omitted_left_running", "3 hours", false, false},
		{"past_global_timeout_control_listed", "3 hours", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRetainedStopRig(t, nil)
			id := r.seed(t, 1, 0)
			r.env.exec(`UPDATE runs SET budget_wall_seconds = NULL, started_at = now() - $2::interval WHERE id = $1`, id, tc.started)
			if tc.listed {
				r.beat(t, entry(id, 1, "running", false))
			} else {
				r.beat(t)
			}
			if tc.requeue {
				r.requireRequeued(t, id, 1, 1)
				return
			}
			r.requireStayedRunning(t, id, 1, 0)
		})
	}
}

// TestRetainedStopPausePendingLiveDB: an owner pause on a running (non-interactive) row leaves it
// running; with the run then omitted from the snapshot the heartbeat requeues it, and listed it
// stays running with the pause pending.
//
// OBSERVED: the requeue statement (FrozenRequeueRunsMissingFromSnapshot) does not touch
// pause_requested_at / pause_mode / pause_after_count, so a pending owner pause SURVIVES the
// requeue (unlike the over-cap park and the wall park, which clear them). The assertions below pin
// that observed value.
//
// CreatePauseInput refuses interactive rows (interactive = false in its predicate), so the pause
// case uses a non-interactive row.
func TestRetainedStopPausePendingLiveDB(t *testing.T) {
	for _, mode := range []string{"milestone", "now"} {
		t.Run(mode+"/omitted_requeues", func(t *testing.T) {
			r := newRetainedStopRig(t, nil)
			id := r.seed(t, 1, 0)
			if _, err := r.svc.SubmitInput(r.env.ctx, r.userID, id, "pause", mode, nil); err != nil {
				t.Fatalf("SubmitInput pause: %v", err)
			}
			before := mustRun(t, r.env, id)
			if before.Status != "running" || before.PauseMode.String != mode || !before.PauseRequestedAt.Valid {
				t.Fatalf("after pause: status %q pause_mode %q requested %v, want running/%s/pending",
					before.Status, before.PauseMode.String, before.PauseRequestedAt.Valid, mode)
			}
			r.beat(t)
			r.requireRequeued(t, id, 1, 1)
			after := mustRun(t, r.env, id)
			if !after.PauseRequestedAt.Valid || after.PauseMode.String != mode ||
				!after.PauseRequestedAt.Time.Equal(before.PauseRequestedAt.Time) {
				t.Fatalf("pause columns after requeue: requested %v mode %q, want the pending %s request preserved (requested_at %v)",
					after.PauseRequestedAt, after.PauseMode.String, mode, before.PauseRequestedAt.Time)
			}
		})
		t.Run(mode+"/control_listed_stays_running_pause_pending", func(t *testing.T) {
			r := newRetainedStopRig(t, nil)
			id := r.seed(t, 1, 0)
			if _, err := r.svc.SubmitInput(r.env.ctx, r.userID, id, "pause", mode, nil); err != nil {
				t.Fatalf("SubmitInput pause: %v", err)
			}
			r.beat(t, entry(id, 1, "running", false))
			r.requireStayedRunning(t, id, 1, 0)
			if got := mustRun(t, r.env, id); !got.PauseRequestedAt.Valid || got.PauseMode.String != mode {
				t.Fatalf("listed run lost its pending pause: requested %v mode %q", got.PauseRequestedAt, got.PauseMode.String)
			}
		})
	}
}

// TestRetainedStopWallSweepLiveDB: a timed pre-attempt row already past its wall is NOT requeued
// by the omitted heartbeat (the requeue statement excludes it); the wall sweep owns it. With boot
// grace inactive, Sweep files the system 'wall' pause request while the worker is live and capable;
// before the 600s grace the row stays running; after it ParkRunsAtWall parks it
// paused/budget_exhausted. The same outcome holds when an ordinary snapshot lists the run (the
// wall sweep never reads the snapshot), so there is no snapshot-present control.
//
// The grace is measured against the database clock (pause_requested_at < now() - 600s), so the
// request is backdated in SQL rather than advancing the service clock.
func TestRetainedStopWallSweepLiveDB(t *testing.T) {
	for _, listed := range []bool{false, true} {
		name := "snapshot_omits_run"
		if listed {
			name = "snapshot_lists_run"
		}
		t.Run(name, func(t *testing.T) {
			r := newRetainedStopRig(t, func(p *Params) { p.SweeperBootGrace = 0 })
			r.env.exec(`UPDATE workers SET protocol_capabilities = ARRAY['wall_park_v1'] WHERE id = $1`, r.worker)
			id := r.seed(t, 1, 0)
			// Past a 60s wall since 2h ago, pre-attempt, non-interactive.
			r.env.exec(`UPDATE runs SET budget_wall_seconds = 60, started_at = now() - interval '2 hours' WHERE id = $1`, id)

			heartbeat := func() {
				t.Helper()
				if listed {
					r.beat(t, entry(id, 1, "running", false))
				} else {
					r.beat(t)
				}
			}
			sweep := func(step string) {
				t.Helper()
				// The worker keeps heartbeating, so it stays live for the sweep's stale classification.
				heartbeat()
				if _, err := r.svc.Sweep(r.env.ctx); err != nil {
					t.Fatalf("Sweep (%s): %v", step, err)
				}
			}

			heartbeat()
			r.requireStayedRunning(t, id, 1, 0)
			if got := mustRun(t, r.env, id); got.PauseMode.Valid {
				t.Fatalf("heartbeat filed pause_mode %q; only the wall sweep files the request", got.PauseMode.String)
			}

			sweep("files the wall request")
			got := mustRun(t, r.env, id)
			if got.Status != "running" || got.PauseMode.String != "wall" || !got.PauseRequestedAt.Valid || got.ClaimReleasedAt.Valid {
				t.Fatalf("after first Sweep: status %q pause_mode %q requested %v released %v, want running with a pending wall request",
					got.Status, got.PauseMode.String, got.PauseRequestedAt.Valid, got.ClaimReleasedAt.Valid)
			}

			// Inside the grace (5m < 600s): still running, still requested.
			r.env.exec(`UPDATE runs SET pause_requested_at = now() - interval '5 minutes' WHERE id = $1`, id)
			sweep("inside grace")
			r.requireStayedRunning(t, id, 1, 0)

			// Past the grace (11m > 600s): parked.
			r.env.exec(`UPDATE runs SET pause_requested_at = now() - interval '11 minutes' WHERE id = $1`, id)
			sweep("past grace")
			got = mustRun(t, r.env, id)
			if got.Status != "paused" || got.HoldReason.String != "budget_exhausted" || !got.ClaimReleasedAt.Valid {
				t.Fatalf("after grace: status %q hold_reason %q released %v, want paused/budget_exhausted with the claim released",
					got.Status, got.HoldReason.String, got.ClaimReleasedAt.Valid)
			}
			if got.RequeueCount != 0 {
				t.Fatalf("requeue_count = %d, want 0 (a wall park is not a requeue)", got.RequeueCount)
			}
		})
	}
}
